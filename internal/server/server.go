// Package server wires the redrop HTTP API, dashboard, and content domain.
package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/auth"
	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/store"
)

// Version is set at build time.
var Version = "dev"

type Server struct {
	cfg   *config.Config
	db    *store.Store
	blobs *blob.Store
	log   *slog.Logger

	uploadDir string

	accessSigner  *auth.Signer // content access cookies
	previewSigner *auth.Signer // owner preview links
	stateSigner   *auth.Signer // OIDC state cookie
	seenSigner    *auth.Signer // view de-duplication cookie

	loginLimiter    *auth.Limiter // per IP+username
	loginIPLimiter  *auth.Limiter // per IP
	unlockLimiter   *auth.Limiter // per IP+artifact
	unlockIPLimiter *auth.Limiter // per IP across artifacts
	publicLimiter   *auth.Limiter // unauthenticated invite/reset/setup lookups

	oidc   map[string]*oidcProvider
	oidcMu sync.Mutex

	// Assets is the built dashboard (index.html at root). May be nil.
	Assets fs.FS
	// CLIDir holds prebuilt CLI binaries served for installation. May be empty.
	CLIDir string

	fuego *fuego.Server
}

func loadSecret(cfg *config.Config) ([]byte, error) {
	path := cfg.SecretFile
	if path == "" {
		path = filepath.Join(cfg.DataDir, "secret.key")
	}
	if b, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(b))
		if len(s) < 32 {
			return nil, fmt.Errorf("%s: secret must be at least 32 characters", path)
		}
		return []byte(s), nil
	} else if !errors.Is(err, fs.ErrNotExist) || cfg.SecretFile != "" {
		return nil, err
	}
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	s := fmt.Sprintf("%x", raw)
	if err := os.WriteFile(path, []byte(s+"\n"), 0o600); err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// New opens storage and prepares the server.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "redrop.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	blobs, err := blob.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	secret, err := loadSecret(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, db: db, blobs: blobs, log: log,
		uploadDir:       filepath.Join(cfg.DataDir, "uploads"),
		accessSigner:    auth.NewSigner(secret, "access"),
		previewSigner:   auth.NewSigner(secret, "preview"),
		stateSigner:     auth.NewSigner(secret, "oidc-state"),
		seenSigner:      auth.NewSigner(secret, "seen"),
		loginLimiter:    auth.NewLimiter(5, 5*time.Minute),
		loginIPLimiter:  auth.NewLimiter(30, 15*time.Minute),
		unlockLimiter:   auth.NewLimiter(5, 5*time.Minute),
		unlockIPLimiter: auth.NewLimiter(60, time.Hour),
		publicLimiter:   auth.NewLimiter(60, 10*time.Minute),
	}
	if err := os.MkdirAll(s.uploadDir, 0o700); err != nil {
		return nil, err
	}
	if err := s.setupOIDC(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() error { return s.db.Close() }

// API builds the Fuego server with every API route registered.
func (s *Server) API() *fuego.Server {
	if s.fuego != nil {
		return s.fuego
	}
	f := fuego.NewServer(
		fuego.WithoutStartupMessages(),
		fuego.WithLogHandler(s.log.Handler()),
		fuego.WithoutLogger(),
		fuego.WithMaxBodySize(1<<20),
		fuego.WithSecurity(openapi3.SecuritySchemes{
			"bearer": &openapi3.SecuritySchemeRef{Value: openapi3.NewSecurityScheme().
				WithType("http").WithScheme("bearer").WithDescription("API token (rdp_…)")},
			"session": &openapi3.SecuritySchemeRef{Value: openapi3.NewSecurityScheme().
				WithType("apiKey").WithIn("cookie").WithName(s.sessionCookieName()).WithDescription("Dashboard session")},
		}),
		fuego.WithEngineOptions(fuego.WithOpenAPIConfig(fuego.OpenAPIConfig{
			DisableLocalSave:     true,
			DisableMessages:      true,
			DisableDefaultServer: true,
			PrettyFormatJSON:     true,
			SpecURL:              "/api/openapi.json",
			SwaggerURL:           "/api/docs",
			Info: &openapi3.Info{
				Title:       "redrop API",
				Description: "Share agent artifacts through expiring, optionally protected links.",
				Version:     Version,
			},
		})),
	)
	s.fuego = f
	s.registerRoutes(f)
	f.RegisterOpenAPIRoutes(f)
	return f
}

// OpenAPI returns the JSON OpenAPI document.
func (s *Server) OpenAPI() ([]byte, error) {
	f := s.API()
	spec := f.OutputOpenAPISpec()
	return spec.MarshalJSON()
}

// Handler dispatches by Host between the content domain and the app.
func (s *Server) Handler() http.Handler {
	app := s.appHandler()
	content := s.contentHandler()
	contentHost := s.cfg.ContentHost()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(host, ".")
		if host == contentHost || strings.HasSuffix(host, "."+contentHost) {
			content.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	})
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if err := s.bootstrap(ctx); err != nil {
		return err
	}
	go s.janitor(ctx)
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Info("redrop listening", "addr", s.cfg.Listen, "public_url", s.cfg.PublicOrigin(), "content_url", s.cfg.ContentURL.String(), "version", Version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// bootstrap prints a one-time setup link when there are no users.
func (s *Server) bootstrap(ctx context.Context) error {
	n, err := s.db.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	link, _, err := s.createOneTime(ctx, &store.OneTime{Purpose: store.PurposeSetup, Role: "admin", ExpiresAt: time.Now().Add(24 * time.Hour)}, true)
	if err != nil {
		return err
	}
	s.log.Warn("no users exist yet; open this one-time link to create the admin account (valid 24h)", "url", link)
	return nil
}

// appHandler is the dashboard/API origin.
func (s *Server) appHandler() http.Handler {
	f := s.API()
	var h http.Handler = f.Mux
	h = s.securityHeaders(h)
	h = s.logRequests(h)
	return h
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if !strings.HasPrefix(r.URL.Path, "/api/docs") {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-src "+s.contentFrameSrc()+"; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		}
		if s.cfg.PublicURL.Scheme == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) contentFrameSrc() string {
	u := s.cfg.ContentURL
	host := "*." + u.Hostname()
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	return u.Scheme + "://" + host
}

type statusWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || sw.status >= 400 {
			// Paths never contain secrets except one-time link tokens, which are redacted.
			s.log.Info("http", "method", r.Method, "path", redactPath(r.URL.Path), "status", sw.status, "bytes", sw.n, "ms", time.Since(start).Milliseconds())
		}
	})
}

func redactPath(p string) string {
	for _, pre := range []string{"/api/v1/invites/", "/api/v1/reset/", "/invite/", "/reset/", "/setup/"} {
		if strings.HasPrefix(p, pre) {
			rest := strings.TrimPrefix(p, pre)
			if i := strings.Index(rest, "/"); i >= 0 {
				return pre + "…" + rest[i:]
			}
			return pre + "…"
		}
	}
	return p
}
