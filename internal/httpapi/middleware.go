package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/dashpaths"
	"github.com/mrRedSun/redrop/internal/sharing"
)

// CSRFHeader must be sent by browsers on cookie-authenticated writes. Custom
// headers force a CORS preflight, which this server never approves.
const CSRFHeader = "X-Redrop-Request"

type ctxKey struct{}

// principalFrom returns the authenticated caller set by requireAuth.
func principalFrom(ctx context.Context) *accounts.Principal {
	p, _ := ctx.Value(ctxKey{}).(*accounts.Principal)
	return p
}

// actorFrom adapts the principal for the sharing service.
func actorFrom(ctx context.Context) sharing.Actor {
	p := principalFrom(ctx)
	a := sharing.Actor{UserID: p.UserID(), Username: p.Username(), Quota: p.QuotaBytes()}
	a.TokenID, a.TokenName = p.Token()
	return a
}

// identify authenticates by bearer token or session cookie (nil, nil = anonymous).
func (a *API) identify(r *http.Request) (*accounts.Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		raw, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return nil, apperr.Unauthorized("malformed Authorization header")
		}
		return a.accounts.AuthenticateToken(r.Context(), raw)
	}
	c, err := r.Cookie(a.cookies.sessionName())
	if err != nil {
		return nil, nil
	}
	return a.accounts.AuthenticateSession(r.Context(), c.Value)
}

// checkCSRF rejects cross-site state changes made with cookies.
func (a *API) checkCSRF(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if r.Header.Get(CSRFHeader) != "1" {
		return apperr.Forbidden("missing %s header", CSRFHeader)
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return apperr.Forbidden("cross-site request rejected")
	}
	if o := r.Header.Get("Origin"); o != "" && o != a.cfg.PublicOrigin() {
		return apperr.Forbidden("cross-origin request rejected")
	}
	return nil
}

// Access levels for routes.
type access int

const (
	anyAuth     access = iota // dashboard session or API token
	sessionOnly               // dashboard session
	adminOnly                 // admin session (optionally SSO-only)
)

func (a *API) requireAuth(level access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.identify(r)
			if err == nil && p == nil {
				err = apperr.Unauthorized("not signed in")
			}
			if err == nil && !p.IsToken() {
				err = a.checkCSRF(r)
			}
			if err == nil {
				err = authorize(p, level, a.cfg.AdminRequiresSSO)
			}
			if err != nil {
				writeError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
		})
	}
}

// caller is what authorization needs to know about a principal.
type caller interface {
	IsToken() bool
	CanAdmin(requireSSO bool) bool
}

func authorize(p caller, level access, requireSSO bool) error {
	switch level {
	case sessionOnly:
		if p.IsToken() {
			return apperr.Forbidden("this endpoint requires a dashboard session, not an API token")
		}
	case adminOnly:
		if !p.CanAdmin(false) {
			return apperr.Forbidden("admin only")
		}
		if !p.CanAdmin(requireSSO) {
			return apperr.Forbidden("admin actions require signing in with single sign-on")
		}
	}
	return nil
}

// csrfOnly protects unauthenticated writes (login, setup, invites).
func (a *API) csrfOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.checkCSRF(r); err != nil {
			writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders applies dashboard-origin headers.
func (a *API) securityHeaders(next http.Handler) http.Handler {
	csp := "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-src " + a.cfg.ContentFrameSource() +
		"; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if !strings.HasPrefix(r.URL.Path, "/api/docs") {
			h.Set("Content-Security-Policy", csp)
		}
		if a.cfg.PublicURL.Scheme == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// --- request logging (shared with the content transport) ---

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusRecorder) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func redactPath(p string) string {
	for _, pre := range dashpaths.SecretPrefixes {
		if rest, ok := strings.CutPrefix(p, pre); ok {
			if _, tail, found := strings.Cut(rest, "/"); found {
				return pre + "…/" + tail
			}
			return pre + "…"
		}
	}
	return p
}

// LogRequests logs API calls and failed requests (never query strings).
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || rec.status >= 400 {
			a.log.Info("http", "method", r.Method, "path", redactPath(r.URL.Path), "status", rec.status, "bytes", rec.bytes, "ms", time.Since(start).Milliseconds())
		}
	})
}
