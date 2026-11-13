package sharing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

const (
	maxTitle     = 200
	maxNote      = 500
	maxViewLimit = 1_000_000
	previewTTL   = 5 * time.Minute
)

// Content describes an upload to publish as a version.
type Content struct {
	UploadID string
	SHA256   string
	Kind     string
	Filename string
	Entry    string
	SPA      bool
	Note     string
}

// Settings are the access settings of a new artifact.
type Settings struct {
	Title    string
	TTL      string // "" = server default, "never", or a duration
	Password string
	MaxViews int
}

// Patch changes an artifact's settings (nil = unchanged).
type Patch struct {
	Title      *string
	TTL        *string
	Password   *string // "" removes the password
	MaxViews   *int    // 0 removes the limit
	ResetViews bool
	Revoked    *bool
	SPA        *bool
}

// --- validation and policy ---

// expiry resolves a TTL string against server policy.
func (s *Service) expiry(ttl string) (*time.Time, error) {
	d, never := s.cfg.DefaultTTL, s.cfg.DefaultNever
	if strings.TrimSpace(ttl) != "" {
		var err error
		if d, never, err = api.ParseTTL(ttl); err != nil {
			return nil, apperr.Invalid("%s", err.Error())
		}
	}
	if never {
		if !s.cfg.AllowNever {
			return nil, apperr.Invalid("this server does not allow artifacts that never expire")
		}
		return nil, nil
	}
	if s.cfg.MaxTTL > 0 && d > s.cfg.MaxTTL {
		return nil, apperr.Invalid("ttl exceeds the server maximum of %s", api.FormatTTL(s.cfg.MaxTTL))
	}
	t := time.Now().Add(d).UTC().Truncate(time.Second)
	return &t, nil
}

func validateMaxViews(n int) error {
	if n < 0 || n > maxViewLimit {
		return apperr.Invalid("max_views must be between 0 and %d", maxViewLimit)
	}
	return nil
}

func hashArtifactPassword(pw string) (string, error) {
	if pw == "" {
		return "", nil
	}
	if len(pw) > 256 {
		return "", apperr.Invalid("password is too long")
	}
	h, err := security.HashPassword(pw)
	if err != nil {
		return "", apperr.Internal(err)
	}
	return h, nil
}

// --- publishing ---

// materialize verifies a completed upload and stores it as version files.
// It returns the version and a suggested title.
func (s *Service) materialize(ctx context.Context, actor Actor, c Content) (*store.NewVersion, string, error) {
	up, err := s.db.UploadByID(ctx, actor.UserID, c.UploadID)
	if err != nil {
		return nil, "", apperr.NotFound("upload not found or expired")
	}
	if up.Received != up.Size {
		return nil, "", apperr.Conflict("upload incomplete: %d of %d bytes received", up.Received, up.Size)
	}
	f, err := os.Open(s.stagedPath(up.ID))
	if err != nil {
		return nil, "", apperr.Internal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, "", apperr.Internal(err)
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), c.SHA256) {
		return nil, "", apperr.Invalid("sha256 mismatch: the upload was corrupted; retry")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, "", apperr.Internal(err)
	}

	v := &store.NewVersion{Kind: c.Kind, SPA: c.SPA, Note: truncate(strings.TrimSpace(c.Note), maxNote), TokenName: actor.TokenName}
	var title string
	switch c.Kind {
	case api.KindHTML:
		head := &headCapture{max: 64 << 10}
		sum, size, err := s.blobs.Put(io.TeeReader(f, head), s.cfg.MaxVersionBytes)
		if err != nil {
			return nil, "", s.blobErr(err)
		}
		v.Entry = "index.html"
		v.Files = []store.File{{Path: "index.html", Blob: sum, Size: size, ContentType: "text/html; charset=utf-8"}}
		title = htmlTitle(head.buf.Bytes())
	case api.KindFile:
		name := sanitizeFilename(c.Filename)
		head := &headCapture{max: 512}
		sum, size, err := s.blobs.Put(io.TeeReader(f, head), s.cfg.MaxVersionBytes)
		if err != nil {
			return nil, "", s.blobErr(err)
		}
		v.Entry = name
		v.Files = []store.File{{Path: name, Blob: sum, Size: size, ContentType: contentTypeFor(name, head.buf.Bytes())}}
		title = name
	case api.KindBundle:
		files, err := s.extractBundle(f)
		if err != nil {
			return nil, "", err
		}
		v.Files = files
		if v.Entry, err = bundleEntry(files, c.Entry); err != nil {
			return nil, "", err
		}
		if strings.HasSuffix(v.Entry, ".html") || strings.HasSuffix(v.Entry, ".htm") {
			for _, fl := range files {
				if fl.Path == v.Entry {
					title = s.blobTitle(fl.Blob)
				}
			}
		}
		if title == "" {
			title = strings.TrimSuffix(sanitizeFilename(c.Filename), ".tar.gz")
		}
	default:
		return nil, "", apperr.Invalid("kind must be html, file, or bundle")
	}
	f.Close()
	s.discardUpload(ctx, up.ID)
	return v, title, nil
}

// bundleEntry picks the start page: requested, else index.html, else none (listing).
func bundleEntry(files []store.File, requested string) (string, error) {
	want := "index.html"
	if requested != "" {
		want = strings.TrimPrefix(path.Clean("/"+requested), "/")
	}
	for _, f := range files {
		if f.Path == want {
			return want, nil
		}
	}
	if requested != "" {
		return "", apperr.Invalid("entry file %s is not in the bundle", want)
	}
	return "", nil
}

func (s *Service) blobErr(err error) error {
	if errors.Is(err, blob.ErrTooLarge) {
		return apperr.TooLarge("content exceeds the %s per-version limit", humanBytes(s.cfg.MaxVersionBytes))
	}
	return apperr.Internal(err)
}

func (s *Service) blobTitle(sum string) string {
	f, err := s.blobs.Open(sum)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	return htmlTitle(b)
}

// --- owner operations ---

// Create publishes an upload as a new artifact.
func (s *Service) Create(ctx context.Context, actor Actor, c Content, set Settings) (*store.Artifact, error) {
	exp, err := s.expiry(set.TTL)
	if err != nil {
		return nil, err
	}
	if err := validateMaxViews(set.MaxViews); err != nil {
		return nil, err
	}
	pwHash, err := hashArtifactPassword(set.Password)
	if err != nil {
		return nil, err
	}
	v, title, err := s.materialize(ctx, actor, c)
	if err != nil {
		return nil, err
	}
	if t := strings.TrimSpace(set.Title); t != "" {
		title = truncate(t, maxTitle)
	}
	id, err := s.db.CreateArtifact(ctx, &store.NewArtifact{
		UserID: actor.UserID, TokenID: actor.TokenID, TokenName: actor.TokenName,
		Title: title, PasswordHash: pwHash, MaxViews: set.MaxViews, ExpiresAt: exp,
	}, v, s.cfg.MaxTotalBytes)
	if err != nil {
		return nil, mapStore(err)
	}
	s.log.Info("artifact created", "id", id, "user", actor.Username, "kind", v.Kind, "token", actor.TokenName)
	return s.db.ArtifactByID(ctx, id)
}

// owned loads an artifact that belongs to the actor.
func (s *Service) owned(ctx context.Context, actor Actor, id string) (*store.Artifact, error) {
	a, err := s.db.ArtifactByID(ctx, id)
	if err != nil || a.UserID != actor.UserID {
		return nil, apperr.NotFound("artifact not found")
	}
	return a, nil
}

func (s *Service) Get(ctx context.Context, actor Actor, id string) (*store.Artifact, error) {
	return s.owned(ctx, actor, id)
}

// List returns the actor's artifacts, optionally filtered by status and query.
func (s *Service) List(ctx context.Context, actor Actor, query, status string) ([]*store.Artifact, error) {
	return s.list(ctx, store.ListFilter{UserID: actor.UserID, Query: strings.TrimSpace(query)}, status)
}

func (s *Service) list(ctx context.Context, f store.ListFilter, status string) ([]*store.Artifact, error) {
	all, err := s.db.ListArtifacts(ctx, f)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if status == "" || status == "all" {
		return all, nil
	}
	now := time.Now()
	out := all[:0]
	for _, a := range all {
		if a.Status(now) == status {
			out = append(out, a)
		}
	}
	return out, nil
}

// AddVersion publishes an upload as the new current version (same link).
func (s *Service) AddVersion(ctx context.Context, actor Actor, id string, c Content) (*store.Artifact, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	v, _, err := s.materialize(ctx, actor, c)
	if err != nil {
		return nil, err
	}
	n, err := s.db.AddVersion(ctx, a.ID, actor.UserID, v, s.cfg.MaxTotalBytes, s.cfg.KeepVersions)
	if err != nil {
		return nil, mapStore(err)
	}
	s.log.Info("artifact version published", "id", a.ID, "version", n, "user", actor.Username)
	return s.db.ArtifactByID(ctx, a.ID)
}

// Update applies a settings patch.
func (s *Service) Update(ctx context.Context, actor Actor, id string, p Patch) (*store.Artifact, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	var u store.ArtifactUpdate
	if p.Title != nil {
		t := truncate(strings.TrimSpace(*p.Title), maxTitle)
		u.Title = &t
	}
	if p.TTL != nil {
		if strings.TrimSpace(*p.TTL) == "" {
			return nil, apperr.Invalid("ttl must not be empty")
		}
		exp, err := s.expiry(*p.TTL)
		if err != nil {
			return nil, err
		}
		u.ExpiresAt = &exp
	}
	if p.Password != nil {
		h, err := hashArtifactPassword(*p.Password)
		if err != nil {
			return nil, err
		}
		u.PasswordHash = &h
	}
	if p.MaxViews != nil {
		if err := validateMaxViews(*p.MaxViews); err != nil {
			return nil, err
		}
		u.MaxViews = p.MaxViews
	}
	u.ResetViews, u.Revoked, u.SPA = p.ResetViews, p.Revoked, p.SPA
	if err := s.db.UpdateArtifact(ctx, a.ID, u); err != nil {
		return nil, mapStore(err)
	}
	return s.db.ArtifactByID(ctx, a.ID)
}

// Rotate replaces the secret link; the old URL stops working.
func (s *Service) Rotate(ctx context.Context, actor Actor, id string) (*store.Artifact, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if err := s.db.UpdateArtifact(ctx, a.ID, store.ArtifactUpdate{RotateSlug: true}); err != nil {
		return nil, mapStore(err)
	}
	return s.db.ArtifactByID(ctx, a.ID)
}

func (s *Service) Delete(ctx context.Context, actor Actor, id string) error {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return err
	}
	if err := s.db.DeleteArtifact(ctx, a.ID); err != nil {
		return mapStore(err)
	}
	s.log.Info("artifact deleted", "id", a.ID, "user", actor.Username)
	return nil
}

// PreviewLink is a short-lived owner link that bypasses gates and counters.
type PreviewLink struct {
	URL       string
	ExpiresAt time.Time
}

func (s *Service) Preview(ctx context.Context, actor Actor, id string) (*PreviewLink, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(previewTTL)
	tok := s.previewSigner.Sign(a.ID+"|"+strconv.Itoa(a.AccessGen), exp)
	return &PreviewLink{URL: s.URL(a) + ReservedPrefix + "/preview?t=" + tok, ExpiresAt: exp.UTC()}, nil
}

func (s *Service) Versions(ctx context.Context, actor Actor, id string) (*store.Artifact, []store.VersionRow, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, nil, err
	}
	list, err := s.db.ListVersions(ctx, a.ID)
	return a, list, mapStore(err)
}

func (s *Service) ActivateVersion(ctx context.Context, actor Actor, id string, number int) (*store.Artifact, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if err := s.db.SetCurrentVersion(ctx, a.ID, number); err != nil {
		return nil, mapStore(err)
	}
	return s.db.ArtifactByID(ctx, a.ID)
}

func (s *Service) DeleteVersion(ctx context.Context, actor Actor, id string, number int) error {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return err
	}
	if number == a.CurrentVersion {
		return apperr.Conflict("cannot delete the current version; make another version current first")
	}
	return mapStore(s.db.DeleteVersion(ctx, a.ID, number))
}

// Files lists a version's files (0 = current version).
func (s *Service) Files(ctx context.Context, actor Actor, id string, version int) (int, []store.File, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return 0, nil, err
	}
	if version <= 0 {
		version = a.CurrentVersion
	}
	files, err := s.db.ListFiles(ctx, a.ID, version)
	return version, files, mapStore(err)
}

const viewLogLimit = 200

func (s *Service) Views(ctx context.Context, actor Actor, id string) ([]store.ViewRow, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.ListViews(ctx, a.ID, viewLogLimit)
	return rows, mapStore(err)
}

// --- admin operations (metadata only) ---

func (s *Service) ListAll(ctx context.Context, query, status string) ([]*store.Artifact, error) {
	return s.list(ctx, store.ListFilter{Query: strings.TrimSpace(query)}, status)
}

func (s *Service) AdminSetRevoked(ctx context.Context, admin, id string, revoked bool) (*store.Artifact, error) {
	if err := s.db.UpdateArtifact(ctx, id, store.ArtifactUpdate{Revoked: &revoked}); err != nil {
		return nil, mapStore(err)
	}
	a, err := s.db.ArtifactByID(ctx, id)
	if err != nil {
		return nil, mapStore(err)
	}
	s.log.Info("admin changed artifact", "admin", admin, "id", id, "owner", a.OwnerUsername, "revoked", revoked)
	return a, nil
}

func (s *Service) AdminDelete(ctx context.Context, admin, id string) error {
	if err := s.db.DeleteArtifact(ctx, id); err != nil {
		return mapStore(err)
	}
	s.log.Info("admin deleted artifact", "admin", admin, "id", id)
	return nil
}

// Stats summarizes server usage.
type Stats struct {
	store.Stats
	StoredBytes     int64
	MaxTotalBytes   int64
	MaxVersionBytes int64
}

func (s *Service) Stats(ctx context.Context) (*Stats, error) {
	st, err := s.db.Stats(ctx)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	stored, _ := s.blobs.Usage()
	return &Stats{Stats: st, StoredBytes: stored, MaxTotalBytes: s.cfg.MaxTotalBytes, MaxVersionBytes: s.cfg.MaxVersionBytes}, nil
}
