package security

import (
	"sync"
	"time"
)

// TokenBucket is a per-key token-bucket limiter: each key may spend up to
// burst tokens at once, refilled continuously at perMinute tokens per minute.
// It keeps at most maxKeys keys; a key whose bucket is full again is
// equivalent to an absent one and is dropped when space is needed.
type TokenBucket struct {
	mu        sync.Mutex
	burst     float64
	perSecond float64
	maxKeys   int
	now       func() time.Time
	lastSweep time.Time
	buckets   map[string]*tokens
}

type tokens struct {
	n  float64
	at time.Time
}

// sweepInterval bounds how often a full table is scanned, so a flood of new
// keys cannot turn every request into an O(maxKeys) scan.
const sweepInterval = time.Second

func NewTokenBucket(burst, perMinute, maxKeys int) *TokenBucket {
	return &TokenBucket{
		burst: float64(burst), perSecond: float64(perMinute) / 60, maxKeys: maxKeys,
		now: time.Now, buckets: map[string]*tokens{},
	}
}

func (l *TokenBucket) level(b *tokens, now time.Time) float64 {
	return min(l.burst, b.n+now.Sub(b.at).Seconds()*l.perSecond)
}

// Exhausted reports whether key has no whole token left, without spending one.
func (l *TokenBucket) Exhausted(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	return b != nil && l.level(b, l.now()) < 1
}

// Take spends one token for key and reports whether one was available. When
// the table is full of keys that are still refilling, new keys are refused.
func (l *TokenBucket) Take(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= l.maxKeys && !l.sweep(now) {
			return false
		}
		b = &tokens{n: l.burst, at: now}
		l.buckets[key] = b
	}
	n := l.level(b, now)
	b.at = now
	if n < 1 {
		b.n = n
		return false
	}
	b.n = n - 1
	return true
}

// sweep drops full buckets and reports whether there is room for a new key.
func (l *TokenBucket) sweep(now time.Time) bool {
	if now.Sub(l.lastSweep) >= sweepInterval {
		l.lastSweep = now
		for k, b := range l.buckets {
			if l.level(b, now) >= l.burst {
				delete(l.buckets, k)
			}
		}
	}
	return len(l.buckets) < l.maxKeys
}
