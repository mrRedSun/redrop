package httpapi

import (
	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/pkg/api"
)

// adminAPI serves /api/v1/admin. It holds only the admin ports of the
// services, so no admin handler can reach an operation that returns an
// artifact's secret link (DESIGN.md: "Admin privacy").
type adminAPI struct {
	users      *accounts.Administration
	moderation *sharing.Moderation
}

func (h adminAPI) stats(c fuego.ContextNoBody) (api.Stats, error) {
	s, err := h.moderation.Stats(c.Context())
	if err != nil {
		return api.Stats{}, err
	}
	return statsDTO(s), nil
}

func (h adminAPI) listUsers(c fuego.ContextNoBody) (api.UserList, error) {
	users, err := h.users.Users(c.Context())
	if err != nil {
		return api.UserList{}, err
	}
	return api.UserList{Items: mapItems(users, userDTO)}, nil
}

func (h adminAPI) updateUser(c fuego.ContextWithBody[api.AdminUpdateUserRequest]) (api.User, error) {
	body, err := c.Body()
	if err != nil {
		return api.User{}, err
	}
	u, err := h.users.UpdateUser(c.Context(), principalFrom(c.Context()), c.PathParam("id"), userPatchFromRequest(body))
	if err != nil {
		return api.User{}, err
	}
	return userDTO(u), nil
}

func (h adminAPI) resetLink(c fuego.ContextWithBody[api.AdminResetRequest]) (api.ResetLink, error) {
	body, err := c.Body()
	if err != nil {
		return api.ResetLink{}, err
	}
	l, err := h.users.CreateResetLink(c.Context(), principalFrom(c.Context()).UserID(), c.PathParam("id"), body.ClearTOTP)
	if err != nil {
		return api.ResetLink{}, err
	}
	return api.ResetLink{URL: l.URL, ExpiresAt: l.ExpiresAt}, nil
}

func (h adminAPI) listInvites(c fuego.ContextNoBody) (api.InviteList, error) {
	list, err := h.users.Invites(c.Context())
	if err != nil {
		return api.InviteList{}, err
	}
	return api.InviteList{Items: mapItems(list, inviteDTO)}, nil
}

func (h adminAPI) createInvite(c fuego.ContextWithBody[api.CreateInviteRequest]) (api.Invite, error) {
	body, err := c.Body()
	if err != nil {
		return api.Invite{}, err
	}
	inv, err := h.users.CreateInvite(c.Context(), principalFrom(c.Context()).UserID(), inviteFromRequest(body))
	if err != nil {
		return api.Invite{}, err
	}
	out := inviteDTO(inv.Invite)
	out.URL = inv.URL
	return out, nil
}

func (h adminAPI) deleteInvite(c fuego.ContextNoBody) (api.OK, error) {
	err := h.users.DeleteInvite(c.Context(), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}

func (h adminAPI) listArtifacts(c fuego.ContextNoBody) (api.ArtifactList, error) {
	list, err := h.moderation.List(c.Context(), c.QueryParam("q"), c.QueryParam("status"))
	if err != nil {
		return api.ArtifactList{}, err
	}
	return api.ArtifactList{Items: mapItems(list, adminArtifactDTO)}, nil
}

func (h adminAPI) updateArtifact(c fuego.ContextWithBody[api.AdminArtifactUpdate]) (api.Artifact, error) {
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	a, err := h.moderation.SetRevoked(c.Context(), principalFrom(c.Context()).Username(), c.PathParam("id"), body.Revoked)
	if err != nil {
		return api.Artifact{}, err
	}
	return adminArtifactDTO(a), nil
}

func (h adminAPI) deleteArtifact(c fuego.ContextNoBody) (api.OK, error) {
	err := h.moderation.Delete(c.Context(), principalFrom(c.Context()).Username(), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}
