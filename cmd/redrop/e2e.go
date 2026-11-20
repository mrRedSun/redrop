package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// End-to-end encrypted links on the command line (DESIGN.md "End-to-end
// encrypted links"): --e2e on share, update and watch, the local key store,
// and full links (with #k=) wherever the CLI prints an artifact.

const e2eFlagHelp = "end-to-end encrypt: the key goes in the link after #k= and never reaches the server (see redrop guide)"

// keysPath is the key store, next to the config file.
func keysPath() string {
	return filepath.Join(filepath.Dir(configPath()), "e2e-keys.json")
}

// keyFile is the key store's content: link keys by server URL and artifact ID.
type keyFile struct {
	Version int                          `json:"version"`
	Keys    map[string]map[string]string `json:"keys"`
}

// refuseKeysSymlink applies the config file's rule to the key store: it is
// never read or written through a symlink.
func refuseKeysSymlink(p string) error {
	if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return withCode(exitUsage, "the key store %s is a symlink; pass --config with a real config directory", p)
	}
	return nil
}

func readKeys(p string) (keyFile, error) {
	kf := keyFile{Version: 1, Keys: map[string]map[string]string{}}
	if err := refuseKeysSymlink(p); err != nil {
		return kf, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return kf, nil
	}
	if err != nil {
		return kf, err
	}
	if err := json.Unmarshal(b, &kf); err != nil {
		return kf, fmt.Errorf("the key store %s is damaged: %w", p, err)
	}
	if kf.Keys == nil {
		kf.Keys = map[string]map[string]string{}
	}
	return kf, nil
}

// storedKey returns the saved key of artifact id on server.
func storedKey(server, id string) (client.E2EKey, bool, error) {
	var k client.E2EKey
	kf, err := readKeys(keysPath())
	if err != nil {
		return k, false, err
	}
	text, ok := kf.Keys[server][id]
	if !ok {
		return k, false, nil
	}
	k, err = client.ParseE2EKey(text)
	if err != nil {
		return k, false, fmt.Errorf("the key store %s has a damaged key for %s", keysPath(), id)
	}
	return k, true, nil
}

// changeKeys applies change to the key store under its lock and replaces
// the file atomically.
func changeKeys(change func(kf *keyFile)) error {
	p := keysPath()
	if err := refuseKeysSymlink(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(p + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	kf, err := readKeys(p)
	if err != nil {
		return err
	}
	change(&kf)
	b, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(p, append(b, '\n'))
}

func saveKey(server, id string, key client.E2EKey) error {
	return changeKeys(func(kf *keyFile) {
		if kf.Keys[server] == nil {
			kf.Keys[server] = map[string]string{}
		}
		kf.Keys[server][id] = key.String()
	})
}

func forgetKey(server, id string) error {
	if _, ok, err := storedKey(server, id); err != nil || !ok {
		return err
	}
	return changeKeys(func(kf *keyFile) {
		delete(kf.Keys[server], id)
		if len(kf.Keys[server]) == 0 {
			delete(kf.Keys, server)
		}
	})
}

// Lock file timing: a writer holds the lock for milliseconds; one older than
// lockStale was left by a process that died.
const (
	lockWait  = 10 * time.Second
	lockPoll  = 50 * time.Millisecond
	lockStale = 30 * time.Second
)

// lockFile takes an exclusive lock by creating p, so concurrent shares
// don't lose each other's keys.
func lockFile(p string) (func(), error) {
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if info, serr := os.Lstat(p); serr == nil && time.Since(info.ModTime()) > lockStale {
			os.Remove(p)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the key store is locked by %s; remove it if no redrop command is running", p)
		}
		time.Sleep(lockPoll)
	}
}

// writePrivateFile replaces p with data through a fresh 0600 file in the
// same directory, never writing through a link at p.
func writePrivateFile(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// e2eKeyFor returns the key of artifact id: from the link given as ID|URL,
// else from the key store.
func e2eKeyFor(c *client.Client, arg, id string) (client.E2EKey, error) {
	key, ok, err := client.E2EKeyFromLink(arg)
	if err != nil {
		return key, withCode(exitUsage, "%v", err)
	}
	if ok {
		return key, nil
	}
	key, ok, err = storedKey(c.BaseURL, id)
	if err != nil {
		return key, err
	}
	if !ok {
		return key, withCode(exitUsage, "no key for end-to-end encrypted artifact %s on this machine: pass its full link (with #k=…) instead of the ID", id)
	}
	return key, nil
}

// checkE2EArtifact stops update --e2e and watch --e2e before any upload
// when the artifact isn't end-to-end encrypted.
func checkE2EArtifact(ctx context.Context, c *client.Client, id string) (*api.Artifact, error) {
	a, err := c.GetArtifact(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	if !a.E2E {
		return nil, withCode(exitUsage, "artifact %s is not end-to-end encrypted: publish it without --e2e", id)
	}
	return a, nil
}

// withKeyLink returns a for printing: an end-to-end artifact's url is its
// full link when this machine has the key. Other artifacts are unchanged.
func withKeyLink(a *api.Artifact) *api.Artifact {
	if !a.E2E || g.server == "" {
		return a
	}
	key, ok, err := storedKey(g.server, a.ID)
	if err != nil || !ok {
		return a
	}
	out := *a
	out.URL = client.E2ELink(a.URL, key)
	return &out
}

// rememberKey saves a key that worked, so later commands find it.
func rememberKey(c *client.Client, id string, key client.E2EKey) {
	if old, ok, err := storedKey(c.BaseURL, id); err == nil && ok && old == key {
		return
	}
	if err := saveKey(c.BaseURL, id, key); err != nil {
		fmt.Fprintf(os.Stderr, "redrop: warning: could not save the key in %s: %v\n", keysPath(), err)
	}
}

func e2eNotice(path string) {
	fmt.Fprintf(os.Stderr, "redrop: end-to-end encrypted: the key is the part of the link after #k=; the server never had it.\n"+
		"redrop: anyone with the full link can read this artifact. A lost key can't be recovered; this machine keeps a copy in %s.\n", path)
}
