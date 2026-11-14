package sharing

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// The viewer frame (DESIGN.md "Viewer frame"). The shared link's host
// (ids.ViewerHost) serves redrop's viewer page; the artifact loads, untouched,
// in a frame from a raw host (ids.RawHost) that has its own origin. The raw
// host admits a request only with a raw cookie, minted from a short-lived
// enter token that the viewer page hands its frame after the usual gates.

// Mode is how a version of an artifact is presented on the shared link.
type Mode int

const (
	ModeDirect   Mode = iota + 1 // stored bytes on the shared link, as without the info button
	ModeFramed                   // viewer page on the shared link, artifact on a raw host
	ModeFilePage                 // redrop's own file page, with the info panel when it is on
)

// modeFor is the only place that decides which kinds get the frame.
func modeFor(kind string, infoOn bool) Mode {
	switch {
	case kind == api.KindFile:
		return ModeFilePage
	case infoOn:
		return ModeFramed
	}
	return ModeDirect
}

// Mode returns how snap, a version of a, is presented.
func (s *Service) Mode(a *store.Artifact, snap Snapshot) Mode {
	return modeFor(snap.Kind, s.infoButton(a))
}

// directWindow is how long a visitor's ?redrop-info=0 choice lasts.
const directWindow = 30 * time.Minute

// DirectChoice records that the visitor asked for the direct view of a
// (?redrop-info=0), so the pages and frames it loads stay direct too.
func (s *Service) DirectChoice(a *store.Artifact) *Grant {
	return &Grant{Value: s.directSigner.Sign(a.ID+"|"+strconv.Itoa(a.AccessGen), s.clock().Add(directWindow)), TTL: directWindow}
}

// Direct reports whether value is a current direct-view choice for a.
func (s *Service) Direct(a *store.Artifact, value string) bool {
	p, ok := s.directSigner.Verify(value)
	return ok && p == a.ID+"|"+strconv.Itoa(a.AccessGen)
}

// InfoShown reports whether a's pages carry the info button.
func (s *Service) InfoShown(a *store.Artifact) bool { return s.infoButton(a) }

// ErrVersionUnavailable means a requested older version may not be opened:
// the owner hasn't enabled it, the info button is off, or the version is not
// retained. Callers fall back to the current version; it is not a miss,
// because the visitor already passed the gates.
var ErrVersionUnavailable = errors.New("version unavailable")

// SelectVersion returns the version a viewer asked for: 0 or the current
// number means the current version.
func (s *Service) SelectVersion(ctx context.Context, a *store.Artifact, v int) (Snapshot, error) {
	if v == 0 || v == a.CurrentVersion {
		return CurrentSnapshot(a), nil
	}
	if !s.oldVersionsOpen(a) || v < 0 || v > ids.MaxHostVersion {
		return Snapshot{}, ErrVersionUnavailable
	}
	snap, ok := s.snapshotAt(ctx, a, v)
	if !ok {
		return Snapshot{}, ErrVersionUnavailable
	}
	return snap, nil
}

func (s *Service) oldVersionsOpen(a *store.Artifact) bool { return a.OldVersions && s.infoButton(a) }

// rawHostFor is the raw host that serves snap: the stable "--raw" host for
// the current version, the pinned "--v<N>" host for older ones.
func rawHostFor(a *store.Artifact, snap Snapshot) ids.Host {
	if snap.Version == a.CurrentVersion {
		return ids.RawHost(a.Slug, 0)
	}
	return ids.RawHost(a.Slug, snap.Version)
}

const (
	enterTTL     = 2 * time.Minute // enter tokens live only in the viewer page and one redirect
	rawCookieMax = time.Hour
)

// Raw grant kinds: what admitted the visitor to the viewer page.
const (
	rawOpen    = "open"    // ungated link: valid only while it stays ungated and active
	rawVisit   = "visit"   // derived from a visit grant (password or view limit)
	rawPreview = "preview" // owner preview from the dashboard
)

// rawClaim is the payload of enter tokens and raw cookies:
// "id|gen|version|kind|frame|limit". Version 0 is the "--raw" host. A claim
// is only honoured on the host whose Version matches, for the artifact and
// access generation it names. Frame is the frame id of the viewer load (for
// the address-bar relay); empty for previews. Limit (unix seconds, 0 = none)
// is the expiry of the visit or preview grant the claim came from: the raw
// cookie, refreshes included, never outlives it.
type rawClaim struct {
	ID      string
	Gen     int
	Version int
	Kind    string
	Frame   string
	Limit   int64
}

func (c rawClaim) encode() string {
	return strings.Join([]string{c.ID, strconv.Itoa(c.Gen), strconv.Itoa(c.Version), c.Kind, c.Frame, strconv.FormatInt(c.Limit, 10)}, "|")
}

func parseRawClaim(s string) (rawClaim, bool) {
	p := strings.Split(s, "|")
	if len(p) != 6 {
		return rawClaim{}, false
	}
	gen, err1 := strconv.Atoi(p[1])
	ver, err2 := strconv.Atoi(p[2])
	limit, err3 := strconv.ParseInt(p[5], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return rawClaim{}, false
	}
	return rawClaim{ID: p[0], Gen: gen, Version: ver, Kind: p[3], Frame: p[4], Limit: limit}, true
}

// cookieExpiry is when a raw cookie for c issued at now expires: one
// rawCookieMax later, but never after the claim's limit.
func (c rawClaim) cookieExpiry(now time.Time) time.Time {
	exp := now.Add(rawCookieMax)
	if c.Limit > 0 {
		exp = minTime(exp, time.Unix(c.Limit, 0))
	}
	return exp
}

// Frame is what one viewer page load needs to show snap in its frame.
type Frame struct {
	RawOrigin string // e.g. https://<slug>--raw.<content domain>
	Enter     string // enter token for RawOrigin/__redrop/enter
	Stream    string // address-bar relay token for the shared link's /__redrop/location
}

// Frame mints the grants for one viewer page load of snap. Call it only
// after Decide returned Serve or ServeOwnerPreview for g. The raw cookie it
// leads to expires within rawCookieMax and never after the visit grant.
func (s *Service) Frame(a *store.Artifact, snap Snapshot, g Grants, now time.Time) Frame {
	kind, limit := rawOpen, time.Time{}
	if visitExp, ok := grantExpiry(s.accessSigner, g.Visit, a, "visit"); ok && Gated(a) {
		kind, limit = rawVisit, visitExp
	} else if prevExp, ok := grantExpiry(s.previewSigner, g.Preview, a, "preview"); ok {
		kind, limit = rawPreview, prevExp
	}
	host := rawHostFor(a, snap)
	c := rawClaim{ID: a.ID, Gen: a.AccessGen, Version: host.Version(), Kind: kind, Frame: ids.Random(8)}
	streamExp := now.Add(rawCookieMax * 12)
	if !limit.IsZero() {
		c.Limit, streamExp = limit.Unix(), limit
	}
	enterExp := now.Add(enterTTL)
	if !limit.IsZero() {
		enterExp = minTime(enterExp, limit)
	}
	return Frame{
		RawOrigin: s.cfg.ContentOrigin(host),
		Enter:     s.enterSigner.Sign(c.encode(), enterExp),
		Stream:    s.streamSigner.Sign(rawClaim{ID: a.ID, Gen: a.AccessGen, Kind: "stream", Frame: c.Frame}.encode(), streamExp),
	}
}

// grantExpiry is validGrant that also returns the grant's expiry.
func grantExpiry(signer *security.Signer, value string, a *store.Artifact, kind string) (time.Time, bool) {
	p, exp, ok := signer.VerifyExpiry(value)
	return exp, ok && p == grantPayload(a, kind)
}

// clock is the time raw grants are issued at; it follows the raw signer's
// clock so tests can move both together.
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// RedeemEnter turns an enter token into the raw cookie for host.
func (s *Service) RedeemEnter(a *store.Artifact, host ids.Host, token string) (*Grant, bool) {
	p, ok := s.enterSigner.Verify(token)
	if !ok {
		return nil, false
	}
	c, ok := parseRawClaim(p)
	if !ok || !host.IsRaw() || c.ID != a.ID || c.Gen != a.AccessGen || c.Version != host.Version() || !s.entered.use(c.Frame, s.clock()) {
		return nil, false
	}
	return s.rawCookie(c, s.clock())
}

// usedFrames makes enter tokens single-use: it remembers the frame ids
// redeemed within the last enterTTL (a token can't be valid longer). It is
// bounded; when full of unexpired ids it refuses new redemptions (the frame
// shows the miss page) rather than forgetting ids that could be replayed.
type usedFrames struct {
	mu   sync.Mutex
	seen map[string]time.Time // frame id -> when it may be forgotten
}

const maxUsedFrames = 100_000

func (u *usedFrames) use(frame string, now time.Time) bool {
	if frame == "" {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.seen == nil {
		u.seen = map[string]time.Time{}
	}
	if _, used := u.seen[frame]; used {
		return false
	}
	if len(u.seen) >= maxUsedFrames {
		for id, until := range u.seen {
			if now.After(until) {
				delete(u.seen, id)
			}
		}
		if len(u.seen) >= maxUsedFrames {
			return false
		}
	}
	u.seen[frame] = now.Add(enterTTL)
	return true
}

// rawCookie signs c as a raw cookie issued at now.
func (s *Service) rawCookie(c rawClaim, now time.Time) (*Grant, bool) {
	exp := c.cookieExpiry(now)
	if !exp.After(now) {
		return nil, false
	}
	return &Grant{Value: s.rawSigner.Sign(c.encode(), exp), TTL: exp.Sub(now)}, true
}

// RedeemRawPreview exchanges a dashboard preview token (the same token the
// shared link accepts) for an owner-preview raw cookie on the "--raw" host.
func (s *Service) RedeemRawPreview(a *store.Artifact, host ids.Host, token string) (*Grant, bool) {
	p, ok := s.previewSigner.Verify(token)
	if !ok || p != previewTokenPayload(a) || !host.IsRaw() || host.Version() != 0 || a.Status(time.Now()) == api.StatusRevoked {
		return nil, false
	}
	c := rawClaim{ID: a.ID, Gen: a.AccessGen, Kind: rawPreview, Limit: s.clock().Add(previewWindow).Unix()}
	return s.rawCookie(c, s.clock())
}

// RawAccess is an admitted raw-host request.
type RawAccess struct {
	Snapshot Snapshot
	Frame    string // frame id for the address-bar relay; "" for previews
	// Refresh, when set, is a renewed raw cookie for the response: a page
	// left open keeps working past one rawCookieMax, up to the grant's limit.
	Refresh *Grant
}

// AuthorizeRaw admits a raw-host request carrying cookie. Everything is
// re-checked live, so revoking, rotating, changing the password, adding a
// view limit, expiry, turning the info button off, or turning older versions
// off takes effect on the next request. Raw requests never count views.
func (s *Service) AuthorizeRaw(ctx context.Context, a *store.Artifact, host ids.Host, cookie string, now time.Time) (RawAccess, bool) {
	p, exp, ok := s.rawSigner.VerifyExpiry(cookie)
	if !ok {
		return RawAccess{}, false
	}
	c, ok := parseRawClaim(p)
	if !ok || !host.IsRaw() || c.ID != a.ID || c.Gen != a.AccessGen || c.Version != host.Version() {
		return RawAccess{}, false
	}
	switch status := a.Status(now); c.Kind {
	case rawOpen:
		ok = status == api.StatusActive && !Gated(a)
	case rawVisit:
		ok = status == api.StatusActive || status == api.StatusExhausted
	case rawPreview:
		ok = status != api.StatusRevoked
	default:
		ok = false
	}
	if !ok {
		return RawAccess{}, false
	}
	snap := CurrentSnapshot(a)
	if c.Version != 0 {
		if !s.oldVersionsOpen(a) || c.Version == a.CurrentVersion {
			return RawAccess{}, false
		}
		if snap, ok = s.snapshotAt(ctx, a, c.Version); !ok {
			return RawAccess{}, false
		}
	}
	if s.Mode(a, snap) != ModeFramed {
		return RawAccess{}, false
	}
	acc := RawAccess{Snapshot: snap, Frame: c.Frame}
	// Sliding refresh: renew once less than half the lifetime is left, if
	// that actually extends it (the grant's limit may not allow more).
	if exp.Sub(now) < rawCookieMax/2 && c.cookieExpiry(now).Sub(exp) > time.Minute {
		acc.Refresh, _ = s.rawCookie(c, now)
	}
	return acc, true
}

// StreamFrame verifies an address-bar relay token for a and returns its frame
// id and expiry.
func (s *Service) StreamFrame(a *store.Artifact, token string) (string, time.Time, bool) {
	p, exp, ok := s.streamSigner.VerifyExpiry(token)
	if !ok {
		return "", time.Time{}, false
	}
	c, ok := parseRawClaim(p)
	if status := a.Status(s.clock()); !ok || c.Kind != "stream" || c.ID != a.ID || c.Gen != a.AccessGen || c.Frame == "" ||
		status == api.StatusRevoked || status == api.StatusExpired {
		return "", time.Time{}, false
	}
	return c.Frame, exp, true
}
