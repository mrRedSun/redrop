// Package accounts implements users, sessions, API tokens, invites, password
// resets, TOTP, and single sign-on. It is transport-agnostic: HTTP handlers
// call it and translate apperr kinds into responses.
package accounts

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Authentication methods recorded on sessions.
const (
	MethodPassword = "password"
	MethodOIDC     = "oidc"
	MethodToken    = "token"
)

const (
	SessionTTL  = 30 * 24 * time.Hour
	TokenPrefix = "rdp_"
	ResetTTL    = 24 * time.Hour
	setupTTL    = 24 * time.Hour
	InviteTTL   = 7 * 24 * time.Hour
)

// Principal is an authenticated caller. It is opaque to transports: they
// read identity through accessors and pass it back to the services.
type Principal struct {
	user        *store.User
	method      string
	sessionHash string       // set for dashboard sessions
	token       *store.Token // set for API tokens
}

func (p *Principal) UserID() string    { return p.user.ID }
func (p *Principal) Username() string  { return p.user.Username }
func (p *Principal) QuotaBytes() int64 { return p.user.QuotaBytes }
func (p *Principal) Method() string    { return p.method }
func (p *Principal) IsToken() bool     { return p.method == MethodToken }

// Token returns the API token's ID and name ("" for dashboard sessions).
func (p *Principal) Token() (id, name string) {
	if p.token == nil {
		return "", ""
	}
	return p.token.ID, p.token.Name
}

// CanAdmin reports whether admin operations are allowed for this principal.
func (p *Principal) CanAdmin(requireSSO bool) bool {
	if p.user.Role != api.RoleAdmin || p.IsToken() {
		return false
	}
	return !requireSSO || p.method == MethodOIDC
}

// Session is a newly created dashboard session; Secret goes in the cookie.
type Session struct {
	Secret    string
	ExpiresAt time.Time
	Principal *Principal
}

type Service struct {
	db  *store.Store
	cfg *config.Config
	log *slog.Logger

	loginLimiter   *security.Limiter // per IP+username
	loginIPLimiter *security.Limiter // per IP
	linkLimiter    *security.Limiter // per IP, unauthenticated link lookups
	stateSigner    *security.Signer  // SSO state

	ssoMu     sync.Mutex
	providers map[string]*provider
}

func New(db *store.Store, cfg *config.Config, secret []byte, log *slog.Logger) *Service {
	s := &Service{
		db: db, cfg: cfg, log: log,
		loginLimiter:   security.NewLimiter(5, 5*time.Minute),
		loginIPLimiter: security.NewLimiter(30, 15*time.Minute),
		linkLimiter:    security.NewLimiter(60, 10*time.Minute),
		stateSigner:    security.NewSigner(secret, "oidc-state"),
	}
	s.initProviders()
	return s
}

// --- authentication ---

// AuthenticateToken resolves an "rdp_…" bearer token.
func (s *Service) AuthenticateToken(ctx context.Context, raw string) (*Principal, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, TokenPrefix) {
		return nil, apperr.Unauthorized("malformed API token")
	}
	t, err := s.db.TokenByHash(ctx, ids.Hash(raw))
	if err != nil {
		return nil, apperr.Unauthorized("invalid or expired API token")
	}
	u, err := s.activeUser(ctx, t.UserID)
	if err != nil {
		return nil, err
	}
	s.db.TouchToken(ctx, t.ID)
	return &Principal{user: u, method: MethodToken, token: t}, nil
}

// AuthenticateSession resolves a session cookie secret.
func (s *Service) AuthenticateSession(ctx context.Context, secret string) (*Principal, error) {
	if secret == "" {
		return nil, apperr.Unauthorized("not signed in")
	}
	hash := ids.Hash(secret)
	se, err := s.db.SessionByHash(ctx, hash)
	if err != nil {
		return nil, apperr.Unauthorized("session expired")
	}
	u, err := s.activeUser(ctx, se.UserID)
	if err != nil {
		return nil, err
	}
	s.db.TouchSession(ctx, hash)
	return &Principal{user: u, method: se.Method, sessionHash: hash}, nil
}

func (s *Service) activeUser(ctx context.Context, id string) (*store.User, error) {
	u, err := s.db.UserByID(ctx, id)
	if err != nil || u.Disabled {
		return nil, apperr.Unauthorized("account disabled")
	}
	return u, nil
}

func (s *Service) newSession(ctx context.Context, u *store.User, method string) (*Session, error) {
	secret := ids.Secret()
	if err := s.db.CreateSession(ctx, ids.Hash(secret), u.ID, method, SessionTTL); err != nil {
		return nil, apperr.Internal(err)
	}
	return &Session{Secret: secret, ExpiresAt: time.Now().Add(SessionTTL), Principal: &Principal{user: u, method: method, sessionHash: ids.Hash(secret)}}, nil
}

// Login verifies a password (and TOTP when enabled) and opens a session.
func (s *Service) Login(ctx context.Context, ip, username, password, totpCode string) (*Session, error) {
	if !s.cfg.PasswordLogin {
		return nil, apperr.Forbidden("password login is disabled; use single sign-on")
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if !s.loginIPLimiter.Allow(ip) || !s.loginLimiter.Allow(ip+"|"+username) {
		return nil, apperr.RateLimited("too many sign-in attempts; wait a few minutes")
	}
	u, err := s.db.UserByUsername(ctx, username)
	hash := ""
	if err == nil {
		hash = u.PasswordHash
	}
	// CheckPassword runs even for unknown users to equalize timing.
	if !security.CheckPassword(hash, password) || u == nil || u.Disabled {
		return nil, apperr.Unauthorized("wrong username or password")
	}
	if u.TOTPEnabled {
		if totpCode == "" {
			return nil, apperr.Unauthorized("enter the code from your authenticator app").WithCode("totp_required")
		}
		if !validateTOTP(totpCode, u.TOTPSecret) {
			return nil, apperr.Unauthorized("wrong authenticator code")
		}
	}
	return s.newSession(ctx, u, MethodPassword)
}

// Logout ends a session (unknown secrets are ignored).
func (s *Service) Logout(ctx context.Context, secret string) {
	if secret != "" {
		s.db.DeleteSession(ctx, ids.Hash(secret))
	}
}

// Me describes the caller, including storage usage.
func (s *Service) Me(ctx context.Context, p *Principal) (Me, error) {
	usage, err := s.db.UserUsage(ctx, p.user.ID)
	if err != nil {
		return Me{}, apperr.Internal(err)
	}
	_, tokenName := p.Token()
	return Me{User: userView(p.user, usage), AuthMethod: p.method, TokenName: tokenName, CanAdmin: p.CanAdmin(s.cfg.AdminRequiresSSO)}, nil
}

// --- validation helpers ---

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

func normalizeUsername(u string) (string, error) {
	u = strings.ToLower(strings.TrimSpace(u))
	if !usernameRE.MatchString(u) {
		return "", apperr.Invalid("username must be 2-32 characters: lowercase letters, digits, dot, dash, underscore")
	}
	return u, nil
}

func validatePassword(pw string) error {
	if err := security.ValidatePassword(pw); err != nil {
		return apperr.Invalid("%s", err.Error())
	}
	return nil
}

func mapStore(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("not found")
	case errors.Is(err, store.ErrLinkUsed):
		return apperr.Gone("this link was already used or has expired")
	}
	return apperr.Internal(err)
}
