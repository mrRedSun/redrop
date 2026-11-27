package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mailEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"REDROP_SMTP_ADDR", "REDROP_SMTP_TLS", "REDROP_SMTP_USERNAME", "REDROP_SMTP_PASSWORD_FILE", "REDROP_SMTP_CA_FILE", "REDROP_MAIL_FROM", "REDROP_NOTIFY_EMAIL", "REDROP_NOTIFY_EMAIL_INTERVAL", "REDROP_SMTP_INSECURE_ALLOW_PLAIN"} {
		t.Setenv(k, "")
	}
}
func TestMailConfiguration(t *testing.T) {
	mailEnv(t)
	t.Run("off", func(t *testing.T) {
		c := &Config{}
		if err := c.loadMail(); err != nil || c.MailEnabled() {
			t.Fatalf("off: %+v %v", c, err)
		}
	})
	t.Run("off ignores recipients", func(t *testing.T) {
		t.Setenv("REDROP_NOTIFY_EMAIL", "ops@example.com")
		c := &Config{}
		if err := c.loadMail(); err != nil || len(c.NotifyEmail) > 0 {
			t.Fatalf("off: %v", err)
		}
	})
	t.Setenv("REDROP_SMTP_ADDR", "smtp.example.com:587")
	t.Setenv("REDROP_MAIL_FROM", "Redrop <share@example.com>")
	pass := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pass, []byte("password-for-tests\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("private relay", func(t *testing.T) {
		t.Setenv("REDROP_SMTP_USERNAME", "relay-user")
		t.Setenv("REDROP_SMTP_PASSWORD_FILE", pass)
		t.Setenv("REDROP_NOTIFY_EMAIL", "ops@example.com, ops@example.com, second@example.com")
		c := &Config{}
		if err := c.loadMail(); err != nil {
			t.Fatal(err)
		}
		if !c.MailEnabled() || c.SMTPTLS != "starttls" || c.SMTPPassword != "password-for-tests" || len(c.NotifyEmail) != 2 || c.AlertInterval != 10*time.Minute {
			t.Fatalf("settings: %+v", c)
		}
	})
	for _, test := range []struct{ key, value string }{
		{"REDROP_SMTP_ADDR", "bad"}, {"REDROP_SMTP_ADDR", ":587"}, {"REDROP_SMTP_ADDR", "smtp.example.com:0"}, {"REDROP_SMTP_ADDR", "smtp.example.com:65536"},
		{"REDROP_SMTP_TLS", "none"}, {"REDROP_SMTP_TLS", "plain"}, {"REDROP_SMTP_USERNAME", "missing-password"}, {"REDROP_SMTP_PASSWORD_FILE", pass},
		{"REDROP_MAIL_FROM", "bad"}, {"REDROP_MAIL_FROM", "A\r\nB <x@example.com>"}, {"REDROP_NOTIFY_EMAIL", "A <a@example.com>"}, {"REDROP_NOTIFY_EMAIL", "a@example.com,,b@example.com"},
		{"REDROP_NOTIFY_EMAIL_INTERVAL", "0s"}, {"REDROP_NOTIFY_EMAIL_INTERVAL", "25h"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if err := (&Config{}).loadMail(); err == nil {
				t.Fatal("expected invalid configuration")
			}
		})
	}
	t.Run("explicit insecure plain", func(t *testing.T) {
		t.Setenv("REDROP_SMTP_TLS", "plain")
		t.Setenv("REDROP_SMTP_INSECURE_ALLOW_PLAIN", "true")
		if err := (&Config{}).loadMail(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("recipient cap", func(t *testing.T) {
		var addr []string
		for range 17 {
			addr = append(addr, strings.Repeat("a", len(addr)+1)+"@example.com")
		}
		t.Setenv("REDROP_NOTIFY_EMAIL", strings.Join(addr, ","))
		if err := (&Config{}).loadMail(); err == nil {
			t.Fatal("expected recipient bound")
		}
	})
}
