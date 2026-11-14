package app

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// The viewer frame (DESIGN.md "Viewer frame"): a top-level page load gets
// redrop's viewer page, which frames the artifact from the same origin.
// Every other request gets the stored bytes.

var (
	frameSrcRE = regexp.MustCompile(`<iframe id="rd-frame"[^>]* src="([^"]+)"`)
	inFrame    = map[string]string{"Sec-Fetch-Dest": "iframe", "Sec-Fetch-Mode": "navigate", "Accept": "text/html"}
	asset      = map[string]string{"Sec-Fetch-Dest": "script", "Sec-Fetch-Mode": "no-cors"}
	noMeta     = map[string]string{"Accept": "text/html"} // curl, old browsers
)

// viewerLoad loads the viewer page and returns its frame src ("" without one).
func (h *harness) viewerLoad(artifactURL, path string, cookies ...*http.Cookie) (*http.Response, string, string) {
	h.t.Helper()
	r, b := h.page("GET", artifactURL, path, nav, cookies...)
	m := frameSrcRE.FindStringSubmatch(b)
	if m == nil {
		return r, b, ""
	}
	return r, b, html.UnescapeString(m[1])
}

func (h *harness) views(c *client.Client, id string) int {
	h.t.Helper()
	a, err := c.GetArtifact(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return a.Views
}

func cookieNamed(r *http.Response, name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestViewerFrame(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	const stored = "<!doctype html><title>Plan</title><h1>plan</h1>\n"
	a := shareFile(t, c, "plan.html", stored, client.PublishOptions{})
	self := strings.TrimSuffix(a.URL, "/")

	r, b, src := h.viewerLoad(a.URL, "/?q=1&redrop-info=1")
	expectStatus(t, r, 200, "viewer page")
	if src != "/?q=1" {
		t.Fatalf("frame src %q, want the same path and query", src)
	}
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src " + self + "/__redrop/viewer.js;", "style-src " + self + "/__redrop/viewer.css;",
		"frame-src 'self' http: https:", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("viewer CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || r.Header.Get("Vary") != "Sec-Fetch-Dest" {
		t.Errorf("viewer headers: CSP %q Vary %q", csp, r.Header.Get("Vary"))
	}
	if strings.Count(b, "<script") != strings.Count(b, "<script src=") || strings.Contains(b, "<h1>plan</h1>") {
		t.Error("viewer page has inline script or artifact content")
	}
	if !strings.Contains(b, `allow="fullscreen; clipboard-write"`) || !strings.Contains(b, `id="rd-info"`) {
		t.Error("frame permissions or info panel missing")
	}

	// The frame, subresources and clients without Fetch Metadata get the
	// stored bytes, untouched, with the blob ETag.
	var etag string
	for name, hdr := range map[string]map[string]string{"frame": inFrame, "subresource": asset, "no Fetch Metadata": noMeta} {
		r, got := h.page("GET", a.URL, "/?q=1", hdr)
		expectStatus(t, r, 200, name)
		if got != stored || r.Header.Get("ETag") == "" {
			t.Fatalf("%s: bytes %q etag %q", name, got, r.Header.Get("ETag"))
		}
		if etag != "" && r.Header.Get("ETag") != etag {
			t.Errorf("%s: etag %q differs", name, r.Header.Get("ETag"))
		}
		etag = r.Header.Get("ETag")
		if fa := r.Header.Get("Content-Security-Policy"); fa != "frame-ancestors 'self' http://app.test" {
			t.Errorf("%s: content CSP %q: must be frameable by its own origin and the dashboard", name, fa)
		}
		if r.Header.Get("Vary") != "Sec-Fetch-Dest" {
			t.Errorf("%s: Vary %q", name, r.Header.Get("Vary"))
		}
	}
	// Directory redirects and missing paths answer as without the viewer.
	if r, _ := h.page("GET", a.URL, "/missing.html", nav); r.StatusCode != 404 {
		t.Errorf("missing path: %d", r.StatusCode)
	}

	// No service workers: one could answer the viewer page's own loads.
	for _, hdr := range []map[string]string{{"Sec-Fetch-Dest": "serviceworker"}, {"Service-Worker": "script"}} {
		if r, _ := h.page("GET", a.URL, "/", hdr); r.StatusCode != 404 {
			t.Errorf("service worker script %v: %d", hdr, r.StatusCode)
		}
	}
}

// TestOneViewPerVisit: the viewer page load counts; its frame's request
// doesn't. A direct load without Fetch Metadata counts once.
func TestOneViewPerVisit(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "v.html", "<p>v</p>", client.PublishOptions{})
	r, _, src := h.viewerLoad(a.URL, "/")
	seen := cookieNamed(r, "rd_seen")
	if src == "" || seen == nil {
		t.Fatal("viewer load: no frame, or no seen cookie")
	}
	h.page("GET", a.URL, src, inFrame, seen)
	h.page("GET", a.URL, src, inFrame) // even without the cookie, a frame never counts
	h.page("GET", a.URL, "/", asset)
	if n := h.views(c, a.ID); n != 1 {
		t.Fatalf("views after one viewer visit = %d, want 1", n)
	}

	b := shareFile(t, c, "w.html", "<p>w</p>", client.PublishOptions{})
	if _, got := h.page("GET", b.URL, "/", noMeta); got != "<p>w</p>" {
		t.Fatalf("no Fetch Metadata: %q", got)
	}
	if n := h.views(c, b.ID); n != 1 {
		t.Fatalf("views after one direct load = %d, want 1", n)
	}

	// A view-limited link spends its view at Open, never on the frame.
	l := shareFile(t, c, "l.html", "<p>limited</p>", client.PublishOptions{MaxViews: 2})
	if _, b := h.page("GET", l.URL, "/", nav); !strings.Contains(b, `action="/__redrop/open"`) {
		t.Fatal("view-limited link without a visit: no gate")
	}
	open := h.content("POST", l.URL, "/__redrop/open", url.Values{"next": {"/"}}, "")
	expectStatus(t, open, http.StatusSeeOther, "open")
	visit := cookieNamed(open, "rd_visit")
	_, _, src = h.viewerLoad(l.URL, "/", visit)
	if _, got := h.page("GET", l.URL, src, inFrame, visit); got != "<p>limited</p>" {
		t.Fatalf("frame with a visit grant: %q", got)
	}
	if n := h.views(c, l.ID); n != 1 {
		t.Fatalf("view-limited views = %d, want 1", n)
	}
}

// TestGatesInTheFrame: a password gate guards the viewer page and the frame's
// content alike, on the same origin.
func TestGatesInTheFrame(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "s.html", "<p>secret</p>", client.PublishOptions{Password: "hunter22"})
	for name, hdr := range map[string]map[string]string{"viewer": nav, "frame": inFrame, "no Fetch Metadata": noMeta} {
		r, b := h.page("GET", a.URL, "/", hdr)
		if r.StatusCode != 200 || strings.Contains(b, "secret") || !strings.Contains(b, `name="password"`) {
			t.Errorf("%s without a grant: %d, gate=%v", name, r.StatusCode, strings.Contains(b, `name="password"`))
		}
	}
	if r, b := h.page("GET", a.URL, "/", asset); r.StatusCode != 403 || strings.Contains(b, "secret") {
		t.Errorf("subresource without a grant: %d", r.StatusCode)
	}
	open := h.content("POST", a.URL, "/__redrop/open", url.Values{"password": {"hunter22"}, "next": {"/"}}, "")
	expectStatus(t, open, http.StatusSeeOther, "unlock")
	visit := cookieNamed(open, "rd_visit")
	if _, _, src := h.viewerLoad(a.URL, "/", visit); src != "/" {
		t.Fatalf("unlocked viewer: frame %q", src)
	}
	if _, got := h.page("GET", a.URL, "/", inFrame, visit); got != "<p>secret</p>" {
		t.Fatalf("unlocked frame: %q", got)
	}
}

// TestDirectViews: with the info button off, or with ?redrop-info=0, a top-
// level load gets the stored bytes as before.
func TestDirectViews(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	const stored = "<p>direct</p>"
	off := false
	a := shareFile(t, c, "d.html", stored, client.PublishOptions{InfoButton: &off})
	if r, b := h.page("GET", a.URL, "/", nav); r.StatusCode != 200 || b != stored {
		t.Fatalf("info off: %d %q", r.StatusCode, b)
	}
	framed := shareFile(t, c, "f.html", stored, client.PublishOptions{})
	if r, b := h.page("GET", framed.URL, "/?redrop-info=0", nav); r.StatusCode != 200 || b != stored {
		t.Fatalf("redrop-info=0: %d %q", r.StatusCode, b)
	}
	if got, _ := c.GetArtifact(context.Background(), framed.ID); !got.InfoButton || got.Views != 1 {
		t.Fatalf("redrop-info=0 changed the setting or did not count: %+v", got)
	}

	// A direct view stays direct for the pages it opens.
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
	choice := cookieNamed(r, "rd_direct")
	if choice == nil || !choice.HttpOnly {
		t.Fatal("no direct-view choice recorded")
	}
	// The page's own iframe gets content even without the choice.
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
	if c := cookieNamed(r, "rd_direct"); !strings.Contains(got, `id="rd-frame"`) || c == nil || c.MaxAge >= 0 {
		t.Fatal("redrop-info=1 did not return to the viewer and clear the choice")
	}
}

func TestOlderVersions(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	dir := t.TempDir()
	write := func(name, s string) { os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600) }
	write("index.html", "<p>one</p>")
	write("page.html", "<p>page one</p>")
	a, err := c.Share(ctx, &client.Source{Path: dir, Note: "first draft"}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	write("index.html", "<p>two</p>")
	if _, err := c.Update(ctx, a.ID, &client.Source{Path: dir, Note: "fixed <b>totals</b>"}, nil); err != nil {
		t.Fatal(err)
	}

	// Off (the default): the panel lists both versions, metadata only.
	_, b, _ := h.viewerLoad(a.URL, "/")
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
	// The old-version paths give the identical miss.
	miss, missBody := h.page("GET", "http://zebra-zebra-zebra-zebra.content.test/", "/", inFrame)
	refused := func(when string) {
		t.Helper()
		for _, p := range []string{"/__redrop/v/1/", "/__redrop/v/1/page.html"} {
			r, b := h.page("GET", a.URL, p, inFrame)
			if r.StatusCode != miss.StatusCode || b != missBody {
				t.Errorf("%s: %s: %d, want the miss", when, p, r.StatusCode)
			}
			for _, k := range []string{"Content-Type", "Cache-Control", "Content-Security-Policy", "Content-Length"} {
				if r.Header.Get(k) != miss.Header.Get(k) {
					t.Errorf("%s: %s: header %s %q differs from the miss %q", when, p, k, r.Header.Get(k), miss.Header.Get(k))
				}
			}
		}
	}
	refused("older versions off")

	// On: version 1 opens in the frame, with the version bar.
	on := true
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{OldVersions: &on}); err != nil {
		t.Fatal(err)
	}
	_, b, _ = h.viewerLoad(a.URL, "/")
	if !strings.Contains(b, `href="/?redrop-version=1"`) {
		t.Error("panel does not offer version 1")
	}
	r, b, src := h.viewerLoad(a.URL, "/page.html?x=1&redrop-version=1")
	expectStatus(t, r, 200, "version 1 viewer")
	if src != "/__redrop/v/1/page.html?x=1" || !strings.Contains(b, "Viewing version 1 of 2") || !strings.Contains(b, `href="/page.html?x=1">Back to latest`) {
		t.Fatalf("version 1 viewer: src %q", src)
	}
	for _, hdr := range []map[string]string{inFrame, asset, noMeta} {
		if _, got := h.page("GET", a.URL, "/__redrop/v/1/", hdr); got != "<p>one</p>" {
			t.Fatalf("version 1 bytes %v: %q", hdr, got)
		}
	}
	if _, got := h.page("GET", a.URL, "/__redrop/v/2/", inFrame); got != "<p>two</p>" {
		t.Fatalf("the current version under its number: %q", got)
	}
	// A top-level load of version 1's files goes to the viewer form.
	if r, _ := h.page("GET", a.URL, "/__redrop/v/1/page.html?x=1", nav); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/page.html?redrop-version=1&x=1" {
		t.Errorf("top-level old version: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// ... unless the visitor asked for the direct view.
	if r, _ := h.page("GET", a.URL, "/?redrop-version=1&redrop-info=0", nav); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/__redrop/v/1/?redrop-info=0" {
		t.Errorf("direct old version: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if _, got := h.page("GET", a.URL, "/__redrop/v/1/?redrop-info=0", nav); got != "<p>one</p>" {
		t.Errorf("direct old version bytes %q", got)
	}
	if r, _ := h.page("GET", a.URL, "/__redrop/v/1", inFrame); r.StatusCode != http.StatusMovedPermanently || r.Header.Get("Location") != "/__redrop/v/1/" {
		t.Errorf("version root without a slash: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	for _, p := range []string{"/__redrop/v/01/", "/__redrop/v/0/", "/__redrop/v/9/", "/__redrop/v/x/", "/__redrop/v/-1/"} {
		if r, b := h.page("GET", a.URL, p, inFrame); r.StatusCode != miss.StatusCode || b != missBody {
			t.Errorf("%s: %d, want the miss", p, r.StatusCode)
		}
	}

	// Off again: version 1 closes at once.
	off := false
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{OldVersions: &off}); err != nil {
		t.Fatal(err)
	}
	refused("older versions off again")
	// Info off closes them too.
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{OldVersions: &on, InfoButton: &off}); err != nil {
		t.Fatal(err)
	}
	refused("info off")
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
	if !strings.Contains(b, "Viewing version 1 of 2") || !strings.Contains(b, `href="/__redrop/v/1/notes.txt"`) || !strings.Contains(b, `href="/__redrop/v/1/notes.txt?download"`) {
		t.Fatalf("old file version page: %d", r.StatusCode)
	}
	if _, raw := h.page("GET", a.URL, "/__redrop/v/1/notes.txt", nav); raw != "hello notes" {
		t.Errorf("old file bytes %q", raw)
	}
	if r, _ := h.page("GET", a.URL, "/notes.txt?redrop-version=1", nav); r.Header.Get("Location") != "/__redrop/v/1/notes.txt" {
		t.Errorf("old file via the parameter: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	off := false
	quiet := shareFile(t, c, "q.txt", "q", client.PublishOptions{InfoButton: &off})
	if _, b := h.page("GET", quiet.URL, "/", nav); strings.Contains(b, "rd-info") {
		t.Error("file page shows the panel with the info button off")
	}
}

// TestPreviewFramesContent: the dashboard preview works as before: its frame
// gets the content, and previews never count views.
func TestPreviewFramesContent(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := shareFile(t, c, "p.html", "<p>preview me</p>", client.PublishOptions{Password: "hunter22"})
	r := h.do("POST", "/api/v1/artifacts/"+a.ID+"/preview", nil, true)
	expectStatus(t, r, 200, "preview link")
	link := decode[api.PreviewLink](t, r).URL
	if !strings.HasPrefix(link, a.URL+"__redrop/preview?t=") {
		t.Fatalf("preview link %q", link)
	}
	u, _ := url.Parse(link)
	r, _ = h.page("GET", a.URL, u.RequestURI(), inFrame)
	preview := cookieNamed(r, "rd_preview")
	if r.StatusCode != http.StatusSeeOther || preview == nil {
		t.Fatalf("preview redeem: %d", r.StatusCode)
	}
	if _, b := h.page("GET", a.URL, "/", inFrame, preview); b != "<p>preview me</p>" {
		t.Fatalf("preview bytes %q", b)
	}
	if h.views(c, a.ID) != 0 {
		t.Fatal("preview counted a view")
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
		ck := cookieNamed(r, "rd_direct")
		return ck != nil && ck.MaxAge > 0
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
	// Owner decision N1: a ?redrop-info=0 link clicked on another website is
	// direct for that page only. The next page the visitor opens from it,
	// from the same origin, gets the viewer again.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<a href="/page.html">p</a>`), 0o600)
	os.WriteFile(filepath.Join(dir, "page.html"), []byte("<p>page</p>"), 0o600)
	b, err := c.Share(context.Background(), &client.Source{Path: dir}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clicked := map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-User": "?1"}
	r, got := h.page("GET", b.URL, "/?redrop-info=0", clicked)
	if r.StatusCode != 200 || strings.Contains(got, `id="rd-frame"`) || cookieNamed(r, "rd_direct") != nil {
		t.Fatalf("cross-site click: %d, framed=%v, choice recorded=%v", r.StatusCode, strings.Contains(got, `id="rd-frame"`), cookieNamed(r, "rd_direct") != nil)
	}
	sameOrigin := map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-User": "?1"}
	if _, got := h.page("GET", b.URL, "/page.html", sameOrigin, r.Cookies()...); !strings.Contains(got, `id="rd-frame"`) {
		t.Fatal("after a cross-site direct link, the next same-origin page should get the viewer")
	}

	// A same-site page can't clear a visitor's choice either.
	r, _ = h.page("GET", a.URL, "/?redrop-info=1", map[string]string{"Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "same-site"})
	if cookieNamed(r, "rd_direct") != nil {
		t.Error("a same-site navigation cleared the direct choice")
	}
}
