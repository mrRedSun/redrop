package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

type oidcProvider struct {
	cfg      config.OIDCProvider
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

func (s *Server) setupOIDC(ctx context.Context) error {
	s.oidc = map[string]*oidcProvider{}
	for _, pc := range s.cfg.OIDC {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		p, err := oidc.NewProvider(dctx, pc.Issuer)
		cancel()
		if err != nil {
			// Don't block startup on an unreachable IdP; retry lazily.
			s.log.Error("OIDC discovery failed; provider will retry on use", "provider", pc.ID, "error", err)
			s.oidc[pc.ID] = &oidcProvider{cfg: pc}
			continue
		}
		s.oidc[pc.ID] = s.newOIDC(pc, p)
	}
	return nil
}

func (s *Server) newOIDC(pc config.OIDCProvider, p *oidc.Provider) *oidcProvider {
	return &oidcProvider{
		cfg:      pc,
		provider: p,
		verifier: p.Verifier(&oidc.Config{ClientID: pc.ClientID}),
		oauth: oauth2.Config{
			ClientID:     pc.ClientID,
			ClientSecret: pc.ClientSecret,
			Endpoint:     p.Endpoint(),
			RedirectURL:  s.cfg.PublicOrigin() + "/api/v1/auth/oidc/" + pc.ID + "/callback",
			Scopes:       pc.Scopes,
		},
	}
}

func (s *Server) getProvider(ctx context.Context, id string) (*oidcProvider, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	p, ok := s.oidc[id]
	if !ok {
		return nil, fmt.Errorf("unknown provider")
	}
	if p.provider == nil {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		prov, err := oidc.NewProvider(dctx, p.cfg.Issuer)
		if err != nil {
			return nil, fmt.Errorf("provider unavailable")
		}
		p = s.newOIDC(p.cfg, prov)
		s.oidc[id] = p
	}
	return p, nil
}

func (s *Server) providerList() []api.AuthProvider {
	out := []api.AuthProvider{}
	for _, pc := range s.cfg.OIDC {
		out = append(out, api.AuthProvider{ID: pc.ID, Label: pc.Label})
	}
	return out
}

type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Mode     string `json:"m"`
	Provider string `json:"p"`
	Invite   string `json:"i,omitempty"` // raw invite token
	UserID   string `json:"u,omitempty"` // for linking
}

func (s *Server) oidcCookieName() string {
	if s.cfg.Insecure {
		return "redrop_oidc"
	}
	return "__Host-redrop_oidc"
}

func (s *Server) oidcFail(w http.ResponseWriter, r *http.Request, dest, msg string) {
	http.Redirect(w, r, dest+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.loginIPLimiter.Allow(s.clientIP(r)) {
		s.oidcFail(w, r, "/login", "too many sign-in attempts; wait a few minutes")
		return
	}
	p, err := s.getProvider(ctx, r.PathValue("provider"))
	if err != nil {
		s.oidcFail(w, r, "/login", "single sign-on provider is unavailable")
		return
	}
	st := oidcState{State: ids.Secret(), Nonce: ids.Secret(), Verifier: oauth2.GenerateVerifier(), Mode: r.URL.Query().Get("mode"), Provider: p.cfg.ID}
	switch st.Mode {
	case "", "login":
		st.Mode = "login"
	case "link":
		pr, err := s.identify(r)
		if err != nil || pr.Method == methodToken {
			s.oidcFail(w, r, "/login", "sign in first to link an account")
			return
		}
		st.UserID = pr.User.ID
	case "invite":
		st.Invite = r.URL.Query().Get("token")
		if _, err := s.db.OneTimeByHash(ctx, store.PurposeInvite, ids.Hash(st.Invite)); err != nil {
			s.oidcFail(w, r, "/login", "this invite link was already used or has expired")
			return
		}
	default:
		http.Error(w, "bad mode", http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{
		Name: s.oidcCookieName(), Value: s.stateSigner.Sign(string(raw), time.Now().Add(10*time.Minute)),
		Path: "/", MaxAge: 600, HttpOnly: true, Secure: !s.cfg.Insecure, SameSite: http.SameSiteLaxMode,
	})
	authURL := p.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

var nonUsername = regexp.MustCompile(`[^a-z0-9._-]+`)

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ck, err := r.Cookie(s.oidcCookieName())
	http.SetCookie(w, &http.Cookie{Name: s.oidcCookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: !s.cfg.Insecure, SameSite: http.SameSiteLaxMode})
	if err != nil {
		s.oidcFail(w, r, "/login", "sign-in session expired; try again")
		return
	}
	payload, ok := s.stateSigner.Verify(ck.Value)
	var st oidcState
	if !ok || json.Unmarshal([]byte(payload), &st) != nil || st.State != r.URL.Query().Get("state") || st.Provider != r.PathValue("provider") {
		s.oidcFail(w, r, "/login", "sign-in session mismatch; try again")
		return
	}
	dest := "/login"
	if st.Mode == "link" {
		dest = "/account"
	}
	if e := r.URL.Query().Get("error"); e != "" {
		s.oidcFail(w, r, dest, "single sign-on was cancelled or denied")
		return
	}
	p, err := s.getProvider(ctx, st.Provider)
	if err != nil {
		s.oidcFail(w, r, dest, "single sign-on provider is unavailable")
		return
	}
	tok, err := p.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.log.Warn("OIDC code exchange failed", "provider", st.Provider, "error", err)
		s.oidcFail(w, r, dest, "single sign-on failed")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := p.verifier.Verify(ctx, rawID)
	if err != nil || idt.Nonce != st.Nonce {
		s.oidcFail(w, r, dest, "single sign-on token was invalid")
		return
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
	}
	idt.Claims(&claims)
	if claims.Email == "" || claims.PreferredUsername == "" {
		if ui, err := p.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			var extra struct {
				Email             string `json:"email"`
				PreferredUsername string `json:"preferred_username"`
				Name              string `json:"name"`
			}
			ui.Claims(&extra)
			if claims.Email == "" {
				claims.Email = extra.Email
			}
			if claims.PreferredUsername == "" {
				claims.PreferredUsername = extra.PreferredUsername
			}
			if claims.Name == "" {
				claims.Name = extra.Name
			}
		}
	}

	existing, lookupErr := s.db.IdentityBySubject(ctx, st.Provider, idt.Subject)
	switch st.Mode {
	case "login":
		if lookupErr != nil {
			s.oidcFail(w, r, dest, "this "+p.cfg.Label+" account is not linked to a redrop user; sign in with a password and link it from Account, or use an invite")
			return
		}
		u, err := s.db.UserByID(ctx, existing.UserID)
		if err != nil || u.Disabled {
			s.oidcFail(w, r, dest, "account disabled")
			return
		}
		if err := s.startSession(ctx, w, u, methodOIDC); err != nil {
			s.oidcFail(w, r, dest, "internal error")
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "link":
		pr, err := s.identify(r)
		if err != nil || pr.User.ID != st.UserID {
			s.oidcFail(w, r, "/login", "sign in again to link an account")
			return
		}
		if lookupErr == nil {
			if existing.UserID == pr.User.ID {
				http.Redirect(w, r, "/account?linked=1", http.StatusSeeOther)
			} else {
				s.oidcFail(w, r, dest, "that "+p.cfg.Label+" account is already linked to another user")
			}
			return
		}
		if err := s.db.LinkIdentity(ctx, pr.User.ID, st.Provider, idt.Subject, claims.Email); err != nil {
			s.oidcFail(w, r, dest, "could not link account")
			return
		}
		// Upgrade the current session to an SSO session.
		if pr.SessionHash != "" {
			s.db.DeleteSession(ctx, pr.SessionHash)
		}
		s.startSession(ctx, w, pr.User, methodOIDC)
		http.Redirect(w, r, "/account?linked=1", http.StatusSeeOther)
	case "invite":
		if lookupErr == nil {
			s.oidcFail(w, r, dest, "that "+p.cfg.Label+" account already has a redrop user; sign in instead")
			return
		}
		o, err := s.db.OneTimeByHash(ctx, store.PurposeInvite, ids.Hash(st.Invite))
		if err != nil {
			s.oidcFail(w, r, dest, "this invite link was already used or has expired")
			return
		}
		base := claims.PreferredUsername
		if base == "" {
			base, _, _ = strings.Cut(claims.Email, "@")
		}
		base = strings.Trim(nonUsername.ReplaceAllString(strings.ToLower(base), ""), "._-")
		if len(base) < 2 {
			base = "user"
		}
		if len(base) > 26 {
			base = base[:26]
		}
		username := base
		for i := 2; ; i++ {
			taken, _ := s.db.UsernameTaken(ctx, username)
			if !taken {
				break
			}
			username = fmt.Sprintf("%s%d", base, i)
		}
		display := claims.Name
		if display == "" {
			display = username
		}
		u, err := s.db.RedeemAccountLink(ctx, o, username, display, "", s.cfg.DefaultQuotaBytes, st.Provider, idt.Subject, claims.Email)
		if err != nil {
			s.oidcFail(w, r, dest, "could not create account")
			return
		}
		if err := s.startSession(ctx, w, u, methodOIDC); err != nil {
			s.oidcFail(w, r, dest, "internal error")
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}
