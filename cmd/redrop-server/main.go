// Command redrop-server runs the redrop artifact sharing server.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/server"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/internal/webui"
)

const usage = `redrop-server — self-hosted artifact sharing

Usage:
  redrop-server [serve]            Run the server (default)
  redrop-server openapi            Print the OpenAPI document
  redrop-server invite [--admin]   Print a new invite link (works offline)
  redrop-server reset USERNAME     Print a password reset link for a user
  redrop-server healthcheck        Exit 0 if the local server answers /healthz
  redrop-server version

Configuration is read from REDROP_* environment variables; see README.md.
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "openapi":
		err = openapi(log)
	case "invite":
		err = invite(os.Args[2:])
	case "reset":
		err = reset(os.Args[2:])
	case "healthcheck":
		err = healthcheck()
	case "version", "--version", "-v":
		fmt.Println(server.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newServer(log *slog.Logger) (*server.Server, *config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	s, err := server.New(context.Background(), cfg, log)
	if err != nil {
		return nil, nil, err
	}
	if sub, err := fs.Sub(webui.Dist, "dist"); err == nil {
		if _, err := fs.Stat(sub, "index.html"); err == nil {
			s.Assets = sub
		}
	}
	s.CLIDir = os.Getenv("REDROP_CLI_DIR")
	if s.CLIDir == "" {
		s.CLIDir = "/usr/share/redrop/cli"
	}
	return s, cfg, nil
}

func serve(log *slog.Logger) error {
	s, _, err := newServer(log)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Run(ctx)
}

func openapi(log *slog.Logger) error {
	// Use throwaway settings so this works without a deployment.
	tmp, err := os.MkdirTemp("", "redrop-openapi")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	os.Setenv("REDROP_DATA_DIR", tmp)
	if os.Getenv("REDROP_CONTENT_URL") == "" {
		os.Setenv("REDROP_CONTENT_URL", "https://content.example")
	}
	s, _, err := newServer(slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer s.Close()
	b, err := s.OpenAPI()
	if err != nil {
		return err
	}
	os.Stdout.Write(b)
	fmt.Println()
	return nil
}

func openStore() (*store.Store, *config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "redrop.db"))
	return db, cfg, err
}

func invite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	admin := fs.Bool("admin", false, "invite an administrator")
	note := fs.String("note", "", "note shown in the admin list")
	fs.Parse(args)
	db, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	role := "user"
	if *admin {
		role = "admin"
	}
	secret := ids.Secret()
	o := &store.OneTime{Purpose: store.PurposeInvite, Role: role, QuotaBytes: cfg.DefaultQuotaBytes, Note: *note, ExpiresAt: time.Now().Add(7 * 24 * time.Hour)}
	if err := db.CreateOneTime(context.Background(), o, ids.Hash(secret)); err != nil {
		return err
	}
	fmt.Println(cfg.PublicOrigin() + "/invite/" + secret)
	return nil
}

func reset(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: redrop-server reset USERNAME")
	}
	db, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	u, err := db.UserByUsername(context.Background(), args[0])
	if err != nil {
		return fmt.Errorf("user %q not found", args[0])
	}
	secret := ids.Secret()
	o := &store.OneTime{Purpose: store.PurposeReset, UserID: u.ID, ClearTOTP: true, ExpiresAt: time.Now().Add(24 * time.Hour)}
	if err := db.CreateOneTime(context.Background(), o, ids.Hash(secret)); err != nil {
		return err
	}
	fmt.Println(cfg.PublicOrigin() + "/reset/" + secret)
	return nil
}

func healthcheck() error {
	addr := os.Getenv("REDROP_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	return httpGet("http://" + addr + "/healthz")
}
