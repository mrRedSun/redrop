package httpapi

import (
	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/pkg/api"
)

func (a *API) getMe(c fuego.ContextNoBody) (api.Me, error) {
	me, err := a.accounts.Me(c.Context(), principalFrom(c.Context()))
	if err != nil {
		return api.Me{}, err
	}
	return meDTO(me), nil
}

func (a *API) updateMe(c fuego.ContextWithBody[api.UpdateMeRequest]) (api.Me, error) {
	body, err := c.Body()
	if err != nil {
		return api.Me{}, err
	}
	me, err := a.accounts.UpdateDisplayName(c.Context(), principalFrom(c.Context()), body.DisplayName)
	if err != nil {
		return api.Me{}, err
	}
	return meDTO(me), nil
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
	list, err := a.accounts.Identities(c.Context(), principalFrom(c.Context()))
	if err != nil {
		return api.IdentityList{}, err
	}
	return api.IdentityList{Items: mapItems(list, identityDTO)}, nil
}

func (a *API) unlinkIdentity(c fuego.ContextNoBody) (api.OK, error) {
	err := a.accounts.UnlinkIdentity(c.Context(), principalFrom(c.Context()), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}

func (a *API) listTokens(c fuego.ContextNoBody) (api.TokenList, error) {
	list, err := a.accounts.Tokens(c.Context(), principalFrom(c.Context()))
	if err != nil {
		return api.TokenList{}, err
	}
	return api.TokenList{Items: mapItems(list, tokenDTO)}, nil
}

func (a *API) createToken(c fuego.ContextWithBody[api.CreateTokenRequest]) (api.CreatedToken, error) {
	body, err := c.Body()
	if err != nil {
		return api.CreatedToken{}, err
	}
	t, err := a.accounts.CreateToken(c.Context(), principalFrom(c.Context()), body.Name, body.TTL)
	if err != nil {
		return api.CreatedToken{}, err
	}
	return api.CreatedToken{Token: tokenDTO(t.Token), Secret: t.Secret}, nil
}

func (a *API) deleteToken(c fuego.ContextNoBody) (api.OK, error) {
	err := a.accounts.RevokeToken(c.Context(), principalFrom(c.Context()), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}
