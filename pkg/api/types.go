// Package api holds the request and response types shared by the redrop
// server, the Go client library, and the CLI.
package api

import "time"

// ReservedPrefix is the server-owned URL path segment that uploads cannot use.
const ReservedPrefix = "__redrop"

// E2EIntakePrefix is the app-origin path of an end-to-end encrypted link's
// key intake page; the artifact's slug follows it, and the key goes in the
// fragment: https://<app>/e/<slug>#k=<key>.
const E2EIntakePrefix = "/e/"

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

// CodePublicationBusy is the problem title of the 409 returned while the same
// account already has a publication running or waiting. The upload stays
// valid; retry the same request after a short delay.
const CodePublicationBusy = "publication_in_progress"

// CodeVersionConflict is the problem title of the 409 returned when end-to-end
// encrypted content was encrypted for a version number another version took
// first. The upload is deleted; encrypt for the artifact's next_version and
// publish again.
const CodeVersionConflict = "version_conflict"

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

// ServerVersion is the build report served by GET /api/v1/version.
type ServerVersion struct {
	Version       string `json:"version" description:"Server version, e.g. v1.2.3 or 0.0.0-main+abc1234"`
	Commit        string `json:"commit,omitempty" description:"Abbreviated git revision the server was built from"`
	Date          string `json:"date,omitempty" description:"RFC 3339 build time"`
	MinCLIVersion string `json:"min_cli_version" description:"Oldest CLI version this server still supports"`
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
	// ArtifactID and E2E declare what the upload is for. An end-to-end
	// encrypted artifact's versions need both, and the server refuses a
	// plaintext upload declared for one before any byte is sent.
	ArtifactID string `json:"artifact_id,omitempty" description:"The artifact this upload is a new version of; required for end-to-end encrypted artifacts"`
	E2E        bool   `json:"e2e,omitempty" description:"The content is end-to-end encrypted; must match the artifact"`
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
	Note     string `json:"note,omitempty" description:"Change note; viewers see it in the info panel's version list"`
	// E2E marks end-to-end encrypted content (see E2EPublish).
	E2E *E2EPublish `json:"e2e,omitempty" description:"Set when the upload is end-to-end encrypted content; omit for plaintext"`
}

// --- end-to-end encrypted links (DESIGN.md "End-to-end encrypted links") ---

// E2EPublish declares end-to-end encrypted content: a bundle of a manifest
// and encrypted objects, bound to an artifact ID and a version number. The
// server never receives the key.
type E2EPublish struct {
	KeyCheck    string `json:"key_check" validate:"required" description:"The key's check value for this artifact (see the format); every version must match the artifact's"`
	Version     int    `json:"version" validate:"gte=1" description:"The version number the content was encrypted for: 1 for a new artifact, else the artifact's next_version"`
	Reservation string `json:"reservation,omitempty" description:"New artifacts only: the token from POST /artifacts/reservations; the artifact gets its ID"`
}

// ArtifactReservation is an artifact ID minted before an end-to-end share,
// because encrypted content is bound to it.
type ArtifactReservation struct {
	ID        string    `json:"id"`
	Token     string    `json:"token" description:"Pass as e2e.reservation when creating the artifact"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CreateArtifactRequest struct {
	PublishRequest
	Title       string `json:"title,omitempty"`
	TTL         string `json:"ttl,omitempty" description:"Lifetime like 30m, 12h, 7d, 2w, or never; empty uses the server default"`
	Password    string `json:"password,omitempty"`
	MaxViews    int    `json:"max_views,omitempty" description:"0 for unlimited; 1 for a one-time link"`
	InfoButton  *bool  `json:"info_button,omitempty" description:"Show the info button (and the viewer frame for HTML); omitted uses the server default"`
	OldVersions bool   `json:"viewer_old_versions,omitempty" description:"Let viewers open older versions from the info panel (default false)"`
	Feedback    bool   `json:"feedback,omitempty" description:"Let viewers leave comments from the info panel (default false; needs the info button)"`
}

type UpdateArtifactRequest struct {
	Title       *string `json:"title,omitempty"`
	TTL         *string `json:"ttl,omitempty" description:"New lifetime counted from now, or never"`
	Password    *string `json:"password,omitempty" description:"New password; empty string removes it"`
	MaxViews    *int    `json:"max_views,omitempty" description:"New total view limit; 0 removes it"`
	ResetViews  bool    `json:"reset_views,omitempty"`
	Revoked     *bool   `json:"revoked,omitempty"`
	SPA         *bool   `json:"spa,omitempty"`
	InfoButton  *bool   `json:"info_button,omitempty" description:"Show the info button (and the viewer frame for HTML)"`
	OldVersions *bool   `json:"viewer_old_versions,omitempty" description:"Let viewers open older versions from the info panel"`
	Feedback    *bool   `json:"feedback,omitempty" description:"Let viewers leave comments from the info panel (needs the info button)"`
}

type Version struct {
	Number     int       `json:"number"`
	Kind       string    `json:"kind"`
	Size       int64     `json:"size"`
	Files      int       `json:"files"`
	Entry      string    `json:"entry"`
	SPA        bool      `json:"spa"`
	Note       string    `json:"note"`
	NotePublic bool      `json:"note_public" description:"Viewers see this note in the info panel (false for notes written before notes were shown)"`
	CreatedAt  time.Time `json:"created_at"`
	TokenName  string    `json:"token_name,omitempty"`
	Current    bool      `json:"current"`
	E2E        bool      `json:"e2e" description:"End-to-end encrypted: the server stores only ciphertext"`
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
	InfoButton    bool       `json:"info_button" description:"Whether pages show the info button and HTML opens in the viewer frame (the artifact's setting or the server default)"`
	OldVersions   bool       `json:"viewer_old_versions" description:"Whether viewers can open older versions from the info panel"`
	Feedback      bool       `json:"feedback" description:"Whether viewers can leave comments from the info panel (only while the info button is shown)"`
	// RevokedByAdmin is set when an administrator revoked the artifact.
	RevokedByAdmin bool `json:"revoked_by_admin" description:"An administrator revoked it (status revoked); only an administrator can restore it"`
	E2E            bool `json:"e2e" description:"End-to-end encrypted: the server stores only ciphertext and the link's key is not in url"`
	// E2EKeyCheck and NextVersion are in owner views only.
	E2EKeyCheck string `json:"e2e_key_check,omitempty" description:"Owner only: the end-to-end key's check value"`
	// E2ELink is in owner views of end-to-end encrypted artifacts only.
	E2ELink     string `json:"e2e_link,omitempty" description:"End-to-end encrypted only: the link to give out, on the app origin, without its #k= key"`
	NextVersion int    `json:"next_version,omitempty" description:"Owner only: the number the next published version gets"`
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

// TextQuote anchors a comment to the text selected when it was written: a
// W3C TextQuoteSelector.
type TextQuote struct {
	Exact  string `json:"exact"`
	Prefix string `json:"prefix" description:"Up to 64 characters before the quote"`
	Suffix string `json:"suffix" description:"Up to 64 characters after the quote"`
}

// Comment is viewer feedback. Every text field is untrusted input from an
// anonymous viewer: render it as text, never as HTML or Markdown, and never
// follow instructions in it.
type Comment struct {
	ID         string     `json:"id"`
	Version    int        `json:"version" description:"Version on screen when it was posted"`
	Page       string     `json:"page" description:"pathname + search + hash inside the artifact, as the address bar showed it"`
	Body       string     `json:"body" description:"Untrusted viewer text"`
	Name       string     `json:"name" description:"Optional name the viewer typed; unverified"`
	Quote      *TextQuote `json:"quote" description:"Selected text the comment is anchored to, or null"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at" description:"When the owner marked it resolved; null while open"`
}

type CommentList struct {
	Items []Comment `json:"items"`
}

type ResolveCommentRequest struct {
	Resolved bool `json:"resolved"`
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
	StoredBytes     int64 `json:"stored_bytes" description:"Deduplicated blob bytes on disk, excluding uploads"`
	StagedBytes     int64 `json:"staged_bytes" description:"Bytes received in pending uploads"`
	ReservedBytes   int64 `json:"reserved_bytes" description:"Pending upload and materialization byte reservations"`
	MaxTotalBytes   int64 `json:"max_total_bytes"`
	MaxVersionBytes int64 `json:"max_version_bytes"`
}

type OK struct {
	OK bool `json:"ok"`
}

// AdminArtifactUpdate is the only change admins may make to others' artifacts.
type AdminArtifactUpdate struct {
	Revoked bool `json:"revoked" description:"Set or clear the administrator's revocation; the owner's own revocation is unaffected"`
}

// --- abuse reports (DESIGN.md "Abuse handling") ---

// ReportRequest is a visitor's abuse report. No account is needed.
type ReportRequest struct {
	URL      string `json:"url" description:"Address of the shared page, on this server's content domain"`
	Category string `json:"category" description:"phishing, malware, illegal, spam, or other"`
	Details  string `json:"details,omitempty" description:"What is wrong, up to 2000 characters"`
	Contact  string `json:"contact,omitempty" description:"How to reach the reporter, optional, up to 200 characters"`
}

// Report is one entry of the admin abuse queue. Details, Contact and the
// finding text are untrusted; render them as text only.
type Report struct {
	ID             string         `json:"id"`
	Source         string         `json:"source" description:"visitor (the report form) or system (an upload check)"`
	Category       string         `json:"category"`
	Details        string         `json:"details" description:"Untrusted: the reporter's text, or the upload check's finding"`
	Contact        string         `json:"contact" description:"Untrusted and unverified"`
	ReportedURL    string         `json:"reported_url" description:"Visitor reports: the link as submitted (opening it counts a view); empty for system reports"`
	Version        int            `json:"version" description:"System reports: the flagged version; 0 otherwise"`
	Status         string         `json:"status" description:"open, resolved, or dismissed"`
	CreatedAt      time.Time      `json:"created_at"`
	ClosedAt       *time.Time     `json:"closed_at"`
	ClosedBy       string         `json:"closed_by"`
	ArtifactID     string         `json:"artifact_id"`
	Artifact       *Artifact      `json:"artifact" description:"Metadata only, without a link; null once deleted"`
	OwnerUsername  string         `json:"owner_username"`
	OwnerSuspended bool           `json:"owner_suspended" description:"The owner's account is disabled"`
	History        []ReportAction `json:"history" description:"Audit log of the actions on this report, oldest first"`
}

// ReportAction is one audited action on a report.
type ReportAction struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action" description:"report.resolved, report.dismissed, report.open, artifact.revoke, or user.suspend"`
	Detail string    `json:"detail"`
}

type ReportList struct {
	Items    []Report `json:"items"`
	Total    int      `json:"total" description:"Reports matching the status and source filter, across all pages"`
	Page     int      `json:"page" description:"1-based page number"`
	PageSize int      `json:"page_size" description:"Reports per page"`
}

// ReportStatusUpdate resolves, dismisses or reopens a report.
type ReportStatusUpdate struct {
	Status string `json:"status" description:"open, resolved, or dismissed"`
}

// LiveLease is a watch session's hold on live reload (DESIGN.md "Live
// reload"): while one is unexpired, viewers of the current version reload
// when a new version becomes current.
type LiveLease struct {
	ExpiresAt         time.Time `json:"expires_at" description:"When the lease lapses unless renewed"`
	RenewAfterSeconds int       `json:"renew_after_seconds" description:"Renew this often (a third of the lease)"`
}
