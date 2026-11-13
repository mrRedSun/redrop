package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	app     *App
	session *http.Cookie
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("REDROP_DATA_DIR", t.TempDir())
	t.Setenv("REDROP_PUBLIC_URL", "http://app.test")
	t.Setenv("REDROP_CONTENT_URL", "http://content.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, slog.New(slog.DiscardHandler), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, app: a}
}

// do sends a request to the app origin. JSON bodies are encoded; the
// session cookie and CSRF header are attached when csrf is true.
func (h *harness) do(method, path string, body any, csrf bool) *http.Response {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if h.session != nil {
		req.AddCookie(h.session)
	}
	if csrf {
		req.Header.Set("X-Redrop-Request", "1")
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// content requests an artifact host on the content domain.
func (h *harness) content(method, artifactURL, path string, form url.Values, ua string, cookies ...*http.Cookie) *http.Response {
	h.t.Helper()
	u, _ := url.Parse(artifactURL)
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Host = u.Host
	req.Header.Set("Accept", "text/html")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, r *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	return v
}

func body(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func expectStatus(t *testing.T, r *http.Response, want int, what string) {
	t.Helper()
	if r.StatusCode != want {
		t.Fatalf("%s: status %d, want %d: %s", what, r.StatusCode, want, body(r))
	}
}

// setupAdmin redeems the bootstrap link and returns an API token.
func (h *harness) setupAdmin() string {
	h.t.Helper()
	link, err := h.app.Accounts.Bootstrap(context.Background())
	if err != nil || link == "" {
		h.t.Fatalf("bootstrap: %q %v", link, err)
	}
	token := link[strings.LastIndex(link, "/")+1:]
	r := h.do("POST", "/api/v1/setup", api.SetupRequest{Token: token, Username: "admin", Password: "long-enough-password"}, true)
	expectStatus(h.t, r, 200, "setup")
	for _, c := range r.Cookies() {
		if c.Name == "redrop_session" {
			h.session = c
		}
	}
	if h.session == nil {
		h.t.Fatal("no session cookie after setup")
	}
	r = h.do("POST", "/api/v1/tokens", api.CreateTokenRequest{Name: "test"}, true)
	expectStatus(h.t, r, 201, "create token")
	return decode[api.CreatedToken](h.t, r).Secret
}

func TestAuthorizationBoundaries(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()

	// Cookie-authenticated writes need the CSRF header.
	expectStatus(t, h.do("POST", "/api/v1/tokens", api.CreateTokenRequest{Name: "x"}, false), 403, "write without CSRF header")

	// API tokens cannot manage tokens or use admin endpoints.
	bearer := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(`{"name":"x"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	expectStatus(t, bearer("POST", "/api/v1/tokens"), 403, "token creating tokens")
	expectStatus(t, bearer("GET", "/api/v1/admin/users"), 403, "token using admin API")
	expectStatus(t, bearer("GET", "/api/v1/me"), 200, "token reading /me")

	// Unknown API paths are JSON 404s, not the dashboard.
	r := h.do("GET", "/api/v1/nope", nil, false)
	expectStatus(t, r, 404, "unknown API path")
	if !strings.Contains(r.Header.Get("Content-Type"), "json") {
		t.Errorf("unknown API path content type %q", r.Header.Get("Content-Type"))
	}

	// Setup links are one-time.
	expectStatus(t, h.do("POST", "/api/v1/setup", api.SetupRequest{Token: "x", Username: "evil", Password: "long-enough-password"}, true), 410, "reusing setup")
}

func TestOneTimePasswordLinkFlow(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)

	page := filepath.Join(t.TempDir(), "report.html")
	os.WriteFile(page, []byte("<!doctype html><title>Quarterly</title><p>secret numbers</p>"), 0o600)
	a, err := c.Share(context.Background(), &client.Source{Path: page}, client.PublishOptions{Password: "hunter22", MaxViews: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "Quarterly" || a.Kind != api.KindHTML || !a.HasPassword || a.MaxViews != 1 {
		t.Fatalf("unexpected artifact: %+v", a)
	}

	// The gate hides content and title, also from link-preview bots.
	r := h.content("GET", a.URL, "/", nil, "TelegramBot (like TwitterBot)")
	expectStatus(t, r, 200, "gate for bot")
	if b := body(r); strings.Contains(b, "secret numbers") || strings.Contains(b, "Quarterly") || !strings.Contains(b, "password protected") {
		t.Fatalf("gate leaked content or is missing: %s", b)
	}
	if r.Header.Get("X-Robots-Tag") == "" || r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Error("content security headers missing")
	}

	// Wrong password is rejected; the right one grants a visit and burns the view.
	expectStatus(t, h.content("POST", a.URL, "/__redrop/open", url.Values{"password": {"nope"}}, ""), 401, "wrong password")
	r = h.content("POST", a.URL, "/__redrop/open", url.Values{"password": {"hunter22"}, "next": {"/"}}, "")
	expectStatus(t, r, 303, "correct password")
	visit := r.Cookies()
	if len(visit) == 0 {
		t.Fatal("no visit cookie")
	}
	r = h.content("GET", a.URL, "/", nil, "", visit...)
	expectStatus(t, r, 200, "content with grant")
	if !strings.Contains(body(r), "secret numbers") {
		t.Fatal("content not served after unlock")
	}

	// Everyone else now gets 410.
	expectStatus(t, h.content("GET", a.URL, "/", nil, ""), 410, "second visitor")

	got, err := c.GetArtifact(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != api.StatusExhausted || got.Views != 1 || got.Previews != 1 {
		t.Fatalf("counters: status=%s views=%d previews=%d", got.Status, got.Views, got.Previews)
	}

	// Rotating the link kills the old URL.
	rot, err := c.RotateArtifact(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rot.URL == a.URL {
		t.Fatal("rotate kept the URL")
	}
	expectStatus(t, h.content("GET", a.URL, "/", nil, ""), 404, "old URL after rotate")
}

func TestAdminSeesMetadataOnly(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	f := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(f, []byte("hello"), 0o600)
	if _, err := c.Share(context.Background(), &client.Source{Path: f}, client.PublishOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	r := h.do("GET", "/api/v1/admin/artifacts", nil, false)
	expectStatus(t, r, 200, "admin artifacts")
	list := decode[api.ArtifactList](t, r)
	if len(list.Items) != 1 || list.Items[0].URL != "" || list.Items[0].OwnerUsername != "admin" {
		t.Fatalf("admin listing exposes links or misses owner: %+v", list.Items)
	}
}

func TestBundleTraversalRejected(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "docs"), 0o700)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>Site</title>root"), 0o600)
	os.WriteFile(filepath.Join(dir, "docs", "a.txt"), []byte("doc"), 0o600)
	a, err := c.Share(context.Background(), &client.Source{Path: dir, SPA: true}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/": 200, "/docs/a.txt": 200, "/docs": 301, "/docs/": 200, "/client/route": 200, "/missing.js": 404} {
		expectStatus(t, h.content("GET", a.URL, path, nil, ""), want, "bundle "+path)
	}
}

func TestAdminUsersAndInvites(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	f := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(f, []byte("hello"), 0o600)
	if _, err := c.Share(context.Background(), &client.Source{Path: f}, client.PublishOptions{}, nil); err != nil {
		t.Fatal(err)
	}

	r := h.do("GET", "/api/v1/admin/users", nil, false)
	expectStatus(t, r, 200, "admin users")
	users := decode[api.UserList](t, r)
	if len(users.Items) != 1 || users.Items[0].UsedBytes != 5 || users.Items[0].Artifacts != 1 {
		t.Fatalf("usage not aggregated: %+v", users.Items)
	}
	self := users.Items[0].ID
	role := api.RoleUser
	expectStatus(t, h.do("PATCH", "/api/v1/admin/users/"+self, api.AdminUpdateUserRequest{Role: &role}, true), 409, "self-demotion")

	expectStatus(t, h.do("POST", "/api/v1/admin/invites", api.CreateInviteRequest{TTL: "never"}, true), 400, "never-expiring invite")
	r = h.do("POST", "/api/v1/admin/invites", api.CreateInviteRequest{Note: "bob", TTL: "1d"}, true)
	expectStatus(t, r, 201, "create invite")
	inv := decode[api.Invite](t, r)
	if !strings.Contains(inv.URL, "/invite/") || inv.Role != api.RoleUser {
		t.Fatalf("invite: %+v", inv)
	}
	r = h.do("GET", "/api/v1/admin/invites", nil, false)
	list := decode[api.InviteList](t, r)
	if len(list.Items) != 1 || list.Items[0].URL != "" {
		t.Fatalf("invite list must not repeat the secret URL: %+v", list.Items)
	}
}
