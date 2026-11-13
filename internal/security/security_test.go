package security

import (
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
	if _, ok := a.Verify(v[:len(v)-1] + "A"); ok {
		t.Error("tampered signature accepted")
	}
	if _, ok := a.Verify(a.Sign("x", time.Now().Add(-time.Second))); ok {
		t.Error("expired value accepted")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	if !l.Allow("k") || !l.Allow("k") || l.Allow("k") {
		t.Error("limiter did not cap at 2")
	}
	if !l.Allow("other") {
		t.Error("keys are not independent")
	}
}
