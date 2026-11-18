package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// Tunnels end to end in one process (DESIGN.md "Tunnels"): the real server
// handler, the CLI's agent (client.RunTunnel) over a real WebSocket, and a
// local httptest app.

// waitFor polls cond until it holds. The bound only catches a real hang.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// noDelay retries at once, so reconnect tests wait on events, not time.
type noDelay struct{}

func (noDelay) Next(int) time.Duration { return time.Millisecond }

// seenRequest is what the local app received.
type seenRequest struct {
	Method, Path, RawQuery, Host, Body string
	Header                             http.Header
}

type tunnelFixture struct {
	t    *testing.T
	h    *harness
	c    *client.Client
	app  *httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

// newTunnelFixture runs a local app: handler, or one that records each
// request and answers "hello from the app" with a few headers to clean.
func newTunnelFixture(t *testing.T, handler http.Handler) *tunnelFixture {
	t.Helper()
	h := newHarness(t)
	f := &tunnelFixture{t: t, h: h, c: client.New(h.srv.URL, h.setupAdmin())}
	if handler == nil {
		handler = http.HandlerFunc(f.record)
	}
	f.app = httptest.NewServer(handler)
	t.Cleanup(f.app.Close)
	return f
}

func (f *tunnelFixture) record(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, seenRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Host: r.Host, Body: string(b), Header: r.Header.Clone()})
	f.mu.Unlock()
	hd := w.Header()
	hd.Add("Set-Cookie", "app=1; Domain=content.test; Path=/; HttpOnly")
	hd.Add("Set-Cookie", "rd_visit=forged; Path=/")
	hd.Set("Cache-Control", "public, max-age=31536000")
	hd.Set("Cdn-Cache-Control", "max-age=600")
	hd.Set("X-Robots-Tag", "all")
	hd.Set("Content-Security-Policy", "script-src 'self'")
	hd.Set("Content-Type", "text/plain")
	io.WriteString(w, "hello from the app "+r.Method+" "+string(b))
}

func (f *tunnelFixture) last() seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		f.t.Fatal("the app saw no request")
	}
	return f.seen[len(f.seen)-1]
}

func (f *tunnelFixture) create(req api.CreateTunnelRequest) *api.Artifact {
	f.t.Helper()
	a, err := f.c.CreateTunnel(context.Background(), &req)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *tunnelFixture) target() *client.TunnelTarget {
	f.t.Helper()
	tg, err := client.ResolveTunnelTarget(context.Background(), strings.TrimPrefix(f.app.URL, "http://"), false, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return tg
}

// running is a connected redrop tunnel.
type running struct {
	stop   context.CancelFunc
	done   chan error
	events chan client.TunnelEvent
}

// connect runs the CLI's agent for a and waits until the server has it.
func (f *tunnelFixture) connect(c *client.Client, a *api.Artifact) *running {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stop: cancel, done: make(chan error, 1), events: make(chan client.TunnelEvent, 64)}
	go func() {
		r.done <- c.RunTunnel(ctx, client.TunnelOptions{ID: a.ID, Target: f.target(), Retrier: noDelay{},
			OnEvent: func(e client.TunnelEvent) {
				select {
				case r.events <- e:
				default:
				}
			}})
	}()
	f.t.Cleanup(func() {
		cancel()
		<-r.done
	})
	waitFor(f.t, "the tunnel to come online", func() bool { return f.h.app.Tunnels.Online(a.ID) })
	return r
}

// ended waits for RunTunnel to return and returns its error.
func (r *running) ended(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("redrop tunnel did not stop")
		return nil
	}
}

var (
	frame = map[string]string{"Sec-Fetch-Dest": "iframe", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin"}
	doc   = map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "none"}
)

func TestTunnelForwardsThroughTheLink(t *testing.T) {
	f := newTunnelFixture(t, nil)
	a := f.create(api.CreateTunnelRequest{Title: "dev"})
	if a.Kind != api.KindTunnel || a.Online || !wordLinkRE.MatchString(a.URL) {
		t.Fatalf("created: %+v", a)
	}
	f.connect(f.c, a)

	// The frame's request (and any subresource) reaches the app.
	r, b := f.h.send("GET", a.URL, "/hello/world?x=1&redrop-info=0&y", nil, with(frame,
		"X-Forwarded-For", "203.0.113.9, 198.51.100.7", "Forwarded", "for=192.0.2.1", "X-Real-Ip", "192.0.2.2",
		"Cf-Connecting-Ip", "192.0.2.3", "Authorization", "Basic YXBwOmFwcA==", "Cookie", "rd_visit=secret; app=1; rd_seen=x"))
	expectStatus(t, r, 200, "through the tunnel")
	if b != "hello from the app GET " {
		t.Fatalf("body %q", b)
	}
	got := f.last()
	if got.Path != "/hello/world" || got.RawQuery != "x=1&y" {
		t.Fatalf("app saw %s?%s", got.Path, got.RawQuery)
	}
	if want := strings.TrimPrefix(f.app.URL, "http://"); got.Host != want {
		t.Fatalf("Host %q, want the target %q", got.Host, want)
	}
	// The resolved client (the harness trusts its loopback peer), never the
	// visitor's own list.
	for k, want := range map[string]string{
		"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Host": hostOf(t, a.URL), "X-Forwarded-Proto": "http",
		"Forwarded": "", "X-Real-Ip": "", "Cf-Connecting-Ip": "", "Authorization": "Basic YXBwOmFwcA==", "Cookie": "app=1",
	} {
		if v := strings.Join(got.Header.Values(k), ","); v != want {
			t.Errorf("app got %s: %q, want %q", k, v, want)
		}
	}
	// What the visitor gets back.
	if c := r.Header.Values("Set-Cookie"); len(c) != 1 || c[0] != "app=1; Path=/; HttpOnly" {
		t.Errorf("Set-Cookie %q: want the app's cookie host-only and redrop's name refused", c)
	}
	for k, want := range map[string]string{
		"Cache-Control": "private, no-cache", "Cdn-Cache-Control": "", "X-Robots-Tag": "noindex, nofollow, noarchive, nosnippet, noimageindex",
		"Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff", "Permissions-Policy": "",
		"Content-Security-Policy": "frame-ancestors 'self' http://app.test,script-src 'self'",
	} {
		if v := strings.Join(r.Header.Values(k), ","); v != want {
			t.Errorf("visitor got %s: %q, want %q", k, v, want)
		}
	}

	// Other methods and their bodies go through too.
	r, b = f.h.send("PUT", a.URL, "/items/1", "payload", frame)
	expectStatus(t, r, 200, "PUT")
	if b != "hello from the app PUT payload" || f.last().Body != "payload" {
		t.Fatalf("PUT: %q, app saw %q", b, f.last().Body)
	}

	// A top-level page load gets the viewer, which frames the same path.
	r, b = f.h.send("GET", a.URL, "/hello?x=1", nil, doc)
	expectStatus(t, r, 200, "viewer")
	if !strings.Contains(b, `<iframe id="rd-frame" class="rd-frame" src="/hello?x=1"`) || !strings.Contains(b, "Live app") || strings.Contains(b, "Versions") {
		t.Fatalf("viewer page: %s", b)
	}
	// Only the viewer load counted, once.
	got2, _ := f.c.GetArtifact(context.Background(), a.ID)
	if got2.Views != 1 || !got2.Online {
		t.Fatalf("after one page view: views %d online %v", got2.Views, got2.Online)
	}
	// Redrop's reserved paths never reach the app.
	before := len(f.seen)
	r, _ = f.h.send("GET", a.URL, "/__redrop/viewer.js", nil, nil)
	expectStatus(t, r, 200, "viewer script")
	r, _ = f.h.send("GET", a.URL, "/__redrop/nope", nil, frame)
	expectStatus(t, r, 404, "unknown reserved path")
	r, _ = f.h.send("POST", a.URL, "/__redrop/nope", "x", frame)
	expectStatus(t, r, 405, "POST to a reserved path")
	f.mu.Lock()
	if len(f.seen) != before {
		t.Fatalf("a reserved path reached the app: %+v", f.seen[before:])
	}
	f.mu.Unlock()
}

func TestTunnelOfflineOnlyAfterTheAccessCheck(t *testing.T) {
	f := newTunnelFixture(t, nil)
	open := f.create(api.CreateTunnelRequest{})
	locked := f.create(api.CreateTunnelRequest{Password: "tunnel-pw"})
	unknown := strings.Replace(open.URL, open.URL[len("http://"):strings.Index(open.URL, ".")], "amber-otter-quilt-river", 1)
	missResp, missBody := f.h.send("GET", unknown, "/", nil, frame)
	expectStatus(t, missResp, 404, "unknown")

	r, b := f.h.send("GET", open.URL, "/", nil, frame)
	expectStatus(t, r, 503, "offline open tunnel")
	if !strings.Contains(b, "This tunnel is offline") {
		t.Fatalf("offline page: %s", b)
	}
	// Without the password: the gate, never the offline state.
	r, b = f.h.send("GET", locked.URL, "/", nil, frame)
	expectStatus(t, r, 200, "gate")
	if !strings.Contains(b, `name="password"`) || strings.Contains(b, "offline") {
		t.Fatalf("gate page: %s", b)
	}
	r, _ = f.h.send("GET", locked.URL, "/api", nil, map[string]string{"Sec-Fetch-Dest": "empty"})
	expectStatus(t, r, 403, "subresource without access")
	cookies := f.unlock(locked, "tunnel-pw")
	r, _ = f.h.send("GET", locked.URL, "/", nil, frame, cookies...)
	expectStatus(t, r, 503, "offline after the password")

	// Revoked or expired: the identical miss, online or not.
	yes := true
	if _, err := f.c.UpdateArtifact(context.Background(), open.ID, &api.UpdateArtifactRequest{Revoked: &yes}); err != nil {
		t.Fatal(err)
	}
	f.h.expire(locked.ID)
	for name, link := range map[string]string{"revoked": open.URL, "expired": locked.URL} {
		r, b := f.h.send("GET", link, "/", nil, frame, cookies...)
		if r.StatusCode != missResp.StatusCode || b != missBody {
			t.Fatalf("%s tunnel: %d %q, want the miss", name, r.StatusCode, b)
		}
	}
}

// unlock enters a's password and returns the visit cookie.
func (f *tunnelFixture) unlock(a *api.Artifact, password string) []*http.Cookie {
	f.t.Helper()
	r := f.h.content("POST", a.URL, "/__redrop/open", url.Values{"password": {password}, "next": {"/"}}, "")
	expectStatus(f.t, r, 303, "unlock")
	return r.Cookies()
}

func TestTunnelPasswordRevokeAndAdminParity(t *testing.T) {
	f := newTunnelFixture(t, nil)
	a := f.create(api.CreateTunnelRequest{Password: "tunnel-pw"})
	run := f.connect(f.c, a)

	r, b := f.h.send("GET", a.URL, "/x", nil, frame)
	expectStatus(t, r, 200, "gate")
	if !strings.Contains(b, `name="password"`) {
		t.Fatal("no gate before the app")
	}
	r, _ = f.h.send("POST", a.URL, "/x", "data", map[string]string{"Sec-Fetch-Dest": "empty"})
	expectStatus(t, r, 403, "POST without access")
	cookies := f.unlock(a, "tunnel-pw")
	r, b = f.h.send("GET", a.URL, "/x", nil, frame, cookies...)
	expectStatus(t, r, 200, "after the password")
	if !strings.HasPrefix(b, "hello from the app") {
		t.Fatalf("body %q", b)
	}
	// A password change signs everyone out, as for artifacts.
	pw := "new-password"
	if _, err := f.c.UpdateArtifact(context.Background(), a.ID, &api.UpdateArtifactRequest{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	r, b = f.h.send("GET", a.URL, "/x", nil, frame, cookies...)
	if r.StatusCode != 200 || !strings.Contains(b, `name="password"`) {
		t.Fatalf("old visit after a password change: %d", r.StatusCode)
	}
	// Admin revocation ends the connection; the CLI stops for good, and the
	// owner can't restore it.
	r = f.h.do("PATCH", "/api/v1/admin/artifacts/"+a.ID, api.AdminArtifactUpdate{Revoked: true}, true)
	expectStatus(t, r, 200, "admin revoke")
	var ended *client.TunnelEndedError
	if err := run.ended(t); !errors.As(err, &ended) || !strings.Contains(ended.Reason, "administrator") {
		t.Fatalf("RunTunnel after admin revoke: %v", err)
	}
	err := f.c.RunTunnel(context.Background(), client.TunnelOptions{ID: a.ID, Target: f.target()})
	if client.StatusOf(err) != http.StatusGone {
		t.Fatalf("reconnecting a revoked tunnel: %v", err)
	}
	no := false
	if _, err := f.c.UpdateArtifact(context.Background(), a.ID, &api.UpdateArtifactRequest{Revoked: &no}); client.StatusOf(err) != http.StatusForbidden {
		t.Fatalf("owner restore after admin revoke: %v", err)
	}
}

// TestTunnelWebSocketUpgrade: a visitor's WebSocket (dev-server hot reload)
// reaches the local app end to end, through the gate's visit cookie.
func TestTunnelWebSocketUpgrade(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte("echo: "), msg...)); err != nil {
				return
			}
		}
	})
	f := newTunnelFixture(t, echo)
	a := f.create(api.CreateTunnelRequest{Password: "tunnel-pw"})
	f.connect(f.c, a)
	cookies := f.unlock(a, "tunnel-pw")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	for _, c := range cookies {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	ws, resp, err := websocket.Dial(ctx, strings.Replace(f.h.srv.URL, "http", "ws", 1)+"/hmr?token=1", &websocket.DialOptions{Host: hostOf(t, a.URL), HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial through the tunnel: %v (%+v)", err, resp)
	}
	defer ws.CloseNow()
	for _, msg := range []string{"first", "second"} {
		if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, got, err := ws.Read(ctx)
		if err != nil || string(got) != "echo: "+msg {
			t.Fatalf("read %q, %v", got, err)
		}
	}
	// Without the visit cookie the upgrade is refused before the app (a
	// browser's handshake says it is a WebSocket, so it gets 403, not the
	// gate page).
	browser := http.Header{"Sec-Fetch-Dest": {"websocket"}, "Sec-Fetch-Mode": {"websocket"}, "Sec-Fetch-Site": {"same-origin"}}
	_, resp, err = websocket.Dial(ctx, strings.Replace(f.h.srv.URL, "http", "ws", 1)+"/hmr", &websocket.DialOptions{Host: hostOf(t, a.URL), HTTPHeader: browser})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("WebSocket without access: %v %+v", err, resp)
	}
}

// TestTunnelModeration: admins list connected tunnels (metadata only) and
// end them; disabling the owner and deleting the token end them too.
func TestTunnelModeration(t *testing.T) {
	f := newTunnelFixture(t, nil)
	userToken := f.h.secondUser("dev")
	user := client.New(f.h.srv.URL, userToken)
	a, err := user.CreateTunnel(context.Background(), &api.CreateTunnelRequest{Title: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	run := f.connect(user, a)

	r := f.h.do("GET", "/api/v1/admin/tunnels", nil, false)
	expectStatus(t, r, 200, "admin list")
	raw := body(r)
	if strings.Contains(raw, hostOf(t, a.URL)) || strings.Contains(raw, `"url"`) {
		t.Fatalf("admin list carries the link: %s", raw)
	}
	r = f.h.do("GET", "/api/v1/admin/tunnels", nil, false)
	list := decode[api.TunnelList](t, r)
	if len(list.Items) != 1 || list.Items[0].ID != a.ID || list.Items[0].OwnerUsername != "dev" || list.Items[0].Title != "demo" {
		t.Fatalf("admin list: %+v", list.Items)
	}
	r = f.h.do("GET", "/api/v1/admin/artifacts", nil, false)
	if arts := decode[api.ArtifactList](t, r); len(arts.Items) != 1 || !arts.Items[0].Online || arts.Items[0].Kind != api.KindTunnel || arts.Items[0].URL != "" {
		t.Fatalf("admin artifacts: %+v", arts.Items)
	}
	// An API token can't reach admin endpoints.
	req, _ := http.NewRequest("DELETE", f.h.srv.URL+"/api/v1/admin/tunnels/"+a.ID, nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token on the admin end: %v %v", resp, err)
	}

	r = f.h.do("DELETE", "/api/v1/admin/tunnels/"+a.ID, nil, true)
	expectStatus(t, r, 200, "admin end")
	var ended *client.TunnelEndedError
	if err := run.ended(t); !errors.As(err, &ended) || ended.Reason != "ended by an administrator" {
		t.Fatalf("after the admin end: %v", err)
	}
	r = f.h.do("DELETE", "/api/v1/admin/tunnels/"+a.ID, nil, true)
	expectStatus(t, r, 404, "ending a tunnel that isn't connected")

	// The owner may start it again; disabling the owner ends it for good.
	run = f.connect(user, a)
	users := decode[api.UserList](t, f.h.do("GET", "/api/v1/admin/users", nil, false))
	var devID string
	for _, u := range users.Items {
		if u.Username == "dev" {
			devID = u.ID
		}
	}
	yes := true
	expectStatus(t, f.h.do("PATCH", "/api/v1/admin/users/"+devID, api.AdminUpdateUserRequest{Disabled: &yes}, true), 200, "disable")
	if err := run.ended(t); !errors.As(err, &ended) || ended.Reason != "the account was disabled" {
		t.Fatalf("after disabling the owner: %v", err)
	}
	if err := user.RunTunnel(context.Background(), client.TunnelOptions{ID: a.ID, Target: f.target()}); client.StatusOf(err) != http.StatusUnauthorized {
		t.Fatalf("a disabled user's token connecting: %v", err)
	}
}

func TestTunnelEndsWhenItsTokenIsDeleted(t *testing.T) {
	f := newTunnelFixture(t, nil)
	r := f.h.do("POST", "/api/v1/tokens", api.CreateTokenRequest{Name: "tunnel-token"}, true)
	expectStatus(t, r, 201, "token")
	tok := decode[api.CreatedToken](t, r)
	c := client.New(f.h.srv.URL, tok.Secret)
	a, err := c.CreateTunnel(context.Background(), &api.CreateTunnelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	run := f.connect(c, a)
	expectStatus(t, f.h.do("DELETE", "/api/v1/tokens/"+tok.ID, nil, true), 200, "delete token")
	var ended *client.TunnelEndedError
	if err := run.ended(t); !errors.As(err, &ended) {
		t.Fatalf("after deleting the token: %v", err)
	}
}

// TestTunnelReconnects: the CLI comes back by itself after its connection
// drops, and the link works again.
func TestTunnelReconnects(t *testing.T) {
	f := newTunnelFixture(t, nil)
	// A front door for the CLI that can cut its WebSocket: httptest stops
	// tracking a connection once it is hijacked.
	var mu sync.Mutex
	var hijacked []net.Conn
	door := httptest.NewUnstartedServer(f.h.app.Handler())
	door.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateHijacked {
			mu.Lock()
			hijacked = append(hijacked, c)
			mu.Unlock()
		}
	}
	door.Start()
	defer door.Close()
	a := f.create(api.CreateTunnelRequest{})
	run := f.connect(client.New(door.URL, f.c.Token), a)
	if e := within(t, "online", run.events); e.Kind != client.TunnelOnline {
		t.Fatalf("first event %v", e.Kind)
	}
	mu.Lock()
	for _, c := range hijacked {
		c.Close()
	}
	mu.Unlock()
	if e := within(t, "offline", run.events); e.Kind != client.TunnelOffline {
		t.Fatalf("after the drop: %v", e.Kind)
	}
	for {
		e := within(t, "online again", run.events)
		if e.Kind == client.TunnelOnline {
			break
		}
		// The server may still hold the dead connection for a moment; the
		// same process replaces it, so retries are rare and short.
		if e.Kind != client.TunnelRetry {
			t.Fatalf("while reconnecting: %v %v", e.Kind, e.Err)
		}
	}
	waitFor(t, "the link to answer again", func() bool {
		r, _ := f.h.send("GET", a.URL, "/again", nil, frame)
		return r.StatusCode == 200
	})
}

// within waits for a channel value. The bound only catches a real hang.
func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func TestTunnelRefusals(t *testing.T) {
	f := newTunnelFixture(t, nil)
	a := f.create(api.CreateTunnelRequest{})
	ctx := context.Background()
	views, yes, spa := 1, true, true
	for name, req := range map[string]*api.UpdateArtifactRequest{
		"view limit": {MaxViews: &views}, "SPA": {SPA: &spa}, "older versions": {OldVersions: &yes},
	} {
		if _, err := f.c.UpdateArtifact(ctx, a.ID, req); client.StatusOf(err) != http.StatusBadRequest {
			t.Errorf("%s on a tunnel: %v", name, err)
		}
	}
	if _, err := f.c.ActivateVersion(ctx, a.ID, 1); client.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("activate a tunnel's version: %v", err)
	}
	page := filepath.Join(t.TempDir(), "p.html")
	if err := os.WriteFile(page, []byte("<p>x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Update(ctx, a.ID, &client.Source{Path: page}, nil); client.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("publish a version to a tunnel: %v", err)
	}
	// Settings that do apply still work.
	title, ttl := "renamed", "2h"
	if got, err := f.c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{Title: &title, TTL: &ttl}); err != nil || got.Title != "renamed" {
		t.Errorf("title and TTL: %+v %v", got, err)
	}

	// Connecting: an HTML artifact is not a tunnel; a session cookie is not
	// an API token; another process can't take a connected tunnel.
	html := f.h.share(f.c, "page", client.PublishOptions{})
	if err := f.c.RunTunnel(ctx, client.TunnelOptions{ID: html.ID, Target: f.target()}); client.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("connect an HTML artifact: %v", err)
	}
	r := f.h.do("GET", "/api/v1/tunnels/"+a.ID+"/connect", nil, false)
	expectStatus(t, r, http.StatusForbidden, "connect with a session")
	f.connect(f.c, a)
	other := make(chan client.TunnelEvent, 1)
	octx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- f.c.RunTunnel(octx, client.TunnelOptions{ID: a.ID, Target: f.target(), Retrier: noDelay{}, OnEvent: func(e client.TunnelEvent) {
			select {
			case other <- e:
			default:
			}
		}})
	}()
	e := <-other
	cancel()
	<-done
	if e.Kind != client.TunnelRetry || client.StatusOf(e.Err) != http.StatusConflict {
		t.Fatalf("second process: %v %v", e.Kind, e.Err)
	}
	if !f.h.app.Tunnels.Online(a.ID) {
		t.Fatal("the first process lost the tunnel")
	}
}

// TestTunnelSameSiteRequestsRefused: other links on the content domain can
// navigate to a tunnel but not send it requests with the visitor's cookies.
func TestTunnelSameSiteRequestsRefused(t *testing.T) {
	f := newTunnelFixture(t, nil)
	a := f.create(api.CreateTunnelRequest{})
	f.connect(f.c, a)
	for _, tc := range []struct {
		method string
		hdr    map[string]string
		want   int
	}{
		{"GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, 403},
		{"POST", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 403},
		{"GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "websocket", "Sec-Fetch-Dest": "websocket"}, 403},
		{"GET", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, 200},
		{"POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, 200},
		{"POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, 200},
	} {
		if r, b := f.h.send(tc.method, a.URL, "/api", "x", tc.hdr); r.StatusCode != tc.want {
			t.Errorf("%s %v: %d %q, want %d", tc.method, tc.hdr, r.StatusCode, b, tc.want)
		}
	}
}

// TestOtherMethodsOnArtifacts: only tunnels take methods other than GET and
// HEAD. Artifacts answer 405 as before; links that don't resolve also spend
// miss budget, as a GET would.
func TestOtherMethodsOnArtifacts(t *testing.T) {
	t.Setenv("REDROP_CONTENT_MISS_BURST", "2")
	t.Setenv("REDROP_CONTENT_MISS_PER_MINUTE", "1")
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	live := h.share(c, "page", client.PublishOptions{})
	for range 4 {
		r, _ := h.send("POST", live.URL, "/form", "x", nil)
		expectStatus(t, r, http.StatusMethodNotAllowed, "POST to a live artifact")
	}
	unknown := strings.Replace(live.URL, live.URL[len("http://"):strings.Index(live.URL, ".")], "amber-otter-quilt-river", 1)
	wantResp, wantBody := h.send("POST", live.URL, "/__redrop/nope", "x", nil)
	for range 2 {
		r, b := h.send("DELETE", unknown, "/x", nil, nil)
		sameResponse(t, "DELETE to an unknown link", r, b, wantResp, wantBody)
	}
	r, _ := h.send("GET", live.URL, "/", nil, nil)
	expectStatus(t, r, http.StatusTooManyRequests, "after spending the miss budget")
}

// TestTunnelFavicon: a visitor with proof of access gets the app's icon;
// everyone else the redrop icon, without a lookup.
func TestTunnelFavicon(t *testing.T) {
	f := newTunnelFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		io.WriteString(w, "app icon")
	}))
	a := f.create(api.CreateTunnelRequest{Password: "tunnel-pw"})
	f.connect(f.c, a)
	if _, b := f.h.send("GET", a.URL, "/favicon.ico", nil, nil); b == "app icon" {
		t.Fatal("the app's icon without access")
	}
	if _, b := f.h.send("GET", a.URL, "/favicon.ico", nil, nil, f.unlock(a, "tunnel-pw")...); b != "app icon" {
		t.Fatalf("icon with access: %q", b)
	}
}
