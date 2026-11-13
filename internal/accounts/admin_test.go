package accounts

import (
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
)

func TestInviteTTL(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", InviteTTL, true},
		{"  ", InviteTTL, true},
		{"1d", 24 * time.Hour, true},
		{"12h", 12 * time.Hour, true},
		{"never", 0, false}, // invites always expire
		{"banana", 0, false},
		{"-1d", 0, false},
	}
	for _, c := range cases {
		got, err := inviteTTL(c.in)
		if c.ok != (err == nil) || got != c.want {
			t.Errorf("inviteTTL(%q) = %v, %v", c.in, got, err)
		}
		if err != nil && !apperr.Is(err, apperr.KindInvalid) {
			t.Errorf("inviteTTL(%q): want Invalid, got %v", c.in, err)
		}
	}
}
