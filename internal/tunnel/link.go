package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mrRedSun/redrop/pkg/api"
)

var (
	errOffline       = errors.New("tunnel offline")
	errBusy          = errors.New("tunnel busy")
	errAccessChanged = errors.New("access changed")
)

// lingerTimeout is how long the CLI has to close a stream the server
// closed (see stream.linger) before the server resets it. The CLI closes
// its end once it reads the server's end, normally within a round trip.
// It can take longer without the CLI being at fault: while the CLI is
// still writing a request body to an app that doesn't read it, nothing on
// the CLI reads the stream.
const lingerTimeout = 10 * time.Second

// maxResponseHeaderBytes bounds the response header block an app may send.
const maxResponseHeaderBytes = 256 << 10

// link is one tunnel's connection: reserved, then serving a yamux session
// until it ends. Its streams carry one HTTP/1.1 exchange each.
type link struct {
	reg       *Registry
	transport *http.Transport // dials by opening a stream
	slots     chan struct{}   // this tunnel's stream slots
	done      chan struct{}   // closed when the link ends

	owner Owner // fixed at Reserve; read without the lock

	mu        sync.Mutex
	expires   *time.Time // the artifact's expiry; changes with Update
	gen       int        // current access generation
	sess      *yamux.Session
	guard     *frameGuard // the connection as the session sees it
	connected time.Time
	streams   map[*stream]struct{} // open streams, for access changes
	waiting   int                  // requests waiting for a slot
	expiry    *time.Timer
	ended     bool
	code      int
	why       string
}

func newLink(r *Registry, o Owner) *link {
	l := &link{reg: r, owner: o, expires: o.Expires, gen: o.AccessGen, streams: map[*stream]struct{}{},
		slots: make(chan struct{}, r.lim.Streams), done: make(chan struct{})}
	// The time to the app's response headers is limited per request, by the
	// proxy (see proxy.headerTimer), not by ResponseHeaderTimeout: a request
	// gets the response timeout once, for waiting for a slot and for the
	// headers together.
	l.transport = &http.Transport{
		Proxy:                  nil,
		DialContext:            l.dial,
		DisableKeepAlives:      true, // one exchange per stream
		DisableCompression:     true, // pass the visitor's Accept-Encoding through
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	return l
}

// session returns the running yamux session, or nil before the upgrade and
// after the end.
func (l *link) session() *yamux.Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return nil
	}
	return l.sess
}

// state returns when the link connected and its streams: the stream slots
// in use, held from a request's start until its stream is closed. ok is
// false unless it is serving.
func (l *link) state() (connected time.Time, streams int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connected, len(l.slots), l.sess != nil && !l.ended
}

// serve runs the session until the CLI goes or the registry ends the link.
func (l *link) serve(conn net.Conn, closeWith func(code int, reason string)) {
	// yamux closes conn when the session ends; closing sends the WebSocket
	// close frame with the link's reason, so the CLI knows whether to retry.
	cfg := yamuxConfig(l.reg.log)
	guard := newFrameGuard(conn, cfg.MaxStreamWindowSize, time.Now, func(reason string) {
		l.reg.log.Warn("tunnel CLI broke the protocol; ending the connection", "id", l.owner.ArtifactID, "user", l.owner.Username, "reason", reason)
		l.reg.end(l, websocketPolicy, "protocol violation: "+reason)
	})
	wc := &closingConn{Conn: guard, closeWith: func() { closeWith(l.closeReason()) }}
	// stream.linger and the guard's reset, not yamux's timer, bound a
	// half-closed stream. yamux marks a timed-out stream closed before it
	// drops it, and a FIN from the CLI that arrives in between tears the
	// whole session down.
	cfg.StreamCloseTimeout = 0
	sess, err := yamux.Client(wc, cfg)
	if err != nil {
		l.reg.end(l, 1011, "internal error")
		wc.Close()
		return
	}
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		sess.Close()
		return
	}
	l.sess, l.guard, l.connected = sess, guard, time.Now()
	l.armExpiryLocked()
	l.mu.Unlock()
	go l.refuseStreams(sess)
	o := l.owner
	l.reg.log.Info("tunnel connected", "id", o.ArtifactID, "user", o.Username, "token", o.TokenName)
	select {
	case <-l.done:
	case <-sess.CloseChan():
		l.reg.end(l, websocketNormal, "")
	}
	sess.Close()
	guard.stop()
	l.transport.CloseIdleConnections()
	// Their reads have ended with the session; close what is left, so every
	// slot goes with the connection.
	l.mu.Lock()
	left := make([]*stream, 0, len(l.streams))
	for st := range l.streams {
		left = append(left, st)
	}
	l.mu.Unlock()
	for _, st := range left {
		st.Close()
	}
	code, why := l.closeReason()
	if why == "" {
		why = "connection closed"
	}
	l.reg.log.Info("tunnel disconnected", "id", o.ArtifactID, "user", o.Username, "code", code, "reason", why,
		"seconds", int(time.Since(l.connected).Seconds()))
}

// refuseStreams ends the connection when the CLI opens a stream of its own.
// Only the server opens streams; a stream the CLI opens would be held, with
// up to a window of data, outside every limit, as long as the session
// lasts. The first one ends the session, and with it every such stream.
func (l *link) refuseStreams(sess *yamux.Session) {
	st, err := sess.AcceptStream()
	if err != nil {
		return // the session ended
	}
	st.Close()
	l.reg.log.Warn("tunnel CLI opened a stream; ending the connection", "id", l.owner.ArtifactID, "user", l.owner.Username)
	l.reg.end(l, websocketPolicy, "protocol violation: the CLI opened a stream")
}

// closeReason is the WebSocket close code and reason the link ended with.
func (l *link) closeReason() (int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ended {
		return websocketNormal, ""
	}
	return l.code, l.why
}

// finish marks the link ended (once) and wakes serve. The caller holds the
// registry's lock and has removed the link from the registry.
func (l *link) finish(code int, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.ended, l.code, l.why = true, code, why
	if l.expiry != nil {
		l.expiry.Stop()
	}
	close(l.done)
}

// setAccess applies a changed expiry and access generation: streams let in
// under an older generation are closed.
func (l *link) setAccess(gen int, expires *time.Time) {
	l.mu.Lock()
	l.expires = expires
	if l.sess != nil && !l.ended {
		l.armExpiryLocked()
	}
	var stale []*stream
	if gen > l.gen {
		l.gen = gen
		for s := range l.streams {
			if s.slot.gen < gen {
				stale = append(stale, s)
			}
		}
	}
	l.mu.Unlock()
	for _, s := range stale {
		s.slot.changed.Store(true)
		s.Close()
	}
}

// armExpiryLocked ends the link when the artifact or the token expires,
// whichever comes first.
func (l *link) armExpiryLocked() {
	if l.expiry != nil {
		l.expiry.Stop()
		l.expiry = nil
	}
	at, why := l.expires, WhyExpired
	if t := l.owner.TokenExpires; t != nil && (at == nil || t.Before(*at)) {
		at, why = t, WhyTokenRevoked
	}
	if at == nil {
		return
	}
	l.expiry = time.AfterFunc(max(time.Until(*at), 0), func() { l.reg.end(l, api.TunnelCloseEnded, why) })
}

// slot is a request's claim on one of its tunnel's stream slots and one of
// the server's. It is released when the request has ended and the stream it
// opened is gone, whichever is later. A closed stream is drained until the
// CLI closes its end, so slots bound the streams and the memory they hold.
type slot struct {
	l       *link
	gen     int          // the access generation the request was let in under
	refs    atomic.Int32 // the request, and its stream once dialed
	changed atomic.Bool  // its stream was closed by an access change
}

// errRequestGone is a dial for a request that has ended: http.Transport
// lets a dial outlive the request that started it, and the dial's context
// still carries the request's slot.
var errRequestGone = errors.New("tunnel: the request already ended")

// hold adds a stream's claim on the slot. It refuses once the slot has
// been released: a late dial would otherwise release it a second time,
// and slots would no longer count the streams that exist.
func (s *slot) hold() bool {
	for {
		n := s.refs.Load()
		if n <= 0 {
			return false
		}
		if s.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (s *slot) done() {
	if s.refs.Add(-1) == 0 {
		<-s.l.reg.slots
		<-s.l.slots
	}
}

// acquire takes one of this tunnel's stream slots and one of the server's,
// waiting until wait has passed. At most as many requests wait as the
// tunnel has slots; more are refused at once. The caller calls done when
// the request ends.
func (l *link) acquire(ctx context.Context, wait time.Duration, gen int) (_ *slot, err error) {
	l.mu.Lock()
	if l.waiting >= cap(l.slots) {
		l.mu.Unlock()
		return nil, errBusy
	}
	l.waiting++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.waiting--
		l.mu.Unlock()
	}()
	timer := time.NewTimer(max(wait, 0))
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, errOffline
	case <-timer.C:
		return nil, errBusy
	}
	select {
	case l.reg.slots <- struct{}{}:
	case <-ctx.Done():
		err = ctx.Err()
	case <-l.done:
		err = errOffline
	case <-timer.C:
		err = errBusy
	}
	if err != nil {
		<-l.slots
		return nil, err
	}
	s := &slot{l: l, gen: gen}
	s.refs.Store(1)
	return s, nil
}

type slotKey struct{}

// withSlot passes a request's slot to the dialer that opens its stream.
func withSlot(ctx context.Context, s *slot) context.Context {
	return context.WithValue(ctx, slotKey{}, s)
}

// dial is the transport's dialer: it opens a stream on the session. It
// ignores the address; the CLI decides where requests go.
func (l *link) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	sl, _ := ctx.Value(slotKey{}).(*slot)
	if sl == nil {
		return nil, errors.New("tunnel: a request without a slot")
	}
	sess := l.session()
	if sess == nil {
		return nil, errOffline
	}
	if !sl.hold() {
		return nil, errRequestGone
	}
	st, err := sess.OpenStream()
	if err != nil {
		sl.done()
		return nil, errOffline
	}
	s := newStream(l, st, st.StreamID(), sl)
	l.mu.Lock()
	if l.ended || sl.gen < l.gen {
		l.mu.Unlock()
		sl.changed.Store(true)
		s.Close()
		return nil, errAccessChanged
	}
	l.streams[s] = struct{}{}
	l.mu.Unlock()
	return s, nil
}

// resetStream resets one stream the CLI hasn't closed: the session drops
// it and the CLI is told to. The guard applies the reset between two of the
// CLI's frames, so a ping makes sure one comes.
func (l *link) resetStream(id uint32) {
	l.mu.Lock()
	sess, guard := l.sess, l.guard
	l.mu.Unlock()
	if guard == nil {
		return
	}
	guard.resetStream(id)
	go sess.Ping()
}

func (l *link) forget(s *stream) {
	l.mu.Lock()
	delete(l.streams, s)
	l.mu.Unlock()
}

// closingConn calls closeWith instead of closing the connection itself, once.
type closingConn struct {
	net.Conn
	once      sync.Once
	closeWith func()
}

func (c *closingConn) Close() error {
	c.once.Do(c.closeWith)
	return nil
}
