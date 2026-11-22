package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mrRedSun/redrop/pkg/api"
)

type fakeResolver map[string][]string

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []netip.Addr
	for _, a := range addrs {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

func TestResolveTunnelTarget(t *testing.T) {
	dns := fakeResolver{
		"devbox": {"127.0.1.1"}, "nas": {"192.168.1.20"}, "example.org": {"192.0.2.80"},
		"mixed": {"127.0.0.1", "192.168.1.20"}, "localhost": {"203.0.113.1"}, // never asked
	}
	for _, tc := range []struct {
		in     string
		remote bool
		host   string
		addrs  string
		class  AddrClass
		err    string
	}{
		{in: "3000", host: "localhost:3000", addrs: "127.0.0.1:3000 [::1]:3000", class: ClassLoopback},
		{in: "localhost:5173", host: "localhost:5173", addrs: "127.0.0.1:5173 [::1]:5173", class: ClassLoopback},
		{in: "app.localhost:8080", host: "app.localhost:8080", addrs: "127.0.0.1:8080 [::1]:8080", class: ClassLoopback},
		{in: "127.0.0.1:8080", host: "127.0.0.1:8080", addrs: "127.0.0.1:8080", class: ClassLoopback},
		{in: "[::1]:9000", host: "[::1]:9000", addrs: "[::1]:9000", class: ClassLoopback},
		{in: "http://localhost:3000/", host: "localhost:3000", addrs: "127.0.0.1:3000 [::1]:3000", class: ClassLoopback},
		{in: "http://devbox", host: "devbox:80", addrs: "127.0.1.1:80", class: ClassLoopback},
		{in: "nas:8080", err: "--allow-remote-target"},
		{in: "nas:8080", remote: true, host: "nas:8080", addrs: "192.168.1.20:8080", class: ClassPrivate},
		{in: "10.0.0.5:80", err: "private network"},
		{in: "100.64.0.1:80", remote: true, host: "100.64.0.1:80", addrs: "100.64.0.1:80", class: ClassPrivate},
		{in: "[fe80::1]:80", remote: true, host: "[fe80::1]:80", addrs: "[fe80::1]:80", class: ClassPrivate},
		{in: "example.org:443", err: "public internet"},
		{in: "example.org:8443", remote: true, host: "example.org:8443", addrs: "192.0.2.80:8443", class: ClassPublic},
		{in: "mixed:80", remote: true, err: "more than one kind of network"},
		{in: "0.0.0.0:3000", remote: true, err: "not a single host"},
		{in: "[::]:3000", remote: true, err: "not a single host"},
		{in: "224.0.0.1:3000", remote: true, err: "multicast"},
		{in: "255.255.255.255:3000", remote: true, err: "broadcast"},
		{in: "https://localhost:3000", err: "HTTPS targets"},
		{in: "ftp://localhost:21", err: "http://host:port"},
		{in: "http://localhost:3000/app", err: "without a path"},
		{in: "http://user@localhost:3000", err: "without a path"},
		{in: "0", err: "invalid"},
		{in: "70000", err: "invalid"},
		{in: "03000", err: "invalid"},
		{in: "localhost", err: "invalid tunnel target"},
		{in: "", err: "missing tunnel target"},
		{in: "unknown-host:80", err: "cannot resolve"},
	} {
		got, err := ResolveTunnelTarget(context.Background(), tc.in, tc.remote, dns)
		if tc.err != "" {
			var usage *UsageError
			if err == nil || !strings.Contains(err.Error(), tc.err) || !errors.As(err, &usage) {
				t.Errorf("%q: %v, want a usage error with %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		var addrs []string
		for _, a := range got.Addrs {
			addrs = append(addrs, a.String())
		}
		if got.Host != tc.host || strings.Join(addrs, " ") != tc.addrs || got.Class != tc.class {
			t.Errorf("%q: host %q addrs %v class %q", tc.in, got.Host, addrs, got.Class)
		}
	}
}

func TestRelativeLocation(t *testing.T) {
	tg := &TunnelTarget{Host: "localhost:3000"}
	for in, want := range map[string]string{
		"http://localhost:3000/login?next=%2Fa#top": "/login?next=%2Fa#top",
		"http://LOCALHOST:3000":                     "/",
		"http://localhost:3000//evil.example/x":     "/evil.example/x",
		"https://other.example/x":                   "https://other.example/x",
		"/relative":                                 "/relative",
	} {
		resp := &http.Response{Header: http.Header{"Location": {in}}}
		tg.relativeLocation(resp)
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// stepClock moves time forward by step on every Now, and by the wait on
// every After, which fires at once: reconnect tests never sleep.
type stepClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// countingRetrier returns attempt seconds and records the attempts asked.
type countingRetrier struct {
	mu       sync.Mutex
	attempts []int
}

func (r *countingRetrier) Next(attempt int) time.Duration {
	r.mu.Lock()
	r.attempts = append(r.attempts, attempt)
	r.mu.Unlock()
	return time.Duration(attempt) * time.Second
}

// fakeTunnelServer answers connect attempts with the scripted steps: an
// HTTP status with a problem title, or an accepted WebSocket closed with a
// code. It records the session header and token of each attempt.
type step struct {
	status     int
	title      string
	retryAfter string
	closeCode  websocket.StatusCode
	reason     string
}

func fakeTunnelServer(t *testing.T, steps []step) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var sessions []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path != "/api/v1/tunnels/tid/connect" || r.Header.Get("Authorization") != "Bearer rdp_test" {
			mu.Unlock()
			http.Error(w, "unexpected request", http.StatusTeapot)
			return
		}
		sessions = append(sessions, r.Header.Get(api.TunnelSessionHeader))
		s := steps[min(n, len(steps)-1)]
		n++
		mu.Unlock()
		if s.status != 0 {
			if s.retryAfter != "" {
				w.Header().Set("Retry-After", s.retryAfter)
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(s.status)
			json.NewEncoder(w).Encode(map[string]any{"title": s.title, "detail": "scripted " + s.title, "status": s.status})
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.Close(s.closeCode, s.reason)
	}))
	t.Cleanup(srv.Close)
	return srv, &sessions
}

func TestRunTunnelReconnectsWithBackoff(t *testing.T) {
	srv, sessions := fakeTunnelServer(t, []step{
		{status: 503, title: "Service Unavailable"},
		{status: 409, title: api.CodeTunnelInUse, retryAfter: "7"},
		{closeCode: websocket.StatusGoingAway, reason: "server restarting"},
		{closeCode: api.TunnelCloseReplaced, reason: "replaced"},
		{closeCode: api.TunnelCloseEnded, reason: "ended by an administrator"},
	})
	c := New(srv.URL, "rdp_test")
	retrier := &countingRetrier{}
	var events []TunnelEvent
	err := c.RunTunnel(context.Background(), TunnelOptions{ID: "tid", Target: &TunnelTarget{Host: "localhost:1"},
		Clock: &stepClock{now: time.Unix(1_800_000_000, 0), step: 10 * time.Second}, Retrier: retrier,
		OnEvent: func(e TunnelEvent) { events = append(events, e) }})
	var ended *TunnelEndedError
	if !errors.As(err, &ended) || ended.Reason != "ended by an administrator" {
		t.Fatalf("RunTunnel: %v, want the server's end", err)
	}
	var got []string
	for _, e := range events {
		got = append(got, string(e.Kind)+" "+e.Delay.String())
	}
	// The 409's Retry-After (7s) beats the backoff (2s); short connections
	// keep growing the backoff.
	want := []string{"retry 1s", "retry 7s", "online 0s", "offline 3s", "online 0s", "offline 4s", "online 0s"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
	if len(*sessions) != 5 {
		t.Fatalf("%d attempts", len(*sessions))
	}
	for _, s := range *sessions {
		if s != (*sessions)[0] || len(s) != 32 {
			t.Fatalf("session values %v: want one random value per process", *sessions)
		}
	}
}

// A connection that lasted a minute resets the backoff.
func TestRunTunnelResetsBackoffAfterAStableConnection(t *testing.T) {
	srv, _ := fakeTunnelServer(t, []step{
		{status: 502, title: "Bad Gateway"},
		{closeCode: websocket.StatusGoingAway},
		{closeCode: api.TunnelCloseEnded, reason: "the link was revoked"},
	})
	retrier := &countingRetrier{}
	err := New(srv.URL, "rdp_test").RunTunnel(context.Background(), TunnelOptions{ID: "tid", Target: &TunnelTarget{Host: "localhost:1"},
		Clock: &stepClock{now: time.Unix(1_800_000_000, 0), step: 2 * time.Minute}, Retrier: retrier})
	var ended *TunnelEndedError
	if !errors.As(err, &ended) {
		t.Fatal(err)
	}
	if got := retrier.attempts; len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("backoff attempts %v, want 1 then 1 again", got)
	}
}

func TestRunTunnelStopsOnPermanentErrors(t *testing.T) {
	for _, s := range []step{
		{status: 401, title: "Unauthorized"},
		{status: 403, title: "Forbidden"},
		{status: 404, title: "Not Found"},
		{status: 400, title: "Bad Request"},
		{status: 410, title: "Gone"},
		{status: 429, title: api.CodeTunnelLimit},
	} {
		srv, sessions := fakeTunnelServer(t, []step{s})
		err := New(srv.URL, "rdp_test").RunTunnel(context.Background(), TunnelOptions{ID: "tid", Target: &TunnelTarget{Host: "localhost:1"},
			Clock: &stepClock{}, Retrier: &countingRetrier{}})
		if StatusOf(err) != s.status || len(*sessions) != 1 {
			t.Errorf("%d %s: %v after %d attempts", s.status, s.title, err, len(*sessions))
		}
	}
}

func TestRunTunnelStopsWithTheContext(t *testing.T) {
	srv, _ := fakeTunnelServer(t, []step{{status: 503}})
	ctx, cancel := context.WithCancel(context.Background())
	retries := 0
	err := New(srv.URL, "rdp_test").RunTunnel(ctx, TunnelOptions{ID: "tid", Target: &TunnelTarget{Host: "localhost:1"},
		Clock: &stepClock{}, Retrier: &countingRetrier{}, OnEvent: func(TunnelEvent) {
			if retries++; retries == 3 {
				cancel()
			}
		}})
	if err != nil {
		t.Fatalf("cancelled: %v, want nil", err)
	}
}

// TestTunnelLogShowsTheFinalStatusAndASafePath: --log reports the final
// status (not a 103 before it), and the path as it was on the wire, so a
// visitor can't write terminal escapes or fake lines to the owner's
// terminal.
func TestTunnelLogShowsTheFinalStatusAndASafePath(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		io.WriteString(w, "ok")
	}))
	defer app.Close()
	tg, err := ResolveTunnelTarget(context.Background(), strings.TrimPrefix(app.URL, "http://"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	logged := make(chan TunnelRequest, 1)
	srv := httptest.NewServer(logTunnelRequests(tg.proxy(), func(r TunnelRequest) { logged <- r }))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/x%1b%5d0;PWNED%07%1b%5b2J%0aGET%20/fake%20200")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-logged
	if resp.StatusCode != 200 || got.Status != 200 {
		t.Fatalf("visitor got %d; --log recorded %d", resp.StatusCode, got.Status)
	}
	for _, r := range got.Path {
		if r < 0x20 || r == 0x7f || r > 0x7e {
			t.Fatalf("--log path %q holds %U", got.Path, r)
		}
	}
	if !strings.Contains(got.Path, "%1B") && !strings.Contains(got.Path, "%1b") {
		t.Fatalf("--log path %q: want the escape percent-encoded", got.Path)
	}
}

// TestLinkLocalTargetKeepsItsZone: a link-local IPv6 address can only be
// dialed with its zone; the Host header has none.
func TestLinkLocalTargetKeepsItsZone(t *testing.T) {
	tg, err := ResolveTunnelTarget(context.Background(), "[fe80::1%lo]:3000", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if z := tg.Addrs[0].Addr().Zone(); z != "lo" || tg.Host != "[fe80::1]:3000" || tg.Class != ClassPrivate {
		t.Fatalf("pinned %s (zone %q), host %q, class %q", tg.Addrs[0], z, tg.Host, tg.Class)
	}
}
