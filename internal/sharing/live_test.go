package sharing

import (
	"errors"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
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

func TestLiveLeaseLifecycle(t *testing.T) {
	h, clock := newTestHub(10, 10)
	const lease = time.Minute
	if h.on("a1") {
		t.Fatal("on before any lease")
	}
	exp, err := h.hold("a1", "s1", lease)
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
		if _, err := h.hold("a1", "s1", lease); err != nil {
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
	h.hold("a1", "s1", lease)
	h.hold("a1", "s2", lease)
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
		if _, err := h.hold("a1", string(rune('a'+i)), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.hold("a1", "extra", time.Minute)
	if apperr.As(err).Kind != apperr.KindConflict {
		t.Fatalf("session over the cap: %v", err)
	}
	// Renewing an existing session is always allowed.
	if _, err := h.hold("a1", "a", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Expired sessions free their places.
	clock.advance(2 * time.Minute)
	if _, err := h.hold("a1", "extra", time.Minute); err != nil {
		t.Fatalf("after the old sessions expired: %v", err)
	}

	// The server-wide cap prunes expired leases before refusing.
	h, clock = newTestHub(10, 10)
	h.nLeases = maxLiveLeases // "old" and others, unexpired
	h.leases["old"] = map[string]time.Time{"s": clock.t.Add(time.Second)}
	if _, err := h.hold("a1", "s", time.Minute); err == nil || apperr.As(err).Kind != apperr.KindRateLimited {
		t.Fatalf("server full: %v", err)
	}
	clock.advance(2 * time.Second)
	if _, err := h.hold("a1", "s", time.Minute); err != nil {
		t.Fatalf("after pruning an expired lease: %v", err)
	}
}

func TestLiveStreamCaps(t *testing.T) {
	h, _ := newTestHub(5, 3)
	var open []*LiveStream
	take := func(artifact, client string) error {
		st, err := h.open(artifact, client)
		if err == nil {
			open = append(open, st)
		}
		return err
	}
	for range 3 {
		if err := take("a1", "c1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := take("a1", "c2"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("per artifact: %v", err)
	}
	for range 2 {
		if err := take("a2", "c2"); err != nil {
			t.Fatal(err)
		}
	}
	if err := take("a3", "c3"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("server total: %v", err)
	}
	// Closing (twice is harmless) frees exactly one place.
	open[0].Close()
	open[0].Close()
	if total, perArtifact, perClient := h.counts("a1", "c1"); total != 4 || perArtifact != 2 || perClient != 2 {
		t.Fatalf("after close: total %d artifact %d client %d", total, perArtifact, perClient)
	}
	if err := take("a3", "c3"); err != nil {
		t.Fatal(err)
	}

	// Per client network.
	h, _ = newTestHub(100, 100)
	for i := range maxStreamsPerClient {
		if _, err := h.open(string(rune('a'+i)), "c1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.open("z", "c1"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("per client: %v", err)
	}
	if _, err := h.open("z", "c2"); err != nil {
		t.Fatalf("another client: %v", err)
	}
}

func TestLivePokesCoalesceAndShutdown(t *testing.T) {
	h, _ := newTestHub(10, 10)
	st, err := h.open("a1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := h.open("a2", "c1")
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
	// Releasing the last lease pokes the artifact's streams.
	h.hold("a1", "s", time.Minute)
	h.release("a1", "s")
	select {
	case <-st.Changed():
	default:
		t.Fatal("release did not poke")
	}

	h.shutdown()
	h.shutdown()
	select {
	case <-st.Quit():
	default:
		t.Fatal("quit not closed at shutdown")
	}
	if _, err := h.open("a1", "c2"); !errors.Is(err, ErrLiveBusy) {
		t.Fatalf("open after shutdown: %v", err)
	}
}
