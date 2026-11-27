package content

import (
	"embed"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// End-to-end encrypted artifacts (DESIGN.md "End-to-end encrypted links",
// "Viewing"). The server holds only ciphertext objects. Every top-level page
// load gets the decrypting viewer page; ciphertext lives at
// /__redrop/v/<N>/<object>; redrop's service worker, served here only for
// these artifacts, decrypts the frame's requests in the browser.

//go:embed static/e2e.js static/e2e-sw.js
var e2eFS embed.FS

const (
	e2eJS         = reservedPrefix + "e2e.js"    // the decryptor, on every content host like viewer.js
	e2eWorkerPath = reservedPrefix + "e2e-sw.js" // the worker, on end-to-end artifacts' hosts only
	// paramE2E marks the live viewer's address (viewer.js keeps it), so a
	// full link differs from it by more than the fragment. It is never part
	// of the artifact's own page.
	paramE2E = "redrop-e2e"
)

// e2eWorker is the service worker script: the decryptor, then the worker.
var e2eWorker = func() viewerAsset {
	lib, err := e2eFS.ReadFile("static/e2e.js")
	if err != nil {
		panic(err)
	}
	sw, err := e2eFS.ReadFile("static/e2e-sw.js")
	if err != nil {
		panic(err)
	}
	return newViewerAsset(append(append(lib, '\n'), sw...), "text/javascript; charset=utf-8")
}()

// e2eWorkerScript serves the worker on the host of an end-to-end encrypted
// artifact, after its gates. Every other host gets the 404 every service
// worker script request gets.
func (h *Handler) e2eWorkerScript(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	if !a.E2E || sharing.IsTunnel(a) {
		h.pages.notFound(w)
		return
	}
	if _, ok := h.admit(w, r, a); !ok {
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", e2eWorker.contentType)
	hd.Set("ETag", e2eWorker.etag)
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(e2eWorker.body)))
}

// serveE2E answers a path of an end-to-end encrypted link outside the
// version space: the viewer page for a top-level page load, else a redirect
// into the version space, where the worker answers frames and scripts find
// the objects.
func (h *Handler) serveE2E(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, d sharing.Decision, direct bool) {
	w.Header().Add("Vary", "Sec-Fetch-Dest")
	if !wantsViewer(r) {
		redirect(w, r, versionBase(snap.Version)+withoutParams(r.URL, paramVersion, paramInfo, paramE2E), http.StatusSeeOther)
		return
	}
	h.countView(w, r, a, d)
	h.e2eViewer(w, r, a, snap, direct)
}

// serveE2EVersion answers /__redrop/v/<N>/<rest> of an end-to-end encrypted
// link: a stored object as ciphertext, for the current version, or an older
// one while the owner lets viewers open them. A top-level page load goes to
// the viewer form of the page.
func (h *Handler) serveE2EVersion(w http.ResponseWriter, r *http.Request, a *store.Artifact, n int, num, rest, sub string) {
	snap, err := h.sharing.SelectVersion(r.Context(), a, n)
	if err != nil {
		h.miss(w, r)
		return
	}
	if !strings.HasPrefix(rest, num+"/") {
		redirect(w, r, withQuery(versionBase(n)+"/", r.URL.RawQuery), http.StatusMovedPermanently)
		return
	}
	w.Header().Add("Vary", "Sec-Fetch-Dest")
	if wantsViewer(r) {
		q := r.URL.Query()
		q.Del(paramVersion)
		if n != a.CurrentVersion {
			q.Set(paramVersion, strconv.Itoa(n))
		}
		redirect(w, r, withQuery((&url.URL{Path: "/" + sub}).EscapedPath(), q.Encode()), http.StatusSeeOther)
		return
	}
	f := h.sharing.E2EObject(r.Context(), snap, sub)
	if f == nil {
		h.pages.notFound(w)
		return
	}
	h.sendFile(w, r, f, true)
}

// e2eViewerData is what the viewer script needs to decrypt: the authenticated
// data's artifact ID and version, and the key check. The ID is the one
// exception to "the panel never shows IDs": it grants nothing without the
// owner's token.
type e2eViewerData struct {
	Artifact string
	Version  int // the version shown
	Current  int
	KeyCheck string
}

// e2eViewer renders the decrypting viewer page for snap. The frame starts
// empty: the script points it at the version space once the worker has the
// key. With the info button off, or a direct-view choice, it has no panel;
// there is no direct view of ciphertext.
func (h *Handler) e2eViewer(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, direct bool) {
	page := withoutParams(r.URL, paramVersion, paramInfo, paramE2E)
	data := viewerData{
		FrameSrc: selfRef(versionBase(snap.Version) + page),
		Latest:   withoutParams(r.URL, paramVersion),
		Panel:    !direct && h.sharing.InfoShown(a),
		E2E:      &e2eViewerData{Artifact: a.ID, Version: snap.Version, Current: a.CurrentVersion, KeyCheck: a.E2EKeyCheck},
	}
	if snap.Version != a.CurrentVersion {
		data.OldVersion = snap.Version
	}
	data.Info = panelFor(h.sharing.ViewerInfo(r.Context(), a, snap), r.URL)
	data.Info.E2E, data.Info.Download = true, snap.Kind == api.KindFile
	data.Info.Feedback = data.Panel && h.sharing.FeedbackOpen(a)
	self := h.selfOrigin(a)
	// The reporter shares the artifact's address, never its decryption key.
	data.Info.Report = h.pages.reportLink(self + "/")
	navigable := "https:"
	if h.cfg.ContentURL.Scheme == "http" {
		navigable = "http: https:"
	}
	connect := "connect-src " + self + reservedPrefix + versionPrefix
	if data.Info.Feedback {
		connect += " " + self + feedbackPath
	}
	csp := pagePolicyScripts(self, "", []string{e2eJS},
		"img-src 'self'", "frame-src 'self' "+navigable, connect, "worker-src "+self+e2eWorkerPath,
		"form-action 'none'", "base-uri 'none'", "frame-ancestors 'none'")
	h.pages.renderWithCSP(w, http.StatusOK, "viewer.html", a.Title, serverIcons, data, csp)
}
