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
}

func New(sh *sharing.Service, cfg *config.Config, clients *netutil.ClientResolver, log *slog.Logger, installers http.Handler) *Handler {
	return &Handler{
		sharing: sh, cfg: cfg, clients: clients, log: log, pages: newPages(cfg), install: installers,
		misses: security.NewTokenBucket(cfg.ContentMissBurst, cfg.ContentMissPerMinute, maxMissClients),
	}
}

// slug extracts the artifact slug from the Host header ("" for the apex,
// ok=false for hosts that are not a single-label subdomain carrying a
// well-formed slug).
func (h *Handler) slug(r *http.Request) (string, bool) {
	label, ok := h.label(r)
	if !ok || (label != "" && !ids.ValidSlug(label)) {
		return "", false
	}
	return label, true
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
	switch {
	case !singleSlash(r.URL.Path):
		// "//host/x" and "/\host/x" would become protocol-relative URLs
		// wherever a path is echoed (redirects, the frame, panel links).
		// They are never artifact paths: answer before gates and routing.
		h.miss(w, r)
		return
	case isServiceWorkerScript(r):
		// A service worker would outlive revocation and could answer the
		// viewer page's own navigations, so no content host serves one,
		// not even redrop's own scripts.
		h.pages.notFound(w)
		return
	case h.serveServerIcon(w, r) || serveViewerAsset(w, r):
		return
	}
	slug, ok := h.slug(r)
	switch {
	case !ok && fallbackIcons[r.URL.Path] && (r.Method == http.MethodGet || r.Method == http.MethodHead) && h.singleLabel(r):
		h.favicon(w, r, "")
		return
	case !ok:
		h.miss(w, r)
		return
	case slug == "":
		h.serveApex(w, r)
		return
	case r.URL.Path == "/robots.txt":
		RobotsTxt(w, r)
		return
	case fallbackIcons[r.URL.Path] && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.favicon(w, r, slug)
		return
	case h.misses.Exhausted(h.missKey(r)):
		h.tooManyMisses(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && r.URL.Path == reservedPrefix+"open") {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a, err := h.sharing.Lookup(r.Context(), slug)
	if err != nil {
		h.miss(w, r)
		return
	}
	if rest, reserved := strings.CutPrefix(r.URL.Path, reservedPrefix); reserved {
		switch {
		case rest == "preview":
			h.preview(w, r, a)
		case rest == "open":
			h.open(w, r, a)
		case strings.HasPrefix(rest, versionPrefix):
			h.serveOldVersion(w, r, a, strings.TrimPrefix(rest, versionPrefix))
		default:
			h.pages.notFound(w)
		}
		return
	}
	h.serve(w, r, a)
}

// singleSlash reports whether p starts with exactly one slash, not followed
// by another slash or a backslash (which browsers treat as a slash).
func singleSlash(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p[1:], "/") && !strings.HasPrefix(p[1:], `\`)
}

// selfRef returns ref, a path with an optional query, as a reference to this
// origin: exactly one leading slash, so it can never be read as
// protocol-relative ("//host") or with a backslash ("/\host"). Every URL the
// content handler emits for its own origin (redirects, the viewer's frame,
// panel and file page links) goes through it.
func selfRef(ref string) string {
	return "/" + strings.TrimLeft(ref, `/\`)
}

// redirect sends the visitor to ref on this origin.
func redirect(w http.ResponseWriter, r *http.Request, ref string, code int) {
	http.Redirect(w, r, selfRef(ref), code)
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

// topLevelLoad reports a top-level page load, the only request that counts
// a view: Sec-Fetch-Dest: document, or, without Fetch Metadata, a request
// that looks like a page load. Frames (the viewer's own included) never count.
func topLevelLoad(r *http.Request) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return isDocument(r)
}

// wantsViewer reports a request that may get the viewer page: a top-level
// navigation by a browser that says so. Frames, subresources and clients
// without Fetch Metadata get the artifact itself.
func wantsViewer(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Dest") == "document"
}

// Query parameters and paths the artifact link understands.
const (
	paramInfo     = "redrop-info"    // "0": the direct, frameless view; "1": back to the viewer
	paramVersion  = "redrop-version" // N: an older version, when the owner allows it
	versionPrefix = "v/"             // /__redrop/v/<N>/<path>: version N's files
)

// versionBase is where version n's files are served.
func versionBase(n int) string { return reservedPrefix + versionPrefix + strconv.Itoa(n) }

// admit runs the gates for a request on a. It returns false when it has
// already answered the request (a miss or a gate).
func (h *Handler) admit(w http.ResponseWriter, r *http.Request, a *store.Artifact) (sharing.Decision, bool) {
	grants := sharing.Grants{Visit: h.cookieValue(r, cookieVisit), Preview: h.cookieValue(r, cookiePreview)}
	d := h.sharing.Decide(a, grants, time.Now())
	switch d {
	case sharing.ShowGone:
		h.miss(w, r)
		return d, false
	case sharing.ShowGate:
		if !isDocument(r) {
			http.Error(w, "access required", http.StatusForbidden)
			return d, false
		}
		h.sharing.RecordGateImpression(r.Context(), a, h.visitor(r))
		h.pages.gate(w, http.StatusOK, a, "", r.URL.RequestURI())
		return d, false
	}
	return d, true
}

// countView records a view of an ungated link for a top-level page load,
// once per browser per window ("seen"). Gated links count at Open.
func (h *Handler) countView(w http.ResponseWriter, r *http.Request, a *store.Artifact, d sharing.Decision) {
	if d == sharing.Serve && !sharing.Gated(a) && r.Method == http.MethodGet && topLevelLoad(r) {
		if g := h.sharing.RecordView(r.Context(), a, h.visitor(r), h.cookieValue(r, cookieSeen)); g != nil {
			h.setCookie(w, cookieSeen, g, false)
		}
	}
}

// serve answers a path of the artifact link: the viewer page, the file page,
// or the current version's files.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	d, ok := h.admit(w, r, a)
	if !ok {
		return
	}
	direct := h.directView(w, r, a)
	snap := sharing.CurrentSnapshot(a)
	if v := r.URL.Query().Get(paramVersion); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && v == strconv.Itoa(n) {
			snap, err = h.sharing.SelectVersion(r.Context(), a, n)
		} else {
			err = sharing.ErrVersionUnavailable
		}
		if err != nil {
			// Closed, unknown or malformed: the same miss as the version path.
			h.miss(w, r)
			return
		}
	}
	old := snap.Version != a.CurrentVersion
	switch {
	case h.getsViewer(w, r, a, snap, direct):
		h.viewer(w, r, a, snap, d)
	case old && h.sharing.Mode(a, snap) == sharing.ModeFilePage && r.URL.Path == "/":
		h.countView(w, r, a, d)
		h.serveContent(w, r, a, snap, versionBase(snap.Version), "/", direct)
	case old:
		// Version N's files live under its own path space.
		redirect(w, r, versionBase(snap.Version)+withoutParams(r.URL, paramVersion), http.StatusSeeOther)
	default:
		h.countView(w, r, a, d)
		h.serveContent(w, r, a, snap, "", r.URL.Path, direct)
	}
}

// serveOldVersion answers /__redrop/v/<N>/<path>: version N's files, only
// when the owner lets viewers open older versions; otherwise the identical
// miss. A top-level page load of a framed artifact goes to the viewer form,
// <path>?redrop-version=N.
func (h *Handler) serveOldVersion(w http.ResponseWriter, r *http.Request, a *store.Artifact, rest string) {
	num, sub, _ := strings.Cut(rest, "/")
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 || num != strconv.Itoa(n) {
		h.miss(w, r)
		return
	}
	d, ok := h.admit(w, r, a)
	if !ok {
		return
	}
	snap, err := h.sharing.OldVersion(r.Context(), a, n)
	if err != nil {
		h.miss(w, r)
		return
	}
	if !strings.HasPrefix(rest, num+"/") {
		redirect(w, r, withQuery(versionBase(n)+"/", r.URL.RawQuery), http.StatusMovedPermanently)
		return
	}
	p, direct := "/"+sub, h.directView(w, r, a)
	if h.getsViewer(w, r, a, snap, direct) {
		// The viewer form of this page: <path>?redrop-version=N.
		q := r.URL.Query()
		q.Set(paramVersion, strconv.Itoa(n))
		redirect(w, r, withQuery((&url.URL{Path: p}).EscapedPath(), q.Encode()), http.StatusSeeOther)
		return
	}
	h.countView(w, r, a, d)
	h.serveContent(w, r, a, snap, versionBase(n), p, direct)
}

// directView reports whether this request gets the direct, frameless view:
// ?redrop-info=0 chooses it for the visit (a signed host-only cookie, so the
// pages that view loads stay direct), ?redrop-info=1 goes back.
//
// The choice is only recorded (or cleared) by a top-level navigation the
// visitor started: typed, bookmarked, or followed from the same origin. A
// same-site subresource or page (another artifact on the content domain) can
// still get a direct response for itself, but can't switch this link's
// viewer off for the visitor's next visits.
func (h *Handler) directView(w http.ResponseWriter, r *http.Request, a *store.Artifact) bool {
	choose := r.Method == http.MethodGet && visitorNavigation(r)
	switch r.URL.Query().Get(paramInfo) {
	case "0":
		if choose {
			h.setCookie(w, cookieDirect, h.sharing.DirectChoice(a), false)
		}
		return true
	case "1":
		if choose {
			h.clearCookie(w, cookieDirect)
		}
		return false
	}
	return h.sharing.Direct(a, h.cookieValue(r, cookieDirect))
}

// getsViewer reports whether this request for snap gets the viewer page: a
// framed artifact, a top-level page load, and no direct-view choice. It is
// also the one place that marks framed responses as varying with
// Sec-Fetch-Dest, since the same URL is content for frames.
func (h *Handler) getsViewer(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, direct bool) bool {
	if h.sharing.Mode(a, snap) != sharing.ModeFramed {
		return false
	}
	w.Header().Add("Vary", "Sec-Fetch-Dest")
	return wantsViewer(r) && !direct
}

// visitorNavigation reports a top-level page load the visitor started: from
// the address bar or a bookmark (Sec-Fetch-Site: none) or from a page of the
// same origin. Without Fetch Metadata it is never assumed.
func visitorNavigation(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return r.Header.Get("Sec-Fetch-Dest") == "document" && (site == "none" || site == "same-origin")
}

// withoutParams returns u's path and query without the named parameters.
func withoutParams(u *url.URL, names ...string) string {
	q := u.Query()
	for _, n := range names {
		q.Del(n)
	}
	return selfRef(withQuery(u.EscapedPath(), q.Encode()))
}

func withQuery(p, rawQuery string) string {
	if rawQuery == "" {
		return p
	}
	return p + "?" + rawQuery
}

// serveContent answers path p from snap's files: the file itself, the file
// page, a directory listing or redirect, or not found. base is where snap's
// files are served ("" for the current version); direct leaves the info
// panel off the file page.
func (h *Handler) serveContent(w http.ResponseWriter, r *http.Request, a *store.Artifact, snap sharing.Snapshot, base, p string, direct bool) {
	t := h.sharing.ResolvePath(r.Context(), snap, p)
	switch t.Kind {
	case sharing.TargetFile:
		h.sendFile(w, r, t.File, r.URL.Query().Has("download"))
	case sharing.TargetViewer:
		var info *sharing.ViewerInfo
		if !direct && h.sharing.InfoShown(a) {
			v := h.sharing.ViewerInfo(r.Context(), a, snap)
			info = &v
		}
		h.pages.fileViewer(w, h.sharing, t.File, h.selfOrigin(a), base, info)
	case sharing.TargetListing:
		h.pages.listing(w, t.Dir, h.sharing.ListDir(r.Context(), snap, t.Dir))
	case sharing.TargetRedirect:
		redirect(w, r, base+t.Location, http.StatusMovedPermanently)
	default:
		h.pages.notFound(w)
	}
}

// selfOrigin is the artifact's own origin, scheme://<slug>.<content domain>[:port].
func (h *Handler) selfOrigin(a *store.Artifact) string {
	return strings.TrimSuffix(h.cfg.ArtifactURL(a.Slug), "/")
}

func (h *Handler) sendFile(w http.ResponseWriter, r *http.Request, f *store.File, download bool) {
	fh, err := h.sharing.OpenBlob(f)
	if err != nil {
		h.pages.message(w, http.StatusInternalServerError, "Content unavailable", "The stored content could not be read.")
		return
	}
	defer fh.Close()
	hd := w.Header()
	hd.Set("Content-Type", f.ContentType)
	hd.Set("ETag", `"`+f.Blob+`"`)
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	hd.Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(path.Base(f.Path)))
	http.ServeContent(w, r, "", time.Time{}, fh)
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	g, ok := h.sharing.RedeemPreview(a, r.URL.Query().Get("t"))
	if !ok {
		h.pages.message(w, http.StatusForbidden, "Preview link expired", "Open the preview again from the dashboard.")
		return
	}
	h.setCookie(w, cookiePreview, g, true)
	redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	if r.Method != http.MethodPost {
		redirect(w, r, "/", http.StatusSeeOther)
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
	redirect(w, r, next, http.StatusSeeOther)
}

// reservedPrefix is redrop's own path space on every content host: gates,
// previews, viewer assets, icons, and older versions' files. Artifacts can't
// upload under it.
const reservedPrefix = "/" + sharing.ReservedPrefix + "/"

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
	name, reserved := strings.CutPrefix(r.URL.Path, reservedPrefix)
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
func (h *Handler) favicon(w http.ResponseWriter, r *http.Request, slug string) {
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
