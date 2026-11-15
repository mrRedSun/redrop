package security

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected hash format %q", h)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("correct password rejected")
	}
	if CheckPassword(h, "wrong") || CheckPassword("", "anything") || CheckPassword("garbage", "x") {
		t.Error("wrong password accepted")
	}
}

func TestSigner(t *testing.T) {
	a := NewSigner([]byte("secret-one-secret-one-secret-one!"), "access")
	b := NewSigner([]byte("secret-one-secret-one-secret-one!"), "preview")
	v := a.Sign("id|1|visit", time.Now().Add(time.Minute))
	if p, ok := a.Verify(v); !ok || p != "id|1|visit" {
		t.Fatalf("verify failed: %q %v", p, ok)
	}
	if _, ok := b.Verify(v); ok {
		t.Error("value from one purpose accepted by another")
	}
	// Tamper with the decoded MAC, not its last base64 character: that
	// character carries only 4 significant bits, and replacing it with a
	// fixed symbol is a no-op when it already is that symbol.
	enc, sig, _ := strings.Cut(v, ".")
	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	for i := range mac {
		for bit := range 8 {
			bad := append([]byte(nil), mac...)
			bad[i] ^= 1 << bit
			if _, ok := a.Verify(enc + "." + base64.RawURLEncoding.EncodeToString(bad)); ok {
				t.Errorf("signature with byte %d bit %d flipped accepted", i, bit)
			}
		}
	}
	if _, ok := a.Verify(a.Sign("x", time.Now().Add(-time.Second))); ok {
		t.Error("expired value accepted")
	}
}

func newTestLimiter(t *testing.T, limit int, window time.Duration, maxKeys int) (*Limiter, *fakeClock) {
	t.Helper()
	c := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	l := NewLimiter(limit, window, maxKeys)
	l.now = c.now
	t.Cleanup(l.Close)
	return l, c
}

func TestLimiter(t *testing.T) {
	l, _ := newTestLimiter(t, 2, time.Minute, 10)
	if !l.Allow("k") || !l.Allow("k") || l.Allow("k") {
		t.Error("limiter did not cap at 2")
	}
	if !l.Allow("other") {
		t.Error("keys are not independent")
	}
}

// Close returns only after the sweeper goroutine has exited, so done is
// already closed when it returns. A long window keeps the ticker from
// firing during the test.
func TestLimiterCloseStopsSweeper(t *testing.T) {
	l := NewLimiter(1, time.Hour, 10)
	select {
	case <-l.done:
		t.Fatal("sweeper exited before Close")
	default:
	}
	l.Close()
	select {
	case <-l.done:
	default:
		t.Fatal("Close returned while the sweeper was still running")
	}
	l.Close() // idempotent
	if !l.Allow("k") || l.Allow("k") || !l.Blocked("k") {
		t.Error("a closed limiter must still limit")
	}
}

// A full table refuses new keys and keeps limiting known ones; a slot frees
// up once its window has expired.
func TestLimiterKeyCap(t *testing.T) {
	l, clock := newTestLimiter(t, 3, time.Minute, 2)
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("keys within the cap refused")
	}
	if l.Allow("c") {
		t.Fatal("a third key was admitted to a table of two")
	}
	if l.Len() != 2 {
		t.Fatalf("table holds %d keys, want 2", l.Len())
	}
	if !l.Allow("a") || !l.Allow("a") || l.Allow("a") {
		t.Fatal("a known key is not limited normally while the table is full")
	}
	clock.add(30 * time.Second)
	if l.Allow("c") {
		t.Fatal("new key admitted while every window is live")
	}
	clock.add(31 * time.Second)
	if !l.Allow("c") {
		t.Fatal("new key refused after the other windows expired")
	}
	if l.Len() != 1 {
		t.Fatalf("expired windows kept: %d keys", l.Len())
	}
}

// Keys longer than maxKeyLen are stored as a fixed-size hash, so a caller
// passing attacker-sized input cannot grow memory per key.
func TestLimiterHashesLongKeys(t *testing.T) {
	l, _ := newTestLimiter(t, 1, time.Minute, 10)
	long := strings.Repeat("x", 1_000_000)
	if !l.Allow(long) || l.Allow(long) {
		t.Fatal("a long key is not limited like any other")
	}
	if !l.Allow(long[:len(long)-1] + "y") {
		t.Fatal("distinct long keys share a window")
	}
	for k := range l.hits {
		if len(k) > maxKeyLen {
			t.Fatalf("stored a %d-byte key", len(k))
		}
	}
}
