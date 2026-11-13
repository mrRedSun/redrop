package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
)

// SSO modes.
const (
	SSOLogin  = "login"
	SSOLink   = "link"
	SSOInvite = "invite"
)

// SSOStateTTL bounds how long a sign-in round trip may take.
const SSOStateTTL = 10 * time.Minute

type provider struct {
	cfg      config.OIDCProvider
	oidc     *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

// Provider is a configured identity provider.
type Provider struct {
	ID    string
	Label string
}

func (s *Service) initProviders(ctx context.Context) {
	s.providers = map[string]*provider{}
	for _, pc := range s.cfg.OIDC {
		s.providers[pc.ID] = &provider{cfg: pc}
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if _, err := s.discover(dctx, pc.ID); err != nil {
			// Don't block startup on an unreachable IdP; discovery retries on use.
			s.log.Error("OIDC discovery failed; will retry on use", "provider", pc.ID, "error", err)
		}
		cancel()
	}
}

// discover returns a ready provider, performing OIDC discovery lazily.
func (s *Service) discover(ctx context.Context, id string) (*provider, error) {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	p, ok := s.providers[id]
	if !ok {
		return nil, apperr.NotFound("unknown sign-in provider")
	}
	if p.oidc != nil {
		return p, nil
	}
	op, err := oidc.NewProvider(ctx, p.cfg.Issuer)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("oidc discovery for %s: %w", id, err))
	}
	p.oidc = op
	p.verifier = op.Verifier(&oidc.Config{ClientID: p.cfg.ClientID})
	p.oauth = oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     op.Endpoint(),
		RedirectURL:  s.cfg.PublicOrigin() + "/api/v1/auth/oidc/" + p.cfg.ID + "/callback",
		Scopes:       p.cfg.Scopes,
	}
	return p, nil
}

func (s *Service) Providers() []Provider {
	out := make([]Provider, 0, len(s.cfg.OIDC))
	for _, pc := range s.cfg.OIDC {
		out = append(out, Provider{ID: pc.ID, Label: pc.Label})
	}
	return out
}

func (s *Service) providerLabel(id string) string {
	for _, pc := range s.cfg.OIDC {
		if pc.ID == id {
			return pc.Label
		}
	}
	return id
}

type ssoState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Mode     string `json:"m"`
	Provider string `json:"p"`
	Invite   string `json:"i,omitempty"`
	UserID   string `json:"u,omitempty"`
}

// SSOStart is the redirect target and the signed state to store in a cookie.
type SSOStart struct {
	RedirectURL string
	State       string
}

// FailurePath is where the browser goes when an SSO flow in mode fails.
func FailurePath(mode string) string {
	if mode == SSOLink {
		return "/account"
	}
	return "/login"
}

// BeginSSO validates the request and builds the provider redirect.
// current is required for SSOLink.
func (s *Service) BeginSSO(ctx context.Context, ip, providerID, mode string, current *Principal, inviteToken string) (*SSOStart, error) {
	if !s.loginIPLimiter.Allow(ip) {
		return nil, apperr.RateLimited("too many sign-in attempts; wait a few minutes")
	}
	p, err := s.discover(ctx, providerID)
	if err != nil {
		return nil, apperr.Forbidden("single sign-on provider is unavailable")
	}
	st := ssoState{State: ids.Secret(), Nonce: ids.Secret(), Verifier: oauth2.GenerateVerifier(), Mode: mode, Provider: p.cfg.ID}
	switch mode {
	case SSOLogin:
	case SSOLink:
		if current == nil || current.IsToken() {
			return nil, apperr.Unauthorized("sign in first to link an account")
		}
		st.UserID = current.User.ID
	case SSOInvite:
		if _, err := s.db.OneTimeByHash(ctx, store.PurposeInvite, ids.Hash(inviteToken)); err != nil {
			return nil, apperr.Gone("this invite link was already used or has expired")
		}
		st.Invite = inviteToken
	default:
		return nil, apperr.Invalid("mode must be login, link, or invite")
	}
	raw, _ := json.Marshal(st)
	return &SSOStart{
		RedirectURL: p.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)),
		State:       s.stateSigner.Sign(string(raw), time.Now().Add(SSOStateTTL)),
	}, nil
}

// SSOResult is the outcome of a completed SSO flow.
type SSOResult struct {
	Session  *Session
	Redirect string
}

type claims struct {
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
}

// CompleteSSO verifies the callback. failPath is always set so the caller can
// redirect with the error message.
func (s *Service) CompleteSSO(ctx context.Context, providerID, signedState string, q url.Values, current *Principal) (res *SSOResult, failPath string, err error) {
	failPath = "/login"
	payload, ok := s.stateSigner.Verify(signedState)
	var st ssoState
	if !ok || json.Unmarshal([]byte(payload), &st) != nil || st.State != q.Get("state") || st.Provider != providerID {
		return nil, failPath, apperr.Invalid("sign-in session expired or mismatched; try again")
	}
	failPath = FailurePath(st.Mode)
	if q.Get("error") != "" {
		return nil, failPath, apperr.Forbidden("single sign-on was cancelled or denied")
	}
	p, err := s.discover(ctx, st.Provider)
	if err != nil {
		return nil, failPath, apperr.Forbidden("single sign-on provider is unavailable")
	}
	tok, err := p.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.log.Warn("OIDC code exchange failed", "provider", st.Provider, "error", err)
		return nil, failPath, apperr.Forbidden("single sign-on failed")
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := p.verifier.Verify(ctx, rawID)
	if err != nil || idt.Nonce != st.Nonce {
		return nil, failPath, apperr.Forbidden("single sign-on token was invalid")
	}
	var c claims
	idt.Claims(&c)
	if c.Email == "" || c.PreferredUsername == "" {
		if ui, err := p.oidc.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			var extra claims
			ui.Claims(&extra)
			c.Email = firstNonEmpty(c.Email, extra.Email)
			c.PreferredUsername = firstNonEmpty(c.PreferredUsername, extra.PreferredUsername)
			c.Name = firstNonEmpty(c.Name, extra.Name)
		}
	}
	label := p.cfg.Label
	existing, lookupErr := s.db.IdentityBySubject(ctx, st.Provider, idt.Subject)

	switch st.Mode {
	case SSOLogin:
		if lookupErr != nil {
			return nil, failPath, apperr.Forbidden("this %s account is not linked to a redrop user; sign in with a password and link it from Account, or use an invite", label)
		}
		u, err := s.activeUser(ctx, existing.UserID)
		if err != nil {
			return nil, failPath, err
		}
		sess, err := s.newSession(ctx, u, MethodOIDC)
		return &SSOResult{Session: sess, Redirect: "/"}, failPath, err

	case SSOLink:
		if current == nil || current.User.ID != st.UserID {
			return nil, "/login", apperr.Unauthorized("sign in again to link an account")
		}
		if lookupErr == nil {
			if existing.UserID != current.User.ID {
				return nil, failPath, apperr.Conflict("that %s account is already linked to another user", label)
			}
		} else if err := s.db.LinkIdentity(ctx, current.User.ID, st.Provider, idt.Subject, c.Email); err != nil {
			return nil, failPath, apperr.Internal(err)
		}
		// Upgrade to an SSO session (enables admin pages when SSO is required).
		if current.SessionHash != "" {
			s.db.DeleteSession(ctx, current.SessionHash)
		}
		sess, err := s.newSession(ctx, current.User, MethodOIDC)
		return &SSOResult{Session: sess, Redirect: "/account?linked=1"}, failPath, err

	case SSOInvite:
		if lookupErr == nil {
			return nil, failPath, apperr.Conflict("that %s account already has a redrop user; sign in instead", label)
		}
		o, err := s.db.OneTimeByHash(ctx, store.PurposeInvite, ids.Hash(st.Invite))
		if err != nil {
			return nil, failPath, apperr.Gone("this invite link was already used or has expired")
		}
		username := s.uniqueUsername(ctx, c)
		u, err := s.db.RedeemAccountLink(ctx, o, username, firstNonEmpty(c.Name, username), "", s.cfg.DefaultQuotaBytes, st.Provider, idt.Subject, c.Email)
		if err != nil {
			return nil, failPath, mapStore(err)
		}
		s.log.Info("account created", "user", u.Username, "role", u.Role, "via", "invite+sso")
		sess, err := s.newSession(ctx, u, MethodOIDC)
		return &SSOResult{Session: sess, Redirect: "/tokens?welcome=1"}, failPath, err
	}
	return nil, failPath, apperr.Invalid("unknown sign-in mode")
}

var nonUsername = regexp.MustCompile(`[^a-z0-9._-]+`)

func (s *Service) uniqueUsername(ctx context.Context, c claims) string {
	base := c.PreferredUsername
	if base == "" {
		base, _, _ = strings.Cut(c.Email, "@")
	}
	base = strings.Trim(nonUsername.ReplaceAllString(strings.ToLower(base), ""), "._-")
	if len(base) < 2 {
		base = "user"
	}
	if len(base) > 26 {
		base = base[:26]
	}
	name := base
	for i := 2; ; i++ {
		if taken, _ := s.db.UsernameTaken(ctx, name); !taken {
			return name
		}
		name = fmt.Sprintf("%s%d", base, i)
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
