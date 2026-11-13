package accounts

import (
	"context"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Administration is the admin-facing port of the accounts service: user
// management, invites, and reset links. Admin transports hold this instead of
// the full *Service.
type Administration struct{ s *Service }

// Administration returns the admin-facing port of the service.
func (s *Service) Administration() *Administration { return &Administration{s: s} }

// Users lists every account with its usage.
func (a *Administration) Users(ctx context.Context) ([]User, error) {
	rows, err := a.s.db.ListUsersWithUsage(ctx)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	out := make([]User, 0, len(rows))
	for _, r := range rows {
		out = append(out, userView(r.User, r.Usage))
	}
	return out, nil
}

// UserPatch changes admin-managed user fields (nil = unchanged).
type UserPatch struct {
	QuotaBytes  *int64
	Disabled    *bool
	Role        *string
	DisplayName *string
}

// UpdateUser applies a patch. Admins cannot demote or disable themselves, and
// the last active admin cannot be demoted or disabled. Disabling a user ends
// all of their sessions.
func (a *Administration) UpdateUser(ctx context.Context, admin *Principal, userID string, patch UserPatch) (User, error) {
	s := a.s
	u, err := s.db.UserByID(ctx, userID)
	if err != nil {
		return User{}, apperr.NotFound("user not found")
	}
	demoting := (patch.Role != nil && *patch.Role != api.RoleAdmin) || (patch.Disabled != nil && *patch.Disabled)
	if demoting && u.ID == admin.user.ID {
		return User{}, apperr.Conflict("you cannot demote or disable yourself")
	}
	if demoting && u.Role == api.RoleAdmin && !u.Disabled {
		n, err := s.db.CountAdmins(ctx)
		if err != nil {
			return User{}, apperr.Internal(err)
		}
		if n <= 1 {
			return User{}, apperr.Conflict("cannot remove the last admin")
		}
	}
	if patch.QuotaBytes != nil {
		if *patch.QuotaBytes < 0 {
			return User{}, apperr.Invalid("quota must be >= 0 (0 = only the server total applies)")
		}
		u.QuotaBytes = *patch.QuotaBytes
	}
	if patch.Disabled != nil {
		u.Disabled = *patch.Disabled
	}
	if patch.Role != nil {
		if *patch.Role != api.RoleAdmin && *patch.Role != api.RoleUser {
			return User{}, apperr.Invalid("role must be user or admin")
		}
		u.Role = *patch.Role
	}
	if patch.DisplayName != nil {
		u.DisplayName = truncate(strings.TrimSpace(*patch.DisplayName), 64)
	}
	if err := s.db.UpdateUser(ctx, u); err != nil {
		return User{}, apperr.Internal(err)
	}
	if u.Disabled {
		if err := s.db.DeleteUserSessions(ctx, u.ID, ""); err != nil {
			return User{}, apperr.Internal(err)
		}
	}
	s.log.Info("admin updated user", "admin", admin.user.Username, "user", u.Username)
	usage, err := s.db.UserUsage(ctx, u.ID)
	if err != nil {
		return User{}, apperr.Internal(err)
	}
	return userView(u, usage), nil
}

// InviteRequest describes a new invite. TTL is untrusted text like "7d";
// empty means the default (7 days); "never" is rejected.
type InviteRequest struct {
	Note       string
	Role       string
	QuotaBytes int64
	TTL        string
}

// CreateInvite returns the invite and its one-time URL. createdBy is empty
// for invites minted offline from the server command line.
func (a *Administration) CreateInvite(ctx context.Context, createdBy string, req InviteRequest) (CreatedInvite, error) {
	if req.Role == "" {
		req.Role = api.RoleUser
	}
	if req.Role != api.RoleUser && req.Role != api.RoleAdmin {
		return CreatedInvite{}, apperr.Invalid("role must be user or admin")
	}
	if req.QuotaBytes < 0 {
		return CreatedInvite{}, apperr.Invalid("quota must be >= 0")
	}
	if req.QuotaBytes == 0 {
		req.QuotaBytes = a.s.cfg.DefaultQuotaBytes
	}
	ttl, err := inviteTTL(req.TTL)
	if err != nil {
		return CreatedInvite{}, err
	}
	o := &store.OneTime{Purpose: store.PurposeInvite, CreatedBy: createdBy, Role: req.Role, QuotaBytes: req.QuotaBytes,
		Note: truncate(strings.TrimSpace(req.Note), 200), ExpiresAt: time.Now().Add(ttl).UTC().Truncate(time.Second)}
	url, err := a.s.createLink(ctx, o)
	if err != nil {
		return CreatedInvite{}, err
	}
	return CreatedInvite{Invite: inviteView(o), URL: url}, nil
}

// inviteTTL parses an invite lifetime; invites always expire.
func inviteTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return InviteTTL, nil
	}
	d, never, err := api.ParseTTL(raw)
	if err != nil || never || d <= 0 {
		return 0, apperr.Invalid("invite ttl must be a duration like 7d")
	}
	return d, nil
}

func (a *Administration) Invites(ctx context.Context) ([]Invite, error) {
	list, err := a.s.db.ListOneTime(ctx, store.PurposeInvite)
	if err != nil {
		return nil, mapStore(err)
	}
	out := make([]Invite, 0, len(list))
	for _, o := range list {
		out = append(out, inviteView(o))
	}
	return out, nil
}

func (a *Administration) DeleteInvite(ctx context.Context, id string) error {
	return mapStore(a.s.db.DeleteOneTime(ctx, store.PurposeInvite, id))
}

// CreateResetLink returns a 24-hour password reset URL for a user, optionally
// also clearing their authenticator when redeemed. createdBy is empty for
// links minted offline.
func (a *Administration) CreateResetLink(ctx context.Context, createdBy, userID string, clearTOTP bool) (ResetLink, error) {
	if _, err := a.s.db.UserByID(ctx, userID); err != nil {
		return ResetLink{}, apperr.NotFound("user not found")
	}
	exp := time.Now().Add(ResetTTL).UTC().Truncate(time.Second)
	url, err := a.s.createLink(ctx, &store.OneTime{Purpose: store.PurposeReset, UserID: userID, CreatedBy: createdBy, ClearTOTP: clearTOTP, ExpiresAt: exp})
	if err != nil {
		return ResetLink{}, err
	}
	return ResetLink{URL: url, ExpiresAt: exp}, nil
}

// UserIDByUsername resolves a username for offline administration.
func (a *Administration) UserIDByUsername(ctx context.Context, username string) (string, error) {
	u, err := a.s.db.UserByUsername(ctx, strings.ToLower(strings.TrimSpace(username)))
	if err != nil {
		return "", apperr.NotFound("user %q not found", username)
	}
	return u.ID, nil
}
