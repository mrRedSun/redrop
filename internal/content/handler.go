// Package content serves artifacts on their own subdomains of the content
// domain (https://<slug>.<content-domain>/) plus the content apex. Access
// rules live in the sharing service; this package only speaks HTTP.
package content

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/brand"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/netutil"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

const maxInlineText = 1 << 20

// Handler serves the content domain.
type Handler struct {
	sharing *sharing.Service
	cfg     *config.Config
	clients *netutil.ClientResolver
	log     *slog.Logger
	pages   *pages
	install http.Handler // installers and CLI downloads, also served on the apex
}

func New(sh *sharing.Service, cfg *config.Config, clients *netutil.ClientResolver, log *slog.Logger, installers http.Handler) *Handler {
	return &Handler{sharing: sh, cfg: cfg, clients: clients, log: log, pages: newPages(cfg), install: installers}
}

// slug extracts the artifact slug from the Host header ("" for the apex,
// ok=false for hosts that are not single-label subdomains).
func (h *Handler) slug(r *http.Request) (string, bool) {
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

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.setHeaders(w)
	if h.serveServerIcon(w, r) {
		return
	}
	slug, ok := h.slug(r)
	switch {
	case !ok:
		h.pages.notFound(w)
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
	}
	openPath := "/" + sharing.ReservedPrefix + "/open"
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && r.URL.Path == openPath) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a, err := h.sharing.Lookup(r.Context(), slug)
	if err != nil {
		h.pages.notFound(w)
		return
	}
	if rest, reserved := strings.CutPrefix(r.URL.Path, "/"+sharing.ReservedPrefix+"/"); reserved {
		switch rest {
		case "preview":
			h.preview(w, r, a)
		case "open":
			h.open(w, r, a)
		default:
			h.pages.notFound(w)
		}
		return
	}
	h.serve(w, r, a)
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

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	grants := sharing.Grants{Visit: h.cookieValue(r, cookieVisit), Preview: h.cookieValue(r, cookiePreview)}
	switch h.sharing.Decide(a, grants, time.Now()) {
	case sharing.ShowGone:
		h.pages.gone(w)
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
	h.serveContent(w, r, a)
}

func (h *Handler) serveContent(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	t := h.sharing.ResolvePath(r.Context(), a, r.URL.Path)
	switch t.Kind {
	case sharing.TargetFile:
		download := r.URL.Query().Has("download")
		if snippet, ok := h.infoFor(w, r, a, t.File, download); ok && h.sendPageWithInfo(w, r, t.File, snippet) {
			return
		}
		h.sendFile(w, r, t.File, download)
	case sharing.TargetViewer:
		h.pages.fileViewer(w, h.sharing, t.File)
	case sharing.TargetListing:
		h.pages.listing(w, t.Dir, h.sharing.ListDir(r.Context(), a, t.Dir))
	case sharing.TargetRedirect:
		http.Redirect(w, r, t.Location, http.StatusMovedPermanently)
	default:
		h.pages.notFound(w)
	}
}

func (h *Handler) sendFile(w http.ResponseWriter, r *http.Request, f *store.File, download bool) {
	fh, err := h.sharing.OpenBlob(f)
	if err != nil {
		h.pages.message(w, http.StatusInternalServerError, "Content unavailable", "The stored content could not be read.")
		return
	}
	defer fh.Close()
	h.fileHeaders(w, f, `"`+f.Blob+`"`, download)
	http.ServeContent(w, r, "", time.Time{}, fh)
}

func (h *Handler) fileHeaders(w http.ResponseWriter, f *store.File, etag string, download bool) {
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
			h.pages.gone(w)
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
		slug, ok := h.slug(r)
		if !ok || slug != "" {
			return false
		}
		name = strings.TrimPrefix(r.URL.Path, "/")
	}
	return brand.Has(name) && brand.Serve(w, r, name, brand.CacheShared)
}

// favicon serves an artifact's own icon when the visitor may see its content,
// and the redrop icon otherwise. Unknown, expired, revoked, exhausted and
// still-locked artifacts all get the same default response, so this path
// reveals nothing the origin's gate pages do not. It never records a view or
// consumes one: browsers fetch icons on their own, not on a visitor's behalf.
func (h *Handler) favicon(w http.ResponseWriter, r *http.Request, slug string) {
	if a, err := h.sharing.Lookup(r.Context(), slug); err == nil {
		grants := sharing.Grants{Visit: h.cookieValue(r, cookieVisit), Preview: h.cookieValue(r, cookiePreview)}
		if d := h.sharing.Decide(a, grants, time.Now()); d == sharing.Serve || d == sharing.ServeOwnerPreview {
			if t := h.sharing.ResolvePath(r.Context(), a, r.URL.Path); t.Kind == sharing.TargetFile {
				h.sendFile(w, r, t.File, false)
				return
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
