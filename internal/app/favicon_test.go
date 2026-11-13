package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// ownIcon is a stand-in for an artifact's own favicon.ico.
var ownIcon = []byte("artifact's own icon")

type iconFixture struct {
	h *harness
	c *client.Client
}

func newIconFixture(t *testing.T) iconFixture {
	h := newHarness(t)
	return iconFixture{h: h, c: client.New(h.srv.URL, h.setupAdmin())}
}

// share publishes an HTML page, or a bundle carrying ownIcon when withIcon.
func (f iconFixture) share(withIcon bool, opts client.PublishOptions) *api.Artifact {
	f.h.t.Helper()
	dir := f.h.t.TempDir()
	src := filepath.Join(dir, "page.html")
	if withIcon {
		src = filepath.Join(dir, "site")
		os.Mkdir(src, 0o700)
		os.WriteFile(filepath.Join(src, "favicon.ico"), ownIcon, 0o600)
		os.WriteFile(filepath.Join(src, "apple-touch-icon.png"), ownIcon, 0o600)
		os.WriteFile(filepath.Join(src, "index.html"), []byte("<!doctype html><title>Site</title><p>site body</p>"), 0o600)
	} else {
		os.WriteFile(src, []byte("<!doctype html><title>Page</title><p>page body</p>"), 0o600)
	}
	a, err := f.c.Share(context.Background(), &client.Source{Path: src}, opts, nil)
	if err != nil {
		f.h.t.Fatal(err)
	}
	return a
}

// icon fetches path from the artifact host as a browser fetching a favicon
// would, though with the harness's text/html Accept header, which would count
// as a page view if favicon requests reached the content path.
func (f iconFixture) icon(artifactURL, path string, cookies ...*http.Cookie) (*http.Response, []byte) {
	f.h.t.Helper()
	r := f.h.content("GET", artifactURL, path, nil, "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0", cookies...)
	b, _ := io.ReadAll(r.Body)
	return r, b
}

func (f iconFixture) defaultIcon(path string) []byte {
	f.h.t.Helper()
	r := f.h.do("GET", path, nil, false)
	expectStatus(f.h.t, r, 200, "dashboard "+path)
	b, _ := io.ReadAll(r.Body)
	return b
}

// expectDefault checks the redrop icon came back with fallback caching.
func (f iconFixture) expectDefault(what, artifactURL, path string, cookies ...*http.Cookie) {
	f.h.t.Helper()
	r, b := f.icon(artifactURL, path, cookies...)
	if r.StatusCode != 200 || !bytes.Equal(b, f.defaultIcon(path)) {
		f.h.t.Fatalf("%s: %s: status %d, got %q, want the default icon", what, path, r.StatusCode, b)
	}
	if cc := r.Header.Get("Cache-Control"); cc != "private, max-age=86400" {
		f.h.t.Errorf("%s: default icon Cache-Control %q", what, cc)
	}
	if r.Header.Get("Set-Cookie") != "" {
		f.h.t.Errorf("%s: icon request set a cookie", what)
	}
}

func (f iconFixture) expectOwn(what, artifactURL string, cookies ...*http.Cookie) {
	f.h.t.Helper()
	r, b := f.icon(artifactURL, "/favicon.ico", cookies...)
	if r.StatusCode != 200 || !bytes.Equal(b, ownIcon) {
		f.h.t.Fatalf("%s: status %d, got %q, want the artifact's icon", what, r.StatusCode, b)
	}
	if cc := r.Header.Get("Cache-Control"); cc != "private, no-cache" {
		f.h.t.Errorf("%s: artifact icon Cache-Control %q, want artifact content caching", what, cc)
	}
}

// expectUncounted checks the artifact's counters and access log.
func (f iconFixture) expectUncounted(what string, a *api.Artifact, views int) {
	f.h.t.Helper()
	got, err := f.c.GetArtifact(context.Background(), a.ID)
	if err != nil {
		f.h.t.Fatal(err)
	}
	log, err := f.c.ListViews(context.Background(), a.ID)
	if err != nil {
		f.h.t.Fatal(err)
	}
	if got.Views != views || got.Previews != 0 || len(log) != views {
		f.h.t.Fatalf("%s: views=%d previews=%d log=%d, want %d views and log entries", what, got.Views, got.Previews, len(log), views)
	}
}

func (f iconFixture) unlock(a *api.Artifact, password string) []*http.Cookie {
	f.h.t.Helper()
	r := f.h.content("POST", a.URL, "/__redrop/open", url.Values{"password": {password}, "next": {"/"}}, "")
	expectStatus(f.h.t, r, 303, "open")
	return r.Cookies()
}

func TestFaviconFallback(t *testing.T) {
	f := newIconFixture(t)
	a := f.share(false, client.PublishOptions{})
	f.expectDefault("html artifact", a.URL, "/favicon.ico")
	f.expectDefault("html artifact", a.URL, "/apple-touch-icon.png")
	r, _ := f.icon(a.URL, "/favicon.ico")
	if ct := r.Header.Get("Content-Type"); ct != "image/x-icon" {
		t.Errorf("fallback Content-Type %q", ct)
	}
	// An ungated artifact counts document views; icon requests are not views.
	f.expectUncounted("ungated artifact", a, 0)
}

// An ungated artifact's own icon wins for a browser that has viewed one of
// its pages (the page sets the "seen" cookie before the browser asks for the
// icon). Without that proof the icon path answers like any other slug.
func TestFaviconArtifactIconWins(t *testing.T) {
	f := newIconFixture(t)
	a := f.share(true, client.PublishOptions{})
	f.expectDefault("bundle, no page view yet", a.URL, "/favicon.ico")
	f.expectUncounted("ungated bundle before page view", a, 0)

	r := f.h.content("GET", a.URL, "/", nil, "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0")
	expectStatus(t, r, 200, "page view")
	seen := r.Cookies()
	if len(seen) == 0 {
		t.Fatal("page view set no seen cookie")
	}
	f.expectOwn("bundle with favicon.ico after a page view", a.URL, seen...)
	if _, b := f.icon(a.URL, "/apple-touch-icon.png", seen...); !bytes.Equal(b, ownIcon) {
		t.Errorf("bundle apple-touch-icon.png: got %q", b)
	}
	f.expectUncounted("ungated bundle", a, 1)

	// Another artifact's seen cookie proves nothing here.
	other := f.share(false, client.PublishOptions{})
	r = f.h.content("GET", other.URL, "/", nil, "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0")
	f.expectDefault("foreign seen cookie", a.URL, "/favicon.ico", r.Cookies()...)
}

func TestFaviconDoesNotConsumeViews(t *testing.T) {
	f := newIconFixture(t)
	for _, c := range []struct {
		name     string
		maxViews int
	}{{"one-time", 1}, {"view-limited", 3}} {
		a := f.share(true, client.PublishOptions{MaxViews: c.maxViews})
		for range 3 {
			f.expectDefault(c.name+" before open", a.URL, "/favicon.ico")
			f.expectDefault(c.name+" before open", a.URL, "/apple-touch-icon.png")
		}
		f.expectUncounted(c.name+" before open", a, 0)

		visit := f.unlock(a, "")
		for range 3 {
			f.expectOwn(c.name+" during visit", a.URL, visit...)
		}
		f.expectUncounted(c.name+" during visit", a, 1)
	}

	// After a one-time link is used up, everyone else gets the default.
	a := f.share(true, client.PublishOptions{MaxViews: 1})
	f.unlock(a, "")
	f.expectDefault("exhausted", a.URL, "/favicon.ico")
	f.expectUncounted("exhausted", a, 1)
}

func TestFaviconPasswordGate(t *testing.T) {
	f := newIconFixture(t)
	a := f.share(true, client.PublishOptions{Password: "hunter22"})
	f.expectDefault("locked", a.URL, "/favicon.ico")
	f.expectDefault("locked", a.URL, "/apple-touch-icon.png")
	f.expectDefault("garbage cookie", a.URL, "/favicon.ico", &http.Cookie{Name: "rd_visit", Value: "garbage"})
	f.expectUncounted("locked", a, 0)

	visit := f.unlock(a, "hunter22")
	f.expectOwn("unlocked", a.URL, visit...)
	f.expectUncounted("unlocked", a, 1)
}

// Unknown, malformed, expired and revoked slugs answer exactly like a locked
// artifact without an icon of its own, so the icon path reveals nothing new.
func TestFaviconUnknownAndExpiredSlugs(t *testing.T) {
	f := newIconFixture(t)
	f.expectDefault("unknown slug", "http://"+string(bytes.Repeat([]byte("a"), 26))+".content.test/", "/favicon.ico")
	f.expectDefault("malformed slug", "http://not-a-slug.content.test/", "/favicon.ico")

	expired := f.share(true, client.PublishOptions{})
	past := time.Now().Add(-time.Minute)
	pp := &past
	if err := f.h.app.db.UpdateArtifact(context.Background(), expired.ID, store.ArtifactUpdate{ExpiresAt: &pp}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, f.h.content("GET", expired.URL, "/", nil, ""), 404, "expired page")
	f.expectDefault("expired", expired.URL, "/favicon.ico")
	f.expectDefault("expired", expired.URL, "/apple-touch-icon.png")

	revoked := f.share(true, client.PublishOptions{})
	yes := true
	if _, err := f.c.UpdateArtifact(context.Background(), revoked.ID, &api.UpdateArtifactRequest{Revoked: &yes}); err != nil {
		t.Fatal(err)
	}
	f.expectDefault("revoked", revoked.URL, "/favicon.ico")

	// Other methods keep the content origin's normal answer.
	r := f.h.content("POST", expired.URL, "/favicon.ico", url.Values{}, "")
	expectStatus(t, r, 405, "POST favicon")
}

// TestServerIcons covers the icons of redrop's own pages on every origin.
func TestServerIcons(t *testing.T) {
	f := newIconFixture(t)
	a := f.share(true, client.PublishOptions{Password: "pw"})
	for _, path := range []string{"/favicon.ico", "/favicon.svg", "/apple-touch-icon.png"} {
		want := f.defaultIcon(path)
		if r := f.h.do("GET", path, nil, false); r.Header.Get("Cache-Control") != "public, max-age=86400" {
			t.Errorf("dashboard %s Cache-Control %q", path, r.Header.Get("Cache-Control"))
		}
		for _, c := range []struct{ host, path string }{
			{"http://content.test/", path},                          // apex
			{"http://content.test/", "/__redrop" + path},            // apex pages link these
			{a.URL, "/__redrop" + path},                             // gate and message pages
			{"http://not-a-slug.content.test/", "/__redrop" + path}, // not-found page
		} {
			r, b := f.icon(c.host, c.path)
			if r.StatusCode != 200 || !bytes.Equal(b, want) || r.Header.Get("Cache-Control") != "public, max-age=86400" {
				t.Errorf("%s%s: status %d, Cache-Control %q, default=%v", c.host, c.path, r.StatusCode, r.Header.Get("Cache-Control"), bytes.Equal(b, want))
			}
		}
	}
	// The gate links the reserved icons, never the artifact's own.
	gate := body(f.h.content("GET", a.URL, "/", nil, ""))
	if !bytes.Contains([]byte(gate), []byte(`<link rel="icon" href="/__redrop/favicon.svg"`)) {
		t.Errorf("gate page does not link the redrop icon: %s", gate)
	}
	if apex := body(f.h.content("GET", "http://content.test/", "/", nil, "")); !bytes.Contains([]byte(apex), []byte(`<link rel="icon" href="/favicon.svg"`)) {
		t.Errorf("apex page does not link the icon: %s", apex)
	}
	f.expectUncounted("gate page and icons", a, 0)
}

// TestFaviconSpendsNoMissBudget: fallback icon requests on unknown, malformed,
// expired and locked slugs never spend the miss budget, are served even to a
// client whose budget is spent, and look the same for live and unknown slugs.
func TestFaviconSpendsNoMissBudget(t *testing.T) {
	t.Setenv("REDROP_CONTENT_MISS_BURST", "2")
	t.Setenv("REDROP_CONTENT_MISS_PER_MINUTE", "1")
	f := newIconFixture(t)
	live := f.share(true, client.PublishOptions{})
	locked := f.share(true, client.PublishOptions{Password: "hunter22"})
	expired := f.share(true, client.PublishOptions{})
	past := time.Now().Add(-time.Minute)
	pp := &past
	if err := f.h.app.db.UpdateArtifact(context.Background(), expired.ID, store.ArtifactUpdate{ExpiresAt: &pp}); err != nil {
		t.Fatal(err)
	}
	from := map[string]string{"X-Forwarded-For": "192.0.2.30"}
	hosts := map[string]string{
		"unknown word":   "amber-otter-quilt-river.content.test",
		"unknown legacy": "s3eybdkc67r7zegmb7qbugpmvy.content.test",
		"malformed":      "not-a-slug.content.test",
		"expired":        hostOf(t, expired.URL),
		"locked":         hostOf(t, locked.URL),
		"live":           hostOf(t, live.URL),
	}
	iconFrom := func(host, path string) *http.Response {
		req, _ := http.NewRequest("GET", f.h.srv.URL+path, nil)
		req.Host = host
		req.Header.Set("X-Forwarded-For", from["X-Forwarded-For"])
		resp, err := f.h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	check := func(stage string) {
		t.Helper()
		for _, path := range []string{"/favicon.ico", "/apple-touch-icon.png"} {
			want := fingerprint(iconFrom(hosts["unknown word"], path))
			if !strings.HasPrefix(want, "200 ") {
				t.Fatalf("%s: %s on unknown slug:\n%s", stage, path, want)
			}
			for name, host := range hosts {
				for range 5 {
					if got := fingerprint(iconFrom(host, path)); got != want {
						t.Fatalf("%s: %s on %s differs from unknown slug:\n%s\n---\n%s", stage, path, name, got, want)
					}
				}
			}
		}
	}
	check("fresh client")

	// Dozens of icon requests later the client still has its whole burst.
	unknown := hosts["unknown word"]
	expectStatus(t, f.h.get(unknown, from), 404, "first miss")
	expectStatus(t, f.h.get(unknown, from), 404, "second miss")
	expectStatus(t, f.h.get(unknown, from), 429, "miss over burst")

	// A spent client still gets the same icons, not 429.
	check("spent client")
}
