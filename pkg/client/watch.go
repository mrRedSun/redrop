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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mrRedSun/redrop/pkg/api"
)

// DefaultDebounce is how long the tree must stay quiet after a change
// before Watch publishes (overridable with the --debounce flag).
const DefaultDebounce = 500 * time.Millisecond

// Retry policy for transient publish failures. A Retry-After header longer
// than the backoff is honored up to MaxRetryAfter (10x RetryMax), so a
// misconfigured proxy cannot park the watcher for hours.
const (
	RetryBase     = time.Second
	RetryMax      = 60 * time.Second
	MaxRetryAfter = 10 * RetryMax
)

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
	// OnNotice receives non-fatal watcher and retry notices (watch
	// errors, retrying publishes, a vanished root, a tree that changed
	// mid-publish, an empty folder). Kind distinguishes them.
	OnNotice func(Notice)
	// Clock overrides timekeeping (tests). It drives both the debounce
	// window and the retry delays.
	Clock Clock
	// Retrier overrides the transient-failure retry policy (tests).
	Retrier Retrier
}

// NoticeKind labels the notices Watch reports through OnNotice.
type NoticeKind string

const (
	// NoticeRetry: a publish failed transiently and will be retried.
	NoticeRetry NoticeKind = "retry"
	// NoticeWatchError: a non-fatal filesystem watcher error.
	NoticeWatchError NoticeKind = "watch_error"
	// NoticeRootVanished: the watched root disappeared; waiting for it.
	NoticeRootVanished NoticeKind = "root_vanished"
	// NoticeRewatchFailed: re-registering watches (e.g. after overflow)
	// failed; watching continues with the old set.
	NoticeRewatchFailed NoticeKind = "rewatch_failed"
	// NoticeTreeChanged: files changed while they were being read for a
	// publish; it is retried once the tree settles again.
	NoticeTreeChanged NoticeKind = "tree_changed"
	// NoticeEmptyTree: the watched folder has no publishable files; the
	// publish is skipped until files appear.
	NoticeEmptyTree NoticeKind = "empty_folder"
)

// Notice is one non-fatal event during watching.
type Notice struct {
	Kind NoticeKind
	// Attempt is the upcoming attempt number, set on NoticeRetry.
	Attempt int
	// Delay is the wait before that attempt, set on NoticeRetry.
	Delay time.Duration
	Err   error
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
// with jitter applied after the cap (a random value in [0.9d, d]) so even
// many watchers at the cap do not retry in lockstep.
type ExponentialRetrier struct {
	Base, Max time.Duration
}

func (r ExponentialRetrier) Next(attempt int) time.Duration {
	base, limit := r.Base, r.Max
	if base <= 0 {
		base = time.Second
	}
	if limit <= 0 {
		limit = time.Minute
	}
	d := min(base, limit)
	for i := 1; i < attempt && d < limit; i++ {
		if d > limit/2 {
			d = limit
		} else {
			d *= 2
		}
	}
	// Apply jitter after capping, including during long outages. Very
	// small injected delays must not call Int63n(0).
	if spread := d / 10; spread > 0 {
		return d - time.Duration(rand.Int63n(int64(spread)))
	}
	return d
}

// WatchResult reports one published version.
type WatchResult struct {
	Artifact *api.Artifact
	Version  int
	At       time.Time
}

// Uploader publishes a new version and reports its source-content digest:
// sorted path/file hashes for directory roots, or the raw source hash for
// explicit files. It must match hashTree's domain, not the upload blob's
// checksum. *Client implements this contract through UpdateDigest.
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
	case s >= 500 || s == http.StatusTooManyRequests:
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
		opts.Retrier = ExponentialRetrier{Base: RetryBase, Max: RetryMax}
	}
	// Resolve a symlinked root once and watch its real path: fsnotify and
	// the walks then see the folder itself, and every publish reads the
	// tree being watched even if the link is retargeted later. The
	// resolved path is also clean, which parent/child comparisons and
	// event filtering need for "./site", "site/", and similar spellings.
	// It is absolute, so every path error from a publish outside the
	// root-confined walk names an absolute path (see publishError).
	if _, err := os.Stat(src.Path); err != nil {
		return filePathError(src.Path, err, os.Lstat)
	}
	root, err := filepath.EvalSymlinks(src.Path)
	if err != nil {
		return err
	}
	if root, err = filepath.Abs(root); err != nil {
		return err
	}
	if root != filepath.Clean(src.Path) {
		resolved := *src
		resolved.Path = root
		if resolved.Name == "" {
			abs, err := filepath.Abs(src.Path)
			if err != nil {
				return err
			}
			resolved.Name = filepath.Base(abs) // publish under the name given
		}
		src = &resolved
	}

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
	root       string // src.Path, cleaned and with symlinks resolved
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
	// changeRetries counts tree_changed retries since the last fs event
	// or publish, so a misclassified persistent error cannot loop.
	changeRetries int
	// dirs holds every directory watchTree registered since the last
	// registerAll. Unlike fsnotify's WatchList, a removed directory stays
	// in it, so the event for its removal is still recognised.
	dirs map[string]bool
}

// maxChangeRetries bounds consecutive tree_changed retries without a
// filesystem event in between; the next one is returned as fatal.
const maxChangeRetries = 3

// run is the only writer of the watcher state, so the invariants (at most
// one upload at a time, exactly one trailing publish) hold by construction:
// while publishIfChanged runs, no event is read.
func (w *watcher) run() error {
	// Seed the published digest with the tree as found: starting watch
	// never uploads by itself. The server has no content-digest API to
	// compare against, so a tree that already differs from the deployed
	// version syncs on its next change (or via an explicit "redrop update").
	if err := w.seed(); err != nil {
		return w.mapShutdown(err)
	}
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

// seed records the digest of the tree as found. A build that removes a
// file mid-scan is waited out like a publish would be: up to
// maxChangeRetries debounce intervals.
func (w *watcher) seed() error {
	for {
		sum, _, err := scanTree(w.root)
		if err == nil {
			w.published = sum
			w.changeRetries = 0
			return nil
		}
		if err = w.changeRetry(w.scanError(err)); err != nil {
			return err
		}
		w.dirty = false // the seed retries itself; nothing is pending
		if !w.awaitRetry(w.opts.Debounce) {
			return context.Canceled
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
	for {
		if w.ctx.Err() != nil {
			return context.Canceled
		}
		remaining := w.quietAt.Sub(w.opts.Clock.Now())
		if remaining <= 0 {
			break
		}
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
		w.notify(Notice{Kind: NoticeWatchError, Err: err})
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
	w.changeRetries = 0
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
				w.notify(Notice{Kind: NoticeRewatchFailed, Err: err})
			}
		case filepath.Dir(w.root):
			// The parent changed; nothing to do beyond the dirty mark.
		default:
			if !w.ignored(name) {
				// Directories the packer skips (.git, node_modules, ...)
				// are not watched either. Best effort: racing with a
				// removal surfaces as a watch error, which is logged.
				if rel, err := filepath.Rel(w.root, name); err == nil {
					_ = w.watchTree(filepath.ToSlash(rel))
				}
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
// and any name the bundle walk skips, itself or through a parent below the
// root (so .git/objects/... and node_modules/... events are ignored too).
// Parents are directories. When the file and directory rules disagree on
// the last component (a directory named "drafts~"), its type decides, as
// in the walk; a name that is gone is not ignored, since it may have been
// a directory with packed content.
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
	for _, part := range parts[:len(parts)-1] {
		if skipEntryName(part, true) {
			return true
		}
	}
	last := parts[len(parts)-1]
	if skipEntryName(last, false) == skipEntryName(last, true) {
		skip, _ := skipEntry(last, 0) // the type cannot change the answer
		return skip
	}
	var mode fs.FileMode
	if info, err := os.Lstat(name); err == nil {
		mode = info.Mode()
	} else if w.dirs[name] {
		mode = fs.ModeDir // a removed directory that held packed content
	}
	skip, _ := skipEntry(last, mode)
	return skip
}

func (w *watcher) notify(n Notice) {
	if w.opts.OnNotice != nil {
		w.opts.OnNotice(n)
	}
}

// publishIfChanged publishes a version when the tree differs from the
// last upload. UpdateDigest records the tree digest of the bytes actually
// packed, using the same digest domain as hashTree.
func (w *watcher) publishIfChanged() error {
	sum, files, err := scanTree(w.root)
	if err != nil {
		return w.sourceError(w.scanError(err))
	}
	if files == 0 {
		// A build tool cleaned the folder. Keep the last version; the
		// rebuilt content publishes as a normal change.
		w.notify(Notice{Kind: NoticeEmptyTree, Err: fmt.Errorf("%s: %w", w.root, ErrEmptyDirectory)})
		return nil
	}
	if sum == w.published {
		return nil
	}
	return w.publishWithRetry()
}

// publishError classifies a not-exist error from a publish by its path,
// not by a later re-check (the root may be back by then, as with
// "rm -rf dist && mkdir dist"). A path at or inside the root means the
// tree moved under Prepare or the upload: a vanished root if it is still
// gone, otherwise a tree change, which maxChangeRetries bounds. A path
// outside the root, such as a missing TMPDIR, stays fatal. A not-exist
// error the packer already classified as a source change is inside the
// root; it is a vanished root too if the root is still gone (a single-file
// root deleted between Prepare's stat and its open).
func (w *watcher) publishError(err error) error {
	if errors.Is(err, fs.ErrNotExist) && errors.Is(err, ErrSourceChanged) {
		if w.rootGone() {
			return fmt.Errorf("%w: %w", errRootVanished, err)
		}
		return err
	}
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pe) || !w.inRoot(pe.Path) {
		return err
	}
	if w.rootGone() {
		return fmt.Errorf("%w: %w", errRootVanished, err)
	}
	return sourceChanged(err)
}

// inRoot reports whether p is the watch root or a path inside it. A
// relative p comes from the walk confined to the (absolute) root, so it is
// relative to the root.
func (w *watcher) inRoot(p string) bool {
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.root, p)
	}
	rel, err := filepath.Rel(w.root, filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// errRootVanished marks a scan or publish that failed because the root
// itself is gone.
var errRootVanished = errors.New("watched path disappeared")

func (w *watcher) rootGone() bool {
	_, err := os.Stat(w.root)
	return errors.Is(err, fs.ErrNotExist)
}

// scanError classifies a not-exist error from scanning the tree, which
// only reads the tree: if the root is gone, it vanished; if it is still
// there, a file inside vanished between the walk and its read.
func (w *watcher) scanError(err error) error {
	switch {
	case !errors.Is(err, fs.ErrNotExist):
		return err
	case w.rootGone():
		return fmt.Errorf("%w: %w", errRootVanished, err)
	}
	return sourceChanged(err)
}

// sourceError keeps watching through local races with a tree that is
// being rewritten. Only errors that wrap ErrSourceChanged, which the
// packer and uploader produce for a file that vanished, shrank, or was
// edited while read, count as a tree change: the tree is marked dirty and
// tried again once it settles, at most maxChangeRetries times in a row
// without a filesystem event. A vanished root waits for its recreation,
// and an empty folder is skipped. Any other error is fatal.
func (w *watcher) sourceError(err error) error {
	switch {
	case errors.Is(err, errRootVanished):
		// There is nothing to publish: the upload path needs the path on
		// disk. The parent watch sees the root's recreation, registerAll
		// rebuilds the watches, and the recreated content publishes as a
		// normal change (an identical recreation publishes nothing, which
		// is the content-hash rule working as specified).
		w.notify(Notice{Kind: NoticeRootVanished, Err: err})
		return nil
	case errors.Is(err, ErrEmptyDirectory):
		w.notify(Notice{Kind: NoticeEmptyTree, Err: err})
		return nil
	}
	return w.changeRetry(err)
}

// changeRetry reports a tree change and marks the tree dirty, or returns
// err when it is not a tree change or the retry budget is spent.
func (w *watcher) changeRetry(err error) error {
	if !errors.Is(err, ErrSourceChanged) || w.changeRetries >= maxChangeRetries {
		return err
	}
	w.changeRetries++
	w.notify(Notice{Kind: NoticeTreeChanged, Err: err})
	w.markDirty(w.root)
	return nil
}

// publishWithRetry calls UpdateDigest until it succeeds, the error is
// fatal, the source changes under it, or ctx is done.
func (w *watcher) publishWithRetry() error {
	for attempt := 1; ; attempt++ {
		sum, a, err := w.up.UpdateDigest(w.ctx, w.artifactID, w.src, w.opts.Progress)
		if err == nil {
			w.published = sum
			w.changeRetries = 0
			if w.opts.OnPublish != nil {
				w.opts.OnPublish(WatchResult{Artifact: a, Version: a.Version, At: w.opts.Clock.Now()})
			}
			return nil
		}
		if w.ctx.Err() != nil {
			return context.Canceled // clean shutdown, not the upload error
		}
		if !transient(err) {
			err = w.publishError(err)
			return w.sourceError(err)
		}
		delay := w.opts.Retrier.Next(attempt)
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests {
			delay = max(delay, min(apiErr.RetryAfter, MaxRetryAfter))
		}
		w.notify(Notice{Kind: NoticeRetry, Attempt: attempt + 1, Delay: delay, Err: err})
		if !w.awaitRetry(delay) {
			return context.Canceled
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
// A directory renamed away can retain watches on the old inode until
// shutdown. They may trigger harmless rehashes under the old path;
// removing these stale registrations is left for a separate change.
func (w *watcher) registerAll() error {
	if fi, err := os.Stat(w.root); err == nil && !fi.IsDir() {
		return w.fw.Add(filepath.Dir(w.root))
	}
	if parent := filepath.Dir(w.root); parent != w.root {
		if err := w.fw.Add(parent); err != nil {
			return err
		}
	}
	w.dirs = nil
	return w.watchTree(".")
}

// watchTree adds the directory start (relative to the root) and every
// directory below it that the bundle walk would pack to the watcher.
func (w *watcher) watchTree(start string) error {
	root, err := os.OpenRoot(w.root)
	if err != nil {
		return filePathError(w.root, err, os.Lstat)
	}
	defer root.Close()
	walk := &bundleWalk{fsys: root.FS(), dir: func(name string) error {
		p := filepath.Join(w.root, filepath.FromSlash(name))
		if w.dirs == nil {
			w.dirs = map[string]bool{}
		}
		w.dirs[p] = true
		return w.fw.Add(p)
	}}
	return walk.run(start)
}

// hashTree returns a deterministic digest of the content under root: the
// same files the packer would pack (one shared walk and skip predicate),
// each path with the followed file's SHA-256, in sorted order. Identical
// content hashes identically regardless of mtimes or traversal order, so
// unchanged trees never publish. The skip rule applies to entries inside
// the tree; an explicitly named single-file root is always hashed (the
// same rule `update` applies when publishing it).
func hashTree(root string) (string, error) {
	sum, _, err := scanTree(root)
	return sum, err
}

// scanTree is hashTree plus the number of files hashed (1 for a file root).
func scanTree(root string) (string, int, error) {
	st, err := os.Stat(root)
	if err != nil {
		return "", 0, err
	}
	if !st.IsDir() {
		sum, err := hashFile(root)
		return sum, 1, err
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", 0, filePathError(root, err, os.Lstat)
	}
	defer dir.Close()
	db := &treeDigestBuilder{}
	walk := &bundleWalk{fsys: dir.FS(), file: func(name string, content io.Reader, size int64) error {
		sum, err := copyHashed(io.Discard, content, size)
		if err == nil {
			db.add(name, sum)
		}
		return err
	}}
	if err := walk.run("."); err != nil {
		return "", 0, err
	}
	return db.digest(), len(db.entries), nil
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
