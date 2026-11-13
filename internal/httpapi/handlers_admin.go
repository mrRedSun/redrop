package httpapi

import (
	"time"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

func (a *API) adminStats(c fuego.ContextNoBody) (api.Stats, error) {
	s, err := a.sharing.Stats(c.Context())
	if err != nil {
		return api.Stats{}, err
	}
	return api.Stats{Users: s.Users, Artifacts: s.Artifacts, ActiveArtifacts: s.Active, LogicalBytes: s.LogicalBytes,
		StoredBytes: s.StoredBytes, MaxTotalBytes: s.MaxTotalBytes, MaxVersionBytes: s.MaxVersionBytes}, nil
}

func (a *API) adminListUsers(c fuego.ContextNoBody) (api.UserList, error) {
	users, err := a.accounts.Users(c.Context())
	if err != nil {
		return api.UserList{}, err
	}
	out := api.UserList{Items: make([]api.User, 0, len(users))}
	for _, u := range users {
		out.Items = append(out.Items, userDTO(u, a.accounts.Usage(c.Context(), u.ID)))
	}
	return out, nil
}

func (a *API) adminUpdateUser(c fuego.ContextWithBody[api.AdminUpdateUserRequest]) (api.User, error) {
	body, err := c.Body()
	if err != nil {
		return api.User{}, err
	}
	u, err := a.accounts.UpdateUser(c.Context(), principalFrom(c.Context()), c.PathParam("id"), accounts.UserPatch{
		QuotaBytes: body.QuotaBytes, Disabled: body.Disabled, Role: body.Role, DisplayName: body.DisplayName,
	})
	if err != nil {
		return api.User{}, err
	}
	return userDTO(u, a.accounts.Usage(c.Context(), u.ID)), nil
}

func (a *API) adminResetLink(c fuego.ContextWithBody[api.AdminResetRequest]) (api.ResetLink, error) {
	body, err := c.Body()
	if err != nil {
		return api.ResetLink{}, err
	}
	url, exp, err := a.accounts.CreateResetLink(c.Context(), principalFrom(c.Context()).User.ID, c.PathParam("id"), body.ClearTOTP)
	if err != nil {
		return api.ResetLink{}, err
	}
	return api.ResetLink{URL: url, ExpiresAt: exp}, nil
}

func (a *API) adminListInvites(c fuego.ContextNoBody) (api.InviteList, error) {
	list, err := a.accounts.Invites(c.Context())
	if err != nil {
		return api.InviteList{}, err
	}
	out := api.InviteList{Items: make([]api.Invite, 0, len(list))}
	for _, o := range list {
		out.Items = append(out.Items, inviteDTO(o))
	}
	return out, nil
}

func (a *API) adminCreateInvite(c fuego.ContextWithBody[api.CreateInviteRequest]) (api.Invite, error) {
	body, err := c.Body()
	if err != nil {
		return api.Invite{}, err
	}
	var ttl time.Duration
	if body.TTL != "" {
		d, never, err := api.ParseTTL(body.TTL)
		if err != nil || never {
			return api.Invite{}, apperr.Invalid("invite ttl must be a duration like 7d")
		}
		ttl = d
	}
	o, url, err := a.accounts.CreateInvite(c.Context(), accounts.InviteRequest{
		CreatedBy: principalFrom(c.Context()).User.ID, Note: body.Note, Role: body.Role, QuotaBytes: body.QuotaBytes, TTL: ttl,
	})
	if err != nil {
		return api.Invite{}, err
	}
	out := inviteDTO(o)
	out.URL = url
	return out, nil
}

func (a *API) adminDeleteInvite(c fuego.ContextNoBody) (api.OK, error) {
	err := a.accounts.DeleteInvite(c.Context(), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}

func (a *API) adminListArtifacts(c fuego.ContextNoBody) (api.ArtifactList, error) {
	list, err := a.sharing.ListAll(c.Context(), c.QueryParam("q"), c.QueryParam("status"))
	if err != nil {
		return api.ArtifactList{}, err
	}
	return a.artifactList(list, true), nil
}

// AdminArtifactUpdate is the only change admins may make to others' artifacts.
type AdminArtifactUpdate struct {
	Revoked bool `json:"revoked"`
}

func (a *API) adminUpdateArtifact(c fuego.ContextWithBody[AdminArtifactUpdate]) (api.Artifact, error) {
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	m, err := a.sharing.AdminSetRevoked(c.Context(), principalFrom(c.Context()).User.Username, c.PathParam("id"), body.Revoked)
	if err != nil {
		return api.Artifact{}, err
	}
	return a.artifactDTO(m, true), nil
}

func (a *API) adminDeleteArtifact(c fuego.ContextNoBody) (api.OK, error) {
	err := a.sharing.AdminDelete(c.Context(), principalFrom(c.Context()).User.Username, c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}
