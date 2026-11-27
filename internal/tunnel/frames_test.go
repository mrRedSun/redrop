package tunnel

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeConn reads from r and keeps what is written; the guard uses nothing
// else.
type fakeConn struct {
	net.Conn
	r  io.Reader
	mu sync.Mutex
	w  bytes.Buffer
}

func (c *fakeConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Write(p)
}

func (c *fakeConn) written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.w.Bytes()...)
}

// testGuard is a guard over in, with a clock the test moves.
type testGuard struct {
	*frameGuard
	conn       *fakeConn
	clock      atomic.Int64 // Unix nanoseconds
	violations []string     // written by the reader
}

func newTestGuard(in io.Reader) *testGuard {
	tg := &testGuard{conn: &fakeConn{r: in}}
	tg.clock.Store(time.Unix(1_000_000, 0).UnixNano())
	tg.frameGuard = newFrameGuard(tg.conn, 256<<10, func() time.Time { return time.Unix(0, tg.clock.Load()) },
		func(reason string) { tg.violations = append(tg.violations, reason) })
	return tg
}

// readAll reads through the guard as the session would, until it fails.
func (tg *testGuard) readAll() ([]byte, error) {
	var out []byte
	buf := make([]byte, 7) // small reads, so frames and injections split
	for {
		n, err := tg.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
}

func dataFrame(id uint32, payload []byte) []byte {
	return append(frameHeader(frameData, 0, id, uint32(len(payload))), payload...)
}

func ping(id uint32) []byte { return frameHeader(framePing, frameFlagSYN, 0, id) }

func TestGuardLetsAKeepalivePass(t *testing.T) {
	r, w := io.Pipe()
	tg := newTestGuard(r)
	done := make(chan error, 1)
	go func() { _, err := tg.readAll(); done <- err }()
	// A burst at once, then a ping every 30 seconds for an hour and more.
	for i := range uint32(pingBurst) {
		w.Write(ping(i))
	}
	for i := range uint32(150) {
		tg.clock.Add(int64(keepAlive))
		w.Write(ping(100 + i))
	}
	w.Close()
	if err := <-done; err != io.EOF || len(tg.violations) != 0 {
		t.Fatalf("keepalive pings: %v, violations %v", err, tg.violations)
	}
}

func TestGuardEndsAPingFlood(t *testing.T) {
	var in []byte
	for i := range uint32(pingBurst + 1) {
		in = append(in, ping(i)...)
	}
	tg := newTestGuard(bytes.NewReader(in))
	got, err := tg.readAll()
	if err != errProtocol || len(tg.violations) != 1 || !strings.Contains(tg.violations[0], "too many pings") {
		t.Fatalf("%d pings at once: %v, violations %v", pingBurst+1, err, tg.violations)
	}
	if len(got) >= len(in) {
		t.Fatalf("the session read the ping past the budget")
	}
	if _, err := tg.Read(make([]byte, 64)); err != errProtocol {
		t.Fatalf("read after the violation: %v", err)
	}
}

// TestGuardInjectsResetsBetweenFrames: a reset reaches the session at a
// frame boundary, never inside the CLI's frame, and the CLI gets one too.
func TestGuardInjectsResetsBetweenFrames(t *testing.T) {
	first := dataFrame(3, bytes.Repeat([]byte("a"), 20))
	second := dataFrame(5, []byte("bbbb"))
	r, w := io.Pipe()
	tg := newTestGuard(r)
	go func() {
		w.Write(first[:15]) // the header and part of the payload
		for !tg.resetPending() {
			time.Sleep(time.Millisecond)
		}
		w.Write(first[15:])
		w.Write(second)
		w.Close()
	}()
	buf := make([]byte, 64)
	n, _ := tg.Read(buf)
	if n != 15 {
		t.Fatalf("first read: %d bytes", n)
	}
	tg.resetStream(3)
	rest, err := tg.readAll()
	if err != io.EOF {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), first[15:]...), frameHeader(frameWindow, frameFlagRST, 3, 0)...), second...)
	if !bytes.Equal(rest, want) {
		t.Fatalf("the session read\n%q\nwant\n%q", rest, want)
	}
	waitFor(t, "the reset to the CLI", func() bool {
		return bytes.Equal(tg.conn.written(), frameHeader(frameWindow, frameFlagRST, 3, 0))
	})
}

func (g *frameGuard) resetPending() bool { return g.injecting() }

// TestGuardSendsBetweenTheSessionsFrames: the reset to the CLI never lands
// between a data frame's header and its payload.
func TestGuardSendsBetweenTheSessionsFrames(t *testing.T) {
	tg := newTestGuard(strings.NewReader(""))
	hdr := frameHeader(frameData, 0, 7, 4)
	tg.Write(hdr)
	tg.resetStream(9)
	time.Sleep(20 * time.Millisecond) // the reset waits for the payload
	tg.Write([]byte("data"))
	want := append(append(append([]byte(nil), hdr...), "data"...), frameHeader(frameWindow, frameFlagRST, 9, 0)...)
	waitFor(t, "the reset to the CLI", func() bool { return bytes.Equal(tg.conn.written(), want) })
	tg.stop()
}

// TestGuardEndsACLIThatIgnoresAReset: data the CLI had in flight on a reset
// stream is fine, up to a window; more is a CLI that ignored the reset.
func TestGuardEndsACLIThatIgnoresAReset(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	var in []byte
	for range 8 { // a whole window: in flight before the reset arrived
		in = append(in, dataFrame(3, chunk)...)
	}
	in = append(in, dataFrame(5, chunk)...) // another stream: not counted
	allowed := len(in)
	in = append(in, dataFrame(3, []byte("more"))...)
	tg := newTestGuard(bytes.NewReader(in))
	tg.resetStream(3)
	got, err := tg.readAll()
	if err != errProtocol || len(tg.violations) != 1 || !strings.Contains(tg.violations[0], "ignored a stream reset") {
		t.Fatalf("data past the window on a reset stream: %v, %v", err, tg.violations)
	}
	// The session got the reset, then everything up to the extra data.
	if len(got) < frameHeaderSize+allowed {
		t.Fatalf("the session read %d bytes; a window of in-flight data counted as ignoring the reset", len(got))
	}
}

// rawConnect serves a link over a real WebSocket and returns the CLI's side
// as a raw byte stream, for a CLI that writes frames itself.
func rawConnect(t *testing.T, f *fixture, o Owner) (net.Conn, chan [2]any) {
	t.Helper()
	lease, err := f.reg.Reserve(o, unchanged(o))
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(-1)
		accepted <- c
	}))
	t.Cleanup(srv.Close)
	cws, _, err := websocket.Dial(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	cws.SetReadLimit(-1)
	t.Cleanup(func() { cws.CloseNow() })
	ws := within(t, "the WebSocket", accepted)
	closed := make(chan [2]any, 1)
	go lease.Serve(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), func(code int, reason string) {
		closed <- [2]any{code, reason}
		ws.Close(websocket.StatusCode(code), reason)
	})
	waitFor(t, "the tunnel to come online", func() bool { return f.reg.Online(o.ArtifactID) })
	return websocket.NetConn(context.Background(), cws, websocket.MessageBinary), closed
}

// TestPingFloodEndsTheConnection: a modified CLI that floods pings, reading
// the answers or not, is ended at once, and the server's goroutines and
// heap stay where they were (yamux starts one goroutine per ping).
func TestPingFloodEndsTheConnection(t *testing.T) {
	for _, reads := range []bool{false, true} {
		t.Run(map[bool]string{false: "never reads", true: "reads the answers"}[reads], func(t *testing.T) {
			f := newFixture(t, testLimits(), nil)
			before := runtime.NumGoroutine()
			cli, closed := rawConnect(t, f, owner("t1", "u1"))
			if reads {
				go io.Copy(io.Discard, cli)
			}
			var flood []byte
			for i := range uint32(100_000) {
				flood = append(flood, ping(i)...)
			}
			go cli.Write(flood)
			deadline := time.Now().Add(10 * time.Second) // only catches a real hang
			var c [2]any
		wait:
			for {
				select {
				case c = <-closed:
					break wait
				case <-time.After(time.Millisecond):
					if g := runtime.NumGoroutine(); g > before+100 {
						t.Fatalf("goroutines rose from %d to %d during the flood", before, g)
					}
					if time.Now().After(deadline) {
						t.Fatal("the connection was not ended")
					}
				}
			}
			if c[0].(int) != websocketPolicy || !strings.Contains(c[1].(string), "too many pings") {
				t.Fatalf("ended with %v", c)
			}
			waitFor(t, "the tunnel to go offline", func() bool { return !f.reg.Online("t1") })
		})
	}
}
