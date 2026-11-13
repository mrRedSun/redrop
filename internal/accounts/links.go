package accounts

import (
	"context"
	"github.com/mrRedSun/redrop/internal/dashpaths"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

var linkPaths = map[string]string{
	store.PurposeInvite: dashpaths.Invite,
	store.PurposeReset:  dashpaths.Reset,
	store.PurposeSetup:  dashpaths.Setup,
}

// createLink stores a one-time link and returns its absolute URL.
func (s *Service) createLink(ctx context.Context, o *store.OneTime) (string, error) {
	secret := ids.Secret()
	if err := s.db.CreateOneTime(ctx, o, ids.Hash(secret)); err != nil {
		return "", apperr.Internal(err)
	}
	return s.cfg.PublicOrigin() + linkPaths[o.Purpose] + secret, nil
}

// lookupLink finds an unused, unexpired one-time link (rate limited per IP).
func (s *Service) lookupLink(ctx context.Context, ip, purpose, token string) (*store.OneTime, error) {
	if !s.linkLimiter.Allow(ip) {
		return nil, apperr.RateLimited("too many requests")
	}
	o, err := s.db.OneTimeByHash(ctx, purpose, ids.Hash(token))
	if err != nil {
		return nil, apperr.Gone("this link was already used or has expired")
	}
	return o, nil
}

// Bootstrap returns a first-run setup URL when no users exist ("" otherwise).
func (s *Service) Bootstrap(ctx context.Context) (string, error) {
	n, err := s.db.CountUsers(ctx)
	if err != nil {
		return "", apperr.Internal(err)
	}
	if n > 0 {
		return "", nil
	}
	return s.createLink(ctx, &store.OneTime{Purpose: store.PurposeSetup, Role: api.RoleAdmin, ExpiresAt: time.Now().Add(setupTTL)})
}

// SetupRequired reports whether the first admin still has to be created.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.db.CountUsers(ctx)
	if err != nil {
		return false, apperr.Internal(err)
	}
	return n == 0, nil
}

// Setup redeems the first-run link and creates the first admin.
func (s *Service) Setup(ctx context.Context, ip, token, username, password string) (*Session, error) {
	o, err := s.lookupLink(ctx, ip, store.PurposeSetup, token)
	if err != nil {
		return nil, err
	}
	sess, err := s.redeemWithPassword(ctx, o, username, password)
	if err != nil {
		return nil, err
	}
	s.db.DeleteOpenSetupLinks(ctx)
	return sess, nil
}

// InviteInfo describes an invite link without consuming it.
type InviteInfo struct {
	Valid     bool
	Role      string
	ExpiresAt time.Time
}

func (s *Service) InviteInfo(ctx context.Context, ip, token string) (InviteInfo, error) {
	o, err := s.lookupLink(ctx, ip, store.PurposeInvite, token)
	if err != nil {
		if apperr.Is(err, apperr.KindRateLimited) {
			return InviteInfo{}, err
		}
		return InviteInfo{Valid: false}, nil
	}
	return InviteInfo{Valid: true, Role: o.Role, ExpiresAt: o.ExpiresAt}, nil
}

// AcceptInvite creates a password account from an invite.
func (s *Service) AcceptInvite(ctx context.Context, ip, token, username, password string) (*Session, error) {
	o, err := s.lookupLink(ctx, ip, store.PurposeInvite, token)
	if err != nil {
		return nil, err
	}
	return s.redeemWithPassword(ctx, o, username, password)
}

func (s *Service) redeemWithPassword(ctx context.Context, o *store.OneTime, username, password string) (*Session, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	if taken, _ := s.db.UsernameTaken(ctx, username); taken {
		return nil, apperr.Conflict("that username is taken")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	u, err := s.db.RedeemAccountLink(ctx, o, username, username, hash, s.cfg.DefaultQuotaBytes, "", "", "")
	if err != nil {
		return nil, mapStore(err)
	}
	s.log.Info("account created", "user", u.Username, "role", u.Role, "via", o.Purpose)
	return s.newSession(ctx, u, MethodPassword)
}

// ResetInfo describes a password reset link without consuming it.
type ResetInfo struct {
	Valid    bool
	Username string
}

func (s *Service) ResetInfo(ctx context.Context, ip, token string) (ResetInfo, error) {
	o, err := s.lookupLink(ctx, ip, store.PurposeReset, token)
	if err != nil {
		if apperr.Is(err, apperr.KindRateLimited) {
			return ResetInfo{}, err
		}
		return ResetInfo{}, nil
	}
	u, err := s.db.UserByID(ctx, o.UserID)
	if err != nil {
		return ResetInfo{}, nil
	}
	return ResetInfo{Valid: true, Username: u.Username}, nil
}

// ResetPassword redeems a reset link; all of the user's sessions end.
func (s *Service) ResetPassword(ctx context.Context, ip, token, password string) error {
	o, err := s.lookupLink(ctx, ip, store.PurposeReset, token)
	if err != nil {
		return err
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return apperr.Internal(err)
	}
	return mapStore(s.db.RedeemReset(ctx, o, hash))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
