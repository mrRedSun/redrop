package accounts

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/mail"
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
		s.userDisabled(u.ID)
	}
	// Suspending and unsuspending are queue-relevant actions: a suspended
	// owner's reports were auto-resolved, and undoing that must leave a
	// trace. user.suspend is written by the queue's own path; every disable
	// or re-enable from this page writes one row too.
	if patch.Disabled != nil {
		action, detail := "user.suspend", "disabled the account"
		if !*patch.Disabled {
			action, detail = "user.unsuspend", "enabled the account"
		}
		if err := s.db.WriteAudit(ctx, store.AuditEntry{Actor: admin.user.Username, Action: action, UserID: u.ID, Detail: detail}); err != nil {
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
// empty means the default (7 days); "never" is rejected. Email, when not
// empty, is where the invite link is sent; it must be a bare address.
type InviteRequest struct {
	Note       string
	Role       string
	QuotaBytes int64
	TTL        string
	Email      string
}

// CreateInvite returns the invite and its one-time URL. createdBy is empty
// for invites minted offline from the server command line. With an email
// address (and mail configured), the link is queued for delivery to it.
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
	var email string
	if req.Email != "" {
		if a.s.mail == nil {
			return CreatedInvite{}, apperr.Invalid("mail is not configured on this server; create the invite without an email")
		}
		email, err = mail.ValidAddress(req.Email)
		if err != nil {
			return CreatedInvite{}, apperr.Invalid("email must be %v", err)
		}
	}
	o := &store.OneTime{Purpose: store.PurposeInvite, CreatedBy: createdBy, Role: req.Role, QuotaBytes: req.QuotaBytes,
		Note: truncate(strings.TrimSpace(req.Note), 200), Email: email, ExpiresAt: a.s.now().Add(ttl).UTC().Truncate(time.Second)}
	if email == "" {
		url, err := a.s.createLink(ctx, o)
		return CreatedInvite{Invite: inviteView(o), URL: url}, err
	}
	inviter := "the server administrator"
	if createdBy != "" {
		u, err := a.s.db.UserByID(ctx, createdBy)
		if err != nil {
			return CreatedInvite{}, apperr.Internal(err)
		}
		inviter = u.Username
		if u.DisplayName != "" {
			inviter = u.DisplayName + " (" + u.Username + ")"
		}
	}
	secret := ids.Secret()
	url := a.s.inviteURLFrom(o, secret)
	m, err := (mail.InviteEmail{Link: url, Note: o.Note, Inviter: inviter, Expires: o.ExpiresAt, Role: o.Role}).Render()
	if err != nil {
		return CreatedInvite{}, apperr.Internal(err)
	}
	m.To = email
	row, err := a.s.mail.Prepare(m, "invite", "")
	if err != nil {
		return CreatedInvite{}, apperr.Internal(err)
	}
	if err = a.s.db.CreateMailedInvite(ctx, o, ids.Hash(secret), createdBy, row, a.s.now()); err != nil {
		return CreatedInvite{}, mapMailError(err)
	}
	a.s.mail.Wake()
	v := inviteView(o)
	v.EmailStatus = store.MailQueued
	return CreatedInvite{Invite: v, URL: url}, nil
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
	statuses, err := a.s.inviteMailStatuses(ctx, list)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	out := make([]Invite, 0, len(list))
	for _, o := range list {
		v := inviteView(o)
		if st, ok := statuses[o.ID]; ok {
			v.EmailStatus, v.EmailError = st.Status, st.Error
			if st.To != "" {
				v.Email = st.To
			}
			v.EmailSentAt = &st.UpdatedAt
		}
		out = append(out, v)
	}
	return out, nil
}

// ResendInvite queues the invite's email again: a copy of the stored mail
// row (the same one-time link, as first rendered), subject to the same
// throttle and bounce list.
func (a *Administration) ResendInvite(ctx context.Context, admin *Principal, id string) (Invite, error) {
	o, err := a.s.db.OneTimeByID(ctx, store.PurposeInvite, id)
	if err != nil {
		return Invite{}, mapStore(err)
	}
	if o.Email == "" {
		return Invite{}, apperr.Invalid("this invite has no email address")
	}
	if o.UsedAt != nil {
		return Invite{}, apperr.Conflict("this invite was already used")
	}
	if a.s.mail == nil {
		return Invite{}, apperr.Invalid("mail is not configured on this server")
	}
	if err := a.s.db.ResendInviteMail(ctx, id, admin.UserID(), a.s.now()); err != nil {
		return Invite{}, mapMailError(err)
	}
	a.s.mail.Wake()
	return a.invite(ctx, o)
}

// invite returns one invite's view with its mail status.
func (a *Administration) invite(ctx context.Context, o *store.OneTime) (Invite, error) {
	v := inviteView(o)
	statuses, err := a.s.inviteMailStatuses(ctx, []*store.OneTime{o})
	if err != nil {
		return Invite{}, apperr.Internal(err)
	}
	if st, ok := statuses[o.ID]; ok {
		v.EmailStatus, v.EmailError = st.Status, st.Error
		if st.To != "" {
			v.Email = st.To
		}
		v.EmailSentAt = &st.UpdatedAt
	}
	return v, nil
}

// inviteMailStatuses looks up the latest send state of the given invites.
func (s *Service) inviteMailStatuses(ctx context.Context, list []*store.OneTime) (map[string]store.OutgoingMail, error) {
	ids := make([]string, 0, len(list))
	for _, o := range list {
		if o.Email != "" {
			ids = append(ids, o.ID)
		}
	}
	if len(ids) == 0 {
		return map[string]store.OutgoingMail{}, nil
	}
	return s.db.MailStatusesForInvites(ctx, ids)
}

func mapMailError(err error) error {
	switch {
	case errors.Is(err, store.ErrMailRate):
		return apperr.RateLimited("invite email limit reached; at most 10 per administrator and 60 globally per hour").WithRetryAfter(time.Hour)
	case errors.Is(err, store.ErrMailFull):
		return apperr.RateLimited("mail queue is full; try again later").WithRetryAfter(time.Minute)
	case errors.Is(err, store.ErrMailBounced):
		return apperr.Conflict("this address permanently refused mail from this server")
	case errors.Is(err, store.ErrMailBusy):
		return apperr.Conflict("this invite email is already queued")
	default:
		return mapStore(err)
	}
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
