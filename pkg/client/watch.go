package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// WatchOptions configures Watch.
type WatchOptions struct {
	// Debounce is how long the tree must stay quiet after a change before
	// it is picked up (default 500ms).
	Debounce time.Duration
	// Kind, Entry, SPA, and Note mirror the Source fields of the same
	// names; they apply to every version Watch publishes.
	Kind  string
	Entry string
	SPA   bool
	Note  string
	// Progress receives upload progress, as in Share/Update.
	Progress Progress
	// OnPublish is called after each successful publish.
	OnPublish func(WatchResult)
	// Log receives transient-failure notices (retried, not fatal).
	Log func(format string, args ...any)
	// RetryBackoff overrides the transient-failure retry delay (tests).
	RetryBackoff time.Duration
}

// WatchResult reports one published version.
type WatchResult struct {
	Artifact *api.Artifact
	Version  int
	At       time.Time
}

// Uploader publishes a new version of an artifact. *Client satisfies it
// through Update; tests use fakes.
type Uploader interface {
	Update(ctx context.Context, artifactID string, src *Source, progress Progress) (*api.Artifact, error)
}

// classifyPublishError decides whether a publish failure is transient
// (network, server 5xx: retry) or fatal (auth, not found, too large, bad
// request: stop). Everything unrecognized is fatal: a watcher that silently
// retries an unrecoverable error looks hung.
func classifyPublishError(err error) retryDecision {
	var ne *NetworkError
	if errors.As(err, &ne) {
		return retryDecision{retry: true}
	}
	switch s := StatusOf(err); {
	case s >= 500:
		return retryDecision{retry: true}
	default:
		return retryDecision{retry: false}
	}
}

type retryDecision struct {
	retry bool
}

// Watch mirrors path onto artifactID: it publishes a new version whenever
// the tree's content changes, never runs two uploads at once, and publishes
// once more when changes land during an upload. It blocks until ctx is
// done; cancellation (Ctrl-C, SIGTERM) returns nil, fatal publish errors
// return the error, transient ones are retried forever with backoff.
func Watch(ctx context.Context, up Uploader, artifactID, root string, opts WatchOptions) error {
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = 3 * time.Second
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()
	if err := watchTree(fw, root); err != nil {
		return err
	}

	w := &watcher{
		up:         up,
		artifactID: artifactID,
		root:       root,
		opts:       opts,
		ctx:        ctx,
		fsWatcher:  fw,
		events:     fw.Events,
		errors:     fw.Errors,
		quit:       ctx.Done(),
	}
	return w.run()
}

type watcher struct {
	up         Uploader
	artifactID string
	root       string
	opts       WatchOptions
	ctx        context.Context
	fsWatcher  *fsnotify.Watcher
	events     <-chan fsnotify.Event
	errors     <-chan error
	quit       <-chan struct{}

	dirty     bool      // a change is pending since the last publish
	quietAt   time.Time // earliest moment the tree counts as settled
	published string    // hash of the last successfully published tree
}

// run is the only writer of the watcher state, so the invariants (at most
// one upload at a time, exactly one trailing publish) hold by construction:
// while publishIfChanged runs, no event is read.
func (w *watcher) run() error {
	// Seed the published hash with the tree as found: starting watch never
	// uploads by itself. The server has no content-hash API to compare
	// against, so a tree that already differs from the deployed version
	// syncs on its next change (or via an explicit "redrop update").
	sum, err := HashTree(w.root)
	if err != nil {
		return err
	}
	w.published = sum
	for {
		if err := w.awaitSettled(); err != nil {
			return err
		}
		if err := w.publishIfChanged(); err != nil {
			return err
		}
	}
}

// awaitSettled blocks until ctx is done (returning context.Canceled) or the
// tree has been quiet for the debounce interval after a change. Events that
// arrive during the wait extend it.
func (w *watcher) awaitSettled() error {
	for !w.dirty {
		select {
		case <-w.quit:
			return context.Canceled
		case err, ok := <-w.errors:
			if !ok {
				return context.Canceled
			}
			w.logf("watch error: %v", err)
		case ev, ok := <-w.events:
			if !ok {
				return context.Canceled
			}
			w.handle(ev)
		}
	}
	for time.Now().Before(w.quietAt) {
		timer := time.NewTimer(time.Until(w.quietAt))
		select {
		case <-w.quit:
			timer.Stop()
			return context.Canceled
		case err, ok := <-w.errors:
			if !ok {
				timer.Stop()
				return context.Canceled
			}
			w.logf("watch error: %v", err)
		case ev, ok := <-w.events:
			if !ok {
				timer.Stop()
				return context.Canceled
			}
			timer.Stop()
			w.handle(ev)
		case <-timer.C:
		}
	}
	w.dirty = false
	return nil
}

// handle records one fsnotify event: content changes mark the tree dirty
// and restart the debounce, newly created directories join the watch.
func (w *watcher) handle(ev fsnotify.Event) {
	if w.ignored(ev.Name) {
		return // editor temp/swap files and VCS data never affect content
	}
	switch {
	case ev.Op.Has(fsnotify.Create):
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() && !skipEntryName(filepath.Base(ev.Name), true) {
			// Directories the packer skips (.git, node_modules, ...) are
			// not watched either. Best effort: racing with a removal
			// surfaces as a watch error, which is logged, not fatal.
			_ = watchTree(w.fsWatcher, ev.Name)
		}
		w.markDirty()
	case ev.Op.Has(fsnotify.Remove) || ev.Op.Has(fsnotify.Rename):
		// Renames arrive as Rename(old) + Create(new); only names inside
		// the watched tree affect the published content.
		if ev.Name == w.root || strings.HasPrefix(ev.Name, w.root+string(filepath.Separator)) {
			w.markDirty()
		}
	default:
		w.markDirty()
	}
}

func (w *watcher) markDirty() {
	w.dirty = true
	w.quietAt = time.Now().Add(w.opts.Debounce)
}

// ignored reports whether events for this name never affect published
// content: VCS data and editor temp/swap files. It matches the skip rules
// the packer applies, so watching and packing agree on what counts.
func (w *watcher) ignored(name string) bool {
	base := filepath.Base(name)
	if base == ".git" || base == ".svn" || base == ".hg" ||
		strings.HasPrefix(base, ".#") || // emacs lock files
		strings.HasPrefix(base, "#") || // emacs autosave
		strings.HasSuffix(base, "~") || // backup files
		strings.HasSuffix(base, ".swp") || strings.HasSuffix(base, ".swx") || // vim
		base == "4913" { // vim's directory-mtime probe
		return true
	}
	return false
}

func (w *watcher) logf(format string, args ...any) {
	if w.opts.Log != nil {
		w.opts.Log(format, args...)
	}
}

// publishIfChanged hashes the tree and publishes a version when the hash
// differs from the last published one. Unchanged content costs one hash
// pass, never an upload.
func (w *watcher) publishIfChanged() error {
	sum, err := HashTree(w.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The root itself vanished; keep watching for a recreation.
			return nil
		}
		return err
	}
	if sum == w.published {
		return nil
	}
	a, err := w.publishWithRetry()
	if err != nil {
		return err
	}
	w.published = sum
	if w.opts.OnPublish != nil {
		w.opts.OnPublish(WatchResult{Artifact: a, Version: a.Version, At: time.Now()})
	}
	return nil
}

// publishWithRetry calls Update until it succeeds, the classifier says the
// error is fatal, or ctx is done (context.Canceled).
func (w *watcher) publishWithRetry() (*api.Artifact, error) {
	src := &Source{
		Path:  w.root,
		Kind:  w.opts.Kind,
		Entry: w.opts.Entry,
		SPA:   w.opts.SPA,
		Note:  w.opts.Note,
	}
	for {
		a, err := w.up.Update(w.ctx, w.artifactID, src, w.opts.Progress)
		if err == nil {
			return a, nil
		}
		if w.ctxDone() {
			return nil, context.Canceled
		}
		d := classifyPublishError(err)
		if !d.retry {
			return nil, err
		}
		w.logf("publish failed (%v); retrying", err)
		timer := time.NewTimer(w.opts.RetryBackoff)
		select {
		case <-w.quit:
			timer.Stop()
			return nil, context.Canceled
		case <-timer.C:
		}
	}
}

func (w *watcher) ctxDone() bool {
	select {
	case <-w.quit:
		return true
	default:
		return false
	}
}

// watchTree adds dir and every existing subdirectory to w.
func watchTree(w *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Add(p)
		}
		return nil
	})
}

// HashTree returns a deterministic digest of the content under root: the
// same files Prepare would pack, each path with its SHA-256, in sorted
// order. Identical content hashes identically regardless of mtimes or
// directory traversal order, so unchanged trees never publish.
func HashTree(root string) (string, error) {
	st, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return hashFile(root)
	}
	names, err := walkNames(root)
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, rel := range names {
		sum, err := hashFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %s\n", rel, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// walkNames lists the regular files under dir that Prepare packs, using the
// same skipEntry rules, as slash-separated paths relative to dir.
func walkNames(dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skipEntry(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && d.Type().IsRegular() {
			names = append(names, rel)
		}
		return nil
	})
	return names, err
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
