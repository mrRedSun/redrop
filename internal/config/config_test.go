package config

import (
	"strings"
	"testing"
)

func TestOpenUploadLimit(t *testing.T) {
	t.Setenv("REDROP_PUBLIC_URL", "https://redrop.example.com")
	t.Setenv("REDROP_CONTENT_URL", "https://usercontent.example")
	c, err := Load()
	if err != nil || c.MaxOpenUploads != 8 {
		t.Fatalf("default: %+v, %v", c, err)
	}
	t.Setenv("REDROP_MAX_OPEN_UPLOADS", "3")
	c, err = Load()
	if err != nil || c.MaxOpenUploads != 3 {
		t.Fatalf("override: %+v, %v", c, err)
	}
	for _, value := range []string{"0", "-1", "many", "1.5"} {
		t.Setenv("REDROP_MAX_OPEN_UPLOADS", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REDROP_MAX_OPEN_UPLOADS") {
			t.Fatalf("%q: %v", value, err)
		}
	}
}

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
		proxies, header, hops string
		wide                  []string
	}{
		{"127.0.0.1/32,::1/128", "", "", nil},
		{"192.0.2.0/24,2001:db8:1::/48", "X-Forwarded-For", "", nil},
		{"192.0.2.10", "x-forwarded-for", "", nil},
		{"198.51.100.0/23", "X-Forwarded-For", "", []string{"198.51.100.0/23"}},
		{"10.0.0.0/8,2001:db8::/32,127.0.0.1", "", "0", []string{"10.0.0.0/8", "2001:db8::/32"}},
		{"10.0.0.0/8,2001:db8::/32", "CF-Connecting-IP", "", nil},
		// A hop count reads a fixed entry, so wide ranges no longer matter.
		{"10.0.0.0/8,2001:db8::/32,127.0.0.1", "", "2", nil},
	}
	for _, c := range cases {
		t.Setenv("REDROP_TRUSTED_PROXIES", c.proxies)
		t.Setenv("REDROP_CLIENT_IP_HEADER", c.header)
		t.Setenv("REDROP_TRUSTED_HOPS", c.hops)
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
		if len(got) != 1 || !strings.Contains(got[0], strings.Join(c.wide, ", ")+".") || !strings.Contains(got[0], "REDROP_TRUSTED_HOPS") {
			t.Errorf("%s with %q: warnings %q, want one naming %v and the remedy", c.proxies, c.header, got, c.wide)
		}
	}
}

func TestAPIDocsSetting(t *testing.T) {
	t.Setenv("REDROP_PUBLIC_URL", "https://redrop.example.com")
	t.Setenv("REDROP_CONTENT_URL", "https://usercontent.example")
	if c, err := Load(); err != nil || c.APIDocs {
		t.Fatalf("default: docs=%v err=%v, want off", c != nil && c.APIDocs, err)
	}
	t.Setenv("REDROP_API_DOCS", "true")
	if c, err := Load(); err != nil || !c.APIDocs {
		t.Fatalf("REDROP_API_DOCS=true: %v", err)
	}
	t.Setenv("REDROP_API_DOCS", "sometimes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REDROP_API_DOCS") {
		t.Fatalf("invalid value: %v", err)
	}
}

func TestTrustedHopsSetting(t *testing.T) {
	t.Setenv("REDROP_PUBLIC_URL", "https://redrop.example.com")
	t.Setenv("REDROP_CONTENT_URL", "https://usercontent.example")
	if c, err := Load(); err != nil || c.TrustedHops != 0 {
		t.Fatalf("default: %v %v", c, err)
	}
	t.Setenv("REDROP_TRUSTED_HOPS", "2")
	if c, err := Load(); err != nil || c.TrustedHops != 2 {
		t.Fatalf("REDROP_TRUSTED_HOPS=2: %v", err)
	}
	for _, bad := range []string{"-1", "two", "1.5", "11"} {
		t.Setenv("REDROP_TRUSTED_HOPS", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REDROP_TRUSTED_HOPS") {
			t.Errorf("REDROP_TRUSTED_HOPS=%q: err %v, want an error naming the variable", bad, err)
		}
	}
	// Hops count X-Forwarded-For entries; a single-value header has none.
	t.Setenv("REDROP_TRUSTED_HOPS", "1")
	t.Setenv("REDROP_CLIENT_IP_HEADER", "CF-Connecting-IP")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "X-Forwarded-For") {
		t.Fatalf("hops with CF-Connecting-IP: %v", err)
	}
}
