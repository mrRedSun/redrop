package accounts

import (
	"context"
	"strings"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

func validateTOTP(code, secret string) bool {
	return totp.Validate(strings.ReplaceAll(strings.TrimSpace(code), " ", ""), secret)
}

// UpdateDisplayName changes the caller's display name.
func (s *Service) UpdateDisplayName(ctx context.Context, p *Principal, name string) error {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 64 {
		return apperr.Invalid("display name is too long")
	}
	p.User.DisplayName = name
	return mapStore(s.db.UpdateUser(ctx, p.User))
}

// ChangePassword verifies the current password (when one is set) and signs
// out every other session.
func (s *Service) ChangePassword(ctx context.Context, p *Principal, current, next string) error {
	if p.User.PasswordHash != "" && !security.CheckPassword(p.User.PasswordHash, current) {
		return apperr.Unauthorized("current password is wrong")
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	hash, err := security.HashPassword(next)
	if err != nil {
		return apperr.Internal(err)
	}
	if err := s.db.SetPassword(ctx, p.User.ID, hash); err != nil {
		return apperr.Internal(err)
	}
	return mapStore(s.db.DeleteUserSessions(ctx, p.User.ID, p.SessionHash))
}

// TOTPEnrollment is shown once while enabling two-factor authentication.
type TOTPEnrollment struct {
	Secret     string
	OTPAuthURL string
}

func (s *Service) BeginTOTP(ctx context.Context, p *Principal) (*TOTPEnrollment, error) {
	if p.User.TOTPEnabled {
		return nil, apperr.Conflict("authenticator is already enabled; disable it first")
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "redrop (" + s.cfg.PublicURL.Hostname() + ")", AccountName: p.User.Username})
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if err := s.db.SetTOTP(ctx, p.User.ID, key.Secret(), false); err != nil {
		return nil, apperr.Internal(err)
	}
	return &TOTPEnrollment{Secret: key.Secret(), OTPAuthURL: key.URL()}, nil
}

func (s *Service) EnableTOTP(ctx context.Context, p *Principal, code string) error {
	if p.User.TOTPEnabled || p.User.TOTPSecret == "" {
		return apperr.Conflict("start enrollment first")
	}
	if !validateTOTP(code, p.User.TOTPSecret) {
		return apperr.Invalid("wrong code; check your device clock")
	}
	return mapStore(s.db.SetTOTP(ctx, p.User.ID, p.User.TOTPSecret, true))
}

func (s *Service) DisableTOTP(ctx context.Context, p *Principal, code string) error {
	if !p.User.TOTPEnabled {
		return nil
	}
	if !validateTOTP(code, p.User.TOTPSecret) {
		return apperr.Invalid("wrong code")
	}
	return mapStore(s.db.SetTOTP(ctx, p.User.ID, "", false))
}

// Identity is a linked SSO identity with its provider label.
type Identity struct {
	store.Identity
	Label string
}

func (s *Service) Identities(ctx context.Context, userID string) ([]Identity, error) {
	list, err := s.db.ListIdentities(ctx, userID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	out := make([]Identity, 0, len(list))
	for _, i := range list {
		out = append(out, Identity{Identity: i, Label: s.providerLabel(i.Provider)})
	}
	return out, nil
}

// UnlinkIdentity refuses to remove the last sign-in method.
func (s *Service) UnlinkIdentity(ctx context.Context, p *Principal, id string) error {
	if p.User.PasswordHash == "" {
		list, err := s.db.ListIdentities(ctx, p.User.ID)
		if err != nil {
			return apperr.Internal(err)
		}
		if len(list) <= 1 {
			return apperr.Conflict("set a password before removing your only sign-in method")
		}
	}
	return mapStore(s.db.UnlinkIdentity(ctx, p.User.ID, id))
}

// --- API tokens ---

// NewToken is a created token; Secret is shown once.
type NewToken struct {
	*store.Token
	Secret string
}

func (s *Service) Tokens(ctx context.Context, userID string) ([]store.Token, error) {
	list, err := s.db.ListTokens(ctx, userID)
	return list, mapStore(err)
}

func (s *Service) CreateToken(ctx context.Context, userID, name, ttl string) (*NewToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, apperr.Invalid("token name must be 1-64 characters")
	}
	var exp *time.Time
	if ttl = strings.TrimSpace(ttl); ttl != "" {
		d, never, err := api.ParseTTL(ttl)
		if err != nil {
			return nil, apperr.Invalid("%s", err.Error())
		}
		if !never {
			t := time.Now().Add(d).UTC().Truncate(time.Second)
			exp = &t
		}
	}
	secret := TokenPrefix + ids.Secret()
	t, err := s.db.CreateToken(ctx, userID, name, secret[:len(TokenPrefix)+6], ids.Hash(secret), exp)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return &NewToken{Token: t, Secret: secret}, nil
}

func (s *Service) RevokeToken(ctx context.Context, userID, id string) error {
	return mapStore(s.db.DeleteToken(ctx, userID, id))
}

// --- admin: users ---

func (s *Service) Users(ctx context.Context) ([]*store.User, error) {
	list, err := s.db.ListUsers(ctx)
	return list, mapStore(err)
}

// UserPatch changes admin-managed user fields (nil = unchanged).
type UserPatch struct {
	QuotaBytes  *int64
	Disabled    *bool
	Role        *string
	DisplayName *string
}

func (s *Service) UpdateUser(ctx context.Context, admin *Principal, userID string, patch UserPatch) (*store.User, error) {
	u, err := s.db.UserByID(ctx, userID)
	if err != nil {
		return nil, apperr.NotFound("user not found")
	}
	demoting := (patch.Role != nil && *patch.Role != api.RoleAdmin) || (patch.Disabled != nil && *patch.Disabled)
	if demoting && u.ID == admin.User.ID {
		return nil, apperr.Conflict("you cannot demote or disable yourself")
	}
	if demoting && u.Role == api.RoleAdmin {
		if n, _ := s.db.CountAdmins(ctx); n <= 1 {
			return nil, apperr.Conflict("cannot remove the last admin")
		}
	}
	if patch.QuotaBytes != nil {
		if *patch.QuotaBytes < 0 {
			return nil, apperr.Invalid("quota must be >= 0 (0 = only the server total applies)")
		}
		u.QuotaBytes = *patch.QuotaBytes
	}
	if patch.Disabled != nil {
		u.Disabled = *patch.Disabled
	}
	if patch.Role != nil {
		if *patch.Role != api.RoleAdmin && *patch.Role != api.RoleUser {
			return nil, apperr.Invalid("role must be user or admin")
		}
		u.Role = *patch.Role
	}
	if patch.DisplayName != nil {
		u.DisplayName = truncate(strings.TrimSpace(*patch.DisplayName), 64)
	}
	if err := s.db.UpdateUser(ctx, u); err != nil {
		return nil, apperr.Internal(err)
	}
	if u.Disabled {
		s.db.DeleteUserSessions(ctx, u.ID, "")
	}
	s.log.Info("admin updated user", "admin", admin.User.Username, "user", u.Username)
	return u, nil
}
