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
	"github.com/mrRedSun/redrop/internal/brand"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/content"
	"github.com/mrRedSun/redrop/internal/dashpaths"
	"github.com/mrRedSun/redrop/internal/httpapi"
	"github.com/mrRedSun/redrop/internal/janitor"
	"github.com/mrRedSun/redrop/internal/mail"
	"github.com/mrRedSun/redrop/internal/netutil"
	"github.com/mrRedSun/redrop/internal/notify"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/internal/tunnel"
	"github.com/mrRedSun/redrop/internal/web"
	apitypes "github.com/mrRedSun/redrop/pkg/api"
)

// Options are runtime inputs that do not come from REDROP_* configuration.
type Options struct {
	Dashboard fs.FS  // built SPA (index.html at root); nil disables it
	CLIDir    string // prebuilt CLI binaries for /install.sh
	Offline   bool   // CLI administration and OpenAPI must not start mail delivery
}

type App struct {
	cfg      *config.Config
	log      *slog.Logger
	db       *store.Store
	Accounts *accounts.Service
	Sharing  *sharing.Service
	API      *httpapi.API
	Tunnels  *tunnel.Registry
	janitor  *janitor.Janitor
	notifier *notify.Webhook // nil without REDROP_NOTIFY_URL
	mail     *mail.Queue     // nil without REDROP_SMTP_ADDR
	handler  http.Handler
}

// New opens storage and builds every component.
func New(cfg *config.Config, log *slog.Logger, opts Options) (*App, error) {
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
	var webhook *notify.Webhook
	if cfg.NotifyURL != "" {
		webhook = notify.New(cfg.NotifyURL, cfg.NotifyToken, log)
		sh.SetNotifier(webhook)
	}
	// Outbound mail (DESIGN.md "Email"): one queue, one worker, persisted
	// send state. Report alerts batch on top of it; the webhook stays the
	// instant channel.
	var mailer mail.Mailer = mail.Discard{}
	var mailQueue *mail.Queue
	if cfg.MailEnabled() {
		sm, err := mail.NewSMTP(mail.SMTPConfig{
			Addr: cfg.SMTPAddr, TLS: mail.TLSMode(cfg.SMTPTLS), Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
			CAFile: cfg.SMTPCAFile, From: cfg.MailFrom, InsecurePlain: cfg.MailInsecurePlain,
		})
		if err == nil {
			mailer = sm
			mailQueue, err = mail.NewQueue(db, mailer, cfg.MailFrom, secret, log, mail.DefaultQueueConfig())
		}
		if err != nil {
			sh.Close()
			if webhook != nil {
				webhook.Close()
			}
			db.Close()
			return nil, err
		}
		mailQueue.SetAlerts(cfg.NotifyEmail, cfg.AlertInterval, cfg.PublicOrigin()+dashpaths.AdminReports)
		if !opts.Offline {
			if err = mailQueue.Start(); err != nil {
				sh.Close()
				if webhook != nil {
					webhook.Close()
				}
				db.Close()
				return nil, err
			}
		}
		if len(cfg.NotifyEmail) > 0 {
			sh.SetAlertNotifier(mailQueue)
		}
	}
	acc := accounts.New(db, cfg, secret, log)
	acc.SetMailer(mailQueue)
	clients := &netutil.ClientResolver{TrustedProxies: cfg.TrustedProxies, IPHeader: cfg.ClientIPHeader, CountryHeader: cfg.CountryHeader, TrustedHops: cfg.TrustedHops}
	// Live tunnel connections: the sharing service reports access changes
	// to them, accounts report disabled users and deleted tokens.
	tunnels := tunnel.New(cfg.Tunnels, log)
	sh.SetTunnels(tunnels)
	acc.SetRevocations(tunnels)

	site := web.New(opts.Dashboard, opts.CLIDir, cfg.PublicOrigin())
	api := httpapi.New(acc, sh, cfg, clients, log)
	api.SetTunnels(tunnels)
	installers := site.Installers()
	api.Mount("/install.sh", installers.ServeHTTP)
	api.Mount("/install.ps1", installers.ServeHTTP)
	api.Mount("/download/{file}", installers.ServeHTTP)
	api.Mount("/robots.txt", content.RobotsTxt)
	intake := content.NewIntake(cfg)
	api.Mount(apitypes.E2EIntakePrefix+"{slug}", intake.Page)
	api.Mount(apitypes.E2EIntakePrefix+"{slug}/{$}", intake.Page)
	api.Mount("/e2e/intake.js", intake.Script)
	api.Mount(content.SecurityTxtPath, content.SecurityTxt(cfg, cfg.PublicOrigin()))
	for _, p := range brand.Paths {
		api.Mount(p, brand.Handler)
	}
	api.Mount("/", site.SPA)

	a := &App{
		cfg: cfg, log: log, db: db, Accounts: acc, Sharing: sh, API: api, Tunnels: tunnels,
		janitor: janitor.New(sh, db, cfg.DataDir, log), notifier: webhook, mail: mailQueue,
	}
	a.handler = hostRouter(cfg.ContentHost(), content.New(sh, cfg, clients, log, installers).WithTunnels(tunnels), api.Handler())
	return a, nil
}

// Close releases what New built: the services' background goroutines and
// the notification and mail workers, then the database. Call it after Run
// has returned.
func (a *App) Close() error {
	a.Tunnels.Shutdown(tunnelShutdownWait)
	a.Tunnels.Close()
	a.Accounts.Close()
	a.Sharing.Close()
	if a.notifier != nil {
		a.notifier.Close()
	}
	if a.mail != nil {
		a.mail.Close()
	}
	return a.db.Close()
}

// tunnelShutdownWait bounds how long shutdown waits for tunnel connections
// to close after telling their CLIs to reconnect elsewhere.
const tunnelShutdownWait = 5 * time.Second

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
	for _, w := range a.cfg.Warnings() {
		a.log.Warn("insecure configuration", "warning", w)
	}
	go a.Accounts.WarmUp(ctx)
	go a.janitor.Run(ctx)
	srv := &http.Server{
		Addr:              a.cfg.Listen,
		Handler:           a.handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		// No read/write timeouts: uploads and downloads can be large and slow.
	}
	// Live reload streams never end on their own: end them when shutdown
	// starts, or Shutdown would wait for them until its timeout.
	srv.RegisterOnShutdown(a.Sharing.EndLiveStreams)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	a.log.Info("redrop listening", "addr", a.cfg.Listen, "public_url", a.cfg.PublicOrigin(), "content_url", a.cfg.ContentURL.String(), "version", httpapi.Version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// In-flight requests finish first, tunnelled ones included; the
		// tunnel connections are hijacked, so Shutdown doesn't wait for
		// them: they are told to reconnect (1001) once it returns.
		err := srv.Shutdown(sctx)
		a.Tunnels.Shutdown(tunnelShutdownWait)
		return err
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
