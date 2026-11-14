package content

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

// The viewer page (DESIGN.md "Viewer frame") is redrop's own page on the
// shared link. It frames the artifact from its raw host and owns the info
// button outside the frame. It runs no inline script: its behaviour is the
// static /__redrop/viewer.js, its look /__redrop/viewer.css.

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

// viewerPage renders the viewer for snap and mints the frame's grants.
func (h *Handler) viewerPage(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, g sharing.Grants) {
	f := h.sharing.Frame(a, snap, g)
	info := h.sharing.ViewerInfo(r.Context(), a, snap)
	self := h.viewerOrigin(a)
	target := withoutParams(r.URL, paramVersion, paramInfo)
	frameSrc := f.RawOrigin + reservedPrefix + "enter?t=" + url.QueryEscape(f.Enter) + "&next=" + url.QueryEscape(target)
	data := viewerData{
		Info: panelFor(info, r.URL), FrameSrc: frameSrc, RawOrigin: f.RawOrigin,
		Stream: reservedPrefix + "location?s=" + url.QueryEscape(f.Stream),
		Latest: withoutParams(r.URL, paramVersion),
	}
	if snap.Version != a.CurrentVersion {
		data.OldVersion = snap.Version
	}
	navigable := "https:"
	if h.cfg.ContentURL.Scheme == "http" {
		navigable = "http: https:"
	}
	hd := w.Header()
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Cache-Control", "private, no-store") // the page carries grants
	// frame-src also governs where the frame may navigate, so it allows the
	// web at large; the viewer itself loads nothing but its own two files.
	csp := fmt.Sprintf("default-src 'none'; script-src %[1]s/__redrop/viewer.js; style-src %[1]s/__redrop/viewer.css; "+
		"img-src 'self'; connect-src 'self'; frame-src %[2]s %[3]s; form-action 'none'; base-uri 'none'; frame-ancestors 'none'", self, f.RawOrigin, navigable)
	h.pages.render(w, http.StatusOK, "viewer.html", a.Title, serverIcons, data, csp)
}

// location streams the address-bar location of one viewer frame as
// server-sent events (DESIGN.md "Address bar"). The stream token, minted with
// the frame, scopes it to that frame of this artifact.
func (h *Handler) location(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	frame, exp, ok := h.sharing.StreamFrame(a, r.URL.Query().Get("s"))
	if !ok || r.Method != http.MethodGet {
		h.miss(w, r)
		return
	}
	sub, ok := h.relay.subscribe(frame, h.missKey(r))
	if !ok {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many open viewers", http.StatusTooManyRequests)
		return
	}
	defer h.relay.unsubscribe(sub)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	idle := time.NewTimer(relayIdle)
	defer idle.Stop()
	end := time.NewTimer(time.Until(exp))
	defer end.Stop()
	beat := time.NewTicker(relayHeartbeat)
	defer beat.Stop()
	for {
		var msg string
		select {
		case p := <-sub.ch:
			msg = "data: " + p + "\n\n"
			idle.Reset(relayIdle)
		case <-beat.C:
			msg = ": keep-alive\n\n"
		case <-idle.C:
			return
		case <-end.C:
			return
		case <-h.relay.done:
			return
		case <-r.Context().Done():
			return
		}
		if _, err := w.Write([]byte(msg)); err != nil || rc.Flush() != nil {
			return
		}
	}
}

const (
	relayIdle      = 5 * time.Minute // a stream without a navigation closes; EventSource reconnects
	relayHeartbeat = 25 * time.Second
)

type viewerData struct {
	Info       panelData
	FrameSrc   string
	RawOrigin  string
	Stream     string
	OldVersion int    // the older version shown, 0 for the current one
	Latest     string // the shared link without the version parameter
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
			q.Set(paramVersion, fmt.Sprint(rev.Number))
			row.Href = u.EscapedPath() + "?" + q.Encode()
		}
		p.Revisions = append(p.Revisions, row)
	}
	return p
}
