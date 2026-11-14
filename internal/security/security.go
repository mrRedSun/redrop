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

type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int
}

func NewLimiter(limit int, window time.Duration) *Limiter {
	l := &Limiter{limit: limit, window: window, hits: map[string]*bucket{}}
	go func() {
		for range time.Tick(window) {
			l.mu.Lock()
			for k, b := range l.hits {
				if time.Since(b.start) > window {
					delete(l.hits, k)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// Allow records a hit and reports whether the key is within its limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.hits[key]
	if b == nil || time.Since(b.start) > l.window {
		b = &bucket{start: time.Now()}
		l.hits[key] = b
	}
	b.n++
	return b.n <= l.limit
}

// Blocked reports whether the key is over the limit without recording.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.hits[key]
	return b != nil && time.Since(b.start) <= l.window && b.n >= l.limit
}
