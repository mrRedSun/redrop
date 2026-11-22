package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// End-to-end encrypted links on the command line (DESIGN.md "End-to-end
// encrypted links"): --e2e on share, update and watch, the local key store,
// and full links (with #k=) wherever the CLI prints an artifact.

const e2eFlagHelp = "end-to-end encrypt: the key goes in the link after #k= and never reaches the server (see redrop guide)"

// e2eVersionFlagHelp is --e2e on update and watch, which encrypt an
// encrypted artifact's versions whether or not it is given.
const e2eVersionFlagHelp = "insist that the artifact is end-to-end encrypted (its new versions are encrypted either way)"

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

// A writer holds the key store's lock for milliseconds; lockWait only
// bounds a wait behind a stuck process.
const (
	lockWait = 10 * time.Second
	lockPoll = 50 * time.Millisecond
)

// errLocked means another holder has the lock (tryLock, per platform).
var errLocked = errors.New("locked")

// lockFile takes an exclusive OS lock (flock, or LockFileEx on Windows) on
// the file p, so concurrent shares don't lose each other's keys. The
// operating system drops it when the holder exits, so a crashed writer
// leaves no stale lock to take over. The file itself stays.
func lockFile(p string) (func(), error) {
	f, err := os.OpenFile(p, lockOpenFlags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the key store's lock %s: %w", p, err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		err := tryLock(f)
		if err == nil {
			return func() {
				unlock(f)
				f.Close()
			}, nil
		}
		if !errors.Is(err, errLocked) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, errLocked) {
				return nil, fmt.Errorf("the key store is locked by another redrop command (%s)", p)
			}
			return nil, fmt.Errorf("lock the key store: %w", err)
		}
		time.Sleep(lockPoll)
	}
}

// writePrivateFile replaces p with data through a fresh 0600 file in the
// same directory, never writing through a link at p. The config and the
// key store are both written this way.
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

// e2eKeyFor returns the key of artifact id and where it came from: the
// full link given as ID|URL, else the key store.
func e2eKeyFor(c *client.Client, arg, id string) (client.E2EKey, string, error) {
	key, ok, err := client.E2EKeyFromLink(arg)
	if err != nil {
		return key, "", withCode(exitUsage, "%v", err)
	}
	if ok {
		return key, "its link", nil
	}
	key, ok, err = storedKey(c.BaseURL, id)
	if err != nil {
		return key, "", err
	}
	if !ok {
		return key, "", withCode(exitUsage, "artifact %s is end-to-end encrypted and this machine has no key for it, so nothing was uploaded: "+
			"pass its full link on stdin (redrop update - PATH < link.txt) or publish from the machine that shared it", id)
	}
	return key, "this machine's key store", nil
}

// versionKey reads artifact id before update or watch prepares anything,
// so plaintext never leaves this machine for an end-to-end encrypted
// artifact. For such an artifact it returns its key, from the full link in
// arg or the key store, checked against the artifact; without one it
// refuses. --e2e (flag) only insists that the artifact is encrypted.
func versionKey(ctx context.Context, c *client.Client, arg, id string, flag bool) (*client.E2EKey, error) {
	a, err := c.GetArtifact(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	if !a.E2E {
		if flag {
			return nil, withCode(exitUsage, "artifact %s is not end-to-end encrypted: publish it without --e2e", id)
		}
		return nil, nil
	}
	key, from, err := e2eKeyFor(c, arg, id)
	if err != nil {
		return nil, err
	}
	if err := client.CheckE2EKey(a, key); err != nil {
		return nil, mapErr(err)
	}
	if !flag {
		fmt.Fprintf(os.Stderr, "redrop: artifact %s is end-to-end encrypted: encrypting with the key from %s\n", id, from)
	}
	rememberKey(c, id, key)
	return &key, nil
}

// linkFromStdin reads ID|URL from stdin when the argument is "-", so a full
// link's key stays off the command line (shell history, process lists).
func linkFromStdin(arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return "", withCode(exitUsage, "read the link from stdin: %v", err)
	}
	link := strings.TrimSpace(string(b))
	if link == "" || strings.ContainsAny(link, "\n\r") {
		return "", withCode(exitUsage, "stdin must hold one artifact ID or link")
	}
	return link, nil
}

// withoutFragment cuts an ID or link at "#": a #k= key stays on this
// machine and never goes into an API path.
func withoutFragment(arg string) string {
	before, _, _ := strings.Cut(arg, "#")
	return before
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
