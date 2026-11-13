package app

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

const infoMarker = "<script>(function (d)"

// page fetches path on an artifact host with exactly the given headers.
func (h *harness) page(method, artifactURL, path string, hdr map[string]string, cookies ...*http.Cookie) (*http.Response, string) {
	h.t.Helper()
	u, _ := url.Parse(artifactURL)
	req, _ := http.NewRequest(method, h.srv.URL+path, nil)
	req.Host = u.Host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	b := body(resp)
	resp.Body.Close()
	return resp, b
}

func shareFile(t *testing.T, c *client.Client, name, content string, opts client.PublishOptions) *api.Artifact {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := c.Share(context.Background(), &client.Source{Path: p}, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var nav = map[string]string{"Sec-Fetch-Dest": "document", "Accept": "text/html"}

func TestInfoButtonServing(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	const stored = "<!doctype html><html><head><title>Plan</title></head><body><h1>plan</h1></body></html>"
	a := shareFile(t, c, "plan.html", stored, client.PublishOptions{})
	if !a.InfoButton {
		t.Fatal("info button should default to on")
	}

	r, got := h.page("GET", a.URL, "/", nav)
	expectStatus(t, r, 200, "page")
	at := strings.Index(got, infoMarker)
	if at < 0 || !strings.HasSuffix(got, "</body></html>") || got[:at] != strings.TrimSuffix(stored, "</body></html>") {
		t.Fatalf("widget not inserted just before </body>: %q", got)
	}
	if r.Header.Get("Content-Length") != strconv.Itoa(len(got)) {
		t.Errorf("Content-Length %s, body %d bytes", r.Header.Get("Content-Length"), len(got))
	}
	if r.Header.Get("Cache-Control") != "private, no-cache" || !strings.Contains(r.Header.Get("Vary"), "Sec-Fetch-Dest") {
		t.Errorf("headers: Cache-Control %q Vary %q", r.Header.Get("Cache-Control"), r.Header.Get("Vary"))
	}
	etag := r.Header.Get("ETag")

	// The stored bytes are unchanged and keep their own ETag.
	r, raw := h.page("GET", a.URL, "/?download", nav)
	rawETag := r.Header.Get("ETag")
	if raw != stored || rawETag == etag || rawETag == "" {
		t.Fatalf("download: etag %q (page %q), body %q", rawETag, etag, raw)
	}

	// HEAD describes the same representation without a body.
	r, b := h.page("HEAD", a.URL, "/", nav)
	if b != "" || r.Header.Get("Content-Length") != strconv.Itoa(len(got)) || r.Header.Get("ETag") != etag {
		t.Errorf("HEAD: length %s etag %s body %q", r.Header.Get("Content-Length"), r.Header.Get("ETag"), b)
	}

	// Conditional requests revalidate against the served representation.
	r, _ = h.page("GET", a.URL, "/", map[string]string{"Sec-Fetch-Dest": "document", "If-None-Match": etag})
	expectStatus(t, r, 304, "revalidate injected page")

	// Range requests address the stored bytes.
	r, b = h.page("GET", a.URL, "/", map[string]string{"Sec-Fetch-Dest": "document", "Range": "bytes=0-14"})
	if r.StatusCode != 206 || b != stored[:15] || r.Header.Get("ETag") != rawETag {
		t.Errorf("range: %d %q etag %s", r.StatusCode, b, r.Header.Get("ETag"))
	}

	// Only top-level navigations: not frames, fetches, or scripts.
	for _, hdr := range []map[string]string{
		{"Sec-Fetch-Dest": "iframe"},
		{"Sec-Fetch-Dest": "empty", "Accept": "*/*"},
		{"Accept": "application/json"},
	} {
		if _, b := h.page("GET", a.URL, "/", hdr); b != stored {
			t.Errorf("%v: got injected or changed page %q", hdr, b)
		}
	}
	// Without Fetch Metadata, a plain HTML request (e.g. curl) still gets it.
	if _, b := h.page("GET", a.URL, "/", map[string]string{"Accept": "text/html"}); !strings.Contains(b, infoMarker) {
		t.Error("no widget without Fetch Metadata")
	}

	// ?redrop-info=0 hides it for one request without changing the setting.
	if _, b := h.page("GET", a.URL, "/?redrop-info=0", nav); b != stored {
		t.Errorf("redrop-info=0 still injected: %q", b)
	}
	if got, _ := c.GetArtifact(context.Background(), a.ID); !got.InfoButton {
		t.Error("query parameter changed the stored setting")
	}

	// Missing </body>: appended at the end.
	bare := shareFile(t, c, "bare.html", "<p>just text", client.PublishOptions{})
	if _, b := h.page("GET", bare.URL, "/", nav); !strings.HasPrefix(b, "<p>just text"+infoMarker) || !strings.HasSuffix(b, "</script>") {
		t.Errorf("no </body>: %q", b)
	}
}

func TestInfoButtonOnlyOnHTMLPages(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "docs"), 0o700)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>Site</title><body>home</body>"), 0o600)
	os.WriteFile(filepath.Join(dir, "docs", "page.html"), []byte("<body>inner</body>"), 0o600)
	os.WriteFile(filepath.Join(dir, "app.css"), []byte("body{}</body>"), 0o600)
	b, err := c.Share(context.Background(), &client.Source{Path: dir}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{"/": true, "/docs/page.html": true, "/app.css": false, "/docs/": false} {
		_, got := h.page("GET", b.URL, path, nav)
		if strings.Contains(got, infoMarker) != want {
			t.Errorf("bundle %s: injected=%v, want %v", path, !want, want)
		}
	}
	// File artifacts, even HTML files shared as files, keep redrop's viewer and raw bytes.
	f := shareFile(t, c, "notes.txt", "</body>", client.PublishOptions{})
	for _, path := range []string{"/", "/notes.txt"} {
		if _, got := h.page("GET", f.URL, path, nav); strings.Contains(got, infoMarker) {
			t.Errorf("file artifact %s got the widget", path)
		}
	}
}

func TestInfoButtonSetting(t *testing.T) {
	t.Setenv("REDROP_INFO_BUTTON_DEFAULT", "false")
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	shown := func(a *api.Artifact) bool {
		_, b := h.page("GET", a.URL, "/", nav)
		return strings.Contains(b, infoMarker)
	}

	def := shareFile(t, c, "a.html", "<body>a</body>", client.PublishOptions{})
	if def.InfoButton || shown(def) {
		t.Fatal("server default off: shown")
	}
	on := true
	explicit := shareFile(t, c, "b.html", "<body>b</body>", client.PublishOptions{InfoButton: &on})
	if !explicit.InfoButton || !shown(explicit) {
		t.Fatal("explicit on at share: hidden")
	}
	off := false
	got, err := c.UpdateArtifact(ctx, explicit.ID, &api.UpdateArtifactRequest{InfoButton: &off})
	if err != nil || got.InfoButton || shown(got) {
		t.Fatalf("set off: %v %+v", err, got)
	}
	got, err = c.UpdateArtifact(ctx, def.ID, &api.UpdateArtifactRequest{InfoButton: &on})
	if err != nil || !got.InfoButton || !shown(got) {
		t.Fatalf("set on: %v %+v", err, got)
	}
	// Other patches leave it alone.
	title := "renamed"
	if got, err = c.UpdateArtifact(ctx, def.ID, &api.UpdateArtifactRequest{Title: &title}); err != nil || !got.InfoButton {
		t.Fatalf("title patch reset the setting: %v %+v", err, got)
	}
}

// TestInfoButtonPrivacyAndViews: a one-time, password-protected page shows its
// own limits but nothing about who shared it, and the button costs no views.
func TestInfoButtonPrivacyAndViews(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	evil := "</script><script>alert(1)</script>"
	a := shareFile(t, c, "x.html", "<body>secret</body>", client.PublishOptions{Title: evil, Password: "hunter22", MaxViews: 2})

	r := h.content("POST", a.URL, "/__redrop/open", url.Values{"password": {"hunter22"}, "next": {"/"}}, "")
	expectStatus(t, r, 303, "open")
	visit := r.Cookies()
	var page string
	for range 3 {
		r, page = h.page("GET", a.URL, "/", nav, visit...)
		expectStatus(t, r, 200, "page with grant")
	}
	h.page("HEAD", a.URL, "/", nav, visit...)
	h.page("GET", a.URL, "/", map[string]string{"Sec-Fetch-Dest": "document", "Range": "bytes=0-3"}, visit...)

	got, err := c.GetArtifact(context.Background(), a.ID)
	if err != nil || got.Views != 1 {
		t.Fatalf("views = %d (%v), want 1: loading the page with its button must not count", got.Views, err)
	}

	snippet := page[strings.Index(page, infoMarker):]
	for _, want := range []string{`"max_views":2`, `"views_left":1`, `"password":true`, jsonEscaped(evil)} {
		if !strings.Contains(snippet, want) {
			t.Errorf("snippet lacks %s", want)
		}
	}
	if strings.Count(strings.ToLower(page), "</script") != 1 {
		t.Errorf("title broke out of the script element: %s", page)
	}
	for _, secret := range []string{a.ID, "admin", `"test"`, "hunter22", strings.TrimPrefix(strings.Split(a.URL, ".")[0], "http://")} {
		if strings.Contains(snippet, secret) {
			t.Errorf("snippet leaks %q", secret)
		}
	}
}

// jsonEscaped is s as encoding/json writes it inside a string: <, > and &
// become \u escapes.
func jsonEscaped(s string) string {
	return strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`).Replace(s)
}
