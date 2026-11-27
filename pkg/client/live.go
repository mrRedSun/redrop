package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
)

// Live reload (DESIGN.md "Live reload"): while a watch session holds a
// lease on an artifact, viewers of its current version reload when a new
// version becomes current.

// HoldLive creates or renews session's live reload lease on artifact id.
func (c *Client) HoldLive(ctx context.Context, id, session string) (*api.LiveLease, error) {
	var out api.LiveLease
	return &out, c.do(ctx, http.MethodPut, "/api/v1/artifacts/"+esc(id)+"/live/"+esc(session), nil, &out)
}

// ReleaseLive ends session's live reload lease on artifact id.
func (c *Client) ReleaseLive(ctx context.Context, id, session string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/artifacts/"+esc(id)+"/live/"+esc(session), nil, nil)
}

// LiveHolder holds live reload leases. *Client implements it.
type LiveHolder interface {
	HoldLive(ctx context.Context, id, session string) (*api.LiveLease, error)
	ReleaseLive(ctx context.Context, id, session string) error
}

const (
	// liveRetryWait is how soon a renewal that failed transiently is tried
	// again; the lease (60 s by default) outlasts a few of them.
	liveRetryWait = 5 * time.Second
	// liveReleaseTimeout bounds the release when watching stops, so a
	// server that is gone can't hold up the exit; the lease then lapses.
	liveReleaseTimeout = 5 * time.Second
	// liveDefaultRenew is used if the server names no renewal interval.
	liveDefaultRenew = 20 * time.Second
)

// LiveOptions configures KeepLive.
type LiveOptions struct {
	// OnNotice receives NoticeLiveOn, NoticeLiveError and NoticeLiveOff.
	OnNotice func(Notice)
	// Clock overrides timekeeping (tests).
	Clock Clock
	// Session is the watch session's ID (tests); a random one by default.
	Session string
}

// NewLiveSession returns a random watch session ID.
func NewLiveSession() string {
	b := make([]byte, 16)
	rand.Read(b) // never fails (crypto/rand)
	return hex.EncodeToString(b)
}

// KeepLive holds live reload on artifact id until ctx is done, renewing the
// lease as the server asks, then releases it. It reports through
// opts.OnNotice: NoticeLiveOn when the lease is held (again), NoticeLiveError
// on the first of a run of failed renewals (network, 5xx, 429), which are
// retried, and NoticeLiveOff on the first of a run of conflicts (409), which
// are retried at the last known renewal interval (20 s by default). Other
// permanent refusals report NoticeLiveOff and return. It never fails the watch.
func KeepLive(ctx context.Context, h LiveHolder, id string, opts LiveOptions) {
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	session := opts.Session
	if session == "" {
		session = NewLiveSession()
	}
	notify := func(n Notice) {
		if opts.OnNotice != nil {
			opts.OnNotice(n)
		}
	}
	// A successful hold must be released even if cancellation arrives with
	// its response. The last notice suppresses repeats within each run.
	held := false
	var state NoticeKind
	renew := liveDefaultRenew
	defer func() {
		if !held {
			return
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), liveReleaseTimeout)
		defer cancel()
		h.ReleaseLive(rctx, id, session) // best effort: an unreleased lease lapses
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		lease, err := h.HoldLive(ctx, id, session)
		if err == nil {
			held = true
		}
		if ctx.Err() != nil {
			return
		}
		wait := renew
		var apiErr *Error
		switch {
		case err == nil:
			if lease.RenewAfterSeconds > 0 {
				renew = time.Duration(lease.RenewAfterSeconds) * time.Second
			}
			wait = renew
			if state != NoticeLiveOn {
				notify(Notice{Kind: NoticeLiveOn})
			}
			state = NoticeLiveOn
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict:
			if state != NoticeLiveOff {
				notify(Notice{Kind: NoticeLiveOff, Err: err})
			}
			state = NoticeLiveOff
		case transient(err):
			if state != NoticeLiveError {
				notify(Notice{Kind: NoticeLiveError, Err: err})
			}
			state = NoticeLiveError
			wait = liveRetryWait
			if apiErr != nil && apiErr.RetryAfter > wait {
				wait = min(apiErr.RetryAfter, MaxRetryAfter)
			}
		default:
			notify(Notice{Kind: NoticeLiveOff, Err: err})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.After(wait):
		}
	}
}
