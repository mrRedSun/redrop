package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

const testServer = "https://redrop.example.com"

func newTestKey(t *testing.T) client.E2EKey {
	t.Helper()
	k, err := client.NewE2EKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyStoreRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	useConfigPath(t, filepath.Join(dir, "config.json"))
	k := newTestKey(t)
	if _, ok, err := storedKey(testServer, "aaaaaaaaaaaaaaaa"); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	if err := saveKey(testServer, "aaaaaaaaaaaaaaaa", k); err != nil {
		t.Fatal(err)
	}
	if keysPath() != filepath.Join(dir, "e2e-keys.json") {
		t.Fatalf("key store at %s, want next to the config", keysPath())
	}
	got, ok, err := storedKey(testServer, "aaaaaaaaaaaaaaaa")
	if err != nil || !ok || got != k {
		t.Fatalf("stored key: %v %v", ok, err)
	}
	if _, ok, _ := storedKey("https://other.example.com", "aaaaaaaaaaaaaaaa"); ok {
		t.Fatal("a key was found for another server")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keysPath())
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("key store mode %v %v", info.Mode(), err)
		}
	}
	if err := forgetKey(testServer, "aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := storedKey(testServer, "aaaaaaaaaaaaaaaa"); ok {
		t.Fatal("forgotten key still stored")
	}
	if err := forgetKey(testServer, "bbbbbbbbbbbbbbbb"); err != nil {
		t.Fatalf("forgetting an unknown key: %v", err)
	}
	requireDirectoryNames(t, dir, []string{"e2e-keys.json", "e2e-keys.json.lock"})
}

func TestKeyStoreRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	useConfigPath(t, filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"keys":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, keysPath()); err != nil {
		t.Fatal(err)
	}
	_, _, err := storedKey(testServer, "aaaaaaaaaaaaaaaa")
	requireExitCode(t, err, exitUsage)
	requireExitCode(t, saveKey(testServer, "aaaaaaaaaaaaaaaa", newTestKey(t)), exitUsage)
	if b, _ := os.ReadFile(target); string(b) != `{"version":1,"keys":{}}` {
		t.Fatalf("the link's target was written: %s", b)
	}
}

// The re-check of review 33 (n-2): a link at the lock's path is refused,
// and nothing is created where it points, by checks that hold on every
// platform (Unix also opens with O_NOFOLLOW).
func TestKeyStoreLockRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	useConfigPath(t, filepath.Join(dir, "config.json"))
	target := filepath.Join(dir, "elsewhere")
	if err := os.Symlink(target, keysPath()+".lock"); err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, saveKey(testServer, "aaaaaaaaaaaaaaaa", newTestKey(t)), exitUsage)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock created the link's target: %v", err)
	}
	if _, err := os.Lstat(keysPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key was saved without the lock: %v", err)
	}
}

// Concurrent shares each keep their key: writers take the lock.
func TestKeyStoreConcurrentWriters(t *testing.T) {
	useConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	const n = 12
	keys := make([]client.E2EKey, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range keys {
		keys[i] = newTestKey(t)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- saveKey(testServer, fmt.Sprintf("artifact%08d", i), keys[i])
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, k := range keys {
		if got, ok, err := storedKey(testServer, fmt.Sprintf("artifact%08d", i)); err != nil || !ok || got != k {
			t.Fatalf("key %d lost: %v %v", i, ok, err)
		}
	}
}

// The lock is the operating system's: a held lock keeps a second writer
// out until it is released, and a lock file left by a writer that died
// holds nothing, so there is no stale lock to take over (review 33 m-B).
func TestKeyStoreLock(t *testing.T) {
	useConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	lock := keysPath() + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil { // left behind
		t.Fatal(err)
	}
	if err := saveKey(testServer, "aaaaaaaaaaaaaaaa", newTestKey(t)); err != nil {
		t.Fatalf("a leftover lock file blocked the store: %v", err)
	}
	unlock, err := lockFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	k := newTestKey(t)
	go func() { saved <- saveKey(testServer, "bbbbbbbbbbbbbbbb", k) }()
	// The writer waits for the lock: it can't finish while it is held. Its
	// result arrives only after unlock, so this is not a timing assertion.
	select {
	case err := <-saved:
		t.Fatalf("a second writer got in while the lock was held: %v", err)
	default:
	}
	if _, ok, _ := storedKey(testServer, "bbbbbbbbbbbbbbbb"); ok {
		t.Fatal("a second writer wrote while the lock was held")
	}
	unlock()
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if got, ok, err := storedKey(testServer, "bbbbbbbbbbbbbbbb"); err != nil || !ok || got != k {
		t.Fatalf("key after unlock: %v %v", ok, err)
	}
	if _, ok, _ := storedKey(testServer, "aaaaaaaaaaaaaaaa"); !ok {
		t.Fatal("the first key was lost")
	}
}

func TestKeyStoreDamaged(t *testing.T) {
	useConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	if err := os.WriteFile(keysPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storedKey(testServer, "aaaaaaaaaaaaaaaa"); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("damaged store: %v", err)
	}
	if err := saveKey(testServer, "aaaaaaaaaaaaaaaa", newTestKey(t)); err == nil {
		t.Fatal("a damaged store was overwritten")
	}
}

func TestE2EKeyFor(t *testing.T) {
	useConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	c := client.New(testServer, "rdp_x")
	k := newTestKey(t)
	link := client.E2ELink("https://amber-otter-quilt-river.usercontent.example/", k)
	if got, from, err := e2eKeyFor(c, link, "aaaaaaaaaaaaaaaa"); err != nil || got != k || from != "its link" {
		t.Fatalf("key from the link: %v", err)
	}
	_, _, err := e2eKeyFor(c, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa")
	requireExitCode(t, err, exitUsage)
	if !strings.Contains(err.Error(), "nothing was uploaded") {
		t.Fatalf("missing-key message: %v", err)
	}
	_, _, err = e2eKeyFor(c, "https://amber-otter-quilt-river.usercontent.example/#k=broken", "aaaaaaaaaaaaaaaa")
	requireExitCode(t, err, exitUsage)
	if err := saveKey(testServer, "aaaaaaaaaaaaaaaa", k); err != nil {
		t.Fatal(err)
	}
	if got, from, err := e2eKeyFor(c, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa"); err != nil || got != k || from != "this machine's key store" {
		t.Fatalf("key from the store: %v", err)
	}
}

func TestPrintedLinkCarriesTheStoredKey(t *testing.T) {
	useConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	g.server = testServer
	k := newTestKey(t)
	a := &api.Artifact{ID: "aaaaaaaaaaaaaaaa", URL: "https://amber-otter-quilt-river.usercontent.example/", E2E: true}
	if got := withKeyLink(a); got.URL != a.URL {
		t.Fatalf("no stored key, link %s", got.URL)
	}
	if err := saveKey(testServer, a.ID, k); err != nil {
		t.Fatal(err)
	}
	if got := withKeyLink(a); got.URL != client.E2ELink(a.URL, k) || a.URL == got.URL {
		t.Fatalf("link with key %s", got.URL)
	}
	plain := &api.Artifact{ID: a.ID, URL: a.URL}
	if withKeyLink(plain).URL != a.URL {
		t.Fatal("a plaintext artifact's link changed")
	}
}
