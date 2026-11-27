package sharing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
)

// The live reload hub (DESIGN.md "Live reload"): lease lifecycle on a fake
// clock, stream limits, and pokes that never block.

type fakeNow struct{ t time.Time }

func (f *fakeNow) now() time.Time          { return f.t }
func (f *fakeNow) advance(d time.Duration) { f.t = f.t.Add(d) }

func newTestHub(maxTotal, maxPerArtifact int) (*liveHub, *fakeNow) {
	clock := &fakeNow{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	return newLiveHub(maxTotal, maxPerArtifact, clock.now), clock
}

func TestLiveOwnerLeaseAdmission(t *testing.T) {
	s, first := budgetService(t, 10000, 10000)
	second := addUser(t, s, "second", 10000)
	s.cfg.LiveLease = time.Minute
	s.live, _ = newTestHub(1000, 100)
	ctx := context.Background()
	artifact := func(actor Actor) string {
		t.Helper()
		info := true
		id, err := s.db.CreateArtifact(ctx, &store.NewArtifact{UserID: actor.UserID, InfoButton: &info},
			&store.NewVersion{Kind: "html", Entry: "index.html"}, 10000)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var firstArtifact string
	for i := range 4 {
		id := artifact(first)
		if i == 0 {
			firstArtifact = id
		}
		for j := range 8 {
			if _, err := s.HoldLive(ctx, first, id, fmt.Sprintf("session-%016d", j)); err != nil {
				t.Fatalf("lease %d: %v", i*8+j, err)
			}
		}
	}
	if _, err := s.HoldLive(ctx, first, artifact(first), "session-overflow"); apperr.KindOf(err) != apperr.KindConflict || !strings.Contains(err.Error(), "32") {
		t.Errorf("33rd owner lease: %v, want conflict explaining the 32-session limit", err)
	}
	if _, err := s.HoldLive(ctx, first, firstArtifact, "session-0000000000000000"); err != nil {
		t.Errorf("renew at owner capacity: %v", err)
	}
	secondArtifact := artifact(second)
	if _, err := s.HoldLive(ctx, first, secondArtifact, "session-not-owned"); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("other owner's artifact: %v", err)
	}
	if _, err := s.HoldLive(ctx, second, secondArtifact, "session-admitted"); err != nil {
		t.Errorf("second owner's lease: %v", err)
	}
}

func TestLiveOwnerStreamAdmission(t *testing.T) {
	for _, limit := range []int{1, 2, 3, 4, 5, 11, 1000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			h, _ := newTestHub(limit, limit)
			s := &Service{live: h}
			share := max(1, limit/4)
			for i := range share {
				st, err := s.OpenLiveStream(&store.Artifact{ID: fmt.Sprint(i), UserID: "first"}, fmt.Sprint(i))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(st.Close)
			}
			if st, err := s.OpenLiveStream(&store.Artifact{ID: "extra", UserID: "first"}, "extra"); !errors.Is(err, ErrLiveBusy) {
				if st != nil {
					t.Cleanup(st.Close)
				}
				t.Errorf("owner stream %d: %v, want busy", share+1, err)
			}
			st, err := s.OpenLiveStream(&store.Artifact{ID: "other", UserID: "second"}, "other")
			if limit == 1 {
				if !errors.Is(err, ErrLiveBusy) {
					t.Fatalf("server-wide limit: %v", err)
				}
			} else if err != nil {
				t.Fatalf("second owner's stream: %v", err)
			} else {
				t.Cleanup(st.Close)
			}
		})
	}
}

func TestLiveOwnerLeaseAccounting(t *testing.T) {
	h, clock := newTestHub(100, 100)
	for _, owner := range []string{"first", "second"} {
		for i := range maxLeasesPerOwner {
			if _, err := h.hold(fmt.Sprintf("%s-%d", owner, i), owner, "s", time.Minute); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.release("first-0", "s")
	h.release("first-0", "s")
	h.release("second-0", "unknown")
	if h.leaseOwners["first"] != 31 || h.leaseOwners["second"] != 32 || h.nLeases != 63 {
		t.Fatalf("release counts: leases=%d owners=%v", h.nLeases, h.leaseOwners)
	}
	if _, err := h.hold("first-new", "first", "s", time.Minute); err != nil {
		t.Fatalf("released owner's replacement: %v", err)
	}
	if _, err := h.hold("second-new", "second", "s", time.Minute); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("release freed another owner's quota: %v", err)
	}
	clock.advance(30 * time.Second)
	if _, err := h.hold("first-1", "first", "s", time.Minute); err != nil {
		t.Fatalf("renew at owner capacity: %v", err)
	}
	if h.nLeases != 64 || h.leaseOwners["first"] != 32 {
		t.Fatalf("renew changed counts: leases=%d owners=%v", h.nLeases, h.leaseOwners)
	}
	clock.advance(30 * time.Second)
	// Admission must reclaim this owner's expired leases on other artifacts,
	// without waiting for maintenance. Only the renewed lease survives.
	if _, err := h.hold("after-expiry", "first", "s", time.Minute); err != nil {
		t.Fatalf("owner-cap expiry prune: %v", err)
	}
	if h.nLeases != 2 || h.leaseOwners["first"] != 2 || len(h.leaseOwners) != 1 {
		t.Fatalf("expiry counts: leases=%d owners=%v", h.nLeases, h.leaseOwners)
	}
	clock.advance(time.Minute)
	h.prune()
	h.prune()
	if h.nLeases != 0 || len(h.leases) != 0 || len(h.leaseOwners) != 0 {
		t.Fatalf("prune retained leases or owners: leases=%d artifacts=%d owners=%v", h.nLeases, len(h.leases), h.leaseOwners)
	}
	// Release also prunes expired sibling sessions and removes the owner key.
	if _, err := h.hold("siblings", "third", "expired", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hold("siblings", "third", "released", time.Minute); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	h.release("siblings", "released")
	if h.nLeases != 0 || len(h.leases) != 0 || len(h.leaseOwners) != 0 {
		t.Fatalf("release retained expired siblings or owner: leases=%d owners=%v", h.nLeases, h.leaseOwners)
	}
}

func TestLiveOwnerStreamAccounting(t *testing.T) {
	h, _ := newTestHub(8, 8)
	var streams []*LiveStream
	take := func(artifact, owner string) {
		t.Helper()
		st, err := h.open(artifact, owner, artifact)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		streams = append(streams, st)
	}
	for _, owner := range []string{"first", "second"} {
		for i := range 2 {
			take(fmt.Sprintf("%s-%d", owner, i), owner)
		}
	}
	streams[0].Close()
	streams[0].Close()
	h.remove(streams[0])
	if h.total != 3 || h.streamOwners["first"] != 1 || h.streamOwners["second"] != 2 {
		t.Fatalf("close counts: total=%d owners=%v", h.total, h.streamOwners)
	}
	if _, err := h.open("second-extra", "second", "new"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("close freed another owner's quota: %v", err)
	}
	take("first-new", "first")
	// Shutdown signals streams; their normal deferred Close frees the counts.
	h.shutdown()
	for _, st := range streams {
		st.Close()
	}
	if h.total != 0 || len(h.streams) != 0 || len(h.clients) != 0 || len(h.streamOwners) != 0 {
		t.Fatalf("close retained streams or owners: total=%d artifacts=%d clients=%v owners=%v", h.total, len(h.streams), h.clients, h.streamOwners)
	}
}

func TestLiveLeaseLifecycle(t *testing.T) {
	h, clock := newTestHub(10, 10)
	const lease = time.Minute
	if h.on("a1") {
		t.Fatal("on before any lease")
	}
	exp, err := h.hold("a1", "owner", "s1", lease)
	if err != nil || !exp.Equal(clock.t.Add(lease)) {
		t.Fatalf("hold: %v %v", exp, err)
	}
	if !h.on("a1") || h.on("a2") {
		t.Fatal("lease on the wrong artifact")
	}
	// Renewals every third of the lease keep it on indefinitely.
	for range 10 {
		clock.advance(lease / 3)
		if !h.on("a1") {
			t.Fatal("lapsed while renewed")
		}
		if _, err := h.hold("a1", "owner", "s1", lease); err != nil {
			t.Fatal(err)
		}
	}
	// A watch that dies: the lease lapses at its expiry, not before.
	clock.advance(lease - time.Nanosecond)
	if !h.on("a1") {
		t.Fatal("lapsed early")
	}
	clock.advance(time.Nanosecond)
	if h.on("a1") {
		t.Fatal("still on after the lease expired")
	}
	h.prune()
	if h.nLeases != 0 || len(h.leases) != 0 {
		t.Fatalf("prune left %d leases in %d artifacts", h.nLeases, len(h.leases))
	}

	// Two sessions: releasing one keeps live mode on; releasing the other
	// ends it. Releasing an unknown session is harmless.
	h.hold("a1", "owner", "s1", lease)
	h.hold("a1", "owner", "s2", lease)
	h.release("a1", "s1")
	h.release("a1", "nope")
	if !h.on("a1") {
		t.Fatal("off while a second session holds it")
	}
	h.release("a1", "s2")
	if h.on("a1") || h.nLeases != 0 {
		t.Fatalf("on after both sessions released (%d leases)", h.nLeases)
	}
}

func TestLiveLeaseBounds(t *testing.T) {
	h, clock := newTestHub(10, 10)
	for i := range maxLiveSessions {
		if _, err := h.hold("a1", "owner", string(rune('a'+i)), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.hold("a1", "owner", "extra", time.Minute)
	if apperr.As(err).Kind != apperr.KindConflict {
		t.Fatalf("session over the cap: %v", err)
	}
	// Renewing an existing session is always allowed.
	if _, err := h.hold("a1", "owner", "a", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Expired sessions free their places.
	clock.advance(2 * time.Minute)
	if _, err := h.hold("a1", "owner", "extra", time.Minute); err != nil {
		t.Fatalf("after the old sessions expired: %v", err)
	}

	// The server-wide cap prunes expired leases before refusing.
	h, clock = newTestHub(10, 10)
	for i := range maxLiveLeases {
		if _, err := h.hold(fmt.Sprint(i), fmt.Sprint(i/32), "s", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.hold("0", "0", "s", 2*time.Minute); err != nil {
		t.Fatalf("renew at server capacity: %v", err)
	}
	if _, err := h.hold("a1", "owner", "s", time.Minute); apperr.KindOf(err) != apperr.KindRateLimited {
		t.Fatalf("server full: %v", err)
	}
	clock.advance(time.Minute)
	if _, err := h.hold("a1", "owner", "s", time.Minute); err != nil {
		t.Fatalf("after pruning expired leases: %v", err)
	}
	if h.nLeases != 2 || len(h.leaseOwners) != 2 || h.leaseOwners["0"] != 1 || h.leaseOwners["owner"] != 1 {
		t.Fatalf("global prune counts: leases=%d owners=%v", h.nLeases, h.leaseOwners)
	}
}

func TestLiveStreamCaps(t *testing.T) {
	take := func(h *liveHub, artifact, owner, client string) *LiveStream {
		t.Helper()
		st, err := h.open(artifact, owner, client)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st
	}
	h, _ := newTestHub(20, 3)
	first := take(h, "a1", "owner", "c1")
	for range 2 {
		take(h, "a1", "owner", "c1")
	}
	if _, err := h.open("a1", "owner", "c2"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("per artifact: %v", err)
	}
	// Closing twice frees exactly one place.
	first.Close()
	first.Close()
	if total, perArtifact, perClient := h.counts("a1", "c1"); total != 2 || perArtifact != 2 || perClient != 2 {
		t.Fatalf("after close: total %d artifact %d client %d", total, perArtifact, perClient)
	}
	take(h, "a1", "owner", "c1")

	// The global cap still applies across distinct owners.
	h, _ = newTestHub(5, 3)
	for i := range 5 {
		take(h, fmt.Sprint(i), fmt.Sprint(i), "c1")
	}
	if _, err := h.open("extra", "extra", "c2"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("server total: %v", err)
	}

	// The network cap applies across artifacts and owners.
	h, _ = newTestHub(100, 100)
	for i := range maxStreamsPerClient {
		take(h, fmt.Sprint(i), fmt.Sprint(i), "c1")
	}
	if _, err := h.open("z", "other", "c1"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("per client: %v", err)
	}
	take(h, "z", "other", "c2")
}

func TestLivePokesCoalesceAndShutdown(t *testing.T) {
	h, _ := newTestHub(10, 10)
	st, err := h.open("a1", "owner", "c1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.open("a2", "owner", "c1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	t.Cleanup(other.Close)
	// A stream that isn't reading never blocks the sender: pokes coalesce
	// into one.
	for range 100 {
		h.poke("a1")
	}
	select {
	case <-st.Changed():
	default:
		t.Fatal("no poke delivered")
	}
	select {
	case <-st.Changed():
		t.Fatal("pokes were not coalesced")
	default:
	}
	select {
	case <-other.Changed():
		t.Fatal("another artifact's stream was poked")
	default:
	}
	// Releasing one of two sessions keeps live mode on and sends no poke.
	for _, session := range []string{"s1", "s2"} {
		if _, err := h.hold("a1", "owner", session, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	h.release("a1", "s1")
	if !h.on("a1") {
		t.Fatal("releasing one session ended live mode")
	}
	select {
	case <-st.Changed():
		t.Fatal("release poked while a second session still holds live mode")
	default:
	}
	// No timer or keep-alive can satisfy this synchronous notification check.
	h.release("a1", "s2")
	if h.on("a1") {
		t.Fatal("live mode on after the last session released")
	}
	select {
	case <-st.Changed():
	default:
		t.Fatal("last release did not poke immediately")
	}

	h.shutdown()
	h.shutdown()
	select {
	case <-st.Quit():
	default:
		t.Fatal("quit not closed at shutdown")
	}
	if _, err := h.open("a1", "owner", "c2"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("open after shutdown: %v", err)
	}
}
