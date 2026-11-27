package sharing

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
)

// Live reload (DESIGN.md "Live reload"). While an owner's `redrop watch`
// holds a lease on an artifact, its viewer pages keep a stream open and
// reload their frame when the current version changes. Leases and streams
// live in memory only: a restart drops them, and the CLI renews its lease
// within a third of the lease time.

const (
	maxLiveSessions     = 8      // watch sessions holding a lease on one artifact
	maxLiveLeases       = 10_000 // leases on the server
	maxLeasesPerOwner   = 32     // watch sessions across an owner's artifacts
	maxStreamsPerClient = 16     // open streams per client network (a team behind one NAT)
)

// liveSessionRE is the shape of a watch session's ID, chosen by the CLI.
var liveSessionRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// ErrLiveBusy means a stream limit (server, owner, artifact or client) is reached.
var ErrLiveBusy = errors.New("too many live streams")

// LiveLease is a watch session's hold on live mode.
type LiveLease struct {
	ExpiresAt  time.Time
	RenewAfter time.Duration // renew this often so a missed renewal or two doesn't end it
}

// HoldLive creates or renews the live lease of a watch session on the
// actor's artifact. It is refused for an artifact without redrop's viewer
// page: the info button off, or a file artifact.
func (s *Service) HoldLive(ctx context.Context, actor Actor, id, session string) (LiveLease, error) {
	if !liveSessionRE.MatchString(session) {
		return LiveLease{}, apperr.Invalid("the live session ID must be 16 to 64 letters, digits, '-' or '_'")
	}
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return LiveLease{}, err
	}
	switch s.Mode(a, CurrentSnapshot(a)) {
	case ModeEncrypted:
		return LiveLease{}, apperr.Conflict("live reload is not supported for end-to-end encrypted artifacts; watch still publishes encrypted versions")
	case ModeDirect:
		return LiveLease{}, apperr.Conflict("live reload needs the info button: with it off, pages are served directly, without redrop's viewer page")
	case ModeFilePage:
		return LiveLease{}, apperr.Conflict("live reload needs redrop's viewer frame; file artifacts are shown on a file page without one")
	}
	exp, err := s.live.hold(a.ID, a.UserID, session, s.cfg.LiveLease)
	if err != nil {
		return LiveLease{}, err
	}
	return LiveLease{ExpiresAt: exp.UTC(), RenewAfter: s.cfg.LiveLease / 3}, nil
}

// ReleaseLive ends a watch session's lease. Releasing an unknown or expired
// session is not an error. When no lease is left, open streams end at once.
func (s *Service) ReleaseLive(ctx context.Context, actor Actor, id, session string) error {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return err
	}
	s.live.release(a.ID, session)
	return nil
}

// Live reports whether a's viewers get live reload now: a lease is held and
// the current version is shown in redrop's viewer frame.
func (s *Service) Live(a *store.Artifact) bool {
	return s.live.on(a.ID) && s.Mode(a, CurrentSnapshot(a)) == ModeFramed
}

// OpenLiveStream registers a viewer's stream on a from client (a client
// network key). The caller has checked access and Live. It returns
// ErrLiveBusy when a limit is reached.
func (s *Service) OpenLiveStream(a *store.Artifact, client string) (*LiveStream, error) {
	return s.live.open(a.ID, a.UserID, client)
}

// EndLiveStreams ends every open stream and refuses new ones. The server
// calls it when it starts shutting down, so streams don't hold it up.
func (s *Service) EndLiveStreams() { s.live.shutdown() }

// changed tells a's open streams to read it again: its current version,
// settings or access may have changed.
func (s *Service) changed(id string) { s.live.poke(id) }

// LiveStream is one viewer's open stream.
type LiveStream struct {
	hub        *liveHub
	artifactID string
	owner      string
	client     string
	poke       chan struct{} // one slot: changes coalesce, senders never block
	once       sync.Once
}

// Changed delivers a value when the artifact may have changed.
func (st *LiveStream) Changed() <-chan struct{} { return st.poke }

// Quit is closed when the server shuts down.
func (st *LiveStream) Quit() <-chan struct{} { return st.hub.quit }

// Close unregisters the stream. It is safe to call more than once.
func (st *LiveStream) Close() { st.once.Do(func() { st.hub.remove(st) }) }

type liveLease struct {
	owner   string
	expires time.Time
}

// liveHub holds the leases and the open streams.
type liveHub struct {
	now            func() time.Time
	maxTotal       int
	maxPerArtifact int
	quit           chan struct{}

	mu           sync.Mutex
	leases       map[string]map[string]liveLease // artifact ID -> session -> lease
	leaseOwners  map[string]int                  // owner ID -> leases
	nLeases      int
	streams      map[string]map[*LiveStream]struct{} // artifact ID -> streams
	streamOwners map[string]int                      // owner ID -> streams
	clients      map[string]int                      // client network -> streams
	total        int
	closed       bool
}

func newLiveHub(maxTotal, maxPerArtifact int, now func() time.Time) *liveHub {
	return &liveHub{
		now: now, maxTotal: maxTotal, maxPerArtifact: maxPerArtifact, quit: make(chan struct{}),
		leases: map[string]map[string]liveLease{}, leaseOwners: map[string]int{},
		streams: map[string]map[*LiveStream]struct{}{}, streamOwners: map[string]int{}, clients: map[string]int{},
	}
}

// hold sets session's lease on artifact to expire ttl from now. Owner comes
// from the verified artifact; renewals do not consume another quota slot.
func (h *liveHub) hold(artifact, owner, session string, ttl time.Duration) (time.Time, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.pruneLocked(artifact, now)
	held := h.leases[artifact]
	if _, renewing := held[session]; !renewing {
		if len(held) >= maxLiveSessions {
			return time.Time{}, apperr.Conflict("too many watch sessions hold live reload on this artifact; stop one first")
		}
		if h.leaseOwners[owner] >= maxLeasesPerOwner || h.nLeases >= maxLiveLeases {
			h.pruneAllLocked(now)
		}
		if h.leaseOwners[owner] >= maxLeasesPerOwner {
			return time.Time{}, apperr.Conflict("too many watch sessions hold live reload on your artifacts; the limit is %d per owner; stop one first", maxLeasesPerOwner)
		}
		if h.nLeases >= maxLiveLeases {
			return time.Time{}, apperr.RateLimited("the server holds too many live reload sessions; try again later").WithRetryAfter(ttl)
		}
		if held == nil {
			held = map[string]liveLease{}
			h.leases[artifact] = held
		}
		h.leaseOwners[owner]++
		h.nLeases++
	}
	exp := now.Add(ttl)
	held[session] = liveLease{owner: owner, expires: exp}
	return exp, nil
}

// release drops session's lease and, when none is left, pokes the streams
// so they end now rather than at their next keep-alive.
func (h *liveHub) release(artifact, session string) {
	h.mu.Lock()
	h.dropLeaseLocked(artifact, session)
	h.pruneLocked(artifact, h.now())
	_, still := h.leases[artifact]
	h.mu.Unlock()
	if !still {
		h.poke(artifact)
	}
}

// on reports whether any lease on artifact is unexpired.
func (h *liveHub) on(artifact string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for _, lease := range h.leases[artifact] {
		if lease.expires.After(now) {
			return true
		}
	}
	return false
}

func (h *liveHub) dropLeaseLocked(artifact, session string) {
	held := h.leases[artifact]
	lease, ok := held[session]
	if !ok {
		return
	}
	delete(held, session)
	if len(held) == 0 {
		delete(h.leases, artifact)
	}
	if h.leaseOwners[lease.owner]--; h.leaseOwners[lease.owner] == 0 {
		delete(h.leaseOwners, lease.owner)
	}
	h.nLeases--
}

// pruneLocked drops artifact's expired leases.
func (h *liveHub) pruneLocked(artifact string, now time.Time) {
	for session, lease := range h.leases[artifact] {
		if !lease.expires.After(now) {
			h.dropLeaseLocked(artifact, session)
		}
	}
}

func (h *liveHub) pruneAllLocked(now time.Time) {
	for artifact := range h.leases {
		h.pruneLocked(artifact, now)
	}
}

// prune drops every expired lease (the maintenance sweep).
func (h *liveHub) prune() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneAllLocked(h.now())
}

func (h *liveHub) open(artifact, owner, client string) (*LiveStream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.total >= h.maxTotal || h.streamOwners[owner] >= max(1, h.maxTotal/4) || len(h.streams[artifact]) >= h.maxPerArtifact || h.clients[client] >= maxStreamsPerClient {
		return nil, ErrLiveBusy
	}
	st := &LiveStream{hub: h, artifactID: artifact, owner: owner, client: client, poke: make(chan struct{}, 1)}
	set := h.streams[artifact]
	if set == nil {
		set = map[*LiveStream]struct{}{}
		h.streams[artifact] = set
	}
	set[st] = struct{}{}
	h.streamOwners[owner]++
	h.clients[client]++
	h.total++
	return st, nil
}

func (h *liveHub) remove(st *LiveStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.streams[st.artifactID]
	if _, ok := set[st]; !ok {
		return
	}
	delete(set, st)
	if len(set) == 0 {
		delete(h.streams, st.artifactID)
	}
	if h.streamOwners[st.owner]--; h.streamOwners[st.owner] == 0 {
		delete(h.streamOwners, st.owner)
	}
	if h.clients[st.client]--; h.clients[st.client] <= 0 {
		delete(h.clients, st.client)
	}
	h.total--
}

// poke wakes artifact's streams without ever blocking: a stream that has
// not read its last poke yet keeps that one.
func (h *liveHub) poke(artifact string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for st := range h.streams[artifact] {
		select {
		case st.poke <- struct{}{}:
		default:
		}
	}
}

func (h *liveHub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.quit)
	}
}

// counts reports the open streams in total and for artifact and client
// (tests).
func (h *liveHub) counts(artifact, client string) (total, perArtifact, perClient int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total, len(h.streams[artifact]), h.clients[client]
}
