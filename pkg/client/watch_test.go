package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mrRedSun/redrop/pkg/api"
)

// fakeUploader records Update calls and can be programmed with failures.
type fakeUploader struct {
	mu        sync.Mutex
	updates   []string // relative paths recorded at each Update call
	packs     [][]string
	failures  []error // errors to return before successes, popped in order
	inFlight  int     // current concurrent Update calls
	maxInFlt  int     // high-water mark of concurrency
	block     chan struct{}
	artifacts []*api.Artifact
}

// UpdateDigest records the packable files under the source path (so tests
// can assert exactly what would be uploaded). Like the real client, it
// snapshots the tree at call start (Prepare) — a mid-upload edit stays out
// of the snapshot and therefore out of the returned digest, which is what
// makes the trailing publish observable.
func (f *fakeUploader) UpdateDigest(ctx context.Context, artifactID string, src *Source, progress Progress) (string, *api.Artifact, error) {
	var names []string
	if fi, err := os.Stat(src.Path); err == nil && !fi.IsDir() {
		// A single-file source: the "pack" is the file itself.
		if !skipEntryName(filepath.Base(src.Path), false) {
			names = append(names, filepath.Base(src.Path))
		}
	} else {
		_ = walkPackable(src.Path, func(rel, abs string, info fs.FileInfo) error {
			names = append(names, rel)
			return nil
		})
	}
	snapshot, _ := hashTree(src.Path)

	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlt {
		f.maxInFlt = f.inFlight
	}
	n := len(f.updates) + 1
	f.updates = append(f.updates, src.Path)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		// A real upload aborts when the context ends; so must this.
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
			return "", nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer func() {
		f.inFlight--
		f.mu.Unlock()
	}()
	f.packs = append(f.packs, names)
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return "", nil, err
	}
	a := &api.Artifact{ID: artifactID, Version: n}
	f.artifacts = append(f.artifacts, a)
	return snapshot, a, nil
}

func (f *fakeUploader) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updates)
}

// lastPack returns the file list of the most recent upload.
func (f *fakeUploader) lastPack() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.packs) == 0 {
		return nil
	}
	return f.packs[len(f.packs)-1]
}

// successes returns the number of completed (non-failed) updates.
func (f *fakeUploader) successes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.artifacts)
}

func (f *fakeUploader) concurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlt
}

// fakeClock drives debounce and backoff deterministically: After returns
// channels the test fires in order, and firing advances Now.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	fireAt time.Duration
	ch     chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{fireAt: d, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t.ch
}

// advance moves time forward by d, firing every pending timer whose
// deadline is within d, in deadline order.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	fired := make([]*fakeTimer, 0, len(c.timers))
	remaining := c.timers[:0]
	for _, t := range c.timers {
		if !c.now.Add(t.fireAt).After(target) {
			fired = append(fired, t)
		} else {
			t.fireAt -= d // still pending, closer now
			remaining = append(remaining, t)
		}
	}
	c.timers = remaining
	c.now = target
	c.mu.Unlock()
	for _, t := range fired {
		t.ch <- target
	}
}

// pendingCount returns the number of unresolved fake timers.
func (c *fakeClock) pendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// pending reports whether any timer is still unresolved.
func (c *fakeClock) pending() bool {
	return c.pendingCount() > 0
}

// staticRetrier returns a fixed delay for every attempt.
type staticRetrier struct{ d time.Duration }

func (r staticRetrier) Next(int) time.Duration { return r.d }

// watchOpts returns test options: a short real debounce and fast retries,
// overridden with the fake clock where determinism matters.
func watchOpts() WatchOptions {
	return WatchOptions{
		Debounce: 30 * time.Millisecond,
		Retrier:  staticRetrier{20 * time.Millisecond},
	}
}

// startWatch runs Watch with a ready channel and returns a stop function.
// The returned function cancels and waits; cancellation exits nil.
func startWatch(t *testing.T, up Uploader, root string, opts WatchOptions) (ready <-chan struct{}, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan struct{})
	opts.OnReady = func() { close(readyCh) }
	src := &Source{Path: root}
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, up, "art", src, opts) }()
	return readyCh, func() error {
		cancel()
		select {
		case err := <-done:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Watch did not return after cancel")
			return nil
		}
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeNew is write without t.Helper: usable inside goroutines.
func writeNew(dir, rel, content string) error {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// atomicSave emulates an editor's safe write: temp file, rename over the
// target. inotify drops a watch added directly on the old inode, which is
// exactly what the parent-directory watch must survive.
func atomicSave(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := p + ".tmp-save"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
}

// --- HashTree ---

func TestHashTreeStableAcrossMtimeAndOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "b.txt", "bee")
	write(t, dir, "a/c.txt", "sea")
	h1, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(dir, "b.txt"), past, past)
	h2, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash changed with mtime only: %s -> %s", h1, h2)
	}
	write(t, dir, "a/c.txt", "bee") // same bytes, different path
	h3, _ := hashTree(dir)
	if h3 == h1 {
		t.Fatal("path-level edit did not change the hash")
	}
}

func TestHashTreeMatchesPackRules(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "keep.txt", "x")
	write(t, dir, ".git/config", "git")
	write(t, dir, "node_modules/pkg/index.js", "js")
	write(t, dir, ".DS_Store", "junk")
	write(t, dir, ".index.html.swp", "swap")
	write(t, dir, "index.html~", "backup")
	write(t, dir, ".#index.html", "lock")
	write(t, dir, "#index.html#", "autosave")
	h1, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Everything skipped: touching them changes nothing.
	write(t, dir, ".git/config", "changed")
	write(t, dir, "node_modules/pkg/index.js", "changed2")
	write(t, dir, ".index.html.swp", "swap2")
	write(t, dir, "index.html~", "backup2")
	h2, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("skipped entries influenced the hash")
	}
	write(t, dir, "keep.txt", "y")
	h3, _ := hashTree(dir)
	if h3 == h1 {
		t.Fatal("content change did not change the hash")
	}
	// A single file root hashes as itself.
	f := filepath.Join(t.TempDir(), "one.html")
	os.WriteFile(f, []byte("<p>hi</p>"), 0o644)
	if _, err := hashTree(f); err != nil {
		t.Fatal(err)
	}
	// A skipped single file root digests as empty content.
	s := filepath.Join(t.TempDir(), "one.html~")
	os.WriteFile(s, []byte("backup"), 0o644)
	got, err := hashTree(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("skipped single-file root digested to %q, want empty", got)
	}
}

func TestHashTreeFollowsSymlinksLikePacker(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "page")
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "shared.css"), []byte("body{}"), 0o644)
	if err := os.Symlink(filepath.Join(outside, "shared.css"), filepath.Join(dir, "shared.css")); err != nil {
		t.Skip("symlinks unavailable")
	}
	h1, _ := hashTree(dir)
	// The packer follows the symlink (os.Stat), so changing the target
	// must change the digest.
	os.WriteFile(filepath.Join(outside, "shared.css"), []byte("body{color:red}"), 0o644)
	h2, _ := hashTree(dir)
	if h1 == h2 {
		t.Fatal("symlinked file content change not hashed; walk drifted from the packer")
	}
}

// --- debounce and coalescing ---

func TestWatchStartupDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	time.Sleep(200 * time.Millisecond)
	if got := up.calls(); got != 0 {
		t.Fatalf("startup published %d versions, want 0", got)
	}
}

func TestWatchDebounceCoalescesBursts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	// A burst of edits within the debounce window must publish once.
	for i := 0; i < 5; i++ {
		write(t, dir, "index.html", fmt.Sprintf("burst-%d", i))
	}
	waitFor(t, 2*time.Second, func() bool { return up.calls() >= 1 })
	time.Sleep(150 * time.Millisecond) // catch late extra publishes
	if got := up.calls(); got != 1 {
		t.Fatalf("burst published %d times, want 1", got)
	}
}

func TestWatchDebounceWithFakeClock(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	clk := newFakeClock()
	opts := watchOpts()
	opts.Clock = clk
	ready, stop := startWatch(t, up, dir, opts)
	defer stop()
	<-ready

	write(t, dir, "index.html", "v1")
	// Let the watcher consume the event and arm its debounce timer, then
	// advance half the window: nothing may publish yet.
	waitFor(t, 2*time.Second, clk.pending)
	clk.advance(15 * time.Millisecond)
	if got := up.calls(); got != 0 {
		t.Fatalf("published before the debounce window closed: %d", got)
	}
	// A second edit restarts the window: the old timer is replaced by a
	// fresh one, so wait for the count to change before advancing.
	before := clk.pendingCount()
	write(t, dir, "index.html", "v2")
	waitFor(t, 2*time.Second, func() bool { return clk.pendingCount() != before })
	time.Sleep(50 * time.Millisecond) // let the restarted window settle
	clk.advance(15 * time.Millisecond)
	if got := up.calls(); got != 0 {
		t.Fatalf("restarted window published early: %d", got)
	}
	// The rest of the (restarted) window elapses: exactly one publish.
	clk.advance(20 * time.Millisecond)
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := up.calls(); got != 1 {
		t.Fatalf("coalesced edits published %d times, want 1", got)
	}
}

func TestWatchPublishesOnlyOnContentChange(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "hello")
	up := &fakeUploader{}
	var pubMu sync.Mutex
	var published []WatchResult
	opts := watchOpts()
	opts.OnPublish = func(r WatchResult) {
		pubMu.Lock()
		defer pubMu.Unlock()
		published = append(published, r)
	}
	ready, stop := startWatch(t, up, dir, opts)
	defer stop()
	<-ready

	// Rewrite the same bytes: no new version.
	write(t, dir, "page.html", "hello")
	time.Sleep(200 * time.Millisecond)
	if got := up.calls(); got != 0 {
		t.Fatalf("unchanged content published: %d calls", got)
	}
	// Real change: exactly one new version, reported via OnPublish.
	write(t, dir, "page.html", "goodbye")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	pubMu.Lock()
	defer pubMu.Unlock()
	if len(published) != 1 {
		t.Fatalf("OnPublish called %d times, want 1", len(published))
	}
	if published[len(published)-1].Version != 1 {
		t.Fatalf("published version %d, want 1", published[len(published)-1].Version)
	}
}

// B2: swap and backup files must never reach the upload.
func TestWatchSwapFilesNotUploaded(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	write(t, dir, ".index.html.swp", "swap-secret")
	write(t, dir, "index.html~", "backup")
	write(t, dir, ".#index.html", "lock")
	write(t, dir, "#index.html#", "autosave")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	write(t, dir, "index.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	pack := up.lastPack()
	if len(pack) != 1 || pack[0] != "index.html" {
		t.Fatalf("upload contains %v; want only [index.html] (swap/backup files leaked)", pack)
	}
	// Swap file churn (editing with vim open) must not publish either.
	write(t, dir, ".index.html.swp", "swap-churn")
	time.Sleep(200 * time.Millisecond)
	if got := up.calls(); got != 1 {
		t.Fatalf("swap file churn published: %d calls", got)
	}
}

// --- single flight and trailing publish ---

func TestWatchSingleFlightWithTrailingPublish(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	block := make(chan struct{})
	up := &fakeUploader{block: block}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	write(t, dir, "index.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return up.calls() >= 1 })

	// More edits while the upload is in flight...
	write(t, dir, "index.html", "v2")
	// ...then unblock for good (all future uploads run straight through).
	up.mu.Lock()
	up.block = nil
	up.mu.Unlock()
	close(block)

	// A trailing publish for the edits, then no more.
	waitFor(t, 3*time.Second, func() bool { return up.calls() >= 2 })
	time.Sleep(300 * time.Millisecond)
	if got := up.calls(); got != 2 {
		t.Fatalf("expected initial + one trailing publish, got %d calls", got)
	}
	if up.concurrency() > 1 {
		t.Fatalf("concurrent uploads observed: %d", up.concurrency())
	}
}

// --- error classification ---

func TestTransientClassification(t *testing.T) {
	cases := map[error]bool{
		&NetworkError{Err: errors.New("conn refused")}:  true,
		&Error{Status: 500, Title: "internal"}:          true,
		&Error{Status: 503, Title: "unavailable"}:       true,
		&Error{Status: 429, Title: "too many requests"}: true,
		&Error{Status: 401, Title: "unauthorized"}:      false,
		&Error{Status: 403, Title: "forbidden"}:         false,
		&Error{Status: 404, Title: "not found"}:         false,
		&Error{Status: 413, Title: "too large"}:         false,
		&Error{Status: 400, Title: "bad request"}:       false,
		errors.New("weird"):                             false,
	}
	for err, want := range cases {
		if got := transient(err); got != want {
			t.Errorf("transient(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestExponentialRetrier(t *testing.T) {
	r := ExponentialRetrier{Base: time.Second, Max: 60 * time.Second}
	prev := time.Duration(0)
	for attempt := 1; attempt <= 8; attempt++ {
		d := r.Next(attempt)
		if d < prev {
			t.Errorf("attempt %d delay %v below previous %v", attempt, d, prev)
		}
		if d > r.Max+r.Max/4 {
			t.Errorf("attempt %d delay %v exceeds max+jitter", attempt, d)
		}
		prev = d
	}
	if r.Next(100) > r.Max {
		t.Errorf("uncapped delay %v", r.Next(100))
	}
}

func TestWatchRetriesTransientKeepsWatching(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{failures: []error{
		&NetworkError{Err: errors.New("conn refused")},
		&Error{Status: 503, Title: "unavailable"},
		&Error{Status: 429, Title: "slow down"},
	}}
	clk := newFakeClock()
	var logMu sync.Mutex
	logged := 0
	opts := watchOpts()
	opts.Clock = clk
	opts.Log = func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logged++
	}
	ready, stop := startWatch(t, up, dir, opts)
	defer stop()
	<-ready

	write(t, dir, "index.html", "v1")
	// Drive the three retry delays deterministically: close the debounce
	// window, then after each failed attempt advance its retry timer.
	waitFor(t, 2*time.Second, clk.pending)
	clk.advance(50 * time.Millisecond)
	for attempt := 1; attempt <= 3; attempt++ {
		waitFor(t, 2*time.Second, func() bool { return up.calls() == attempt })
		waitFor(t, 2*time.Second, clk.pending) // the retry timer for this failure
		clk.advance(20 * time.Millisecond)
	}
	waitFor(t, 5*time.Second, func() bool { return up.successes() == 1 })
	logMu.Lock()
	n := logged
	logMu.Unlock()
	if n != 3 {
		t.Fatalf("expected 3 retry logs, got %d", n)
	}
	// Watching continues after the retries: the next edit's debounce
	// window is also on the fake clock.
	before := clk.pendingCount()
	write(t, dir, "index.html", "v2")
	waitFor(t, 2*time.Second, func() bool { return clk.pendingCount() != before })
	time.Sleep(50 * time.Millisecond) // let the restarted window settle
	clk.advance(50 * time.Millisecond)
	waitFor(t, 5*time.Second, func() bool { return up.successes() == 2 })
}

func TestWatchFatalErrorStops(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{failures: []error{&Error{Status: 401, Title: "unauthorized"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	opts := watchOpts()
	opts.OnReady = func() { close(ready) }
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, up, "art", &Source{Path: dir}, opts) }()
	<-ready

	write(t, dir, "index.html", "v1")
	select {
	case err := <-done:
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != 401 {
			t.Fatalf("want the 401 error back, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch kept running after a fatal error")
	}
}

// --- shutdown ---

func TestWatchCancelDuringUploadExitsCleanly(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	opts := watchOpts()
	opts.OnReady = func() { close(ready) }
	go func() { done <- Watch(ctx, up, "art", &Source{Path: dir}, opts) }()
	<-ready

	write(t, dir, "index.html", "v1") // trigger the blocked upload
	waitFor(t, 2*time.Second, func() bool { return up.calls() >= 1 })
	cancel()
	select {
	case err := <-done:
		// m4: a clean shutdown returns nil, not context.Canceled.
		if err != nil {
			t.Fatalf("want nil on cancel during upload, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not exit on cancel during upload")
	}
}

func TestWatchIdleCancelExitsZero(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	opts := watchOpts()
	opts.OnReady = func() { close(ready) }
	go func() { done <- Watch(ctx, up, "art", &Source{Path: dir}, opts) }()
	<-ready
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("idle cancel returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle Watch did not exit on cancel")
	}
}

// --- B1: rename-over saves, delete+recreate, root recreation ---

func TestWatchRenameOverSavesFileRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	// Two consecutive atomic saves must each publish.
	atomicSave(t, dir, "page.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	atomicSave(t, dir, "page.html", "v2")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 2 })
	// And an in-place write still works afterwards.
	write(t, dir, "page.html", "v3")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 3 })
}

func TestWatchRenameOverSavesInsideDirRoot(t *testing.T) {
	site := t.TempDir()
	write(t, site, "index.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, site, watchOpts())
	defer stop()
	<-ready

	atomicSave(t, site, "index.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	atomicSave(t, site, "deep/new.css", "p{}") // rename into a new subdir
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 2 })
}

func TestWatchSingleFileRootByPath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, filepath.Join(dir, "page.html"), watchOpts())
	defer stop()
	<-ready

	write(t, dir, "page.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	pack := up.lastPack()
	if len(pack) != 1 || pack[0] != filepath.Base("page.html") && pack[0] != "page.html" {
		t.Fatalf("single-file upload contains %v", pack)
	}
	// A sibling file must not be published (it is not part of the root).
	write(t, dir, "other.html", "x")
	time.Sleep(200 * time.Millisecond)
	if got := up.calls(); got != 1 {
		t.Fatalf("sibling of file root published: %d calls", got)
	}
}

func TestWatchDeleteAndRecreateFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	os.Remove(filepath.Join(dir, "page.html"))
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	write(t, dir, "page.html", "recreated")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 2 })
}

func TestWatchDirRootRemovedAndRecreated(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "site")
	write(t, root, "index.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, root, watchOpts())
	defer stop()
	<-ready

	write(t, root, "index.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	// Remove the whole root: nothing can be uploaded (the path is gone),
	// but the watch must survive it...
	os.RemoveAll(root)
	time.Sleep(300 * time.Millisecond)
	if got := up.calls(); got != 1 {
		t.Fatalf("removal of the root published %d versions, want 1 (nothing to upload)", got)
	}
	// ...and the recreated root publishes as a normal change.
	if err := writeNew(root, "index.html", "fresh"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return up.calls() == 2 })
}

// --- M1: unclean roots ---

func TestWatchRelativeAndTrailingSlashRoots(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "site")
	write(t, root, "index.html", "v0")
	write(t, root, "extra.css", "e")
	up := &fakeUploader{}
	// Deliberately unclean forms: trailing slash and ./ prefix.
	ready, stop := startWatch(t, up, root+string(filepath.Separator), watchOpts())
	defer stop()
	<-ready

	os.Remove(filepath.Join(root, "extra.css"))
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	if pack := up.lastPack(); len(pack) != 1 || pack[0] != "index.html" {
		t.Fatalf("after delete, upload contains %v", pack)
	}
}

// --- M2: skipped directories are not watched ---

func TestWatchSkipsVCSAndDependencyDirs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	for i := 0; i < 20; i++ {
		write(t, dir, fmt.Sprintf(".git/objects/%02d", i), "x")
		write(t, dir, fmt.Sprintf("node_modules/pkg%d/lib", i), "x")
	}
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	// Activity under a skipped directory must not publish anything.
	write(t, dir, ".git/objects/00", "churn")
	write(t, dir, "node_modules/pkg0/lib", "churn")
	time.Sleep(300 * time.Millisecond)
	if got := up.calls(); got != 0 {
		t.Fatalf("VCS/dependency churn published: %d calls", got)
	}
	// A real file still publishes exactly once, without the churn.
	write(t, dir, "index.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return up.calls() == 1 })
	pack := up.lastPack()
	for _, name := range pack {
		if strings.HasPrefix(name, ".git/") || strings.HasPrefix(name, "node_modules/") {
			t.Fatalf("skipped directory content uploaded: %v", pack)
		}
	}
}

func TestWatchWatchListCount(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	for i := 0; i < 20; i++ {
		write(t, dir, fmt.Sprintf(".git/objects/%02d", i), "x")
		write(t, dir, fmt.Sprintf("node_modules/pkg%d/lib", i), "x")
	}
	// watchTree is what registerAll uses; count its registrations
	// directly, because the watcher's WatchList is not reachable from here.
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	if err := watchTree(fw, dir); err != nil {
		t.Fatal(err)
	}
	// Only the root itself: none of the 42 skipped subdirectories.
	if got := len(fw.WatchList()); got != 1 || fw.WatchList()[0] != dir {
		t.Fatalf("watchTree registered %v; want only [%s]", fw.WatchList(), dir)
	}
}

func TestWatchOverflowReWalks(t *testing.T) {
	// An fsnotify overflow error must mark the tree dirty and re-register
	// the watches rather than being swallowed.
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	opts := watchOpts()
	opts.Clock = realClock{} // hand-built watcher skips Watch's defaults
	opts.OnReady = func() { close(ready) }
	done := make(chan error, 1)
	// Inject an errors channel we control: build the watcher by hand.
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	if err := watchTree(fw, dir); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	w := &watcher{
		up: up, artifactID: "art", root: dir,
		src:  &Source{Path: dir},
		opts: opts, ctx: ctx, fw: fw,
		events: fw.Events, errors: errs,
		quit: ctx.Done(),
	}
	go func() { done <- w.run() }()
	<-ready

	errs <- fsnotify.ErrEventOverflow
	// The overflow handler re-walks the watches (root + parent) and marks
	// the tree dirty; with unchanged content nothing publishes (the
	// content-hash rule), but the next edit must still be seen.
	waitFor(t, 2*time.Second, func() bool { return len(fw.WatchList()) == 2 })
	time.Sleep(200 * time.Millisecond) // debounce for the overflow mark
	if got := up.calls(); got != 0 {
		t.Fatalf("overflow with unchanged content published %d versions", got)
	}
	write(t, dir, "index.html", "v1")
	waitFor(t, 3*time.Second, func() bool { return up.calls() == 1 })
	cancel()
	<-done
}

func TestWatchNewSubdirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	ready, stop := startWatch(t, up, dir, watchOpts())
	defer stop()
	<-ready

	write(t, dir, "deep/nested/asset.css", "p{}")
	waitFor(t, 3*time.Second, func() bool { return up.calls() == 1 })
	// And a later change inside it too (the directory was added to the watch).
	write(t, dir, "deep/nested/asset.css", "q{}")
	waitFor(t, 3*time.Second, func() bool { return up.calls() == 2 })
}
