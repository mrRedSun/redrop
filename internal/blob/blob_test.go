package blob

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gcBlob(t *testing.T, s *Store, digit string, body string, at time.Time) string {
	t.Helper()
	hash := strings.Repeat(digit, 64)
	path := s.path(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestGCContinuesAfterRemoveFailures(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1, 0)
	first := gcBlob(t, s, "0", "blocked first", old)
	second := gcBlob(t, s, "1", "two", old)
	third := gcBlob(t, s, "2", "blocked third", old)
	fourth := gcBlob(t, s, "3", "four", old)
	firstErr, thirdErr := errors.New("first remove failure"), errors.New("third remove failure")
	attempts := make(map[string]bool)
	removed, freed, err := s.gc(nil, 0, func(path string) error {
		attempts[filepath.Base(path)] = true
		switch filepath.Base(path) {
		case first:
			return &fs.PathError{Op: "remove", Path: path, Err: firstErr}
		case third:
			return &fs.PathError{Op: "remove", Path: path, Err: thirdErr}
		default:
			return os.Remove(path)
		}
	})
	if !errors.Is(err, firstErr) || !errors.Is(err, thirdErr) {
		t.Errorf("GC error = %v, want both remove errors", err)
	}
	if removed != 2 || freed != 7 || len(attempts) != 4 {
		t.Errorf("GC stopped after failure: removed=%d freed=%d attempted=%d, want 2, 7, 4", removed, freed, len(attempts))
	}
	for _, hash := range []string{first, third} {
		if _, err := os.Stat(s.path(hash)); err != nil {
			t.Errorf("failed-remove blob %s was lost: %v", hash, err)
		}
	}
	for _, hash := range []string{second, fourth} {
		if _, err := os.Stat(s.path(hash)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("later blob %s not collected: %v", hash, err)
		}
	}
}

func TestGCRetainsReferencesAndRecentBlobs(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1, 0)
	referenced := gcBlob(t, s, "0", "referenced", old)
	recent := gcBlob(t, s, "1", "recent", time.Unix(4_102_444_800, 0))
	orphan := gcBlob(t, s, "2", "orphan", old)
	removed, freed, err := s.GC(map[string]struct{}{referenced: {}}, time.Hour)
	if err != nil || removed != 1 || freed != 6 {
		t.Fatalf("GC = %d, %d, %v; want 1, 6, nil", removed, freed, err)
	}
	for _, hash := range []string{referenced, recent} {
		if _, err := os.Stat(s.path(hash)); err != nil {
			t.Fatalf("protected blob %s lost: %v", hash, err)
		}
	}
	if _, err := os.Stat(s.path(orphan)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphan not removed: %v", err)
	}
}

func TestGCIgnoresAlreadyRemovedBlob(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gcBlob(t, s, "0", "gone", time.Unix(1, 0))
	removed, freed, err := s.gc(nil, 0, func(path string) error {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
	})
	if err != nil || removed != 0 || freed != 0 {
		t.Fatalf("missing blob = %d, %d, %v; want 0, 0, nil", removed, freed, err)
	}
}
