// Package content serves artifacts on their own subdomains of the content
// domain (https://<slug>.<content-domain>/) plus the content apex. Access
// rules live in the sharing service; this package only speaks HTTP.
//
// Every miss on an artifact host (malformed, unknown, deleted, expired,
// exhausted, or revoked slug) gets the same 404 page and spends one token of
// the client's miss budget. A client that has spent its budget gets 429 for
// every artifact-host request until it refills; hits never spend budget.
// Fallback icon requests (/favicon.ico, /apple-touch-icon.png) are exempt:
// they are free, so they answer every slug identically unless the visitor
// has proven access to the artifact (see favicon).
package content

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/brand"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/netutil"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

const maxInlineText = 1 << 20

// maxMissClients bounds the miss limiter's memory (roughly 100 bytes each).
const maxMissClients = 100_000

// Handler serves the content domain.
type Handler struct {
	sharing *sharing.Service
	cfg     *config.Config
	clients *netutil.ClientResolver
	log     *slog.Logger
	pages   *pages
	install http.Handler // installers and CLI downloads, also served on the apex
	misses  *security.TokenBucket
	relay   *relay // address-bar locations of viewer frames
}

func New(sh *sharing.Service, cfg *config.Config, clients *netutil.ClientResolver, log *slog.Logger, installers http.Handler) *Handler {
	return &Handler{
		sharing: sh, cfg: cfg, clients: clients, log: log, pages: newPages(cfg), install: installers,
		misses: security.NewTokenBucket(cfg.ContentMissBurst, cfg.ContentMissPerMinute, maxMissClients),
		relay:  newRelay(),
	}
}

// Close ends open address-bar streams; call it when the server shuts down.
func (h *Handler) Close() { h.relay.close() }

// host parses the artifact host of the request (ok=false for the apex, other
// hosts, and labels that are not exactly an ids.Host).
func (h *Handler) host(r *http.Request) (ids.Host, bool) {
	label, ok := h.label(r)
	if !ok || label == "" {
		return ids.Host{}, false
	}
	return ids.ParseHostLabel(label)
}

// label extracts the subdomain label from the Host header without checking
// its slug syntax ("" for the apex, ok=false for other hosts).
func (h *Handler) label(r *http.Request) (string, bool) {
	host := strings.ToLower(r.Host)
	if hp, _, err := net.SplitHostPort(host); err == nil {
		host = hp
	}
	host = strings.TrimSuffix(host, ".")
	base := h.cfg.ContentHost()
	if host == base {
		return "", true
	}
	label, ok := strings.CutSuffix(host, "."+base)
	if !ok || label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// singleLabel reports whether the host is one label under the content domain
// (an artifact host, whether or not the label is a well-formed slug).
func (h *Handler) singleLabel(r *http.Request) bool {
	label, ok := h.label(r)
	return ok && label != ""
}

// missKey is the client a miss is charged to: the trusted-proxy-resolved
// address, widened to its /64 for IPv6 because one subscriber usually holds
// a whole /64.
func (h *Handler) missKey(r *http.Request) string {
	addr := h.clients.IP(r)
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

// miss answers a request for a slug that does not resolve to an available
// artifact. All misses look the same, whatever the reason.
func (h *Handler) miss(w http.ResponseWriter, r *http.Request) {
	if !h.misses.Take(h.missKey(r)) {
		h.tooManyMisses(w)
		return
	}
	h.pages.notFound(w)
}

func (h *Handler) tooManyMisses(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa((60+h.cfg.ContentMissPerMinute-1)/h.cfg.ContentMissPerMinute))
	h.pages.message(w, http.StatusTooManyRequests, "Too many requests", "Too many links from your network did not work. Wait a minute and try again.")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.setHeaders(w)
	if h.serveServerIcon(w, r) || serveViewerAsset(w, r) {
		return
	}
	label, isContent := h.label(r)
	host, ok := h.host(r)
	switch {
	case isContent && label == "":
		h.serveApex(w, r)
		return
	case !ok && fallbackIcons[r.URL.Path] && (r.Method == http.MethodGet || r.Method == http.MethodHead) && h.singleLabel(r):
		h.favicon(w, r, ids.Host{})
		return
	case !ok:
		h.miss(w, r)
		return
	case r.URL.Path == "/robots.txt":
		RobotsTxt(w, r)
		return
	case isServiceWorkerScript(r):
		// Service workers would outlive revocation and could answer the
		// viewer's own navigations, so no artifact host ever serves one.
		h.pages.notFound(w)
		return
	case fallbackIcons[r.URL.Path] && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.favicon(w, r, host)
		return
	case h.misses.Exhausted(h.missKey(r)):
		h.tooManyMisses(w)
		return
	case host.IsRaw():
		h.serveRaw(w, r, host)
		return
	}
	reserved := "/" + sharing.ReservedPrefix + "/"
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && r.URL.Path == reserved+"open") {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a, err := h.sharing.Lookup(r.Context(), host.Slug())
	if err != nil {
		h.miss(w, r)
		return
	}
	if rest, isReserved := strings.CutPrefix(r.URL.Path, reserved); isReserved {
		switch rest {
		case "preview":
			h.preview(w, r, a)
		case "open":
			h.open(w, r, a)
		case "location":
			h.location(w, r, a)
		default:
			h.pages.notFound(w)
		}
		return
	}
	h.serve(w, r, a)
}

// isServiceWorkerScript reports a request for a service worker's script
// (registration or update check).
func isServiceWorkerScript(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Dest") == "serviceworker" || r.Header.Get("Service-Worker") == "script"
}

func (h *Handler) setHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("X-Robots-Tag", "noindex, nofollow, noarchive, nosnippet, noimageindex")
	hd.Set("Referrer-Policy", "no-referrer")
	// Never cache at the CDN: revocation, expiry and view limits must be immediate.
	hd.Set("Cache-Control", "private, no-cache")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "frame-ancestors 'self' "+h.cfg.PublicOrigin())
	hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	hd.Set("Cross-Origin-Resource-Policy", "same-site")
	if h.cfg.ContentURL.Scheme == "https" {
		hd.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (h *Handler) visitor(r *http.Request) sharing.Visitor {
	return sharing.Visitor{IP: h.clients.IP(r), Country: h.clients.Country(r), UserAgent: r.UserAgent()}
}

// isDocument reports whether the request is a page navigation.
func isDocument(r *http.Request) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document" || d == "iframe"
	}
	accept := r.Header.Get("Accept")
	return accept == "" || accept == "*/*" || strings.Contains(accept, "text/html")
}

// Query parameters the shared link understands. They are not passed to the
// artifact's frame.
const (
	paramInfo    = "redrop-info"    // "0": a direct, frameless view (screenshots)
	paramVersion = "redrop-version" // N: an older version, when the owner allows it
)

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	grants := sharing.Grants{Visit: h.cookieValue(r, cookieVisit), Preview: h.cookieValue(r, cookiePreview)}
	switch h.sharing.Decide(a, grants, time.Now()) {
	case sharing.ShowGone:
		h.miss(w, r)
		return
	case sharing.ShowGate:
		if !isDocument(r) {
			http.Error(w, "access required", http.StatusForbidden)
			return
		}
		h.sharing.RecordGateImpression(r.Context(), a, h.visitor(r))
		h.pages.gate(w, http.StatusOK, a, "", r.URL.RequestURI())
		return
	case sharing.Serve:
		if !sharing.Gated(a) && r.Method == http.MethodGet && isDocument(r) {
			if g := h.sharing.RecordView(r.Context(), a, h.visitor(r), h.cookieValue(r, cookieSeen)); g != nil {
				h.setCookie(w, cookieSeen, g, false)
			}
		}
	case sharing.ServeOwnerPreview:
	}
	q := r.URL.Query()
	version := 0
	if v := q.Get(paramVersion); v != "" {
		var err error
		if version, err = strconv.Atoi(v); err != nil || version <= 0 {
			version = -1 // unavailable: redirect to the current version
		}
	}
	snap, err := h.sharing.SelectVersion(r.Context(), a, version)
	if err != nil {
		// Not a miss: the visitor passed the gates. Show the current version.
		http.Redirect(w, r, withoutParams(r.URL, paramVersion), http.StatusSeeOther)
		return
	}
	mode := h.sharing.Mode(a, snap)
	direct := h.directView(w, r, a)
	if mode == sharing.ModeFramed && !direct && pageNavigation(r) {
		// Directory redirects and missing paths answer as they would without
		// the frame; only real pages get the viewer.
		switch t := h.sharing.ResolvePath(r.Context(), snap, r.URL.Path); t.Kind {
		case sharing.TargetRedirect:
			http.Redirect(w, r, t.Location, http.StatusMovedPermanently)
		case sharing.TargetNotFound:
			h.pages.notFound(w)
		default:
			h.viewerPage(w, r, a, snap, grants)
		}
		return
	}
	var info *sharing.ViewerInfo
	if mode == sharing.ModeFilePage && !direct && h.sharing.InfoShown(a) {
		v := h.sharing.ViewerInfo(r.Context(), a, snap)
		info = &v
	}
	h.serveContent(w, r, a, snap, info)
}

// directView reports whether this request gets the direct, frameless view:
// ?redrop-info=0 chooses it for the visit (a signed host-only cookie, so the
// pages and frames that view loads stay direct), ?redrop-info=1 goes back.
func (h *Handler) directView(w http.ResponseWriter, r *http.Request, a *store.Artifact) bool {
	switch r.URL.Query().Get(paramInfo) {
	case "0":
		if r.Method == http.MethodGet {
			h.setCookie(w, cookieDirect, h.sharing.DirectChoice(a), false)
		}
		return true
	case "1":
		http.SetCookie(w, &http.Cookie{Name: h.cookieName(cookieDirect), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: !h.cfg.Insecure, SameSite: http.SameSiteLaxMode})
		return false
	}
	return h.sharing.Direct(a, h.cookieValue(r, cookieDirect))
}

// pageNavigation reports a top-level page load: the only request that gets
// the viewer page. A viewer page in a frame could never render (its own
// frame-ancestors is 'none'), so frames get content. Without Fetch Metadata
// the Accept check decides, as for view counting.
func pageNavigation(r *http.Request) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return isDocument(r)
}

// withoutParams returns u's path and query without the named parameters.
func withoutParams(u *url.URL, names ...string) string {
	q := u.Query()
	for _, n := range names {
		q.Del(n)
	}
	if len(q) == 0 {
		return u.EscapedPath()
	}
	return u.EscapedPath() + "?" + q.Encode()
}

// serveContent answers from snap's files on the shared link: the direct view,
// subresources of a direct view, and the file page. info is the file page's
// panel, nil without one.
func (h *Handler) serveContent(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, info *sharing.ViewerInfo) {
	t := h.sharing.ResolvePath(r.Context(), snap, r.URL.Path)
	switch {
	case t.Kind == sharing.TargetViewer:
		h.pages.fileViewer(w, h.sharing, t.File, h.viewerOrigin(a), snap, info, r.URL)
		return
	case t.Kind != sharing.TargetFile:
	case h.sharing.Mode(a, snap) == sharing.ModeFilePage && scriptable(t.File.ContentType):
		// The file page carries trusted info on this origin; a shared SVG
		// or HTML file opened here must not be able to script it.
		w.Header().Set("Content-Security-Policy", "sandbox")
	case r.Header.Get("Sec-Fetch-Dest") == "object" || r.Header.Get("Sec-Fetch-Dest") == "embed":
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
	h.sendTarget(w, r, snap, t)
}

// sendTarget answers a resolved path from snap's files: the file itself, a
// directory listing, a directory redirect, or not found.
func (h *Handler) sendTarget(w http.ResponseWriter, r *http.Request, snap sharing.Snapshot, t sharing.Target) {
	switch t.Kind {
	case sharing.TargetFile:
		h.sendFile(w, r, t.File, r.URL.Query().Has("download"))
	case sharing.TargetListing:
		h.pages.listing(w, t.Dir, h.sharing.ListDir(r.Context(), snap, t.Dir))
	case sharing.TargetRedirect:
		http.Redirect(w, r, t.Location, http.StatusMovedPermanently)
	default:
		h.pages.notFound(w)
	}
}

// scriptable reports content types a browser runs scripts in when opened.
func scriptable(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return ct == "text/html" || ct == "image/svg+xml" || ct == "application/xhtml+xml" || ct == "application/xml" || ct == "text/xml"
}

func (h *Handler) viewerOrigin(a *store.Artifact) string {
	return h.cfg.ContentOrigin(ids.ViewerHost(a.Slug))
}

func (h *Handler) sendFile(w http.ResponseWriter, r *http.Request, f *store.File, download bool) {
	fh, err := h.sharing.OpenBlob(f)
	if err != nil {
		h.pages.message(w, http.StatusInternalServerError, "Content unavailable", "The stored content could not be read.")
		return
	}
	defer fh.Close()
	fileHeaders(w, f, `"`+f.Blob+`"`, download)
	http.ServeContent(w, r, "", time.Time{}, fh)
}

func fileHeaders(w http.ResponseWriter, f *store.File, etag string, download bool) {
	hd := w.Header()
	hd.Set("Content-Type", f.ContentType)
	hd.Set("ETag", etag)
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	hd.Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(path.Base(f.Path)))
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	g, ok := h.sharing.RedeemPreview(a, r.URL.Query().Get("t"))
	if !ok {
		h.pages.message(w, http.StatusForbidden, "Preview link expired", "Open the preview again from the dashboard.")
		return
	}
	h.setCookie(w, cookiePreview, g, true)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostForm.Get("next"))
	g, err := h.sharing.Open(r.Context(), a, h.visitor(r), r.PostForm.Get("password"))
	if err != nil {
		switch e := apperr.As(err); e.Kind {
		case apperr.KindGone:
			h.miss(w, r)
		case apperr.KindUnauthorized:
			h.pages.gate(w, http.StatusUnauthorized, a, e.Msg, next)
		case apperr.KindRateLimited:
			h.pages.gate(w, http.StatusTooManyRequests, a, e.Msg, next)
		default:
			h.log.Error("open artifact", "error", err)
			h.pages.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again.")
		}
		return
	}
	h.setCookie(w, cookieVisit, g, false)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// iconPrefix is where server-rendered pages on the content domain link their
// icons. It is reserved, so it can never collide with artifact files.
const iconPrefix = "/" + sharing.ReservedPrefix + "/"

// fallbackIcons are the paths browsers request without being told to. On an
// artifact origin they fall back to the redrop icon when the artifact has none.
var fallbackIcons = map[string]bool{"/favicon.ico": true, "/apple-touch-icon.png": true}

// cacheFallbackIcon keeps the default icon out of shared caches on artifact
// origins: a CDN must not pin it over an artifact's own icon once a visitor
// unlocks the artifact or its owner uploads one.
const cacheFallbackIcon = "private, max-age=86400"

// serveServerIcon answers icon requests that never depend on an artifact:
// /__redrop/<icon> on any content host (linked by server-rendered pages) and
// the root icons on the apex.
func (h *Handler) serveServerIcon(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	name, reserved := strings.CutPrefix(r.URL.Path, iconPrefix)
	if !reserved {
		if label, ok := h.label(r); !ok || label != "" {
			return false
		}
		name = strings.TrimPrefix(r.URL.Path, "/")
	}
	return brand.Has(name) && brand.Serve(w, r, name, brand.CacheShared)
}

// favicon serves an artifact's own icon to a visitor who has proven access to
// it, and the redrop icon otherwise. Proof is a valid visit or preview grant,
// or, for ungated artifacts, the "seen" cookie set by viewing a page; the
// browser fetches the icon after the page, so real visitors still see the
// artifact's icon. These requests spend no miss budget, so without proof
// every slug (unknown, malformed, expired, revoked, exhausted, locked, or
// live) gets the identical default response and no lookup is made: free
// icon requests cannot tell hits from misses. Icon requests never record or
// consume a view.
func (h *Handler) favicon(w http.ResponseWriter, r *http.Request, host ids.Host) {
	if host.IsRaw() {
		if acc, ok := h.rawAccess(r, host); ok {
			if t := h.sharing.ResolvePath(r.Context(), acc.Snapshot, r.URL.Path); t.Kind == sharing.TargetFile {
				h.sendFile(w, r, t.File, false)
				return
			}
		}
		brand.Serve(w, r, strings.TrimPrefix(r.URL.Path, "/"), cacheFallbackIcon)
		return
	}
	slug := host.Slug()
	grants := sharing.Grants{Visit: h.cookieValue(r, cookieVisit), Preview: h.cookieValue(r, cookiePreview)}
	seen := h.cookieValue(r, cookieSeen)
	if slug != "" && (grants.Visit != "" || grants.Preview != "" || seen != "") {
		if a, err := h.sharing.Lookup(r.Context(), slug); err == nil {
			d := h.sharing.Decide(a, grants, time.Now())
			proven := d == sharing.ServeOwnerPreview || (d == sharing.Serve && (sharing.Gated(a) || h.sharing.Seen(a, seen)))
			if proven {
				if t := h.sharing.ResolvePath(r.Context(), sharing.CurrentSnapshot(a), r.URL.Path); t.Kind == sharing.TargetFile {
					h.sendFile(w, r, t.File, false)
					return
				}
			}
		}
	}
	brand.Serve(w, r, strings.TrimPrefix(r.URL.Path, "/"), cacheFallbackIcon)
}

// safeNext only allows same-host relative paths outside the reserved space.
func safeNext(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.HasPrefix(p, "/"+sharing.ReservedPrefix) {
		return "/"
	}
	return p
}

// serveApex answers the bare content domain: landing page, robots, installers.
func (h *Handler) serveApex(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		h.pages.apex(w)
	case "/robots.txt":
		RobotsTxt(w, r)
	default:
		h.install.ServeHTTP(w, r)
	}
}

// RobotsTxt disallows all crawling.
func RobotsTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}
