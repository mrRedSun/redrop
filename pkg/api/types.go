// Package api holds the request and response types shared by the redrop
// server, the Go client library, and the CLI.
package api

import "time"

// Artifact kinds.
const (
	KindHTML   = "html"
	KindFile   = "file"
	KindBundle = "bundle"
)

// Artifact statuses.
const (
	StatusActive    = "active"
	StatusExpired   = "expired"
	StatusExhausted = "exhausted"
	StatusRevoked   = "revoked"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

type AuthProvider struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type AuthConfig struct {
	Version          string         `json:"version"`
	SetupRequired    bool           `json:"setup_required"`
	PasswordLogin    bool           `json:"password_login"`
	Providers        []AuthProvider `json:"providers"`
	AdminRequiresSSO bool           `json:"admin_requires_sso"`
	PublicURL        string         `json:"public_url"`
	ContentURL       string         `json:"content_url"`
}

type LoginRequest struct {
	Username string `json:"username" validate:"required"`
	Password string `json:"password" validate:"required"`
	TOTP     string `json:"totp,omitempty"`
}

type SetupRequest struct {
	Token    string `json:"token" validate:"required"`
	Username string `json:"username" validate:"required"`
	Password string `json:"password" validate:"required"`
}

type User struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	QuotaBytes  int64     `json:"quota_bytes"`
	UsedBytes   int64     `json:"used_bytes"`
	TOTPEnabled bool      `json:"totp_enabled"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
	Artifacts   int       `json:"artifacts"`
}

type Me struct {
	User       User   `json:"user"`
	AuthMethod string `json:"auth_method" description:"password, oidc, or token"`
	TokenName  string `json:"token_name,omitempty"`
	CanAdmin   bool   `json:"can_admin" description:"true when admin endpoints are usable with this session"`
}

type UpdateMeRequest struct {
	DisplayName string `json:"display_name"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password" validate:"required"`
}

type TOTPSetup struct {
	Secret     string `json:"secret"`
	OTPAuthURL string `json:"otpauth_url"`
}

type TOTPCode struct {
	Code string `json:"code" validate:"required"`
}

type Identity struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Label     string    `json:"label"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Artifacts  int        `json:"artifacts"`
}

type CreateTokenRequest struct {
	Name string `json:"name" validate:"required"`
	TTL  string `json:"ttl,omitempty" description:"Token lifetime like 90d; empty or never for no expiry"`
}

type CreatedToken struct {
	Token
	Secret string `json:"secret" description:"Shown only once"`
}

type CreateUploadRequest struct {
	Size int64 `json:"size" validate:"gte=0"`
}

type Upload struct {
	ID        string    `json:"id"`
	Size      int64     `json:"size"`
	Received  int64     `json:"received"`
	ChunkSize int64     `json:"chunk_size"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PublishRequest finalizes an upload into a new artifact or a new version.
type PublishRequest struct {
	UploadID string `json:"upload_id" validate:"required"`
	SHA256   string `json:"sha256" validate:"required"`
	Kind     string `json:"kind" validate:"required,oneof=html file bundle"`
	Filename string `json:"filename,omitempty" description:"Original file name for html/file kinds"`
	Entry    string `json:"entry,omitempty" description:"Entry page for bundles (default index.html)"`
	SPA      bool   `json:"spa,omitempty" description:"Serve the entry page for unknown extension-less paths"`
	Note     string `json:"note,omitempty"`
}

type CreateArtifactRequest struct {
	PublishRequest
	Title    string `json:"title,omitempty"`
	TTL      string `json:"ttl,omitempty" description:"Lifetime like 30m, 12h, 7d, 2w, or never; empty uses the server default"`
	Password string `json:"password,omitempty"`
	MaxViews int    `json:"max_views,omitempty" description:"0 for unlimited; 1 for a one-time link"`
}

type UpdateArtifactRequest struct {
	Title      *string `json:"title,omitempty"`
	TTL        *string `json:"ttl,omitempty" description:"New lifetime counted from now, or never"`
	Password   *string `json:"password,omitempty" description:"New password; empty string removes it"`
	MaxViews   *int    `json:"max_views,omitempty" description:"New total view limit; 0 removes it"`
	ResetViews bool    `json:"reset_views,omitempty"`
	Revoked    *bool   `json:"revoked,omitempty"`
	SPA        *bool   `json:"spa,omitempty"`
}

type Version struct {
	Number    int       `json:"number"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	Files     int       `json:"files"`
	Entry     string    `json:"entry"`
	SPA       bool      `json:"spa"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	TokenName string    `json:"token_name,omitempty"`
	Current   bool      `json:"current"`
}

type Artifact struct {
	ID            string     `json:"id"`
	URL           string     `json:"url"`
	Title         string     `json:"title"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	Size          int64      `json:"size" description:"Current version size in bytes"`
	TotalSize     int64      `json:"total_size" description:"All retained versions in bytes"`
	Files         int        `json:"files"`
	Version       int        `json:"version"`
	Versions      int        `json:"versions"`
	HasPassword   bool       `json:"has_password"`
	MaxViews      int        `json:"max_views"`
	Views         int        `json:"views"`
	Previews      int        `json:"previews"`
	LastViewedAt  *time.Time `json:"last_viewed_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	PurgeAt       *time.Time `json:"purge_at" description:"When content is permanently removed"`
	TokenName     string     `json:"token_name,omitempty" description:"API token that created it"`
	OwnerUsername string     `json:"owner_username,omitempty" description:"Only in admin listings"`
}

type ArtifactList struct {
	Items []Artifact `json:"items"`
}

type FileEntry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

type FileList struct {
	Version int         `json:"version"`
	Items   []FileEntry `json:"items"`
}

type VersionList struct {
	Items []Version `json:"items"`
}

type View struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome" description:"view, preview, denied, or bad_password"`
	Country string    `json:"country"`
	Client  string    `json:"client"`
}

type ViewList struct {
	Items []View `json:"items"`
}

type PreviewLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type TokenList struct {
	Items []Token `json:"items"`
}

type IdentityList struct {
	Items []Identity `json:"items"`
}

type UserList struct {
	Items []User `json:"items"`
}

type AdminUpdateUserRequest struct {
	QuotaBytes  *int64  `json:"quota_bytes,omitempty"`
	Disabled    *bool   `json:"disabled,omitempty"`
	Role        *string `json:"role,omitempty" validate:"omitempty,oneof=admin user"`
	DisplayName *string `json:"display_name,omitempty"`
}

type Invite struct {
	ID         string     `json:"id"`
	Note       string     `json:"note"`
	Role       string     `json:"role"`
	QuotaBytes int64      `json:"quota_bytes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at"`
	UsedBy     string     `json:"used_by,omitempty"`
	URL        string     `json:"url,omitempty" description:"Only returned when created"`
}

type InviteList struct {
	Items []Invite `json:"items"`
}

type CreateInviteRequest struct {
	Note       string `json:"note"`
	Role       string `json:"role,omitempty" validate:"omitempty,oneof=admin user"`
	QuotaBytes int64  `json:"quota_bytes,omitempty"`
	TTL        string `json:"ttl,omitempty" description:"Invite validity, default 7d"`
}

type InviteInfo struct {
	Valid     bool           `json:"valid"`
	Role      string         `json:"role"`
	ExpiresAt time.Time      `json:"expires_at"`
	Providers []AuthProvider `json:"providers"`
}

type AcceptInviteRequest struct {
	Username string `json:"username" validate:"required"`
	Password string `json:"password" validate:"required"`
}

type ResetLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ResetInfo struct {
	Valid    bool   `json:"valid"`
	Username string `json:"username"`
}

type ResetPasswordRequest struct {
	Password string `json:"password" validate:"required"`
}

type AdminResetRequest struct {
	ClearTOTP bool `json:"clear_totp"`
}

type Stats struct {
	Users           int   `json:"users"`
	Artifacts       int   `json:"artifacts"`
	ActiveArtifacts int   `json:"active_artifacts"`
	LogicalBytes    int64 `json:"logical_bytes"`
	StoredBytes     int64 `json:"stored_bytes" description:"Deduplicated bytes on disk"`
	MaxTotalBytes   int64 `json:"max_total_bytes"`
	MaxVersionBytes int64 `json:"max_version_bytes"`
}

type OK struct {
	OK bool `json:"ok"`
}
