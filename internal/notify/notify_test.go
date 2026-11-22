package notify

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fake is a webhook receiver that answers with the next status of a
// script and records each request.
type fake struct {
	srv      *httptest.Server
	statuses chan int
	got      chan *http.Request
	bodies   chan []byte
	hits     atomic.Int32
}

func newFake(t *testing.T, statuses ...int) *fake {
	f := &fake{statuses: make(chan int, 32), got: make(chan *http.Request, 32), bodies: make(chan []byte, 32)}
	for _, s := range statuses {
		f.statuses <- s
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		status := http.StatusOK
		select {
		case status = <-f.statuses:
		default:
		}
		w.WriteHeader(status)
		f.got <- r
		f.bodies <- b
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// wait returns the next delivery attempt; the bound only catches a hang.
func (f *fake) wait(t *testing.T) (*http.Request, []byte) {
	t.Helper()
	select {
	case r := <-f.got:
		return r, <-f.bodies
	case <-time.After(30 * time.Second):
		t.Fatal("no delivery attempt")
		return nil, nil
	}
}

var quick = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

func hook(t *testing.T, url, token string, backoff []time.Duration) *Webhook {
	w := newWebhook(url, token, slog.New(slog.DiscardHandler), &http.Client{Timeout: 10 * time.Second}, backoff)
	t.Cleanup(w.Close)
	return w
}

var msg = Message{Level: "warning", Title: "redrop: abuse report", Message: "Category spam, artifact a1, owner bob.", Key: "redrop-report-r1"}

func TestDeliversJSONWithBearerToken(t *testing.T) {
	f := newFake(t)
	w := hook(t, f.srv.URL+"/hook", "tok", quick)
	w.Notify(msg)
	r, b := f.wait(t)
	var got Message
	if err := json.Unmarshal(b, &got); err != nil || got != msg {
		t.Fatalf("body %s: %v", b, err)
	}
	if r.Method != "POST" || r.URL.Path != "/hook" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request: %s %s %v", r.Method, r.URL, r.Header)
	}
	var fields map[string]any
	json.Unmarshal(b, &fields)
	if len(fields) != 4 {
		t.Fatalf("fields: %v", fields)
	}
}

func TestNoTokenNoAuthorization(t *testing.T) {
	f := newFake(t)
	hook(t, f.srv.URL, "", quick).Notify(msg)
	if r, _ := f.wait(t); r.Header.Get("Authorization") != "" {
		t.Fatal("Authorization sent without a token")
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	f := newFake(t, 500, 429, 408)
	hook(t, f.srv.URL, "", quick).Notify(msg)
	for i := 0; i < 4; i++ {
		f.wait(t)
	}
	// The fourth attempt succeeded; nothing more is sent.
	if n := f.hits.Load(); n != 4 {
		t.Fatalf("%d attempts", n)
	}
}

func TestGivesUpAfterFourAttempts(t *testing.T) {
	f := newFake(t, 503, 503, 503, 503, 503, 503)
	w := hook(t, f.srv.URL, "", quick)
	w.Notify(msg)
	for i := 0; i < 4; i++ {
		f.wait(t)
	}
	// A second message is delivered after the first was given up: the
	// fifth request is the new message's first attempt.
	second := msg
	second.Key = "redrop-report-r2"
	w.Notify(second)
	_, b := f.wait(t)
	var got Message
	json.Unmarshal(b, &got)
	if got.Key != "redrop-report-r2" {
		t.Fatalf("fifth request carried %q: the first message was retried more than 4 times", got.Key)
	}
}

func TestPermanentFailuresAreNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 404} {
		f := newFake(t, status)
		w := newWebhook(f.srv.URL, "tok", slog.New(slog.DiscardHandler), httpClient(), quick)
		w.Notify(msg)
		f.wait(t)
		next := msg
		next.Key = "next"
		w.Notify(next)
		_, b := f.wait(t)
		var got Message
		json.Unmarshal(b, &got)
		if got.Key != "next" {
			t.Errorf("%d was retried", status)
		}
		w.Close()
	}
}

// A redirect is a refusal, not something to follow: with a Location header
// present, the client must still send the token only to the configured URL.
func TestRedirectWithLocationIsNotFollowed(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	requests := make(chan *http.Request, 4)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		http.Redirect(w, r, target.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	w := newWebhook(source.URL, "tok", slog.New(slog.DiscardHandler), httpClient(), quick)
	t.Cleanup(w.Close)
	first := msg
	first.Key = "first"
	second := msg
	second.Key = "second"
	w.Notify(first)
	w.Notify(second)
	// Each message gets exactly one attempt: two requests to the configured
	// URL, none to the Location target.
	for i := 0; i < 2; i++ {
		select {
		case r := <-requests:
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Fatalf("attempt %d sent Authorization %q", i, r.Header.Get("Authorization"))
			}
		case <-time.After(30 * time.Second):
			t.Fatal("no redirect request")
		}
	}
	w.Close()
	if n := redirected.Load(); n != 0 {
		t.Fatalf("the redirect target was hit %d times", n)
	}
}

// roundTripFunc turns a function into an http.RoundTripper, so tests can
// answer without a real server (synctest cannot fast-forward real sockets).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}
}

// The production schedule (1, 4, 16 s), under a fake clock so the waits are
// not real. A stub transport answers; only the backoff takes time.
func TestProductionBackoffSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts atomic.Int32
		at := make(chan time.Time, 4)
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			at <- time.Now()
			if n := attempts.Add(1); n >= 4 {
				return resp(http.StatusOK), nil
			}
			return resp(http.StatusInternalServerError), nil
		})
		w := newWebhook("http://hook.test/hook", "tok", slog.New(slog.DiscardHandler), &http.Client{Transport: rt}, Backoff)
		defer w.Close()
		w.Notify(msg)
		for attempts.Load() < 4 {
			time.Sleep(time.Millisecond)
		}
		first := <-at
		var gaps []time.Duration
		for range 3 {
			next := <-at
			gaps = append(gaps, next.Sub(first))
		}
		// Each gap is the sum of the backoffs before it, within the poll sleep.
		cumulative := time.Duration(0)
		for i, b := range Backoff {
			cumulative += b
			if gaps[i] < cumulative || gaps[i] > cumulative+50*time.Millisecond {
				t.Fatalf("attempt %d gap %v, want %v (backoff %v)", i+2, gaps[i], cumulative, Backoff)
			}
		}
	})
}

// A receiver that never answers is abandoned after AttemptTimeout per
// attempt, also under the fake clock.
func TestAttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts atomic.Int32
		done := make(chan time.Time, 2)
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts.Add(1)
			<-r.Context().Done() // hang until the attempt's deadline
			done <- time.Now()
			return nil, r.Context().Err()
		})
		w := newWebhook("http://hook.test/hook", "tok", slog.New(slog.DiscardHandler), &http.Client{Transport: rt}, []time.Duration{time.Millisecond})
		defer w.Close()
		start := time.Now()
		w.Notify(msg)
		for attempts.Load() < 1 {
			time.Sleep(time.Millisecond)
		}
		abandoned := <-done
		// The first attempt was abandoned by its timeout, not by Close:
		// the deadline elapsed while the request still hung.
		if got := abandoned.Sub(start); got < AttemptTimeout || got > AttemptTimeout+50*time.Millisecond {
			t.Fatalf("attempt abandoned after %v, want %v", got, AttemptTimeout)
		}
	})
}

func TestNotifyNeverBlocks(t *testing.T) {
	// A receiver that never answers holds the worker on its first message.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	w := hook(t, srv.URL, "", quick)
	done := make(chan struct{})
	go func() {
		for i := 0; i < QueueSize*3; i++ {
			w.Notify(msg)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Notify blocked on a full queue")
	}
	if n := len(w.queue); n > QueueSize {
		t.Fatalf("queue holds %d", n)
	}
	// Close cancels the stuck attempt and returns; later messages are dropped.
	closed := make(chan struct{})
	go func() { w.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close waited for a stuck delivery")
	}
	w.Notify(msg)
}

func TestCloseInterruptsBackoff(t *testing.T) {
	f := newFake(t, 500)
	w := newWebhook(f.srv.URL, "", slog.New(slog.DiscardHandler), &http.Client{}, []time.Duration{time.Hour})
	w.Notify(msg)
	f.wait(t)
	closed := make(chan struct{})
	go func() { w.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close waited out the retry backoff")
	}
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("%d attempts after Close", n)
	}
}
