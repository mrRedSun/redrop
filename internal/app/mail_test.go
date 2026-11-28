package app

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	stdmail "net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// smtpSink keeps SMTP I/O in process. hold, when set, blocks the first
// DATA result so the HTTP test proves the request does not await delivery.
type smtpSink struct {
	ln       net.Listener
	bodies   chan []byte
	hold     chan struct{}
	release  sync.Once
	attempts atomic.Int32
	wg       sync.WaitGroup
}

func newSMTPSink(t *testing.T, hold bool, replies ...int) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln, bodies: make(chan []byte, 16)}
	if hold {
		s.hold = make(chan struct{})
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				tp := textproto.NewConn(c)
				tp.PrintfLine("220 smtp.example.com")
				for {
					line, err := tp.ReadLine()
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						tp.PrintfLine("250 smtp.example.com")
					case strings.HasPrefix(line, "HELO"), strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
						tp.PrintfLine("250 ok")
					case line == "DATA":
						tp.PrintfLine("354 message")
						b, err := tp.ReadDotBytes()
						if err != nil {
							return
						}
						s.bodies <- b
						n := int(s.attempts.Add(1))
						if n == 1 && s.hold != nil {
							<-s.hold
						}
						code := 250
						if n <= len(replies) {
							code = replies[n-1]
						}
						tp.PrintfLine("%d result", code)
					case line == "QUIT":
						tp.PrintfLine("221 bye")
						return
					default:
						tp.PrintfLine("500 unexpected command")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { s.unblock(); s.ln.Close(); s.wg.Wait() })
	t.Setenv("REDROP_SMTP_ADDR", ln.Addr().String())
	t.Setenv("REDROP_SMTP_TLS", "plain")
	t.Setenv("REDROP_SMTP_INSECURE_ALLOW_PLAIN", "true")
	t.Setenv("REDROP_MAIL_FROM", "Redrop <share@example.com>")
	return s
}
func (s *smtpSink) unblock() {
	s.release.Do(func() {
		if s.hold != nil {
			close(s.hold)
		}
	})
}
func (s *smtpSink) next(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-s.bodies:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no SMTP delivery")
		return nil
	}
}
func smtpPlainText(t *testing.T, b []byte) string {
	t.Helper()
	m, err := stdmail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := multipart.NewReader(m.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}
func waitMailState(t *testing.T, h *harness, id, status string) api.Invite {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		rows := decode[api.InviteList](t, h.do("GET", "/api/v1/admin/invites", nil, false))
		for _, r := range rows.Items {
			if r.ID == id && r.EmailStatus == status {
				return r
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("invite did not reach %s", status)
			return api.Invite{}
		case <-tick.C:
		}
	}
}
func TestInviteEmailRequestReturnsBeforeDelivery(t *testing.T) {
	sink := newSMTPSink(t, true)
	var logs syncBuffer
	h := newHarnessWith(t, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Options{})
	h.setupAdmin()
	cfg := decode[api.AuthConfig](t, h.do("GET", "/api/v1/auth/config", nil, false))
	if !cfg.Mail {
		t.Fatal("missing mail capability")
	}
	result := make(chan api.Invite, 1)
	go func() {
		r := h.do("POST", "/api/v1/admin/invites", api.CreateInviteRequest{Email: "recipient@example.com", Note: "Welcome <friend>", TTL: "1d"}, true)
		expectStatus(t, r, 201, "email invite")
		result <- decode[api.Invite](t, r)
	}()
	var inv api.Invite
	select {
	case inv = <-result:
	case <-time.After(5 * time.Second):
		sink.unblock()
		t.Fatal("HTTP request waited for SMTP")
	}
	// An emailed invite's link exists only in the email.
	if inv.URL != "" || inv.Email != "recipient@example.com" || inv.EmailStatus != "queued" {
		t.Fatalf("create response: %+v", inv)
	}
	body := sink.next(t)
	text := smtpPlainText(t, body)
	if !strings.Contains(text, "Welcome <friend>") || !strings.Contains(text, "Invited by admin.") {
		t.Fatalf("invite template missing fields (or repeats the inviter):\n%s", text)
	}
	first := emailedToken(t, text)
	expectStatus(t, h.do("POST", "/api/v1/admin/invites/"+inv.ID+"/resend", nil, false), 403, "resend needs CSRF")
	expectStatus(t, h.do("POST", "/api/v1/admin/invites/"+inv.ID+"/resend", nil, true), 409, "already sending")
	sink.unblock()
	sent := waitMailState(t, h, inv.ID, store.MailSent)
	if sent.URL != "" || sent.EmailAt == nil {
		t.Fatal("list leaked URL or lost send timestamp")
	}
	backupHoldsNoToken(t, h, first)

	// Send again mints a new link and a new message; the first link dies.
	expectStatus(t, h.do("POST", "/api/v1/admin/invites/"+inv.ID+"/resend", nil, true), 200, "resend")
	again := sink.next(t)
	second := emailedToken(t, smtpPlainText(t, again))
	if second == first || emailMessageID(t, again) == emailMessageID(t, body) {
		t.Fatal("Send again resent the same link or Message-ID")
	}
	waitMailState(t, h, inv.ID, store.MailSent)
	for _, secret := range []string{first, second} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("link appeared in logs")
		}
	}
	if strings.Contains(logs.String(), "recipient@example.com") {
		t.Fatal("recipient appeared in logs")
	}
	accept := func(token string) *http.Response {
		return h.do("POST", "/api/v1/invites/"+token+"/accept", api.AcceptInviteRequest{Username: "recipient", Password: "recipient-password-long"}, true)
	}
	expectStatus(t, accept(first), 410, "the replaced link")
	expectStatus(t, accept(second), 200, "redeem the emailed link")
	expectStatus(t, h.do("POST", "/api/v1/admin/invites/"+inv.ID+"/resend", nil, true), 409, "used invite resend")
}

// emailedToken returns the invite token in an invite email's text.
func emailedToken(t *testing.T, text string) string {
	t.Helper()
	const prefix = "http://app.test/invite/"
	i := strings.Index(text, prefix)
	if i < 0 {
		t.Fatalf("no invite link in email:\n%s", text)
	}
	token, _, _ := strings.Cut(text[i+len(prefix):], "\n")
	return strings.TrimSpace(token)
}

func emailMessageID(t *testing.T, raw []byte) string {
	t.Helper()
	m, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m.Header.Get("Message-ID")
}

// backupHoldsNoToken is the review's backup probe, inverted (H-1): a copy of
// the database together with secret.key holds no invite token in any form,
// so no invite email can be rebuilt from a backup.
func backupHoldsNoToken(t *testing.T, h *harness, token string) {
	t.Helper()
	dir := t.TempDir()
	if err := h.app.db.Backup(t.Context(), filepath.Join(dir, "redrop.db")); err != nil {
		t.Fatal(err)
	}
	var files [][]byte
	for _, p := range []string{filepath.Join(dir, "redrop.db"), filepath.Join(h.app.cfg.DataDir, "secret.key")} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, b)
	}
	for _, b := range files {
		if bytes.Contains(b, []byte(token)) {
			t.Fatal("the backup holds the emailed invite token")
		}
	}
	var stored int
	if err := h.app.db.DB.QueryRow(`SELECT count(*) FROM mail_out WHERE kind='invite' AND length(body)>0`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("%d invite emails stored: %v", stored, err)
	}
}

func TestSMTPDeliveryRetriesTransientButNotPermanent(t *testing.T) {
	for _, code := range []int{451, 550} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			sink := newSMTPSink(t, false, code)
			h := newHarness(t)
			h.setupAdmin()
			r := h.do("POST", "/api/v1/admin/invites", api.CreateInviteRequest{Email: "recipient@example.com"}, true)
			expectStatus(t, r, 201, "create invite")
			inv := decode[api.Invite](t, r)
			sink.next(t)
			if code == 451 {
				// The retry is a minute away; bring it forward once the
				// failure is recorded rather than waiting.
				deadline := time.Now().Add(5 * time.Second)
				for {
					row, err := h.app.db.MailByInvite(t.Context(), inv.ID)
					if err != nil {
						t.Fatal(err)
					}
					if row.Status == store.MailQueued && row.Attempts == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("451 not scheduled for retry: %+v", row)
					}
					time.Sleep(5 * time.Millisecond)
				}
				if _, err := h.app.db.DB.Exec(`UPDATE mail_out SET next_at=0`); err != nil {
					t.Fatal(err)
				}
				h.app.mail.Wake()
				sink.next(t)
				waitMailState(t, h, inv.ID, store.MailSent)
				row, err := h.app.db.MailByInvite(t.Context(), inv.ID)
				if err != nil || row.Attempts != 2 {
					t.Fatal("4xx was not retried exactly once")
				}
			} else {
				failed := waitMailState(t, h, inv.ID, store.MailFailed)
				if !strings.Contains(failed.EmailError, "550") || sink.attempts.Load() != 1 {
					t.Fatal("permanent failure lost reason or retried")
				}
			}
		})
	}
}

func TestReportHookDeliversPrivateEmailDigest(t *testing.T) {
	sink := newSMTPSink(t, false)
	t.Setenv("REDROP_NOTIFY_EMAIL", "ops@example.com")
	// The long interval makes batching deterministic; bring its persisted
	// deadline forward after filing both reports rather than sleeping.
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, "report target", client.PublishOptions{})
	for _, network := range []string{"192.0.2.1", "192.0.2.2"} {
		expectStatus(t, h.report(network, api.ReportRequest{URL: a.URL, Category: "spam", Details: "PRIVATE REPORT TEXT", Contact: "private@example.com"}), 202, "report")
	}
	reports := h.reports("")
	if len(reports) != 2 {
		t.Fatal("reports not stored")
	}
	if _, err := h.app.db.DB.Exec(`UPDATE mail_alerts SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	h.app.mail.Wake()
	body := smtpPlainText(t, sink.next(t))
	for _, r := range reports {
		if !strings.Contains(body, r.ID) {
			t.Fatal("report missing from digest")
		}
	}
	if !strings.Contains(body, "http://app.test/admin/reports") {
		t.Fatal("missing moderation link")
	}
	for _, secret := range []string{a.URL, "PRIVATE REPORT TEXT", "private@example.com"} {
		if strings.Contains(body, secret) {
			t.Fatal("digest leaked private report fields")
		}
	}
}

func TestMailOffKeepsManualInvites(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	cfg := decode[api.AuthConfig](t, h.do("GET", "/api/v1/auth/config", nil, false))
	if cfg.Mail {
		t.Fatal("mail enabled without SMTP")
	}
	expectStatus(t, h.do("POST", "/api/v1/admin/invites", api.CreateInviteRequest{Email: "recipient@example.com"}, true), 400, "mail off")
	rows := decode[api.InviteList](t, h.do("GET", "/api/v1/admin/invites", nil, false))
	if len(rows.Items) != 0 {
		t.Fatal("rejected email created an invite")
	}
	r := h.do("POST", "/api/v1/admin/invites", api.CreateInviteRequest{Note: "manual"}, true)
	expectStatus(t, r, 201, "manual invite")
	inv := decode[api.Invite](t, r)
	if inv.URL == "" || inv.Email != "" || inv.EmailStatus != "" {
		t.Fatalf("manual invite changed: %+v", inv)
	}
}
