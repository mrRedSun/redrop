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

	"github.com/mrRedSun/redrop/internal/app"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/httpapi"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/internal/webui"
	"github.com/mrRedSun/redrop/pkg/api"
)

const usage = `redrop-server — self-hosted artifact sharing

Usage:
  redrop-server [serve]                Run the server (default)
  redrop-server openapi                Print the OpenAPI document
  redrop-server invite [--admin]       Print a new invite link (works offline)
  redrop-server reset USERNAME         Print a password reset link (also clears TOTP)
  redrop-server healthcheck            Exit 0 if the local server answers /healthz
  redrop-server version

Configuration is read from REDROP_* environment variables; see README.md.
`

const defaultCLIDir = "/usr/share/redrop/cli"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd, args := "serve", []string(nil)
	if len(os.Args) > 1 {
		cmd, args = os.Args[1], os.Args[2:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "openapi":
		err = openapi()
	case "invite":
		err = invite(args)
	case "reset":
		err = reset(args)
	case "healthcheck":
		err = healthcheck()
	case "version", "--version", "-v":
		fmt.Println(httpapi.Version)
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

func dashboard() fs.FS {
	sub, err := fs.Sub(webui.Dist, "dist")
	if err != nil {
		return nil
	}
	return sub
}

func newApp(ctx context.Context, log *slog.Logger) (*app.App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	cliDir := os.Getenv("REDROP_CLI_DIR")
	if cliDir == "" {
		cliDir = defaultCLIDir
	}
	return app.New(ctx, cfg, log, app.Options{Dashboard: dashboard(), CLIDir: cliDir})
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := newApp(ctx, log)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}

// openapi prints the spec using throwaway settings so it works anywhere.
func openapi() error {
	tmp, err := os.MkdirTemp("", "redrop-openapi")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	os.Setenv("REDROP_DATA_DIR", tmp)
	if os.Getenv("REDROP_CONTENT_URL") == "" {
		os.Setenv("REDROP_CONTENT_URL", "https://content.example")
	}
	a, err := newApp(context.Background(), slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := a.API.OpenAPI()
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(b))
	return err
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
	fl := flag.NewFlagSet("invite", flag.ExitOnError)
	admin := fl.Bool("admin", false, "invite an administrator")
	note := fl.String("note", "", "note shown in the admin list")
	fl.Parse(args)
	db, cfg, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	role := api.RoleUser
	if *admin {
		role = api.RoleAdmin
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
