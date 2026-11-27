package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mrRedSun/redrop/internal/e2e"
	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// End-to-end encrypted links (DESIGN.md "End-to-end encrypted links").

const e2eSecret = "e2e-plaintext-marker-7f3a"

// fetchContent requests path on an artifact host with the given headers
// (pairs), without following redirects.
func (h *harness) fetchContent(artifactURL, path string, cookies []*http.Cookie, header ...string) *http.Response {
	h.t.Helper()
	u, _ := url.Parse(artifactURL)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	req.Host = u.Host
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
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

var (
	docHeaders   = []string{"Sec-Fetch-Dest", "document", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Site", "none"}
	frameHeaders = []string{"Sec-Fetch-Dest", "iframe", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Site", "same-origin"}
	fetchHeaders = []string{"Sec-Fetch-Dest", "empty", "Sec-Fetch-Mode", "same-origin", "Sec-Fetch-Site", "same-origin"}
	swHeaders    = []string{"Sec-Fetch-Dest", "serviceworker", "Service-Worker", "script", "Sec-Fetch-Site", "same-origin"}
)

// viewerAttr reads a data attribute of the e2e viewer page's frame.
func viewerAttr(t *testing.T, page, name string) string {
	t.Helper()
	m := regexp.MustCompile(`data-` + name + `="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("viewer page has no data-%s: %s", name, page)
	}
	return m[1]
}

// decryptVersion does what the viewer does: it reads version n's manifest
// and every file with the key, failing on anything that doesn't open.
func (h *harness) decryptVersion(artifactURL, id string, n int, key e2e.Key, cookies []*http.Cookie) (*e2e.Manifest, map[string][]byte) {
	h.t.Helper()
	get := func(name string) []byte {
		r := h.fetchContent(artifactURL, "/__redrop/v/"+itoa(n)+"/"+name, cookies, fetchHeaders...)
		expectStatus(h.t, r, 200, "object "+name)
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			h.t.Fatalf("object %s served as %q", name, ct)
		}
		b, _ := io.ReadAll(r.Body)
		return b
	}
	plain, err := e2e.OpenBytes(get(e2e.ManifestPath), key, e2e.ManifestBinding(id, n))
	if err != nil {
		h.t.Fatalf("manifest of version %d: %v", n, err)
	}
	m, err := e2e.ParseManifest(plain)
	if err != nil {
		h.t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range m.Files {
		b, err := e2e.OpenBytes(get(f.Object), key, e2e.FileBinding(id, n, f.Path))
		if err != nil {
			h.t.Fatalf("%s: %v", f.Path, err)
		}
		files[f.Path] = b
	}
	return m, files
}

func itoa(n int) string { return strconv.Itoa(n) }

// tarGz packs files (name to content) as an upload.
func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for name, b := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	return out.Bytes()
}

func sealed(t *testing.T, key e2e.Key, b e2e.Binding, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := e2e.Seal(&out, bytes.NewReader(plain), int64(len(plain)), key, b, rand.Reader); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// uploadStatus is the status of reading an upload session.
func (h *harness) uploadStatus(token, id string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/uploads/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestE2EShareStoresAndServesOnlyCiphertext(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, dir, "site/index.html", "<!doctype html><title>Secret plans</title><p>"+e2eSecret+"</p>")
	writeFile(t, dir, "site/docs/notes.txt", e2eSecret+" in a note")
	var remembered string
	a, key, err := c.ShareE2E(ctx, &client.Source{Path: filepath.Join(dir, "site")}, client.PublishOptions{},
		func(id string, _ client.E2EKey) error { remembered = id; return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !a.E2E || a.ID != remembered || a.Title != "" || a.Kind != api.KindBundle || a.NextVersion != 2 || a.E2EKeyCheck != key.Check(a.ID) {
		t.Fatalf("artifact: %+v (reserved %s)", a, remembered)
	}
	if strings.Contains(a.URL, "#") {
		t.Fatalf("the server's link has a fragment: %s", a.URL)
	}

	// The data directory never holds the plaintext, a path or the title.
	filepath.WalkDir(h.app.cfg.DataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		for _, s := range []string{e2eSecret, "Secret plans", "notes.txt", key.String()} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds %q", p, s)
			}
		}
		return nil
	})

	// A top-level page load gets the decrypting viewer, at any path.
	for _, path := range []string{"/", "/docs/notes.txt", "/no/such/page"} {
		r := h.fetchContent(a.URL, path, nil, docHeaders...)
		expectStatus(t, r, 200, "viewer "+path)
		page := body(r)
		if strings.Contains(page, e2eSecret) || !strings.Contains(page, `src="/__redrop/e2e.js"`) || strings.Contains(page, ` src="/__redrop/v/`) {
			t.Fatalf("viewer page %s: %s", path, page)
		}
		if viewerAttr(t, page, "artifact") != a.ID || viewerAttr(t, page, "version") != "1" || viewerAttr(t, page, "key-check") != a.E2EKeyCheck {
			t.Fatalf("viewer data: %s", page)
		}
		if want := "/__redrop/v/1" + path; viewerAttr(t, page, "src") != want {
			t.Fatalf("frame page %q, want %q", viewerAttr(t, page, "src"), want)
		}
		csp := r.Header.Get("Content-Security-Policy")
		for _, want := range []string{"worker-src http://" + hostOf(t, a.URL) + "/__redrop/e2e-sw.js", "/__redrop/e2e.js", "connect-src http://" + hostOf(t, a.URL) + "/__redrop/v/", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Fatalf("viewer CSP %q lacks %q", csp, want)
			}
		}
	}

	m, files := h.decryptVersion(a.URL, a.ID, 1, key, nil)
	if m.Entry != "index.html" || m.Title != "Secret plans" || !strings.Contains(string(files["index.html"]), e2eSecret) || string(files["docs/notes.txt"]) != e2eSecret+" in a note" {
		t.Fatalf("decrypted: %+v", m)
	}
	for _, f := range m.Files {
		if f.Path == "index.html" && f.Type != "text/html; charset=utf-8" {
			t.Fatalf("type chosen by the CLI: %q", f.Type)
		}
	}

	// The live viewer's marker (review 33 re-check B-2a) is the viewer's
	// alone: never in the frame's address or a redirect into the version
	// space, while the artifact's own query is kept.
	for path, want := range map[string]string{
		"/?redrop-e2e=1":                   "/__redrop/v/1/",
		"/docs/notes.txt?q=1&redrop-e2e=1": "/__redrop/v/1/docs/notes.txt?q=1",
		"/?redrop-e2e=1&redrop-version=1":  "/__redrop/v/1/",
	} {
		page := body(h.fetchContent(a.URL, path, nil, docHeaders...))
		if got := viewerAttr(t, page, "src"); got != want {
			t.Fatalf("frame page for %s: %q, want %q", path, got, want)
		}
		r := h.fetchContent(a.URL, path, nil, frameHeaders...)
		if r.StatusCode != 303 || r.Header.Get("Location") != want {
			t.Fatalf("frame request for %s: %d %q, want %q", path, r.StatusCode, r.Header.Get("Location"), want)
		}
	}

	// Frames and clients without Fetch Metadata go to the version space;
	// a top-level load of a version path goes to the viewer form.
	r := h.fetchContent(a.URL, "/docs/notes.txt?x=1", nil, frameHeaders...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/__redrop/v/1/docs/notes.txt?x=1" {
		t.Fatalf("frame outside the version space: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := h.fetchContent(a.URL, "/", nil); r.StatusCode != 303 || r.Header.Get("Location") != "/__redrop/v/1/" {
		t.Fatalf("no Fetch Metadata: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := h.fetchContent(a.URL, "/__redrop/v/1/docs/a.html", nil, docHeaders...); r.StatusCode != 303 || r.Header.Get("Location") != "/docs/a.html" {
		t.Fatalf("top-level version path: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// Only stored names are served; plaintext paths are unknown to the server.
	for _, p := range []string{"/__redrop/v/1/index.html", "/__redrop/v/1/docs/notes.txt", "/manifest", "/__redrop/v/2/manifest"} {
		if r := h.fetchContent(a.URL, p, nil, fetchHeaders...); r.StatusCode == 200 {
			t.Fatalf("%s served: %s", p, body(r))
		}
	}

	// The worker script: on this host only, past the gates; nowhere else.
	r = h.fetchContent(a.URL, "/__redrop/e2e-sw.js", nil, swHeaders...)
	expectStatus(t, r, 200, "worker script")
	if js := body(r); !strings.Contains(js, "RedropE2E") || !strings.Contains(js, "redrop-e2e-need-key") || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("worker script: %s", r.Header.Get("Content-Type"))
	}
	plainFile := writeFile(t, dir, "plain.html", "<p>plain</p>")
	plain, err := c.Share(ctx, &client.Source{Path: plainFile}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{plain.URL, "http://amber-otter-quilt-river.content.test/"} {
		expectStatus(t, h.fetchContent(link, "/__redrop/e2e-sw.js", nil, swHeaders...), 404, "worker on "+link)
	}
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/viewer.js", nil, swHeaders...), 404, "another worker script")
}

// Every version of an artifact is on one side of the line, uses its key,
// and keeps its link.
func TestE2EVersionsKeepTheLinkAndTheRules(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	ctx := context.Background()
	dir := t.TempDir()
	page := writeFile(t, dir, "page.html", "<title>v1</title>one")
	a, key, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "page.html", "<title>v2</title>two")
	b, err := c.UpdateE2E(ctx, a.ID, key, &client.Source{Path: page}, client.VersionOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.URL != a.URL || b.Version != 2 || !b.E2E {
		t.Fatalf("update: %+v", b)
	}
	if _, files := h.decryptVersion(a.URL, a.ID, 2, key, nil); string(files["index.html"]) != "<title>v2</title>two" {
		t.Fatal("version 2 does not open with the link's key")
	}
	// Older versions stay closed unless the owner opens them.
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/v/1/manifest", nil, fetchHeaders...), 404, "old version, closed")
	on := true
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{OldVersions: &on}); err != nil {
		t.Fatal(err)
	}
	if _, files := h.decryptVersion(a.URL, a.ID, 1, key, nil); string(files["index.html"]) != "<title>v1</title>one" {
		t.Fatal("version 1 does not open")
	}

	// Watch publishes through the same path: same link, same key, and the
	// digest is the source's, so unchanged content isn't published again.
	writeFile(t, dir, "page.html", "<title>v3</title>three")
	up := &client.E2EUploader{Client: c, Key: key}
	digest, w, err := up.UpdateDigest(ctx, a.ID, &client.Source{Path: page}, nil)
	if err != nil || w.URL != a.URL || w.Version != 3 {
		t.Fatalf("watch publish: %+v %v", w, err)
	}
	if want := sha256Hex("<title>v3</title>three"); digest != want {
		t.Fatalf("watch digest %s, want the source's %s", digest, want)
	}

	// Plaintext versions can't join (TestE2EPlaintextNeverUploaded covers
	// that), nor encrypted ones a plaintext artifact.
	plain, err := c.Share(ctx, &client.Source{Path: page}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateE2E(ctx, plain.ID, key, &client.Source{Path: page}, client.VersionOptions{}, nil); err == nil || !strings.Contains(err.Error(), "not end-to-end encrypted") {
		t.Fatalf("e2e version on a plaintext artifact, client side: %v", err)
	}
	other, _ := client.NewE2EKey()
	if _, err := c.UpdateE2E(ctx, a.ID, other, &client.Source{Path: page}, client.VersionOptions{}, nil); !errors.Is(err, client.ErrE2EKeyMismatch) {
		t.Fatalf("wrong key, client side: %v", err)
	}

	// The server's checks, for clients that skip their own. Each upload is
	// declared for the artifact it is published to.
	upload := func(content string, target client.UploadTarget) (string, string, error) {
		f := writeFile(t, dir, "raw.bin", content)
		return c.UploadFileFor(ctx, f, target, nil)
	}
	encrypted := encryptedBundle(t, key, a.ID, 4)
	publish := func(id string, req api.PublishRequest) error {
		_, err := c.CreateVersion(ctx, id, &req)
		return err
	}
	for name, tc := range map[string]struct {
		id      string
		content string
		e2e     *api.E2EPublish
		spa     bool
		status  int
		title   string
	}{
		"another key's check":     {a.ID, encrypted, &api.E2EPublish{KeyCheck: other.Check(a.ID), Version: 4}, false, 400, ""},
		"a taken version number":  {a.ID, encrypted, &api.E2EPublish{KeyCheck: key.Check(a.ID), Version: 3}, false, 409, api.CodeVersionConflict},
		"a future version number": {a.ID, encrypted, &api.E2EPublish{KeyCheck: key.Check(a.ID), Version: 9}, false, 409, api.CodeVersionConflict},
		"SPA fallback":            {a.ID, encrypted, &api.E2EPublish{KeyCheck: key.Check(a.ID), Version: 4}, true, 400, ""},
		"plaintext labelled e2e":  {a.ID, plainBundle(t), &api.E2EPublish{KeyCheck: key.Check(a.ID), Version: 4}, false, 400, ""},
	} {
		id, sum, err := upload(tc.content, client.UploadTarget{ArtifactID: tc.id, E2E: true})
		if err != nil {
			t.Fatal(err)
		}
		err = publish(tc.id, api.PublishRequest{UploadID: id, SHA256: sum, Kind: api.KindBundle, SPA: tc.spa, E2E: tc.e2e})
		var e *client.Error
		if !errors.As(err, &e) || e.Status != tc.status || (tc.title != "" && e.Title != tc.title) {
			t.Errorf("%s: %v", name, err)
		}
		// Each rejection deletes its upload: it can never be published.
		if got := h.uploadStatus(token, id); got != 404 {
			t.Errorf("%s: upload kept (status %d)", name, got)
		}
	}
	// An encrypted upload for a plaintext artifact is refused when declared.
	if _, _, err := upload(encrypted, client.UploadTarget{ArtifactID: plain.ID, E2E: true}); client.StatusOf(err) != 400 {
		t.Fatalf("e2e upload declared for a plaintext artifact: %v", err)
	}
	id, sum, err := upload(encrypted, client.UploadTarget{ArtifactID: a.ID, E2E: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := publish(a.ID, api.PublishRequest{UploadID: id, SHA256: sum, Kind: api.KindBundle, E2E: &api.E2EPublish{KeyCheck: key.Check(a.ID), Version: 4}}); err != nil {
		t.Fatalf("a correct raw publication: %v", err)
	}
	spa := true
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{SPA: &spa}); client.StatusOf(err) != 400 {
		t.Fatalf("turning SPA on: %v", err)
	}
	vs, err := c.ListVersions(ctx, a.ID)
	if err != nil || len(vs) != 4 || !vs[0].E2E || !vs[3].E2E {
		t.Fatalf("versions: %+v %v", vs, err)
	}

	// Reservations: only the token's own, unforged.
	res, err := c.ReserveArtifact(ctx)
	if err != nil || res.ID == "" || res.Token == "" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	bob := client.New(h.srv.URL, h.tokenFor(h.inviteUser("bob")))
	for name, tc := range map[string]struct {
		c   *client.Client
		tok string
	}{
		"forged": {c, res.Token[:len(res.Token)-4] + "AAAA"}, "empty": {c, ""}, "another user's": {bob, res.Token},
	} {
		f := writeFile(t, dir, "raw.bin", encryptedBundle(t, key, res.ID, 1))
		id, sum, err := tc.c.UploadFileFor(ctx, f, client.UploadTarget{E2E: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tc.c.CreateArtifact(ctx, &api.CreateArtifactRequest{PublishRequest: api.PublishRequest{UploadID: id, SHA256: sum, Kind: api.KindBundle,
			E2E: &api.E2EPublish{KeyCheck: key.Check(res.ID), Version: 1, Reservation: tc.tok}}})
		if client.StatusOf(err) != 400 {
			t.Errorf("reservation %s: %v", name, err)
		}
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// encryptedBundle is a valid encrypted upload for (id, version).
func encryptedBundle(t *testing.T, key e2e.Key, id string, version int) string {
	t.Helper()
	return string(tarGz(t, map[string][]byte{
		e2e.ManifestPath:             sealed(t, key, e2e.ManifestBinding(id, version), []byte(`{"format":1,"kind":"html","title":"","entry":"index.html","files":[{"path":"index.html","object":"aaaaaaaaaaaaaaaaaaaaaaaaaa","type":"text/html","size":1}]}`)),
		"aaaaaaaaaaaaaaaaaaaaaaaaaa": sealed(t, key, e2e.FileBinding(id, version, "index.html"), []byte("x")),
	}))
}

// plainBundle has the encrypted layout but plaintext bytes.
func plainBundle(t *testing.T) string {
	return string(tarGz(t, map[string][]byte{e2e.ManifestPath: []byte("{}"), "aaaaaaaaaaaaaaaaaaaaaaaaaa": []byte("<p>plain</p>")}))
}

// Passwords, view limits and revocation hold for the viewer page, the
// ciphertext and the worker script alike.
func TestE2EAccessChecks(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	page := writeFile(t, t.TempDir(), "page.html", "<p>"+e2eSecret+"</p>")
	a, key, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{Password: "hunter22", MaxViews: 1}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := h.fetchContent(a.URL, "/", nil, docHeaders...); r.StatusCode != 200 || !strings.Contains(body(r), "password protected") {
		t.Fatal("no gate on the viewer page")
	}
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/v/1/manifest", nil, fetchHeaders...), 403, "ciphertext without access")
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/e2e-sw.js", nil, swHeaders...), 403, "worker without access")

	r := h.content("POST", a.URL, "/__redrop/open", url.Values{"password": {"hunter22"}, "next": {"/"}}, "")
	expectStatus(t, r, 303, "open")
	visit := r.Cookies()
	r = h.fetchContent(a.URL, "/", visit, docHeaders...)
	expectStatus(t, r, 200, "viewer after open")
	if !strings.Contains(body(r), "data-e2e") {
		t.Fatal("no viewer after open")
	}
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/e2e-sw.js", visit, swHeaders...), 200, "worker during the visit")
	if _, files := h.decryptVersion(a.URL, a.ID, 1, key, visit); !strings.Contains(string(files["index.html"]), e2eSecret) {
		t.Fatal("content during the visit")
	}
	got, err := c.GetArtifact(ctx, a.ID)
	if err != nil || got.Views != 1 || got.Status != api.StatusExhausted {
		t.Fatalf("a visit is one view, of objects and worker requests alike: %+v %v", got, err)
	}
	// The one view is used: everyone else gets the miss, for everything.
	for _, p := range []string{"/", "/__redrop/v/1/manifest", "/__redrop/e2e-sw.js"} {
		expectStatus(t, h.fetchContent(a.URL, p, nil, docHeaders...), 404, "after the last view "+p)
	}
	// Revoking ends the visit too.
	revoked := true
	if _, err := c.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{Revoked: &revoked}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/__redrop/v/1/manifest", "/__redrop/e2e-sw.js"} {
		expectStatus(t, h.fetchContent(a.URL, p, visit, fetchHeaders...), 404, "revoked "+p)
	}
}

// recordingTransport keeps every request the client sends.
type recordingTransport struct {
	mu   sync.Mutex
	sent bytes.Buffer
	n    atomic.Int64
}

// String is everything recorded so far.
func (rt *recordingTransport) String() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.sent.String()
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	rt.mu.Lock()
	rt.sent.WriteString(req.Method + " " + req.URL.String() + "\n")
	req.Header.Write(&rt.sent)
	rt.sent.Write(b)
	rt.mu.Unlock()
	rt.n.Add(1)
	return http.DefaultTransport.RoundTrip(req)
}

// The key never reaches the server: not in any request the client sends
// (paths, queries, headers, bodies, uploads) nor in any server log line.
func TestE2EKeyNeverReachesTheServer(t *testing.T) {
	var logs syncBuffer
	h := newHarnessWith(t, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Options{})
	c := client.New(h.srv.URL, h.setupAdmin())
	rt := &recordingTransport{}
	c.HTTP = &http.Client{Transport: rt}
	ctx := context.Background()
	dir := t.TempDir()
	page := writeFile(t, dir, "page.html", "<p>one</p>")
	a, key, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "page.html", "<p>two</p>")
	if _, err := c.UpdateE2E(ctx, a.ID, key, &client.Source{Path: page}, client.VersionOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&client.E2EUploader{Client: c, Key: key}).UpdateDigest(ctx, a.ID, &client.Source{Path: page, Note: "three"}, nil); err != nil {
		t.Fatal(err)
	}
	h.fetchContent(a.URL, "/", nil, docHeaders...)
	h.decryptVersion(a.URL, a.ID, 3, key, nil)
	if rt.n.Load() < 6 {
		t.Fatalf("only %d requests recorded", rt.n.Load())
	}
	raw := key[:]
	forms := []string{key.String(), hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw), base64.RawStdEncoding.EncodeToString(raw)}
	rt.mu.Lock()
	sent := rt.sent.String()
	rt.mu.Unlock()
	for _, s := range forms {
		if strings.Contains(sent, s) {
			t.Errorf("a request carried the key (%s)", s)
		}
		if strings.Contains(logs.String(), s) {
			t.Errorf("a log line carried the key (%s)", s)
		}
	}
	if !strings.Contains(sent, key.Check(a.ID)) {
		t.Fatal("the recorder missed the publication bodies")
	}
}

// A publication that lost its version number to another one is encrypted
// for the new number and published again.
func TestE2EUpdateRetriesAVersionConflict(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	ctx := context.Background()
	dir := t.TempDir()
	page := writeFile(t, dir, "page.html", "<p>one</p>")
	a, key, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Another client publishes version 2 between this client's read of the
	// artifact and its publication.
	rival := writeFile(t, dir, "rival.html", "<p>rival</p>")
	var raced atomic.Bool
	c.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/versions") && raced.CompareAndSwap(false, true) {
			other := client.New(h.srv.URL, token)
			if _, err := other.UpdateE2E(ctx, a.ID, key, &client.Source{Path: rival}, client.VersionOptions{}, nil); err != nil {
				t.Errorf("rival publish: %v", err)
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	writeFile(t, dir, "page.html", "<p>two</p>")
	b, err := c.UpdateE2E(ctx, a.ID, key, &client.Source{Path: page}, client.VersionOptions{}, nil)
	if err != nil || b.Version != 3 || !raced.Load() {
		t.Fatalf("update after a conflict: %+v %v", b, err)
	}
	if _, files := h.decryptVersion(a.URL, a.ID, 3, key, nil); string(files["index.html"]) != "<p>two</p>" {
		t.Fatal("version 3 is not this client's content")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// B-1 of review 33: plaintext meant for an end-to-end encrypted artifact
// never leaves the client, and the server refuses it before any byte of it
// arrives, for clients that don't check.
func TestE2EPlaintextNeverUploaded(t *testing.T) {
	h := newHarness(t)
	token := h.setupAdmin()
	c := client.New(h.srv.URL, token)
	rt := &recordingTransport{}
	ctx := context.Background()
	dir := t.TempDir()
	page := writeFile(t, dir, "page.html", "<p>encrypted one</p>")
	a, key, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = &http.Client{Transport: rt}
	writeFile(t, dir, "page.html", "<p>"+e2eSecret+"</p>")
	off := false
	if _, err := c.UpdateWithOptions(ctx, a.ID, &client.Source{Path: page}, client.VersionOptions{InfoButton: &off}, nil); !errors.Is(err, client.ErrEncryptedArtifact) {
		t.Fatalf("plaintext update of an e2e artifact: %v", err)
	}
	if _, _, err := c.UpdateDigest(ctx, a.ID, &client.Source{Path: page}, nil); !errors.Is(err, client.ErrEncryptedArtifact) {
		t.Fatalf("plaintext watch publish of an e2e artifact: %v", err)
	}
	rt.mu.Lock()
	sent := rt.sent.String()
	rt.mu.Unlock()
	if strings.Contains(sent, "/api/v1/uploads") || strings.Contains(sent, e2eSecret) || strings.Contains(sent, "PATCH") {
		t.Fatalf("the client sent an upload, plaintext or a settings change:\n%s", sent)
	}
	if got, err := c.GetArtifact(ctx, a.ID); err != nil || got.Version != 1 || !got.InfoButton {
		t.Fatalf("artifact changed: %+v %v", got, err)
	}

	// A client that skips the check: the upload is refused when it is
	// created for the artifact, and an undeclared one can't be published to it.
	raw := writeFile(t, dir, "raw.html", "<p>"+e2eSecret+"</p>")
	if _, _, err := c.UploadFileFor(ctx, raw, client.UploadTarget{ArtifactID: a.ID}, nil); client.StatusOf(err) != 400 {
		t.Fatalf("plaintext upload declared for an e2e artifact: %v", err)
	}
	if !strings.Contains(rt.String(), `"artifact_id":"`+a.ID+`"`) || strings.Contains(rt.String(), "PUT /api/v1/uploads") {
		t.Fatal("the refused upload sent bytes")
	}
	id, sum, err := c.UploadFile(ctx, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateVersion(ctx, a.ID, &api.PublishRequest{UploadID: id, SHA256: sum, Kind: api.KindHTML}); client.StatusOf(err) != 400 {
		t.Fatalf("an undeclared plaintext upload published to an e2e artifact: %v", err)
	}
	// Undeclared, it can't pose as encrypted content either.
	enc := writeFile(t, dir, "enc.bin", encryptedBundle(t, key, a.ID, 2))
	id, sum, err = c.UploadFile(ctx, enc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateVersion(ctx, a.ID, &api.PublishRequest{UploadID: id, SHA256: sum, Kind: api.KindBundle,
		E2E: &api.E2EPublish{KeyCheck: key.Check(a.ID), Version: 2}}); client.StatusOf(err) != 400 {
		t.Fatalf("an undeclared upload published as an e2e version: %v", err)
	}
	// The key's holder still publishes, declared.
	if _, err := c.UpdateE2E(ctx, a.ID, key, &client.Source{Path: page}, client.VersionOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rt.String(), `"artifact_id":"`+a.ID+`","e2e":true`) {
		t.Fatal("the encrypted upload was not declared for the artifact")
	}
	// Plaintext artifacts: updates declare their artifact, and old clients'
	// undeclared uploads still publish.
	plain, err := c.Share(ctx, &client.Source{Path: page}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(ctx, plain.ID, &client.Source{Path: page}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rt.String(), `"artifact_id":"`+plain.ID+`"}`) {
		t.Fatal("a plaintext update did not declare its artifact")
	}
	id, sum, err = c.UploadFile(ctx, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateVersion(ctx, plain.ID, &api.PublishRequest{UploadID: id, SHA256: sum, Kind: api.KindHTML}); err != nil {
		t.Fatalf("an undeclared upload for a plaintext artifact: %v", err)
	}
}

// M3 of the second re-check of review 33: a link whose "#" became "%23"
// (or "%2523") sends its key to the server in the path; and B-2b″ of the
// third: one whose "#" became "?" sends it in the query. A top-level load
// gets a fixed damaged-link page, before any gate: no viewer, no frame
// address and no gate form built from the request, no view, no key in the
// answer or the log.
func TestE2EKeyInThePathOpensNothing(t *testing.T) {
	var logs syncBuffer
	h := newHarnessWith(t, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Options{})
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	page := writeFile(t, t.TempDir(), "page.html", "<p>"+e2eSecret+"</p>")
	open, key, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gated, _, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{Password: "hunter22"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []*api.Artifact{open, gated} {
		for _, p := range []string{
			"/%23k=" + key.String(), "/%2523k=" + key.String(), "/docs/%23k%3D" + key.String(), "/__redrop/v/1/%23k=" + key.String(),
			"/?k=" + key.String(), "/?x=1&k=" + key.String(), "/?K=" + key.String(), "/?k%3D" + key.String(),
			"/?redrop-e2e=1&k=" + key.String(), "/docs/a.html?k=" + key.String(), "/__redrop/v/1/?k=" + key.String(),
		} {
			r := h.fetchContent(a.URL, p, nil, docHeaders...)
			b := body(r)
			if r.StatusCode != http.StatusBadRequest || !strings.Contains(b, "This link is damaged") || r.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("%s: %d %q %s", p, r.StatusCode, r.Header.Get("Cache-Control"), b)
			}
			if strings.Contains(b, key.String()) || strings.Contains(b, "rd-frame") || strings.Contains(b, "password") {
				t.Fatalf("%s: the answer holds the key, a frame or a gate: %s", p, b)
			}
		}
		// A frame or a script isn't a link anyone opened: the usual answers.
		if r := h.fetchContent(a.URL, "/%23k="+key.String(), nil, frameHeaders...); r.StatusCode == http.StatusBadRequest {
			t.Fatal("a frame request got the damaged-link page")
		}
	}
	if got, err := c.GetArtifact(ctx, open.ID); err != nil || got.Views != 0 {
		t.Fatalf("views after damaged links: %v %v", got, err)
	}
	// An ordinary query still opens the viewer, and keeps its parameters.
	if r := h.fetchContent(open.URL, "/?q=1&page=2&redrop-e2e=1", nil, docHeaders...); r.StatusCode != http.StatusOK || viewerAttr(t, body(r), "src") != "/__redrop/v/1/?page=2&amp;q=1" {
		t.Fatalf("an ordinary query: %d", r.StatusCode)
	}
	// A "#" in a path, or k= in a query, is only refused on encrypted hosts:
	// a plaintext artifact's own query strings keep working.
	plainFile := writeFile(t, t.TempDir(), "plain.html", "<p>plain</p>")
	plain, err := c.Share(ctx, &client.Source{Path: plainFile}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/%23k=x", "/?k=" + key.String()} {
		if r := h.fetchContent(plain.URL, p, nil, docHeaders...); r.StatusCode == http.StatusBadRequest {
			t.Fatalf("a plaintext artifact got the damaged-link page for %s", p)
		}
	}
	if strings.Contains(logs.String(), key.String()) || strings.Contains(logs.String(), "%23") {
		t.Fatalf("the key reached the log:\n%s", logs.String())
	}
}

// M2 of review 33: a fragment a client sent encoded in a path (an
// end-to-end key pasted where an ID belongs) never reaches the log, encoded
// once (%23) or more (%2523: what a client that path-escapes "ID%23k=..."
// sends).
func TestE2EFragmentsAreNeverLogged(t *testing.T) {
	var logs syncBuffer
	h := newHarnessWith(t, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Options{})
	token := h.setupAdmin()
	const secret = "SecretKeyText0123456789abcdefghijklmnopqrs"
	for _, p := range []string{
		"/api/v1/artifacts/k2j4m9qx7d4abcde%23k=" + secret, "/api/v1/artifacts/k2j4m9qx7d4abcde%23k=" + secret + "/versions", "/no/such%23k=" + secret,
		"/api/v1/artifacts/k2j4m9qx7d4abcde%2523k=" + secret, "/api/v1/artifacts/k2j4m9qx7d4abcde%2523k%253D" + secret + "/versions",
		"/api/v1/artifacts/k2j4m9qx7d4abcde/%252523k=" + secret, "/no/such%2523k=" + secret,
	} {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if !strings.Contains(logs.String(), "/api/v1/artifacts/k2j4m9qx7d4abcde#…") {
		t.Fatalf("the requests were not logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "%23") {
		t.Fatalf("a fragment was logged:\n%s", logs.String())
	}
}

// M1 of review 33: the gate keeps the link's fragment through Open. Its one
// inline script is pinned by hash in the gate's policy, and the served
// script is exactly the hashed one.
func TestGateScriptIsPinned(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	page := writeFile(t, t.TempDir(), "page.html", "<p>x</p>")
	a, _, err := c.ShareE2E(context.Background(), &client.Source{Path: page}, client.PublishOptions{Password: "hunter22"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := h.fetchContent(a.URL, "/", nil, docHeaders...)
	expectStatus(t, r, 200, "gate")
	html := body(r)
	_, script, ok := strings.Cut(html, "<script>")
	script, _, ok2 := strings.Cut(script, "</script>")
	if !ok || !ok2 || !strings.Contains(script, "location.hash") {
		t.Fatalf("gate script: %s", html)
	}
	sum := sha256.Sum256([]byte(script))
	csp := r.Header.Get("Content-Security-Policy")
	if want := "script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
		t.Fatalf("gate CSP %q lacks %q", csp, want)
	}
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src") && strings.Count(d, " ") != 1 {
			t.Fatalf("the gate allows more than its pinned script: %q", d)
		}
	}
	if strings.Count(html, "<script") != 1 {
		t.Fatal("the gate has more than one script")
	}
	// Other server pages still run no script at all.
	if csp := h.fetchContent("http://amber-otter-quilt-river.content.test/", "/", nil, docHeaders...).Header.Get("Content-Security-Policy"); strings.Contains(csp, "script-src") {
		t.Fatalf("the miss page allows scripts: %q", csp)
	}
}

// Key intake on the app origin (B-2a″ of the third re-check): the link is
// the app's /e/<slug> page, which holds no artifact data, looks nothing
// up, counts no view and opens no gate. Its policy allows only its own
// script and pinned style, and it is never cached, framed or referred. The
// helper on the artifact's origin takes the key only from the app origin.
func TestE2EIntakeOnTheAppOrigin(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	page := writeFile(t, t.TempDir(), "page.html", "<title>Secret plans</title><p>"+e2eSecret+"</p>")
	a, _, err := c.ShareE2E(ctx, &client.Source{Path: page}, client.PublishOptions{Password: "hunter22", Title: "Plans title"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(a.URL, "http://"), ".content.test/")
	if a.E2ELink != "http://app.test/e/"+slug {
		t.Fatalf("e2e_link %q for %s", a.E2ELink, a.URL)
	}
	plainFile := writeFile(t, t.TempDir(), "plain.html", "<p>plain</p>")
	plain, err := c.Share(ctx, &client.Source{Path: plainFile}, client.PublishOptions{}, nil)
	if err != nil || plain.E2ELink != "" {
		t.Fatalf("a plaintext artifact has an e2e link: %+v %v", plain, err)
	}

	for _, s := range []string{slug, "amber-otter-quilt-river"} { // a real slug and one nobody has
		r := h.raw("GET", "/e/"+s, "", nil)
		b := body(r)
		expectStatus(t, r, 200, "intake "+s)
		hd := r.Header
		if hd.Get("Cache-Control") != "no-store" || hd.Get("Referrer-Policy") != "no-referrer" || hd.Get("Cross-Origin-Opener-Policy") != "same-origin-allow-popups" {
			t.Fatalf("intake headers: %v", hd)
		}
		csp := hd.Get("Content-Security-Policy")
		style := b[strings.Index(b, "<style>")+len("<style>") : strings.Index(b, "</style>")]
		sum := sha256.Sum256([]byte(style))
		want := "default-src 'none'; script-src http://app.test/e2e/intake.js; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
		if csp != want {
			t.Fatalf("intake CSP\n%s\nwant\n%s", csp, want)
		}
		if !strings.Contains(b, `data-content="http://`+s+`.content.test/"`) || !strings.Contains(b, `<script src="/e2e/intake.js"></script>`) {
			t.Fatalf("intake page: %s", b)
		}
		for _, leak := range []string{a.ID, "Plans title", "Secret plans", a.E2EKeyCheck, "password"} {
			if strings.Contains(b, leak) {
				t.Fatalf("the intake page holds %q", leak)
			}
		}
	}
	slash := h.raw("GET", "/e/"+slug+"/", "", nil)
	if b := body(slash); slash.StatusCode != 200 || !strings.Contains(b, `data-content="`+a.URL+`"`) {
		t.Fatalf("trailing-slash intake: %d %s", slash.StatusCode, b)
	}
	expectStatus(t, h.raw("GET", "/e/not_a_slug", "", nil), 404, "intake for a malformed slug")
	r := h.raw("GET", "/e2e/intake.js", "", nil)
	if js := body(r); r.StatusCode != 200 || !strings.Contains(js, "RedropE2E") || !strings.Contains(js, "rd-intake") || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("intake script: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}

	// The helper, on the artifact's host: no gate, no view, nothing of the
	// artifact; it takes keys from the app origin only.
	r = h.fetchContent(a.URL, "/__redrop/e2e-intake", nil, docHeaders...)
	b := body(r)
	expectStatus(t, r, 200, "helper")
	if r.Header.Get("Cache-Control") != "no-store" || !strings.Contains(b, `data-app="http://app.test"`) || !strings.Contains(b, `<script src="/__redrop/e2e-intake.js"></script>`) {
		t.Fatalf("helper: %v %s", r.Header, b)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src http://"+slug+".content.test/__redrop/e2e-intake.js;") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("helper CSP %q", csp)
	}
	for _, leak := range []string{a.ID, "Plans title", a.E2EKeyCheck, "password"} {
		if strings.Contains(b, leak) {
			t.Fatalf("the helper holds %q", leak)
		}
	}
	r = h.fetchContent(a.URL, "/__redrop/e2e-intake.js", nil, fetchHeaders...)
	if js := body(r); r.StatusCode != 200 || !strings.Contains(js, "RedropE2E") || !strings.Contains(js, "redrop-e2e-key") {
		t.Fatalf("helper script: %d", r.StatusCode)
	}
	expectStatus(t, h.fetchContent("http://content.test/", "/__redrop/e2e-intake", nil, docHeaders...), 404, "helper on the apex")
	// The gate still stands in front of the artifact, and nothing counted a view.
	if b := body(h.fetchContent(a.URL, "/", nil, docHeaders...)); !strings.Contains(b, "password") {
		t.Fatal("the gate is gone")
	}
	if got, err := c.GetArtifact(ctx, a.ID); err != nil || got.Views != 0 {
		t.Fatalf("views after intake and helper: %v %v", got, err)
	}
}
