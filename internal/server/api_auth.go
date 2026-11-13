package server

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-fuego/fuego"
	"github.com/pquerna/otp/totp"

	"github.com/mrRedSun/redrop/internal/auth"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

func normalizeUsername(u string) (string, error) {
	u = strings.ToLower(strings.TrimSpace(u))
	if !usernameRE.MatchString(u) {
		return "", errBadRequest("username must be 2-32 characters: lowercase letters, digits, dot, dash, underscore")
	}
	return u, nil
}

func (s *Server) getAuthConfig(c fuego.ContextNoBody) (api.AuthConfig, error) {
	n, err := s.db.CountUsers(c.Context())
	if err != nil {
		return api.AuthConfig{}, fail(c.Context(), err)
	}
	return api.AuthConfig{
		Version:          Version,
		SetupRequired:    n == 0,
		PasswordLogin:    s.cfg.PasswordLogin,
		Providers:        s.providerList(),
		AdminRequiresSSO: s.cfg.AdminRequiresSSO,
		PublicURL:        s.cfg.PublicOrigin(),
		ContentURL:       s.cfg.ContentURL.String(),
	}, nil
}

func (s *Server) login(c fuego.ContextWithBody[api.LoginRequest]) (api.Me, error) {
	ctx := c.Context()
	if !s.cfg.PasswordLogin {
		return api.Me{}, errForbidden("password login is disabled; use single sign-on")
	}
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	ip := s.clientIP(c.Request())
	username := strings.ToLower(strings.TrimSpace(body.Username))
	if !s.loginIPLimiter.Allow(ip) || !s.loginLimiter.Allow(ip+"|"+username) {
		return api.Me{}, errTooMany("too many sign-in attempts; wait a few minutes")
	}
	u, err := s.db.UserByUsername(ctx, username)
	hash := ""
	if err == nil {
		hash = u.PasswordHash
	}
	if !auth.CheckPassword(hash, body.Password) || u == nil || u.Disabled {
		return api.Me{}, errUnauthorized("wrong username or password")
	}
	if u.TOTPEnabled {
		if body.TOTP == "" {
			return api.Me{}, fuego.HTTPError{Status: 401, Title: "totp_required", Detail: "enter the code from your authenticator app"}
		}
		if !totp.Validate(strings.ReplaceAll(body.TOTP, " ", ""), u.TOTPSecret) {
			return api.Me{}, errUnauthorized("wrong authenticator code")
		}
	}
	if err := s.startSession(ctx, c.Response(), u, methodPass); err != nil {
		return api.Me{}, fail(ctx, err)
	}
	return s.meFor(ctx, &Principal{User: u, Method: methodPass})
}

func (s *Server) logout(c fuego.ContextNoBody) (api.OK, error) {
	if ck, err := c.Request().Cookie(s.sessionCookieName()); err == nil {
		s.db.DeleteSession(c.Context(), ids.Hash(ck.Value))
	}
	s.setSessionCookie(c.Response(), "", -1)
	return api.OK{OK: true}, nil
}

// createOneTime stores a one-time link and returns its URL and secret.
func (s *Server) createOneTime(ctx context.Context, o *store.OneTime, _ bool) (string, string, error) {
	secret := ids.Secret()
	if err := s.db.CreateOneTime(ctx, o, ids.Hash(secret)); err != nil {
		return "", "", err
	}
	path := map[string]string{store.PurposeInvite: "/invite/", store.PurposeReset: "/reset/", store.PurposeSetup: "/setup/"}[o.Purpose]
	return s.cfg.PublicOrigin() + path + secret, secret, nil
}

func (s *Server) lookupOneTime(c interface {
	Context() context.Context
	PathParam(string) string
}, ip, purpose, token string) (*store.OneTime, error) {
	if !s.publicLimiter.Allow(ip) {
		return nil, errTooMany("too many requests")
	}
	o, err := s.db.OneTimeByHash(c.Context(), purpose, ids.Hash(token))
	if err != nil {
		return nil, store.ErrLinkUsed
	}
	return o, nil
}

func (s *Server) setup(c fuego.ContextWithBody[api.SetupRequest]) (api.Me, error) {
	ctx := c.Context()
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	o, err := s.lookupOneTime(c, s.clientIP(c.Request()), store.PurposeSetup, body.Token)
	if err != nil {
		return api.Me{}, fail(ctx, err)
	}
	return s.redeemAccount(c.Context(), c.Response(), o, body.Username, body.Password)
}

func (s *Server) redeemAccount(ctx context.Context, w http.ResponseWriter, o *store.OneTime, username, password string) (api.Me, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return api.Me{}, err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return api.Me{}, errBadRequest(err.Error())
	}
	if taken, _ := s.db.UsernameTaken(ctx, username); taken {
		return api.Me{}, errConflict("that username is taken")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return api.Me{}, fail(ctx, err)
	}
	u, err := s.db.RedeemAccountLink(ctx, o, username, username, hash, s.cfg.DefaultQuotaBytes, "", "", "")
	if err != nil {
		return api.Me{}, fail(ctx, err)
	}
	if o.Purpose == store.PurposeSetup {
		s.db.DeleteOpenSetupLinks(ctx)
	}
	if err := s.startSession(ctx, w, u, methodPass); err != nil {
		return api.Me{}, fail(ctx, err)
	}
	return s.meFor(ctx, &Principal{User: u, Method: methodPass})
}

func (s *Server) inviteInfo(c fuego.ContextNoBody) (api.InviteInfo, error) {
	o, err := s.lookupOneTime(c, s.clientIP(c.Request()), store.PurposeInvite, c.PathParam("token"))
	if err != nil {
		if he := toHTTPError(c.Context(), err); he.Status == 429 {
			return api.InviteInfo{}, he
		}
		return api.InviteInfo{Valid: false, Providers: s.providerList()}, nil
	}
	return api.InviteInfo{Valid: true, Role: o.Role, ExpiresAt: o.ExpiresAt, Providers: s.providerList()}, nil
}

func (s *Server) acceptInvite(c fuego.ContextWithBody[api.AcceptInviteRequest]) (api.Me, error) {
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	o, err := s.lookupOneTime(c, s.clientIP(c.Request()), store.PurposeInvite, c.PathParam("token"))
	if err != nil {
		return api.Me{}, fail(c.Context(), err)
	}
	return s.redeemAccount(c.Context(), c.Response(), o, body.Username, body.Password)
}

func (s *Server) resetInfo(c fuego.ContextNoBody) (api.ResetInfo, error) {
	o, err := s.lookupOneTime(c, s.clientIP(c.Request()), store.PurposeReset, c.PathParam("token"))
	if err != nil {
		if he := toHTTPError(c.Context(), err); he.Status == 429 {
			return api.ResetInfo{}, he
		}
		return api.ResetInfo{Valid: false}, nil
	}
	u, err := s.db.UserByID(c.Context(), o.UserID)
	if err != nil {
		return api.ResetInfo{Valid: false}, nil
	}
	return api.ResetInfo{Valid: true, Username: u.Username}, nil
}

func (s *Server) resetPassword(c fuego.ContextWithBody[api.ResetPasswordRequest]) (api.OK, error) {
	ctx := c.Context()
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	o, err := s.lookupOneTime(c, s.clientIP(c.Request()), store.PurposeReset, c.PathParam("token"))
	if err != nil {
		return api.OK{}, fail(ctx, err)
	}
	if err := auth.ValidatePassword(body.Password); err != nil {
		return api.OK{}, errBadRequest(err.Error())
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		return api.OK{}, fail(ctx, err)
	}
	if err := s.db.RedeemReset(ctx, o, hash); err != nil {
		return api.OK{}, fail(ctx, err)
	}
	return api.OK{OK: true}, nil
}

// --- me ---

func (s *Server) userView(ctx context.Context, u *store.User) api.User {
	used, _ := s.db.UsedBytes(ctx, u.ID)
	n, _ := s.db.CountUserArtifacts(ctx, u.ID)
	return api.User{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role,
		QuotaBytes: u.QuotaBytes, UsedBytes: used, TOTPEnabled: u.TOTPEnabled,
		Disabled: u.Disabled, CreatedAt: u.CreatedAt, Artifacts: n,
	}
}

func (s *Server) meFor(ctx context.Context, p *Principal) (api.Me, error) {
	m := api.Me{User: s.userView(ctx, p.User), AuthMethod: p.Method, CanAdmin: p.CanAdmin(s.cfg.AdminRequiresSSO)}
	if p.Token != nil {
		m.TokenName = p.Token.Name
	}
	return m, nil
}

func (s *Server) getMe(c fuego.ContextNoBody) (api.Me, error) {
	return s.meFor(c.Context(), principal(c.Context()))
}

func (s *Server) updateMe(c fuego.ContextWithBody[api.UpdateMeRequest]) (api.Me, error) {
	ctx := c.Context()
	p := principal(ctx)
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	name := strings.TrimSpace(body.DisplayName)
	if len(name) > 64 {
		return api.Me{}, errBadRequest("display name is too long")
	}
	p.User.DisplayName = name
	if err := s.db.UpdateUser(ctx, p.User); err != nil {
		return api.Me{}, fail(ctx, err)
	}
	return s.meFor(ctx, p)
}

func (s *Server) changePassword(c fuego.ContextWithBody[api.ChangePasswordRequest]) (api.OK, error) {
	ctx := c.Context()
	p := principal(ctx)
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	if p.User.PasswordHash != "" && !auth.CheckPassword(p.User.PasswordHash, body.CurrentPassword) {
		return api.OK{}, errUnauthorized("current password is wrong")
	}
	if err := auth.ValidatePassword(body.NewPassword); err != nil {
		return api.OK{}, errBadRequest(err.Error())
	}
	hash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		return api.OK{}, fail(ctx, err)
	}
	if err := s.db.SetPassword(ctx, p.User.ID, hash); err != nil {
		return api.OK{}, fail(ctx, err)
	}
	s.db.DeleteUserSessions(ctx, p.User.ID, p.SessionHash)
	return api.OK{OK: true}, nil
}

func (s *Server) totpSetup(c fuego.ContextNoBody) (api.TOTPSetup, error) {
	ctx := c.Context()
	p := principal(ctx)
	if p.User.TOTPEnabled {
		return api.TOTPSetup{}, errConflict("authenticator is already enabled; disable it first")
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "redrop (" + s.cfg.PublicURL.Hostname() + ")", AccountName: p.User.Username})
	if err != nil {
		return api.TOTPSetup{}, fail(ctx, err)
	}
	if err := s.db.SetTOTP(ctx, p.User.ID, key.Secret(), false); err != nil {
		return api.TOTPSetup{}, fail(ctx, err)
	}
	return api.TOTPSetup{Secret: key.Secret(), OTPAuthURL: key.URL()}, nil
}

func (s *Server) totpEnable(c fuego.ContextWithBody[api.TOTPCode]) (api.OK, error) {
	ctx := c.Context()
	p := principal(ctx)
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	if p.User.TOTPSecret == "" || p.User.TOTPEnabled {
		return api.OK{}, errConflict("start enrollment first")
	}
	if !totp.Validate(strings.ReplaceAll(body.Code, " ", ""), p.User.TOTPSecret) {
		return api.OK{}, errBadRequest("wrong code; check your device clock")
	}
	return api.OK{OK: true}, fail(ctx, s.db.SetTOTP(ctx, p.User.ID, p.User.TOTPSecret, true))
}

func (s *Server) totpDisable(c fuego.ContextWithBody[api.TOTPCode]) (api.OK, error) {
	ctx := c.Context()
	p := principal(ctx)
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	if !p.User.TOTPEnabled {
		return api.OK{OK: true}, nil
	}
	if !totp.Validate(strings.ReplaceAll(body.Code, " ", ""), p.User.TOTPSecret) {
		return api.OK{}, errBadRequest("wrong code")
	}
	return api.OK{OK: true}, fail(ctx, s.db.SetTOTP(ctx, p.User.ID, "", false))
}

func (s *Server) listIdentities(c fuego.ContextNoBody) (api.IdentityList, error) {
	ctx := c.Context()
	list, err := s.db.ListIdentities(ctx, principal(ctx).User.ID)
	if err != nil {
		return api.IdentityList{}, fail(ctx, err)
	}
	out := api.IdentityList{Items: []api.Identity{}}
	for _, i := range list {
		label := i.Provider
		if p, ok := s.oidc[i.Provider]; ok {
			label = p.cfg.Label
		}
		out.Items = append(out.Items, api.Identity{ID: i.ID, Provider: i.Provider, Label: label, Email: i.Email, CreatedAt: i.CreatedAt})
	}
	return out, nil
}

func (s *Server) unlinkIdentity(c fuego.ContextNoBody) (api.OK, error) {
	ctx := c.Context()
	p := principal(ctx)
	if p.User.PasswordHash == "" {
		list, _ := s.db.ListIdentities(ctx, p.User.ID)
		if len(list) <= 1 {
			return api.OK{}, errConflict("set a password before removing your only sign-in method")
		}
	}
	return api.OK{OK: true}, fail(ctx, s.db.UnlinkIdentity(ctx, p.User.ID, c.PathParam("id")))
}

// --- tokens ---

func tokenView(t store.Token) api.Token {
	return api.Token{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt, Artifacts: t.Artifacts}
}

func (s *Server) listTokens(c fuego.ContextNoBody) (api.TokenList, error) {
	ctx := c.Context()
	list, err := s.db.ListTokens(ctx, principal(ctx).User.ID)
	if err != nil {
		return api.TokenList{}, fail(ctx, err)
	}
	out := api.TokenList{Items: []api.Token{}}
	for _, t := range list {
		out.Items = append(out.Items, tokenView(t))
	}
	return out, nil
}

func (s *Server) createToken(c fuego.ContextWithBody[api.CreateTokenRequest]) (api.CreatedToken, error) {
	ctx := c.Context()
	body, err := c.Body()
	if err != nil {
		return api.CreatedToken{}, err
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 64 {
		return api.CreatedToken{}, errBadRequest("token name must be 1-64 characters")
	}
	var exp *time.Time
	if body.TTL != "" {
		d, never, err := api.ParseTTL(body.TTL)
		if err != nil {
			return api.CreatedToken{}, errBadRequest(err.Error())
		}
		if !never {
			t := time.Now().Add(d).UTC().Truncate(time.Second)
			exp = &t
		}
	}
	secret := tokenPrefix + ids.Secret()
	t, err := s.db.CreateToken(ctx, principal(ctx).User.ID, name, secret[:len(tokenPrefix)+6], ids.Hash(secret), exp)
	if err != nil {
		return api.CreatedToken{}, fail(ctx, err)
	}
	return api.CreatedToken{Token: tokenView(*t), Secret: secret}, nil
}

func (s *Server) deleteToken(c fuego.ContextNoBody) (api.OK, error) {
	ctx := c.Context()
	return api.OK{OK: true}, fail(ctx, s.db.DeleteToken(ctx, principal(ctx).User.ID, c.PathParam("id")))
}
