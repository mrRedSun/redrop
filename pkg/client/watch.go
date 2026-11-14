package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mrRedSun/redrop/pkg/api"
)

// DefaultDebounce is how long the tree must stay quiet after a change
// before Watch publishes (overridable with the --debounce flag).
const DefaultDebounce = 500 * time.Millisecond

// WatchOptions configures Watch. The content to publish is described by
// the Source passed to Watch itself, so there is exactly one description
// of what gets uploaded.
type WatchOptions struct {
	// Debounce is how long the tree must stay quiet after a change before
	// it is picked up (default 500ms).
	Debounce time.Duration
	// Progress receives upload progress, as in Share/Update.
	Progress Progress
	// OnPublish is called after each successful publish.
	OnPublish func(WatchResult)
	// OnReady is called once, after the filesystem watches are registered
	// and the initial content digest is seeded: changes made after OnReady
	// are guaranteed to be picked up.
	OnReady func()
	// Log receives transient-failure notices (retried, not fatal).
	Log func(format string, args ...any)
	// Clock overrides timekeeping (tests). It drives both the debounce
	// window and the retry delays.
	Clock Clock
	// Retrier overrides the transient-failure retry policy (tests).
	Retrier Retrier
}

// Clock is the subset of time Watch needs; injectable for deterministic
// debounce and backoff tests.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Retrier decides the delay before the next attempt after a transient
// failure. attempt starts at 1.
type Retrier interface {
	Next(attempt int) time.Duration
}

// ExponentialRetrier grows the delay exponentially from Base up to Max,
// with up to 25% jitter so many watchers on one host do not retry in
// lockstep.
type ExponentialRetrier struct {
	Base, Max time.Duration
}

func (r ExponentialRetrier) Next(attempt int) time.Duration {
	d := r.Base
	for i := 1; i < attempt && d < r.Max; i++ {
		d *= 2
	}
	if d > r.Max {
		d = r.Max
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 4))
	return min(d+jitter, r.Max)
}

// WatchResult reports one published version.
type WatchResult struct {
	Artifact *api.Artifact
	Version  int
	At       time.Time
}

// Uploader publishes a new version of an artifact and reports the digest
// of the bytes it actually uploaded. *Client satisfies it through
// UpdateDigest; tests use fakes.
type Uploader interface {
	UpdateDigest(ctx context.Context, artifactID string, src *Source, progress Progress) (string, *api.Artifact, error)
}

// ErrWatchClosed is returned when the fsnotify watcher fails or its
// channels close: watching can no longer work, and unlike a clean
// shutdown it is not caused by the caller's context.
var ErrWatchClosed = errors.New("filesystem watcher closed unexpectedly")

// transient reports whether a publish failure should be retried: network
// failures, server 5xx, and rate limiting (429) pass. Everything else —
// auth, not found, too large, bad request, unknown — is fatal: a watcher
// that silently retries an unrecoverable error looks hung.
func transient(err error) bool {
	var ne *NetworkError
	if errors.As(err, &ne) {
		return true
	}
	switch s := StatusOf(err); {
	case s >= 500 || s == 429:
		return true
	default:
		return false
	}
}

// Watch mirrors src.Path onto artifactID: it publishes a new version of
// the same link whenever the content changes, never runs two uploads at
// once, and publishes once more when changes land during an upload. It
// blocks until ctx is done (returning nil) or a fatal error occurs;
// transient publish failures are retried with exponential backoff while
// watching continues.
func Watch(ctx context.Context, up Uploader, artifactID string, src *Source, opts WatchOptions) error {
	if src.Path == "" {
		return errors.New("watch needs a path on disk; stdin cannot be watched")
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Retrier == nil {
		opts.Retrier = ExponentialRetrier{Base: time.Second, Max: 60 * time.Second}
	}
	// A clean root makes parent/child comparisons and event filtering
	// correct for "./site", "site/", and tab-completed variants.
	root := filepath.Clean(src.Path)

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()
	w := &watcher{
		up:         up,
		artifactID: artifactID,
		root:       root,
		src:        src,
		opts:       opts,
		ctx:        ctx,
		fw:         fw,
		events:     fw.Events,
		errors:     fw.Errors,
		quit:       ctx.Done(),
	}
	if err := w.registerAll(); err != nil {
		return err
	}
	return w.run()
}

type watcher struct {
	up         Uploader
	artifactID string
	root       string // cleaned src.Path
	src        *Source
	opts       WatchOptions
	ctx        context.Context
	fw         *fsnotify.Watcher
	events     <-chan fsnotify.Event // injectable for tests
	errors     <-chan error          // injectable for tests
	quit       <-chan struct{}

	dirty     bool      // a change is pending since the last publish
	quietAt   time.Time // earliest moment the tree counts as settled
	published string    // digest of the last successfully uploaded pack
}

// run is the only writer of the watcher state, so the invariants (at most
// one upload at a time, exactly one trailing publish) hold by construction:
// while publishIfChanged runs, no event is read.
func (w *watcher) run() error {
	// Seed the published digest with the tree as found: starting watch
	// never uploads by itself. The server has no content-digest API to
	// compare against, so a tree that already differs from the deployed
	// version syncs on its next change (or via an explicit "redrop update").
	sum, err := hashTree(w.root)
	if err != nil {
		return err
	}
	w.published = sum
	if w.opts.OnReady != nil {
		w.opts.OnReady()
	}
	for {
		if err := w.awaitSettled(); err != nil {
			return w.mapShutdown(err)
		}
		if err := w.publishIfChanged(); err != nil {
			return w.mapShutdown(err)
		}
	}
}

// mapShutdown turns a cancellation that came from our own context into a
// clean nil return; other errors (including ErrWatchClosed) pass through.
func (w *watcher) mapShutdown(err error) error {
	if errors.Is(err, context.Canceled) && w.ctx.Err() != nil {
		return nil // clean shutdown (Ctrl-C, SIGTERM)
	}
	return err
}

// awaitSettled blocks until ctx is done (returning context.Canceled) or
// the tree has been quiet for the debounce interval after a change.
// Events that arrive during the wait extend it.
func (w *watcher) awaitSettled() error {
	for !w.dirty {
		if w.ctx.Err() != nil {
			return context.Canceled
		}
		if err := w.waitEvent(0); err != nil {
			return err
		}
	}
	for w.opts.Clock.Now().Before(w.quietAt) {
		if w.ctx.Err() != nil {
			return context.Canceled
		}
		remaining := w.quietAt.Sub(w.opts.Clock.Now())
		if err := w.waitEvent(remaining); err != nil {
			return err
		}
	}
	w.dirty = false
	return nil
}

// waitEvent consumes one event or timer tick (timeout 0 waits forever).
// The timer goes through opts.Clock so injected clocks control debounce
// end-to-end; with the real clock, a timer whose event path wins the
// select simply expires unreferenced. ErrWatchClosed means the watcher
// died; nil means an event or the timeout was handled.
func (w *watcher) waitEvent(timeout time.Duration) error {
	var timerC <-chan time.Time
	if timeout > 0 {
		timerC = w.opts.Clock.After(timeout)
	}
	select {
	case <-w.quit:
		return nil
	case <-timerC:
		return nil
	case err, ok := <-w.errors:
		if !ok {
			return ErrWatchClosed
		}
		if errors.Is(err, fsnotify.ErrEventOverflow) {
			// The kernel queue overflowed; events were lost. Treat the
			// tree as changed and re-establish the watches so nothing
			// stays unseen.
			w.markDirty(w.root)
			if rerr := w.registerAll(); rerr != nil {
				return rerr
			}
			return nil
		}
		w.logf("watch error: %v", err)
		return nil
	case ev, ok := <-w.events:
		if !ok {
			return ErrWatchClosed
		}
		w.handle(ev)
		return nil
	}
}

// handle records one fsnotify event: content changes mark the tree dirty
// and restart the debounce, newly created directories join the watch, and
// a recreated root is re-watched from scratch.
func (w *watcher) handle(ev fsnotify.Event) {
	if w.ignored(ev.Name) {
		return // skipped entries never affect published content
	}
	switch {
	case ev.Op.Has(fsnotify.Create):
		w.onCreate(ev.Name)
	case ev.Op.Has(fsnotify.Rename) || ev.Op.Has(fsnotify.Remove):
		// Renames arrive as Rename(old) + Create(new). A file root is
		// watched through its parent directory, so the replacement shows
		// up as the Create side. Removals only matter for content.
		w.markDirty(ev.Name)
	default:
		w.markDirty(ev.Name)
	}
}

func (w *watcher) onCreate(name string) {
	fi, err := os.Stat(name)
	if err != nil {
		w.markDirty(name) // vanished again; still treat as a change
		return
	}
	if fi.IsDir() {
		switch name {
		case w.root:
			// The root directory was recreated: rebuild the whole watch.
			if err := w.registerAll(); err != nil {
				w.logf("re-watching %s failed: %v", name, err)
			}
		case filepath.Dir(w.root):
			// The parent changed; nothing to do beyond the dirty mark.
		default:
			if !skipEntryName(filepath.Base(name), true) {
				// Directories the packer skips (.git, node_modules, ...)
				// are not watched either. Best effort: racing with a
				// removal surfaces as a watch error, which is logged.
				_ = watchTree(w.fw, name)
			}
		}
	}
	w.markDirty(name)
}

func (w *watcher) markDirty(name string) {
	w.dirty = true
	w.quietAt = w.opts.Clock.Now().Add(w.opts.Debounce)
}

// ignored reports whether events for this name never affect published
// content: any name outside the root (the parent watch sees siblings too)
// and any name whose last component, or a parent component below the
// root, matches the shared skip predicate (so .git/objects/... and
// node_modules/... events are ignored too).
func (w *watcher) ignored(name string) bool {
	rel, err := filepath.Rel(w.root, name)
	if err != nil {
		return false
	}
	if rel == "." {
		return false
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "../") {
		return true // outside the root: only root events matter
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if skipEntryName(part, i < len(parts)-1) {
			return true
		}
	}
	return false
}

func (w *watcher) logf(format string, args ...any) {
	if w.opts.Log != nil {
		w.opts.Log(format, args...)
	}
}

// publishIfChanged publishes a version when the tree differs from the
// last upload. The recorded state after an upload is the digest of the
// packed bytes themselves (UpdateDigest), so "what was hashed" and "what
// was uploaded" can never diverge.
func (w *watcher) publishIfChanged() error {
	sum, err := hashTree(w.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The root itself vanished. There is nothing to publish: the
			// upload path needs the path on disk. The parent watch sees
			// the root's recreation, registerAll rebuilds the watches,
			// and the recreated content publishes as a normal change
			// (an identical recreation publishes nothing, which is the
			// content-hash rule working as specified).
			w.logf("%s disappeared; waiting for it to come back", w.root)
			return nil
		}
		return err
	}
	if sum == w.published {
		return nil
	}
	_, _, err = w.publishWithRetry()
	return err
}

// publishWithRetry calls Update until it succeeds, the error is fatal, or
// ctx is done. It returns the digest of the uploaded pack and the artifact.
func (w *watcher) publishWithRetry() (string, *api.Artifact, error) {
	for attempt := 1; ; attempt++ {
		sum, a, err := w.up.UpdateDigest(w.ctx, w.artifactID, w.src, w.opts.Progress)
		if err == nil {
			w.published = sum
			if w.opts.OnPublish != nil {
				w.opts.OnPublish(WatchResult{Artifact: a, Version: a.Version, At: w.opts.Clock.Now()})
			}
			return sum, a, nil
		}
		if w.ctx.Err() != nil {
			return "", nil, context.Canceled // clean shutdown, not the upload error
		}
		if !transient(err) {
			return "", nil, err
		}
		w.logf("publish failed (%v); retrying", err)
		if !w.awaitRetry(w.opts.Retrier.Next(attempt)) {
			return "", nil, context.Canceled
		}
	}
}

// awaitRetry waits for a retry delay, reporting whether it elapsed (false
// means ctx was done first).
func (w *watcher) awaitRetry(d time.Duration) bool {
	select {
	case <-w.quit:
		return false
	case <-w.opts.Clock.After(d):
		return true
	}
}

// registerAll establishes every watch. A file root is watched through its
// parent directory: fsnotify follows the name, so rename-over saves and
// delete+recreate still produce events (a watch on the file itself would
// die with its inode on the first atomic save). A directory root watches
// its parent too, so the root's own removal and recreation are seen, plus
// the root and its packable subdirectories. Skipped directories (.git,
// node_modules, ...) are never registered: watching them would burn the
// user's inotify budget and make every git command rehash the tree.
func (w *watcher) registerAll() error {
	if fi, err := os.Stat(w.root); err == nil && !fi.IsDir() {
		return w.fw.Add(filepath.Dir(w.root))
	}
	if parent := filepath.Dir(w.root); parent != w.root {
		if err := w.fw.Add(parent); err != nil {
			return err
		}
	}
	return watchTree(w.fw, w.root)
}

// watchTree adds dir and every packable subdirectory to w.
func watchTree(w *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != dir && skipEntryName(d.Name(), true) {
			return filepath.SkipDir
		}
		return w.Add(p)
	})
}

// hashTree returns a deterministic digest of the content under root: the
// same files the packer would pack (one shared walk and skip predicate),
// each path with the followed file's SHA-256, in sorted order. Identical
// content hashes identically regardless of mtimes or traversal order, so
// unchanged trees never publish.
func hashTree(root string) (string, error) {
	st, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		if skipEntryName(filepath.Base(root), false) {
			return "", nil
		}
		return hashFile(root)
	}
	type entry struct{ rel, sum string }
	var entries []entry
	err = walkPackable(root, func(rel, abs string, info fs.FileInfo) error {
		sum, err := hashFile(abs)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel, sum})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s %s\n", e.rel, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
