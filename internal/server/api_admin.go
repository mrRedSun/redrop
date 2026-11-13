package server

import (
	"strings"
	"time"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

func (s *Server) adminStats(c fuego.ContextNoBody) (api.Stats, error) {
	ctx := c.Context()
	st, err := s.db.Stats(ctx)
	if err != nil {
		return api.Stats{}, fail(ctx, err)
	}
	stored, _ := s.blobs.Usage()
	return api.Stats{Users: st.Users, Artifacts: st.Artifacts, ActiveArtifacts: st.Active, LogicalBytes: st.LogicalBytes,
		StoredBytes: stored, MaxTotalBytes: s.cfg.MaxTotalBytes, MaxVersionBytes: s.cfg.MaxVersionBytes}, nil
}

func (s *Server) adminListUsers(c fuego.ContextNoBody) (api.UserList, error) {
	ctx := c.Context()
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return api.UserList{}, fail(ctx, err)
	}
	out := api.UserList{Items: []api.User{}}
	for _, u := range users {
		out.Items = append(out.Items, s.userView(ctx, u))
	}
	return out, nil
}

func (s *Server) adminUpdateUser(c fuego.ContextWithBody[api.AdminUpdateUserRequest]) (api.User, error) {
	ctx := c.Context()
	me := principal(ctx)
	u, err := s.db.UserByID(ctx, c.PathParam("id"))
	if err != nil {
		return api.User{}, errNotFound("user not found")
	}
	body, err := c.Body()
	if err != nil {
		return api.User{}, err
	}
	demoting := (body.Role != nil && *body.Role != api.RoleAdmin) || (body.Disabled != nil && *body.Disabled)
	if u.ID == me.User.ID && demoting {
		return api.User{}, errConflict("you cannot demote or disable yourself")
	}
	if u.Role == api.RoleAdmin && demoting {
		if n, _ := s.db.CountAdmins(ctx); n <= 1 {
			return api.User{}, errConflict("cannot remove the last admin")
		}
	}
	if body.QuotaBytes != nil {
		if *body.QuotaBytes < 0 {
			return api.User{}, errBadRequest("quota must be >= 0 (0 = unlimited within server total)")
		}
		u.QuotaBytes = *body.QuotaBytes
	}
	if body.Disabled != nil {
		u.Disabled = *body.Disabled
	}
	if body.Role != nil {
		u.Role = *body.Role
	}
	if body.DisplayName != nil {
		u.DisplayName = truncate(strings.TrimSpace(*body.DisplayName), 64)
	}
	if err := s.db.UpdateUser(ctx, u); err != nil {
		return api.User{}, fail(ctx, err)
	}
	if u.Disabled {
		s.db.DeleteUserSessions(ctx, u.ID, "")
	}
	s.log.Info("admin updated user", "admin", me.User.Username, "user", u.Username)
	return s.userView(ctx, u), nil
}

func (s *Server) adminResetLink(c fuego.ContextWithBody[api.AdminResetRequest]) (api.ResetLink, error) {
	ctx := c.Context()
	u, err := s.db.UserByID(ctx, c.PathParam("id"))
	if err != nil {
		return api.ResetLink{}, errNotFound("user not found")
	}
	body, err := c.Body()
	if err != nil {
		return api.ResetLink{}, err
	}
	exp := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	link, _, err := s.createOneTime(ctx, &store.OneTime{Purpose: store.PurposeReset, UserID: u.ID, CreatedBy: principal(ctx).User.ID, ClearTOTP: body.ClearTOTP, ExpiresAt: exp}, false)
	if err != nil {
		return api.ResetLink{}, fail(ctx, err)
	}
	return api.ResetLink{URL: link, ExpiresAt: exp}, nil
}

func inviteView(o *store.OneTime) api.Invite {
	return api.Invite{ID: o.ID, Note: o.Note, Role: o.Role, QuotaBytes: o.QuotaBytes, CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt, UsedAt: o.UsedAt, UsedBy: o.UsedByName}
}

func (s *Server) adminListInvites(c fuego.ContextNoBody) (api.InviteList, error) {
	ctx := c.Context()
	list, err := s.db.ListOneTime(ctx, store.PurposeInvite)
	if err != nil {
		return api.InviteList{}, fail(ctx, err)
	}
	out := api.InviteList{Items: []api.Invite{}}
	for _, o := range list {
		out.Items = append(out.Items, inviteView(o))
	}
	return out, nil
}

func (s *Server) adminCreateInvite(c fuego.ContextWithBody[api.CreateInviteRequest]) (api.Invite, error) {
	ctx := c.Context()
	body, err := c.Body()
	if err != nil {
		return api.Invite{}, err
	}
	ttl := 7 * 24 * time.Hour
	if body.TTL != "" {
		d, never, err := api.ParseTTL(body.TTL)
		if err != nil || never {
			return api.Invite{}, errBadRequest("invite ttl must be a duration like 7d")
		}
		ttl = d
	}
	role := body.Role
	if role == "" {
		role = api.RoleUser
	}
	if body.QuotaBytes < 0 {
		return api.Invite{}, errBadRequest("quota must be >= 0")
	}
	quota := body.QuotaBytes
	if quota == 0 {
		quota = s.cfg.DefaultQuotaBytes
	}
	o := &store.OneTime{Purpose: store.PurposeInvite, CreatedBy: principal(ctx).User.ID, Role: role, QuotaBytes: quota,
		Note: truncate(strings.TrimSpace(body.Note), 200), ExpiresAt: time.Now().Add(ttl).UTC().Truncate(time.Second)}
	link, _, err := s.createOneTime(ctx, o, false)
	if err != nil {
		return api.Invite{}, fail(ctx, err)
	}
	v := inviteView(o)
	v.URL = link
	return v, nil
}

func (s *Server) adminDeleteInvite(c fuego.ContextNoBody) (api.OK, error) {
	ctx := c.Context()
	return api.OK{OK: true}, fail(ctx, s.db.DeleteOneTime(ctx, store.PurposeInvite, c.PathParam("id")))
}

func (s *Server) adminListArtifacts(c fuego.ContextNoBody) (api.ArtifactList, error) {
	return s.artifactList(c.Context(), store.ListFilter{Query: strings.TrimSpace(c.QueryParam("q"))}, c.QueryParam("status"), true)
}

type adminArtifactUpdate struct {
	Revoked *bool `json:"revoked"`
}

func (s *Server) adminUpdateArtifact(c fuego.ContextWithBody[adminArtifactUpdate]) (api.Artifact, error) {
	ctx := c.Context()
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	id := c.PathParam("id")
	if err := s.db.UpdateArtifact(ctx, id, store.ArtifactUpdate{Revoked: body.Revoked}); err != nil {
		return api.Artifact{}, fail(ctx, err)
	}
	a, err := s.db.ArtifactByID(ctx, id)
	if err != nil {
		return api.Artifact{}, fail(ctx, err)
	}
	s.log.Info("admin changed artifact", "admin", principal(ctx).User.Username, "id", id, "owner", a.OwnerUsername)
	return s.artifactView(a, true), nil
}

func (s *Server) adminDeleteArtifact(c fuego.ContextNoBody) (api.OK, error) {
	ctx := c.Context()
	id := c.PathParam("id")
	if err := s.db.DeleteArtifact(ctx, id); err != nil {
		return api.OK{}, fail(ctx, err)
	}
	s.log.Info("admin deleted artifact", "admin", principal(ctx).User.Username, "id", id)
	return api.OK{OK: true}, nil
}
