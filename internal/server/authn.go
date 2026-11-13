package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

const (
	sessionTTL   = 30 * 24 * time.Hour
	tokenPrefix  = "rdp_"
	methodToken  = "token"
	methodPass   = "password"
	methodOIDC   = "oidc"
	csrfHeader   = "X-Redrop-Request"
	ctxPrincipal = ctxKey("principal")
)

type ctxKey string

// Principal is the authenticated caller.
type Principal struct {
	User        *store.User
	Method      string // password, oidc, token
	SessionHash string
	Token       *store.Token
}

func (p *Principal) CanAdmin(requireSSO bool) bool {
	if p.User.Role != api.RoleAdmin || p.Method == methodToken {
		return false
	}
	return !requireSSO || p.Method == methodOIDC
}

func principal(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxPrincipal).(*Principal)
	return p
}

func (s *Server) sessionCookieName() string {
	if s.cfg.Insecure {
		return "redrop_session"
	}
	return "__Host-redrop_session"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookieName(), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !s.cfg.Insecure, SameSite: http.SameSiteLaxMode,
	})
}

// startSession creates a session for u and sets the cookie.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, u *store.User, method string) error {
	secret := ids.Secret()
	if err := s.db.CreateSession(ctx, ids.Hash(secret), u.ID, method, sessionTTL); err != nil {
		return err
	}
	s.setSessionCookie(w, secret, int(sessionTTL.Seconds()))
	return nil
}

// identify resolves the caller from a bearer token or the session cookie.
func (s *Server) identify(r *http.Request) (*Principal, error) {
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); h != "" {
		raw, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || !strings.HasPrefix(raw, tokenPrefix) {
			return nil, errUnauthorized("malformed Authorization header")
		}
		t, err := s.db.TokenByHash(ctx, ids.Hash(strings.TrimSpace(raw)))
		if err != nil {
			return nil, errUnauthorized("invalid or expired API token")
		}
		u, err := s.db.UserByID(ctx, t.UserID)
		if err != nil || u.Disabled {
			return nil, errUnauthorized("account disabled")
		}
		s.db.TouchToken(ctx, t.ID)
		return &Principal{User: u, Method: methodToken, Token: t}, nil
	}
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil || c.Value == "" {
		return nil, errUnauthorized("not signed in")
	}
	hash := ids.Hash(c.Value)
	se, err := s.db.SessionByHash(ctx, hash)
	if err != nil {
		return nil, errUnauthorized("session expired")
	}
	u, err := s.db.UserByID(ctx, se.UserID)
	if err != nil || u.Disabled {
		return nil, errUnauthorized("account disabled")
	}
	s.db.TouchSession(ctx, hash)
	return &Principal{User: u, Method: se.Method, SessionHash: hash}, nil
}

// checkCSRF rejects cross-site cookie-authenticated state changes.
func (s *Server) checkCSRF(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if r.Header.Get(csrfHeader) != "1" {
		return errForbidden("missing " + csrfHeader + " header")
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return errForbidden("cross-site request rejected")
	}
	if o := r.Header.Get("Origin"); o != "" && o != s.cfg.PublicOrigin() {
		return errForbidden("cross-origin request rejected")
	}
	return nil
}

type access int

const (
	accessAny     access = iota // session or token
	accessSession               // dashboard session only
	accessAdmin                 // admin session (optionally SSO)
)

// authMW returns route middleware enforcing the given access level.
func (s *Server) authMW(level access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := s.identify(r)
			if err != nil {
				writeErr(w, r, err)
				return
			}
			if p.Method != methodToken {
				if err := s.checkCSRF(r); err != nil {
					writeErr(w, r, err)
					return
				}
			}
			switch level {
			case accessSession:
				if p.Method == methodToken {
					writeErr(w, r, errForbidden("this endpoint requires a dashboard session, not an API token"))
					return
				}
			case accessAdmin:
				if p.User.Role != api.RoleAdmin || p.Method == methodToken {
					writeErr(w, r, errForbidden("admin only"))
					return
				}
				if s.cfg.AdminRequiresSSO && p.Method != methodOIDC {
					writeErr(w, r, errForbidden("admin actions require signing in with single sign-on"))
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
		})
	}
}

// csrfOnly protects unauthenticated mutating endpoints (login, setup).
func (s *Server) csrfOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.checkCSRF(r); err != nil {
			writeErr(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	he := toHTTPError(r.Context(), err)
	fuego.SendJSONError(w, r, he)
}

func errStatus(status int, detail string) fuego.HTTPError {
	return fuego.HTTPError{Status: status, Title: http.StatusText(status), Detail: detail}
}

func errUnauthorized(d string) error { return errStatus(http.StatusUnauthorized, d) }
func errForbidden(d string) error    { return errStatus(http.StatusForbidden, d) }
func errBadRequest(d string) error   { return errStatus(http.StatusBadRequest, d) }
func errNotFound(d string) error     { return errStatus(http.StatusNotFound, d) }
func errConflict(d string) error     { return errStatus(http.StatusConflict, d) }
func errTooMany(d string) error      { return errStatus(http.StatusTooManyRequests, d) }

// toHTTPError maps internal errors to safe HTTP errors.
func toHTTPError(ctx context.Context, err error) fuego.HTTPError {
	var he fuego.HTTPError
	switch {
	case errors.As(err, &he):
		return he
	case errors.Is(err, store.ErrNotFound):
		return errStatus(http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrQuota):
		return errStatus(http.StatusRequestEntityTooLarge, "storage quota exceeded")
	case errors.Is(err, store.ErrLinkUsed):
		return errStatus(http.StatusGone, "this link was already used or has expired")
	}
	slog.ErrorContext(ctx, "internal error", "error", err)
	return errStatus(http.StatusInternalServerError, "internal error")
}

// fail converts errors for Fuego handlers.
func fail(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return toHTTPError(ctx, err)
}

// clientIP returns the caller IP, honouring the forwarding header only when
// the direct peer is a trusted proxy.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !s.trusted(ip) {
		return host
	}
	hdr := r.Header.Get(s.cfg.ClientIPHeader)
	if hdr == "" {
		return host
	}
	parts := strings.Split(hdr, ",")
	// Walk right-to-left past trusted proxies.
	for i := len(parts) - 1; i >= 0; i-- {
		cand := strings.TrimSpace(parts[i])
		cip := net.ParseIP(cand)
		if cip == nil {
			break
		}
		if !s.trusted(cip) || i == 0 {
			return cip.String()
		}
	}
	return host
}

func (s *Server) trusted(ip net.IP) bool {
	for _, n := range s.cfg.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// country returns the CDN-provided country code from a trusted proxy.
func (s *Server) country(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil || !s.trusted(ip) || s.cfg.CountryHeader == "" {
		return ""
	}
	c := strings.ToUpper(strings.TrimSpace(r.Header.Get(s.cfg.CountryHeader)))
	if len(c) != 2 || c == "XX" || c == "T1" {
		return ""
	}
	return c
}
