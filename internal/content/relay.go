package content

import (
	"sync"
	"time"
)

// relay carries the address-bar location of viewer frames (DESIGN.md "Address
// bar"). A raw host publishes the path of every navigation it admits for a
// frame id; the viewer page of that frame subscribes over server-sent events
// and mirrors the path with history.replaceState. It needs nothing from the
// artifact. All state is in memory and bounded:
//
//   - at most relayMaxFrames frame ids are remembered (least recently used
//     ones without subscribers are dropped), each for at most relayFrameTTL;
//   - at most relayMaxPerClient streams per client and relayMaxStreams in all;
//   - a path is at most relayMaxPath bytes (longer ones are not relayed);
//   - each subscriber holds one pending path (newer replaces older).
type relay struct {
	mu      sync.Mutex
	frames  map[string]*relayFrame
	clients map[string]int
	streams int
	done    chan struct{}
	closed  bool
	now     func() time.Time
}

type relayFrame struct {
	path string
	seen time.Time
	subs map[*relaySub]struct{}
}

type relaySub struct {
	client string
	frame  string
	ch     chan string // capacity 1: the latest path
}

const (
	relayMaxFrames    = 10_000
	relayFrameTTL     = time.Hour // a frame's last path is kept this long after its last navigation
	relayMaxPerClient = 4
	relayMaxStreams   = 1_000
	relayMaxPath      = 2048
)

func newRelay() *relay {
	return &relay{frames: map[string]*relayFrame{}, clients: map[string]int{}, done: make(chan struct{}), now: time.Now}
}

// publish records path as frame's location and hands it to its subscribers.
func (r *relay) publish(frame, path string) {
	if frame == "" || len(path) > relayMaxPath {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	f := r.frames[frame]
	if f == nil {
		if len(r.frames) >= relayMaxFrames && !r.evictLocked() {
			return
		}
		f = &relayFrame{subs: map[*relaySub]struct{}{}}
		r.frames[frame] = f
	}
	f.path, f.seen = path, r.now()
	for s := range f.subs {
		select {
		case <-s.ch: // drop the older pending path
		default:
		}
		s.ch <- path
	}
}

// evictLocked drops expired frames, or else the least recently seen frame
// without subscribers. It reports whether there is room.
func (r *relay) evictLocked() bool {
	now := r.now()
	var oldest string
	for id, f := range r.frames {
		if len(f.subs) > 0 {
			continue
		}
		if now.Sub(f.seen) > relayFrameTTL {
			delete(r.frames, id)
			continue
		}
		if oldest == "" || f.seen.Before(r.frames[oldest].seen) {
			oldest = id
		}
	}
	if len(r.frames) < relayMaxFrames {
		return true
	}
	if oldest == "" {
		return false
	}
	delete(r.frames, oldest)
	return true
}

// subscribe opens a stream for frame on behalf of client. The returned
// channel first carries the frame's last known path, if any. ok is false when
// the limits are reached or the relay is closed.
func (r *relay) subscribe(frame, client string) (*relaySub, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.streams >= relayMaxStreams || r.clients[client] >= relayMaxPerClient {
		return nil, false
	}
	f := r.frames[frame]
	if f == nil {
		if len(r.frames) >= relayMaxFrames && !r.evictLocked() {
			return nil, false
		}
		f = &relayFrame{seen: r.now(), subs: map[*relaySub]struct{}{}}
		r.frames[frame] = f
	}
	s := &relaySub{client: client, frame: frame, ch: make(chan string, 1)}
	if f.path != "" {
		s.ch <- f.path
	}
	f.subs[s] = struct{}{}
	r.clients[client]++
	r.streams++
	return s, true
}

func (r *relay) unsubscribe(s *relaySub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.frames[s.frame]; f != nil {
		if _, ok := f.subs[s]; ok {
			delete(f.subs, s)
			r.streams--
			if r.clients[s.client]--; r.clients[s.client] <= 0 {
				delete(r.clients, s.client)
			}
		}
	}
}

// close ends every stream (on server shutdown) and refuses new ones.
func (r *relay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.done)
	}
}
