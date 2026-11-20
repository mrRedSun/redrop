package tunnel

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

func TestReservePerUserLimitAndInUse(t *testing.T) {
	lim := testLimits()
	lim.PerUser = 2
	f := newFixture(t, lim, http.NotFoundHandler())
	f.connect(owner("t1", "u1"))
	f.connect(owner("t2", "u1"))
	_, err := f.reg.Reserve(owner("t3", "u1"))
	if e := apperr.As(err); e.Kind != apperr.KindRateLimited || e.Code != api.CodeTunnelLimit {
		t.Fatalf("third tunnel: %v", err)
	}
	// Another user has their own limit.
	f.connect(owner("t3", "u2"))
	// Another CLI process can't take a connected tunnel.
	other := owner("t1", "u1")
	other.Client = "another-process"
	if _, err := f.reg.Reserve(other); apperr.As(err).Code != api.CodeTunnelInUse {
		t.Fatalf("second process: %v", err)
	}
	// Turned off: refused.
	offLim := testLimits()
	offLim.PerUser = 0
	off := New(offLim, f.reg.log)
	defer off.Close()
	if _, err := off.Reserve(owner("t9", "u9")); apperr.As(err).Kind != apperr.KindForbidden {
		t.Fatalf("tunnels off: %v", err)
	}
}

// TestSameProcessReplaces: a CLI that reconnects with its own session value
// replaces its old connection at once, without counting twice.
func TestSameProcessReplaces(t *testing.T) {
	lim := testLimits()
	lim.PerUser = 1
	f := newFixture(t, lim, http.NotFoundHandler())
	old := f.connect(owner("t1", "u1"))
	f.connect(owner("t1", "u1"))
	if code, reason := old.closeCode(t); code != api.TunnelCloseReplaced || reason == "" {
		t.Fatalf("old connection closed with %d %q", code, reason)
	}
	if !f.reg.Online("t1") || len(f.reg.Connected()) != 1 {
		t.Fatalf("after replacing: online %v, %d connected", f.reg.Online("t1"), len(f.reg.Connected()))
	}
}

func TestEndingTunnels(t *testing.T) {
	f := newFixture(t, testLimits(), http.NotFoundHandler())
	byAdmin := f.connect(owner("t1", "u1"))
	if !f.reg.End("t1", WhyAdmin) || f.reg.End("t1", WhyAdmin) {
		t.Fatal("End reports whether a tunnel was connected")
	}
	if code, reason := byAdmin.closeCode(t); code != api.TunnelCloseEnded || reason != WhyAdmin {
		t.Fatalf("admin end: %d %q", code, reason)
	}

	disabled := f.connect(owner("t2", "u2"))
	other := f.connect(owner("t3", "u3"))
	f.reg.EndUser("u2")
	if code, reason := disabled.closeCode(t); code != api.TunnelCloseEnded || reason != WhyUserDisabled {
		t.Fatalf("disabled user: %d %q", code, reason)
	}
	o := owner("t4", "u3")
	o.TokenID = "tok-deleted"
	deleted := f.connect(o)
	f.reg.EndToken("tok-deleted")
	if code, reason := deleted.closeCode(t); code != api.TunnelCloseEnded || reason != WhyTokenRevoked {
		t.Fatalf("deleted token: %d %q", code, reason)
	}
	f.reg.Update("t3", false, 0, nil, WhyRevoked)
	if code, reason := other.closeCode(t); code != api.TunnelCloseEnded || reason != WhyRevoked {
		t.Fatalf("revoked: %d %q", code, reason)
	}
	if len(f.reg.Connected()) != 0 {
		t.Fatalf("still connected: %+v", f.reg.Connected())
	}
}

func TestExpiryEndsTheConnection(t *testing.T) {
	f := newFixture(t, testLimits(), http.NotFoundHandler())
	soon := time.Now().Add(50 * time.Millisecond)
	o := owner("t1", "u1")
	o.Expires = &soon
	p := f.start(o)
	if code, reason := p.closeCode(t); code != api.TunnelCloseEnded || reason != WhyExpired {
		t.Fatalf("expired: %d %q", code, reason)
	}
	// The token's expiry counts too, when it comes first.
	later := time.Now().Add(time.Hour)
	o = owner("t2", "u1")
	o.Expires, o.TokenExpires = &later, &soon
	p = f.start(o)
	if code, reason := p.closeCode(t); code != api.TunnelCloseEnded || reason != WhyTokenRevoked {
		t.Fatalf("token expired: %d %q", code, reason)
	}
	// A new TTL resets the timer.
	o = owner("t3", "u1")
	o.Expires = &later
	p = f.connect(o)
	f.reg.Update("t3", true, 0, &soon, "")
	if code, reason := p.closeCode(t); code != api.TunnelCloseEnded || reason != WhyExpired {
		t.Fatalf("shortened TTL: %d %q", code, reason)
	}
}

// TestNewAccessGenerationClosesOldStreams: a password change or a rotation
// closes what was let in before it, open WebSockets and downloads included.
func TestNewAccessGenerationClosesOldStreams(t *testing.T) {
	entered := make(chan struct{}, 1)
	f := newFixture(t, testLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "open")
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	f.connect(owner("t1", "u1"))
	r, err := f.request(context.Background(), "GET", "t1", "/", nil, "X-Test-Gen", "1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	within(t, "the request", entered)
	f.reg.Update("t1", true, 2, nil, "")
	if b, err := io.ReadAll(r.Body); err == nil || string(b) != "open" {
		t.Fatalf("stream from the old generation: %q %v; want it closed", b, err)
	}
	// A request still carrying the old generation is not let through.
	if status, _ := f.get("t1", "/", "X-Test-Gen", "1"); status != http.StatusServiceUnavailable {
		t.Fatalf("old generation after the change: %d", status)
	}
	if !f.reg.Online("t1") {
		t.Fatal("the tunnel itself must stay connected")
	}
}

func TestShutdownTellsCLIsToReconnect(t *testing.T) {
	f := newFixture(t, testLimits(), http.NotFoundHandler())
	p := f.connectWS(owner("t1", "u1"))
	f.reg.Shutdown(5 * time.Second)
	if code, _ := p.closeCode(t); code != websocketGoingAway {
		t.Fatalf("shutdown closed with %d", code)
	}
	if _, err := f.reg.Reserve(owner("t2", "u2")); apperr.As(err).Kind != apperr.KindConflict {
		t.Fatalf("reserve after shutdown: %v", err)
	}
}

func TestCancelledLeaseFreesTheSlot(t *testing.T) {
	lim := testLimits()
	lim.PerUser = 1
	f := newFixture(t, lim, http.NotFoundHandler())
	lease, err := f.reg.Reserve(owner("t1", "u1"))
	if err != nil {
		t.Fatal(err)
	}
	if f.reg.Online("t1") {
		t.Fatal("a reservation is not online")
	}
	lease.Cancel()
	lease.Cancel()
	f.connect(owner("t2", "u1"))
}
