package app

import (
	"bufio"
	"context"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// The viewer frame (DESIGN.md "Viewer frame"): the shared link serves
// redrop's viewer page, and the artifact loads from a raw host.

var (
	frameSrcRE = regexp.MustCompile(`<iframe id="rd-frame"[^>]* src="([^"]+)"`)
	streamRE   = regexp.MustCompile(`data-stream="([^"]+)"`)
	inFrame    = map[string]string{"Sec-Fetch-Dest": "iframe", "Sec-Fetch-Mode": "navigate", "Accept": "text/html"}
	asset      = map[string]string{"Sec-Fetch-Dest": "script", "Sec-Fetch-Mode": "no-cors"}
	// userNav is a navigation the visitor made inside the frame (a click).
	userNav = map[string]string{"Sec-Fetch-Dest": "iframe", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-User": "?1", "Accept": "text/html"}
)

// rawURL is the raw host of a viewer URL with the given suffix ("raw", "v2").
func rawURL(viewer, suffix string) string {
	u, _ := url.Parse(viewer)
	label, rest, _ := strings.Cut(u.Host, ".")
	return u.Scheme + "://" + label + "--" + suffix + "." + rest + "/"
}

// viewerLoad loads the viewer page and returns its frame src and stream URL.
func (h *harness) viewerLoad(viewerURL, path string, cookies ...*http.Cookie) (*http.Response, string, string, string) {
	h.t.Helper()
	r, b := h.page("GET", viewerURL, path, nav, cookies...)
	m, s := frameSrcRE.FindStringSubmatch(b), streamRE.FindStringSubmatch(b)
	if m == nil || s == nil {
		return r, b, "", ""
	}
	return r, b, html.UnescapeString(m[1]), html.UnescapeString(s[1])
}

// enterFrame follows the frame's enter URL like a browser frame and returns
// the raw cookie and the path it redirects to.
func (h *harness) enterFrame(src string) (*http.Cookie, string) {
	h.t.Helper()
	u, _ := url.Parse(src)
	r, b := h.page("GET", src, u.RequestURI(), inFrame)
	if r.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("enter: status %d: %s", r.StatusCode, b)
	}
	for _, c := range r.Cookies() {
		if c.Name == "rd_raw" {
			return c, r.Header.Get("Location")
		}
	}
	h.t.Fatal("enter set no raw cookie")
	return nil, ""
}

func (h *harness) views(c *client.Client, id string) int {
	h.t.Helper()
	a, err := c.GetArtifact(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return a.Views
}

func TestViewerFrame(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	const stored = "<!doctype html><title>Plan</title><h1>plan</h1>\n"
	a := shareFile(t, c, "plan.html", stored, client.PublishOptions{})
	viewer := strings.TrimSuffix(a.URL, "/")
	raw := rawURL(a.URL, "raw")

	r, b, src, _ := h.viewerLoad(a.URL, "/?q=1")
	expectStatus(t, r, 200, "viewer page")
	if !strings.HasPrefix(src, raw+"__redrop/enter?t=") || !strings.Contains(src, "&next=%2F%3Fq%3D1") {
		t.Fatalf("frame src %q", src)
	}
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src " + viewer + "/__redrop/viewer.js;", "style-src " + viewer + "/__redrop/viewer.css;",
		"connect-src 'self'", "frame-src " + strings.TrimSuffix(raw, "/"), "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("viewer CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || r.Header.Get("Cross-Origin-Opener-Policy") != "same-origin" || r.Header.Get("Cache-Control") != "private, no-store" {
		t.Errorf("viewer headers: CSP %q COOP %q Cache-Control %q", csp, r.Header.Get("Cross-Origin-Opener-Policy"), r.Header.Get("Cache-Control"))
	}
	if strings.Count(b, "<script") != strings.Count(b, "<script src=") || strings.Contains(b, "<h1>plan</h1>") {
		t.Error("viewer page has inline script or artifact content")
	}
	if !strings.Contains(b, `allow="fullscreen; clipboard-write"`) || !strings.Contains(b, `id="rd-info"`) {
		t.Error("frame permissions or info panel missing")
	}
	if n := h.views(c, a.ID); n != 1 {
		t.Fatalf("views after one viewer load = %d", n)
	}

	cookie, next := h.enterFrame(src)
	if next != "/?q=1" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode { // Lax only on plain-HTTP dev hosts
		t.Fatalf("enter: next %q, cookie %+v", next, cookie)
	}
	r, got := h.page("GET", raw, "/", inFrame, cookie)
	expectStatus(t, r, 200, "raw page")
	direct, _ := h.page("GET", a.URL, "/?redrop-info=0", nav)
	if got != stored || r.Header.Get("ETag") == "" || r.Header.Get("ETag") != direct.Header.Get("ETag") {
		t.Fatalf("raw bytes %q (etag %s, stored etag %s)", got, r.Header.Get("ETag"), direct.Header.Get("ETag"))
	}
	if fa := r.Header.Get("Content-Security-Policy"); fa != "frame-ancestors 'self' "+viewer+" http://app.test" {
		t.Errorf("raw CSP %q", fa)
	}
	if corp := r.Header.Get("Cross-Origin-Resource-Policy"); corp != "same-origin" {
		t.Errorf("raw CORP %q: other artifacts could include these bytes", corp)
	}
	// The enter token is single-use: a copy from the page source is useless.
	if r, _ := h.page("GET", src, src[len(raw)-1:], inFrame); r.StatusCode != 404 {
		t.Errorf("replayed enter token: %d", r.StatusCode)
	}

	// Views: one per viewer load (de-duplicated as before); raw never counts.
	for range 5 {
		h.page("GET", raw, "/", inFrame, cookie)
		h.page("GET", raw, "/", asset, cookie)
	}
	if n := h.views(c, a.ID); n != 2 { // the ?redrop-info=0 load above is a viewer-host load too
		t.Fatalf("views = %d, want 2", n)
	}

	// A top-level visit to a raw host goes to the shared link, decided before
	// any lookup: unknown slugs answer the same way.
	r, _ = h.page("GET", raw, "/a/b?x=1", nav)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != viewer+"/a/b?x=1" {
		t.Errorf("top-level raw: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	r, _ = h.page("GET", rawURL(a.URL, "v2"), "/a", nav)
	if r.Header.Get("Location") != viewer+"/a?redrop-version=2" {
		t.Errorf("top-level pinned raw: %q", r.Header.Get("Location"))
	}
	r, _ = h.page("GET", raw, src[len(raw)-1:], nav)
	if r.Header.Get("Location") != viewer+"/" {
		t.Errorf("top-level enter must not redeem or echo the token: %q", r.Header.Get("Location"))
	}
	unknown := "http://zebra-zebra-zebra-zebra--raw.content.test/"
	if r, _ := h.page("GET", unknown, "/a/b?x=1", nav); r.StatusCode != http.StatusSeeOther {
		t.Errorf("top-level unknown raw: %d", r.StatusCode)
	}

	// Without a valid grant every raw response is the unknown-link miss.
	miss, missBody := h.page("GET", unknown, "/", inFrame)
	other := shareFile(t, c, "other.html", "<p>other</p>", client.PublishOptions{})
	_, _, otherSrc, _ := h.viewerLoad(other.URL, "/")
	otherCookie, _ := h.enterFrame(otherSrc)
	for name, cookies := range map[string][]*http.Cookie{"none": nil, "other artifact": {otherCookie}, "forged": {{Name: "rd_raw", Value: "x.y"}}} {
		r, b := h.page("GET", raw, "/", inFrame, cookies...)
		if r.StatusCode != miss.StatusCode || b != missBody {
			t.Errorf("%s: %d, want the miss", name, r.StatusCode)
		}
		for _, k := range []string{"Content-Type", "Cache-Control", "Content-Security-Policy", "Content-Length"} {
			if r.Header.Get(k) != miss.Header.Get(k) {
				t.Errorf("%s: header %s %q differs from the unknown-link miss %q", name, k, r.Header.Get(k), miss.Header.Get(k))
			}
		}
	}
	if r, b := h.page("GET", raw, "/__redrop/enter?t=bogus&next=/", inFrame); r.StatusCode != miss.StatusCode || b != missBody {
		t.Errorf("bad enter token: %d", r.StatusCode)
	}
	// A password change bumps the access generation: the frame closes.
	pw := "hunter22"
	if _, err := c.UpdateArtifact(context.Background(), a.ID, &api.UpdateArtifactRequest{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	if r, b := h.page("GET", raw, "/", inFrame, cookie); r.StatusCode != miss.StatusCode || b != missBody {
		t.Errorf("after a password change: %d", r.StatusCode)
	}

	// No service workers on any artifact host.
	for _, u := range []string{a.URL, raw} {
		if r, _ := h.page("GET", u, "/sw.js", map[string]string{"Sec-Fetch-Dest": "serviceworker"}); r.StatusCode != 404 {
			t.Errorf("service worker script on %s: %d", u, r.StatusCode)
		}
	}
}

// TestDirectViews: with the info button off, or with ?redrop-info=0, the
// shared link serves the stored bytes as before: no frame, no raw host.
func TestDirectViews(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	const stored = "<p>direct</p>"
	off := false
	a := shareFile(t, c, "d.html", stored, client.PublishOptions{InfoButton: &off})
	if r, b := h.page("GET", a.URL, "/", nav); r.StatusCode != 200 || b != stored {
		t.Fatalf("info off: %d %q", r.StatusCode, b)
	}
	if r, _ := h.page("GET", rawURL(a.URL, "raw"), "/", inFrame); r.StatusCode != 404 {
		t.Errorf("info off: raw host answered %d", r.StatusCode)
	}
	framed := shareFile(t, c, "f.html", stored, client.PublishOptions{})
	if r, b := h.page("GET", framed.URL, "/?redrop-info=0", nav); r.StatusCode != 200 || b != stored {
		t.Fatalf("redrop-info=0: %d %q", r.StatusCode, b)
	}
	if got, _ := c.GetArtifact(context.Background(), framed.ID); !got.InfoButton || got.Views != 1 {
		t.Fatalf("redrop-info=0 changed the setting or did not count: %+v", got)
	}

	// A direct view stays direct for its own frames and pages.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<iframe src="/frame.html"></iframe><a href="/page.html">p</a>`), 0o600)
	os.WriteFile(filepath.Join(dir, "frame.html"), []byte("<p>framed part</p>"), 0o600)
	os.WriteFile(filepath.Join(dir, "page.html"), []byte("<p>second page</p>"), 0o600)
	b, err := c.Share(context.Background(), &client.Source{Path: dir}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	typed := map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "none", "Accept": "text/html"}
	r, _ := h.page("GET", b.URL, "/?redrop-info=0", typed)
	var choice *http.Cookie
	for _, ck := range r.Cookies() {
		if ck.Name == "rd_direct" {
			choice = ck
		}
	}
	if choice == nil || !choice.HttpOnly {
		t.Fatal("no direct-view choice recorded")
	}
	// The page's own iframe gets content even without the choice: a viewer
	// page can never render in a frame.
	if _, got := h.page("GET", b.URL, "/frame.html", map[string]string{"Sec-Fetch-Dest": "iframe"}); got != "<p>framed part</p>" {
		t.Fatalf("iframe inside a direct view got %q", got)
	}
	if _, got := h.page("GET", b.URL, "/page.html", nav, choice); got != "<p>second page</p>" {
		t.Fatalf("link inside a direct view got %q", got)
	}
	forged := &http.Cookie{Name: "rd_direct", Value: "x.y"}
	if _, got := h.page("GET", b.URL, "/page.html", nav, forged); !strings.Contains(got, `id="rd-frame"`) {
		t.Fatal("a forged direct choice skipped the viewer")
	}
	r, got := h.page("GET", b.URL, "/page.html?redrop-info=1", typed, choice)
	cleared := false
	for _, ck := range r.Cookies() {
		cleared = cleared || (ck.Name == "rd_direct" && ck.MaxAge < 0)
	}
	if !strings.Contains(got, `id="rd-frame"`) || !cleared {
		t.Fatal("redrop-info=1 did not return to the viewer and clear the choice")
	}
}

func TestOlderVersions(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	dir := t.TempDir()
	write := func(s string) { os.WriteFile(filepath.Join(dir, "index.html"), []byte(s), 0o600) }
	write("<p>one</p>")
	a, err := c.Share(ctx, &client.Source{Path: dir, Note: "first draft"}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	write("<p>two</p>")
	if _, err := c.Update(ctx, a.ID, &client.Source{Path: dir, Note: "fixed <b>totals</b>"}, nil); err != nil {
		t.Fatal(err)
	}
	v1 := rawURL(a.URL, "v1")

	// Off (the default): the panel lists both versions, metadata only.
	_, b, _, _ := h.viewerLoad(a.URL, "/")
	for _, want := range []string{"Version 1", "Version 2", "first draft", "fixed &lt;b&gt;totals&lt;/b&gt;", `<span class="rd-tag">current</span>`} {
		if !strings.Contains(b, want) {
			t.Errorf("panel lacks %q", want)
		}
	}
	if strings.Contains(b, "redrop-version=1") || strings.Contains(b, a.ID) || strings.Contains(b, "test</") {
		t.Error("panel offers version 1, or shows the artifact ID or token name")
	}
	// A note written before notes were shown to viewers (as migrated) stays private.
	if _, err := h.app.db.DB.Exec(`UPDATE versions SET note_public = 0 WHERE artifact_id = ? AND number = 1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, b := h.page("GET", a.URL, "/", nav); strings.Contains(b, "first draft") || !strings.Contains(b, "Version 1") {
		t.Error("a private note reached the panel, or its version vanished")
	}
	for _, q := range []string{"1", "abc", "-1", "0x1"} {
		if r, _ := h.page("GET", a.URL, "/?redrop-version="+q, nav); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
			t.Errorf("version %q: %d %q", q, r.StatusCode, r.Header.Get("Location"))
		}
	}
	_, _, src, _ := h.viewerLoad(a.URL, "/")
	latest, _ := h.enterFrame(src)
	if r, _ := h.page("GET", v1, "/", inFrame, latest); r.StatusCode != 404 {
		t.Errorf("v1 host with a latest-version cookie: %d", r.StatusCode)
	}

	// On: version 1 opens in its own pinned raw host, with the version bar.
	on := true
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{OldVersions: &on}); err != nil {
		t.Fatal(err)
	}
	_, b, _, _ = h.viewerLoad(a.URL, "/")
	if !strings.Contains(b, `href="/?redrop-version=1"`) {
		t.Error("panel does not offer version 1")
	}
	r, b, src, _ := h.viewerLoad(a.URL, "/?redrop-version=1")
	expectStatus(t, r, 200, "version 1 viewer")
	if !strings.HasPrefix(src, v1+"__redrop/enter?") || !strings.Contains(b, "Viewing version 1 of 2") || !strings.Contains(b, `href="/">Back to latest`) {
		t.Fatalf("version 1 viewer: src %q", src)
	}
	old, _ := h.enterFrame(src)
	r, got := h.page("GET", v1, "/", inFrame, old)
	if got != "<p>one</p>" {
		t.Fatalf("v1 bytes %q", got)
	}
	if fa := r.Header.Get("Content-Security-Policy"); strings.Contains(fa, "app.test") {
		t.Errorf("a pinned host is frameable by the dashboard: %q", fa)
	}
	if r, _ := h.page("GET", rawURL(a.URL, "raw"), "/", inFrame, old); r.StatusCode != 404 {
		t.Error("a version-1 cookie opened the latest host")
	}
	if r, _ := h.page("GET", v1, "/", inFrame, latest); r.StatusCode != 404 {
		t.Error("a latest cookie opened version 1")
	}
	// Off again: version 1 closes at once.
	off := false
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{OldVersions: &off}); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.page("GET", v1, "/", inFrame, old); r.StatusCode != 404 {
		t.Errorf("v1 after turning older versions off: %d", r.StatusCode)
	}
}

// TestFilePagePanel: file artifacts keep redrop's own page, with the panel
// rendered natively (no frame, nothing injected into the file).
func TestFilePagePanel(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "notes.txt", "hello notes", client.PublishOptions{})
	r, b := h.page("GET", a.URL, "/", nav)
	expectStatus(t, r, 200, "file page")
	if !strings.Contains(b, `id="rd-info"`) || !strings.Contains(b, `<script src="/__redrop/viewer.js"`) || strings.Contains(b, "rd-frame") {
		t.Fatal("file page panel missing, or framed")
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src "+strings.TrimSuffix(a.URL, "/")+"/__redrop/viewer.js") {
		t.Errorf("file page CSP %q", csp)
	}
	if _, raw := h.page("GET", a.URL, "/notes.txt", nav); raw != "hello notes" {
		t.Errorf("file bytes %q", raw)
	}
	svg := shareFile(t, c, "x.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`, client.PublishOptions{})
	if r, _ := h.page("GET", svg.URL, "/x.svg", nav); r.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("SVG file opened on the shared link is not sandboxed: %q", r.Header.Get("Content-Security-Policy"))
	}
	// Revisions on the file page, and an older file version opened there.
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(p, []byte("hello again"), 0o600)
	if _, err := c.Update(ctx, a.ID, &client.Source{Path: p, Note: "second take"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, b := h.page("GET", a.URL, "/", nav); !strings.Contains(b, "second take") || strings.Contains(b, "redrop-version=1") {
		t.Error("file page revisions: note missing, or version 1 offered while closed")
	}
	on := true
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{OldVersions: &on}); err != nil {
		t.Fatal(err)
	}
	r, b = h.page("GET", a.URL, "/?redrop-version=1", nav)
	if !strings.Contains(b, "Viewing version 1 of 2") || !strings.Contains(b, `href="/notes.txt?redrop-version=1"`) || !strings.Contains(b, `href="/notes.txt?redrop-version=1&amp;download"`) {
		t.Fatalf("old file version page: %d", r.StatusCode)
	}
	if _, raw := h.page("GET", a.URL, "/notes.txt?redrop-version=1", nav); raw != "hello notes" {
		t.Errorf("old file bytes %q", raw)
	}
	off := false
	quiet := shareFile(t, c, "q.txt", "q", client.PublishOptions{InfoButton: &off})
	if _, b := h.page("GET", quiet.URL, "/", nav); strings.Contains(b, "rd-info") {
		t.Error("file page shows the panel with the info button off")
	}
}

// TestRawMissBudget: raw-host misses spend the same per-client budget as
// shared-link misses, and an exhausted client is refused on both.
func TestRawMissBudget(t *testing.T) {
	t.Setenv("REDROP_CONTENT_MISS_BURST", "3")
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "x.html", "<p>x</p>", client.PublishOptions{})
	raw := rawURL(a.URL, "raw")
	for i := range 3 {
		if r, _ := h.page("GET", raw, "/", inFrame); r.StatusCode != 404 {
			t.Fatalf("miss %d: %d", i, r.StatusCode)
		}
	}
	for _, u := range []string{raw, a.URL, "http://not-a-slug--raw.content.test/"} {
		if r, _ := h.page("GET", u, "/", inFrame); r.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s after the budget: %d", u, r.StatusCode)
		}
	}
}

// sse reads server-sent "data:" lines from a stream until ctx ends.
func (h *harness) sse(ctx context.Context, viewerURL, path string) (<-chan string, int) {
	h.t.Helper()
	u, _ := url.Parse(viewerURL)
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+path, nil)
	req.Host = u.Host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make(chan string, 16)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		close(out)
		return out, resp.StatusCode
	}
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				out <- d
			}
		}
	}()
	return out, 200
}

func next(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("no location event")
	}
	return ""
}

func none(t *testing.T, ch <-chan string) {
	t.Helper()
	select {
	case p := <-ch:
		t.Fatalf("unexpected location %q", p)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestAddressBarRelay: frame navigations on the raw host reach that frame's
// viewer over its own stream, and nobody else's.
func TestAddressBarRelay(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>home</p>"), 0o600)
	os.WriteFile(filepath.Join(dir, "b.html"), []byte("<p>b</p>"), 0o600)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("1"), 0o600)
	a, err := c.Share(context.Background(), &client.Source{Path: dir}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawURL(a.URL, "raw")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, src1, stream1 := h.viewerLoad(a.URL, "/")
	_, _, src2, stream2 := h.viewerLoad(a.URL, "/")
	ev1, code := h.sse(ctx, a.URL, stream1)
	ev2, _ := h.sse(ctx, a.URL, stream2)
	if code != 200 {
		t.Fatalf("stream: %d", code)
	}
	frame1, _ := h.enterFrame(src1) // two browsers: separate cookie jars
	frame2, _ := h.enterFrame(src2)

	h.page("GET", raw, "/b.html?x=1", userNav, frame1)
	if got := next(t, ev1); got != "/b.html?x=1" {
		t.Fatalf("frame 1 location %q", got)
	}
	none(t, ev2)
	h.page("GET", raw, "/app.js", asset, frame1) // subresources don't move the address bar
	// Loads nobody clicked (a frame nested in the artifact, a scripted
	// navigation, or a client without Fetch Metadata) aren't reported either.
	h.page("GET", raw, "/b.html", inFrame, frame1)
	h.page("GET", raw, "/b.html", map[string]string{"Accept": "text/html"}, frame1)
	none(t, ev1)
	h.page("GET", raw, "/", userNav, frame2)
	if got := next(t, ev2); got != "/" {
		t.Fatalf("frame 2 location %q", got)
	}
	none(t, ev1)

	// Two tabs in one browser share the raw cookie, so the newest frame's id
	// wins: the older tab's clicks reach the newer tab's stream (where
	// viewer.js ignores them unless that tab's own frame loads; see
	// web/scripts/viewer-js.test.ts) and never another browser's.
	_, _, src3, stream3 := h.viewerLoad(a.URL, "/")
	ev3, _ := h.sse(ctx, a.URL, stream3)
	shared, _ := h.enterFrame(src3) // the same browser as frame 1 now holds frame 3's cookie
	h.page("GET", raw, "/b.html", userNav, shared)
	if got := next(t, ev3); got != "/b.html" {
		t.Fatalf("shared-cookie click went to %q", got)
	}
	none(t, ev1)
	none(t, ev2)

	// A stream token is scoped to its artifact and checked live.
	other := shareFile(t, c, "o.html", "<p>o</p>", client.PublishOptions{})
	if _, code := h.sse(ctx, other.URL, stream1); code != 404 {
		t.Errorf("stream token on another artifact: %d", code)
	}
	if _, code := h.sse(ctx, a.URL, "/__redrop/location?s=forged"); code != 404 {
		t.Errorf("forged stream token: %d", code)
	}
	// At most four streams per client (three are open).
	for range 1 {
		h.sse(ctx, a.URL, stream1)
	}
	if _, code := h.sse(ctx, a.URL, stream1); code != http.StatusTooManyRequests {
		t.Errorf("fifth stream for one client: %d", code)
	}
}

// TestFramedPreview: the dashboard frames a framed artifact's raw host
// directly; previews skip gates and never count.
func TestFramedPreview(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	a := shareFile(t, c, "p.html", "<p>preview me</p>", client.PublishOptions{Password: "hunter22"})
	r := h.do("POST", "/api/v1/artifacts/"+a.ID+"/preview", nil, true)
	expectStatus(t, r, 200, "preview link")
	link := decode[api.PreviewLink](t, r).URL
	raw := rawURL(a.URL, "raw")
	if !strings.HasPrefix(link, raw+"__redrop/preview?t=") {
		t.Fatalf("preview link %q", link)
	}
	u, _ := url.Parse(link)
	r, _ = h.page("GET", raw, u.RequestURI(), inFrame)
	if r.StatusCode != http.StatusSeeOther || len(r.Cookies()) == 0 {
		t.Fatalf("preview redeem: %d", r.StatusCode)
	}
	if _, b := h.page("GET", raw, "/", inFrame, r.Cookies()...); b != "<p>preview me</p>" {
		t.Fatalf("preview bytes %q", b)
	}
	if h.views(c, a.ID) != 0 {
		t.Fatal("preview counted a view")
	}
	if r, _ := h.page("GET", raw, "/__redrop/preview?t=bogus", inFrame); r.StatusCode != 404 {
		t.Errorf("bad preview token: %d, want the miss", r.StatusCode)
	}
}

// TestDirectChoiceOnlyByVisitor: ?redrop-info=0 is recorded for the visit
// only on a top-level navigation the visitor started. Another artifact on the
// same site can't switch this link's viewer off with an image, a fetch, or a
// navigation of its own.
func TestDirectChoiceOnlyByVisitor(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "a.html", "<p>a</p>", client.PublishOptions{})
	sets := func(hdr map[string]string) bool {
		r, _ := h.page("GET", a.URL, "/?redrop-info=0", hdr)
		for _, ck := range r.Cookies() {
			if ck.Name == "rd_direct" && ck.MaxAge > 0 {
				return true
			}
		}
		return false
	}
	for name, hdr := range map[string]map[string]string{
		"same-site image":     {"Sec-Fetch-Dest": "image", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Site": "same-site"},
		"same-site fetch":     {"Sec-Fetch-Dest": "empty", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-site"},
		"same-site document":  {"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-site"},
		"cross-site document": {"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site"},
		"same-origin frame":   {"Sec-Fetch-Dest": "iframe", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin"},
		"no Fetch Metadata":   {"Accept": "text/html"},
	} {
		if sets(hdr) {
			t.Errorf("%s recorded the direct choice", name)
		}
	}
	for name, hdr := range map[string]map[string]string{
		"typed or bookmarked": {"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "none"},
		"same-origin link":    {"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin"},
	} {
		if !sets(hdr) {
			t.Errorf("%s did not record the direct choice", name)
		}
	}
	// Documented expectation (owner decision N1): a ?redrop-info=0 link
	// clicked on another website is direct for that page only. The next page
	// the visitor opens from it, from the same origin, gets the viewer again.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<a href="/page.html">p</a>`), 0o600)
	os.WriteFile(filepath.Join(dir, "page.html"), []byte("<p>page</p>"), 0o600)
	b, err := c.Share(context.Background(), &client.Source{Path: dir}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clicked := map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-User": "?1"}
	r, got := h.page("GET", b.URL, "/?redrop-info=0", clicked)
	recorded := false
	for _, ck := range r.Cookies() {
		recorded = recorded || ck.Name == "rd_direct"
	}
	if r.StatusCode != 200 || strings.Contains(got, `id="rd-frame"`) || recorded {
		t.Fatalf("cross-site click: %d, direct=%v, choice recorded=%v", r.StatusCode, !strings.Contains(got, `id="rd-frame"`), recorded)
	}
	sameOrigin := map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-User": "?1"}
	if _, got := h.page("GET", b.URL, "/page.html", sameOrigin, r.Cookies()...); !strings.Contains(got, `id="rd-frame"`) {
		t.Fatal("after a cross-site direct link, the next same-origin page should get the viewer")
	}

	// A same-site page can't clear a visitor's choice either.
	r, _ = h.page("GET", a.URL, "/?redrop-info=1", map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "same-site"})
	for _, ck := range r.Cookies() {
		if ck.Name == "rd_direct" {
			t.Error("a same-site navigation cleared the direct choice")
		}
	}
}

// TestEnterQuota: one network gets a bounded share of frame entries; over
// it, a valid token is refused with 429 without spending miss budget.
func TestEnterQuota(t *testing.T) {
	t.Setenv("REDROP_CONTENT_MISS_BURST", "3")
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "q.html", "<p>q</p>", client.PublishOptions{})
	var src string
	for i := 0; ; i++ {
		_, _, src, _ = h.viewerLoad(a.URL, "/")
		u, _ := url.Parse(src)
		r, _ := h.page("GET", src, u.RequestURI(), inFrame)
		if r.StatusCode == http.StatusTooManyRequests {
			if i != 200 || r.Header.Get("Retry-After") == "" {
				t.Fatalf("refused after %d entries (Retry-After %q)", i, r.Header.Get("Retry-After"))
			}
			break
		}
		if r.StatusCode != http.StatusSeeOther || i > 200 {
			t.Fatalf("entry %d: %d", i, r.StatusCode)
		}
	}
	// Refusals are not misses: the client's miss budget is untouched.
	for range 5 {
		u, _ := url.Parse(src)
		h.page("GET", src, u.RequestURI(), inFrame)
	}
	if r, _ := h.page("GET", a.URL, "/", nav); r.StatusCode != 200 {
		t.Fatalf("quota refusals spent miss budget: %d", r.StatusCode)
	}
}

// TestRawCookieRefreshOverHTTP: a raw response renews the cookie once half
// its life is gone, and the renewed cookie works past the original hour.
func TestRawCookieRefreshOverHTTP(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "r.html", "<p>r</p>", client.PublishOptions{})
	now := time.Now()
	h.app.Sharing.SetClock(func() time.Time { return now })
	_, _, src, _ := h.viewerLoad(a.URL, "/")
	cookie, _ := h.enterFrame(src)
	raw := rawURL(a.URL, "raw")

	now = now.Add(10 * time.Minute)
	if r, _ := h.page("GET", raw, "/", inFrame, cookie); r.StatusCode != 200 || len(r.Cookies()) != 0 {
		t.Fatalf("early: %d, %d cookies", r.StatusCode, len(r.Cookies()))
	}
	now = now.Add(40 * time.Minute)
	r, _ := h.page("GET", raw, "/", inFrame, cookie)
	var renewed *http.Cookie
	for _, ck := range r.Cookies() {
		if ck.Name == "rd_raw" {
			renewed = ck
		}
	}
	if r.StatusCode != 200 || renewed == nil || renewed.MaxAge != 3600 || !renewed.HttpOnly {
		t.Fatalf("no renewed raw cookie at 50 minutes: %d %+v", r.StatusCode, renewed)
	}
	now = now.Add(50 * time.Minute) // 100 minutes: past the original hour
	if r, _ := h.page("GET", raw, "/", inFrame, cookie); r.StatusCode != 404 {
		t.Errorf("original cookie at 100 minutes: %d", r.StatusCode)
	}
	if r, b := h.page("GET", raw, "/", inFrame, renewed); r.StatusCode != 200 || b != "<p>r</p>" {
		t.Errorf("renewed cookie at 100 minutes: %d", r.StatusCode)
	}
}
