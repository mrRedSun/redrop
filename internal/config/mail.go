package config

import (
	"fmt"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

func (c *Config) loadMail() error {
	var err error
	c.SMTPAddr = env("REDROP_SMTP_ADDR", "")
	if c.MailInsecurePlain, err = envBool("REDROP_SMTP_INSECURE_ALLOW_PLAIN", false); err != nil {
		return err
	}
	if c.SMTPAddr != "" {
		host, port, e := net.SplitHostPort(c.SMTPAddr)
		n, pe := strconv.Atoi(port)
		if e != nil || pe != nil || host == "" || strings.ContainsAny(host, "\r\n \t") || n < 1 || n > 65535 {
			return fmt.Errorf("REDROP_SMTP_ADDR must be host:port with a numeric port from 1 to 65535")
		}
		c.SMTPTLS = env("REDROP_SMTP_TLS", "starttls")
		switch c.SMTPTLS {
		case "starttls", "implicit":
		case "plain":
			if !c.MailInsecurePlain {
				return fmt.Errorf("REDROP_SMTP_TLS=plain requires REDROP_SMTP_INSECURE_ALLOW_PLAIN=true; use only for tests")
			}
		default:
			return fmt.Errorf("REDROP_SMTP_TLS must be starttls or implicit; plain needs explicit insecure opt-in")
		}
		c.SMTPUsername = env("REDROP_SMTP_USERNAME", "")
		if c.SMTPPassword, err = readSecret("", "REDROP_SMTP_PASSWORD_FILE"); err != nil {
			return err
		}
		if (c.SMTPUsername == "") != (c.SMTPPassword == "") {
			return fmt.Errorf("REDROP_SMTP_USERNAME and a non-empty REDROP_SMTP_PASSWORD_FILE must be set together")
		}
		if strings.ContainsAny(c.SMTPUsername+c.SMTPPassword, "\r\n\x00") {
			return fmt.Errorf("SMTP credentials must each be one line without NUL")
		}
		c.SMTPCAFile = env("REDROP_SMTP_CA_FILE", "")
		c.MailFrom = env("REDROP_MAIL_FROM", "")
		a, e := mail.ParseAddress(c.MailFrom)
		if e != nil || a.Address == "" || strings.ContainsAny(c.MailFrom, "\r\n\x00") {
			return fmt.Errorf("REDROP_MAIL_FROM must be an address with an optional display name")
		}
	}
	// No relay means no mail features. In particular notify addresses alone
	// do not enable outbound mail or change today's manual-invite behavior.
	raw := env("REDROP_NOTIFY_EMAIL", "")
	if c.SMTPAddr != "" && raw != "" {
		seen := map[string]bool{}
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			a, e := mail.ParseAddress(v)
			at := strings.LastIndexByte(v, '@')
			if e != nil || a.Name != "" || a.Address != v || len(v) > 254 || strings.ContainsAny(v, "\r\n\x00") || at < 1 || strings.ContainsAny(v[at+1:], "[]") {
				return fmt.Errorf("REDROP_NOTIFY_EMAIL must contain comma-separated bare addresses with domain names")
			}
			// The domain is case-insensitive; store it in lower case, as
			// the mail layer does, so suppression and alert state match.
			v = v[:at+1] + strings.ToLower(v[at+1:])
			key := strings.ToLower(v)
			if !seen[key] {
				c.NotifyEmail = append(c.NotifyEmail, v)
				seen[key] = true
			}
		}
		if len(c.NotifyEmail) > 16 {
			return fmt.Errorf("REDROP_NOTIFY_EMAIL allows at most 16 recipients")
		}
	}
	c.AlertInterval, err = envDurationIn("REDROP_NOTIFY_EMAIL_INTERVAL", "10m", time.Second, 24*time.Hour)
	return err
}

func (c *Config) MailEnabled() bool { return c.SMTPAddr != "" }
