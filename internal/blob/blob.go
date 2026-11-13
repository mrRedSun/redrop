// Package blob is a content-addressed (SHA-256) file store.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct {
	root string // <data>/blobs
	tmp  string // <data>/tmp
}

func New(dataDir string) (*Store, error) {
	s := &Store{root: filepath.Join(dataDir, "blobs"), tmp: filepath.Join(dataDir, "tmp")}
	for _, d := range []string{s.root, s.tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil && strings.ToLower(h) == h
}

func (s *Store) path(h string) string { return filepath.Join(s.root, h[:2], h[2:4], h) }

// ErrTooLarge is returned when a stream exceeds the given limit.
var ErrTooLarge = errors.New("content exceeds size limit")

// Put stores r and returns its hash and size. A non-negative limit caps the
// size (0 allows only empty content); a negative limit means unlimited.
func (s *Store) Put(r io.Reader, limit int64) (string, int64, error) {
	f, err := os.CreateTemp(s.tmp, "blob-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	src := r
	if limit >= 0 {
		src = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err == nil && limit >= 0 && n > limit {
		err = ErrTooLarge
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	dst := s.path(sum)
	if _, err := os.Stat(dst); err == nil {
		now := time.Now()
		os.Chtimes(dst, now, now) // protect from concurrent GC
		return sum, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", 0, err
	}
	return sum, n, nil
}

// Open returns the blob for reading.
func (s *Store) Open(h string) (*os.File, error) {
	if !validHash(h) {
		return nil, fs.ErrNotExist
	}
	return os.Open(s.path(h))
}

// GC removes blobs that are not referenced and older than minAge.
func (s *Store) GC(referenced map[string]struct{}, minAge time.Duration) (removed int, freed int64, err error) {
	cutoff := time.Now().Add(-minAge)
	err = filepath.WalkDir(s.root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		name := d.Name()
		if !validHash(name) {
			return nil
		}
		if _, ok := referenced[name]; ok {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if os.Remove(p) == nil {
			removed++
			freed += info.Size()
		}
		return nil
	})
	return
}

// Usage returns the bytes stored on disk.
func (s *Store) Usage() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// CleanTmp removes temporary files older than minAge.
func (s *Store) CleanTmp(minAge time.Duration) {
	entries, _ := os.ReadDir(s.tmp)
	cutoff := time.Now().Add(-minAge)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(s.tmp, e.Name()))
		}
	}
}

// TempDir is where callers may stage temporary files.
func (s *Store) TempDir() string { return s.tmp }
