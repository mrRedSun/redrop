package httpapi

import (
	"context"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-fuego/fuego"

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
	a := sharing.Actor{UserID: p.UserID(), Username: p.Username(), Quota: p.QuotaBytes(), TokenExpires: p.TokenExpiresAt()}
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
	tokenOnly                 // API token (the tunnel WebSocket: no web page can send one)
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
	case tokenOnly:
		if !p.IsToken() {
			return apperr.Forbidden("this endpoint requires an API token")
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

// Request body limits. The unauthenticated sign-in and onboarding routes
// take a username, password, code and link token, which fit in a few KiB.
const (
	maxAuthBody = 4 << 10
	maxJSONBody = 1 << 20
)

// authBody admits the JSON body of an unauthenticated sign-in or onboarding
// route, up to maxAuthBody.
func (a *API) authBody(next http.Handler) http.Handler { return jsonBody(next, maxAuthBody) }

// jsonIn admits the JSON body of any other route, up to maxJSONBody.
func (a *API) jsonIn(next http.Handler) http.Handler { return jsonBody(next, maxJSONBody) }

// jsonBody runs before anything decodes the body. A body that is not
// application/json gets 415; Fuego would otherwise pick a decoder from the
// Content-Type and panic on text/plain. A declared length over n gets 413,
// and a body without one is cut off at n (413 from toProblem). A request
// with neither a body nor a Content-Type passes: Fuego decodes it as empty.
func jsonBody(next http.Handler, n int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" || r.ContentLength != 0 {
			if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
				writeError(w, r, fuego.HTTPError{Status: http.StatusUnsupportedMediaType, Title: http.StatusText(http.StatusUnsupportedMediaType), Detail: "the request body must be application/json"})
				return
			}
		}
		if r.ContentLength > n {
			writeError(w, r, apperr.TooLarge("request body is too large"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, n)
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
		// The docs page (REDROP_API_DOCS) loads its script from a CDN.
		if !a.cfg.APIDocs || !strings.HasPrefix(r.URL.Path, "/api/docs") {
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

// redactPath hides the one-time secret that follows a SecretPrefixes
// prefix. It matches the canonical path (empty and dot segments cleaned,
// the prefix compared case-insensitively), so /api/v1//invites/<t> and
// /API/v1/invites/<t> are redacted too, and logs a redacted path in that
// canonical form.
func redactPath(p string) string {
	c := path.Clean("/" + p)
	for _, pre := range dashpaths.SecretPrefixes {
		if len(c) <= len(pre) || !strings.EqualFold(c[:len(pre)], pre) {
			continue
		}
		if _, tail, found := strings.Cut(c[len(pre):], "/"); found {
			return pre + "…/" + tail
		}
		return pre + "…"
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
