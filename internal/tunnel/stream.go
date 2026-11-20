package tunnel

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// errOverBudget is a stream's error once its tunnel has used up the hour's
// byte budget.
var errOverBudget = errors.New("tunnel byte budget used up")

// stream is one yamux stream as the transport's connection. It charges
// every byte to the tunnel's hourly budget and closes when it has moved no
// bytes for the idle timeout or the budget runs out.
type stream struct {
	net.Conn
	l     *link
	gen   int // access generation the request was let in under
	start time.Time
	last  atomic.Int64 // time.Since(start) at the last byte, in nanoseconds

	overBudget atomic.Bool

	mu     sync.Mutex // guards idle and closed
	idle   *time.Timer
	closed bool
}

func newStream(l *link, c net.Conn, gen int) *stream {
	s := &stream{Conn: c, l: l, gen: gen, start: time.Now()}
	s.mu.Lock()
	s.idle = time.AfterFunc(l.reg.lim.IdleTimeout, s.checkIdle)
	s.mu.Unlock()
	return s
}

func (s *stream) Read(p []byte) (int, error) {
	n, err := s.Conn.Read(p)
	if !s.charge(n) {
		return n, errOverBudget
	}
	return n, err
}

func (s *stream) Write(p []byte) (int, error) {
	if s.overBudget.Load() {
		return 0, errOverBudget
	}
	n, err := s.Conn.Write(p)
	if !s.charge(n) && err == nil {
		err = errOverBudget
	}
	return n, err
}

// charge records n bytes of activity. It reports false, and closes the
// stream, when the tunnel's budget is used up.
func (s *stream) charge(n int) bool {
	if n <= 0 {
		return !s.overBudget.Load()
	}
	s.last.Store(int64(time.Since(s.start)))
	if s.l.reg.budget.AllowN(s.l.owner.ArtifactID, n) {
		return true
	}
	s.overBudget.Store(true)
	s.Close()
	return false
}

// checkIdle closes the stream when no byte moved for the idle timeout, and
// otherwise looks again when it would be.
func (s *stream) checkIdle() {
	idle := s.l.reg.lim.IdleTimeout
	quiet := time.Since(s.start) - time.Duration(s.last.Load())
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if quiet < idle {
		s.idle.Reset(idle - quiet)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.Close()
}

func (s *stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.idle.Stop()
	s.mu.Unlock()
	s.l.forget(s)
	return s.Conn.Close()
}
