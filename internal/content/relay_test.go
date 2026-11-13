package content

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func recv(t *testing.T, s *relaySub) string {
	t.Helper()
	select {
	case p := <-s.ch:
		return p
	case <-time.After(time.Second):
		t.Fatal("no path")
	}
	return ""
}

func empty(t *testing.T, s *relaySub) {
	t.Helper()
	select {
	case p := <-s.ch:
		t.Fatalf("unexpected path %q", p)
	default:
	}
}

// TestRelayScoping: a subscriber sees only its own frame's paths, gets the
// last known path on subscribe, and only the newest when it falls behind.
func TestRelayScoping(t *testing.T) {
	r := newRelay()
	a, ok1 := r.subscribe("frame-a", "c1")
	b, ok2 := r.subscribe("frame-b", "c1")
	if !ok1 || !ok2 {
		t.Fatal("subscribe failed")
	}
	r.publish("frame-a", "/one")
	if got := recv(t, a); got != "/one" {
		t.Fatalf("a got %q", got)
	}
	empty(t, b)
	r.publish("frame-a", "/two")
	r.publish("frame-a", "/three")
	if got := recv(t, a); got != "/three" {
		t.Fatalf("a got %q, want only the newest", got)
	}
	late, _ := r.subscribe("frame-a", "c2")
	if got := recv(t, late); got != "/three" {
		t.Fatalf("late subscriber got %q", got)
	}
	r.publish("", "/x")
	r.publish("frame-a", "/"+strings.Repeat("x", relayMaxPath))
	empty(t, a)
}

func TestRelayLimits(t *testing.T) {
	r := newRelay()
	var subs []*relaySub
	for i := range relayMaxPerClient {
		s, ok := r.subscribe("f"+strconv.Itoa(i), "busy")
		if !ok {
			t.Fatalf("subscription %d refused", i)
		}
		subs = append(subs, s)
	}
	if _, ok := r.subscribe("f-extra", "busy"); ok {
		t.Fatal("per-client limit not enforced")
	}
	if _, ok := r.subscribe("f-other", "other"); !ok {
		t.Fatal("another client was refused")
	}
	r.unsubscribe(subs[0])
	r.unsubscribe(subs[0]) // idempotent
	if _, ok := r.subscribe("f-extra", "busy"); !ok {
		t.Fatal("slot not released")
	}

	total := newRelay()
	for i := range relayMaxStreams {
		if _, ok := total.subscribe("f"+strconv.Itoa(i), "c"+strconv.Itoa(i)); !ok {
			t.Fatalf("stream %d refused", i)
		}
	}
	if _, ok := total.subscribe("f-last", "c-last"); ok {
		t.Fatal("global stream limit not enforced")
	}

	// The frame table stays bounded: the oldest unwatched frame goes first.
	now := time.Unix(0, 0)
	frames := newRelay()
	frames.now = func() time.Time { now = now.Add(time.Second); return now }
	watched, _ := frames.subscribe("f0", "c")
	for i := 1; i <= relayMaxFrames+5; i++ {
		frames.publish("f"+strconv.Itoa(i), "/p")
	}
	if len(frames.frames) > relayMaxFrames {
		t.Fatalf("%d frames kept", len(frames.frames))
	}
	if _, ok := frames.frames["f0"]; !ok || frames.frames["f1"] != nil {
		t.Fatal("evicted a watched frame or kept the oldest unwatched one")
	}
	frames.publish("f0", "/still")
	if got := recv(t, watched); got != "/still" {
		t.Fatalf("watched frame got %q", got)
	}

	frames.close()
	if _, ok := frames.subscribe("f0", "c"); ok {
		t.Fatal("subscribed after close")
	}
	select {
	case <-frames.done:
	default:
		t.Fatal("done not closed")
	}
}
