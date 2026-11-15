package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func useConfigPath(t *testing.T, p string) {
	t.Helper()
	old := g
	g = globalOpts{config: p}
	t.Cleanup(func() { g = old })
}

func requireSavedConfig(t *testing.T, p string, want fileConfig) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("saved config is not a regular file: %v", info.Mode())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("saved config mode = %04o, want 0600", info.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got fileConfig
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != want || !bytes.HasSuffix(b, []byte{'\n'}) {
		t.Error("saved config does not match or is missing its trailing newline")
	}
}

func requireDirectoryNames(t *testing.T, dir string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("directory entries = %q, want %q; temporary config leaked", got, want)
	}
}

func TestSaveConfigPrivateAtomicReplacement(t *testing.T) {
	for _, name := range []string{"first creation", "existing 0644", "existing 0600"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "new-directory", "config.json")
			useConfigPath(t, p)
			var old *os.File
			const original = "{\"url\":\"https://old.example\",\"token\":\"synthetic-old-token\"}\n"
			if name != "first creation" {
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0o600)
				if name == "existing 0644" {
					mode = 0o644
				}
				if err := os.Chmod(p, mode); err != nil {
					t.Fatal(err)
				}
				var err error
				old, err = os.Open(p)
				if err != nil {
					t.Fatal(err)
				}
				defer old.Close()
			}
			want := fileConfig{URL: "https://redrop.example.com", Token: "synthetic-new-token"}
			if err := saveConfig(want); err != nil {
				t.Fatal(err)
			}
			requireSavedConfig(t, p, want)
			if old != nil {
				b, err := io.ReadAll(old)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != original {
					t.Error("existing config inode was overwritten instead of atomically replaced")
				}
			}
			// A later write, including logout, must replace even a 0600 file.
			current, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer current.Close()
			before, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			want.Token = ""
			if err := saveConfig(want); err != nil {
				t.Fatal(err)
			}
			requireSavedConfig(t, p, want)
			preserved, err := io.ReadAll(current)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(preserved, before) {
				t.Error("subsequent config write overwrote the previous inode")
			}
			requireDirectoryNames(t, filepath.Dir(p), []string{"config.json"})
		})
	}
}

func TestSaveConfigDoesNotFollowDestinationSymlink(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		name := "existing target"
		if dangling {
			name = "dangling target"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p, target := filepath.Join(dir, "config.json"), filepath.Join(t.TempDir(), "target.json")
			useConfigPath(t, p)
			const original = "target must stay untouched\n"
			if !dangling {
				if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, p); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink unavailable: %v", err)
				}
				t.Fatal(err)
			}
			want := fileConfig{URL: "https://redrop.example.com", Token: "synthetic-token"}
			if err := saveConfig(want); err != nil {
				t.Fatal(err)
			}
			requireSavedConfig(t, p, want)
			if dangling {
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Errorf("dangling symlink target was created: %v", err)
				}
			} else {
				b, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != original {
					t.Error("destination symlink target was overwritten")
				}
				info, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
					t.Errorf("target mode changed to %04o", info.Mode().Perm())
				}
			}
			requireDirectoryNames(t, dir, []string{"config.json"})
		})
	}
}

func TestSaveConfigFailurePreservesDestination(t *testing.T) {
	t.Run("rename failure", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.json")
		useConfigPath(t, p)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(p, "preserved")
		if err := os.WriteFile(marker, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := saveConfig(fileConfig{Token: "synthetic-token"}); err == nil {
			t.Fatal("config write over a directory succeeded")
		}
		b, err := os.ReadFile(marker)
		if err != nil || string(b) != "original" {
			t.Fatalf("failed write damaged the destination: %v", err)
		}
		requireDirectoryNames(t, dir, []string{"config.json"})
		requireDirectoryNames(t, p, []string{"preserved"})
	})
	t.Run("parent creation failure", func(t *testing.T) {
		dir := t.TempDir()
		parent := filepath.Join(dir, "not-a-directory")
		if err := os.WriteFile(parent, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		useConfigPath(t, filepath.Join(parent, "config.json"))
		if err := saveConfig(fileConfig{Token: "synthetic-token"}); err == nil {
			t.Fatal("config write below a regular file succeeded")
		}
		b, err := os.ReadFile(parent)
		if err != nil || string(b) != "original" {
			t.Fatalf("failed write damaged the parent: %v", err)
		}
		requireDirectoryNames(t, dir, []string{"not-a-directory"})
	})
}
