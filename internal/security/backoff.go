package security

import (
	"hash/maphash"
	"sync"
	"time"
)

// Backoff slows repeated attempts on one key, such as one account, from any
// number of clients. It never locks a key: after free attempts in a row
// without a Reset, each attempt must wait twice as long as the one before,
// from base up to max, so the right answer always gets through within max
// of the last attempt.
//
// Keys are hashed with a per-process random seed into a fixed table of
// slots. Memory is fixed, the table is never full, and an attacker cannot
// choose which keys share a slot. Two keys that share a slot share their
// backoff, which slows both a little and locks out neither.
type Backoff struct {
	mu     sync.Mutex
	seed   maphash.Seed
	slots  []backoffSlot
	free   int
	base   time.Duration
	max    time.Duration
	forget time.Duration // a slot with no attempt for this long starts over
	now    func() time.Time
}

type backoffSlot struct {
	attempts int64
	last     int64 // last attempt, Unix nanoseconds
	until    int64 // next attempt allowed at, Unix nanoseconds
}

// NewBackoff tracks keys in slots (a power of two) and lets free attempts
// through before the delays start. forget must be longer than max. now is
// the clock (time.Now, or a test's).
func NewBackoff(slots, free int, base, max, forget time.Duration, now func() time.Time) *Backoff {
	if slots <= 0 || slots&(slots-1) != 0 || forget <= max {
		panic("security: backoff needs a power-of-two slot count and forget > max")
	}
	return &Backoff{
		seed: maphash.MakeSeed(), slots: make([]backoffSlot, slots),
		free: free, base: base, max: max, forget: forget, now: now,
	}
}

func (b *Backoff) slot(key string) *backoffSlot {
	return &b.slots[maphash.String(b.seed, key)&uint64(len(b.slots)-1)]
}

// Begin charges one attempt to key before the caller checks it, and reports
// whether the attempt may go ahead. Charging first makes concurrent attempts
// queue behind each other instead of all reading the old count. A refused
// attempt is not charged; wait is how long until the next one is allowed.
// Call Reset when the attempt succeeds.
func (b *Backoff) Begin(key string) (ok bool, wait time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now().UnixNano()
	s := b.slot(key)
	if time.Duration(now-s.last) > b.forget {
		*s = backoffSlot{}
	}
	if now < s.until {
		return false, time.Duration(s.until - now)
	}
	s.attempts++
	s.last = now
	if over := s.attempts - int64(b.free); over >= 0 {
		d := b.base
		for ; over > 0 && d < b.max; over-- {
			d *= 2
		}
		s.until = now + int64(min(d, b.max))
	}
	return true, 0
}

// Reset clears key's attempts after a success.
func (b *Backoff) Reset(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	*b.slot(key) = backoffSlot{}
}
