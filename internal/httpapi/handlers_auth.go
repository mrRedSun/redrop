package httpapi

import (
	"net/http"
	"net/url"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

var errNoRoute = apperr.NotFound("no such API endpoint")

// startSession sets the session cookie and returns the signed-in user.
func (a *API) startSession(w http.ResponseWriter, r *http.Request, s *accounts.Session) (api.Me, error) {
	me, err := a.accounts.Me(r.Context(), s.Principal)
	if err != nil {
		return api.Me{}, err
	}
	a.cookies.setSession(w, s)
	return meDTO(me), nil
}

func (a *API) getAuthConfig(c fuego.ContextNoBody) (api.AuthConfig, error) {
	setup, err := a.accounts.SetupRequired(c.Context())
	if err != nil {
		return api.AuthConfig{}, err
	}
	return api.AuthConfig{
		Version:          Version,
		SetupRequired:    setup,
		PasswordLogin:    a.cfg.PasswordLogin,
		Providers:        providersDTO(a.accounts.Providers()),
		AdminRequiresSSO: a.cfg.AdminRequiresSSO,
		PublicURL:        a.cfg.PublicOrigin(),
		ContentURL:       a.cfg.ContentURL.String(),
	}, nil
}

func (a *API) login(c fuego.ContextWithBody[api.LoginRequest]) (api.Me, error) {
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	s, err := a.accounts.Login(c.Context(), a.clients.IP(c.Request()), body.Username, body.Password, body.TOTP)
	if err != nil {
		return api.Me{}, err
	}
	return a.startSession(c.Response(), c.Request(), s)
}

func (a *API) logout(c fuego.ContextNoBody) (api.OK, error) {
	a.accounts.Logout(c.Context(), cookieValue(c.Request(), a.cookies.sessionName()))
	a.cookies.clearSession(c.Response())
	return api.OK{OK: true}, nil
}

func (a *API) setup(c fuego.ContextWithBody[api.SetupRequest]) (api.Me, error) {
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	s, err := a.accounts.Setup(c.Context(), a.clients.IP(c.Request()), body.Token, body.Username, body.Password)
	if err != nil {
		return api.Me{}, err
	}
	return a.startSession(c.Response(), c.Request(), s)
}

func (a *API) inviteInfo(c fuego.ContextNoBody) (api.InviteInfo, error) {
	info, err := a.accounts.InviteInfo(c.Context(), a.clients.IP(c.Request()), c.PathParam("token"))
	if err != nil {
		return api.InviteInfo{}, err
	}
	return api.InviteInfo{Valid: info.Valid, Role: info.Role, ExpiresAt: info.ExpiresAt, Providers: providersDTO(a.accounts.Providers())}, nil
}

func (a *API) acceptInvite(c fuego.ContextWithBody[api.AcceptInviteRequest]) (api.Me, error) {
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	s, err := a.accounts.AcceptInvite(c.Context(), a.clients.IP(c.Request()), c.PathParam("token"), body.Username, body.Password)
	if err != nil {
		return api.Me{}, err
	}
	return a.startSession(c.Response(), c.Request(), s)
}

func (a *API) resetInfo(c fuego.ContextNoBody) (api.ResetInfo, error) {
	info, err := a.accounts.ResetInfo(c.Context(), a.clients.IP(c.Request()), c.PathParam("token"))
	return api.ResetInfo{Valid: info.Valid, Username: info.Username}, err
}

func (a *API) resetPassword(c fuego.ContextWithBody[api.ResetPasswordRequest]) (api.OK, error) {
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	err = a.accounts.ResetPassword(c.Context(), a.clients.IP(c.Request()), c.PathParam("token"), body.Password)
	return api.OK{OK: err == nil}, err
}

// --- single sign-on (browser redirects, not JSON) ---

func redirectWithError(w http.ResponseWriter, r *http.Request, path string, err error) {
	http.Redirect(w, r, path+"?error="+url.QueryEscape(apperr.As(err).Msg), http.StatusSeeOther)
}

func (a *API) ssoStart(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = accounts.SSOLogin
	}
	var current *accounts.Principal
	if mode == accounts.SSOLink {
		current, _ = a.identify(r)
	}
	start, err := a.accounts.BeginSSO(r.Context(), a.clients.IP(r), r.PathValue("provider"), mode, current, r.URL.Query().Get("token"))
	if err != nil {
		redirectWithError(w, r, accounts.FailurePath(mode), err)
		return
	}
	a.cookies.setSSO(w, start.State)
	http.Redirect(w, r, start.RedirectURL, http.StatusFound)
}

func (a *API) ssoCallback(w http.ResponseWriter, r *http.Request) {
	state := cookieValue(r, a.cookies.ssoName())
	a.cookies.clearSSO(w)
	current, _ := a.identify(r)
	res, failPath, err := a.accounts.CompleteSSO(r.Context(), r.PathValue("provider"), state, r.URL.Query(), current)
	if err != nil {
		redirectWithError(w, r, failPath, err)
		return
	}
	a.cookies.setSession(w, res.Session)
	http.Redirect(w, r, res.Redirect, http.StatusSeeOther)
}
