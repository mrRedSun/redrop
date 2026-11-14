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

func TestLimiter(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	t.Cleanup(l.Close)
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
	l := NewLimiter(1, time.Hour)
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
