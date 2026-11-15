package security

import (
	"sync"
	"testing"
	"time"
)

func newTestBackoff(t *testing.T) (*Backoff, *fakeClock) {
	t.Helper()
	c := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	return NewBackoff(1<<10, 3, time.Second, time.Minute, time.Hour, c.now), c
}

func TestBackoffDoublesUpToMax(t *testing.T) {
	b, clock := newTestBackoff(t)
	for i := range 3 {
		if ok, _ := b.Begin("acct"); !ok {
			t.Fatalf("free attempt %d refused", i+1)
		}
	}
	// After the free attempts, each wait doubles: 1s, 2s, 4s, ... 60s.
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute} {
		ok, wait := b.Begin("acct")
		if ok || wait != want {
			t.Fatalf("attempt during backoff: ok=%v wait=%v, want refused for %v", ok, wait, want)
		}
		// Refused attempts are free: they do not extend the wait.
		if _, again := b.Begin("acct"); again != want {
			t.Fatalf("a refused attempt changed the wait to %v", again)
		}
		clock.add(want)
		if ok, _ := b.Begin("acct"); !ok {
			t.Fatalf("attempt refused after waiting %v", want)
		}
	}
	if ok, _ := b.Begin("other"); !ok {
		t.Fatal("another key is slowed by this one")
	}
}

// A success starts over, and so does a long quiet period: the right answer
// is never locked out for longer than max.
func TestBackoffResetAndForget(t *testing.T) {
	b, clock := newTestBackoff(t)
	for range 12 {
		if ok, wait := b.Begin("acct"); !ok {
			clock.add(wait)
			b.Begin("acct")
		}
	}
	if ok, wait := b.Begin("acct"); ok || wait != time.Minute {
		t.Fatalf("backed-off key: ok=%v wait=%v, want refused for the 1m maximum", ok, wait)
	}
	b.Reset("acct")
	for i := range 3 {
		if ok, _ := b.Begin("acct"); !ok {
			t.Fatalf("attempt %d after Reset refused", i+1)
		}
	}
	clock.add(time.Hour + time.Second)
	if ok, _ := b.Begin("acct"); !ok {
		t.Fatal("attempt refused after the forget period")
	}
}

// Charging before the check means concurrent attempts cannot all see the
// old count: exactly the free attempts go through at once.
func TestBackoffConcurrentAttempts(t *testing.T) {
	b, _ := newTestBackoff(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for range 200 {
		wg.Go(func() {
			if ok, _ := b.Begin("acct"); ok {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if passed != 3 {
		t.Fatalf("%d of 200 concurrent attempts went through, want 3", passed)
	}
}

func TestLimiterRefund(t *testing.T) {
	l, _ := newTestLimiter(t, 2, time.Minute, 10, EvictOldest)
	for range 5 {
		if !l.Allow("k") {
			t.Fatal("refunded attempts used up the limit")
		}
		l.Refund("k")
	}
	if !l.Allow("k") || !l.Allow("k") || l.Allow("k") {
		t.Fatal("limit not applied to unrefunded attempts")
	}
	l.Refund("missing") // no-op
}
