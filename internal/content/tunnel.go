package content

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/internal/tunnel"
)

// Tunnels (DESIGN.md "Tunnels") share the artifact host's lookup, gates,
// viewer and misses; this file is what happens after the access check: the
// same-site rule, the offline page, and handing the request to the proxy.

// WithTunnels connects the handler to the registry of live tunnel
// connections. Without it every tunnel is offline.
func (h *Handler) WithTunnels(r *tunnel.Registry) *Handler {
	h.tunnels = r
	return h
}

// serveTunnel answers a tunnel's path for a visitor who passed the access
// check: the viewer page for a top-level page load of a framed tunnel,
// otherwise the app's answer through the tunnel.
func (h *Handler) serveTunnel(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, d sharing.Decision, direct bool) {
	if sameSiteRefused(r) {
		h.pages.message(w, http.StatusForbidden, "Request refused", "This tunnel doesn't answer requests from other links on this domain.")
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && h.getsViewer(w, r, a, snap, direct) {
		h.viewer(w, r, a, snap, d)
		return
	}
	if reservedPath(r.URL.Path) {
		// Never the app's: a path that is reserved once cleaned.
		h.miss(w, r)
		return
	}
	h.countView(w, r, a, d)
	h.proxyTunnel(w, withoutLinkParams(r), a)
}

// proxyTunnel forwards r through a's tunnel, or answers with the offline
// page when its CLI isn't connected. The caller has checked access.
func (h *Handler) proxyTunnel(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	if h.tunnels == nil || !h.tunnels.Serve(w, r, h.tunnelVisit(r, a)) {
		tunnel.OfflinePage(w, h.pages.message)
	}
}

func (h *Handler) tunnelVisit(r *http.Request, a *store.Artifact) tunnel.Visit {
	return tunnel.Visit{
		ArtifactID: a.ID, AccessGen: a.AccessGen, ClientIP: h.clients.IP(r), Proto: h.cfg.ContentURL.Scheme,
		Reserved: h.reservedCookie, Strip: []string{h.cfg.ClientIPHeader, h.cfg.CountryHeader}, Page: h.pages.message,
	}
}

// reservedCookie reports one of redrop's own cookie names on content hosts.
func (h *Handler) reservedCookie(name string) bool {
	for _, base := range []string{cookieVisit, cookiePreview, cookieSeen, cookieDirect} {
		if name == h.cookieName(base) {
			return true
		}
	}
	return false
}

// sameSiteRefused reports a request from another origin on the content
// domain (another artifact or tunnel) that is not a GET or HEAD navigation.
// SameSite=Lax cookies, redrop's visit cookie and the app's own, ride along
// on such requests, so another user's page could act as the visitor.
func sameSiteRefused(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") != "same-site" {
		return false
	}
	navigation := r.Header.Get("Sec-Fetch-Mode") == "navigate" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
	return !navigation
}

// reservedPath reports a path in redrop's reserved space once cleaned.
func reservedPath(p string) bool {
	c := path.Clean("/" + p)
	return c == strings.TrimSuffix(reservedPrefix, "/") || strings.HasPrefix(c, reservedPrefix)
}

// withoutLinkParams drops the link's own parameters (redrop-info,
// redrop-version) from the query, keeping every other byte of it.
func withoutLinkParams(r *http.Request) *http.Request {
	if r.URL.RawQuery == "" {
		return r
	}
	parts := strings.Split(r.URL.RawQuery, "&")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		k, _, _ := strings.Cut(p, "=")
		if name, err := url.QueryUnescape(k); err == nil && (name == paramInfo || name == paramVersion) {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == len(parts) {
		return r
	}
	r2 := r.WithContext(r.Context())
	u := *r.URL
	u.RawQuery = strings.Join(kept, "&")
	r2.URL = &u
	return r2
}
