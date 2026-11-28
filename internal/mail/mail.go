// Package mail is redrop's provider-neutral outbound mail layer (DESIGN.md
// "Email"): a Mailer interface with an SMTP implementation, RFC 5322 message
// rendering, and the database-backed send queue that invite emails and
// report alerts go through. No send ever happens inside an HTTP request.
package mail

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"net/mail"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
)

// Message is one outbound email. To is a bare address; Subject, Text and
// HTML are the message the templates produce. Raw, when set, is a fully
// rendered RFC 5322 message that skips template rendering (used by the
// queue to resend a stored row).
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
	Raw     []byte
}

// Mailer delivers a message. Implementations must never log the message
// body: it can hold a one-time link.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Discard is the mailer used when nothing should be sent.
type Discard struct{}

func (Discard) Send(context.Context, Message) error { return nil }

// ValidAddress checks one bare address (no display name) of at most 254
// characters and returns it trimmed, with its domain in lower case so that
// the bounce list and alert state match however the domain was typed (the
// local part keeps its case: RFC 5321 leaves it to the receiving server).
// Domain literals such as a@[192.0.2.1] are refused: they would make the
// relay deliver straight to an address. Invite emails and alert recipients
// pass through it before anything is stored.
func ValidAddress(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("an email address must not contain line breaks")
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 || !isASCII(s) {
		return "", fmt.Errorf("an email address of up to 254 characters")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", fmt.Errorf("a valid email address, without a display name")
	}
	i := strings.LastIndexByte(s, '@')
	if i < 1 || strings.ContainsAny(s[i+1:], "[]") {
		return "", fmt.Errorf("an email address with a domain name, not an IP address")
	}
	return NormalizeAddress(s), nil
}

// NormalizeAddress lower-cases the domain of an address already checked by
// ValidAddress. Command-line tools use it to name a bounce-list entry.
func NormalizeAddress(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		return s[:i+1] + strings.ToLower(s[i+1:])
	}
	return s
}

// esc escapes text for the HTML part.
func esc(s string) string { return html.EscapeString(s) }

// RenderWith builds the RFC 5322 bytes of m: From, To, Subject, Date,
// Message-ID, MIME-Version, Auto-Submitted, and a multipart/alternative body
// with a plain-text and an HTML part, both base64-encoded so the message is
// 7-bit clean for any relay. There is no tracking: no images, no external
// styles, and the only links are the ones the templates put in. A message
// with Raw set renders as its own bytes. Each call mints a new Message-ID
// and Date, so a resend is a new message, not a duplicate.
func RenderWith(from string, m Message, now time.Time) ([]byte, error) {
	fromAddr, err := parseSender(from)
	if err != nil {
		return nil, err
	}
	to, err := ValidAddress(m.To)
	if err != nil {
		return nil, fmt.Errorf("recipient: %w", err)
	}
	if m.Raw != nil {
		return m.Raw, nil
	}
	subject := strings.TrimSpace(m.Subject)
	if subject == "" || len(subject) > 200 || strings.ContainsAny(m.Subject, "\r\n") || !isASCII(subject) {
		return nil, fmt.Errorf("subject must be one ASCII line of up to 200 characters")
	}
	boundary := "redrop-" + ids.Random(24)
	var b strings.Builder
	writeHeader := func(k, v string) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	writeHeader("From", fromAddr.String())
	writeHeader("To", to)
	writeHeader("Subject", subject)
	writeHeader("Date", now.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	writeHeader("Message-ID", "<redrop-"+ids.Random(24)+"@"+domainOf(fromAddr.Address)+">")
	writeHeader("MIME-Version", "1.0")
	writeHeader("Auto-Submitted", "auto-generated")
	writeHeader("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", boundary))
	b.WriteString("\r\n")
	writePart(&b, boundary, "text/plain; charset=utf-8", m.Text)
	writePart(&b, boundary, "text/html; charset=utf-8", m.HTML)
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String()), nil
}

// writePart appends one base64-encoded alternative body.
func writePart(b *strings.Builder, boundary, contentType, body string) {
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: " + contentType + "\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64Lines(crlf(body))
	for _, line := range enc {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
}

// base64Lines encodes s and wraps it at 76 columns.
func base64Lines(s string) []string {
	const cols = 76
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	out := make([]string, 0, len(enc)/cols+1)
	for len(enc) > cols {
		out = append(out, enc[:cols])
		enc = enc[cols:]
	}
	if len(enc) > 0 {
		out = append(out, enc)
	}
	return out
}

// crlf normalizes every line break to CRLF and drops stray CRs.
func crlf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// parseSender accepts a bare address or a "Display Name <addr>" form and
// returns the parsed address for header rendering.
func parseSender(from string) (*mail.Address, error) {
	if strings.ContainsAny(from, "\r\n") {
		return nil, fmt.Errorf("invalid sender address")
	}
	a, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil || a.Address == "" {
		return nil, fmt.Errorf("invalid sender address")
	}
	if _, err := ValidAddress(a.Address); err != nil {
		return nil, fmt.Errorf("invalid sender address")
	}
	return a, nil
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return "localhost"
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7e || s[i] < 0x20 {
			return false
		}
	}
	return true
}
