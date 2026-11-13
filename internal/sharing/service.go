// Package sharing implements uploads, publishing, artifact lifecycle,
// versions, and the access rules for shared links. It is transport-agnostic.
package sharing

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
)

// Actor is who performs an owner operation.
type Actor struct {
	UserID    string
	Username  string
	Quota     int64
	TokenID   string // set when acting through an API token
	TokenName string
}

type Service struct {
	db        *store.Store
	blobs     *blob.Store
	cfg       *config.Config
	log       *slog.Logger
	uploadDir string

	accessSigner  *security.Signer // visit grants after Open/unlock
	previewSigner *security.Signer // owner preview links and cookies
	seenSigner    *security.Signer // de-duplicates ungated views

	unlockLimiter   *security.Limiter // per IP + artifact
	unlockIPLimiter *security.Limiter // per IP across artifacts
}

func New(db *store.Store, blobs *blob.Store, cfg *config.Config, secret []byte, log *slog.Logger) (*Service, error) {
	s := &Service{
		db: db, blobs: blobs, cfg: cfg, log: log,
		uploadDir:       filepath.Join(cfg.DataDir, "uploads"),
		accessSigner:    security.NewSigner(secret, "access"),
		previewSigner:   security.NewSigner(secret, "preview"),
		seenSigner:      security.NewSigner(secret, "seen"),
		unlockLimiter:   security.NewLimiter(5, 5*time.Minute),
		unlockIPLimiter: security.NewLimiter(60, time.Hour),
	}
	if err := os.MkdirAll(s.uploadDir, 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

// URL returns the public link of an artifact.
func (s *Service) URL(a *store.Artifact) string { return s.cfg.ArtifactURL(a.Slug) }

// PurgeAt returns when an expired or used-up artifact's content is deleted.
func (s *Service) PurgeAt(a *store.Artifact, status string) *time.Time {
	var t time.Time
	switch {
	case status == "expired" && a.ExpiresAt != nil:
		t = a.ExpiresAt.Add(s.cfg.ExpiredRetention)
	case status == "exhausted" && a.ExhaustedAt != nil:
		t = a.ExhaustedAt.Add(s.cfg.ViewWindowLimited)
	default:
		return nil
	}
	return &t
}

func mapStore(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("not found")
	case errors.Is(err, store.ErrQuota):
		return apperr.TooLarge("storage quota exceeded (your quota or the server total); delete or let old artifacts expire")
	}
	return apperr.Internal(err)
}
