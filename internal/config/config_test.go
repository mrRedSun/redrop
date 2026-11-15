package config

import (
	"strings"
	"testing"
)

func TestContentMissLimits(t *testing.T) {
	t.Setenv("REDROP_PUBLIC_URL", "https://redrop.example.com")
	t.Setenv("REDROP_CONTENT_URL", "https://usercontent.example")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ContentMissBurst != 30 || c.ContentMissPerMinute != 5 {
		t.Fatalf("defaults: burst=%d perMinute=%d", c.ContentMissBurst, c.ContentMissPerMinute)
	}

	t.Setenv("REDROP_CONTENT_MISS_BURST", "100")
	t.Setenv("REDROP_CONTENT_MISS_PER_MINUTE", "12")
	if c, err = Load(); err != nil || c.ContentMissBurst != 100 || c.ContentMissPerMinute != 12 {
		t.Fatalf("overrides: %+v %v", c, err)
	}

	for key, bad := range map[string]string{
		"REDROP_CONTENT_MISS_BURST":      "0",
		"REDROP_CONTENT_MISS_PER_MINUTE": "-1",
	} {
		t.Run(key, func(t *testing.T) {
			for _, v := range []string{bad, "lots", "1.5"} {
				t.Setenv(key, v)
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("%s=%q: err %v, want an error naming the variable", key, v, err)
				}
			}
		})
	}
}

// A trusted range wider than a /24 or /48 with X-Forwarded-For lets anyone
// inside it choose the client address (security review A4). One warning
// names every such range.
func TestWideTrustedProxyWarning(t *testing.T) {
	t.Setenv("REDROP_PUBLIC_URL", "https://redrop.example.com")
	t.Setenv("REDROP_CONTENT_URL", "https://usercontent.example")
	cases := []struct {
		proxies, header string
		wide            []string
	}{
		{"127.0.0.1/32,::1/128", "", nil},
		{"192.0.2.0/24,2001:db8:1::/48", "X-Forwarded-For", nil},
		{"192.0.2.10", "x-forwarded-for", nil},
		{"198.51.100.0/23", "X-Forwarded-For", []string{"198.51.100.0/23"}},
		{"10.0.0.0/8,2001:db8::/32,127.0.0.1", "", []string{"10.0.0.0/8", "2001:db8::/32"}},
		{"10.0.0.0/8,2001:db8::/32", "CF-Connecting-IP", nil},
	}
	for _, c := range cases {
		t.Setenv("REDROP_TRUSTED_PROXIES", c.proxies)
		t.Setenv("REDROP_CLIENT_IP_HEADER", c.header)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.Warnings()
		if len(c.wide) == 0 {
			if len(got) != 0 {
				t.Errorf("%s with %q: unexpected warnings %q", c.proxies, c.header, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(got[0], strings.Join(c.wide, ", ")+".") || !strings.Contains(got[0], "CF-Connecting-IP") {
			t.Errorf("%s with %q: warnings %q, want one naming %v and the remedy", c.proxies, c.header, got, c.wide)
		}
	}
}
