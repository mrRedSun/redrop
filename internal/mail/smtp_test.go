package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretLink = "https://redrop.example.com/invite/private-test-token"

type smtpFixture struct {
	implicit bool
	startTLS bool
	tls      *tls.Config
	auth     string
	replies  map[string]string
	stall    string
}

type fakeSMTP struct {
	addr     string
	entered  chan string
	messages chan []byte
	done     chan struct{}
	mu       sync.Mutex
	commands []string
}

// Each fixture accepts one connection. Cleanup closes both the listener and
// accepted socket, including when a client never completes its TLS handshake.
func startSMTP(t *testing.T, cfg smtpFixture) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{addr: ln.Addr().String(), entered: make(chan string, 16), messages: make(chan []byte, 1), done: make(chan struct{})}
	var socketMu sync.Mutex
	var socket net.Conn
	t.Cleanup(func() {
		ln.Close()
		socketMu.Lock()
		if socket != nil {
			socket.Close()
		}
		socketMu.Unlock()
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			t.Error("fake SMTP did not stop")
		}
	})
	go func() {
		defer close(s.done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		socketMu.Lock()
		socket = conn
		socketMu.Unlock()
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if cfg.implicit && cfg.stall == "tls" {
			s.entered <- "tls"
			io.Copy(io.Discard, conn)
			return
		}
		if cfg.implicit {
			conn = tls.Server(conn, cfg.tls)
		}
		tp := textproto.NewConn(conn)
		if cfg.stall == "greeting" {
			s.entered <- "greeting"
			io.Copy(io.Discard, conn)
			return
		}
		greeting := "220 smtp.example.com ready"
		if r := cfg.replies["greeting"]; r != "" {
			greeting = r
		}
		if err := tp.PrintfLine("%s", greeting); err != nil {
			return
		}
		tlsActive := cfg.implicit
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.commands = append(s.commands, line)
			s.mu.Unlock()
			verb, _, _ := strings.Cut(line, " ")
			s.entered <- verb
			if cfg.stall == verb {
				io.Copy(io.Discard, conn)
				return
			}
			if r := cfg.replies[verb]; r != "" {
				tp.PrintfLine("%s", r)
				continue
			}
			switch verb {
			case "EHLO":
				tp.PrintfLine("250-smtp.example.com")
				if cfg.startTLS && !tlsActive {
					tp.PrintfLine("250-STARTTLS")
				}
				if cfg.auth != "" {
					tp.PrintfLine("250-AUTH %s", cfg.auth)
				}
				tp.PrintfLine("250 OK")
			case "HELO", "MAIL", "RCPT", "RSET":
				tp.PrintfLine("250 OK")
			case "STARTTLS":
				tp.PrintfLine("220 begin TLS")
				if cfg.stall == "tls" {
					s.entered <- "tls"
					io.Copy(io.Discard, conn)
					return
				}
				conn = tls.Server(conn, cfg.tls)
				tp = textproto.NewConn(conn)
				tlsActive = true
			case "AUTH":
				if line == "AUTH CRAM-MD5" {
					challenge := "<fixture@smtp.example.com>"
					tp.PrintfLine("334 %s", base64.StdEncoding.EncodeToString([]byte(challenge)))
					response, err := tp.ReadLine()
					if err != nil {
						return
					}
					decoded, err := base64.StdEncoding.DecodeString(response)
					mac := hmac.New(md5.New, []byte("fixture-password"))
					mac.Write([]byte(challenge))
					if err != nil || string(decoded) != fmt.Sprintf("fixture-user %x", mac.Sum(nil)) {
						tp.PrintfLine("535 bad challenge response")
						continue
					}
				}
				tp.PrintfLine("235 authenticated")
			case "DATA":
				tp.PrintfLine("354 send message")
				raw, err := io.ReadAll(tp.DotReader())
				if err != nil {
					return
				}
				s.messages <- raw
				s.entered <- "accepted"
				if cfg.stall == "accepted" {
					io.Copy(io.Discard, conn)
					return
				}
				reply := cfg.replies["accepted"]
				if reply == "" {
					reply = "250 queued"
				}
				tp.PrintfLine("%s", reply)
			case "QUIT":
				tp.PrintfLine("221 bye")
				return
			default:
				tp.PrintfLine("500 unexpected command")
			}
		}
	}()
	return s
}

func (s *fakeSMTP) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func smtpCertificates(t *testing.T) (*tls.Config, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SMTP fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, caPath
}

func smtpMessage() Message {
	return Message{To: "recipient@example.com", Subject: "SMTP test", Text: secretLink, HTML: "<p>" + secretLink + "</p>"}
}

func smtpClient(t *testing.T, server *fakeSMTP, mode TLSMode, ca string) *SMTP {
	t.Helper()
	m, err := NewSMTP(SMTPConfig{Addr: server.addr, TLS: mode, CAFile: ca, From: "Sender <sender@example.com>", InsecurePlain: mode == Plain})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSMTPTransport(t *testing.T) {
	cert, ca := smtpCertificates(t)
	for _, tc := range []struct {
		name string
		mode TLSMode
		cfg  smtpFixture
		ca   string
	}{
		{"plain explicit opt-in", Plain, smtpFixture{}, ""},
		{"starttls custom CA", STARTTLS, smtpFixture{startTLS: true, tls: cert}, ca},
		{"implicit TLS custom CA", ImplicitTLS, smtpFixture{implicit: true, tls: cert}, ca},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startSMTP(t, tc.cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := smtpClient(t, srv, tc.mode, tc.ca).Send(ctx, smtpMessage()); err != nil {
				t.Fatal(err)
			}
			select {
			case raw := <-srv.messages:
				if !bytes.Contains(raw, []byte("Subject: SMTP test")) {
					t.Fatal("message was not submitted")
				}
			default:
				t.Fatal("server received no message")
			}
		})
	}
}

func TestSMTPRequiredTLS(t *testing.T) {
	cert, _ := smtpCertificates(t)
	for _, tc := range []struct {
		name string
		mode TLSMode
		cfg  smtpFixture
	}{
		{"STARTTLS missing", STARTTLS, smtpFixture{}},
		{"STARTTLS untrusted CA", STARTTLS, smtpFixture{startTLS: true, tls: cert}},
		{"implicit untrusted CA", ImplicitTLS, smtpFixture{implicit: true, tls: cert}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startSMTP(t, tc.cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := smtpClient(t, srv, tc.mode, "").Send(ctx, smtpMessage()); err == nil {
				t.Fatal("unverified TLS accepted")
			}
			for _, c := range srv.snapshot() {
				if strings.HasPrefix(c, "MAIL ") || strings.HasPrefix(c, "AUTH ") {
					t.Fatal("sent message or credentials without verified TLS")
				}
			}
		})
	}
}

func TestSMTPAuthRelayHost(t *testing.T) {
	cert, ca := smtpCertificates(t)
	srv := startSMTP(t, smtpFixture{startTLS: true, tls: cert, auth: "PLAIN"})
	m := smtpClient(t, srv, STARTTLS, ca)
	m.cfg.Username, m.cfg.Password = "fixture-user", "fixture-password"
	if err := m.Send(context.Background(), smtpMessage()); err != nil {
		t.Fatal(err)
	}
	want := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00fixture-user\x00fixture-password"))
	for _, c := range srv.snapshot() {
		if c == want {
			return
		}
	}
	t.Fatal("PLAIN authentication not sent to configured host")
}

func TestSMTPAuthModes(t *testing.T) {
	cert, ca := smtpCertificates(t)
	for _, tc := range []struct {
		name string
		mode TLSMode
		mech string
	}{
		{"implicit PLAIN", ImplicitTLS, "PLAIN"},
		{"STARTTLS CRAM-MD5", STARTTLS, "CRAM-MD5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startSMTP(t, smtpFixture{implicit: tc.mode == ImplicitTLS, startTLS: tc.mode == STARTTLS, tls: cert, auth: tc.mech})
			m := smtpClient(t, srv, tc.mode, ca)
			m.cfg.Username, m.cfg.Password = "fixture-user", "fixture-password"
			if err := m.Send(context.Background(), smtpMessage()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSMTPHostnameVerification(t *testing.T) {
	cert, ca := smtpCertificates(t)
	for _, mode := range []TLSMode{STARTTLS, ImplicitTLS} {
		t.Run(string(mode), func(t *testing.T) {
			srv := startSMTP(t, smtpFixture{implicit: mode == ImplicitTLS, startTLS: mode == STARTTLS, tls: cert})
			m := smtpClient(t, srv, mode, ca)
			// Separate the connection address from the requested certificate
			// name, as if DNS resolved the relay name to the fixture socket.
			m.host = "wrong.example.com"
			if err := m.Send(context.Background(), smtpMessage()); err == nil {
				t.Fatal("trusted certificate for another host accepted")
			}
			for _, command := range srv.snapshot() {
				if strings.HasPrefix(command, "MAIL ") {
					t.Fatal("message sent despite hostname mismatch")
				}
			}
		})
	}
}

func TestSMTPReplyRedaction(t *testing.T) {
	for _, stage := range []string{"greeting", "EHLO", "MAIL", "RCPT", "DATA", "accepted"} {
		t.Run(stage, func(t *testing.T) {
			replies := map[string]string{stage: "451 echo " + secretLink}
			if stage == "EHLO" {
				replies["HELO"] = replies[stage]
			}
			srv := startSMTP(t, smtpFixture{replies: replies})
			err := smtpClient(t, srv, Plain, "").Send(context.Background(), smtpMessage())
			if err == nil {
				t.Fatal("SMTP refusal succeeded")
			}
			var log bytes.Buffer
			slog.New(slog.NewTextHandler(&log, nil)).Error("send failed", "error", err)
			for _, text := range []string{fmt.Sprintf("%+v", err), log.String()} {
				if strings.Contains(text, secretLink) || strings.Contains(text, "private-test-token") {
					t.Fatalf("SMTP reply leaked in error/log: %s", text)
				}
			}
		})
	}
}

func TestSMTPCancelClosesSocket(t *testing.T) {
	for _, stage := range []string{"greeting", "EHLO", "MAIL", "RCPT", "DATA", "accepted"} {
		t.Run(stage, func(t *testing.T) {
			srv := startSMTP(t, smtpFixture{stall: stage})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			m := smtpClient(t, srv, Plain, "")
			go func() { result <- m.Send(ctx, smtpMessage()) }()
			for {
				select {
				case at := <-srv.entered:
					if at == stage {
						cancel()
						goto canceled
					}
				case <-time.After(2 * time.Second):
					t.Fatal("client never reached stalled stage")
				}
			}
		canceled:
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("want context cancellation, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt SMTP read")
			}
			select {
			case <-srv.done:
			case <-time.After(time.Second):
				t.Fatal("cancellation did not close SMTP socket")
			}
		})
	}
}

// Check that even a syntactically invalid SMTP reply cannot escape through a
// generic protocol error, rather than textproto.Error's ordinary status path.
func TestSMTPMalformedReplyRedacted(t *testing.T) {
	srv := startSMTP(t, smtpFixture{replies: map[string]string{"greeting": "wat " + secretLink}})
	err := smtpClient(t, srv, Plain, "").Send(context.Background(), smtpMessage())
	if err == nil || strings.Contains(err.Error(), secretLink) {
		t.Fatalf("unsafe protocol error: %v", err)
	}
}

func TestSMTPFailureClassification(t *testing.T) {
	cert, ca := smtpCertificates(t)
	for _, tc := range []struct {
		name      string
		verb      string
		code      int
		stage     SMTPStage
		permanent bool
		recipient bool
	}{
		{"transient recipient", "RCPT", 450, StageRecipient, false, false},
		// A bare 550/553 without an enhanced status fails the message but
		// does not suppress the address (relays also use it for policy).
		{"bare permanent recipient", "RCPT", 550, StageRecipient, true, false},
		{"bare invalid mailbox", "RCPT", 553, StageRecipient, true, false},
		{"auth required at RCPT", "RCPT", 530, StageRecipient, true, false},
		{"bad command at RCPT", "RCPT", 503, StageRecipient, true, false},
		{"ambiguous transaction at RCPT", "RCPT", 554, StageRecipient, true, false},
		{"transient authentication", "AUTH", 454, StageAuth, false, false},
		{"permanent authentication", "AUTH", 535, StageAuth, true, false},
		{"permanent sender", "MAIL", 550, StageMail, true, false},
		{"transient sender", "MAIL", 451, StageMail, false, false},
		{"permanent DATA", "DATA", 554, StageData, true, false},
		{"transient DATA", "DATA", 451, StageData, false, false},
		{"permanent body", "accepted", 550, StageBody, true, false},
		{"transient body", "accepted", 451, StageBody, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startSMTP(t, smtpFixture{startTLS: true, tls: cert, auth: "PLAIN", replies: map[string]string{tc.verb: fmt.Sprintf("%d echo %s", tc.code, secretLink)}})
			m := smtpClient(t, srv, STARTTLS, ca)
			m.cfg.Username, m.cfg.Password = "fixture-user", "fixture-password"
			err := m.Send(context.Background(), smtpMessage())
			var permanent *PermanentError
			if got := errors.As(err, &permanent); got != tc.permanent {
				t.Fatalf("permanent=%v; want %v; error %v", got, tc.permanent, err)
			}
			if permanent != nil && permanent.Recipient != tc.recipient {
				t.Fatalf("recipient=%v; want %v", permanent.Recipient, tc.recipient)
			}
			var status *SMTPError
			if !errors.As(err, &status) || status.Stage != tc.stage || status.Code != tc.code {
				t.Fatalf("wrong SMTP stage/status: %+v, error %v", status, err)
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(fmt.Sprintf("%+v", e), "private-test-token") {
					t.Fatal("SMTP error chain retains secret server text")
				}
			}
		})
	}
}

func TestSMTPEnhancedRecipientStatus(t *testing.T) {
	for _, tc := range []struct {
		reply     string
		recipient bool
	}{
		{"550 5.1.1 no such user", true},
		{"553 5.1.3 bad address syntax", true},
		{"552 5.2.2 mailbox full", true},
		{"550 5.2.1 mailbox disabled", true},
		{"550 5.7.1 relaying denied", false},
		{"550 5.3.0 system error", false},
		{"550 5.4.1 no route", false},
		{"550 5.0.0 unknown", false},
		{"550 Relaying denied", false},
		{"550 no such user here", false},
		{"554 5.1.1 transaction failed", false},
		{"550", false},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			srv := startSMTP(t, smtpFixture{replies: map[string]string{"RCPT": tc.reply + " " + secretLink}})
			err := smtpClient(t, srv, Plain, "").Send(context.Background(), smtpMessage())
			var permanent *PermanentError
			if !errors.As(err, &permanent) || permanent.Recipient != tc.recipient {
				t.Fatalf("enhanced status classified incorrectly: %+v", err)
			}
		})
	}
}

func TestSMTPUnsupportedAuth(t *testing.T) {
	for _, mech := range []string{"", "LOGIN", "XPLAIN"} {
		t.Run(mech, func(t *testing.T) {
			srv := startSMTP(t, smtpFixture{auth: mech})
			m := smtpClient(t, srv, Plain, "")
			m.cfg.Username, m.cfg.Password = "fixture-user", "fixture-password"
			err := m.Send(context.Background(), smtpMessage())
			var permanent *PermanentError
			if !errors.As(err, &permanent) || permanent.Stage != StageAuth || permanent.Recipient {
				t.Fatalf("wrong AUTH refusal: %v", err)
			}
			for _, command := range srv.snapshot() {
				if strings.HasPrefix(command, "AUTH ") || strings.HasPrefix(command, "MAIL ") {
					t.Fatal("sent credentials/message without supported AUTH")
				}
			}
		})
	}
}

func TestSMTPDeadline(t *testing.T) {
	srv := startSMTP(t, smtpFixture{stall: "greeting"})
	m := smtpClient(t, srv, Plain, "")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := m.Send(ctx, smtpMessage())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context deadline, got %v", err)
	}
	var permanent *PermanentError
	if errors.As(err, &permanent) {
		t.Fatal("timeout classified as permanent")
	}
	select {
	case <-srv.done:
	case <-time.After(time.Second):
		t.Fatal("deadline did not close socket")
	}
}

func TestSMTPCancelTLSAndAuth(t *testing.T) {
	cert, ca := smtpCertificates(t)
	for _, tc := range []struct {
		name string
		mode TLSMode
		cfg  smtpFixture
		at   string
	}{
		{"implicit handshake", ImplicitTLS, smtpFixture{implicit: true, tls: cert, stall: "tls"}, "tls"},
		{"STARTTLS handshake", STARTTLS, smtpFixture{startTLS: true, tls: cert, stall: "tls"}, "tls"},
		{"AUTH", STARTTLS, smtpFixture{startTLS: true, tls: cert, auth: "PLAIN", stall: "AUTH"}, "AUTH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startSMTP(t, tc.cfg)
			m := smtpClient(t, srv, tc.mode, ca)
			if tc.at == "AUTH" {
				m.cfg.Username, m.cfg.Password = "fixture-user", "fixture-password"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- m.Send(ctx, smtpMessage()) }()
			for {
				select {
				case at := <-srv.entered:
					if at == tc.at {
						cancel()
						goto canceled
					}
				case <-time.After(2 * time.Second):
					t.Fatal("client never reached TLS/AUTH stall")
				}
			}
		canceled:
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("want cancellation, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("TLS/AUTH ignored cancellation")
			}
			select {
			case <-srv.done:
			case <-time.After(time.Second):
				t.Fatal("TLS/AUTH cancellation did not close socket")
			}
		})
	}
}

func TestSMTPConfiguration(t *testing.T) {
	for _, change := range []func(*SMTPConfig){
		func(c *SMTPConfig) { c.Addr = "bad" },
		func(c *SMTPConfig) { c.Addr = ":25" },
		func(c *SMTPConfig) { c.Addr = "smtp.example.com:0" },
		func(c *SMTPConfig) { c.Addr = "smtp.example.com:65536" },
		func(c *SMTPConfig) { c.Addr = "smtp.example.com:smtp" },
		func(c *SMTPConfig) { c.TLS = "unknown" },
		func(c *SMTPConfig) { c.TLS = Plain },
		func(c *SMTPConfig) { c.From = "" },
		func(c *SMTPConfig) { c.From = "sender@example.com\r\nBcc: other@example.com" },
		func(c *SMTPConfig) { c.Username = "user" },
		func(c *SMTPConfig) { c.Password = "password" },
		func(c *SMTPConfig) { c.Username, c.Password = "user", "password\r\n" },
		func(c *SMTPConfig) { c.CAFile = filepath.Join(t.TempDir(), "missing.pem") },
	} {
		cfg := SMTPConfig{Addr: "smtp.example.com:587", TLS: STARTTLS, From: "sender@example.com"}
		change(&cfg)
		if _, err := NewSMTP(cfg); err == nil {
			t.Fatal("invalid SMTP configuration accepted")
		}
	}
	// n-3: no AUTH without TLS except to a loopback relay.
	if _, err := NewSMTP(SMTPConfig{Addr: "smtp.example.com:25", TLS: Plain, InsecurePlain: true, From: "sender@example.com", Username: "user", Password: "password"}); err == nil {
		t.Fatal("cleartext AUTH to a remote relay accepted")
	}
	for _, host := range []string{"127.0.0.1", "[::1]", "localhost"} {
		if _, err := NewSMTP(SMTPConfig{Addr: host + ":25", TLS: Plain, InsecurePlain: true, From: "sender@example.com", Username: "user", Password: "password"}); err != nil {
			t.Fatalf("loopback test relay %s refused: %v", host, err)
		}
	}
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSMTP(SMTPConfig{Addr: "smtp.example.com:587", TLS: STARTTLS, From: "sender@example.com", CAFile: path}); err == nil {
		t.Fatal("invalid private CA accepted")
	}
}

func TestSMTPSendRejectsInvalidEnvelope(t *testing.T) {
	// The relay address intentionally has no listener: validation must run
	// before connecting, including for a previously rendered Raw message.
	m, err := NewSMTP(SMTPConfig{Addr: "127.0.0.1:1", TLS: Plain, InsecurePlain: true, From: "sender@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{nil, []byte("stored RFC message")} {
		err := m.Send(context.Background(), Message{To: "recipient@example.com\r\nRCPT TO:<other@example.com>", Subject: "test", Raw: raw})
		var permanent *PermanentError
		if !errors.As(err, &permanent) || permanent.Stage != StageValidation || permanent.Recipient {
			t.Fatalf("invalid envelope reached network: %v", err)
		}
	}
}

func TestSMTPBoundsRelayInput(t *testing.T) {
	srv := startSMTP(t, smtpFixture{replies: map[string]string{"greeting": "220 " + strings.Repeat("x", maxRelayBytes) + secretLink}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := smtpClient(t, srv, Plain, "").Send(ctx, smtpMessage())
	var status *SMTPError
	if !errors.As(err, &status) || status.Stage != StageGreeting || status.Code != 0 || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("oversized reply not rejected at input limit: %v", err)
	}
	if strings.Contains(err.Error(), secretLink) {
		t.Fatal("oversized response leaked link")
	}
}

func TestSMTPAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m, err := NewSMTP(SMTPConfig{Addr: "127.0.0.1:1", TLS: Plain, InsecurePlain: true, From: "sender@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(ctx, smtpMessage()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context cancellation, got %v", err)
	}
}

func TestSMTPHelloNamesSenderDomain(t *testing.T) {
	srv := startSMTP(t, smtpFixture{})
	if err := smtpClient(t, srv, Plain, "").Send(context.Background(), smtpMessage()); err != nil {
		t.Fatal(err)
	}
	if got := srv.snapshot(); len(got) == 0 || got[0] != "EHLO example.com" {
		t.Fatalf("EHLO did not name the sender's domain: %v", got)
	}
}

func TestSMTPWireUsesCRLF(t *testing.T) {
	// DotReader normalizes CRLF when reading DATA. RenderWith's own MIME
	// tests cover line endings before dot stuffing on the wire.
	raw, err := RenderWith("sender@example.com", smtpMessage(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s := bufio.NewScanner(bytes.NewReader(raw))
	for s.Scan() {
		if len(s.Bytes()) > 998 {
			t.Fatal("message exceeds SMTP line length")
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
}
