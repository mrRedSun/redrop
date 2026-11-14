package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mrRedSun/redrop/pkg/api"
)

// The fake uses the real preparation path. In particular its return value
// is Prepared.Digest, not an independently hashed live tree or a blob hash.
type fakeUploader struct {
	// duringPack runs inside Prepare's walk, between each file's stat and
	// read (see walkTestHook); hashTree's walks are not affected.
	duringPack            func(rel string)
	beforePrepare         func() // runs once, before the next Prepare
	mu                    sync.Mutex
	updates               int
	packs                 [][]string
	failures              []error
	block                 <-chan struct{}
	inFlight, maxInFlight int
}

func (f *fakeUploader) UpdateDigest(ctx context.Context, id string, src *Source, _ Progress) (string, *api.Artifact, error) {
	f.mu.Lock()
	walkTestHook = f.duringPack
	before := f.beforePrepare
	f.beforePrepare = nil
	f.mu.Unlock()
	if before != nil {
		before()
	}
	p, err := Prepare(src)
	walkTestHook = nil
	if err != nil {
		return "", nil, err
	}
	defer p.Close()
	names := []string{p.Filename}
	if p.Kind == api.KindBundle {
		files, err := readPack(p.File)
		if err != nil {
			return "", nil, err
		}
		names = sortedKeys(files)
	}
	f.mu.Lock()
	f.updates++
	number := f.updates
	f.packs = append(f.packs, names)
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	block := f.block
	var failure error
	if len(f.failures) > 0 {
		failure, f.failures = f.failures[0], f.failures[1:]
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inFlight--; f.mu.Unlock() }()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	if failure != nil {
		return "", nil, failure
	}
	digest := p.Digest
	if digest == "" { // File is the source itself; the client uses the upload checksum
		if digest, err = hashFile(p.File); err != nil {
			return "", nil, err
		}
	}
	return digest, &api.Artifact{ID: id, Version: number}, nil
}
func (f *fakeUploader) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.updates }
func (f *fakeUploader) lastPack() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.packs[len(f.packs)-1]...)
}

// All debounce/backoff and negative assertions run against this clock.
// Real time is used only as a bounded failure timeout, never to establish
// that an unchanged/ignored event does not publish.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
	armed  chan time.Duration
}
type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC), armed: make(chan time.Duration, 128)}
}
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if d <= 0 {
		ch <- c.now
	} else {
		c.timers = append(c.timers, fakeTimer{at, ch})
	}
	c.mu.Unlock()
	c.armed <- d
	return ch
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	pending := c.timers[:0]
	for _, timer := range c.timers {
		if timer.at.After(c.now) {
			pending = append(pending, timer)
		} else {
			timer.ch <- c.now
		}
	}
	c.timers = pending
}

type staticRetrier struct{ d time.Duration }

func (r staticRetrier) Next(int) time.Duration { return r.d }

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watcher")
		var zero T
		return zero
	}
}
func write(t *testing.T, dir, rel, contents string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
func atomicSave(t *testing.T, dir, rel, contents string) {
	t.Helper()
	write(t, dir, rel+".tmp-save", contents)
	if err := os.Rename(filepath.Join(dir, rel+".tmp-save"), filepath.Join(dir, rel)); err != nil {
		t.Fatal(err)
	}
}

// An unbuffered event channel plus acknowledged error notices provides an
// event-loop barrier. No sleeps are needed to wait for event consumption.
type watchHarness struct {
	t         *testing.T
	w         *watcher
	clk       *fakeClock
	events    chan fsnotify.Event
	errs      chan error
	ack       chan struct{}
	barrier   error
	published chan WatchResult
	notices   chan Notice
	cancel    context.CancelFunc
	done      chan error
}

func newHarness(t *testing.T, up Uploader, root string) *watchHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	h := &watchHarness{t: t, clk: newFakeClock(), events: make(chan fsnotify.Event), errs: make(chan error), ack: make(chan struct{}), barrier: errors.New("test barrier"), published: make(chan WatchResult, 16), notices: make(chan Notice, 16), cancel: cancel, done: make(chan error, 1)}
	ready := make(chan struct{})
	h.w = &watcher{up: up, artifactID: "art", root: filepath.Clean(root), src: &Source{Path: root}, ctx: ctx, quit: ctx.Done(), fw: fw, events: h.events, errors: h.errs, opts: WatchOptions{Debounce: time.Second, Clock: h.clk, Retrier: staticRetrier{time.Second}, OnReady: func() { close(ready) }, OnPublish: func(r WatchResult) { h.published <- r }, OnNotice: func(n Notice) {
		if n.Err == h.barrier {
			h.ack <- struct{}{}
		} else {
			h.notices <- n
		}
	}}}
	go func() { h.done <- h.w.run() }()
	receive(t, ready)
	t.Cleanup(func() {
		cancel()
		if err := receive(t, h.done); err != nil {
			t.Errorf("watcher exit: %v", err)
		}
		fw.Close()
	})
	return h
}
func (h *watchHarness) sync() {
	h.t.Helper()
	select {
	case h.errs <- h.barrier:
	case <-time.After(5 * time.Second):
		h.t.Fatal("watcher failed to reach barrier")
	}
	receive(h.t, h.ack)
}
func (h *watchHarness) event(name string, op fsnotify.Op) {
	h.t.Helper()
	select {
	case h.events <- fsnotify.Event{Name: name, Op: op}:
	case <-time.After(5 * time.Second):
		h.t.Fatal("watcher did not receive event")
	}
	h.sync()
}
func (h *watchHarness) advance(d time.Duration) {
	h.t.Helper()
	// After the first barrier, any timer from the previous event is armed.
	h.sync()
	h.clk.advance(d)
	// The second barrier ensures a timer selected alongside the first
	// barrier has completed its publish/hash pass before we assert counts.
	h.sync()
	h.sync()
}

func TestWatchDebounceCoalescesBursts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	h := newHarness(t, up, dir)
	for i := 0; i < 5; i++ {
		write(t, dir, "index.html", fmt.Sprintf("v%d", i+1))
		h.event(filepath.Join(dir, "index.html"), fsnotify.Write)
		h.advance(100 * time.Millisecond)
		if up.calls() != 0 {
			t.Fatal("published before restarted debounce elapsed")
		}
	}
	h.advance(time.Second)
	if up.calls() != 1 {
		t.Fatalf("burst made %d uploads", up.calls())
	}
	h.advance(time.Hour)
	if up.calls() != 1 {
		t.Fatal("late duplicate upload")
	}
}

func TestWatchStartupAndIdenticalEdits(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "same")
	up := &fakeUploader{}
	h := newHarness(t, up, dir)
	h.advance(time.Hour)
	if up.calls() != 0 {
		t.Fatal("startup uploaded")
	}
	h.event(filepath.Join(dir, "page.html"), fsnotify.Write)
	h.advance(time.Second)
	if up.calls() != 0 {
		t.Fatal("identical content uploaded before first publish")
	}
	write(t, dir, "page.html", "new")
	h.event(filepath.Join(dir, "page.html"), fsnotify.Write)
	h.advance(time.Second)
	if up.calls() != 1 {
		t.Fatalf("real edit uploads=%d", up.calls())
	}
	// The original regression occurred only AFTER the first publication.
	write(t, dir, "page.html", "new")
	h.event(filepath.Join(dir, "page.html"), fsnotify.Write)
	h.advance(time.Second)
	if up.calls() != 1 {
		t.Fatal("identical rewrite uploaded after publication")
	}
}

func TestWatchIgnoresEditorAndDependencyChurn(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	h := newHarness(t, up, dir)
	for _, name := range []string{".index.html.swo", ".index.html.swa", ".index.html.swx", "index.html~", ".#index.html", "#index.html#", "index.html___jb_tmp___", "index.html___jb_old___", ".goutputstream-temp", ".git/index", "node_modules/pkg/index.js"} {
		write(t, dir, name, "private")
		h.event(filepath.Join(dir, name), fsnotify.Create)
	}
	h.advance(time.Hour)
	if up.calls() != 0 {
		t.Fatal("ignored changes published")
	}
	write(t, dir, "index.html", "v1")
	h.event(filepath.Join(dir, "index.html"), fsnotify.Write)
	h.advance(time.Second)
	if names := up.lastPack(); len(names) != 1 || names[0] != "index.html" {
		t.Fatalf("private files in upload: %v", names)
	}
	// A skipped-looking explicitly named root is NOT ignored.
}

func TestWatchExplicitBackupNamedFile(t *testing.T) {
	for _, name := range []string{"notes~", ".page.swo", "#draft#", "4913"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, name, "before")
			up := &fakeUploader{}
			h := newHarness(t, up, filepath.Join(dir, name))
			write(t, dir, name, "after")
			h.event(filepath.Join(dir, name), fsnotify.Write)
			h.advance(time.Second)
			if up.calls() != 1 {
				t.Fatalf("explicit file %s never uploaded", name)
			}
			h.event(filepath.Join(dir, "sibling"), fsnotify.Create)
			h.advance(time.Hour)
			if up.calls() != 1 {
				t.Fatal("sibling uploaded")
			}
		})
	}
}

func TestWatchSingleFlightWithTrailingPublish(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	block := make(chan struct{})
	up := &fakeUploader{block: block}
	h := newHarness(t, up, dir)
	write(t, dir, "index.html", "v1")
	h.event(filepath.Join(dir, "index.html"), fsnotify.Write)
	h.clk.advance(time.Second)
	// A notice barrier is intentionally unavailable while an upload is in
	// flight. Wait for the fake's snapshot, then edit that live source.
	waitFor(t, func() bool { return up.calls() == 1 })
	write(t, dir, "index.html", "v2")
	close(block)
	receive(t, h.published)
	h.event(filepath.Join(dir, "index.html"), fsnotify.Write)
	h.advance(time.Second)
	if up.calls() != 2 {
		t.Fatalf("expected initial plus trailing, got %d", up.calls())
	}
	h.advance(time.Hour)
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.updates != 2 || up.maxInFlight != 1 {
		t.Fatalf("uploads=%d concurrency=%d", up.updates, up.maxInFlight)
	}
}

// Polling is limited to a positive cross-goroutine milestone; no negative
// assertion uses wall-clock delays.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("condition not reached")
		}
	}
}

func TestWatchRetryNoticesAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name             string
		retryAfter, want time.Duration
	}{
		{"honored", 10 * time.Second, 10 * time.Second},
		{"clamped", 365 * 24 * time.Hour, MaxRetryAfter},
		{"backoff wins", time.Millisecond, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "index.html", "v0")
			up := &fakeUploader{failures: []error{&Error{Status: http.StatusTooManyRequests, RetryAfter: tc.retryAfter}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clk := newFakeClock()
			notices := make(chan Notice, 1)
			w := &watcher{ctx: ctx, quit: ctx.Done(), up: up, artifactID: "art", src: &Source{Path: dir},
				opts: WatchOptions{Clock: clk, Retrier: staticRetrier{time.Second}, OnNotice: func(n Notice) { notices <- n }}}
			done := make(chan error, 1)
			go func() { done <- w.publishWithRetry() }()
			notice := receive(t, notices)
			if notice.Kind != NoticeRetry || notice.Attempt != 2 || notice.Delay != tc.want {
				t.Fatalf("notice=%+v; want delay %v", notice, tc.want)
			}
			if delay := receive(t, clk.armed); delay != tc.want {
				t.Fatalf("waited %v; want %v", delay, tc.want)
			}
			clk.advance(tc.want - time.Millisecond)
			if up.calls() != 1 {
				t.Fatal("retried before the delay elapsed")
			}
			clk.advance(time.Millisecond)
			if err := receive(t, done); err != nil {
				t.Fatal(err)
			}
			if up.calls() != 2 {
				t.Fatal("retry never published")
			}
		})
	}
}

func TestTransientClassification(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 422, 429, 500, 503} {
		if got := transient(&Error{Status: status}); got != (status == 429 || status >= 500) {
			t.Errorf("status=%d transient=%v", status, got)
		}
	}
	if !transient(&NetworkError{Err: errors.New("offline")}) || transient(errors.New("invalid source")) {
		t.Fatal("incorrect network/local error classification")
	}
}
func TestExponentialRetrier(t *testing.T) {
	r := ExponentialRetrier{Base: time.Second, Max: time.Minute}
	for attempt := 1; attempt <= 20; attempt++ {
		nominal := min(time.Second*time.Duration(1<<min(attempt-1, 10)), time.Minute)
		for i := 0; i < 10; i++ {
			d := r.Next(attempt)
			if d < nominal-nominal/10 || d > nominal {
				t.Fatalf("attempt=%d delay=%v nominal=%v", attempt, d, nominal)
			}
		}
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[r.Next(100)] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter disappeared at cap")
	}
	for _, r := range []ExponentialRetrier{{}, {Base: -1}, {Base: time.Nanosecond, Max: time.Nanosecond}, {Base: 1 << 62, Max: 1<<63 - 1}} {
		if d := r.Next(100); d <= 0 {
			t.Fatalf("invalid default/overflow delay %v", d)
		}
	}
}

func TestWatchTypedErrorsAndNotices(t *testing.T) {
	for _, kind := range []string{"events_closed", "errors_closed", "watch_error", "root_vanished", "rewatch_failed"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "index.html", "v0")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fw, err := fsnotify.NewWatcher()
			if err != nil {
				t.Fatal(err)
			}
			defer fw.Close()
			events := make(chan fsnotify.Event, 1)
			errs := make(chan error, 1)
			notices := make(chan Notice, 1)
			w := &watcher{root: dir, ctx: ctx, quit: ctx.Done(), fw: fw, events: events, errors: errs, opts: WatchOptions{Clock: newFakeClock(), Debounce: time.Second, OnNotice: func(n Notice) { notices <- n }}}
			switch kind {
			case "events_closed":
				close(events)
				if err := w.waitEvent(0); !errors.Is(err, ErrWatchClosed) {
					t.Fatalf("closed channel: %v", err)
				}
			case "errors_closed":
				close(errs)
				if err := w.waitEvent(0); !errors.Is(err, ErrWatchClosed) {
					t.Fatalf("closed channel: %v", err)
				}
			case "watch_error":
				errs <- errors.New("read failed")
				if err := w.waitEvent(0); err != nil {
					t.Fatal(err)
				}
				if n := receive(t, notices); n.Kind != NoticeWatchError || n.Attempt != 0 {
					t.Fatalf("%+v", n)
				}
			case "root_vanished":
				w.root = filepath.Join(dir, "missing")
				if err := w.publishIfChanged(); err != nil {
					t.Fatal(err)
				}
				if n := receive(t, notices); n.Kind != NoticeRootVanished || n.Attempt != 0 {
					t.Fatalf("%+v", n)
				}
			case "rewatch_failed":
				fw.Close()
				w.onCreate(dir)
				if n := receive(t, notices); n.Kind != NoticeRewatchFailed || n.Attempt != 0 {
					t.Fatalf("%+v", n)
				}
			}
		})
	}
}

// Real fsnotify coverage is positive-only and synchronized with readiness
// and publication callbacks, not startup sleeps.
func runFilesystemWatch(t *testing.T, up Uploader, root string) (<-chan WatchResult, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	published := make(chan WatchResult, 16)
	done := make(chan error, 1)
	opts := WatchOptions{Debounce: 10 * time.Millisecond, OnReady: func() { close(ready) }, OnPublish: func(r WatchResult) { published <- r }}
	go func() { done <- Watch(ctx, up, "art", &Source{Path: root}, opts) }()
	receive(t, ready)
	return published, cancel, done
}
func TestWatchAtomicSaveAndRecreateFileRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "v0")
	up := &fakeUploader{}
	pub, cancel, done := runFilesystemWatch(t, up, filepath.Join(dir, "page.html"))
	defer func() {
		cancel()
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
	}()
	for i := 1; i <= 2; i++ {
		atomicSave(t, dir, "page.html", fmt.Sprint(i))
		receive(t, pub)
	}
	if err := os.Remove(filepath.Join(dir, "page.html")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "page.html", "recreated")
	receive(t, pub)
	write(t, dir, "page.html", "still watching")
	receive(t, pub)
	if up.calls() != 4 {
		t.Fatalf("atomic saves/recreation uploaded %d", up.calls())
	}
}
func TestWatchDirectoryRootRecreated(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "site")
	write(t, root, "index.html", "v0")
	up := &fakeUploader{}
	pub, cancel, done := runFilesystemWatch(t, up, root+"/")
	defer func() {
		cancel()
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	write(t, root, "index.html", "recreated")
	receive(t, pub)
	write(t, root, "deep/nested/asset.css", "v1")
	receive(t, pub)
	atomicSave(t, root, "deep/nested/asset.css", "v2")
	receive(t, pub)
}
func TestWatchUncleanRootDeletion(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	write(t, dir, "extra.css", "extra")
	up := &fakeUploader{}
	pub, cancel, done := runFilesystemWatch(t, up, dir+"/./")
	defer func() {
		cancel()
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Remove(filepath.Join(dir, "extra.css")); err != nil {
		t.Fatal(err)
	}
	receive(t, pub)
	if names := up.lastPack(); len(names) != 1 || names[0] != "index.html" {
		t.Fatalf("deleted file uploaded: %v", names)
	}
}
func TestWatchRelativeRoot(t *testing.T) {
	parent := t.TempDir()
	write(t, parent, "site/index.html", "v0")
	write(t, parent, "site/extra.css", "extra")
	t.Chdir(parent)
	up := &fakeUploader{}
	pub, cancel, done := runFilesystemWatch(t, up, "./site/")
	defer func() {
		cancel()
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Remove("site/extra.css"); err != nil {
		t.Fatal(err)
	}
	receive(t, pub)
	if names := up.lastPack(); len(names) != 1 || names[0] != "index.html" {
		t.Fatalf("relative root deletion dropped: %v", names)
	}
}

func TestWatchSkippedDirectoriesNotRegistered(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	for i := 0; i < 20; i++ {
		write(t, dir, fmt.Sprintf(".git/objects/%d/file", i), "x")
		write(t, dir, fmt.Sprintf("node_modules/pkg%d/lib/file", i), "x")
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	if err := watchTree(fw, dir); err != nil {
		t.Fatal(err)
	}
	if paths := fw.WatchList(); len(paths) != 1 || paths[0] != dir {
		t.Fatalf("registered skipped dirs: %v", paths)
	}
}
func TestWatchOverflowRewalksAndHashesLostChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	h := newHarness(t, up, dir)
	// No normal file event is delivered: an overflow must find this change.
	write(t, dir, "index.html", "lost event")
	h.errs <- fsnotify.ErrEventOverflow
	h.sync()
	h.advance(time.Second)
	if up.calls() != 1 {
		t.Fatal("overflow lost content change")
	}
	if paths := h.w.fw.WatchList(); len(paths) != 2 {
		t.Fatalf("missing root/parent after overflow: %v", paths)
	}
	h.errs <- fsnotify.ErrEventOverflow
	h.sync()
	h.advance(time.Second)
	if up.calls() != 1 {
		t.Fatal("unchanged overflow uploaded again")
	}
}
func TestWatchShutdown(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(fmt.Sprint(inFlight), func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "index.html", "v0")
			up := &fakeUploader{}
			if inFlight {
				up.block = make(chan struct{})
			}
			_, cancel, done := runFilesystemWatch(t, up, dir)
			defer cancel()
			if inFlight {
				write(t, dir, "index.html", "v1")
				waitFor(t, func() bool { return up.calls() == 1 })
			}
			cancel()
			if err := receive(t, done); err != nil {
				t.Fatalf("cancel returned %v", err)
			}
		})
	}
}
func TestWatchFatalError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{failures: []error{&Error{Status: 401}}}
	_, cancel, done := runFilesystemWatch(t, up, dir)
	defer cancel()
	write(t, dir, "index.html", "v1")
	if err := receive(t, done); StatusOf(err) != 401 {
		t.Fatalf("expected 401, got %v", err)
	}
}

// A build tool rewriting the folder while a version is packed must not
// stop the watcher: the publish waits for the tree to settle again.
func TestWatchTreeChangedDuringPack(t *testing.T) {
	for _, change := range []string{"deleted", "truncated"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "a.html", "a0")
			write(t, dir, "b.html", "b0")
			up := &fakeUploader{}
			h := newHarness(t, up, dir)
			once := false
			up.mu.Lock()
			up.duringPack = func(rel string) {
				if rel != "b.html" || once {
					return
				}
				once = true
				var err error
				if change == "deleted" {
					err = os.Remove(filepath.Join(dir, rel))
				} else {
					err = os.Truncate(filepath.Join(dir, rel), 1)
				}
				if err != nil {
					t.Error(err)
				}
			}
			up.mu.Unlock()
			write(t, dir, "a.html", "a1")
			h.event(filepath.Join(dir, "a.html"), fsnotify.Write)
			h.advance(time.Second)
			if n := receive(t, h.notices); n.Kind != NoticeTreeChanged || !errors.Is(n.Err, ErrSourceChanged) {
				t.Fatalf("notice=%+v", n)
			}
			if up.calls() != 0 {
				t.Fatal("published a pack of a changing tree")
			}
			// No new event: the failed pack itself marked the tree dirty.
			h.advance(time.Second)
			if up.calls() != 1 {
				t.Fatalf("uploads=%d after the tree settled; want 1", up.calls())
			}
			want := []string{"a.html", "b.html"}
			if change == "deleted" {
				want = want[:1]
			}
			if got := up.lastPack(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("pack=%v; want %v", got, want)
			}
		})
	}
}

// An emptied folder (a clean step before a rebuild) skips the publish and
// keeps watching; the rebuilt content then publishes normally.
func TestWatchEmptyFolder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	h := newHarness(t, up, dir)
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	h.event(filepath.Join(dir, "index.html"), fsnotify.Remove)
	h.advance(time.Second)
	if n := receive(t, h.notices); n.Kind != NoticeEmptyTree || !errors.Is(n.Err, ErrEmptyDirectory) {
		t.Fatalf("notice=%+v", n)
	}
	if up.calls() != 0 {
		t.Fatal("published an empty folder")
	}
	write(t, dir, "index.html", "v1")
	h.event(filepath.Join(dir, "index.html"), fsnotify.Create)
	h.advance(time.Second)
	if up.calls() != 1 {
		t.Fatalf("uploads=%d after rebuild; want 1", up.calls())
	}
}

// The only file vanishes after the scan but before the pack: the packer
// finds the folder empty, which is skipped like an empty scan.
func TestWatchFolderEmptiedBeforePack(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "v0")
	up := &fakeUploader{}
	h := newHarness(t, up, dir)
	up.mu.Lock()
	up.beforePrepare = func() {
		if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
			t.Error(err)
		}
	}
	up.mu.Unlock()
	write(t, dir, "index.html", "v1")
	h.event(filepath.Join(dir, "index.html"), fsnotify.Write)
	h.advance(time.Second)
	if n := receive(t, h.notices); n.Kind != NoticeEmptyTree || !errors.Is(n.Err, ErrEmptyDirectory) {
		t.Fatalf("notice=%+v", n)
	}
	if up.calls() != 0 {
		t.Fatal("published an empty folder")
	}
}

// A single file edited while the real client uploads it marks the tree
// dirty; the settled content publishes as one version.
func TestRealClientWatchMidUploadEdit(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "page.html", "v0")
	c, state := newUploadClient(t)
	h := newHarness(t, c, filepath.Join(dir, "page.html"))
	state.mu.Lock()
	state.onStart = func() { // runs with state.mu held
		state.onStart = nil
		if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte("v2"), 0600); err != nil {
			t.Error(err)
		}
	}
	state.mu.Unlock()
	write(t, dir, "page.html", "v1")
	h.event(filepath.Join(dir, "page.html"), fsnotify.Write)
	h.advance(time.Second)
	if n := receive(t, h.notices); n.Kind != NoticeTreeChanged || !errors.Is(n.Err, ErrSourceChanged) {
		t.Fatalf("notice=%+v", n)
	}
	h.advance(time.Second)
	if got, versions := state.lastFiles()["page.html"], state.versionCount(); got != "v2" || versions != 1 {
		t.Fatalf("published %q in %d versions; want v2 once", got, state.versions)
	}
	receive(t, h.published)
}
