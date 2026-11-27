// Package notify delivers operator notifications to a generic webhook
// (DESIGN.md "Abuse handling", "Notifications"). Sending never blocks the
// caller: messages go into a bounded queue that one worker drains, with
// bounded retries. A full queue drops the message and logs it.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Message is the webhook's JSON body. It must never carry untrusted text.
type Message struct {
	Level   string `json:"level"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Key     string `json:"key"`
}

// Notifier accepts messages without blocking.
type Notifier interface {
	Notify(Message)
}

// Discard is the notifier used when no webhook is configured.
type Discard struct{}

func (Discard) Notify(Message) {}

// Delivery bounds.
const (
	QueueSize      = 64
	Attempts       = 4
	AttemptTimeout = 10 * time.Second
)

// Backoff is the wait before each retry (the first attempt is immediate).
var Backoff = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

// Webhook posts each message to a URL, with an optional bearer token.
type Webhook struct {
	url, token string
	client     *http.Client
	log        *slog.Logger
	backoff    []time.Duration

	queue  chan Message
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// New starts a webhook worker. Close stops it.
func New(url, token string, log *slog.Logger) *Webhook {
	return newWebhook(url, token, log, httpClient(), Backoff)
}

// httpClient does not follow redirects: a 3xx is a refusal, so the token
// is only ever sent to the configured URL.
func httpClient() *http.Client {
	return &http.Client{Timeout: AttemptTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func newWebhook(url, token string, log *slog.Logger, client *http.Client, backoff []time.Duration) *Webhook {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Webhook{url: url, token: token, client: client, log: log, backoff: backoff,
		queue: make(chan Message, QueueSize), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go w.run()
	return w
}

// Notify queues m. It never blocks: with the queue full, or after Close,
// m is dropped and logged.
func (w *Webhook) Notify(m Message) {
	if w.ctx.Err() != nil {
		w.log.Warn("notification dropped: shutting down", "key", m.Key)
		return
	}
	select {
	case w.queue <- m:
	default:
		w.log.Warn("notification dropped: queue full", "key", m.Key)
	}
}

// Close stops the worker: an attempt in flight is cancelled and queued
// messages are dropped (and counted in the log). It waits for the worker.
func (w *Webhook) Close() {
	w.once.Do(func() {
		w.cancel()
		<-w.done
		if n := len(w.queue); n > 0 {
			w.log.Warn("notifications dropped at shutdown", "count", n)
		}
	})
}

func (w *Webhook) run() {
	defer close(w.done)
	for {
		select {
		case <-w.ctx.Done():
			return
		case m := <-w.queue:
			w.deliver(m)
		}
	}
}

// deliver sends m, retrying network errors, 408, 429 and 5xx.
func (w *Webhook) deliver(m Message) {
	body, err := json.Marshal(m)
	if err != nil {
		w.log.Error("notification encode", "error", err)
		return
	}
	for attempt := 1; ; attempt++ {
		err := w.post(body)
		if err == nil {
			return
		}
		var perm permanent
		if errors.As(err, &perm) || attempt >= Attempts || attempt > len(w.backoff) {
			w.log.Warn("notification not delivered", "key", m.Key, "attempts", attempt, "error", err)
			return
		}
		t := time.NewTimer(w.backoff[attempt-1])
		select {
		case <-w.ctx.Done():
			t.Stop()
			w.log.Warn("notification not delivered: shutting down", "key", m.Key)
			return
		case <-t.C:
		}
	}
}

// permanent is a refusal that a retry would not change.
type permanent struct{ status int }

func (p permanent) Error() string { return fmt.Sprintf("webhook answered %d", p.status) }

func (w *Webhook) post(body []byte) error {
	ctx, cancel := context.WithTimeout(w.ctx, AttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return permanent{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "redrop-notify")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		// The error text can hold the URL; log only that the request failed.
		return errors.New("request failed")
	}
	resp.Body.Close()
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		return nil
	case s == http.StatusRequestTimeout || s == http.StatusTooManyRequests || s >= 500:
		return fmt.Errorf("webhook answered %d", s)
	default:
		return permanent{status: s}
	}
}
