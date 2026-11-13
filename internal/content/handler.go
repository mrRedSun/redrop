// Package content serves artifacts on their own subdomains of the content
// domain (https://<slug>.<content-domain>/) plus the content apex. Access
// rules live in the sharing service; this package only speaks HTTP.
//
// Every miss on an artifact host (malformed, unknown, deleted, expired,
// exhausted, or revoked slug) gets the same 404 page and spends one token of
// the client's miss budget. A client that has spent its budget gets 429 for
// every artifact-host request until it refills; hits never spend budget.
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
	if !ok || !ids.ValidSlug(label) {
		return "", false
	}
	return label, true
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
	slug, ok := h.slug(r)
	switch {
	case !ok:
		h.miss(w, r)
		return
	case slug == "":
		h.serveApex(w, r)
		return
	case r.URL.Path == "/robots.txt":
		RobotsTxt(w, r)
		return
	case h.misses.Exhausted(h.missKey(r)):
		h.tooManyMisses(w)
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
		h.miss(w, r)
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
	h.serveContent(w, r, a)
}

func (h *Handler) serveContent(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	t := h.sharing.ResolvePath(r.Context(), a, r.URL.Path)
	switch t.Kind {
	case sharing.TargetFile:
		h.sendFile(w, r, t.File, r.URL.Query().Has("download"))
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
