// Package security contains password hashing, signed values, and rate limiting.
package security

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// --- argon2id password hashing (PHC string format) ---

const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// argonSem bounds concurrent hashing so password floods cannot exhaust memory.
var argonSem = make(chan struct{}, 4)

func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	argonSem <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-argonSem
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// dummyHash is used to equalize timing for unknown users.
var dummyHash, _ = HashPassword("redrop-timing-equalizer")

func CheckPassword(hash, pw string) bool {
	if hash == "" {
		checkPassword(dummyHash, pw)
		return false
	}
	return checkPassword(hash, pw)
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	argonSem <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-argonSem
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidatePassword enforces a minimal account password policy.
func ValidatePassword(pw string) error {
	if len(pw) < 10 {
		return errors.New("password must be at least 10 characters")
	}
	if len(pw) > 256 {
		return errors.New("password is too long")
	}
	return nil
}

// --- HMAC-signed values ---

type Signer struct{ key []byte }

func NewSigner(secret []byte, purpose string) *Signer {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("redrop/" + purpose))
	return &Signer{key: m.Sum(nil)}
}

// Sign returns payload.signature (base64url) with an expiry.
func (s *Signer) Sign(payload string, exp time.Time) string {
	body := fmt.Sprintf("%d|%s", exp.Unix(), payload)
	enc := base64.RawURLEncoding.EncodeToString([]byte(body))
	return enc + "." + s.mac(enc)
}

func (s *Signer) mac(v string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify returns the payload if the value is authentic and unexpired.
func (s *Signer) Verify(v string) (string, bool) {
	enc, sig, ok := strings.Cut(v, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac(enc))) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	expS, payload, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", false
	}
	var exp int64
	if _, err := fmt.Sscanf(expS, "%d", &exp); err != nil || time.Now().Unix() >= exp {
		return "", false
	}
	return payload, true
}

// --- rate limiting (fixed window per key) ---

// maxKeyLen bounds the bytes a Limiter stores per key. Longer keys are
// replaced by their SHA-256, so a caller that passes attacker-sized input
// still costs a fixed amount of memory per key.
const maxKeyLen = 64

// WhenFull is what a Limiter does with a new key while its table is full.
type WhenFull int

const (
	// EvictOldest drops the entry whose window started first, so a new
	// client is never refused. For limits that something else also bounds
	// (per-network limits, behind per-account and per-artifact ones): an
	// attacker who rotates networks gains nothing from the eviction.
	EvictOldest WhenFull = iota
	// RefuseNew refuses the new key until a window expires (fail closed).
	// For limits that are the only bound on what they count.
	RefuseNew
)

// Limiter is a fixed-window counter per key. It tracks at most maxKeys keys,
// so a flood of distinct keys cannot grow memory; a new key that does not
// fit is handled by its WhenFull policy. Known keys keep their own count.
// Entries are kept in window-start order, so expiring and evicting take
// constant time. A background goroutine drops expired windows once per
// window; Close stops it.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	full    WhenFull
	now     func() time.Time
	hits    map[string]*list.Element // of *bucket
	order   *list.List               // buckets by window start, oldest first

	stop      chan struct{} // closed by Close
	done      chan struct{} // closed when the sweeper has returned
	closeOnce sync.Once
}

type bucket struct {
	key   string
	start time.Time
	n     int
}

// NewLimiter allows limit hits per key per window, for at most maxKeys keys
// at a time, handling a full table as full says. now is the clock
// (time.Now, or a test's).
func NewLimiter(limit int, window time.Duration, maxKeys int, full WhenFull, now func() time.Time) *Limiter {
	l := &Limiter{
		limit: limit, window: window, maxKeys: maxKeys, full: full, now: now,
		hits: map[string]*list.Element{}, order: list.New(),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go l.sweeper()
	return l
}

func (l *Limiter) sweeper() {
	defer close(l.done)
	t := time.NewTicker(l.window)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.mu.Lock()
			l.dropExpired(l.now())
			l.mu.Unlock()
		}
	}
}

func (l *Limiter) expired(b *bucket, now time.Time) bool { return now.Sub(b.start) > l.window }

// dropExpired removes expired windows from the oldest end. Each entry is
// removed once, so this is constant time per request on average.
func (l *Limiter) dropExpired(now time.Time) {
	for e := l.order.Front(); e != nil && l.expired(e.Value.(*bucket), now); e = l.order.Front() {
		l.remove(e)
	}
}

func (l *Limiter) remove(e *list.Element) {
	delete(l.hits, e.Value.(*bucket).key)
	l.order.Remove(e)
}

// Close stops the sweeper and returns once it has exited. It is safe to
// call more than once. Allow and Blocked keep working afterwards, without
// expired windows being dropped, so a request that outlives shutdown is
// still limited.
func (l *Limiter) Close() {
	l.closeOnce.Do(func() { close(l.stop) })
	<-l.done
}

func limiterKey(key string) string {
	if len(key) <= maxKeyLen {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return string(sum[:])
}

// Allow records a hit and reports whether the key is within its limit.
func (l *Limiter) Allow(key string) bool { return l.AllowN(key, 1) }

// AllowN records n hits at once, such as bytes against a byte budget, and
// reports whether the key is still within its limit.
func (l *Limiter) AllowN(key string, n int) bool {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.dropExpired(now)
	e, ok := l.hits[key]
	if !ok {
		if len(l.hits) >= l.maxKeys {
			if l.full == RefuseNew {
				return false
			}
			l.remove(l.order.Front())
		}
		e = l.order.PushBack(&bucket{key: key, start: now})
		l.hits[key] = e
	}
	b := e.Value.(*bucket)
	b.n += n
	return b.n <= l.limit
}

// Count is how many hits key has in its current window (0 when it has none).
func (l *Limiter) Count(key string) int {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.hits[key]; ok && !l.expired(e.Value.(*bucket), l.now()) {
		return e.Value.(*bucket).n
	}
	return 0
}

// Wait is how long until key may be allowed again: the end of its window,
// or, for a key refused by a full RefuseNew table, the end of the oldest
// window. It is 0 when key would be allowed now. Callers send it as
// Retry-After.
func (l *Limiter) Wait(key string) time.Duration {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.dropExpired(now)
	var b *bucket
	if e, ok := l.hits[key]; ok {
		if b = e.Value.(*bucket); b.n < l.limit {
			return 0
		}
	} else if len(l.hits) < l.maxKeys || l.full == EvictOldest {
		return 0
	} else {
		b = l.order.Front().Value.(*bucket)
	}
	// A window ends just after start+window (see expired).
	return b.start.Add(l.window).Sub(now) + time.Nanosecond
}

// Refund takes back one hit recorded for key in its current window. Callers
// that charge an attempt before checking it refund the ones that succeed,
// so only failures use up the limit.
func (l *Limiter) Refund(key string) {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.hits[key]; ok {
		if b := e.Value.(*bucket); b.n > 0 {
			b.n--
		}
	}
}

// Blocked reports whether the key is over the limit without recording.
func (l *Limiter) Blocked(key string) bool {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.hits[key]
	return ok && !l.expired(e.Value.(*bucket), l.now()) && e.Value.(*bucket).n >= l.limit
}

// Len is the number of keys currently tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}
