// Package ids generates random identifiers and secrets.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

var enc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Random returns n random bytes encoded as lowercase base32 (DNS-label safe).
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return enc.EncodeToString(b)
}

// ID is a short, stable public identifier (80 bits).
func ID() string { return Random(10) }

// Slug is the secret artifact subdomain (130 bits, 26 chars).
func Slug() string { return Random(16)[:26] }

// Secret returns a 256-bit secret string.
func Secret() string { return Random(32) }

// Hash returns the hex SHA-256 of s, used to store bearer secrets.
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ValidSlug reports whether s looks like a slug (cheap pre-check before DB).
func ValidSlug(s string) bool {
	if len(s) != 26 {
		return false
	}
	return strings.Trim(s, "abcdefghijklmnopqrstuvwxyz234567") == ""
}
