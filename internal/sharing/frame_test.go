package sharing

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

func frameService(t *testing.T) *Service {
	t.Helper()
	s := testService()
	secret := []byte("0123456789abcdef0123456789abcdef")
	s.enterSigner = security.NewSigner(secret, "raw-enter")
	s.rawSigner = security.NewSigner(secret, "raw-access")
	s.streamSigner = security.NewSigner(secret, "frame-stream")
	s.cfg.ContentURL, _ = url.Parse("https://usercontent.example")
	s.cfg.InfoButtonDefault = true
	return s
}

const testSlug = "amber-otter-quilt-river"

func framedArtifact() *store.Artifact {
	exp := time.Now().Add(24 * time.Hour)
	return &store.Artifact{ID: "a1", Slug: testSlug, AccessGen: 3, Kind: api.KindHTML, CurrentVersion: 2, ExpiresAt: &exp}
}

// enter runs the frame bootstrap: a viewer load's grants redeemed on host.
func enter(t *testing.T, s *Service, a *store.Artifact, g Grants, host ids.Host) (*Grant, bool) {
	t.Helper()
	f := s.Frame(a, CurrentSnapshot(a), g, time.Now())
	if want := "https://" + testSlug + "--raw.usercontent.example"; f.RawOrigin != want {
		t.Fatalf("raw origin %q, want %q", f.RawOrigin, want)
	}
	return redeem(s, a, host, f.Enter)
}

func TestRawGrants(t *testing.T) {
	s := frameService(t)
	ctx := context.Background()
	raw := ids.RawHost(testSlug, 0)
	a := framedArtifact()
	g, ok := enter(t, s, a, Grants{}, raw)
	if !ok || g.TTL <= 0 || g.TTL > rawCookieMax {
		t.Fatalf("enter: %v %+v", ok, g)
	}
	acc, ok := s.AuthorizeRaw(ctx, a, raw, g.Value)
	if !ok || acc.Snapshot.Version != 2 || acc.Frame == "" {
		t.Fatalf("authorize: %v %+v", ok, acc)
	}

	// The token and the cookie are bound to the artifact, its access
	// generation, and the exact raw host.
	if _, ok := enter(t, s, a, Grants{}, ids.RawHost(testSlug, 2)); ok {
		t.Error("enter token accepted on a pinned host")
	}
	if _, ok := enter(t, s, a, Grants{}, ids.ViewerHost(testSlug)); ok {
		t.Error("enter token accepted on the viewer host")
	}
	other := framedArtifact()
	other.ID = "a2"
	f := s.Frame(a, CurrentSnapshot(a), Grants{}, time.Now())
	if _, ok := redeem(s, other, raw, f.Enter); ok {
		t.Error("enter token accepted for another artifact")
	}
	if _, ok := s.AuthorizeRaw(ctx, other, raw, g.Value); ok {
		t.Error("cookie accepted for another artifact")
	}
	if _, ok := s.AuthorizeRaw(ctx, a, ids.RawHost(testSlug, 1), g.Value); ok {
		t.Error("cookie accepted on another version's host")
	}
	if _, ok := redeem(s, a, raw, g.Value); ok {
		t.Error("a raw cookie was accepted as an enter token")
	}
	if _, ok := s.AuthorizeRaw(ctx, a, raw, f.Enter); ok {
		t.Error("an enter token was accepted as a raw cookie")
	}
	expired := s.rawSigner.Sign(rawClaim{ID: a.ID, Gen: a.AccessGen, Kind: rawOpen, Frame: "f"}.encode(), time.Now().Add(-time.Second))
	if _, ok := s.AuthorizeRaw(ctx, a, raw, expired); ok {
		t.Error("expired cookie accepted")
	}

	// Live checks: each of these ends access on the next request.
	for name, change := range map[string]func(*store.Artifact){
		"rotate/password/revoke (gen)": func(a *store.Artifact) { a.AccessGen++ },
		"revoked":                      func(a *store.Artifact) { a.Revoked = true },
		"expired":                      func(a *store.Artifact) { past := time.Now().Add(-time.Minute); a.ExpiresAt = &past },
		"view limit added":             func(a *store.Artifact) { a.MaxViews = 3 },
		"password added":               func(a *store.Artifact) { a.PasswordHash = "x" },
		"info button off":              func(a *store.Artifact) { off := false; a.InfoButton = &off },
		"now a file":                   func(a *store.Artifact) { a.Kind = api.KindFile },
	} {
		b := framedArtifact()
		change(b)
		if _, ok := s.AuthorizeRaw(ctx, b, raw, g.Value); ok {
			t.Errorf("%s: cookie still accepted", name)
		}
	}
}

// TestRawGrantFromVisit: behind a gate, the raw cookie derives from the
// visit grant: it never outlives it, and survives the link being used up.
func TestRawGrantFromVisit(t *testing.T) {
	s := frameService(t)
	ctx := context.Background()
	raw := ids.RawHost(testSlug, 0)
	a := framedArtifact()
	a.MaxViews, a.Views = 1, 1
	visit := s.accessSigner.Sign(grantPayload(a, "visit"), time.Now().Add(10*time.Minute))
	g, ok := enter(t, s, a, Grants{Visit: visit}, raw)
	if !ok || g.TTL > 10*time.Minute {
		t.Fatalf("visit-derived cookie: %v, ttl %v", ok, g.TTL)
	}
	if _, ok := s.AuthorizeRaw(ctx, a, raw, g.Value); !ok {
		t.Fatal("exhausted link with a visit: frame refused")
	}
	a.Revoked = true
	if _, ok := s.AuthorizeRaw(ctx, a, raw, g.Value); ok {
		t.Fatal("revoked: frame still served")
	}
}

// TestFrameGrantKind: only a grant that Decide would honour for this artifact
// and generation lifts the raw grant above "open".
func TestFrameGrantKind(t *testing.T) {
	s := frameService(t)
	ctx := context.Background()
	raw := ids.RawHost(testSlug, 0)
	a := framedArtifact()
	stale := *a
	stale.AccessGen--
	oldPreview := s.previewSigner.Sign(grantPayload(&stale, "preview"), time.Now().Add(time.Minute))
	g, ok := enter(t, s, a, Grants{Preview: oldPreview}, raw)
	if !ok {
		t.Fatal("enter")
	}
	past := time.Now().Add(-time.Minute)
	a.ExpiresAt = &past
	if _, ok := s.AuthorizeRaw(ctx, a, raw, g.Value); ok {
		t.Fatal("a stale preview cookie minted a preview-kind grant that outlives expiry")
	}
}

func TestRawPreview(t *testing.T) {
	s := frameService(t)
	ctx := context.Background()
	raw := ids.RawHost(testSlug, 0)
	a := framedArtifact()
	tok := s.previewSigner.Sign(previewTokenPayload(a), time.Now().Add(time.Minute))
	if _, ok := s.RedeemRawPreview(a, ids.RawHost(testSlug, 1), tok); ok {
		t.Error("preview redeemed on a pinned host")
	}
	g, ok := s.RedeemRawPreview(a, raw, tok)
	if !ok {
		t.Fatal("preview not redeemed")
	}
	past := time.Now().Add(-time.Hour)
	a.ExpiresAt = &past // owners can preview expired artifacts
	if acc, ok := s.AuthorizeRaw(ctx, a, raw, g.Value); !ok || acc.Frame != "" {
		t.Fatalf("preview of an expired artifact: %v %+v", ok, acc)
	}
	a.Revoked = true
	if _, ok := s.AuthorizeRaw(ctx, a, raw, g.Value); ok {
		t.Error("preview of a revoked artifact")
	}
}

func TestStreamFrame(t *testing.T) {
	s := frameService(t)
	a := framedArtifact()
	f := s.Frame(a, CurrentSnapshot(a), Grants{}, time.Now())
	frame, exp, ok := s.StreamFrame(a, f.Stream)
	if !ok || frame == "" || time.Until(exp) > 12*time.Hour {
		t.Fatalf("stream: %v %q %v", ok, frame, exp)
	}
	g, _ := redeem(s, a, ids.RawHost(testSlug, 0), f.Enter)
	if acc, _ := s.AuthorizeRaw(context.Background(), a, ids.RawHost(testSlug, 0), g.Value); acc.Frame != frame {
		t.Fatalf("relay frame ids differ: raw %q, stream %q", acc.Frame, frame)
	}
	other := framedArtifact()
	other.ID = "a2"
	if _, _, ok := s.StreamFrame(other, f.Stream); ok {
		t.Error("stream token accepted for another artifact")
	}
	a.AccessGen++
	if _, _, ok := s.StreamFrame(a, f.Stream); ok {
		t.Error("stream token survived a generation change")
	}
	if f2 := s.Frame(a, CurrentSnapshot(a), Grants{}, time.Now()); f2.Stream == f.Stream {
		t.Error("two viewer loads share a frame id")
	}
}

// fakeClock sets the service's clock (grants and signers) to a movable time.
func fakeClock(s *Service, start time.Time) *time.Time {
	now := start
	s.SetClock(func() time.Time { return now })
	return &now
}

// redeem is RedeemEnter for one test client; ok reports a raw cookie.
func redeem(s *Service, a *store.Artifact, host ids.Host, token string) (*Grant, bool) {
	g, err := s.RedeemEnter(a, host, token, "client")
	return g, err == nil && g != nil
}

// TestRawCookieSlidingRefresh: a page left open keeps working past one
// rawCookieMax. Admitted requests renew the cookie once less than half its
// life is left; an open link's renewals are unbounded (each request is
// re-checked live anyway), a visit-derived cookie never outlives its visit.
func TestRawCookieSlidingRefresh(t *testing.T) {
	s := frameService(t)
	ctx := context.Background()
	raw := ids.RawHost(testSlug, 0)
	t0 := time.Now().Truncate(time.Second)
	now := fakeClock(s, t0)
	a := framedArtifact()
	far := t0.Add(48 * time.Hour)
	a.ExpiresAt = &far

	g, ok := enter(t, s, a, Grants{}, raw)
	if !ok || g.TTL != rawCookieMax {
		t.Fatalf("enter: %v %v", ok, g)
	}
	*now = t0.Add(10 * time.Minute)
	if acc, ok := s.AuthorizeRaw(ctx, a, raw, g.Value); !ok || acc.Refresh != nil {
		t.Fatalf("early request: %v, refresh %v", ok, acc.Refresh)
	}
	*now = t0.Add(50 * time.Minute)
	acc, ok := s.AuthorizeRaw(ctx, a, raw, g.Value)
	if !ok || acc.Refresh == nil || acc.Refresh.TTL != rawCookieMax {
		t.Fatalf("late request: %v, refresh %+v", ok, acc.Refresh)
	}
	*now = t0.Add(100 * time.Minute) // past the old one-hour cap
	if _, ok := s.AuthorizeRaw(ctx, a, raw, g.Value); ok {
		t.Fatal("the original cookie outlived its hour")
	}
	if _, ok := s.AuthorizeRaw(ctx, a, raw, acc.Refresh.Value); !ok {
		t.Fatal("the refreshed cookie stopped working past the old cap")
	}

	// Behind a gate the visit grant is the hard limit.
	*now = t0
	b := framedArtifact()
	b.PasswordHash, b.ExpiresAt = "x", &far
	visit := s.accessSigner.Sign(grantPayload(b, "visit"), t0.Add(75*time.Minute))
	g, _ = enter(t, s, b, Grants{Visit: visit}, raw)
	*now = t0.Add(50 * time.Minute)
	acc, ok = s.AuthorizeRaw(ctx, b, raw, g.Value)
	if !ok || acc.Refresh == nil || acc.Refresh.TTL != 25*time.Minute {
		t.Fatalf("visit-bounded refresh: %v %+v", ok, acc.Refresh)
	}
	*now = t0.Add(80 * time.Minute)
	if _, ok := s.AuthorizeRaw(ctx, b, raw, acc.Refresh.Value); ok {
		t.Fatal("a refreshed cookie outlived the visit grant")
	}
}

// TestEnterTokenExpiry: an enter token works for enterTTL only.
func TestEnterTokenExpiry(t *testing.T) {
	s := frameService(t)
	t0 := time.Now().Truncate(time.Second)
	now := fakeClock(s, t0)
	a := framedArtifact()
	f := s.Frame(a, CurrentSnapshot(a), Grants{}, t0)
	*now = t0.Add(enterTTL + time.Second)
	if _, ok := redeem(s, a, ids.RawHost(testSlug, 0), f.Enter); ok {
		t.Fatal("expired enter token redeemed")
	}
}

// TestEnterTokenSingleUse: an enter token is redeemed once; a copy (from the
// page source or a log) is useless afterwards.
func TestEnterTokenSingleUse(t *testing.T) {
	s := frameService(t)
	a := framedArtifact()
	raw := ids.RawHost(testSlug, 0)
	f := s.Frame(a, CurrentSnapshot(a), Grants{}, time.Now())
	other := framedArtifact()
	other.ID = "a2"
	if _, ok := redeem(s, other, raw, f.Enter); ok {
		t.Fatal("redeemed for another artifact")
	}
	if _, ok := redeem(s, a, raw, f.Enter); !ok {
		t.Fatal("a refused redemption used the token up")
	}
	if _, ok := redeem(s, a, raw, f.Enter); ok {
		t.Fatal("enter token replayed")
	}

	// One client gets enterQuota frames per enterTTL; others are unaffected,
	// and the refusal is not a miss.
	var u usedFrames
	t0 := time.Now()
	for i := range enterQuota {
		if u.use("f"+strconv.Itoa(i), "greedy", t0) != enterOK {
			t.Fatalf("frame %d refused", i)
		}
	}
	if got := u.use("one-more", "greedy", t0); got != enterOverQuota {
		t.Fatalf("over quota: %v", got)
	}
	if got := u.use("fresh", "someone-else", t0); got != enterOK {
		t.Fatalf("another client was refused: %v", got)
	}
	if got := u.use("f0", "someone-else", t0); got != enterReplay {
		t.Fatalf("replay by another client: %v", got)
	}
	// The window slides: after enterTTL the client's share is back.
	if got := u.use("later", "greedy", t0.Add(enterTTL)); got != enterOK || len(u.order) != 1 {
		t.Fatalf("after the window: %v, %d kept", got, len(u.order))
	}
}

// TestEnterSetFull: the set never forgets an unexpired id. When it is full
// the redemption is refused as busy (not a miss); expired ids make room.
func TestEnterSetFull(t *testing.T) {
	var u usedFrames
	t0 := time.Now()
	for i := range maxUsedFrames {
		u.use("f"+strconv.Itoa(i), "c"+strconv.Itoa(i/enterQuota), t0)
	}
	if got := u.use("fresh", "new-client", t0); got != enterFull {
		t.Fatalf("full set: %v", got)
	}
	if got := u.use("f1", "new-client", t0); got != enterReplay {
		t.Fatalf("an id was forgotten early: %v", got)
	}
	if got := u.use("fresh", "new-client", t0.Add(enterTTL)); got != enterOK {
		t.Fatalf("expired ids did not make room: %v", got)
	}
}
