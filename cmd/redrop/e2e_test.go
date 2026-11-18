package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
	requireDirectoryNames(t, dir, []string{"e2e-keys.json"})
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

func TestKeyStoreStaleLock(t *testing.T) {
	useConfigPath(t, filepath.Join(t.TempDir(), "config.json"))
	lock := keysPath() + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if err := saveKey(testServer, "aaaaaaaaaaaaaaaa", newTestKey(t)); err != nil {
		t.Fatalf("a stale lock blocked the store: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
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
	if got, err := e2eKeyFor(c, link, "aaaaaaaaaaaaaaaa"); err != nil || got != k {
		t.Fatalf("key from the link: %v", err)
	}
	_, err := e2eKeyFor(c, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa")
	requireExitCode(t, err, exitUsage)
	if !strings.Contains(err.Error(), "#k=") {
		t.Fatalf("missing-key message: %v", err)
	}
	_, err = e2eKeyFor(c, "https://amber-otter-quilt-river.usercontent.example/#k=broken", "aaaaaaaaaaaaaaaa")
	requireExitCode(t, err, exitUsage)
	if err := saveKey(testServer, "aaaaaaaaaaaaaaaa", k); err != nil {
		t.Fatal(err)
	}
	if got, err := e2eKeyFor(c, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa"); err != nil || got != k {
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
