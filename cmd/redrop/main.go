// Command redrop shares files, HTML pages, and folders through a redrop server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrRedSun/redrop/pkg/client"
)

var version = "dev"

// Exit codes (documented in `redrop guide`).
const (
	exitOK       = 0
	exitGeneral  = 1
	exitUsage    = 2
	exitAuth     = 3
	exitNotFound = 4
	exitTooLarge = 5
	exitNetwork  = 6
	exitGone     = 7
	exitConfig   = 8
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func withCode(code int, format string, a ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, a...)}
}

type globalOpts struct {
	url, token, config string
	json               bool
}

var g globalOpts

type fileConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func configPath() string {
	if g.config != "" {
		return g.config
	}
	if p := os.Getenv("REDROP_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "redrop", "config.json")
}

func loadConfig() fileConfig {
	var c fileConfig
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c fileConfig) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("invalid server URL %q", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// newClient resolves settings: flags > environment > config file.
func newClient() (*client.Client, error) {
	cfg := loadConfig()
	u := firstNonEmpty(g.url, os.Getenv("REDROP_URL"), cfg.URL)
	t := firstNonEmpty(g.token, os.Getenv("REDROP_TOKEN"), cfg.Token)
	if u == "" || t == "" {
		return nil, withCode(exitConfig, "not configured: run `redrop login --url https://your-server --token rdp_…` or set REDROP_URL and REDROP_TOKEN")
	}
	nu, err := normalizeURL(u)
	if err != nil {
		return nil, withCode(exitConfig, "%v", err)
	}
	c := client.New(nu, t)
	c.UserAgent = "redrop/" + version
	return c, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// mapErr converts API errors into exit codes.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return err
	}
	var usage *client.UsageError
	if errors.As(err, &usage) {
		return &exitError{exitUsage, err}
	}
	var ne *client.NetworkError
	if errors.As(err, &ne) {
		return &exitError{exitNetwork, err}
	}
	switch s := client.StatusOf(err); {
	case s == 401 || s == 403:
		return &exitError{exitAuth, err}
	case s == 404:
		return &exitError{exitNotFound, err}
	case s == 413:
		return &exitError{exitTooLarge, err}
	case s == 409 || s == 410:
		return &exitError{exitGone, err}
	case s == 400 || s == 422:
		return &exitError{exitUsage, err}
	}
	return &exitError{exitGeneral, err}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func main() {
	root := newRootCmd()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := root.ExecuteContext(ctx)
	if err == nil {
		os.Exit(exitOK)
	}
	code := exitGeneral
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
	} else if strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "flag") || strings.Contains(err.Error(), "arg(s)") {
		code = exitUsage
	}
	if g.json {
		json.NewEncoder(os.Stderr).Encode(map[string]any{"error": err.Error(), "exit_code": code})
	} else {
		fmt.Fprintln(os.Stderr, "redrop: "+err.Error())
	}
	os.Exit(code)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "redrop",
		Short: "Share files, HTML pages, and folders through expiring links",
		Long: `redrop publishes artifacts (an HTML page, any file, or a whole folder/static
site) to your redrop server and prints a private, unguessable link that can
expire, require a password, or work only once.

Every artifact gets its own isolated origin, named by four random words
(https://amber-otter-quilt-river.<content-domain>/), so shared HTML/JS runs
safely. Links are not indexed by search engines. Commands that take an ID also
accept the link or its four-word part; older 26-character links still work.

Quick start:
  redrop login --url https://redrop.example.com      # paste an API token
  redrop share report.html                           # prints the link
  redrop share ./dist --ttl 2d --password-generate   # share a folder/site
  redrop share secret.pdf --once                     # one-time link
  redrop update <id> report.html                     # new version, same link
  redrop list                                        # what am I sharing?

Agents: run "redrop guide" for a complete usage guide, use --json for
machine-readable output, and check exit codes (listed in the guide).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.SetVersionTemplate("redrop {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&g.url, "url", "", "server URL (overrides REDROP_URL and saved login)")
	pf.StringVar(&g.token, "token", "", "API token (overrides REDROP_TOKEN and saved login)")
	pf.StringVar(&g.config, "config", "", "config file path (default: user config dir/redrop/config.json, or REDROP_CONFIG)")
	pf.BoolVar(&g.json, "json", false, "print machine-readable JSON to stdout")
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return withCode(exitUsage, "%v\nRun '%s --help' for usage.", err, c.CommandPath())
	})

	root.AddGroup(&cobra.Group{ID: "share", Title: "Sharing:"}, &cobra.Group{ID: "manage", Title: "Managing artifacts:"}, &cobra.Group{ID: "setup", Title: "Setup:"})
	for _, c := range []*cobra.Command{shareCmd(), updateCmd()} {
		c.GroupID = "share"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{listCmd(), infoCmd(), setCmd(), revokeCmd(true), revokeCmd(false), rotateCmd(), deleteCmd(), versionsCmd(), rollbackCmd(), filesCmd(), viewsCmd()} {
		c.GroupID = "manage"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{loginCmd(), logoutCmd(), whoamiCmd(), guideCmd(), versionCmd()} {
		c.GroupID = "setup"
		root.AddCommand(c)
	}
	root.SetHelpCommandGroupID("setup")
	root.SetCompletionCommandGroupID("setup")
	return root
}
