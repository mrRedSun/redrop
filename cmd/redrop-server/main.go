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
	"syscall"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/app"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/httpapi"
	"github.com/mrRedSun/redrop/internal/mail"
	"github.com/mrRedSun/redrop/internal/webui"
	"github.com/mrRedSun/redrop/pkg/api"
)

const usage = `redrop-server — self-hosted artifact sharing

Usage:
  redrop-server [serve]                Run the server (default)
  redrop-server openapi                Print the OpenAPI document
  redrop-server invite [--admin] [--email ADDR]
                                       Print a new invite link (works offline);
                                       with --email, queue it for delivery
  redrop-server reset USERNAME         Print a password reset link (also clears TOTP)
  redrop-server mail-test --to ADDR    Send a test message and print the SMTP result
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
	case "mail-test":
		err = mailTest(args)
	case "healthcheck":
		err = healthcheck()
	case "version", "--version", "-v":
		fmt.Print(serverVersionText())
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

// serverVersionText reports the stamped build on one line: the version
// always, the commit and build date when the build carried them.
func serverVersionText() string {
	s := httpapi.Version
	if httpapi.Commit != "" {
		s += " (commit " + httpapi.Commit
		if httpapi.Date != "" {
			s += ", built " + httpapi.Date
		}
		s += ")"
	}
	return s + "\n"
}

func dashboard() fs.FS {
	sub, err := fs.Sub(webui.Dist, "dist")
	if err != nil {
		return nil
	}
	return sub
}

func newApp(log *slog.Logger, offline bool) (*app.App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	cliDir := os.Getenv("REDROP_CLI_DIR")
	if cliDir == "" {
		cliDir = defaultCLIDir
	}
	return app.New(cfg, log, app.Options{Dashboard: dashboard(), CLIDir: cliDir, Offline: offline})
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := newApp(log, false)
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
	a, err := newApp(slog.New(slog.DiscardHandler), true)
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

// offlineAdmin opens the data directory without serving, for commands that
// mint one-time links. The links follow the same rules as the dashboard's.
func offlineAdmin() (*accounts.Administration, func(), error) {
	a, err := newApp(slog.New(slog.DiscardHandler), true)
	if err != nil {
		return nil, nil, err
	}
	return a.Accounts.Administration(), func() { a.Close() }, nil
}

func invite(args []string) error {
	fl := flag.NewFlagSet("invite", flag.ExitOnError)
	admin := fl.Bool("admin", false, "invite an administrator")
	note := fl.String("note", "", "note shown in the admin list")
	email := fl.String("email", "", "send the invite link to this address")
	fl.Parse(args)
	adm, done, err := offlineAdmin()
	if err != nil {
		return err
	}
	defer done()
	role := api.RoleUser
	if *admin {
		role = api.RoleAdmin
	}
	inv, err := adm.CreateInvite(context.Background(), "", accounts.InviteRequest{Note: *note, Role: role, Email: *email})
	if err != nil {
		return err
	}
	fmt.Println(inv.URL)
	if *email != "" {
		fmt.Fprintln(os.Stderr, "invite email queued; the serving process will deliver it")
	}
	return nil
}

// mailTest sends one message through the configured relay and reports the
// outcome. It sends directly, not through the queue: the point is to see
// the SMTP dialogue's result on the command line.
func mailTest(args []string) error {
	fl := flag.NewFlagSet("mail-test", flag.ExitOnError)
	to := fl.String("to", "", "recipient of the test message")
	fl.Parse(args)
	if *to == "" {
		return fmt.Errorf("mail-test needs --to ADDR")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.MailEnabled() {
		return fmt.Errorf("mail is not configured; set REDROP_SMTP_ADDR and REDROP_MAIL_FROM")
	}
	sm, err := mail.NewSMTP(mail.SMTPConfig{Addr: cfg.SMTPAddr, TLS: mail.TLSMode(cfg.SMTPTLS), Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, CAFile: cfg.SMTPCAFile, From: cfg.MailFrom, InsecurePlain: cfg.MailInsecurePlain})
	if err != nil {
		return err
	}
	m := mail.TestEmail(cfg.PublicOrigin())
	m.To = *to
	ctx, cancel := context.WithTimeout(context.Background(), mail.Timeout)
	defer cancel()
	if err := sm.Send(ctx, m); err != nil {
		return fmt.Errorf("SMTP test failed: %w", err)
	}
	fmt.Println("SMTP test accepted by the relay")
	return nil
}

func reset(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: redrop-server reset USERNAME")
	}
	adm, done, err := offlineAdmin()
	if err != nil {
		return err
	}
	defer done()
	ctx := context.Background()
	id, err := adm.UserIDByUsername(ctx, args[0])
	if err != nil {
		return err
	}
	link, err := adm.CreateResetLink(ctx, "", id, true)
	if err != nil {
		return err
	}
	fmt.Println(link.URL)
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
