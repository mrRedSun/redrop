// Package sharing implements uploads, publishing, artifact lifecycle,
// versions, and the access rules for shared links. It is transport-agnostic.
package sharing

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	directSigner  *security.Signer // a visitor's choice of the direct, frameless view

	// publication serializes materialization, commit/rollback, and GC. Waiting
	// requests do no decompression and can leave through their context.
	publication   chan struct{}
	uploadMu      sync.Mutex
	activeUploads map[string]bool // chunks/publication/expiry never overlap for one upload

	unlockLimiter   *security.Limiter // per IP + artifact
	unlockIPLimiter *security.Limiter // per IP across artifacts
}

func New(db *store.Store, blobs *blob.Store, cfg *config.Config, secret []byte, log *slog.Logger) (*Service, error) {
	s := &Service{
		db: db, blobs: blobs, cfg: cfg, log: log,
		publication:     make(chan struct{}, 1),
		uploadDir:       filepath.Join(cfg.DataDir, "uploads"),
		accessSigner:    security.NewSigner(secret, "access"),
		previewSigner:   security.NewSigner(secret, "preview"),
		seenSigner:      security.NewSigner(secret, "seen"),
		directSigner:    security.NewSigner(secret, "direct-view"),
		unlockLimiter:   security.NewLimiter(5, 5*time.Minute),
		unlockIPLimiter: security.NewLimiter(60, time.Hour),
	}
	if err := os.MkdirAll(s.uploadDir, 0o700); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close stops the rate limiters' background sweepers. The service stays
// usable; call it once requests have drained, at shutdown.
func (s *Service) Close() {
	s.unlockLimiter.Close()
	s.unlockIPLimiter.Close()
}

func mapStore(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("not found")
	case errors.Is(err, store.ErrUploadLimit):
		return apperr.TooLarge("too many open upload sessions; finish an upload or wait for expiry")
	case errors.Is(err, store.ErrQuota):
		return apperr.TooLarge("storage quota exceeded (your quota or the server total); delete or let old artifacts expire")
	}
	return apperr.Internal(err)
}
