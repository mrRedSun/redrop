package content

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

// The viewer page (DESIGN.md "Viewer frame") is redrop's own page on the
// artifact's origin. It frames the artifact from the same origin and owns the
// info button outside the frame. It runs no inline script: its behaviour is
// the static /__redrop/viewer.js, its look /__redrop/viewer.css.

//go:embed static/viewer.js static/viewer.css
var viewerFS embed.FS

type viewerAsset struct {
	body        []byte
	contentType string
	etag        string
}

var viewerAssets = func() map[string]viewerAsset {
	out := map[string]viewerAsset{}
	for name, ct := range map[string]string{"viewer.js": "text/javascript; charset=utf-8", "viewer.css": "text/css; charset=utf-8"} {
		b, err := viewerFS.ReadFile("static/" + name)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(b)
		out[reservedPrefix+name] = viewerAsset{body: b, contentType: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	}
	return out
}()

// serveViewerAsset serves the viewer's static script and stylesheet on any
// content host, before any lookup: they never depend on an artifact.
func serveViewerAsset(w http.ResponseWriter, r *http.Request) bool {
	a, ok := viewerAssets[r.URL.Path]
	if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	hd := w.Header()
	hd.Set("Content-Type", a.contentType)
	hd.Set("Cache-Control", "public, no-cache")
	hd.Set("ETag", a.etag)
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(a.body)))
	return true
}

// viewer renders the viewer page for snap: the info button and panel, and a
// full-viewport frame of the same path and query on this origin (under
// /__redrop/v/<N>/ for an older version). Directory redirects and missing
// paths answer as they would without the viewer; only real pages get it.
func (h *Handler) viewer(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, d sharing.Decision) {
	w.Header().Add("Vary", "Sec-Fetch-Dest") // the same URL is content for frames
	switch t := h.sharing.ResolvePath(r.Context(), snap, r.URL.Path); t.Kind {
	case sharing.TargetRedirect:
		http.Redirect(w, r, withQuery(t.Location, r.URL.RawQuery), http.StatusMovedPermanently)
		return
	case sharing.TargetNotFound:
		h.pages.notFound(w)
		return
	}
	h.countView(w, r, a, d)
	frameSrc := withoutParams(r.URL, paramVersion, paramInfo)
	data := viewerData{FrameSrc: frameSrc, Latest: withoutParams(r.URL, paramVersion)}
	if snap.Version != a.CurrentVersion {
		data.OldVersion, data.FrameSrc = snap.Version, versionBase(snap.Version)+frameSrc
	}
	data.Info = panelFor(h.sharing.ViewerInfo(r.Context(), a, snap), r.URL)
	self := h.selfOrigin(a)
	// frame-src also governs where the frame may navigate, so it allows the
	// web at large; the viewer itself loads nothing but its own two files.
	navigable := "https:"
	if h.cfg.ContentURL.Scheme == "http" {
		navigable = "http: https:"
	}
	csp := fmt.Sprintf("default-src 'none'; script-src %[1]s/__redrop/viewer.js; style-src %[1]s/__redrop/viewer.css; "+
		"img-src 'self'; frame-src 'self' %[2]s; form-action 'none'; base-uri 'none'; frame-ancestors 'none'", self, navigable)
	h.pages.render(w, http.StatusOK, "viewer.html", a.Title, serverIcons, data, csp)
}

type viewerData struct {
	Info       panelData
	FrameSrc   string
	OldVersion int    // the older version shown, 0 for the current one
	Latest     string // this page without the version parameter
}

// panelData is the info panel as templates render it, shared by the viewer
// page and the file page.
type panelData struct {
	sharing.ViewerInfo
	Revisions []revisionRow
}

type revisionRow struct {
	sharing.Revision
	Href    string // "" when it can't be opened
	Viewing bool
}

// panelFor builds the panel for page u: revision links keep u's path and
// other parameters and only set or drop the version parameter.
func panelFor(info sharing.ViewerInfo, u *url.URL) panelData {
	p := panelData{ViewerInfo: info}
	for _, rev := range info.Revisions {
		row := revisionRow{Revision: rev, Viewing: rev.Number == info.Version}
		switch {
		case rev.Current:
			row.Href = withoutParams(u, paramVersion, paramInfo)
		case info.CanOpen:
			q := u.Query()
			q.Del(paramInfo)
			q.Set(paramVersion, strconv.Itoa(rev.Number))
			row.Href = u.EscapedPath() + "?" + q.Encode()
		}
		p.Revisions = append(p.Revisions, row)
	}
	return p
}
