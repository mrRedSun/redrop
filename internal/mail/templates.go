package mail

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Templates for the two kinds of mail redrop sends. Every untrusted or
// user-supplied value, including link attributes, goes through esc. Links
// must also be absolute HTTP(S) URLs; HTML escaping alone cannot make a
// javascript: URL safe.

// InviteEmail is the message sent for an invite. The link is the one-time
// URL; the note and inviter are admin-supplied but still escaped.
type InviteEmail struct {
	Link    string
	Note    string
	Inviter string
	Expires time.Time
	Role    string
}

// Render returns the two bodies of the invite email.
func (e InviteEmail) Render() (Message, error) {
	if !validLink(e.Link) {
		return Message{}, fmt.Errorf("invite link must be an absolute HTTP(S) URL")
	}
	subject := "You are invited to redrop"
	if e.Role == "admin" {
		subject = "You are invited to redrop as an administrator"
	}
	note := strings.TrimSpace(e.Note)
	text := strings.Join(nonEmpty(
		"Someone has invited you to redrop, a self-hosted artifact sharing server.",
		inviterLine(e.Inviter, note),
		"",
		"Open this one-time link to create your account:",
		e.Link,
		"",
		"The link expires "+humanTime(e.Expires)+". It can only be used once.",
		"If you were not expecting this, ignore this message.",
	), "\n")
	link := esc(e.Link)
	htmlBody := wrapHTML("You are invited", inviteHTML(e, link))
	return Message{Subject: subject, Text: text, HTML: htmlBody}, nil
}

func inviterLine(inviter, note string) string {
	parts := []string{}
	if inviter != "" {
		parts = append(parts, "Invited by "+inviter+".")
	}
	if note != "" {
		parts = append(parts, "Note: "+note)
	}
	return strings.Join(parts, " ")
}

func inviteHTML(e InviteEmail, link string) string {
	var b strings.Builder
	b.WriteString("<p>Someone has invited you to redrop, a self-hosted artifact sharing server.</p>\n")
	if e.Inviter != "" || e.Note != "" {
		b.WriteString("<p>")
		if e.Inviter != "" {
			b.WriteString("Invited by <strong>" + esc(e.Inviter) + "</strong>. ")
		}
		if e.Note != "" {
			b.WriteString("Note: " + esc(e.Note))
		}
		b.WriteString("</p>\n")
	}
	b.WriteString("<p>Open this one-time link to create your account:</p>\n")
	b.WriteString("<p><a href=\"" + link + "\">" + link + "</a></p>\n")
	b.WriteString("<p class=\"muted\">The link expires " + esc(humanTime(e.Expires)) + " and can only be used once.</p>\n")
	b.WriteString("<p class=\"muted\">If you were not expecting this, ignore this message.</p>\n")
	return b.String()
}

// ReportAlert is one line of a batched report alert.
type ReportAlert struct {
	ID       string
	Category string
	Source   string // visitor | system
	Created  time.Time
}

// AlertEmail is the digest message to one operator address. It lists the
// reports filed since the last one was sent, without any report text: no
// details, no contact, no URL, no artifact content (DESIGN.md "Abuse
// handling"). The queue link goes to the moderation queue.
type AlertEmail struct {
	QueueURL string
	Reports  []ReportAlert
	Interval time.Duration
}

// Render returns the digest bodies.
func (e AlertEmail) Render() (Message, error) {
	if !validLink(e.QueueURL) {
		return Message{}, fmt.Errorf("moderation queue link must be an absolute HTTP(S) URL")
	}
	n := len(e.Reports)
	if n == 0 {
		return Message{}, fmt.Errorf("an alert with no reports")
	}
	pluralS := "s"
	if n == 1 {
		pluralS = ""
	}
	subject := fmt.Sprintf("redrop: %d new abuse report%s", n, pluralS)
	var lines []string
	for _, r := range e.Reports {
		lines = append(lines, "  - "+describeReport(r))
	}
	text := strings.Join(nonEmpty(
		fmt.Sprintf("%d new report%s since the last alert:", n, pluralS),
		strings.Join(lines, "\n"),
		"",
		"Open the moderation queue:",
		e.QueueURL,
		"",
		"At most one alert per "+humanDuration(e.Interval)+".",
	), "\n")
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<p>%d new report%s since the last alert:</p>\n<ul>\n", n, pluralS))
	for _, r := range e.Reports {
		b.WriteString("<li>" + esc(describeReport(r)) + "</li>\n")
	}
	b.WriteString("</ul>\n")
	b.WriteString("<p><a href=\"" + esc(e.QueueURL) + "\">Open the moderation queue</a></p>\n")
	b.WriteString("<p class=\"muted\">At most one alert per " + esc(humanDuration(e.Interval)) + ".</p>\n")
	return Message{Subject: subject, Text: text, HTML: wrapHTML("Abuse report alert", b.String())}, nil
}

// TestEmail is the mail-test command's message.
func TestEmail(serverName string) Message {
	return Message{
		Subject: "redrop mail test",
		Text: strings.Join(nonEmpty(
			"This is a test message from redrop.",
			"If you can read this, outbound mail works on server "+serverName+".",
		), "\n"),
		HTML: wrapHTML("Mail test", "<p>This is a test message from redrop.</p>\n<p>If you can read this, outbound mail works on server "+esc(serverName)+".</p>\n"),
	}
}

// describeReport is the one line about a report: never its text.
func describeReport(r ReportAlert) string {
	src := "visitor report"
	if r.Source == "system" {
		src = "upload flag"
	}
	return fmt.Sprintf("%s, %s, filed %s UTC (report %s)", r.Category, src, r.Created.UTC().Format("2006-01-02 15:04"), r.ID)
}

// wrapHTML frames a body with the minimal document redrop sends.
func wrapHTML(title, body string) string {
	return "<!doctype html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>" + esc(title) + "</title>\n" +
		"<style>\nbody{font-family:system-ui,sans-serif;max-width:36em;margin:2em auto;padding:0 1em;color:#111}\n" +
		".muted{color:#666;font-size:0.9em}\n</style>\n</head>\n<body>\n" + body + "</body>\n</html>\n"
}

func validLink(link string) bool {
	if strings.ContainsAny(link, "\r\n\x00") {
		return false
	}
	u, err := url.Parse(link)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil
}

// humanTime renders an expiry for a reader.
func humanTime(t time.Time) string {
	if t.IsZero() {
		return "when it expires"
	}
	return t.UTC().Format("on 2 January 2006 at 15:04 UTC")
}

func humanDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	return d.String()
}

func nonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
