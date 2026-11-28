package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"html"
	"io"
	"mime"
	"mime/multipart"
	stdmail "net/mail"
	"strings"
	"testing"
	"time"
)

func TestValidAddress(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"person@example.com", "person@example.com"},
		{" person+tag@example.com ", "person+tag@example.com"},
		{"Person@Example.COM", "Person@example.com"},
		{"", ""},
		{"person@[192.0.2.1]", ""},
		{"person@[IPv6:2001:db8::1]", ""},
		{"Display Name <person@example.com>", ""},
		{"person@example.com, other@example.com", ""},
		{"not-an-address", ""},
		{"person@example.com\r\nBcc: other@example.com", ""},
		{"\r\nperson@example.com", ""},
		{"person@example.com\n", ""},
		{"person\x00@example.com", ""},
		{"pérson@example.com", ""},
		{strings.Repeat("x", 250) + "@example.com", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ValidAddress(tc.input)
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid address accepted")
				}
				if strings.Contains(err.Error(), tc.input) && tc.input != "" {
					t.Fatal("validation error echoes address")
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRenderMIME(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 4, 5, 0, time.UTC)
	m := Message{To: "recipient@example.com", Subject: "MIME test", Text: "Hello, 世界\nline two\rline three", HTML: "<p>Hello, 世界</p>"}
	raw, err := RenderWith("Sender 名 <sender@example.com>", m, now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) || bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\r")) {
		t.Fatal("message contains bare line breaks")
	}
	if !isASCII(strings.ReplaceAll(string(raw), "\r\n", "")) {
		t.Fatal("message is not 7-bit clean")
	}
	parsed, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"To": m.To, "Subject": m.Subject, "MIME-Version": "1.0", "Auto-Submitted": "auto-generated"} {
		if got := parsed.Header.Get(k); got != want {
			t.Errorf("%s = %q; want %q", k, got, want)
		}
	}
	date, err := parsed.Header.Date()
	if err != nil || !date.Equal(now) {
		t.Fatalf("Date = %v, %v", date, err)
	}
	from, err := stdmail.ParseAddress(parsed.Header.Get("From"))
	if err != nil || from.Name != "Sender 名" || from.Address != "sender@example.com" {
		t.Fatalf("From = %v, %v", from, err)
	}
	id := parsed.Header.Get("Message-ID")
	if !strings.HasPrefix(id, "<redrop-") || !strings.HasSuffix(id, "@example.com>") {
		t.Fatalf("invalid Message-ID %q", id)
	}
	kind, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q, %v", kind, err)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	for i, body := range []string{m.Text, m.HTML} {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		contentType, p, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		wantType := []string{"text/plain", "text/html"}[i]
		if err != nil || contentType != wantType || p["charset"] != "utf-8" {
			t.Fatalf("part type = %q, %v", contentType, err)
		}
		if part.Header.Get("Content-Transfer-Encoding") != "base64" {
			t.Fatal("body must use base64 transfer encoding")
		}
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if err != nil || string(decoded) != crlf(body) {
			t.Fatalf("part decoded to %q, %v", decoded, err)
		}
	}
	if _, err := reader.NextPart(); err != io.EOF {
		t.Fatalf("unexpected extra MIME part: %v", err)
	}
	other, err := RenderWith("sender@example.com", m, now)
	if err != nil {
		t.Fatal(err)
	}
	otherParsed, err := stdmail.ReadMessage(bytes.NewReader(other))
	if err != nil || otherParsed.Header.Get("Message-ID") == id {
		t.Fatal("Message-ID was reused")
	}
}

func TestRenderRejectsHeaderInjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		m    Message
	}{
		{"sender", "sender@example.com\r\nBcc: leak@example.com", smtpMessage()},
		{"sender leading newline", "\nsender@example.com", smtpMessage()},
		{"recipient", "sender@example.com", Message{To: "recipient@example.com\r\nBcc: leak@example.com", Subject: "test"}},
		{"subject", "sender@example.com", Message{To: "recipient@example.com", Subject: "test\r\nBcc: leak@example.com"}},
		{"subject leading newline", "sender@example.com", Message{To: "recipient@example.com", Subject: "\ntest"}},
		{"raw recipient", "sender@example.com", Message{To: "recipient@example.com\r\nBcc: leak@example.com", Raw: []byte("stored bytes")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderWith(tc.from, tc.m, time.Now()); err == nil {
				t.Fatal("injected header accepted")
			}
		})
	}
}

func TestRenderRawPreservesBytes(t *testing.T) {
	want := []byte("stored RFC message\r\nwith exact bytes\r\n")
	got, err := RenderWith("sender@example.com", Message{To: "recipient@example.com", Raw: want}, time.Now())
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("raw message changed: %q, %v", got, err)
	}
}

func TestTemplatesEscapeHTML(t *testing.T) {
	attack := `"><script>alert('x')</script>&`
	link := "https://redrop.example.com/invite/one-time?x=1&y=%22"
	invite, err := (InviteEmail{Link: link, Note: attack, Inviter: attack, Role: "admin", Expires: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)}).Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(invite.HTML, "<script>") || !strings.Contains(invite.HTML, html.EscapeString(attack)) || !strings.Contains(invite.HTML, `href="`+html.EscapeString(link)+`"`) {
		t.Fatal("invite HTML failed to escape untrusted text/link")
	}
	if !strings.Contains(invite.Text, attack) || !strings.Contains(invite.Text, link) || !strings.Contains(invite.Subject, "administrator") || !strings.Contains(invite.Text, "28 November 2026") {
		t.Fatal("invite lost required text")
	}
	alert, err := (AlertEmail{QueueURL: "https://redrop.example.com/admin/reports", Interval: 10 * time.Minute, Reports: []ReportAlert{{ID: attack, Category: attack, Source: "visitor", Created: time.Now()}}}).Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(alert.HTML, "<script>") || !strings.Contains(alert.HTML, html.EscapeString(attack)) {
		t.Fatal("alert HTML failed to escape report metadata")
	}
	if strings.Contains(TestEmail(attack).HTML, "<script>") {
		t.Fatal("test email failed to escape server name")
	}
	for _, message := range []Message{invite, alert, TestEmail("example")} {
		if strings.Contains(message.HTML, "<img") || strings.Contains(message.HTML, "<script") || strings.Contains(message.HTML, "stylesheet") {
			t.Fatal("message contains tracking or external resources")
		}
	}
}

func TestTemplatesRequireHTTPLinks(t *testing.T) {
	for _, link := range []string{"", "http-not-a-url", "https:", "https:///path", "//example.com/path", "javascript:alert(1)", "https://user:password@example.com/path", "https://example.com/\r\nsecret"} {
		t.Run(link, func(t *testing.T) {
			if _, err := (InviteEmail{Link: link}).Render(); err == nil {
				t.Fatal("invalid invite link accepted")
			}
			if _, err := (AlertEmail{QueueURL: link, Reports: []ReportAlert{{ID: "report"}}}).Render(); err == nil {
				t.Fatal("invalid moderation link accepted")
			}
		})
	}
}

func TestDiscard(t *testing.T) {
	if err := (Discard{}).Send(context.Background(), smtpMessage()); err != nil {
		t.Fatal(err)
	}
}
