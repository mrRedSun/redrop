package client

import (
	"context"
	"errors"
	"fmt"
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
	mu         sync.Mutex
	answers    []error // nil: grant a lease; consumed in order, then nil
	renewAfter int
	onHold     func()
	calls      chan liveCall
}

func (f *fakeLiveHolder) HoldLive(ctx context.Context, id, session string) (*api.LiveLease, error) {
	f.mu.Lock()
	var err error
	if len(f.answers) > 0 {
		err, f.answers = f.answers[0], f.answers[1:]
	}
	f.mu.Unlock()
	f.calls <- liveCall{"hold", id, session, ctx.Err()}
	if f.onHold != nil {
		f.onHold()
	}
	if err != nil {
		return nil, err
	}
	return &api.LiveLease{RenewAfterSeconds: f.renewAfter}, nil
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
	return startKeepLiveWithRenew(t, 20, answers...)
}

func startKeepLiveWithRenew(t *testing.T, renewAfter int, answers ...error) *liveRun {
	t.Helper()
	r := &liveRun{t: t, holder: &fakeLiveHolder{answers: answers, renewAfter: renewAfter, calls: make(chan liveCall, 16)}, clk: newFakeClock(),
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
	select {
	case d := <-r.clk.armed:
		return d
	case <-r.done:
		r.t.Fatal("KeepLive returned instead of scheduling a retry")
	case <-time.After(30 * time.Second):
		r.t.Fatal("no wait within 30s")
	}
	return 0
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

func TestKeepLiveConflictsRecover(t *testing.T) {
	for _, reason := range []string{
		"too many watch sessions hold live reload on this artifact; stop one first",
		"live reload needs the info button",
		"live reload needs a framed artifact (HTML or bundle)",
	} {
		t.Run(reason, func(t *testing.T) {
			refused := &Error{Status: http.StatusConflict, Detail: reason}
			r := startKeepLive(t, refused, refused, nil, nil, refused, refused, nil)
			kinds := []NoticeKind{NoticeLiveOff, "", NoticeLiveOn, "", NoticeLiveOff, "", NoticeLiveOn}
			for attempt, kind := range kinds {
				r.call("hold")
				if kind != "" {
					n := r.notice(kind)
					if kind == NoticeLiveOff && !errors.Is(n.Err, refused) {
						t.Fatalf("live_off cause %v, want %v", n.Err, refused)
					}
				}
				if d := r.wait(); d != 20*time.Second {
					t.Fatalf("attempt %d waits %s, want 20s", attempt, d)
				}
				r.noNotice() // no repeated live_off or live_on within a run
				if attempt+1 < len(kinds) {
					r.clk.advance(20 * time.Second)
				}
			}
		})
	}
}

func TestKeepLiveConflictUsesKnownRenewalInterval(t *testing.T) {
	refused := &Error{Status: http.StatusConflict, Detail: "live reload needs the info button"}
	unavailable := &Error{Status: http.StatusServiceUnavailable, Detail: "down"}
	for _, seconds := range []int{7, 0} {
		t.Run(fmt.Sprintf("renew_after_%d", seconds), func(t *testing.T) {
			r := startKeepLiveWithRenew(t, seconds, nil, refused, unavailable, refused, refused, nil)
			renew := 20 * time.Second
			if seconds > 0 {
				renew = time.Duration(seconds) * time.Second
			}
			kinds := []NoticeKind{NoticeLiveOn, NoticeLiveOff, NoticeLiveError, NoticeLiveOff, "", NoticeLiveOn}
			for attempt, kind := range kinds {
				r.call("hold")
				if kind != "" {
					r.notice(kind)
				}
				wait := renew
				if kind == NoticeLiveError {
					wait = liveRetryWait
				}
				if d := r.wait(); d != wait {
					t.Fatalf("attempt %d waits %s, want %s", attempt, d, wait)
				}
				r.noNotice()
				if attempt+1 < len(kinds) {
					r.clk.advance(wait)
				}
			}
		})
	}
}

func TestKeepLiveConflictCancellation(t *testing.T) {
	refused := &Error{Status: http.StatusConflict, Detail: "live reload needs the info button"}
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held_%t", held), func(t *testing.T) {
			answers := []error{refused}
			if held {
				answers = append([]error{nil}, answers...)
			}
			r := startKeepLive(t, answers...)
			if held {
				r.call("hold")
				r.notice(NoticeLiveOn)
				r.wait()
				r.clk.advance(20 * time.Second)
			}
			r.call("hold")
			r.notice(NoticeLiveOff)
			r.wait()
			r.cancel()
			if held {
				if c := r.call("release"); c.ctxErr != nil {
					t.Fatalf("release ran with a cancelled context: %v", c.ctxErr)
				}
			}
			recv(t, r.done, "KeepLive to return")
			r.clk.advance(time.Hour)
			select {
			case c := <-r.holder.calls:
				t.Fatalf("unexpected call after cancellation %+v", c)
			default:
			}
			r.noNotice()
		})
	}
}

func TestKeepLiveReleasesSuccessfulHoldOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder := &fakeLiveHolder{calls: make(chan liveCall, 2), onHold: cancel}
	KeepLive(ctx, holder, "art", LiveOptions{Session: "session-0123456789", OnNotice: func(n Notice) {
		t.Fatalf("notice after cancellation %+v", n)
	}})
	recv(t, holder.calls, "hold")
	select {
	case c := <-holder.calls:
		if c.method != "release" || c.ctxErr != nil {
			t.Fatalf("call %+v, want release with an uncancelled context", c)
		}
	default:
		t.Fatal("successful hold was not released when cancellation arrived with its response")
	}
}

func TestKeepLiveRefused(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			refused := &Error{Status: status, Detail: "live reload refused"}
			r := startKeepLive(t, refused)
			r.call("hold")
			if n := r.notice(NoticeLiveOff); !errors.Is(n.Err, refused) {
				t.Fatalf("live_off cause %v", n.Err)
			}
			recv(t, r.done, "KeepLive to return")
			select {
			case c := <-r.holder.calls:
				t.Fatalf("unexpected call %+v", c)
			default:
			}
			select {
			case d := <-r.clk.armed:
				t.Fatalf("terminal refusal scheduled a retry after %s", d)
			default:
			}

			// A terminal refusal after a successful hold releases the lease.
			r = startKeepLive(t, nil, refused)
			r.call("hold")
			r.notice(NoticeLiveOn)
			r.wait()
			r.clk.advance(20 * time.Second)
			r.call("hold")
			r.notice(NoticeLiveOff)
			r.call("release")
			recv(t, r.done, "KeepLive to return")
		})
	}
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
