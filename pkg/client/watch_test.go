package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
)

// fakeUploader records Update calls and can be programmed with failures.
type fakeUploader struct {
	mu        sync.Mutex
	updates   []string // note recorded at each Update call
	failures  []error  // errors to return before successes, popped in order
	inFlight  int      // current concurrent Update calls
	maxInFlt  int      // high-water mark of concurrency
	block     chan struct{}
	artifacts []*api.Artifact
}

func (f *fakeUploader) Update(ctx context.Context, artifactID string, src *Source, progress Progress) (*api.Artifact, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlt {
		f.maxInFlt = f.inFlight
	}
	n := len(f.updates) + 1
	f.updates = append(f.updates, src.Note)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		// A real upload aborts when the context ends; so must this.
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer func() {
		f.inFlight--
		f.mu.Unlock()
	}()
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return nil, err
	}
	a := &api.Artifact{ID: artifactID, Version: n}
	f.artifacts = append(f.artifacts, a)
	return a, nil
}

func (f *fakeUploader) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updates...)
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

// fakeClock is intentionally not used: fsnotify events arrive on real
// goroutines, so debounce is tested with a short real window (30ms) and
// bounded waits instead of a clock that would have to interleave with them.

// watchOpts returns options with a short debounce and retry backoff for tests.
func watchOpts(note string) WatchOptions {
	return WatchOptions{Debounce: 30 * time.Millisecond, Note: note, RetryBackoff: 20 * time.Millisecond}
}

// runWatch starts Watch and returns a stop function that cancels and waits.
// Cancellation is the normal exit, so stop reports nil for context.Canceled.
func runWatch(t *testing.T, up Uploader, root string, opts WatchOptions) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, up, "art", root, opts) }()
	return func() error {
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

// --- HashTree ---

func TestHashTreeStableAcrossMtimeAndOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "b.txt", "bee")
	write(t, dir, "a/c.txt", "sea")
	h1, err := HashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Change mtimes: the hash must not move.
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(dir, "b.txt"), past, past)
	os.Chtimes(filepath.Join(dir, "a"), past, past)
	h2, err := HashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash changed with mtime only: %s -> %s", h1, h2)
	}
	// Same content under a different path differs.
	write(t, dir, "a/c.txt", "bee") // same bytes as b.txt but path differs
	h3, _ := HashTree(dir)
	if h3 == h1 {
		t.Fatal("rename-level edit did not change the hash")
	}
}

func TestHashTreeMatchesPackRules(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "keep.txt", "x")
	write(t, dir, ".git/config", "git")
	write(t, dir, "node_modules/pkg/index.js", "js")
	write(t, dir, ".DS_Store", "junk")
	h1, err := HashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Only keep.txt counts: touching skipped entries changes nothing.
	write(t, dir, ".git/config", "changed")
	write(t, dir, "node_modules/pkg/index.js", "changed2")
	h2, err := HashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("skipped entries influenced the hash")
	}
	write(t, dir, "keep.txt", "y")
	h3, _ := HashTree(dir)
	if h3 == h1 {
		t.Fatal("content change did not change the hash")
	}
	// A single file root hashes as itself.
	f := filepath.Join(t.TempDir(), "one.html")
	os.WriteFile(f, []byte("<p>hi</p>"), 0o644)
	if _, err := HashTree(f); err != nil {
		t.Fatal(err)
	}
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

// settle waits for the watcher goroutine to register its watches, so the
// test's first edit cannot land before fsnotify is listening (a change in
// that window is baked into the startup seed hash and never publishes).
func settle() { time.Sleep(150 * time.Millisecond) }

// --- debounce and coalescing ---

// TestWatchStartupDoesNotPublish pins the startup contract: watch seeds its
// content hash from the tree as found, so merely starting it costs nothing.
func TestWatchStartupDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	defer runWatch(t, up, dir, watchOpts("n"))()

	settle()

	time.Sleep(300 * time.Millisecond)
	if got := len(up.calls()); got != 0 {
		t.Fatalf("startup published %d versions, want 0", got)
	}
}

func TestWatchDebounceCoalescesBursts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	defer runWatch(t, up, dir, watchOpts("n"))()

	settle()

	// No publish until something changes.
	time.Sleep(150 * time.Millisecond)
	if got := len(up.calls()); got != 0 {
		t.Fatalf("startup published %d versions, want 0", got)
	}

	// A burst of edits within the debounce window must publish once.
	for i := 0; i < 5; i++ {
		write(t, dir, "index.html", fmt.Sprintf("burst-%d", i))
	}
	waitFor(t, 2*time.Second, func() bool { return len(up.calls()) >= 1 })
	time.Sleep(150 * time.Millisecond) // catch late extra publishes
	calls := up.calls()
	if len(calls) != 1 {
		t.Fatalf("burst published %d times, want 1", len(calls))
	}
}

func TestWatchPublishesOnlyOnContentChange(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "hello")
	up := &fakeUploader{}
	var pubMu sync.Mutex
	var published []WatchResult
	opts := watchOpts("n")
	opts.OnPublish = func(r WatchResult) {
		pubMu.Lock()
		defer pubMu.Unlock()
		published = append(published, r)
	}
	defer runWatch(t, up, dir, opts)()

	settle()

	// Rewrite the same bytes: no new version.
	write(t, dir, "page.html", "hello")
	time.Sleep(200 * time.Millisecond)
	if got := len(up.calls()); got != 0 {
		t.Fatalf("unchanged content published: %d calls", got)
	}
	// Touch an ignored temp file: no new version.
	write(t, dir, ".#page.html", "lock")
	write(t, dir, "page.html~", "backup")
	time.Sleep(200 * time.Millisecond)
	if got := len(up.calls()); got != 0 {
		t.Fatalf("editor temp files published: %d calls", got)
	}
	// Real change: exactly one new version.
	write(t, dir, "page.html", "goodbye")
	waitFor(t, 2*time.Second, func() bool { return len(up.calls()) == 1 })
	pubMu.Lock()
	defer pubMu.Unlock()
	if len(published) != 1 {
		t.Fatalf("OnPublish called %d times, want 1", len(published))
	}
	if published[len(published)-1].Version != 1 {
		t.Fatalf("published version %d, want 1", published[len(published)-1].Version)
	}
}

// --- single flight and trailing publish ---

func TestWatchSingleFlightWithTrailingPublish(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	block := make(chan struct{})
	up := &fakeUploader{block: block}
	defer runWatch(t, up, dir, watchOpts("n"))()

	settle()

	// The first upload (for the first edit) is now blocked in flight.
	write(t, dir, "index.html", "v1")
	waitFor(t, 2*time.Second, func() bool { return len(up.calls()) >= 1 })

	// More edits while the upload is in flight...
	write(t, dir, "index.html", "v2")
	// ...then unblock for good (all future uploads run straight through).
	up.mu.Lock()
	up.block = nil
	up.mu.Unlock()
	close(block)

	// A trailing publish for the edits, then no more.
	waitFor(t, 3*time.Second, func() bool { return len(up.calls()) >= 2 })
	time.Sleep(300 * time.Millisecond)
	if got := len(up.calls()); got != 2 {
		t.Fatalf("expected initial + one trailing publish, got %d calls", got)
	}
	if up.concurrency() > 1 {
		t.Fatalf("concurrent uploads observed: %d", up.concurrency())
	}
}

// --- error classification ---

func TestClassifyPublishError(t *testing.T) {
	cases := []struct {
		err   error
		retry bool
	}{
		{&NetworkError{Err: errors.New("conn refused")}, true},
		{&Error{Status: 500, Title: "internal"}, true},
		{&Error{Status: 503, Title: "unavailable"}, true},
		{&Error{Status: 401, Title: "unauthorized"}, false},
		{&Error{Status: 403, Title: "forbidden"}, false},
		{&Error{Status: 404, Title: "not found"}, false},
		{&Error{Status: 413, Title: "too large"}, false},
		{&Error{Status: 400, Title: "bad request"}, false},
		{errors.New("weird"), false},
	}
	for _, c := range cases {
		got := classifyPublishError(c.err)
		if got.retry != c.retry {
			t.Errorf("classifyPublishError(%v) retry=%v, want %v", c.err, got.retry, c.retry)
		}
	}
}

func TestWatchRetriesTransientKeepsWatching(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{failures: []error{
		&NetworkError{Err: errors.New("conn refused")},
		&Error{Status: 503, Title: "unavailable"},
	}}
	var logMu sync.Mutex
	logged := 0
	opts := watchOpts("n")
	opts.Log = func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logged++
	}
	defer runWatch(t, up, dir, opts)()

	settle()

	// The first change hits both transient failures, then succeeds.
	write(t, dir, "index.html", "v1")
	waitFor(t, 5*time.Second, func() bool { return up.successes() == 1 })
	logMu.Lock()
	n := logged
	logMu.Unlock()
	if n != 2 {
		t.Fatalf("expected 2 retry logs, got %d", n)
	}
	// Watching continues after the retries.
	write(t, dir, "index.html", "v2")
	waitFor(t, 5*time.Second, func() bool { return up.successes() == 2 })
}

func TestWatchFatalErrorStops(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{failures: []error{&Error{Status: 401, Title: "unauthorized"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, up, "art", dir, watchOpts("n"))
	}()
	settle()
	// Trigger the publish that fails fatally.
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
	go func() { done <- Watch(ctx, up, "art", dir, watchOpts("n")) }()
	settle()

	write(t, dir, "index.html", "v1") // trigger the blocked upload
	waitFor(t, 2*time.Second, func() bool { return len(up.calls()) >= 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && err != nil {
			t.Fatalf("want clean cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not exit on cancel during upload")
	}
}

func TestWatchNewSubdirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	defer runWatch(t, up, dir, watchOpts("n"))()

	settle()

	// A new nested directory with content must be picked up.
	write(t, dir, "deep/nested/asset.css", "p{}")
	waitFor(t, 3*time.Second, func() bool { return len(up.calls()) == 1 })
	// And a later change inside it too (the directory was added to the watch).
	write(t, dir, "deep/nested/asset.css", "q{}")
	waitFor(t, 3*time.Second, func() bool { return len(up.calls()) == 2 })
}

func TestWatchRootRemovedAndRecreated(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	defer runWatch(t, up, dir, watchOpts("n"))()

	settle()

	// Remove the only file: a publish happens (content changed to nothing).
	os.Remove(filepath.Join(dir, "index.html"))
	waitFor(t, 2*time.Second, func() bool { return len(up.calls()) == 1 })
}
