// Package e2e is the format of end-to-end encrypted redrop links (DESIGN.md
// "End-to-end encrypted links", "Format"). The CLI encrypts with it; the
// server only checks an object's magic; viewers decrypt with the JavaScript
// implementation in internal/content/static/e2e.js, which the vectors in
// testdata keep in step with this one.
//
// An object is magic ‖ salt ‖ segments. Each segment is AES-256-GCM over up
// to SegmentSize plaintext bytes, under a per-object key derived from the
// link key and the salt, with nonce uint88(index) ‖ last. Every segment
// authenticates the object's Binding (artifact ID, version, type and path).
package e2e

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const (
	// FormatVersion is the format this package writes and reads.
	FormatVersion = 1
	// KeySize is the link key's size in bytes.
	KeySize = 32
	// SaltSize is the per-object random salt's size in bytes.
	SaltSize = 16
	// HeaderSize is the magic plus the salt.
	HeaderSize = 4 + SaltSize
	// SegmentSize is the plaintext size of every segment but the last.
	SegmentSize = 64 << 10
	// TagSize is the GCM tag after each segment.
	TagSize = 16
	// KeyCheckSize is the key check's size in bytes.
	KeyCheckSize = 16
	// KeyTextLen is the length of a key in a link (unpadded base64url).
	KeyTextLen = 43
)

// Magic starts every object: "RDE" and the format version.
var Magic = [4]byte{'R', 'D', 'E', FormatVersion}

// HKDF info strings.
const (
	infoObject   = "redrop-e2e/1 object"
	infoKeyCheck = "redrop-e2e/1 key-check\x00"
)

var (
	// ErrFormat means the bytes are not a format-1 object at all.
	ErrFormat = errors.New("not a redrop end-to-end encrypted object")
	// ErrAuth means an object failed authentication: the wrong key, damaged
	// or truncated bytes, or an object presented for another artifact,
	// version, path or type.
	ErrAuth = errors.New("end-to-end encrypted object failed authentication")
)

var b64 = base64.RawURLEncoding.Strict()

// Key is an artifact's link key.
type Key [KeySize]byte

// NewKey returns a key read from rnd (crypto/rand.Reader outside tests).
func NewKey(rnd io.Reader) (Key, error) {
	var k Key
	_, err := io.ReadFull(rnd, k[:])
	return k, err
}

// String is the key as it appears in links: unpadded base64url.
func (k Key) String() string { return b64.EncodeToString(k[:]) }

// ParseKey reads a key in its link form.
func ParseKey(s string) (Key, error) {
	var k Key
	if len(s) != KeyTextLen {
		return k, errors.New("an end-to-end key is 43 characters of base64url")
	}
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != KeySize {
		return k, errors.New("an end-to-end key is 43 characters of base64url")
	}
	copy(k[:], b)
	return k, nil
}

// Check is the key check stored with an artifact: it shows whether a key is
// the artifact's without revealing anything about the key.
func (k Key) Check(artifactID string) string {
	b, err := hkdf.Key(sha256.New, k[:], nil, infoKeyCheck+artifactID, KeyCheckSize)
	if err != nil {
		panic(err) // only for lengths HKDF can't produce
	}
	return b64.EncodeToString(b)
}

// ValidKeyCheck reports whether s has the form of a key check.
func ValidKeyCheck(s string) bool {
	b, err := b64.DecodeString(s)
	return err == nil && len(b) == KeyCheckSize && len(s) == 22
}

// ObjectType says what an object holds; it is authenticated.
type ObjectType byte

const (
	ObjectManifest ObjectType = 1
	ObjectFile     ObjectType = 2
)

// Binding is what an object is bound to. Opening it under any other binding
// fails with ErrAuth.
type Binding struct {
	Type     ObjectType
	Artifact string // the artifact ID
	Version  int    // the version number, from 1
	Path     string // the file's path in the version; "" for the manifest
}

// ManifestBinding is the binding of version's manifest.
func ManifestBinding(artifact string, version int) Binding {
	return Binding{Type: ObjectManifest, Artifact: artifact, Version: version}
}

// FileBinding is the binding of the file at path in version.
func FileBinding(artifact string, version int, path string) Binding {
	return Binding{Type: ObjectFile, Artifact: artifact, Version: version, Path: path}
}

// aad is the authenticated data of every segment of the object whose
// header (magic and salt) is given.
func (b Binding) aad(header []byte) ([]byte, error) {
	switch {
	case b.Type != ObjectManifest && b.Type != ObjectFile:
		return nil, fmt.Errorf("e2e: unknown object type %d", b.Type)
	case b.Type == ObjectManifest && b.Path != "":
		return nil, errors.New("e2e: the manifest has no path")
	case b.Type == ObjectFile && b.Path == "":
		return nil, errors.New("e2e: a file needs a path")
	case b.Artifact == "" || len(b.Artifact) > math.MaxUint16 || len(b.Path) > math.MaxUint16:
		return nil, errors.New("e2e: artifact ID or path out of range")
	case b.Version < 1 || int64(b.Version) > math.MaxUint32:
		return nil, errors.New("e2e: version out of range")
	}
	out := make([]byte, 0, len(header)+1+2+len(b.Artifact)+4+2+len(b.Path))
	out = append(out, header...)
	out = append(out, byte(b.Type))
	out = binary.BigEndian.AppendUint16(out, uint16(len(b.Artifact)))
	out = append(out, b.Artifact...)
	out = binary.BigEndian.AppendUint32(out, uint32(b.Version))
	out = binary.BigEndian.AppendUint16(out, uint16(len(b.Path)))
	return append(out, b.Path...), nil
}

// objectAEAD is the object key's cipher.
func objectAEAD(k Key, salt []byte) (cipher.AEAD, error) {
	ok, err := hkdf.Key(sha256.New, k[:], salt, infoObject, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(ok)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// nonce is uint88(i) ‖ last.
func nonce(i uint64, last bool) []byte {
	var n [12]byte
	binary.BigEndian.PutUint64(n[3:11], i)
	if last {
		n[11] = 1
	}
	return n[:]
}

// SealedSize is the size of the object for n plaintext bytes.
func SealedSize(n int64) int64 {
	segments := max(1, (n+SegmentSize-1)/SegmentSize)
	return HeaderSize + n + segments*TagSize
}

// Seal writes the object for exactly n bytes read from src, under a fresh
// salt from rnd (crypto/rand.Reader outside tests). A src that ends early
// returns io.ErrUnexpectedEOF; bytes after the first n are not read.
func Seal(dst io.Writer, src io.Reader, n int64, k Key, b Binding, rnd io.Reader) error {
	if n < 0 {
		return errors.New("e2e: negative size")
	}
	header := make([]byte, HeaderSize)
	copy(header, Magic[:])
	if _, err := io.ReadFull(rnd, header[4:]); err != nil {
		return fmt.Errorf("e2e: salt: %w", err)
	}
	aad, err := b.aad(header)
	if err != nil {
		return err
	}
	aead, err := objectAEAD(k, header[4:])
	if err != nil {
		return err
	}
	if _, err := dst.Write(header); err != nil {
		return err
	}
	buf := make([]byte, SegmentSize+TagSize)
	left := n
	for i := uint64(0); ; i++ {
		size := min(left, SegmentSize)
		if _, err := io.ReadFull(src, buf[:size]); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		left -= size
		out := aead.Seal(buf[:0], nonce(i, left == 0), buf[:size], aad)
		if _, err := dst.Write(out); err != nil {
			return err
		}
		if left == 0 {
			return nil
		}
	}
}

// Open writes the plaintext of the object read from src to dst. Only
// authenticated segments are written; a damaged or truncated object returns
// ErrAuth, possibly after earlier segments were written. Callers that must
// not release any byte of a damaged object open into a buffer (OpenBytes).
func Open(dst io.Writer, src io.Reader, k Key, b Binding) error {
	r := bufio.NewReaderSize(src, SegmentSize+TagSize)
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrFormat
		}
		return err
	}
	if !bytes.Equal(header[:4], Magic[:]) {
		return ErrFormat
	}
	aad, err := b.aad(header)
	if err != nil {
		return err
	}
	aead, err := objectAEAD(k, header[4:])
	if err != nil {
		return err
	}
	chunk := make([]byte, SegmentSize+TagSize)
	for i := uint64(0); ; i++ {
		n, err := io.ReadFull(r, chunk)
		last := false
		switch {
		case errors.Is(err, io.EOF):
			return ErrAuth // the previous segment was not the last
		case errors.Is(err, io.ErrUnexpectedEOF):
			last = true
		case err != nil:
			return err
		default:
			if _, perr := r.Peek(1); errors.Is(perr, io.EOF) {
				last = true
			} else if perr != nil {
				return perr
			}
		}
		if n < TagSize {
			return ErrAuth
		}
		plain, err := aead.Open(chunk[:0], nonce(i, last), chunk[:n], aad)
		if err != nil {
			return ErrAuth
		}
		if last && len(plain) == 0 && i > 0 {
			return ErrAuth // only an empty object ends with an empty segment
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// OpenBytes returns the plaintext of obj, or an error and nothing.
func OpenBytes(obj []byte, k Key, b Binding) ([]byte, error) {
	var out bytes.Buffer
	if err := Open(&out, bytes.NewReader(obj), k, b); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// HasMagic reports whether head starts like a format-1 object.
func HasMagic(head []byte) bool { return bytes.HasPrefix(head, Magic[:]) }

// Link returns the artifact's link with the key in its fragment.
func Link(artifactURL string, k Key) string {
	base, _, _ := strings.Cut(artifactURL, "#")
	return base + "#k=" + k.String()
}

// SplitFragment splits a link fragment (without "#") into the key text and
// the rest (DESIGN.md "Keys and links"): "k=<43 characters>", optionally
// followed by "&" and the rest. Without a key the whole fragment is the rest.
func SplitFragment(fragment string) (key, rest string, ok bool) {
	if !strings.HasPrefix(fragment, "k=") || len(fragment) < 2+KeyTextLen {
		return "", fragment, false
	}
	key, rest = fragment[2:2+KeyTextLen], fragment[2+KeyTextLen:]
	for _, c := range key {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", fragment, false
		}
	}
	switch {
	case rest == "":
		return key, "", true
	case rest[0] == '&':
		return key, rest[1:], true
	}
	return "", fragment, false
}

// KeyFromLink returns the key in a link's fragment. ok is false when the
// link has none; err is set when it has one that is malformed.
func KeyFromLink(link string) (k Key, ok bool, err error) {
	_, fragment, found := strings.Cut(strings.TrimSpace(link), "#")
	if !found {
		return k, false, nil
	}
	text, _, ok := SplitFragment(fragment)
	if !ok {
		if strings.HasPrefix(fragment, "k=") {
			return k, false, errors.New("the link's #k= key is malformed; copy the whole link")
		}
		return k, false, nil
	}
	k, err = ParseKey(text)
	return k, err == nil, err
}
