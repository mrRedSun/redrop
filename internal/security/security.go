// Package security contains password hashing, signed values, and rate limiting.
package security

import (
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

// Limiter is a fixed-window counter per key. It tracks at most maxKeys keys:
// when every slot holds a live window, a new key is refused (Allow returns
// false) until a window expires, so a flood of distinct keys cannot grow
// memory. Known keys keep their own count. A background goroutine drops
// expired windows once per window; Close stops it.
type Limiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	maxKeys   int
	now       func() time.Time
	lastSweep time.Time
	hits      map[string]bucket

	stop      chan struct{} // closed by Close
	done      chan struct{} // closed when the sweeper has returned
	closeOnce sync.Once
}

type bucket struct {
	start time.Time
	n     int
}

// NewLimiter allows limit hits per key per window, for at most maxKeys keys
// at a time. now is the clock (time.Now, or a test's).
func NewLimiter(limit int, window time.Duration, maxKeys int, now func() time.Time) *Limiter {
	l := &Limiter{
		limit: limit, window: window, maxKeys: maxKeys, now: now, hits: map[string]bucket{},
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

func (l *Limiter) dropExpired(now time.Time) {
	l.lastSweep = now
	for k, b := range l.hits {
		if now.Sub(b.start) > l.window {
			delete(l.hits, k)
		}
	}
}

// room reports whether a new key fits, dropping expired windows first. The
// scan runs at most once per sweepInterval, so a flood of new keys against a
// full table cannot turn every request into an O(maxKeys) scan.
func (l *Limiter) room(now time.Time) bool {
	if len(l.hits) < l.maxKeys {
		return true
	}
	if now.Sub(l.lastSweep) >= sweepInterval {
		l.dropExpired(now)
	}
	return len(l.hits) < l.maxKeys
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

// Allow records a hit and reports whether the key is within its limit. A
// new key is refused while the table is full.
func (l *Limiter) Allow(key string) bool {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.hits[key]
	if !ok && !l.room(now) {
		return false
	}
	if !ok || now.Sub(b.start) > l.window {
		b = bucket{start: now}
	}
	b.n++
	l.hits[key] = b
	return b.n <= l.limit
}

// Refund takes back one hit recorded for key in its current window. Callers
// that charge an attempt before checking it refund the ones that succeed,
// so only failures use up the limit.
func (l *Limiter) Refund(key string) {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.hits[key]; ok && b.n > 0 {
		b.n--
		l.hits[key] = b
	}
}

// Blocked reports whether the key is over the limit without recording.
func (l *Limiter) Blocked(key string) bool {
	key = limiterKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.hits[key]
	return ok && l.now().Sub(b.start) <= l.window && b.n >= l.limit
}

// Len is the number of keys currently tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}
