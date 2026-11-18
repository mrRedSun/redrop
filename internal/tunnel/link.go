package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mrRedSun/redrop/pkg/api"
)

var (
	errOffline       = errors.New("tunnel offline")
	errBusy          = errors.New("tunnel busy")
	errAccessChanged = errors.New("access changed")
)

// streamCloseTimeout bounds how long a stream this side closed may wait for
// the CLI's own close before it is dropped (yamux's default is 5 minutes).
const streamCloseTimeout = 30 * time.Second

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
	connected time.Time
	streams   map[*stream]struct{}
	waiting   int // requests waiting for a slot
	expiry    *time.Timer
	ended     bool
	code      int
	why       string
}

func newLink(r *Registry, o Owner) *link {
	l := &link{reg: r, owner: o, expires: o.Expires, gen: o.AccessGen, streams: map[*stream]struct{}{},
		slots: make(chan struct{}, r.lim.Streams), done: make(chan struct{})}
	l.transport = &http.Transport{
		Proxy:                  nil,
		DialContext:            l.dial,
		DisableKeepAlives:      true, // one exchange per stream
		DisableCompression:     true, // pass the visitor's Accept-Encoding through
		ResponseHeaderTimeout:  r.lim.ResponseTimeout,
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

// state returns when the link connected and its open streams; ok is false
// unless it is serving.
func (l *link) state() (connected time.Time, streams int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connected, len(l.streams), l.sess != nil && !l.ended
}

// serve runs the session until the CLI goes or the registry ends the link.
func (l *link) serve(conn net.Conn, closeWith func(code int, reason string)) {
	// yamux closes conn when the session ends; closing sends the WebSocket
	// close frame with the link's reason, so the CLI knows whether to retry.
	wc := &closingConn{Conn: conn, closeWith: func() { closeWith(l.closeReason()) }}
	cfg := yamuxConfig(l.reg.log)
	cfg.StreamCloseTimeout = streamCloseTimeout
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
	l.sess, l.connected = sess, time.Now()
	l.armExpiryLocked()
	l.mu.Unlock()
	o := l.owner
	l.reg.log.Info("tunnel connected", "id", o.ArtifactID, "user", o.Username, "token", o.TokenName)
	select {
	case <-l.done:
	case <-sess.CloseChan():
		l.reg.end(l, websocketNormal, "")
	}
	sess.Close()
	l.transport.CloseIdleConnections()
	code, why := l.closeReason()
	if why == "" {
		why = "connection closed"
	}
	l.reg.log.Info("tunnel disconnected", "id", o.ArtifactID, "user", o.Username, "code", code, "reason", why,
		"seconds", int(time.Since(l.connected).Seconds()))
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
			if s.gen < gen {
				stale = append(stale, s)
			}
		}
	}
	l.mu.Unlock()
	for _, s := range stale {
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

// acquire takes one of this tunnel's stream slots and one of the server's,
// waiting until the response timeout. At most as many requests wait as the
// tunnel has slots; more are refused at once.
func (l *link) acquire(ctx context.Context) (release func(), err error) {
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
	timer := time.NewTimer(l.reg.lim.ResponseTimeout)
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
	return func() { <-l.reg.slots; <-l.slots }, nil
}

type genKey struct{}

// withGen records the access generation a request was let in under.
func withGen(ctx context.Context, gen int) context.Context {
	return context.WithValue(ctx, genKey{}, gen)
}

// dial is the transport's dialer: it opens a stream on the session. It
// ignores the address; the CLI decides where requests go.
func (l *link) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	sess := l.session()
	if sess == nil {
		return nil, errOffline
	}
	st, err := sess.OpenStream()
	if err != nil {
		return nil, errOffline
	}
	gen, _ := ctx.Value(genKey{}).(int)
	s := newStream(l, st, gen)
	l.mu.Lock()
	if l.ended || gen < l.gen {
		l.mu.Unlock()
		s.Close()
		return nil, errAccessChanged
	}
	l.streams[s] = struct{}{}
	l.mu.Unlock()
	return s, nil
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
