package content

import (
	"net/http"

	"github.com/mrRedSun/redrop/internal/sharing"
)

// Cookies on artifact hosts are host-only (each artifact has its own origin)
// and HttpOnly, so artifact scripts cannot read or forge them.
const (
	cookieVisit   = "visit"   // granted after Open / correct password
	cookiePreview = "preview" // owner preview from the dashboard
	cookieSeen    = "seen"    // de-duplicates view counting
	cookieDirect  = "direct"  // the visitor chose the direct view (?redrop-info=0)
)

func (h *Handler) cookieName(base string) string {
	if h.cfg.Insecure {
		return "rd_" + base
	}
	return "__Host-rd_" + base
}

func (h *Handler) cookieValue(r *http.Request, base string) string {
	if c, err := r.Cookie(h.cookieName(base)); err == nil {
		return c.Value
	}
	return ""
}

// setCookie writes a grant. crossSite cookies (owner previews embedded in the
// dashboard iframe) use SameSite=None with CHIPS partitioning.
func (h *Handler) setCookie(w http.ResponseWriter, base string, g *sharing.Grant, crossSite bool) {
	c := &http.Cookie{
		Name: h.cookieName(base), Value: g.Value, Path: "/", MaxAge: int(g.TTL.Seconds()),
		HttpOnly: true, Secure: !h.cfg.Insecure, SameSite: http.SameSiteLaxMode,
	}
	if crossSite && !h.cfg.Insecure {
		c.SameSite, c.Partitioned = http.SameSiteNoneMode, true
	}
	http.SetCookie(w, c)
}

// clearCookie removes a cookie set by setCookie (non-cross-site).
func (h *Handler) clearCookie(w http.ResponseWriter, base string) {
	http.SetCookie(w, &http.Cookie{Name: h.cookieName(base), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: !h.cfg.Insecure, SameSite: http.SameSiteLaxMode})
}
