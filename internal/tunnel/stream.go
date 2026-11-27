package tunnel

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// errOverBudget is a stream's error once its owner has used up the hour's
// byte budget.
var errOverBudget = errors.New("tunnel byte budget used up")

// stream is one yamux stream as the transport's connection. It charges
// every byte to its owner's hourly budget, closes when it has moved no
// bytes for the idle timeout or the budget runs out, and holds its
// request's slot until it is closed.
type stream struct {
	net.Conn
	l     *link
	slot  *slot
	id    uint32 // the yamux stream ID
	start time.Time
	last  atomic.Int64 // time.Since(start) at the last byte, in nanoseconds

	overBudget atomic.Bool

	// readMu makes reads one at a time: yamux v0.1.2 deadlocks when two
	// goroutines read one stream (Read and its window update take the
	// state and receive locks in opposite orders), and linger reads while
	// the transport may still be in a Read.
	readMu sync.Mutex

	mu     sync.Mutex // guards idle and closed
	idle   *time.Timer
	closed bool
}

func newStream(l *link, c net.Conn, id uint32, sl *slot) *stream {
	sl.hold()
	s := &stream{Conn: c, l: l, slot: sl, id: id, start: time.Now()}
	s.mu.Lock()
	s.idle = time.AfterFunc(l.reg.lim.IdleTimeout, s.checkIdle)
	s.mu.Unlock()
	return s
}

func (s *stream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
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
	if s.l.reg.budget.AllowN(s.l.owner.UserID, n) {
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
	err := s.Conn.Close() // a FIN: the CLI stops writing and closes its end
	go s.linger()
	return err
}

// linger holds the request's slot until the stream is gone from the
// session, reading and discarding what the CLI sent meanwhile. The stream,
// and up to a window of unread data, live until then, so the slot is what
// bounds them.
//
// Normally the CLI closes its end within a round trip. If it hasn't after
// the linger time, the stream alone is reset (link.resetStream): the
// connection and its other streams go on. A CLI that goes on sending on a
// reset stream is ended by the guard.
func (s *stream) linger() {
	defer s.slot.done()
	deadline := time.Now().Add(s.l.reg.linger)
	s.Conn.SetReadDeadline(deadline) // also ends a Read the transport is still in
	buf := make([]byte, 4<<10)
	reset := false
	for {
		s.readMu.Lock()
		n, err := s.Conn.Read(buf)
		s.readMu.Unlock()
		if n > 0 {
			s.l.reg.budget.AllowN(s.l.owner.UserID, n)
		}
		switch {
		case !reset && (errors.Is(err, yamux.ErrTimeout) || (err == nil && time.Now().After(deadline))):
			reset = true
			s.l.reg.log.Debug("tunnel stream reset: the CLI did not close it", "id", s.l.owner.ArtifactID)
			s.Conn.SetReadDeadline(time.Time{})
			s.l.resetStream(s.id)
		case err != nil:
			return // the CLI closed it, the reset arrived, or the session ended
		}
	}
}
