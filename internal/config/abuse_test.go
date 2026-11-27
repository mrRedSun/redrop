package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func abuseBase(t *testing.T) {
	t.Setenv("REDROP_PUBLIC_URL", "https://redrop.example.com")
	t.Setenv("REDROP_CONTENT_URL", "https://usercontent.example")
}

func TestAbuseDefaults(t *testing.T) {
	abuseBase(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AbuseContact != "" || c.NotifyURL != "" || c.NotifyToken != "" || c.ReportsPerHour != 5 || c.ReportsMaxOpen != 1000 || c.ReportsMaxOpenPerArtifact != 20 {
		t.Fatalf("defaults: %+v", c)
	}
	for _, key := range []string{"REDROP_REPORTS_PER_HOUR", "REDROP_REPORTS_MAX_OPEN", "REDROP_REPORTS_MAX_OPEN_PER_ARTIFACT"} {
		for _, v := range []string{"0", "-2", "lots"} {
			t.Setenv(key, v)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q: %v", key, v, err)
			}
		}
		t.Setenv(key, "7")
	}
	c, _ = Load()
	if c.ReportsPerHour != 7 || c.ReportsMaxOpen != 7 || c.ReportsMaxOpenPerArtifact != 7 {
		t.Fatalf("overrides: %+v", c)
	}
}

func TestAbuseContact(t *testing.T) {
	abuseBase(t)
	good := map[string]string{
		"abuse@example.com":                "mailto:abuse@example.com",
		"mailto:abuse@example.com":         "mailto:abuse@example.com",
		"https://example.com/abuse":        "https://example.com/abuse",
		"https://example.com/report?x=1":   "https://example.com/report?x=1",
		"  security+redrop@example.org   ": "mailto:security+redrop@example.org",
	}
	for raw, want := range good {
		t.Setenv("REDROP_ABUSE_CONTACT", raw)
		c, err := Load()
		if err != nil || c.AbuseContact != want {
			t.Errorf("%q: %q %v, want %q", raw, c.AbuseContact, err, want)
		}
	}
	for _, raw := range []string{"http://example.com/abuse", "ftp://example.com", "not an address", "abuse@", "Abuse <abuse@example.com>",
		"abuse@example.com\nContact: https://evil.example", "https://user:pw@example.com/", "mailto:", "https://"} {
		t.Setenv("REDROP_ABUSE_CONTACT", raw)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REDROP_ABUSE_CONTACT") {
			t.Errorf("%q accepted: %v", raw, err)
		}
	}
}

func TestNotifySettings(t *testing.T) {
	abuseBase(t)
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("s3cret\n"), 0o600)

	t.Setenv("REDROP_NOTIFY_TOKEN_FILE", token)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "needs REDROP_NOTIFY_URL") {
		t.Fatalf("token without a URL: %v", err)
	}
	for _, bad := range []string{"ftp://hooks.example/", "hooks.example/x", "https://user:pw@hooks.example/", "https://"} {
		t.Setenv("REDROP_NOTIFY_URL", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REDROP_NOTIFY_URL") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	t.Setenv("REDROP_NOTIFY_URL", "https://hooks.example/redrop")
	c, err := Load()
	if err != nil || c.NotifyURL != "https://hooks.example/redrop" || c.NotifyToken != "s3cret" {
		t.Fatalf("valid: %q %q %v", c.NotifyURL, c.NotifyToken, err)
	}
	if len(c.Warnings()) != 0 {
		t.Fatalf("https warns: %v", c.Warnings())
	}
	// A bearer token over plain http to another host draws a warning;
	// to this host it does not.
	t.Setenv("REDROP_NOTIFY_URL", "http://hooks.example/redrop")
	if c, _ = Load(); len(c.Warnings()) != 1 || !strings.Contains(c.Warnings()[0], "REDROP_NOTIFY_URL") {
		t.Fatalf("plain http warnings: %v", c.Warnings())
	}
	t.Setenv("REDROP_NOTIFY_URL", "http://127.0.0.1:9000/hook")
	if c, _ = Load(); len(c.Warnings()) != 0 {
		t.Fatalf("loopback warnings: %v", c.Warnings())
	}
	// The token file must exist and hold one line.
	os.WriteFile(token, []byte("two\nlines"), 0o600)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "one non-empty line") {
		t.Fatalf("two-line token: %v", err)
	}
	t.Setenv("REDROP_NOTIFY_TOKEN_FILE", filepath.Join(dir, "missing"))
	if _, err := Load(); err == nil {
		t.Fatal("missing token file accepted")
	}
}
