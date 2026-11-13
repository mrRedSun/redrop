package api

import (
	"testing"
	"time"
)

func TestParseTTL(t *testing.T) {
	ok := map[string]time.Duration{
		"30m": 30 * time.Minute, "12h": 12 * time.Hour, "7d": 7 * 24 * time.Hour,
		"2w": 14 * 24 * time.Hour, "1d12h": 36 * time.Hour, "1.5h": 90 * time.Minute, " 7D ": 7 * 24 * time.Hour,
	}
	for in, want := range ok {
		d, never, err := ParseTTL(in)
		if err != nil || never || d != want {
			t.Errorf("ParseTTL(%q) = %v %v %v, want %v", in, d, never, err, want)
		}
	}
	for _, in := range []string{"never", "Forever"} {
		if _, never, err := ParseTTL(in); err != nil || !never {
			t.Errorf("ParseTTL(%q) should mean never", in)
		}
	}
	for _, in := range []string{"", "banana", "7", "d", "-1h", "0m", "5x"} {
		if _, _, err := ParseTTL(in); err == nil {
			t.Errorf("ParseTTL(%q) should fail", in)
		}
	}
}

func TestFormatTTL(t *testing.T) {
	for d, want := range map[time.Duration]string{14 * 24 * time.Hour: "2w", 3 * 24 * time.Hour: "3d", 36 * time.Hour: "36h", 45 * time.Minute: "45m"} {
		if got := FormatTTL(d); got != want {
			t.Errorf("FormatTTL(%v) = %q, want %q", d, got, want)
		}
	}
}
