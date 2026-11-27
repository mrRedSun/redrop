package tunnel

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

// The yamux v0.1.2 frame layout: a 12-byte header (version, type, flags,
// stream ID, length). Only data frames carry a payload, of that length.
const (
	frameHeaderSize = 12
	frameData       = 0
	frameWindow     = 1
	framePing       = 2
	frameFlagSYN    = 1
	frameFlagRST    = 8
)

// The CLI pings every 30 seconds (keepAlive). yamux v0.1.2 answers each
// ping in a goroutine of its own, which may wait 30 seconds to send, so a
// ping flood is a goroutine flood: 400,000 pings, under 5 MB, took a
// 768 MB server down. pingBurst pings at once, then one per pingEvery,
// is far more than an honest CLI sends; past that the connection ends.
const (
	pingBurst = 10
	pingEvery = time.Second
)

// resetMemory is how long the guard remembers a stream it reset, to tell
// the data the CLI had in flight from data it sends after the reset.
const resetMemory = 2 * time.Minute

// errProtocol is what the session reads once the CLI broke the protocol.
var errProtocol = errors.New("tunnel protocol violation")

// frameGuard is the CLI's connection as the server's yamux session sees
// it. It follows the frames in both directions, reading their headers
// only, and:
//   - ends the connection when the CLI pings faster than any keepalive;
//   - resets one stream on request (resetStream). yamux v0.1.2 has no
//     reset of its own, so the guard hands the session a reset frame as if
//     the CLI had sent one, between two of the CLI's frames, and sends one
//     to the CLI between two of the session's;
//   - ends the connection when the CLI goes on sending data on a stream it
//     was told to reset, past the window it may have had in flight.
//
// violation is called once, from the session's receive loop, before the
// session's read fails.
type frameGuard struct {
	net.Conn
	window    uint32 // a stream's receive window
	now       func() time.Time
	violation func(reason string)

	// What the CLI sends. Only the session's receive loop (Read) uses
	// these.
	hdr     [frameHeaderSize]byte
	hn      int    // header bytes so far
	payload uint32 // data payload bytes still to come
	dataID  uint32 // the stream they are for
	tokens  float64
	refill  time.Time
	rest    []byte // read from the connection, not yet passed on
	readErr error
	failed  bool

	mu     sync.Mutex
	inject []byte // frames for the session, passed on at the next frame boundary
	resets map[uint32]*resetStream

	// What the session sends.
	wmu      sync.Mutex
	wready   *sync.Cond // signalled at frame boundaries and at stop
	whdr     [frameHeaderSize]byte
	whn      int
	wpayload uint32
	stopped  bool
}

// resetStream is a stream the guard reset, and the payload the CLI sent on
// it since.
type resetStream struct {
	at   time.Time
	data uint64
}

func newFrameGuard(c net.Conn, window uint32, now func() time.Time, violation func(string)) *frameGuard {
	g := &frameGuard{Conn: c, window: window, now: now, violation: violation,
		tokens: pingBurst, refill: now(), resets: map[uint32]*resetStream{}}
	g.wready = sync.NewCond(&g.wmu)
	return g
}

func frameHeader(typ byte, flags uint16, id, length uint32) []byte {
	h := make([]byte, frameHeaderSize)
	h[1] = typ
	binary.BigEndian.PutUint16(h[2:4], flags)
	binary.BigEndian.PutUint32(h[4:8], id)
	binary.BigEndian.PutUint32(h[8:12], length)
	return h
}

func (g *frameGuard) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if g.failed {
			return 0, errProtocol
		}
		if n := g.takeInjected(p); n > 0 {
			return n, nil
		}
		var n int
		switch {
		case len(g.rest) > 0:
			n = copy(p, g.rest)
			g.rest = g.rest[n:]
		case g.readErr != nil:
			return 0, g.readErr
		default:
			n, g.readErr = g.Conn.Read(p)
			if n == 0 {
				continue
			}
		}
		k, reason := g.scan(p[:n])
		if reason != "" {
			g.failed = true
			g.violation(reason)
			return 0, errProtocol
		}
		if k < n { // a frame is waiting to be injected at this boundary
			g.rest = append(append([]byte(nil), p[k:n]...), g.rest...)
		}
		if k > 0 {
			return k, nil
		}
	}
}

// takeInjected copies frames waiting for the session into p, at a frame
// boundary of what the CLI sends.
func (g *frameGuard) takeInjected(p []byte) int {
	if g.hn != 0 || g.payload != 0 {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := copy(p, g.inject)
	g.inject = g.inject[n:]
	return n
}

func (g *frameGuard) injecting() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inject) > 0
}

// scan follows the CLI's frames through b. It returns how many bytes the
// session may have now: all of b, or up to the first frame boundary when a
// frame is waiting to be injected there. reason is set when the CLI broke
// the protocol.
func (g *frameGuard) scan(b []byte) (n int, reason string) {
	for n < len(b) {
		if g.payload > 0 {
			m := uint32(min(len(b)-n, int(min(g.payload, 1<<30))))
			if r := g.data(g.dataID, m); r != "" {
				return n, r
			}
			g.payload -= m
			n += int(m)
			continue
		}
		if g.hn == 0 && g.injecting() {
			return n, ""
		}
		m := copy(g.hdr[g.hn:], b[n:])
		g.hn += m
		n += m
		if g.hn < frameHeaderSize {
			continue
		}
		g.hn = 0
		if r := g.frame(); r != "" {
			return n, r
		}
	}
	return n, ""
}

// frame checks one complete header from the CLI.
func (g *frameGuard) frame() string {
	typ, flags := g.hdr[1], binary.BigEndian.Uint16(g.hdr[2:4])
	switch typ {
	case frameData:
		g.dataID, g.payload = binary.BigEndian.Uint32(g.hdr[4:8]), binary.BigEndian.Uint32(g.hdr[8:12])
	case framePing:
		if flags&frameFlagSYN != 0 && !g.allowPing() {
			return "the CLI sent too many pings"
		}
	}
	return ""
}

// allowPing takes a token for a ping from the CLI: pingBurst at most, one
// more per pingEvery.
func (g *frameGuard) allowPing() bool {
	now := g.now()
	g.tokens = min(pingBurst, g.tokens+float64(now.Sub(g.refill))/float64(pingEvery))
	g.refill = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

// data counts n payload bytes the CLI sent on stream id.
func (g *frameGuard) data(id, n uint32) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.resets[id]
	if r == nil {
		return ""
	}
	r.data += uint64(n)
	if r.data > uint64(g.window) {
		return "the CLI ignored a stream reset"
	}
	return ""
}

// resetStream resets stream id: the session forgets it, with whatever it
// buffered, as soon as the next frame from the CLI arrives, and the CLI is
// told to reset it too. The caller makes sure a frame from the CLI comes
// (a ping is answered).
func (g *frameGuard) resetStream(id uint32) {
	rst := frameHeader(frameWindow, frameFlagRST, id, 0)
	g.mu.Lock()
	now := g.now()
	for k, r := range g.resets {
		if now.Sub(r.at) > resetMemory {
			delete(g.resets, k)
		}
	}
	g.resets[id] = &resetStream{at: now}
	g.inject = append(g.inject, rst...)
	g.mu.Unlock()
	go g.send(rst)
}

func (g *frameGuard) Write(p []byte) (int, error) {
	g.wmu.Lock()
	defer g.wmu.Unlock()
	n, err := g.Conn.Write(p)
	g.scanOut(p[:n])
	if g.whn == 0 && g.wpayload == 0 {
		g.wready.Broadcast()
	}
	return n, err
}

// scanOut follows the session's frames, so send writes between two.
func (g *frameGuard) scanOut(b []byte) {
	for len(b) > 0 {
		if g.wpayload > 0 {
			m := uint32(min(len(b), int(min(g.wpayload, 1<<30))))
			g.wpayload -= m
			b = b[m:]
			continue
		}
		m := copy(g.whdr[g.whn:], b)
		g.whn += m
		b = b[m:]
		if g.whn < frameHeaderSize {
			continue
		}
		g.whn = 0
		if g.whdr[1] == frameData {
			g.wpayload = binary.BigEndian.Uint32(g.whdr[8:12])
		}
	}
}

// send writes a frame to the CLI between two of the session's frames.
func (g *frameGuard) send(frame []byte) {
	g.wmu.Lock()
	defer g.wmu.Unlock()
	for !g.stopped && (g.whn != 0 || g.wpayload != 0) {
		g.wready.Wait()
	}
	if !g.stopped {
		g.Conn.Write(frame) // an error means the connection is ending anyway
	}
}

// stop releases sends still waiting once the session has ended.
func (g *frameGuard) stop() {
	g.wmu.Lock()
	g.stopped = true
	g.wready.Broadcast()
	g.wmu.Unlock()
}
