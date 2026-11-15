package accounts

import (
	"context"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// testClock is a goroutine-safe fake clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTestService returns a service over a fresh database with password
// login on and no identity providers. Its limiters run on the returned
// fake clock.
func newTestService(t *testing.T) (*Service, *testClock) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "redrop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, _ := url.Parse("https://redrop.example.com")
	cfg := &config.Config{PublicURL: u, PasswordLogin: true, DefaultQuotaBytes: 1 << 30}
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	s := newService(db, cfg, []byte("test-secret-test-secret-test-secret!"), slog.New(slog.DiscardHandler), clock.now)
	t.Cleanup(s.Close)
	return s, clock
}

// addUser creates a password account directly in the store.
func addUser(t *testing.T, s *Service, username, password, role string) *store.User {
	t.Helper()
	hash, err := security.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	o := &store.OneTime{Purpose: store.PurposeInvite, Role: role, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.db.CreateOneTime(ctx, o, "hash-"+username); err != nil {
		t.Fatal(err)
	}
	u, err := s.db.RedeemAccountLink(ctx, o, username, username, hash, 1<<30, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A name that can never be valid is refused before any limiter or database
// call, so oversized input never becomes a limiter key.
func TestLoginRejectsInvalidUsernamesBeforeLimiting(t *testing.T) {
	s, _ := newTestService(t)
	addUser(t, s, "bob", "bob-password-123", api.RoleUser)
	huge := "u" + strings.Repeat("a", 1_000_000)
	for _, name := range []string{huge, "a", "has space", strings.Repeat("b", 33), "bob@example.com", ""} {
		_, err := s.Login(context.Background(), "198.51.100.7", name, "whatever-password", "")
		if !apperr.Is(err, apperr.KindUnauthorized) || apperr.As(err).Msg != "wrong username or password" {
			t.Errorf("Login(%.20q): %v, want the wrong-login answer", name, err)
		}
	}
	if n, m := s.loginIPLimiter.Len(), s.loginLimiter.Len(); n != 0 || m != 0 {
		t.Fatalf("invalid names reached the limiters: %d per-IP keys, %d per-user keys", n, m)
	}
	// Normalization still applies to valid names.
	if _, err := s.Login(context.Background(), "198.51.100.7", "  BOB ", "bob-password-123", ""); err != nil {
		t.Fatalf("valid login after rejected names: %v", err)
	}
}
