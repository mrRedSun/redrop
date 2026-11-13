package accounts

import (
	"time"

	"github.com/mrRedSun/redrop/internal/store"
)

// The types in this file are what the accounts service returns to
// transports. They never carry password hashes, TOTP secrets, or token hashes.

// User is an account with its storage usage.
type User struct {
	ID          string
	Username    string
	DisplayName string
	Role        string
	QuotaBytes  int64
	UsedBytes   int64
	Artifacts   int
	TOTPEnabled bool
	Disabled    bool
	CreatedAt   time.Time
}

// Me is the signed-in caller as the dashboard and CLI see it.
type Me struct {
	User       User
	AuthMethod string
	TokenName  string // set when authenticated by an API token
	CanAdmin   bool   // admin endpoints are usable with this session
}

// Token is an API token's metadata (never its secret or hash).
type Token struct {
	ID         string
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	Artifacts  int
}

// NewToken is a created token; Secret is shown once.
type NewToken struct {
	Token
	Secret string
}

// Identity is a linked single sign-on identity.
type Identity struct {
	ID        string
	Provider  string
	Label     string
	Email     string
	CreatedAt time.Time
}

// Invite is an invite link's metadata.
type Invite struct {
	ID         string
	Note       string
	Role       string
	QuotaBytes int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UsedAt     *time.Time
	UsedBy     string
}

// CreatedInvite is a new invite; URL carries the one-time secret and is only
// returned at creation.
type CreatedInvite struct {
	Invite
	URL string
}

// ResetLink is a one-time password reset URL.
type ResetLink struct {
	URL       string
	ExpiresAt time.Time
}

func userView(u *store.User, usage store.Usage) User {
	return User{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role,
		QuotaBytes: u.QuotaBytes, UsedBytes: usage.UsedBytes, Artifacts: usage.Artifacts,
		TOTPEnabled: u.TOTPEnabled, Disabled: u.Disabled, CreatedAt: u.CreatedAt,
	}
}

func tokenView(t *store.Token) Token {
	return Token{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt, Artifacts: t.Artifacts}
}

func inviteView(o *store.OneTime) Invite {
	return Invite{ID: o.ID, Note: o.Note, Role: o.Role, QuotaBytes: o.QuotaBytes, CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt, UsedAt: o.UsedAt, UsedBy: o.UsedByName}
}
