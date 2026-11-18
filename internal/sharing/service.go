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
	"github.com/mrRedSun/redrop/internal/tunnel"
)

// Actor is who performs an owner operation.
type Actor struct {
	UserID    string
	Username  string
	Quota     int64
	TokenID   string // set when acting through an API token
	TokenName string
	// TokenExpires is when that token stops working (nil = never); a tunnel
	// it connects ends then.
	TokenExpires *time.Time
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
	publication      chan struct{}
	uploadMu         sync.Mutex
	activeUploads    map[string]bool // chunks/publication/expiry never overlap for one upload
	activePublishers map[string]bool // user IDs with a publication running or waiting

	unlockLimiter   *security.Limiter // per client network + artifact
	unlockIPLimiter *security.Limiter // per client network across artifacts
	unlockBudget    *security.Limiter // wrong passwords per artifact, from any network

	checkPassword func(hash, password string) bool // security.CheckPassword; tests replace it

	tunnels *tunnel.Registry // live tunnel connections (SetTunnels); nil = none
}

// maxLimiterKeys bounds each unlock limiter's table (about 10 MiB when
// full). The per-network tables evict their oldest entry when full: the
// per-artifact budget bounds guessing on its own. The budget refuses
// instead: it is that bound, and only a client that knows a link creates
// its key.
const maxLimiterKeys = 50_000

func New(db *store.Store, blobs *blob.Store, cfg *config.Config, secret []byte, log *slog.Logger) (*Service, error) {
	s := &Service{
		db: db, blobs: blobs, cfg: cfg, log: log,
		publication:     make(chan struct{}, 1),
		uploadDir:       filepath.Join(cfg.DataDir, "uploads"),
		accessSigner:    security.NewSigner(secret, "access"),
		previewSigner:   security.NewSigner(secret, "preview"),
		seenSigner:      security.NewSigner(secret, "seen"),
		directSigner:    security.NewSigner(secret, "direct-view"),
		unlockLimiter:   security.NewLimiter(5, 5*time.Minute, maxLimiterKeys, security.EvictOldest, time.Now),
		unlockIPLimiter: security.NewLimiter(60, time.Hour, maxLimiterKeys, security.EvictOldest, time.Now),
		unlockBudget:    security.NewLimiter(lockThreshold, time.Hour, maxLimiterKeys, security.RefuseNew, time.Now),
		checkPassword:   security.CheckPassword,
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
	s.unlockBudget.Close()
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
