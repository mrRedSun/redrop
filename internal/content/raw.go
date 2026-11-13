package content

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/sharing"
)

// serveRaw answers a raw host (<slug>--raw or <slug>--v<N>): the artifact's
// stored bytes, unchanged, for the viewer frame (DESIGN.md "Viewer frame").
// It is reached only after robots, fallback icons and the miss budget.
func (h *Handler) serveRaw(w http.ResponseWriter, r *http.Request, host ids.Host) {
	viewer := h.cfg.ContentOrigin(host.Viewer())
	// Only its own viewer (and the dashboard preview) may frame a raw page;
	// 'self' lets an artifact frame its own pages.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'self' "+viewer+" "+h.cfg.PublicOrigin())

	// A raw page is never a top-level page: send the visitor to the shared
	// link. Decided from the host alone, before any lookup, so it says
	// nothing about whether the artifact exists.
	if r.Header.Get("Sec-Fetch-Dest") == "document" {
		target := r.URL.EscapedPath()
		if strings.HasPrefix(target, "/"+sharing.ReservedPrefix+"/") {
			target = "/"
		} else if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		if v := host.Version(); v != 0 {
			u, _ := url.Parse(target)
			q := u.Query()
			q.Set(paramVersion, strconv.Itoa(v))
			u.RawQuery = q.Encode()
			target = u.String()
		}
		http.Redirect(w, r, viewer+target, http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.miss(w, r)
		return
	}
	a, err := h.sharing.Lookup(r.Context(), host.Slug())
	if err != nil {
		h.miss(w, r)
		return
	}
	switch r.URL.Path {
	case "/" + sharing.ReservedPrefix + "/enter":
		g, ok := h.sharing.RedeemEnter(a, host, r.URL.Query().Get("t"))
		if !ok {
			h.miss(w, r)
			return
		}
		h.setRawCookie(w, g)
		w.Header().Set("Cache-Control", "private, no-store")
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	case "/" + sharing.ReservedPrefix + "/preview":
		g, ok := h.sharing.RedeemRawPreview(a, host, r.URL.Query().Get("t"))
		if !ok {
			h.miss(w, r)
			return
		}
		h.setRawCookie(w, g)
		w.Header().Set("Cache-Control", "private, no-store")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/"+sharing.ReservedPrefix+"/") {
		h.miss(w, r)
		return
	}
	acc, ok := h.sharing.AuthorizeRaw(r.Context(), a, host, h.cookieValue(r, cookieRaw), time.Now())
	if !ok {
		h.miss(w, r)
		return
	}
	if acc.Frame != "" && r.Method == http.MethodGet && frameNavigation(r) {
		h.relay.publish(acc.Frame, r.URL.RequestURI())
	}
	t := h.sharing.ResolvePath(r.Context(), acc.Snapshot, r.URL.Path)
	switch t.Kind {
	case sharing.TargetFile:
		h.sendFile(w, r, t.File, r.URL.Query().Has("download"))
	case sharing.TargetListing:
		h.pages.listing(w, t.Dir, h.sharing.ListDir(r.Context(), acc.Snapshot, t.Dir))
	case sharing.TargetRedirect:
		http.Redirect(w, r, t.Location, http.StatusMovedPermanently)
	default:
		h.pages.notFound(w) // the visitor is admitted; this path just doesn't exist
	}
}

// frameNavigation reports a navigation inside the viewer's frame: a new page
// the address bar should follow, as opposed to a subresource or fetch.
// Without Fetch Metadata only an explicit HTML Accept counts.
func frameNavigation(r *http.Request) bool {
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" {
		return m == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "iframe"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
