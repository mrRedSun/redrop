package sharing

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/store"
)

// newStoreService is a service over a real database and blob store, with one
// password-protected artifact.
func newStoreService(t *testing.T) (*Service, *store.Artifact) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "redrop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir, ViewWindowPassword: time.Hour, ViewWindowLimited: time.Hour}
	s, err := New(db, blobs, cfg, []byte("0123456789abcdef0123456789abcdef"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	o := &store.OneTime{Purpose: store.PurposeInvite, Role: "user", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.CreateOneTime(ctx, o, "invite-hash"); err != nil {
		t.Fatal(err)
	}
	u, err := db.RedeemAccountLink(ctx, o, "invite-hash", "owner", "owner", "", 1<<30, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateArtifact(ctx, &store.NewArtifact{UserID: u.ID, Title: "t", PasswordHash: "stored-hash"},
		&store.NewVersion{Kind: "html", Entry: "index.html", Files: []store.File{{Path: "index.html", Blob: "b", Size: 1, ContentType: "text/html"}}}, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.ArtifactByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return s, a
}

// Security review A5: 150 concurrent wrong passwords from 150 networks must
// not get more than the artifact's hourly budget evaluated. The password
// check blocks until every attempt has either reached it or been refused,
// so the old read-then-record order lets all 150 through.
func TestUnlockBudgetHoldsUnderConcurrency(t *testing.T) {
	s, a := newStoreService(t)
	const n = 150
	var evaluated, finished atomic.Int32
	release := make(chan struct{})
	s.checkPassword = func(hash, pw string) bool {
		evaluated.Add(1)
		<-release
		return false
	}
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, err := s.Open(context.Background(), a, Visitor{Network: fmt.Sprintf("2001:db8:%x::/64", i+1)}, "wrong")
			finished.Add(1)
			results <- err
		})
	}
	// Generously bounded: fails only if attempts hang outside the check.
	deadline := time.Now().Add(30 * time.Second)
	for evaluated.Load()+finished.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("stuck: %d evaluated, %d finished", evaluated.Load(), finished.Load())
		}
		time.Sleep(time.Millisecond)
	}
	got := evaluated.Load()
	close(release)
	wg.Wait()
	close(results)
	if got > lockThreshold {
		t.Fatalf("%d of %d concurrent wrong passwords were evaluated, budget %d", got, n, lockThreshold)
	}
	counts := map[apperr.Kind]int{}
	for err := range results {
		counts[apperr.KindOf(err)]++
	}
	if counts[apperr.KindUnauthorized] != lockThreshold || counts[apperr.KindRateLimited] != n-lockThreshold {
		t.Fatalf("outcomes %v, want %d wrong and %d locked", counts, lockThreshold, n-lockThreshold)
	}
	// The link is now locked for an hour, also for the right password.
	s.checkPassword = func(string, string) bool { return true }
	if _, err := s.Open(context.Background(), a, Visitor{Network: "198.51.100.1"}, "right"); !apperr.Is(err, apperr.KindRateLimited) {
		t.Fatalf("right password after the budget was spent: %v", err)
	}
}

// Right passwords give their charge back: busy links are never locked by
// their own visitors.
func TestUnlockBudgetCountsOnlyFailures(t *testing.T) {
	s, a := newStoreService(t)
	s.checkPassword = func(string, string) bool { return true }
	for i := range lockThreshold * 2 {
		if _, err := s.Open(context.Background(), a, Visitor{Network: fmt.Sprintf("198.51.%d.%d", i/250, i%250+1)}, "right"); err != nil {
			t.Fatalf("right password %d: %v", i+1, err)
		}
	}
}
