// Package tunnel is the server side of `redrop tunnel` (DESIGN.md
// "Tunnels"): the in-memory registry of connected CLIs, the yamux session
// each one runs over its WebSocket, and the reverse proxy that carries a
// visitor's request through it. It knows nothing about HTTP routes, slugs or
// access rules: the API transport hands it an authenticated connection, the
// content handler hands it requests that passed the access check, and the
// sharing service tells it when a tunnel's access changes.
//
// The registry is per process. Several redrop processes would each see only
// the CLIs connected to them.
package tunnel

import (
	"io"
	"log"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/pkg/api"
)

// keepAlive is how often each end pings over the WebSocket. Cloudflare drops
// a WebSocket after about 100 idle seconds; a ping is WebSocket data.
const keepAlive = 30 * time.Second

// writeTimeout bounds how long a frame, a keepalive ping's answer included,
// may wait in the session's send queue. A ping queues behind data frames,
// so yamux's 10-second default could drop a healthy connection on a slow
// uplink with many streams sending; a dead peer is still noticed within
// about a minute.
const writeTimeout = 30 * time.Second

// maxBudgetKeys bounds the byte-budget table, keyed by user. Only users with
// a connected tunnel create keys, so it never fills in practice; when it
// does, a new user is refused traffic (fail closed) rather than resetting
// another user's budget.
const maxBudgetKeys = 50_000

// Close reasons sent with api.TunnelCloseEnded.
const (
	WhyRevoked      = "the link was revoked"
	WhyDeleted      = "the tunnel was deleted"
	WhyExpired      = "the link expired"
	WhyUserDisabled = "the account was disabled"
	WhyTokenRevoked = "the API token was deleted or expired"
	WhyAdmin        = "ended by an administrator"
	WhyAdminRevoked = "an administrator revoked the link"
)

// Registry holds the connected tunnels, keyed by artifact ID.
type Registry struct {
	lim    config.Tunnels
	log    *slog.Logger
	budget *security.Limiter // bytes per user per hour, over all their tunnels
	slots  chan struct{}     // server-wide stream slots
	linger time.Duration     // lingerTimeout; shorter in tests

	mu      sync.Mutex
	links   map[string]*link // reserved or connected
	closed  bool
	serving sync.WaitGroup // Serve calls in progress
}

// New returns an empty registry with the given limits. Call Shutdown, then
// Close, when the server stops.
func New(lim config.Tunnels, logger *slog.Logger) *Registry {
	return &Registry{
		lim: lim, log: logger,
		budget: security.NewLimiter(int(lim.BytesPerHour), time.Hour, maxBudgetKeys, security.RefuseNew, time.Now),
		slots:  make(chan struct{}, lim.StreamsTotal),
		linger: lingerTimeout,
		links:  map[string]*link{},
	}
}

// Close stops the byte budget's background sweeper.
func (r *Registry) Close() { r.budget.Close() }

// Owner is who a connection belongs to, and the access it was let in with.
type Owner struct {
	ArtifactID string
	UserID     string
	Username   string
	TokenID    string
	TokenName  string
	// TokenExpires and Expires end the connection when they pass; Update
	// changes the expiry later.
	TokenExpires *time.Time
	Expires      *time.Time
	AccessGen    int
	// Client is the CLI's api.TunnelSessionHeader value: a connection with
	// the same value replaces this one, another is refused.
	Client string
}

// Reserve claims the tunnel for a connection that is about to be upgraded.
// It enforces the per-user limit and refuses a tunnel held by another CLI
// process; a reservation by the same process replaces the old connection.
//
// The caller checked access before calling it. A revoke, a disabled account
// or a deleted token that lands after that check but before the link is
// registered finds nothing to end, so once the link is registered Reserve
// calls recheck, which runs the checks again: an error gives the
// reservation up, and the owner it returns brings the access generation and
// expiry up to date. Changes after that reach the registered link.
//
// Errors are apperr values the API returns as they are.
func (r *Registry) Reserve(o Owner, recheck func() (Owner, error)) (*Lease, error) {
	le, err := r.reserve(o)
	if err != nil {
		return nil, err
	}
	now, err := recheck()
	if err != nil {
		le.Cancel()
		return nil, err
	}
	le.l.setAccess(now.AccessGen, now.Expires)
	return le, nil
}

func (r *Registry) reserve(o Owner) (*Lease, error) {
	if !r.lim.Enabled() {
		return nil, apperr.Forbidden("tunnels are turned off on this server")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, apperr.Conflict("the server is restarting; try again in a moment").WithRetryAfter(5 * time.Second)
	}
	old := r.links[o.ArtifactID]
	if old != nil && (o.Client == "" || old.owner.Client != o.Client) {
		return nil, apperr.Conflict("this tunnel is connected from another redrop tunnel; stop that one first").WithCode(api.CodeTunnelInUse)
	}
	n := 0
	for _, l := range r.links {
		if l.owner.UserID == o.UserID && l != old {
			n++
		}
	}
	if n >= r.lim.PerUser {
		return nil, apperr.RateLimited("you already have %d tunnel%s connected, the most this server allows; stop one first", n, plural(n)).WithCode(api.CodeTunnelLimit)
	}
	if old != nil {
		r.endLocked(old, api.TunnelCloseReplaced, "replaced by a newer connection")
	}
	l := newLink(r, o)
	r.links[o.ArtifactID] = l
	r.serving.Add(1)
	return &Lease{l: l}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Lease is a reserved tunnel. Exactly one of Serve or Cancel must be called.
type Lease struct {
	l    *link
	once sync.Once
}

// Cancel gives the reservation up (the upgrade failed).
func (le *Lease) Cancel() {
	le.once.Do(func() {
		le.l.reg.end(le.l, websocketNormal, "")
		le.l.reg.serving.Done()
	})
}

// websocketNormal is a normal WebSocket closure (RFC 6455 1000).
const websocketNormal = 1000

// websocketGoingAway is sent at server shutdown (RFC 6455 1001).
const websocketGoingAway = 1001

// websocketPolicy ends a connection whose CLI broke the protocol (RFC 6455
// 1008, policy violation).
const websocketPolicy = 1008

// Serve runs the tunnel over conn, the upgraded WebSocket as a byte stream,
// until the CLI goes away or the registry ends it. When the registry ends
// it, closeWith is called with the WebSocket close code and reason before
// conn is closed, so the CLI learns why. Serve logs the connection and its
// end; it returns once the session is over.
func (le *Lease) Serve(conn net.Conn, closeWith func(code int, reason string)) {
	ran := false
	le.once.Do(func() {
		ran = true
		defer le.l.reg.serving.Done()
		le.l.serve(conn, closeWith)
	})
	if !ran {
		conn.Close()
	}
}

// Online reports whether the tunnel's CLI is connected.
func (r *Registry) Online(artifactID string) bool { return r.connected(artifactID) != nil }

// connected returns the tunnel's live link, or nil.
func (r *Registry) connected(artifactID string) *link {
	r.mu.Lock()
	l := r.links[artifactID]
	r.mu.Unlock()
	if l == nil || l.session() == nil {
		return nil
	}
	return l
}

// Update tells a connected tunnel about its access after a change: a link
// that is no longer live ends with why; otherwise the expiry timer is reset
// and streams let in under an older access generation (before a password
// change or a rotation) are closed.
func (r *Registry) Update(artifactID string, live bool, gen int, expires *time.Time, why string) {
	r.mu.Lock()
	l := r.links[artifactID]
	if l != nil && !live {
		r.endLocked(l, api.TunnelCloseEnded, why)
		l = nil
	}
	r.mu.Unlock()
	if l != nil {
		l.setAccess(gen, expires)
	}
}

// End ends the tunnel's connection with api.TunnelCloseEnded and why. It
// reports whether one was connected.
func (r *Registry) End(artifactID, why string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.links[artifactID]
	if l == nil {
		return false
	}
	r.endLocked(l, api.TunnelCloseEnded, why)
	return true
}

// EndUser ends every tunnel of a user (the account was disabled).
func (r *Registry) EndUser(userID string) {
	r.endWhere(func(o Owner) bool { return o.UserID == userID }, WhyUserDisabled)
}

// EndToken ends every tunnel connected with an API token (it was deleted).
func (r *Registry) EndToken(tokenID string) {
	r.endWhere(func(o Owner) bool { return o.TokenID == tokenID }, WhyTokenRevoked)
}

func (r *Registry) endWhere(match func(Owner) bool, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.links {
		if match(l.owner) {
			r.endLocked(l, api.TunnelCloseEnded, why)
		}
	}
}

// Shutdown ends every tunnel with 1001 (going away), refuses new ones, and
// waits up to wait for the connections to close. The CLIs reconnect to the
// next server process.
func (r *Registry) Shutdown(wait time.Duration) {
	r.mu.Lock()
	r.closed = true
	for _, l := range r.links {
		r.endLocked(l, websocketGoingAway, "server restarting")
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.serving.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		r.log.Warn("tunnels still closing at shutdown")
	}
}

// Info describes a connected tunnel for administrators.
type Info struct {
	ArtifactID  string
	UserID      string
	Username    string
	TokenName   string
	ConnectedAt time.Time
	// Streams are the stream slots in use: requests in flight and streams
	// not yet closed.
	Streams int
	// BytesThisHour is the owner's use of the hourly byte budget, over all
	// their tunnels.
	BytesThisHour int64
}

// Connected lists the connected tunnels, longest-connected first.
func (r *Registry) Connected() []Info {
	r.mu.Lock()
	links := make([]*link, 0, len(r.links))
	for _, l := range r.links {
		links = append(links, l)
	}
	r.mu.Unlock()
	out := make([]Info, 0, len(links))
	for _, l := range links {
		at, streams, ok := l.state()
		if !ok {
			continue
		}
		out = append(out, Info{ArtifactID: l.owner.ArtifactID, UserID: l.owner.UserID, Username: l.owner.Username, TokenName: l.owner.TokenName,
			ConnectedAt: at, Streams: streams, BytesThisHour: int64(r.budget.Count(l.owner.UserID))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

// end ends l if it is still the registered link for its tunnel.
func (r *Registry) end(l *link, code int, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLocked(l, code, why)
}

func (r *Registry) endLocked(l *link, code int, why string) {
	if r.links[l.owner.ArtifactID] == l {
		delete(r.links, l.owner.ArtifactID)
	}
	l.finish(code, why)
}

// yamuxConfig is the server's session configuration: 256 KiB windows
// (yamux's minimum), keepalive pings every 30 seconds with 30 seconds to
// answer (the CLI uses the same, see pkg/client), and yamux's own
// diagnostics at debug level (they never hold request data). The accept
// backlog stays at yamux's default: it also sizes the server's own
// in-flight stream opens, and the CLI may not open streams anyway (see
// link.refuseStreams).
func yamuxConfig(logger *slog.Logger) *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = keepAlive
	c.ConnectionWriteTimeout = writeTimeout
	c.LogOutput = nil
	c.Logger = log.New(slogWriter{logger}, "", 0)
	return c
}

// slogWriter turns yamux's log lines into debug records.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.log.Debug("tunnel multiplexer", "message", string(trimNewline(p)))
	return len(p), nil
}

func trimNewline(p []byte) []byte {
	for len(p) > 0 && (p[len(p)-1] == '\n' || p[len(p)-1] == '\r') {
		p = p[:len(p)-1]
	}
	return p
}

// discardLog is the reverse proxy's error log: its messages can quote the
// outbound URL, which must never be logged (DESIGN.md "Logging").
var discardLog = log.New(io.Discard, "", 0)
