package notify

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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

func TestPermanentFailuresAndRedirectsAreNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 404, 302} {
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
