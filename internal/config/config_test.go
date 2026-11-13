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
