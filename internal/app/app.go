// Package app wires configuration, storage, services, and transports, and
// dispatches requests by Host between the app origin and the content domain.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/content"
	"github.com/mrRedSun/redrop/internal/httpapi"
	"github.com/mrRedSun/redrop/internal/janitor"
	"github.com/mrRedSun/redrop/internal/netutil"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/internal/web"
)

// Options are runtime inputs that do not come from REDROP_* configuration.
type Options struct {
	Dashboard fs.FS  // built SPA (index.html at root); nil disables it
	CLIDir    string // prebuilt CLI binaries for /install.sh
}

type App struct {
	cfg      *config.Config
	log      *slog.Logger
	db       *store.Store
	Accounts *accounts.Service
	Sharing  *sharing.Service
	API      *httpapi.API
	janitor  *janitor.Janitor
	handler  http.Handler
}

// New opens storage and builds every component.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger, opts Options) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	secret, err := loadSecret(cfg)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "redrop.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	blobs, err := blob.New(cfg.DataDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	sh, err := sharing.New(db, blobs, cfg, secret, log)
	if err != nil {
		db.Close()
		return nil, err
	}
	acc := accounts.New(ctx, db, cfg, secret, log)
	clients := &netutil.ClientResolver{TrustedProxies: cfg.TrustedProxies, IPHeader: cfg.ClientIPHeader, CountryHeader: cfg.CountryHeader}

	site := web.New(opts.Dashboard, opts.CLIDir, cfg.PublicOrigin())
	api := httpapi.New(acc, sh, cfg, clients, log)
	installers := site.Installers()
	api.Mount("/install.sh", installers.ServeHTTP)
	api.Mount("/install.ps1", installers.ServeHTTP)
	api.Mount("/download/{file}", installers.ServeHTTP)
	api.Mount("/robots.txt", content.RobotsTxt)
	api.Mount("/", site.SPA)

	a := &App{
		cfg: cfg, log: log, db: db, Accounts: acc, Sharing: sh, API: api,
		janitor: janitor.New(sh, db, cfg.DataDir, log),
	}
	a.handler = hostRouter(cfg.ContentHost(), content.New(sh, cfg, clients, log, installers), api.Handler())
	return a, nil
}

func (a *App) Close() error { return a.db.Close() }

// Handler routes content-domain hosts to the content transport and every
// other host to the API/dashboard.
func (a *App) Handler() http.Handler { return a.handler }

func hostRouter(contentHost string, contentH, appH http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(host, ".")
		if host == contentHost || strings.HasSuffix(host, "."+contentHost) {
			contentH.ServeHTTP(w, r)
			return
		}
		appH.ServeHTTP(w, r)
	})
}

// Run serves HTTP and background jobs until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	if url, err := a.Accounts.Bootstrap(ctx); err != nil {
		return err
	} else if url != "" {
		a.log.Warn("no users exist yet; open this one-time link to create the admin account (valid 24h)", "url", url)
	}
	go a.janitor.Run(ctx)
	srv := &http.Server{
		Addr:              a.cfg.Listen,
		Handler:           a.handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		// No read/write timeouts: uploads and downloads can be large and slow.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	a.log.Info("redrop listening", "addr", a.cfg.Listen, "public_url", a.cfg.PublicOrigin(), "content_url", a.cfg.ContentURL.String(), "version", httpapi.Version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// loadSecret reads the signing secret, generating one on first start.
func loadSecret(cfg *config.Config) ([]byte, error) {
	path := cfg.SecretFile
	if path == "" {
		path = filepath.Join(cfg.DataDir, "secret.key")
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		s := strings.TrimSpace(string(b))
		if len(s) < 32 {
			return nil, fmt.Errorf("%s: secret must be at least 32 characters", path)
		}
		return []byte(s), nil
	case !errors.Is(err, fs.ErrNotExist) || cfg.SecretFile != "":
		return nil, err
	}
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	s := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(s+"\n"), 0o600); err != nil {
		return nil, err
	}
	return []byte(s), nil
}
