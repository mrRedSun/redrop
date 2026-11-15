package accounts

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

// guess signs in from its own network and classifies the answer.
func guess(s *Service, i int, user, password, code string) (string, error) {
	_, err := s.Login(context.Background(), fmt.Sprintf("198.51.%d.%d", i/250, i%250+1), user, password, code)
	switch {
	case err == nil:
		return "ok", nil
	case apperr.Is(err, apperr.KindRateLimited):
		return "429", err
	case apperr.Is(err, apperr.KindUnauthorized):
		return "401", err
	}
	return "other", err
}

// Security review A3: guesses from ever-new networks are slowed per
// account. Each one is charged before the password check, so only the free
// attempts are evaluated, and the real password works once the attacker's
// last wait has passed.
func TestAccountBackoffIgnoresNetworks(t *testing.T) {
	s, clock := newTestService(t)
	addUser(t, s, "admin", "admin-password-123", api.RoleAdmin)
	counts := map[string]int{}
	var last error
	for i := range 40 {
		kind, err := guess(s, i, "admin", fmt.Sprintf("guess-%06d", i), "")
		counts[kind]++
		if kind == "429" {
			last = err
		}
	}
	if counts["401"] != accountFreeAttempts || counts["429"] != 40-accountFreeAttempts {
		t.Fatalf("40 guesses from 40 networks: %v, want %d evaluated", counts, accountFreeAttempts)
	}
	if msg := apperr.As(last).Msg; !strings.Contains(msg, "for this account") {
		t.Fatalf("refusal message %q", msg)
	}

	// The real password waits like everyone else, for a bounded time.
	kind, err := guess(s, 100, "admin", "admin-password-123", "")
	wait := apperr.As(err).RetryAfter
	if kind != "429" || wait <= 0 || wait > accountMaxDelay {
		t.Fatalf("real password during the backoff: %s %v (wait %v)", kind, err, wait)
	}
	clock.add(wait)
	if kind, err := guess(s, 101, "admin", "admin-password-123", ""); kind != "ok" {
		t.Fatalf("real password after the wait: %s %v", kind, err)
	}
	// A success starts over.
	if kind, _ := guess(s, 102, "admin", "wrong", ""); kind != "401" {
		t.Fatalf("first wrong guess after a success: %s", kind)
	}
}

// Unknown names back off exactly like real ones, so the backoff does not
// reveal which accounts exist.
func TestAccountBackoffSameForUnknownNames(t *testing.T) {
	s, _ := newTestService(t)
	addUser(t, s, "bob", "bob-password-123", api.RoleUser)
	for _, name := range []string{"bob", "ghost"} {
		var got []string
		for i := range accountFreeAttempts + 1 {
			kind, _ := guess(s, i, name, "wrong-password", "")
			got = append(got, kind)
		}
		if strings.Join(got, ",") != "401,401,401,401,401,429" {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// With the password known, wrong authenticator codes are capped per account
// per hour, whatever network they come from.
func TestTOTPFailureCeiling(t *testing.T) {
	s, clock := newTestService(t)
	u := addUser(t, s, "bob", "bob-password-123", api.RoleUser)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "redrop", AccountName: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTOTP(context.Background(), u.ID, key.Secret(), true); err != nil {
		t.Fatal(err)
	}
	// try waits out the account backoff, from a new network each time, so
	// only the TOTP ceiling decides.
	network := 0
	try := func(code string) (string, error) {
		for {
			network++
			kind, err := guess(s, network, "bob", "bob-password-123", code)
			if wait := apperr.As(err).RetryAfter; kind == "429" && wait > 0 {
				clock.add(wait)
				continue
			}
			return kind, err
		}
	}
	for i := range totpFailuresPerHour {
		if kind, err := try("000000"); kind != "401" || !strings.Contains(err.Error(), "authenticator code") {
			t.Fatalf("wrong code %d: %s %v", i+1, kind, err)
		}
	}
	valid, _ := totp.GenerateCode(key.Secret(), clock.now())
	if kind, err := try(valid); kind != "429" || !strings.Contains(err.Error(), "authenticator codes") {
		t.Fatalf("code after %d wrong ones: %s %v, want the ceiling", totpFailuresPerHour, kind, err)
	}
	clock.add(time.Hour + time.Second)
	valid, _ = totp.GenerateCode(key.Secret(), clock.now())
	if kind, err := try(valid); kind != "ok" {
		t.Fatalf("valid code an hour later: %s %v", kind, err)
	}

	// Turning the authenticator off counts against the same ceiling.
	u, _ = s.db.UserByID(context.Background(), u.ID)
	p := &Principal{user: u, method: MethodPassword}
	for i := range totpFailuresPerHour {
		if err := s.DisableTOTP(context.Background(), p, "000000"); !apperr.Is(err, apperr.KindInvalid) {
			t.Fatalf("wrong code %d to disable: %v", i+1, err)
		}
	}
	valid, _ = totp.GenerateCode(key.Secret(), clock.now())
	if err := s.DisableTOTP(context.Background(), p, valid); !apperr.Is(err, apperr.KindRateLimited) {
		t.Fatalf("disable after %d wrong codes: %v, want the ceiling", totpFailuresPerHour, err)
	}
}
