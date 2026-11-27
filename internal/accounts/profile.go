package accounts

import (
	"context"
	"crypto/subtle"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// totpPeriod and totpSkew are the authenticator parameters: 30-second
// steps, and a code from one step before or after now is accepted.
const (
	totpPeriod = 30
	totpSkew   = 1
)

// totpStep returns the time step whose code matches, within totpSkew steps
// of now.
func totpStep(code, secret string, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	opts := totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	cur := now.Unix() / totpPeriod
	for step := cur - totpSkew; step <= cur+totpSkew; step++ {
		want, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0).UTC(), opts)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

type totpResult int

const (
	totpValid  totpResult = iota
	totpWrong             // not a current code
	totpReused            // a current code, but its step was already used
)

// useTOTP accepts a code once: the step it belongs to must be later than the
// last step this user's authenticator was accepted at, which it becomes.
func (s *Service) useTOTP(ctx context.Context, userID, secret, code string) (totpResult, error) {
	step, ok := totpStep(code, secret, s.now())
	if !ok {
		return totpWrong, nil
	}
	fresh, err := s.db.UseTOTPStep(ctx, userID, step)
	if err != nil {
		return totpWrong, apperr.Internal(err)
	}
	if !fresh {
		return totpReused, nil
	}
	return totpValid, nil
}

// checkTOTP verifies an authenticator code of u's enabled authenticator
// within the account's ceiling on wrong codes. The attempt is charged before
// the check, so concurrent guesses cannot all see the old count, and a
// valid code is refunded. A reused code counts as wrong.
func (s *Service) checkTOTP(ctx context.Context, u *store.User, code string) (totpResult, error) {
	if !s.totpLimiter.Allow(u.ID) {
		return totpWrong, apperr.RateLimited("too many wrong authenticator codes for this account; try again in an hour").WithRetryAfter(s.totpLimiter.Wait(u.ID))
	}
	res, err := s.useTOTP(ctx, u.ID, u.TOTPSecret, code)
	if err == nil && res == totpValid {
		s.totpLimiter.Refund(u.ID)
	}
	return res, err
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
	if res, err := s.useTOTP(ctx, p.user.ID, p.user.TOTPSecret, code); err != nil {
		return err
	} else if res != totpValid {
		return apperr.Invalid("wrong code; check your device clock")
	}
	return mapStore(s.db.SetTOTP(ctx, p.user.ID, p.user.TOTPSecret, true))
}

func (s *Service) DisableTOTP(ctx context.Context, p *Principal, code string) error {
	if !p.user.TOTPEnabled {
		return nil
	}
	switch res, err := s.checkTOTP(ctx, p.user, code); {
	case err != nil:
		return err
	case res == totpReused:
		return apperr.Invalid("that code was already used; wait for the next one")
	case res != totpValid:
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

// UnlinkIdentity refuses to remove the last sign-in method, and follows the
// admin single sign-on rule (mayUnlink).
func (s *Service) UnlinkIdentity(ctx context.Context, p *Principal, id string) error {
	if err := s.mayUnlink(p); err != nil {
		return err
	}
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
	if err := s.db.DeleteToken(ctx, p.user.ID, id); err != nil {
		return mapStore(err)
	}
	s.tokenDeleted(id)
	return nil
}
