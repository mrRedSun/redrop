package sharing

import (
	"context"
	"time"

	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// The types in this file are what the sharing service returns to transports.
// Storage rows never leave the package through the owner or admin APIs, and
// every derived field (status, link, purge time) is computed here once.

// ArtifactMeta is what anyone allowed to see an artifact may see. It carries
// no secret: no link, slug, password hash, or access generation.
type ArtifactMeta struct {
	ID           string
	Title        string
	Kind         string
	Status       string
	Size         int64 // current version
	TotalSize    int64 // all retained versions
	Files        int
	Version      int
	Versions     int
	HasPassword  bool
	MaxViews     int
	Views        int
	Previews     int
	LastViewedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ExpiresAt    *time.Time
	PurgeAt      *time.Time // when expired or used-up content is deleted
	TokenName    string     // API token that created it
	InfoButton   bool       // effective: the artifact's setting or the server default
	OldVersions  bool       // viewers may open older versions
}

// Artifact is the owner's view: metadata plus the secret link.
type Artifact struct {
	ArtifactMeta
	URL string
}

// AdminArtifact is the moderation view. Admins see metadata only (DESIGN.md:
// "Admin privacy"); the type has no field that could carry the link.
type AdminArtifact struct {
	ArtifactMeta
	OwnerUsername string
}

// Version is one entry of an artifact's version history.
type Version struct {
	Number    int
	Kind      string
	Size      int64
	Files     int
	Entry     string
	SPA       bool
	Note      string
	TokenName string
	CreatedAt time.Time
	Current   bool
}

// FileEntry is one file of a version.
type FileEntry struct {
	Path        string
	Size        int64
	ContentType string
}

// FileList is the files of one version.
type FileList struct {
	Version int
	Items   []FileEntry
}

// View is one access log entry.
type View struct {
	At      time.Time
	Outcome string
	Country string
	Client  string
}

// Upload is a resumable upload in progress.
type Upload struct {
	ID        string
	Size      int64
	Received  int64
	ChunkSize int64
	ExpiresAt time.Time
}

// Stats summarizes server usage for administrators.
type Stats struct {
	Users           int
	Artifacts       int
	Active          int
	LogicalBytes    int64
	StoredBytes     int64
	MaxTotalBytes   int64
	MaxVersionBytes int64
}

func (s *Service) meta(a *store.Artifact, now time.Time) ArtifactMeta {
	status := a.Status(now)
	return ArtifactMeta{
		ID: a.ID, Title: a.Title, Kind: a.Kind, Status: status,
		Size: a.Size, TotalSize: a.TotalSize, Files: a.Files, Version: a.CurrentVersion, Versions: a.Versions,
		HasPassword: a.PasswordHash != "", MaxViews: a.MaxViews, Views: a.Views, Previews: a.Previews,
		LastViewedAt: a.LastViewedAt, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, ExpiresAt: a.ExpiresAt,
		PurgeAt: s.purgeAt(a, status), TokenName: a.TokenName, InfoButton: s.infoButton(a), OldVersions: a.OldVersions,
	}
}

func (s *Service) infoButton(a *store.Artifact) bool {
	if a.InfoButton != nil {
		return *a.InfoButton
	}
	return s.cfg.InfoButtonDefault
}

// ViewerInfo is what an artifact's info panel tells a visitor. DESIGN.md
// ("Info panel") fixes its contents: only what the visitor can already see
// or infer from the page they were let into, plus the owner's revision
// history (numbers, times and change notes). It has no owner, ID, slug, token
// name, view count of an unlimited link, or other versions' contents or sizes.
type ViewerInfo struct {
	Title     string
	Version   int        // the version shown
	Published time.Time  // when the shown version was published
	ExpiresAt *time.Time // nil = never
	MaxViews  int        // 0 = unlimited
	ViewsLeft int        // only meaningful when MaxViews > 0
	Password  bool
	Size      int64 // the shown version
	Files     int   // the shown version
	Current   int   // the current version's number
	Latest    int   // the highest retained version number ("version N of Latest")
	CanOpen   bool  // viewers may open older versions
	Revisions []Revision
}

// Revision is one retained version as viewers see it: never its size, files,
// kind, uploader or token.
type Revision struct {
	Number    int
	Published time.Time
	Note      string
	Current   bool
}

// ViewerInfo returns the info panel contents for shown, a version of a.
func (s *Service) ViewerInfo(ctx context.Context, a *store.Artifact, shown Snapshot) ViewerInfo {
	info := ViewerInfo{
		Title: a.Title, Version: shown.Version, Published: shown.Published, ExpiresAt: a.ExpiresAt,
		Password: a.PasswordHash != "", Size: shown.Size, Files: shown.Files,
		Current: a.CurrentVersion, Latest: a.CurrentVersion, CanOpen: s.oldVersionsOpen(a),
	}
	if a.MaxViews > 0 {
		info.MaxViews, info.ViewsLeft = a.MaxViews, max(a.MaxViews-a.Views, 0)
	}
	rows, err := s.db.ListVersions(ctx, a.ID)
	if err != nil {
		s.log.Error("list versions for viewer", "error", err)
	}
	for _, v := range rows {
		info.Revisions = append(info.Revisions, Revision{Number: v.Number, Published: v.CreatedAt, Note: v.Note, Current: v.Number == a.CurrentVersion})
		info.Latest = max(info.Latest, v.Number)
	}
	return info
}

func (s *Service) ownerView(a *store.Artifact) Artifact {
	return Artifact{ArtifactMeta: s.meta(a, time.Now()), URL: s.cfg.ArtifactURL(a.Slug)}
}

func (s *Service) adminView(a *store.Artifact) AdminArtifact {
	return AdminArtifact{ArtifactMeta: s.meta(a, time.Now()), OwnerUsername: a.OwnerUsername}
}

// purgeAt returns when an expired or used-up artifact's content is deleted.
func (s *Service) purgeAt(a *store.Artifact, status string) *time.Time {
	var t time.Time
	switch {
	case status == api.StatusExpired && a.ExpiresAt != nil:
		t = a.ExpiresAt.Add(s.cfg.ExpiredRetention)
	case status == api.StatusExhausted && a.ExhaustedAt != nil:
		t = a.ExhaustedAt.Add(s.cfg.ViewWindowLimited)
	default:
		return nil
	}
	return &t
}

func versionsView(current int, rows []store.VersionRow) []Version {
	out := make([]Version, 0, len(rows))
	for _, v := range rows {
		out = append(out, Version{Number: v.Number, Kind: v.Kind, Size: v.Size, Files: v.Files, Entry: v.Entry, SPA: v.SPA,
			Note: v.Note, TokenName: v.TokenName, CreatedAt: v.CreatedAt, Current: v.Number == current})
	}
	return out
}

func filesView(version int, files []store.File) FileList {
	out := FileList{Version: version, Items: make([]FileEntry, 0, len(files))}
	for _, f := range files {
		out.Items = append(out.Items, FileEntry{Path: f.Path, Size: f.Size, ContentType: f.ContentType})
	}
	return out
}

func viewsView(rows []store.ViewRow) []View {
	out := make([]View, 0, len(rows))
	for _, v := range rows {
		out = append(out, View{At: v.At, Outcome: v.Outcome, Country: v.Country, Client: v.Client})
	}
	return out
}

func (s *Service) uploadView(u *store.Upload) *Upload {
	return &Upload{ID: u.ID, Size: u.Size, Received: u.Received, ChunkSize: s.cfg.ChunkSize, ExpiresAt: u.ExpiresAt}
}
