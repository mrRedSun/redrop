package accounts

import (
	"context"
	"strings"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/pkg/api"
)

func validateTOTP(code, secret string) bool {
	return totp.Validate(strings.ReplaceAll(strings.TrimSpace(code), " ", ""), secret)
}

// UpdateDisplayName changes the caller's display name.
func (s *Service) UpdateDisplayName(ctx context.Context, p *Principal, name string) (Me, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 64 {
		return Me{}, apperr.Invalid("display name is too long")
	}
	p.user.DisplayName = name
	if err := s.db.UpdateUser(ctx, p.user); err != nil {
		return Me{}, mapStore(err)
	}
	return s.Me(ctx, p)
}

// ChangePassword verifies the current password (when one is set) and signs
// out every other session.
func (s *Service) ChangePassword(ctx context.Context, p *Principal, current, next string) error {
	if p.user.PasswordHash != "" && !security.CheckPassword(p.user.PasswordHash, current) {
		return apperr.Unauthorized("current password is wrong")
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	hash, err := security.HashPassword(next)
	if err != nil {
		return apperr.Internal(err)
	}
	if err := s.db.SetPassword(ctx, p.user.ID, hash); err != nil {
		return apperr.Internal(err)
	}
	return mapStore(s.db.DeleteUserSessions(ctx, p.user.ID, p.sessionHash))
}

// TOTPEnrollment is shown once while enabling two-factor authentication.
type TOTPEnrollment struct {
	Secret     string
	OTPAuthURL string
}

func (s *Service) BeginTOTP(ctx context.Context, p *Principal) (*TOTPEnrollment, error) {
	if p.user.TOTPEnabled {
		return nil, apperr.Conflict("authenticator is already enabled; disable it first")
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "redrop (" + s.cfg.PublicURL.Hostname() + ")", AccountName: p.user.Username})
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if err := s.db.SetTOTP(ctx, p.user.ID, key.Secret(), false); err != nil {
		return nil, apperr.Internal(err)
	}
	return &TOTPEnrollment{Secret: key.Secret(), OTPAuthURL: key.URL()}, nil
}

func (s *Service) EnableTOTP(ctx context.Context, p *Principal, code string) error {
	if p.user.TOTPEnabled || p.user.TOTPSecret == "" {
		return apperr.Conflict("start enrollment first")
	}
	if !validateTOTP(code, p.user.TOTPSecret) {
		return apperr.Invalid("wrong code; check your device clock")
	}
	return mapStore(s.db.SetTOTP(ctx, p.user.ID, p.user.TOTPSecret, true))
}

func (s *Service) DisableTOTP(ctx context.Context, p *Principal, code string) error {
	if !p.user.TOTPEnabled {
		return nil
	}
	if !validateTOTP(code, p.user.TOTPSecret) {
		return apperr.Invalid("wrong code")
	}
	return mapStore(s.db.SetTOTP(ctx, p.user.ID, "", false))
}

func (s *Service) Identities(ctx context.Context, p *Principal) ([]Identity, error) {
	list, err := s.db.ListIdentities(ctx, p.user.ID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	out := make([]Identity, 0, len(list))
	for _, i := range list {
		out = append(out, Identity{ID: i.ID, Provider: i.Provider, Label: s.providerLabel(i.Provider), Email: i.Email, CreatedAt: i.CreatedAt})
	}
	return out, nil
}

// UnlinkIdentity refuses to remove the last sign-in method.
func (s *Service) UnlinkIdentity(ctx context.Context, p *Principal, id string) error {
	if p.user.PasswordHash == "" {
		list, err := s.db.ListIdentities(ctx, p.user.ID)
		if err != nil {
			return apperr.Internal(err)
		}
		if len(list) <= 1 {
			return apperr.Conflict("set a password before removing your only sign-in method")
		}
	}
	return mapStore(s.db.UnlinkIdentity(ctx, p.user.ID, id))
}

// --- API tokens ---

func (s *Service) Tokens(ctx context.Context, p *Principal) ([]Token, error) {
	list, err := s.db.ListTokens(ctx, p.user.ID)
	if err != nil {
		return nil, mapStore(err)
	}
	out := make([]Token, 0, len(list))
	for i := range list {
		out = append(out, tokenView(&list[i]))
	}
	return out, nil
}

// CreateToken mints an API token. ttl is "" or "never" for no expiry, or a
// duration like 90d.
func (s *Service) CreateToken(ctx context.Context, p *Principal, name, ttl string) (NewToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return NewToken{}, apperr.Invalid("token name must be 1-64 characters")
	}
	var exp *time.Time
	if ttl = strings.TrimSpace(ttl); ttl != "" {
		d, never, err := api.ParseTTL(ttl)
		if err != nil {
			return NewToken{}, apperr.Invalid("%s", err.Error())
		}
		if !never {
			t := time.Now().Add(d).UTC().Truncate(time.Second)
			exp = &t
		}
	}
	secret := TokenPrefix + ids.Secret()
	t, err := s.db.CreateToken(ctx, p.user.ID, name, secret[:len(TokenPrefix)+6], ids.Hash(secret), exp)
	if err != nil {
		return NewToken{}, apperr.Internal(err)
	}
	return NewToken{Token: tokenView(t), Secret: secret}, nil
}

func (s *Service) RevokeToken(ctx context.Context, p *Principal, id string) error {
	return mapStore(s.db.DeleteToken(ctx, p.user.ID, id))
}
