package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/pkg/client"
)

// The fixture runs the registry behind a visitor-facing test server, a CLI
// peer (client.ServeTunnelSession, the real agent) and a local test app.
// Visitors pick their tunnel with X-Test-Tunnel and their access
// generation with X-Test-Gen.

func testLimits() config.Tunnels {
	return config.Tunnels{PerUser: 3, Streams: 16, StreamsTotal: 64, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		BytesPerHour: 1 << 30, ResponseTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second}
}

// waitFor polls cond until it holds. The bound only catches a real hang.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// within waits for a channel value. The bound only catches a real hang.
func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func testPage(w http.ResponseWriter, status int, heading, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, heading+": "+body)
}

type fixture struct {
	t     *testing.T
	reg   *Registry
	front *httptest.Server
	app   *httptest.Server
}

func newFixture(t *testing.T, lim config.Tunnels, app http.Handler) *fixture {
	t.Helper()
	f := &fixture{t: t, reg: New(lim, slog.New(slog.DiscardHandler))}
	t.Cleanup(func() {
		f.reg.Shutdown(5 * time.Second)
		f.reg.Close()
	})
	if app != nil {
		f.app = httptest.NewServer(app)
		t.Cleanup(f.app.Close)
	}
	f.front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Test-Tunnel")
		if id == "" {
			id = "t1"
		}
		gen, _ := strconv.Atoi(r.Header.Get("X-Test-Gen"))
		v := Visit{ArtifactID: id, AccessGen: gen, ClientIP: "192.0.2.1", Proto: "https",
			Reserved: func(name string) bool { return name == "__Host-rd_visit" }, Strip: []string{"CF-IPCountry"}, Page: testPage}
		if !f.reg.Serve(w, r, v) {
			OfflinePage(w, testPage)
		}
	}))
	t.Cleanup(f.front.Close)
	return f
}

func owner(id, user string) Owner {
	return Owner{ArtifactID: id, UserID: user, Username: user, TokenID: "tok-" + user, Client: "cli-" + id, AccessGen: 0}
}

// peer is one connected CLI.
type peer struct {
	closed chan [2]any // the close code and reason the registry sent
	done   chan error  // ServeTunnelSession's result
	stop   context.CancelFunc
}

func (p *peer) closeCode(t *testing.T) (int, string) {
	t.Helper()
	c := within(t, "the connection to close", p.closed)
	return c[0].(int), c[1].(string)
}

// connect connects a CLI for o over an in-memory pipe and waits until the
// tunnel is online.
func (f *fixture) connect(o Owner) *peer {
	f.t.Helper()
	p := f.start(o)
	waitFor(f.t, "the tunnel to come online", func() bool { return f.reg.Online(o.ArtifactID) })
	return p
}

// start connects like connect without waiting for the tunnel to be online
// (it may end at once).
func (f *fixture) start(o Owner) *peer {
	f.t.Helper()
	lease, err := f.reg.Reserve(o)
	if err != nil {
		f.t.Fatal(err)
	}
	server, cli := net.Pipe()
	return f.serveWith(lease, server, cli, func(int, string) { server.Close() })
}

// connectWS connects over a real WebSocket, as in production.
func (f *fixture) connectWS(o Owner) *peer {
	f.t.Helper()
	lease, err := f.reg.Reserve(o)
	if err != nil {
		f.t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	var ws *websocket.Conn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws = c
		accepted <- websocket.NetConn(context.Background(), c, websocket.MessageBinary)
		<-r.Context().Done()
	}))
	f.t.Cleanup(srv.Close)
	cws, _, err := websocket.Dial(context.Background(), srv.URL, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	server := within(f.t, "the WebSocket", accepted)
	p := f.serveWith(lease, server, websocket.NetConn(context.Background(), cws, websocket.MessageBinary), func(code int, reason string) {
		ws.Close(websocket.StatusCode(code), reason)
	})
	waitFor(f.t, "the tunnel to come online", func() bool { return f.reg.Online(o.ArtifactID) })
	return p
}

func (f *fixture) serveWith(lease *Lease, server, cli net.Conn, closeWS func(int, string)) *peer {
	f.t.Helper()
	p := &peer{closed: make(chan [2]any, 1), done: make(chan error, 1)}
	go lease.Serve(server, func(code int, reason string) {
		p.closed <- [2]any{code, reason}
		closeWS(code, reason)
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.stop = cancel
	target := f.target()
	go func() { p.done <- client.ServeTunnelSession(ctx, cli, target, nil) }()
	f.t.Cleanup(func() {
		cancel()
		within(f.t, "the CLI to stop", p.done)
	})
	return p
}

func (f *fixture) target() *client.TunnelTarget {
	f.t.Helper()
	if f.app == nil {
		return &client.TunnelTarget{Host: "localhost:1"}
	}
	tg, err := client.ResolveTunnelTarget(context.Background(), strings.TrimPrefix(f.app.URL, "http://"), false, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return tg
}

// request sends a visitor's request for tunnel id.
func (f *fixture) request(ctx context.Context, method, id, path string, body io.Reader, hdr ...string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, f.front.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Test-Tunnel", id)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return f.front.Client().Do(req)
}

// get sends a GET and returns the status and body.
func (f *fixture) get(id, path string, hdr ...string) (int, string) {
	f.t.Helper()
	r, err := f.request(context.Background(), "GET", id, path, nil, hdr...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}
