package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// TLSMode is how the SMTP connection is protected.
type TLSMode string

const (
	// STARTTLS requires the relay to upgrade to verified TLS after EHLO.
	STARTTLS TLSMode = "starttls"
	// ImplicitTLS uses verified TLS before reading the SMTP greeting.
	ImplicitTLS TLSMode = "implicit"
	// Plain is allowed only with the explicit test-only InsecurePlain flag.
	Plain TLSMode = "plain"
)

// SMTPConfig is the relay settings. The zero value is invalid.
type SMTPConfig struct {
	Addr          string // host:port
	TLS           TLSMode
	Username      string
	Password      string
	CAFile        string // optional PEM extending the system trust pool
	From          string // envelope sender and From header, optional display name
	InsecurePlain bool   // test-only opt-in to TLS=plain
}

// SMTPStage identifies the operation that failed, without relay reply text.
type SMTPStage string

const (
	StageValidation SMTPStage = "validation"
	StageRender     SMTPStage = "render"
	StageConnect    SMTPStage = "connect"
	StageTLS        SMTPStage = "tls"
	StageGreeting   SMTPStage = "greeting"
	StageHello      SMTPStage = "ehlo"
	StageSTARTTLS   SMTPStage = "starttls"
	StageAuth       SMTPStage = "auth"
	StageMail       SMTPStage = "mail"
	StageRecipient  SMTPStage = "rcpt"
	StageData       SMTPStage = "data"
	StageBody       SMTPStage = "body"
)

// SMTPError contains only a safe operation and numeric SMTP status. It never
// retains a server reply or an underlying transport/protocol error, either of
// which may contain a one-time link. Unwrap exposes only context cancellation
// or deadline expiry, so errors.Is works without exposing untrusted text.
type SMTPError struct {
	Stage SMTPStage
	Code  int // zero when no SMTP status was received
	cause error
}

func (e *SMTPError) Error() string {
	s := fmt.Sprintf("smtp: %s failed", e.Stage)
	if e.Code != 0 {
		return fmt.Sprintf("%s (status %d)", s, e.Code)
	}
	if e.cause != nil {
		return s + ": " + e.cause.Error()
	}
	return s
}

func (e *SMTPError) Unwrap() error { return e.cause }

// PermanentError marks a failure that retrying this message cannot fix.
// Recipient is true ONLY for a permanent RCPT mailbox refusal. Authentication,
// sender, message, and configuration failures must not suppress the address.
// Errors.As also exposes the wrapped SMTPError's safe stage and status.
type PermanentError struct {
	Stage     SMTPStage
	Code      int
	Recipient bool
}

func (e *PermanentError) Error() string {
	return (&SMTPError{Stage: e.Stage, Code: e.Code}).Error()
}

func (e *PermanentError) Unwrap() error { return &SMTPError{Stage: e.Stage, Code: e.Code} }

// NewSMTP validates configuration. Private CAs extend system roots; neither
// STARTTLS nor implicit TLS can silently skip certificate or hostname checks.
func NewSMTP(cfg SMTPConfig) (*SMTP, error) {
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil || host == "" || strings.ContainsAny(host, "\r\n\x00") {
		return nil, errors.New("REDROP_SMTP_ADDR must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("REDROP_SMTP_ADDR needs a port from 1 to 65535")
	}
	switch cfg.TLS {
	case STARTTLS, ImplicitTLS:
	case Plain:
		if !cfg.InsecurePlain {
			return nil, errors.New("REDROP_SMTP_TLS=plain needs an explicit insecure opt-in; use starttls or implicit")
		}
	default:
		return nil, errors.New("REDROP_SMTP_TLS must be starttls, implicit or plain")
	}
	if (cfg.Username == "") != (cfg.Password == "") || strings.ContainsAny(cfg.Username+cfg.Password, "\r\n\x00") {
		return nil, errors.New("SMTP authentication needs a username and password without line breaks or NUL")
	}
	sender, err := envelopeSender(cfg.From)
	if err != nil {
		return nil, err
	}
	root, err := x509.SystemCertPool()
	if err != nil || root == nil {
		root = x509.NewCertPool()
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, errors.New("REDROP_SMTP_CA_FILE could not be read")
		}
		if !root.AppendCertsFromPEM(pem) {
			return nil, errors.New("REDROP_SMTP_CA_FILE holds no certificate")
		}
	}
	return &SMTP{cfg: cfg, host: host, root: root, sender: sender}, nil
}

// SMTP sends through one relay connection per message. It is safe for
// concurrent Send calls; its configuration and roots are immutable.
type SMTP struct {
	cfg    SMTPConfig
	host   string
	root   *x509.CertPool
	sender string
}

// Timeout bounds an entire send, including connection and TLS negotiation.
const Timeout = 30 * time.Second

// Bound all relay input, including TLS certificates and multiline replies.
// net/textproto otherwise accumulates an arbitrarily large reply in memory.
const maxRelayBytes = 1 << 20

type smtpReadLimit struct {
	net.Conn
	reader io.Reader
}

func (c *smtpReadLimit) Read(p []byte) (int, error) { return c.reader.Read(p) }

// Send authenticates and submits one message. Returned errors never contain
// relay reply text, addresses, credentials, or message content.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if ctx.Err() != nil {
		return smtpFailure(ctx, StageConnect, ctx.Err())
	}
	to, err := ValidAddress(m.To)
	if err != nil {
		return &PermanentError{Stage: StageValidation}
	}
	raw, err := RenderWith(s.cfg.From, m, time.Now())
	if err != nil {
		return &PermanentError{Stage: StageRender}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return smtpFailure(ctx, StageConnect, err)
	}
	defer conn.Close()
	// A deadline alone does not react to cancellation before that deadline.
	// Closing the underlying socket interrupts greeting, TLS, AUTH, and DATA.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return smtpFailure(ctx, StageConnect, err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: s.host, RootCAs: s.root}
	limited := &smtpReadLimit{Conn: conn, reader: io.LimitReader(conn, maxRelayBytes)}
	var smtpConn net.Conn = limited
	if s.cfg.TLS == ImplicitTLS {
		secure := tls.Client(limited, tlsCfg)
		if err := secure.HandshakeContext(ctx); err != nil {
			return smtpFailure(ctx, StageTLS, err)
		}
		smtpConn = secure
	}
	c, err := smtp.NewClient(smtpConn, s.host)
	if err != nil {
		return smtpFailure(ctx, StageGreeting, err)
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return smtpFailure(ctx, StageHello, err)
	}
	if s.cfg.TLS == STARTTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &PermanentError{Stage: StageSTARTTLS}
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return smtpFailure(ctx, StageSTARTTLS, err)
		}
	}
	if s.cfg.Username != "" {
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return &PermanentError{Stage: StageAuth}
		}
		auth := authFor(mechs, s.host, s.cfg.Username, s.cfg.Password)
		if auth == nil {
			return &PermanentError{Stage: StageAuth}
		}
		if err := c.Auth(auth); err != nil {
			return smtpFailure(ctx, StageAuth, err)
		}
	}
	if err := c.Mail(s.sender); err != nil {
		return smtpFailure(ctx, StageMail, err)
	}
	if err := c.Rcpt(to); err != nil {
		return smtpFailure(ctx, StageRecipient, err)
	}
	w, err := c.Data()
	if err != nil {
		return smtpFailure(ctx, StageData, err)
	}
	if _, err := w.Write(raw); err != nil {
		return smtpFailure(ctx, StageBody, err)
	}
	if err := w.Close(); err != nil {
		return smtpFailure(ctx, StageBody, err)
	}
	// DATA's final 250 transfers responsibility to the relay. Close directly:
	// waiting for QUIT would delay an accepted send, and treating a QUIT error
	// as failure could deliver the same one-time link twice after a retry.
	return nil
}

func envelopeSender(from string) (string, error) {
	a, err := parseSender(from)
	if err != nil {
		return "", err
	}
	return a.Address, nil
}

func smtpFailure(ctx context.Context, stage SMTPStage, err error) error {
	e := &SMTPError{Stage: stage}
	if ctx.Err() != nil {
		e.cause = ctx.Err()
		return e
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		e.cause = context.DeadlineExceeded
		return e
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		e.Code = te.Code
		if te.Code >= 500 && te.Code < 600 {
			return &PermanentError{Stage: stage, Code: te.Code, Recipient: stage == StageRecipient && mailboxRefused(te.Code, te.Msg)}
		}
	}
	return e
}

// Generic command/authentication errors at RCPT are not mailbox failures.
// Keep suppression conservative when the relay returns an ambiguous status.
func mailboxRefused(code int, reply string) bool {
	// Enhanced statuses distinguish mailbox/address failures (5.1.x/5.2.x)
	// from relay policy, authentication (5.7.x), or system/network failures.
	// Inspect this field for classification but never retain the reply.
	status := strings.TrimSpace(reply)
	if i := strings.IndexAny(status, " \t"); i >= 0 {
		status = status[:i]
	}
	if strings.HasPrefix(status, "5.") && !strings.HasPrefix(status, "5.1.") && !strings.HasPrefix(status, "5.2.") {
		return false
	}
	switch code {
	case 550, 551, 552, 553, 556:
		return true
	default:
		return false
	}
}

// Match whole advertised mechanism names. PLAIN verifies the actual relay
// hostname and refuses cleartext credentials outside explicit localhost tests.
func authFor(mechs, host, username, password string) smtp.Auth {
	var cram bool
	for _, mech := range strings.Fields(mechs) {
		switch mech {
		case "PLAIN":
			return smtp.PlainAuth("", username, password, host)
		case "CRAM-MD5":
			cram = true
		}
	}
	if cram {
		return smtp.CRAMMD5Auth(username, password)
	}
	return nil
}
