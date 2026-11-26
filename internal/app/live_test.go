package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// Live reload (DESIGN.md "Live reload"): the lease API, the stream on the
// artifact's own host, its access check and limits, and view counting.

const liveSession = "watch-session-0001"

// stream is what the viewer page's EventSource sends.
var stream = map[string]string{"Sec-Fetch-Dest": "empty", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin", "Accept": "text/event-stream"}

// sseEvent is one server-sent event, or a comment line (Comment set).
type sseEvent struct {
	Event, Data, Retry, Comment string
}

// liveStream is an open stream; its events arrive on Events, which is
// closed when the server ends the stream.
type liveStream struct {
	resp   *http.Response
	Events chan sseEvent
	cancel context.CancelFunc
}

// openLive requests the stream. When the answer is not a stream it returns
// the response and its body, and no stream.
func (h *harness) openLive(artifactURL string, hdr map[string]string, cookies ...*http.Cookie) (*liveStream, *http.Response, string) {
	h.t.Helper()
	u, _ := url.Parse(artifactURL)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/__redrop/live", nil)
	req.Host = u.Host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b := body(resp)
		resp.Body.Close()
		cancel()
		return nil, resp, b
	}
	s := &liveStream{resp: resp, Events: make(chan sseEvent, 16), cancel: cancel}
	h.t.Cleanup(s.close)
	go func() {
		defer close(s.Events)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev != (sseEvent{}) {
					s.Events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
				s.Events <- sseEvent{Comment: strings.TrimSpace(line[1:])}
			case strings.HasPrefix(line, "event: "):
				ev.Event = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				ev.Data = line[len("data: "):]
			case strings.HasPrefix(line, "retry: "):
				ev.Retry = line[len("retry: "):]
			}
		}
	}()
	return s, resp, ""
}

func (s *liveStream) close() {
	s.cancel()
	s.resp.Body.Close()
}

// next returns the next event or comment. The bound only catches a hang.
func (s *liveStream) next(t *testing.T) (sseEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-s.Events:
		return ev, ok
	case <-time.After(30 * time.Second):
		t.Fatal("no event from the live stream within 30s")
		return sseEvent{}, false
	}
}

// nextEvent skips keep-alive comments.
func (s *liveStream) nextEvent(t *testing.T) (sseEvent, bool) {
	t.Helper()
	for {
		ev, ok := s.next(t)
		if !ok || ev.Comment == "" {
			return ev, ok
		}
	}
}

// expectVersion reads the next event and checks it announces version n.
func (s *liveStream) expectVersion(t *testing.T, n int) {
	t.Helper()
	ev, ok := s.nextEvent(t)
	if !ok || ev.Event != "version" {
		t.Fatalf("want a version event for %d, got %+v (open=%v)", n, ev, ok)
	}
	var v struct {
		Version   int       `json:"version"`
		Published time.Time `json:"published"`
		Size      string    `json:"size"`
		Files     int       `json:"files"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &v); err != nil || v.Version != n || v.Published.IsZero() || v.Size == "" || v.Files != 1 {
		t.Fatalf("version event %q: %+v %v, want version %d", ev.Data, v, err, n)
	}
}

// expectEnd reads the "end" event and the stream's close.
func (s *liveStream) expectEnd(t *testing.T) {
	t.Helper()
	// Normal access/lease endings must not ask for reconnect or emit
	// unrelated events. The hub test separately pins the synchronous poke.
	if ev, ok := s.next(t); !ok || ev.Event != "end" || ev.Data != "{}" {
		t.Fatalf("want end, got %+v (open=%v)", ev, ok)
	}
	if ev, ok := s.next(t); ok {
		t.Fatalf("stream still open after end: %+v", ev)
	}
}

func (h *harness) holdLive(token, id, session string) *http.Response {
	h.t.Helper()
	return h.bearer("PUT", "/api/v1/artifacts/"+id+"/live/"+session, token)
}

func (h *harness) releaseLive(token, id, session string) *http.Response {
	h.t.Helper()
	return h.bearer("DELETE", "/api/v1/artifacts/"+id+"/live/"+session, token)
}

func (h *harness) bearer(method, path, token string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// liveArtifact shares an HTML page and turns live mode on for it.
func (h *harness) liveArtifact(c *client.Client, token string, opts client.PublishOptions) *api.Artifact {
	h.t.Helper()
	a := shareFile(h.t, c, "live.html", "<!doctype html><title>Live</title><p>one</p>", opts)
	expectStatus(h.t, h.holdLive(token, a.ID, liveSession), http.StatusOK, "hold live")
	return a
}

func publish(t *testing.T, c *client.Client, id, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "live.html")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(context.Background(), id, &client.Source{Path: p}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLiveLeaseAPI(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	a := shareFile(t, c, "a.html", "<p>a</p>", client.PublishOptions{})

	started := time.Now()
	r := h.holdLive(token, a.ID, liveSession)
	expectStatus(t, r, http.StatusOK, "hold")
	l := decode[api.LiveLease](t, r)
	if l.RenewAfterSeconds != 20 || !l.ExpiresAt.After(started) {
		t.Fatalf("lease %+v: want a 60 s lease renewed every 20 s", l)
	}
	// Renewing is the same call; the client library does the same.
	if got, err := c.HoldLive(context.Background(), a.ID, liveSession); err != nil || got.RenewAfterSeconds != 20 {
		t.Fatalf("renew: %+v %v", got, err)
	}
	expectStatus(t, h.releaseLive(token, a.ID, liveSession), http.StatusOK, "release")
	expectStatus(t, h.releaseLive(token, a.ID, liveSession), http.StatusOK, "release again")

	for _, bad := range []string{"short", strings.Repeat("x", 65), "has.dots-in-the-session-id"} {
		expectStatus(t, h.holdLive(token, a.ID, bad), http.StatusBadRequest, "session "+bad)
	}
	// Not without a token, and not on another user's artifact.
	r = h.bearer("PUT", "/api/v1/artifacts/"+a.ID+"/live/"+liveSession, "rdp_wrong")
	expectStatus(t, r, http.StatusUnauthorized, "bad token")
	other := h.secondUser("bob")
	expectStatus(t, h.holdLive(other, a.ID, liveSession), http.StatusNotFound, "another user's artifact")
	expectStatus(t, h.releaseLive(other, a.ID, liveSession), http.StatusNotFound, "another user's release")

	// Refused, with a reason, where there is no viewer frame to reload.
	off := false
	direct := shareFile(t, c, "d.html", "<p>d</p>", client.PublishOptions{InfoButton: &off})
	r = h.holdLive(token, direct.ID, liveSession)
	expectStatus(t, r, http.StatusConflict, "info button off")
	if b := body(r); !strings.Contains(b, "info button") {
		t.Fatalf("info off reason: %s", b)
	}
	file := shareFile(t, c, "data.csv", "a,b\n", client.PublishOptions{})
	r = h.holdLive(token, file.ID, liveSession)
	expectStatus(t, r, http.StatusConflict, "file artifact")
	if b := body(r); !strings.Contains(b, "file") {
		t.Fatalf("file reason: %s", b)
	}

	// At most 8 watch sessions per artifact.
	for i := range 8 {
		expectStatus(t, h.holdLive(token, a.ID, "watch-session-"+strings.Repeat("abcdefgh"[i:i+1], 4)), http.StatusOK, "session")
	}
	expectStatus(t, h.holdLive(token, a.ID, "watch-session-ninth"), http.StatusConflict, "ninth session")
}

func TestLiveOwnerShareLeavesAnotherWatchLive(t *testing.T) {
	t.Setenv("REDROP_LIVE_MAX_STREAMS", "4") // one stream per owner
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	var first *api.Artifact
	for range 4 {
		a := shareFile(t, c, "a.html", "<p>a</p>", client.PublishOptions{})
		if first == nil {
			first = a
		}
		for i := range 8 {
			session := "watch-session-" + strings.Repeat("abcdefgh"[i:i+1], 4)
			expectStatus(t, h.holdLive(token, a.ID, session), http.StatusOK, "owner's lease share")
		}
	}
	extra := shareFile(t, c, "extra.html", "<p>extra</p>", client.PublishOptions{})
	expectStatus(t, h.holdLive(token, extra.ID, liveSession), http.StatusConflict, "33rd owner lease")
	s, r, b := h.openLive(first.URL, stream)
	if s == nil {
		t.Fatalf("first owner's stream: %d %s", r.StatusCode, b)
	}
	s.expectVersion(t, 1)
	if s, r, _ := h.openLive(first.URL, stream); s != nil || r.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("first owner exceeded their stream share")
	}

	// The same lease call watch uses still succeeds for another account,
	// and its viewer receives updates while the first account is at capacity.
	otherToken := h.secondUser("bob")
	other := client.New(h.srv.URL, otherToken)
	a := shareFile(t, other, "b.html", "<p>b</p>", client.PublishOptions{})
	if _, err := other.HoldLive(context.Background(), a.ID, liveSession); err != nil {
		t.Fatal(err)
	}
	s, r, b = h.openLive(a.URL, stream)
	if s == nil {
		t.Fatalf("another owner's stream: %d %s", r.StatusCode, b)
	}
	s.expectVersion(t, 1)
	publish(t, other, a.ID, "<p>updated</p>")
	s.expectVersion(t, 2)
}

func TestLiveViewerPage(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	a := shareFile(t, c, "a.html", "<p>a</p>", client.PublishOptions{Feedback: true, OldVersions: true})
	self := strings.TrimSuffix(a.URL, "/")

	// Off: nothing live on the page or in its CSP.
	r, b := h.page("GET", a.URL, "/", nav)
	if strings.Contains(b, "rd-live") || strings.Contains(r.Header.Get("Content-Security-Policy"), "/__redrop/live") {
		t.Fatal("live element or CSP without a lease")
	}

	expectStatus(t, h.holdLive(token, a.ID, liveSession), http.StatusOK, "hold")
	r, b = h.page("GET", a.URL, "/", nav)
	if !strings.Contains(b, `id="rd-live"`) || !strings.Contains(b, `data-src="/__redrop/live"`) || !strings.Contains(b, `data-version="1"`) {
		t.Fatalf("viewer page without the live row: %s", b)
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "connect-src "+self+"/__redrop/feedback "+self+"/__redrop/live") {
		t.Fatalf("CSP: %s", r.Header.Get("Content-Security-Policy"))
	}
	// Not on an older version, a direct view, the frame's own bytes, or
	// a file page.
	publish(t, c, a.ID, "<p>b</p>")
	for name, path := range map[string]string{"older version": "/?redrop-version=1", "direct view": "/?redrop-info=0"} {
		if _, b := h.page("GET", a.URL, path, nav); strings.Contains(b, "rd-live") {
			t.Errorf("%s has the live row", name)
		}
	}
	if _, b := h.page("GET", a.URL, "/", inFrame); strings.Contains(b, "rd-live") {
		t.Error("the artifact's bytes changed")
	}
	if _, b := h.page("GET", a.URL, "/", nav); !strings.Contains(b, `data-version="2"`) {
		t.Error("the live row names the shown version")
	}

	expectStatus(t, h.releaseLive(token, a.ID, liveSession), http.StatusOK, "release")
	if _, b := h.page("GET", a.URL, "/", nav); strings.Contains(b, "rd-live") {
		t.Fatal("live row after the lease was released")
	}
}

// With live mode off, and for anyone without access, the stream is the
// identical miss: an unknown reserved path's answer.
func TestLiveStreamIdenticalMiss(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	a := shareFile(t, c, "a.html", "<p>a</p>", client.PublishOptions{})
	unknown, ub := h.page("GET", a.URL, "/__redrop/nothing-here", stream)
	expectStatus(t, unknown, http.StatusNotFound, "unknown reserved path")
	check := func(what, artifactURL string, hdr map[string]string, cookies ...*http.Cookie) {
		t.Helper()
		s, r, b := h.openLive(artifactURL, hdr, cookies...)
		if s != nil {
			t.Fatalf("%s: got a stream", what)
		}
		sameResponse(t, what, r, b, unknown, ub)
	}
	check("live off", a.URL, stream)

	expectStatus(t, h.holdLive(token, a.ID, liveSession), http.StatusOK, "hold")
	s, _, _ := h.openLive(a.URL, stream)
	if s == nil {
		t.Fatal("no stream with live mode on")
	}
	s.close()
	check("another site", a.URL, with(stream, "Sec-Fetch-Site", "same-site"))
	check("cross-site", a.URL, with(stream, "Sec-Fetch-Site", "cross-site"))

	// A password: only with the visit grant.
	p := shareFile(t, c, "p.html", "<p>p</p>", client.PublishOptions{Password: "hunter22"})
	expectStatus(t, h.holdLive(token, p.ID, liveSession), http.StatusOK, "hold p")
	check("password not entered", p.URL, stream)
	open := h.content("POST", p.URL, "/__redrop/open", url.Values{"password": {"hunter22"}, "next": {"/"}}, "")
	expectStatus(t, open, http.StatusSeeOther, "unlock")
	visit := cookieNamed(open, "rd_visit")
	if s, r, b := h.openLive(p.URL, stream, visit); s == nil {
		t.Fatalf("with the visit grant: %d %s", r.StatusCode, b)
	} else {
		s.close()
	}

	// Expired and revoked links.
	h.expire(a.ID)
	check("expired", a.URL, stream)
	yes := true
	if _, err := c.UpdateArtifact(context.Background(), p.ID, &api.UpdateArtifactRequest{Revoked: &yes}); err != nil {
		t.Fatal(err)
	}
	check("revoked", p.URL, stream, visit)

	// The info button turned off while the lease is held.
	off := false
	d := shareFile(t, c, "d.html", "<p>d</p>", client.PublishOptions{})
	expectStatus(t, h.holdLive(token, d.ID, liveSession), http.StatusOK, "hold d")
	if _, err := c.UpdateArtifact(context.Background(), d.ID, &api.UpdateArtifactRequest{InfoButton: &off}); err != nil {
		t.Fatal(err)
	}
	check("info button off", d.URL, stream)
}

func TestLiveStreamEvents(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	a := h.liveArtifact(c, token, client.PublishOptions{})

	s, r, b := h.openLive(a.URL, stream)
	if s == nil {
		t.Fatalf("no stream: %d %s", r.StatusCode, b)
	}
	if r.Header.Get("Cache-Control") != "private, no-cache" || r.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("stream headers: %v", r.Header)
	}
	noCORS(t, r, "stream")
	// The first event carries the reconnect delay and the current version.
	first, ok := s.nextEvent(t)
	if !ok || first.Retry != "25000" || first.Event != "version" || !strings.Contains(first.Data, `"version":1`) {
		t.Fatalf("first event %+v", first)
	}
	publish(t, c, a.ID, "<p>two</p>")
	s.expectVersion(t, 2)
	if _, err := c.ActivateVersion(context.Background(), a.ID, 1); err != nil {
		t.Fatal(err)
	}
	s.expectVersion(t, 1)
	// A settings change that keeps access and live mode sends nothing new.
	title := "Renamed"
	if _, err := c.UpdateArtifact(context.Background(), a.ID, &api.UpdateArtifactRequest{Title: &title}); err != nil {
		t.Fatal(err)
	}
	publish(t, c, a.ID, "<p>three</p>")
	s.expectVersion(t, 3)

	// Observe the service's notification independently of the HTTP stream's
	// periodic rechecks. Removing the release poke must fail this test even
	// if a later keep-alive would discover that the lease is gone.
	record, err := h.app.db.ArtifactByID(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := h.app.Sharing.OpenLiveStream(record, "release-observer")
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	expectStatus(t, h.releaseLive(token, a.ID, liveSession), http.StatusOK, "release")
	select {
	case <-observer.Changed():
	default:
		t.Fatal("release returned without notifying the open streams")
	}
	s.expectEnd(t)

	// Revoking ends it too, and so does a password change, for streams
	// opened before them.
	expectStatus(t, h.holdLive(token, a.ID, liveSession), http.StatusOK, "hold again")
	s, _, _ = h.openLive(a.URL, stream)
	s.expectVersion(t, 3)
	pw := "hunter22"
	if _, err := c.UpdateArtifact(context.Background(), a.ID, &api.UpdateArtifactRequest{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	s.expectEnd(t)

	// Two sessions: releasing one leaves live mode on.
	b2 := h.liveArtifact(c, token, client.PublishOptions{})
	expectStatus(t, h.holdLive(token, b2.ID, "watch-session-0002"), http.StatusOK, "second session")
	s, _, _ = h.openLive(b2.URL, stream)
	s.expectVersion(t, 1)
	expectStatus(t, h.releaseLive(token, b2.ID, liveSession), http.StatusOK, "release one")
	publish(t, c, b2.ID, "<p>still live</p>")
	s.expectVersion(t, 2)

	// Graceful shutdown asks for exactly one delayed reconnect. A dropped
	// connection without this event must not trigger automatic requests.
	h.app.Sharing.EndLiveStreams()
	if ev, ok := s.next(t); !ok || ev.Event != "end" || ev.Data != `{"reconnect":true}` {
		t.Fatalf("shutdown reconnect event: %+v (open=%v)", ev, ok)
	}
	if ev, ok := s.next(t); ok {
		t.Fatalf("stream open after shutdown began: %+v", ev)
	}
	if s, r, _ := h.openLive(b2.URL, stream); s != nil || r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new stream during shutdown: %d", r.StatusCode)
	}
}

func TestLiveStreamKeepAlive(t *testing.T) {
	t.Setenv("REDROP_LIVE_KEEPALIVE", "1s")
	h := newHarness(t)
	token := h.setupAdmin()
	a := h.liveArtifact(client.New(h.srv.URL, token), token, client.PublishOptions{})
	s, r, b := h.openLive(a.URL, stream)
	if s == nil {
		t.Fatalf("no stream: %d %s", r.StatusCode, b)
	}
	s.expectVersion(t, 1)
	if ev, ok := s.next(t); !ok || ev.Comment != "keep-alive" {
		t.Fatalf("want a keep-alive, got %+v (open=%v)", ev, ok)
	}
}

func TestLiveStreamLifecycleEnds(t *testing.T) {
	for _, change := range []string{"release", "owner revoke", "admin revoke", "rotation", "deletion"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t)
			token := h.setupAdmin()
			c := client.New(h.srv.URL, token)
			a := h.liveArtifact(c, token, client.PublishOptions{})
			s, r, b := h.openLive(a.URL, stream)
			if s == nil {
				t.Fatalf("no stream: %d %s", r.StatusCode, b)
			}
			s.expectVersion(t, 1)
			ctx := context.Background()
			var err error
			switch change {
			case "release":
				expectStatus(t, h.releaseLive(token, a.ID, liveSession), http.StatusOK, change)
			case "owner revoke":
				yes := true
				_, err = c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{Revoked: &yes})
			case "admin revoke":
				expectStatus(t, h.do("PATCH", "/api/v1/admin/artifacts/"+a.ID, api.AdminArtifactUpdate{Revoked: true}, true), http.StatusOK, change)
			case "rotation":
				_, err = c.RotateArtifact(ctx, a.ID)
			case "deletion":
				err = c.DeleteArtifact(ctx, a.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			s.expectEnd(t)
		})
	}
}

func TestLiveStreamLimits(t *testing.T) {
	t.Setenv("REDROP_LIVE_MAX_STREAMS", "16")
	t.Setenv("REDROP_LIVE_MAX_STREAMS_PER_ARTIFACT", "2")
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	a := h.liveArtifact(c, token, client.PublishOptions{})
	b := h.liveArtifact(c, token, client.PublishOptions{})
	d := h.liveArtifact(c, token, client.PublishOptions{})

	open := func(art *api.Artifact) *liveStream {
		t.Helper()
		s, r, body := h.openLive(art.URL, stream)
		if s == nil {
			t.Fatalf("stream refused: %d %s", r.StatusCode, body)
		}
		s.expectVersion(t, 1)
		return s
	}
	refused := func(art *api.Artifact, what string) {
		t.Helper()
		s, r, _ := h.openLive(art.URL, stream)
		if s != nil || r.StatusCode != http.StatusServiceUnavailable || r.Header.Get("Retry-After") == "" {
			t.Fatalf("%s: want 503 with Retry-After, got %d", what, r.StatusCode)
		}
		noCORS(t, r, what)
	}
	a1 := open(a)
	open(a)
	refused(a, "third stream on one artifact")
	open(b)
	open(b)
	refused(d, "fifth stream for one owner")
	// Each owner can hold a quarter of the global cap. Fill the remaining
	// shares with other accounts before exercising the server-wide cap.
	for _, name := range []string{"bob", "carol", "dana"} {
		tok := h.secondUser(name)
		other := client.New(h.srv.URL, tok)
		for range 2 {
			art := h.liveArtifact(other, tok, client.PublishOptions{})
			open(art)
			open(art)
		}
	}
	refused(d, "seventeenth stream on the server")
	// A closed stream frees its slot.
	a1.close()
	waitFor(t, "the closed stream's slot", func() bool {
		s, _, _ := h.openLive(a.URL, stream)
		if s != nil {
			s.close()
		}
		return s != nil
	})
}

// waitFor polls cond; the bound only catches a hang.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s: not within 30s", what)
}

func TestLiveStreamsPerClient(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	a := h.liveArtifact(c, token, client.PublishOptions{})
	for range 16 {
		if s, r, _ := h.openLive(a.URL, stream); s == nil {
			t.Fatalf("stream refused: %d", r.StatusCode)
		}
	}
	if s, r, _ := h.openLive(a.URL, stream); s != nil || r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("17th stream from one client: %d", r.StatusCode)
	}
}

// Reloading never uses a view: neither the stream nor the frame's reload
// counts, on an ungated link, a view-limited link, or a one-time link.
func TestLiveReloadUsesNoViews(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)

	// Ungated: the viewer page counts once; the stream and reloads don't,
	// even without the seen cookie.
	u := h.liveArtifact(c, token, client.PublishOptions{})
	r, _, src := h.viewerLoad(u.URL, "/")
	seen := cookieNamed(r, "rd_seen")
	s, _, _ := h.openLive(u.URL, stream, seen)
	s.expectVersion(t, 1)
	publish(t, c, u.ID, "<p>two</p>")
	s.expectVersion(t, 2)
	for range 3 {
		if _, b := h.page("GET", u.URL, src, inFrame, seen); b != "<p>two</p>" {
			t.Fatalf("reloaded frame: %q", b)
		}
		h.page("GET", u.URL, src, inFrame)
	}
	if n := h.views(c, u.ID); n != 1 {
		t.Fatalf("ungated views = %d, want 1", n)
	}

	for _, maxViews := range []int{1, 2} {
		l := h.liveArtifact(c, token, client.PublishOptions{MaxViews: maxViews})
		open := h.content("POST", l.URL, "/__redrop/open", url.Values{"next": {"/"}}, "")
		expectStatus(t, open, http.StatusSeeOther, "open")
		visit := cookieNamed(open, "rd_visit")
		_, _, src := h.viewerLoad(l.URL, "/", visit)
		s, r, b := h.openLive(l.URL, stream, visit)
		if s == nil {
			t.Fatalf("max views %d: no stream for the visitor: %d %s", maxViews, r.StatusCode, b)
		}
		s.expectVersion(t, 1)
		publish(t, c, l.ID, "<p>two</p>")
		s.expectVersion(t, 2)
		for range 3 {
			if _, b := h.page("GET", l.URL, src, inFrame, visit); b != "<p>two</p>" {
				t.Fatalf("max views %d: reloaded frame: %q", maxViews, b)
			}
		}
		if n := h.views(c, l.ID); n != 1 {
			t.Fatalf("max views %d: views = %d, want 1", maxViews, n)
		}
		// Without the visit, the stream is the miss: a one-time link stays
		// used up for everyone else.
		if s, _, _ := h.openLive(l.URL, stream); s != nil {
			t.Fatalf("max views %d: stream without a visit", maxViews)
		}
	}
}
