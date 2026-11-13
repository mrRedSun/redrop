package httpapi

import (
	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/pkg/api"
)

func (a *API) getMe(c fuego.ContextNoBody) (api.Me, error) {
	return a.meDTO(c.Request(), principalFrom(c.Context())), nil
}

func (a *API) updateMe(c fuego.ContextWithBody[api.UpdateMeRequest]) (api.Me, error) {
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	p := principalFrom(c.Context())
	if err := a.accounts.UpdateDisplayName(c.Context(), p, body.DisplayName); err != nil {
		return api.Me{}, err
	}
	return a.meDTO(c.Request(), p), nil
}

func (a *API) changePassword(c fuego.ContextWithBody[api.ChangePasswordRequest]) (api.OK, error) {
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	err = a.accounts.ChangePassword(c.Context(), principalFrom(c.Context()), body.CurrentPassword, body.NewPassword)
	return api.OK{OK: err == nil}, err
}

func (a *API) totpSetup(c fuego.ContextNoBody) (api.TOTPSetup, error) {
	e, err := a.accounts.BeginTOTP(c.Context(), principalFrom(c.Context()))
	if err != nil {
		return api.TOTPSetup{}, err
	}
	return api.TOTPSetup{Secret: e.Secret, OTPAuthURL: e.OTPAuthURL}, nil
}

func (a *API) totpEnable(c fuego.ContextWithBody[api.TOTPCode]) (api.OK, error) {
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	err = a.accounts.EnableTOTP(c.Context(), principalFrom(c.Context()), body.Code)
	return api.OK{OK: err == nil}, err
}

func (a *API) totpDisable(c fuego.ContextWithBody[api.TOTPCode]) (api.OK, error) {
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	err = a.accounts.DisableTOTP(c.Context(), principalFrom(c.Context()), body.Code)
	return api.OK{OK: err == nil}, err
}

func (a *API) listIdentities(c fuego.ContextNoBody) (api.IdentityList, error) {
	list, err := a.accounts.Identities(c.Context(), principalFrom(c.Context()).User.ID)
	if err != nil {
		return api.IdentityList{}, err
	}
	out := api.IdentityList{Items: make([]api.Identity, 0, len(list))}
	for _, i := range list {
		out.Items = append(out.Items, api.Identity{ID: i.ID, Provider: i.Provider, Label: i.Label, Email: i.Email, CreatedAt: i.CreatedAt})
	}
	return out, nil
}

func (a *API) unlinkIdentity(c fuego.ContextNoBody) (api.OK, error) {
	err := a.accounts.UnlinkIdentity(c.Context(), principalFrom(c.Context()), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}

func (a *API) listTokens(c fuego.ContextNoBody) (api.TokenList, error) {
	list, err := a.accounts.Tokens(c.Context(), principalFrom(c.Context()).User.ID)
	if err != nil {
		return api.TokenList{}, err
	}
	out := api.TokenList{Items: make([]api.Token, 0, len(list))}
	for _, t := range list {
		out.Items = append(out.Items, tokenDTO(t))
	}
	return out, nil
}

func (a *API) createToken(c fuego.ContextWithBody[api.CreateTokenRequest]) (api.CreatedToken, error) {
	body, err := c.Body()
	if err != nil {
		return api.CreatedToken{}, err
	}
	t, err := a.accounts.CreateToken(c.Context(), principalFrom(c.Context()).User.ID, body.Name, body.TTL)
	if err != nil {
		return api.CreatedToken{}, err
	}
	return api.CreatedToken{Token: tokenDTO(*t.Token), Secret: t.Secret}, nil
}

func (a *API) deleteToken(c fuego.ContextNoBody) (api.OK, error) {
	err := a.accounts.RevokeToken(c.Context(), principalFrom(c.Context()).User.ID, c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}
