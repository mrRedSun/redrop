package security

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newTestBucket(burst, perMinute, maxKeys int) (*TokenBucket, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	l := NewTokenBucket(burst, perMinute, maxKeys)
	l.now = c.now
	return l, c
}

func TestTokenBucketBurstAndRefill(t *testing.T) {
	l, clock := newTestBucket(30, 6, 100)
	if l.Exhausted("a") {
		t.Fatal("unknown key reported exhausted")
	}
	for i := range 30 {
		if !l.Take("a") {
			t.Fatalf("take %d refused within burst", i+1)
		}
	}
	if !l.Exhausted("a") || l.Take("a") {
		t.Fatal("31st take allowed")
	}
	if l.Exhausted("b") || !l.Take("b") {
		t.Fatal("keys are not independent")
	}
	clock.add(9 * time.Second)
	if !l.Exhausted("a") {
		t.Fatal("refilled faster than 6/min")
	}
	clock.add(time.Second)
	if l.Exhausted("a") || !l.Take("a") || l.Take("a") {
		t.Fatal("expected exactly one token after 10s at 6/min")
	}
	// Refused takes do not push the refill further out.
	for range 5 {
		l.Take("a")
	}
	clock.add(10 * time.Second)
	if !l.Take("a") {
		t.Fatal("refused takes delayed the refill")
	}
	// Refill is capped at burst.
	clock.add(24 * time.Hour)
	for i := range 30 {
		if !l.Take("a") {
			t.Fatalf("take %d after full refill refused", i+1)
		}
	}
	if l.Take("a") {
		t.Fatal("refill exceeded burst")
	}
}

func TestTokenBucketBoundedKeys(t *testing.T) {
	l, clock := newTestBucket(2, 60, 3)
	for _, k := range []string{"a", "b", "c"} {
		l.Take(k)
	}
	if l.Take("d") {
		t.Fatal("table over capacity accepted a new key")
	}
	if l.Exhausted("d") {
		t.Fatal("refused key must not be tracked as exhausted")
	}
	clock.add(2 * time.Second) // a, b, c are full again and may be dropped
	if !l.Take("d") {
		t.Fatal("full buckets were not reclaimed")
	}
	if len(l.buckets) != 1 {
		t.Fatalf("expected only d after sweep, have %d keys", len(l.buckets))
	}
}

func TestTokenBucketConcurrent(t *testing.T) {
	l := NewTokenBucket(50, 1, 10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Go(func() {
			if l.Take("k") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed < 50 || allowed > 51 {
		t.Fatalf("%d concurrent takes allowed, want 50 (51 if a token refilled)", allowed)
	}
}
