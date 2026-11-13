// Package ids generates random identifiers and secrets, and owns the artifact
// slug format (the secret subdomain label of every artifact link).
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
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

// Secret returns a 256-bit secret string.
func Secret() string { return Random(32) }

// Hash returns the hex SHA-256 of s, used to store bearer secrets.
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
