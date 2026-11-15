package accounts

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// withTOTP gives u an enabled authenticator and returns its secret.
func withTOTP(t *testing.T, s *Service, u *store.User) string {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "redrop", AccountName: u.Username})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetTOTP(context.Background(), u.ID, key.Secret(), true); err != nil {
		t.Fatal(err)
	}
	return key.Secret()
}

// Security review A11: an authenticator code works once. A code that was
// seen or phished cannot be used again within its window, and neither can a
// code from an earlier step once a later one was accepted.
func TestTOTPCodeWorksOnce(t *testing.T) {
	s, clock := newTestService(t)
	u := addUser(t, s, "bob", "bob-password-123", api.RoleUser)
	secret := withTOTP(t, s, u)
	login := func(code string) error {
		_, err := s.Login(context.Background(), "198.51.100.7", "bob", "bob-password-123", code)
		return err
	}

	code, _ := totp.GenerateCode(secret, clock.now())
	if err := login(code); err != nil {
		t.Fatalf("first use: %v", err)
	}
	clock.add(10 * time.Second) // still the same step, or the next within the skew
	if err := login(code); !apperr.Is(err, apperr.KindUnauthorized) || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("replay: %v", err)
	}
	earlier, _ := totp.GenerateCode(secret, clock.now().Add(-totpPeriod*time.Second))
	if earlier != code {
		if err := login(earlier); !apperr.Is(err, apperr.KindUnauthorized) {
			t.Fatalf("code from an earlier step after a later one was used: %v", err)
		}
	}
	clock.add(totpPeriod * time.Second)
	next, _ := totp.GenerateCode(secret, clock.now())
	if err := login(next); err != nil {
		t.Fatalf("next step's code: %v", err)
	}

	// A code used to sign in cannot turn the authenticator off either.
	u, _ = s.db.UserByID(context.Background(), u.ID)
	p := &Principal{user: u, method: MethodPassword}
	if err := s.DisableTOTP(context.Background(), p, next); !apperr.Is(err, apperr.KindInvalid) || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("disable with a used code: %v", err)
	}
	clock.add(totpPeriod * time.Second)
	fresh, _ := totp.GenerateCode(secret, clock.now())
	if err := s.DisableTOTP(context.Background(), p, fresh); err != nil {
		t.Fatalf("disable with a fresh code: %v", err)
	}
}

// Two requests racing with the same code: exactly one wins.
func TestTOTPConcurrentReplay(t *testing.T) {
	s, clock := newTestService(t)
	u := addUser(t, s, "bob", "bob-password-123", api.RoleUser)
	secret := withTOTP(t, s, u)
	code, _ := totp.GenerateCode(secret, clock.now())
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 4 {
		wg.Go(func() {
			_, err := s.Login(context.Background(), "198.51.100."+string(rune('1'+i)), "bob", "bob-password-123", code)
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d of 4 concurrent sign-ins with one code succeeded, want 1", ok)
	}
}

// Enrolling consumes the confirmation code too, and a new secret starts its
// own record.
func TestTOTPEnrollmentCode(t *testing.T) {
	s, clock := newTestService(t)
	u := addUser(t, s, "bob", "bob-password-123", api.RoleUser)
	p := &Principal{user: u, method: MethodPassword}
	enroll, err := s.BeginTOTP(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	p.user, _ = s.db.UserByID(context.Background(), u.ID)
	code, _ := totp.GenerateCode(enroll.Secret, clock.now())
	if err := s.EnableTOTP(context.Background(), p, code); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := s.Login(context.Background(), "198.51.100.7", "bob", "bob-password-123", code); !apperr.Is(err, apperr.KindUnauthorized) {
		t.Fatalf("sign-in with the enrollment code: %v", err)
	}
}
