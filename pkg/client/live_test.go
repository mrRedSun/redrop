package client

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
)

// KeepLive (DESIGN.md "Live reload"): renewals on the server's schedule,
// retries, a refusal, and the release when watching stops. Waits are on the
// fake clock; real time only bounds a hang.

type liveCall struct {
	method, id, session string
	ctxErr              error
}

type fakeLiveHolder struct {
	mu      sync.Mutex
	answers []error // nil: grant a lease; consumed in order, then nil
	calls   chan liveCall
}

func (f *fakeLiveHolder) HoldLive(ctx context.Context, id, session string) (*api.LiveLease, error) {
	f.mu.Lock()
	var err error
	if len(f.answers) > 0 {
		err, f.answers = f.answers[0], f.answers[1:]
	}
	f.mu.Unlock()
	f.calls <- liveCall{"hold", id, session, ctx.Err()}
	if err != nil {
		return nil, err
	}
	return &api.LiveLease{ExpiresAt: time.Now().Add(time.Minute), RenewAfterSeconds: 20}, nil
}

func (f *fakeLiveHolder) ReleaseLive(ctx context.Context, id, session string) error {
	f.calls <- liveCall{"release", id, session, ctx.Err()}
	return nil
}

type liveRun struct {
	t       *testing.T
	holder  *fakeLiveHolder
	clk     *fakeClock
	notices chan Notice
	cancel  context.CancelFunc
	done    chan struct{}
}

func startKeepLive(t *testing.T, answers ...error) *liveRun {
	t.Helper()
	r := &liveRun{t: t, holder: &fakeLiveHolder{answers: answers, calls: make(chan liveCall, 16)}, clk: newFakeClock(),
		notices: make(chan Notice, 16), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(func() { cancel(); <-r.done })
	go func() {
		defer close(r.done)
		KeepLive(ctx, r.holder, "art", LiveOptions{Clock: r.clk, Session: "session-0123456789", OnNotice: func(n Notice) { r.notices <- n }})
	}()
	return r
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("no %s within 30s", what)
		panic("unreachable")
	}
}

func (r *liveRun) call(method string) liveCall {
	r.t.Helper()
	c := recv(r.t, r.holder.calls, method)
	if c.method != method || c.id != "art" || c.session != "session-0123456789" {
		r.t.Fatalf("call %+v, want %s", c, method)
	}
	return c
}

func (r *liveRun) notice(kind NoticeKind) Notice {
	r.t.Helper()
	n := recv(r.t, r.notices, string(kind))
	if n.Kind != kind {
		r.t.Fatalf("notice %+v, want %s", n, kind)
	}
	return n
}

// wait returns the next wait KeepLive armed on the clock.
func (r *liveRun) wait() time.Duration {
	r.t.Helper()
	return recv(r.t, (<-chan time.Duration)(r.clk.armed), "wait")
}

func (r *liveRun) noNotice() {
	r.t.Helper()
	select {
	case n := <-r.notices:
		r.t.Fatalf("unexpected notice %+v", n)
	default:
	}
}

func TestKeepLiveRenewsRetriesAndReleases(t *testing.T) {
	unavailable := &Error{Status: http.StatusServiceUnavailable, Detail: "down"}
	r := startKeepLive(t, nil, nil, &NetworkError{Err: errors.New("reset")}, unavailable, nil)
	r.call("hold")
	r.notice(NoticeLiveOn)
	if d := r.wait(); d != 20*time.Second {
		t.Fatalf("renewal after %s, want the server's 20s", d)
	}
	r.clk.advance(20 * time.Second)
	r.call("hold")
	r.wait()
	r.noNotice() // a renewal is quiet

	// Transient failures: one notice for the run, retried sooner.
	r.clk.advance(20 * time.Second)
	r.call("hold")
	if n := r.notice(NoticeLiveError); n.Err == nil {
		t.Fatal("live_error without the cause")
	}
	if d := r.wait(); d != liveRetryWait {
		t.Fatalf("retry after %s, want %s", d, liveRetryWait)
	}
	r.clk.advance(liveRetryWait)
	r.call("hold")
	r.wait()
	r.noNotice()
	r.clk.advance(liveRetryWait)
	r.call("hold")
	r.notice(NoticeLiveOn) // back on
	r.wait()

	// Stopping releases the lease, with a context that is not cancelled.
	r.cancel()
	if c := r.call("release"); c.ctxErr != nil {
		t.Fatalf("release ran with a cancelled context: %v", c.ctxErr)
	}
	recv(t, r.done, "KeepLive to return")
}

func TestKeepLiveRefused(t *testing.T) {
	refused := &Error{Status: http.StatusConflict, Detail: "live reload needs the info button"}
	r := startKeepLive(t, refused)
	r.call("hold")
	if n := r.notice(NoticeLiveOff); !errors.Is(n.Err, refused) {
		t.Fatalf("live_off cause %v", n.Err)
	}
	recv(t, r.done, "KeepLive to return")
	// Nothing was held, so nothing is released.
	select {
	case c := <-r.holder.calls:
		t.Fatalf("unexpected call %+v", c)
	default:
	}

	// Refused later (the info button was turned off): the lease is
	// released.
	r = startKeepLive(t, nil, refused)
	r.call("hold")
	r.notice(NoticeLiveOn)
	r.wait()
	r.clk.advance(20 * time.Second)
	r.call("hold")
	r.notice(NoticeLiveOff)
	r.call("release")
	recv(t, r.done, "KeepLive to return")
}

func TestNewLiveSession(t *testing.T) {
	a, b := NewLiveSession(), NewLiveSession()
	if len(a) != 32 || a == b {
		t.Fatalf("sessions %q %q", a, b)
	}
}

func TestKeepLiveHonorsRetryAfter(t *testing.T) {
	full := &Error{Status: http.StatusTooManyRequests, Detail: "too many live reload sessions", RetryAfter: time.Minute}
	r := startKeepLive(t, full)
	r.call("hold")
	r.notice(NoticeLiveError)
	if d := r.wait(); d != time.Minute {
		t.Fatalf("retry after %s, want the server's Retry-After", d)
	}
	r.clk.advance(time.Minute)
	r.call("hold")
	r.notice(NoticeLiveOn)
}
