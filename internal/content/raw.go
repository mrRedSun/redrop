package content

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

// serveRaw answers a raw host (<slug>--raw or <slug>--v<N>): the artifact's
// stored bytes, unchanged, for the viewer frame (DESIGN.md "Viewer frame").
// It is reached only after robots, fallback icons and the miss budget.
func (h *Handler) serveRaw(w http.ResponseWriter, r *http.Request, host ids.Host) {
	viewer := h.cfg.ContentOrigin(host.Viewer())
	// Only its own viewer may frame a raw page ('self' lets an artifact frame
	// its own pages); the dashboard previews on the "--raw" host only.
	ancestors := "frame-ancestors 'self' " + viewer
	if host.Version() == 0 {
		ancestors += " " + h.cfg.PublicOrigin()
	}
	hd := w.Header()
	hd.Set("Content-Security-Policy", ancestors)
	// Raw bytes are for this artifact's own pages: other artifacts on the
	// same site must not include them (with this visitor's raw cookie) as
	// scripts or images. Frames are navigations and are not affected.
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")

	// A raw page is never a top-level page: send the visitor to the shared
	// link. Decided from the host alone, before any lookup, so it says
	// nothing about whether the artifact exists.
	if r.Header.Get("Sec-Fetch-Dest") == "document" {
		target := r.URL.EscapedPath()
		if strings.HasPrefix(target, reservedPrefix) {
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
	if g, next, err, isEntry := h.redeemRaw(r, a, host); isEntry {
		switch {
		case errors.Is(err, sharing.ErrEnterQuota):
			// A valid token over this client's share: refused, but not a
			// miss, so no miss budget is spent.
			w.Header().Set("Retry-After", "120")
			http.Error(w, "too many pages opened from your network; try again in a few minutes", http.StatusTooManyRequests)
			return
		case g == nil:
			h.miss(w, r)
			return
		}
		h.setRawCookie(w, g)
		w.Header().Set("Cache-Control", "private, no-store")
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(r.URL.Path, reservedPrefix) {
		h.miss(w, r)
		return
	}
	acc, ok := h.sharing.AuthorizeRaw(r.Context(), a, host, h.cookieValue(r, cookieRaw))
	if !ok {
		h.miss(w, r)
		return
	}
	if acc.Refresh != nil {
		h.setRawCookie(w, acc.Refresh)
	}
	if acc.Frame != "" && r.Method == http.MethodGet && frameNavigation(r) {
		h.relay.publish(acc.Frame, r.URL.RequestURI())
	}
	h.sendTarget(w, r, acc.Snapshot, h.sharing.ResolvePath(r.Context(), acc.Snapshot, r.URL.Path))
}

// redeemRaw handles the raw host's two ways in: the viewer frame's enter
// token and the dashboard's preview token. isEntry is false for other paths;
// g is nil when the token is refused.
func (h *Handler) redeemRaw(r *http.Request, a *store.Artifact, host ids.Host) (g *sharing.Grant, next string, err error, isEntry bool) {
	t := r.URL.Query().Get("t")
	switch r.URL.Path {
	case reservedPrefix + "enter":
		g, err = h.sharing.RedeemEnter(a, host, t, h.missKey(r))
		return g, safeNext(r.URL.Query().Get("next")), err, true
	case reservedPrefix + "preview":
		g, _ = h.sharing.RedeemRawPreview(a, host, t)
		return g, "/", nil, true
	}
	return nil, "", nil, false
}

// rawAccess admits a raw-host request by its cookie, or reports false.
func (h *Handler) rawAccess(r *http.Request, host ids.Host) (sharing.RawAccess, bool) {
	cookie := h.cookieValue(r, cookieRaw)
	if cookie == "" {
		return sharing.RawAccess{}, false
	}
	a, err := h.sharing.Lookup(r.Context(), host.Slug())
	if err != nil {
		return sharing.RawAccess{}, false
	}
	return h.sharing.AuthorizeRaw(r.Context(), a, host, cookie)
}

// frameNavigation reports a navigation the visitor made inside a frame: a
// link they clicked or a form they sent (Sec-Fetch-User). Scripted and
// automatic loads, including the frame's first load and nested frames that
// load by themselves, are not reported; requests without Fetch Metadata never
// are.
func frameNavigation(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "iframe" && r.Header.Get("Sec-Fetch-User") == "?1"
}
