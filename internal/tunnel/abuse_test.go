package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mrRedSun/redrop/internal/apperr"
)

// TestCLIOpenedStreamEndsTheConnection: only the server opens streams. A
// modified CLI that opens its own (and fills them) would make the server
// hold a window of data on each, outside every limit; the first one ends
// the connection instead.
func TestCLIOpenedStreamEndsTheConnection(t *testing.T) {
	f := newFixture(t, testLimits(), nil)
	o := owner("t1", "u1")
	lease, err := f.reg.Reserve(o, unchanged(o))
	if err != nil {
		t.Fatal(err)
	}
	server, cli := net.Pipe()
	closed := make(chan [2]any, 1)
	go lease.Serve(server, func(code int, reason string) {
		closed <- [2]any{code, reason}
		server.Close()
	})
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	hostile, err := yamux.Server(cli, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer hostile.Close()
	waitFor(t, "the tunnel to come online", func() bool { return f.reg.Online("t1") })
	sess := f.reg.connected("t1").session()
	go func() {
		buf := make([]byte, 256<<10)
		for range 64 {
			st, err := hostile.OpenStream()
			if err != nil {
				return
			}
			go st.Write(buf)
		}
	}()
	c := within(t, "the connection to end", closed)
	if c[0].(int) != websocketPolicy || !strings.Contains(c[1].(string), "opened a stream") {
		t.Fatalf("ended with %d %q", c[0], c[1])
	}
	waitFor(t, "the session to close", sess.IsClosed)
	if f.reg.Online("t1") {
		t.Fatal("still online")
	}
}

// readResponses reads responses from br until a final (non-1xx) one,
// returning the informational status codes before it.
func readResponses(t *testing.T, br *bufio.Reader) ([]int, *http.Response) {
	t.Helper()
	var interim []int
	for {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode >= 200 {
			return interim, resp
		}
		interim = append(interim, resp.StatusCode)
	}
}

// wantContentHeaders checks that the content host's headers reached the
// final response, as they are for a tunnel (no Permissions-Policy).
func wantContentHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for k, v := range contentHeaders {
		if k == "Permissions-Policy" {
			v = ""
		}
		if got := strings.Join(h.Values(k), ","); got != v {
			t.Errorf("%s: %s %q, want %q", what, k, got, v)
		}
	}
}

// TestInformationalResponsesKeepTheHeaders: a 1xx before the final response
// (103 Early Hints from the app, 100 Continue for a visitor's Expect) must
// not take the content host's headers off the final response. 1xx from the
// app are not passed on; the server answers Expect itself.
func TestInformationalResponsesKeepTheHeaders(t *testing.T) {
	sawExpect := make(chan string, 1)
	f := newFixture(t, testLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/early":
			w.Header().Set("Link", "</app.css>; rel=preload; as=style")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Set("Content-Type", "text/javascript")
			io.WriteString(w, "console.log(1)")
		case "/upload":
			b, _ := io.ReadAll(r.Body)
			sawExpect <- r.Header.Get("Expect")
			io.WriteString(w, "got "+string(b))
		}
	}))
	f.connect(owner("t1", "u1"))

	var interim []int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		interim = append(interim, code)
		return nil
	}}
	r, err := f.request(httptrace.WithClientTrace(context.Background(), trace), "GET", "t1", "/early", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || string(b) != "console.log(1)" || len(interim) != 0 {
		t.Fatalf("103 from the app: status %d body %q, interim %v (want none)", r.StatusCode, b, interim)
	}
	wantContentHeaders(t, "after a 103", r.Header)

	// A visitor's Expect: 100-continue, raw so every interim answer shows.
	c, err := net.Dial("tcp", f.front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	io.WriteString(c, "POST /upload HTTP/1.1\r\nHost: x\r\nX-Test-Tunnel: t1\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\n")
	cont, err := http.ReadResponse(br, nil)
	if err != nil || cont.StatusCode != http.StatusContinue {
		t.Fatalf("waiting to send the body: %v %v", cont, err)
	}
	io.WriteString(c, "hello")
	more, resp := readResponses(t, br)
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "got hello" || len(more) != 0 {
		t.Fatalf("Expect: status %d body %q, %v more 1xx after the server's 100 (want none)", resp.StatusCode, b, more)
	}
	wantContentHeaders(t, "after 100 Continue", resp.Header)
	if got := within(t, "the app's request", sawExpect); got != "" {
		t.Fatalf("the app got Expect: %q", got)
	}
}

// TestStalledVisitorReleasesTheSlots: a visitor who stops reading can hold
// its request, stream and slots only for the idle timeout, for a response
// and for a WebSocket alike, and the admin view counts the stream meanwhile.
func TestStalledVisitorReleasesTheSlots(t *testing.T) {
	for _, tc := range []struct{ name, request string }{
		{"response", "GET /big HTTP/1.1\r\nHost: x\r\nX-Test-Tunnel: t1\r\n\r\n"},
		{"websocket", "GET /ws HTTP/1.1\r\nHost: x\r\nX-Test-Tunnel: t1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lim := testLimits()
			lim.Streams, lim.IdleTimeout, lim.ResponseTimeout, lim.MaxResponseBytes = 1, 200*time.Millisecond, 300*time.Millisecond, 1<<40
			ended := make(chan struct{}, 1)
			chunk := make([]byte, 32<<10)
			f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/quick":
					io.WriteString(w, "quick")
				case "/big":
					for {
						if _, err := w.Write(chunk); err != nil {
							break
						}
					}
					ended <- struct{}{}
				case "/ws":
					c, brw, err := http.NewResponseController(w).Hijack()
					if err != nil {
						return
					}
					defer c.Close()
					brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
					brw.Flush()
					for {
						if _, err := c.Write(chunk); err != nil {
							break
						}
					}
					ended <- struct{}{}
				}
			}))
			f.connect(owner("t1", "u1"))
			c, err := net.Dial("tcp", f.front.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.(*net.TCPConn).SetReadBuffer(4096)
			io.WriteString(c, tc.request)
			c.Read(make([]byte, 64)) // the start of the answer, then nothing
			waitFor(t, "the admin view to count the stream", func() bool {
				got := f.reg.Connected()
				return len(got) == 1 && got[0].Streams == 1
			})
			waitFor(t, "the stalled visitor's slot to be released", func() bool {
				status, _ := f.get("t1", "/quick")
				return status == 200
			})
			within(t, "the app's side to end", ended)
		})
	}
}

// TestAbandonedDownloadReleasesItsStream: a visitor who leaves while the
// stream's window is full must not leave the stream, its unread buffer and
// a CLI stuck writing behind; the slot is held until they are gone.
func TestAbandonedDownloadReleasesItsStream(t *testing.T) {
	lim := testLimits()
	lim.MaxResponseBytes = 1 << 40
	var lastWrite atomic.Int64
	ended := make(chan struct{}, 1)
	chunk := make([]byte, 32<<10)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := w.Write(chunk); err != nil {
				break
			}
			lastWrite.Store(time.Now().UnixNano())
		}
		ended <- struct{}{}
	}))
	f.connectWS(owner("t1", "u1"))
	sess := f.reg.connected("t1").session()
	c, err := net.Dial("tcp", f.front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).SetReadBuffer(4096)
	io.WriteString(c, "GET /stream HTTP/1.1\r\nHost: x\r\nX-Test-Tunnel: t1\r\n\r\n")
	c.Read(make([]byte, 64))
	// Every buffer on the way fills, the stream's window last: the app can
	// write no more.
	waitFor(t, "the app to be blocked", func() bool {
		last := lastWrite.Load()
		return last != 0 && time.Since(time.Unix(0, last)) > 300*time.Millisecond
	})
	c.Close() // the visitor leaves
	within(t, "the app's request to end", ended)
	waitFor(t, "the stream to be released", func() bool { return sess.NumStreams() == 0 })
	waitFor(t, "the slots to be released", func() bool { return len(f.reg.slots) == 0 })
	if !f.reg.Online("t1") {
		t.Fatal("an honest CLI's connection must stay up")
	}
}

// TestOneBudgetForSlotAndHeaders: a request gets the response timeout once,
// for waiting for a slot and for the app's headers together; behind
// Cloudflare (100 s) the two must not add up.
func TestOneBudgetForSlotAndHeaders(t *testing.T) {
	lim := testLimits()
	lim.Streams, lim.ResponseTimeout = 1, time.Second
	hold := make(chan struct{})
	holding := make(chan struct{}, 1)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hold":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			holding <- struct{}{}
			<-hold
		case "/slow":
			select {
			case <-time.After(700 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, "late")
		}
	}))
	f.connect(owner("t1", "u1"))
	first, err := f.request(context.Background(), "GET", "t1", "/hold", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	within(t, "the slot to be taken", holding)
	time.AfterFunc(700*time.Millisecond, func() { close(hold) })
	// About 700 ms waiting for the slot, then the app needs 700 ms more.
	if status, body := f.get("t1", "/slow"); status != http.StatusGatewayTimeout || !strings.Contains(body, "too long") {
		t.Fatalf("1.4 s against a 1 s budget: %d %q", status, body)
	}
}

// TestAccessChangeBeforeHeaders: a password change while a request waits
// for the app's headers answers with redrop's page, not with whatever the
// CLI sends when its request is cut.
func TestAccessChangeBeforeHeaders(t *testing.T) {
	entered := make(chan struct{}, 1)
	f := newFixture(t, testLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	f.connect(owner("t1", "u1"))
	type result struct {
		status int
		body   string
	}
	done := make(chan result, 1)
	go func() {
		r, err := f.request(context.Background(), "GET", "t1", "/", nil, "X-Test-Gen", "1")
		if err != nil {
			done <- result{-1, err.Error()}
			return
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		done <- result{r.StatusCode, string(b)}
	}()
	within(t, "the request", entered)
	f.reg.Update("t1", true, 2, nil, "")
	if r := within(t, "the answer", done); r.status != http.StatusServiceUnavailable || !strings.Contains(r.body, "Access changed") {
		t.Fatalf("access changed while waiting for the app: %d %q", r.status, r.body)
	}
}

// TestReserveChecksAgainAfterRegistering: a revoke, a disabled account or a
// deleted token that lands after the connect checks but before the link is
// registered finds no link to end; Reserve checks again once it is
// registered, and keeps the newest access.
func TestReserveChecksAgainAfterRegistering(t *testing.T) {
	f := newFixture(t, testLimits(), http.NotFoundHandler())
	o := owner("t1", "u1")
	f.reg.EndUser("u1") // the account is disabled before the link exists: nothing to end
	refused := apperr.Unauthorized("account disabled")
	if _, err := f.reg.Reserve(o, func() (Owner, error) { return Owner{}, refused }); !errors.Is(err, refused) {
		t.Fatalf("recheck failed: %v", err)
	}
	if f.reg.Online("t1") {
		t.Fatal("a refused reservation is registered")
	}
	// A password change in between: the link starts with the new generation.
	newer := o
	newer.AccessGen = 5
	lease, err := f.reg.Reserve(o, unchanged(newer))
	if err != nil {
		t.Fatal(err)
	}
	server, cli := net.Pipe()
	f.serveWith(lease, server, cli, func(int, string) { server.Close() })
	waitFor(t, "online", func() bool { return f.reg.Online("t1") })
	if status, body := f.get("t1", "/", "X-Test-Gen", "1"); status != http.StatusServiceUnavailable || !strings.Contains(body, "Access changed") {
		t.Fatalf("a request let in under the old generation: %d %q", status, body)
	}
	if status, _ := f.get("t1", "/", "X-Test-Gen", "5"); status != http.StatusNotFound {
		t.Fatalf("under the new generation: %d, want the app's 404", status)
	}
}

// TestByteBudgetIsPerUser: a fresh tunnel doesn't reset its owner's budget.
func TestByteBudgetIsPerUser(t *testing.T) {
	lim := testLimits()
	lim.BytesPerHour = 64 << 10
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 128<<10))
	}))
	f.connect(owner("t1", "u1"))
	f.connect(owner("t2", "u1"))
	f.connect(owner("t3", "u2"))
	r, err := f.request(context.Background(), "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if status, _ := f.get("t2", "/"); status != http.StatusTooManyRequests {
		t.Fatalf("the same owner's other tunnel: %d, want 429", status)
	}
	if status, _ := f.get("t3", "/"); status != 200 {
		t.Fatalf("another owner's tunnel: %d", status)
	}
}

// TestEndingALinkReleasesItsSlots: when a connection ends mid-stream, every
// slot its streams held goes with it.
func TestEndingALinkReleasesItsSlots(t *testing.T) {
	lim := testLimits()
	lim.MaxResponseBytes = 1 << 40
	writing := make(chan struct{}, 4)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 32<<10)
		writing <- struct{}{}
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	f.connectWS(owner("t1", "u1"))
	for range 3 {
		r, err := f.request(context.Background(), "GET", "t1", "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		within(t, "the download", writing)
	}
	if got := len(f.reg.slots); got != 3 {
		t.Fatalf("slots in use while downloading: %d", got)
	}
	f.reg.End("t1", WhyAdmin)
	waitFor(t, "the slots to be released", func() bool { return len(f.reg.slots) == 0 })
}

// TestStuckStreamIsResetNotTheConnection: while the CLI is still writing a
// request body to an app that hasn't read it, nothing on the CLI reads the
// stream, so it doesn't see the server close it. After the linger time the
// server resets that stream alone: its slot comes back, and the connection
// and another visitor's download go on.
func TestStuckStreamIsResetNotTheConnection(t *testing.T) {
	lim := testLimits()
	lim.MaxRequestBytes, lim.MaxResponseBytes, lim.IdleTimeout = 1<<30, 1<<40, 500*time.Millisecond
	release := make(chan struct{})
	appRead := make(chan error, 1)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload": // slow work (authentication, a queue) before reading the body
			<-release
			_, err := io.Copy(io.Discard, r.Body)
			appRead <- err
		case "/download":
			for {
				if _, err := w.Write(make([]byte, 1<<10)); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				time.Sleep(10 * time.Millisecond)
			}
		}
	}))
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) }) // before the app's server closes
	f.reg.linger = 300 * time.Millisecond
	p := f.connectWS(owner("t1", "u1"))
	sess := f.reg.connected("t1").session()

	dl, err := f.request(context.Background(), "GET", "t1", "/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	dlDone := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, dl.Body); dlDone <- err }()

	c, err := net.Dial("tcp", f.front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /upload HTTP/1.1\r\nHost: x\r\nX-Test-Tunnel: t1\r\nContent-Length: %d\r\n\r\n", 64<<20)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	}()
	waitFor(t, "the upload's stream", func() bool { return sess.NumStreams() == 2 })
	// It moves nothing once every buffer is full: the idle timeout closes
	// it, and the linger time later it is reset.
	waitFor(t, "the upload's slot to come back", func() bool { return len(f.reg.slots) == 1 })
	waitFor(t, "the upload's stream to be gone", func() bool { return sess.NumStreams() == 1 })
	select {
	case cl := <-p.closed:
		t.Fatalf("an honest CLI's connection was ended: %v", cl)
	case err := <-dlDone:
		t.Fatalf("the other visitor's download ended: %v", err)
	default:
	}
	if !f.reg.Online("t1") {
		t.Fatal("the tunnel went offline")
	}
	// The CLI was told: once the app reads, the rest of the body isn't there.
	once.Do(func() { close(release) })
	if err := within(t, "the app's read", appRead); err == nil {
		t.Fatal("the app got the whole body of a reset stream")
	}
}

// TestCLIThatKeepsWritingGetsTheStreamReset: a CLI that ignores the
// server's end and keeps writing has that stream reset after the linger
// time. yamux stops it there, so the connection stays.
func TestCLIThatKeepsWritingGetsTheStreamReset(t *testing.T) {
	lim := testLimits()
	lim.MaxResponseBytes = 1 << 40
	f := newFixture(t, lim, nil)
	f.reg.linger = 300 * time.Millisecond
	cli, closed := rawConnect(t, f, owner("t1", "u1"))
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	hs, err := yamux.Server(cli, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	go func() {
		for {
			st, err := hs.AcceptStream()
			if err != nil {
				return
			}
			go func() { // answers, and never stops writing
				io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: 1000000000000\r\n\r\n")
				buf := make([]byte, 32<<10)
				for {
					if _, err := st.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	r, err := f.request(ctx, "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Read(make([]byte, 100))
	cancel() // the visitor leaves
	r.Body.Close()
	waitFor(t, "the slots to come back", func() bool { return len(f.reg.slots) == 0 })
	// The connection carries the next request.
	r, err = f.request(context.Background(), "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if _, err := io.ReadFull(r.Body, make([]byte, 64<<10)); err != nil {
		t.Fatalf("after the reset: %v", err)
	}
	select {
	case c := <-closed:
		t.Fatalf("the connection was ended: %v", c)
	default:
	}
}
