package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// tunnelEvent is one --json output line of redrop tunnel.
type tunnelEvent struct {
	Event    string `json:"event"`
	ID       string `json:"id"`
	URL      string `json:"url,omitempty"`
	Target   string `json:"target,omitempty"`
	Password string `json:"password,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
	DelayMS  int64  `json:"delay_ms,omitempty"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at"`
}

func tunnelCmd() *cobra.Command {
	var id, title, ttl, password, passwordFile string
	var genPassword, info, noInfo, fb, noFb, allowRemote, logRequests, quiet, once bool
	var maxViews int
	cmd := &cobra.Command{
		Use:   "tunnel TARGET",
		Short: "Share an app running on this computer through a private link",
		Long: `Share a running app, such as a dev server with hot reload, an API or a
webhook receiver, through a private link like any artifact's. Requests to the
link travel through this command to TARGET on this computer and back. The link
works while the command runs; Ctrl-C ends it.

TARGET is a port (3000 means http://localhost:3000, tried on 127.0.0.1 and
::1), host:port, or http://host:port. Only this computer is allowed by
default: anything else needs --allow-remote-target, and then everyone with the
link can reach that address through this computer. HTTPS targets are not
supported. The app receives Host: TARGET; the link's host is in
X-Forwarded-Host, the visitor's address in X-Forwarded-For.

The link has the same protections as other artifacts: --ttl, a password,
revoke, the info panel and the viewer frame (--no-info serves the app
directly). A view limit and --once don't fit a live app. WebSockets (hot
reload) work. While this command isn't connected, visitors who pass the
password see "This tunnel is offline"; it reconnects by itself after network
trouble. --id ID reuses an existing tunnel's link, so the link stays the same
across restarts.

Requests are limited in size, time and bandwidth by the server; the app must
start answering within about 90 seconds. Each user keeps a limited number of
tunnels, so reuse one with --id. --log prints one line per request (method,
percent-encoded path, final status, time) to stderr, never headers or bodies.

With --json, the first line has the link, then each change (online, offline,
retry) is one JSON line. Exit codes: 0 after Ctrl-C, 7 when the server ended
the tunnel (revoked, deleted, expired, or ended by an administrator).`,
		Example: `  redrop tunnel 3000
  redrop tunnel 5173 --password-generate --ttl 2h
  redrop tunnel 127.0.0.1:8080 --title "API preview" --log
  redrop tunnel 5173 --id k2j4m9qx7d4a
  redrop tunnel 192.0.2.10:8080 --allow-remote-target`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if once || cmd.Flags().Changed("max-views") {
				return withCode(exitUsage, "a tunnel can't have a view limit (--once, --max-views); use --ttl or a password")
			}
			infoSetting, err := pair(info, noInfo, "--info", "--no-info")
			if err != nil {
				return err
			}
			fbSetting, err := pair(fb, noFb, "--feedback", "--no-feedback")
			if err != nil {
				return err
			}
			pw, generated, err := tunnelPassword(password, passwordFile, genPassword, cmd.Flags().Changed("password"))
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			target, err := client.ResolveTunnelTarget(ctx, args[0], allowRemote, nil)
			if err != nil {
				return mapErr(err)
			}
			if target.Class != client.ClassLoopback {
				fmt.Fprintf(os.Stderr, "warning: %s is on the %s, not this computer. Anyone with the link can reach it through this computer.\n", target, target.Class)
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			var a *api.Artifact
			if id == "" {
				if title == "" {
					title = target.Host
				}
				a, err = c.CreateTunnel(ctx, &api.CreateTunnelRequest{Title: title, TTL: ttl, Password: pw, InfoButton: infoSetting, Feedback: fbSetting != nil && *fbSetting})
			} else {
				a, err = reuseTunnel(ctx, c, id, cmd, title, ttl, pw, infoSetting, fbSetting)
			}
			if err != nil {
				return mapErr(err)
			}
			printTunnel(a, target, generated, quiet)
			err = c.RunTunnel(ctx, client.TunnelOptions{ID: a.ID, Target: target, OnEvent: tunnelEvents(a.ID), OnRequest: requestLogger(logRequests)})
			var ended *client.TunnelEndedError
			switch {
			case errors.As(err, &ended):
				return withCode(exitGone, "%v", err)
			case err != nil:
				return mapErr(err)
			}
			if !g.json {
				fmt.Fprintln(os.Stderr, "redrop: tunnel stopped")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&id, "id", "", "reuse this tunnel's link (an ID or the link) instead of creating one")
	f.StringVarP(&title, "title", "t", "", "title shown in the dashboard and the info button (default: the target)")
	f.StringVar(&ttl, "ttl", "", "lifetime such as 30m, 12h, 7d, or never (default: server default, usually 7d)")
	f.StringVarP(&password, "password", "p", "", "require this password (visible in process lists; prefer --password-file or --password-generate)")
	f.StringVar(&passwordFile, "password-file", "", "read the password from FILE (- for stdin)")
	f.BoolVarP(&genPassword, "password-generate", "g", false, "generate a strong password and print it with the link")
	f.BoolVar(&info, "info", false, "show the info button; pages open in redrop's viewer frame")
	f.BoolVar(&noInfo, "no-info", false, "hide the info button; the app is served directly, without the viewer frame")
	f.BoolVar(&fb, "feedback", false, "let viewers leave comments from the info panel (read them with redrop feedback)")
	f.BoolVar(&noFb, "no-feedback", false, "close comments (the default)")
	f.BoolVar(&allowRemote, "allow-remote-target", false, "allow a target that is not on this computer (a LAN or internet address)")
	f.BoolVar(&logRequests, "log", false, "print one line per request to stderr: method, path, status, time")
	f.BoolVarP(&quiet, "quiet", "q", false, "print only the URL (and generated password)")
	// Accepted only to explain why they don't apply.
	f.BoolVar(&once, "once", false, "not for tunnels")
	f.IntVar(&maxViews, "max-views", 0, "not for tunnels")
	f.MarkHidden("once")
	f.MarkHidden("max-views")
	return cmd
}

// tunnelPassword picks the password from at most one source. generated is
// set when it was generated, to print it.
func tunnelPassword(password, file string, generate, given bool) (pw, generated string, err error) {
	n := 0
	for _, set := range []bool{given, file != "", generate} {
		if set {
			n++
		}
	}
	switch {
	case n > 1:
		return "", "", withCode(exitUsage, "use only one of --password, --password-file, --password-generate")
	case file != "":
		if pw, err = readSecretFile(file); err != nil {
			return "", "", withCode(exitUsage, "read password: %v", err)
		}
		if pw == "" {
			return "", "", withCode(exitUsage, "password file is empty")
		}
		return pw, "", nil
	case generate:
		pw = generatePassword()
		return pw, pw, nil
	}
	return password, "", nil
}

// reuseTunnel looks up --id, checks it is a tunnel, and applies any settings
// given on the command line.
func reuseTunnel(ctx context.Context, c *client.Client, arg string, cmd *cobra.Command, title, ttl, pw string, info, fb *bool) (*api.Artifact, error) {
	id, err := resolveID(ctx, c, arg)
	if err != nil {
		return nil, err
	}
	a, err := c.GetArtifact(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Kind != api.KindTunnel {
		return nil, withCode(exitUsage, "artifact %s is a %s, not a tunnel; leave out --id to create a tunnel", a.ID, a.Kind)
	}
	req := &api.UpdateArtifactRequest{InfoButton: info, Feedback: fb}
	f := cmd.Flags()
	if f.Changed("title") {
		req.Title = &title
	}
	if f.Changed("ttl") {
		req.TTL = &ttl
	}
	if pw != "" {
		req.Password = &pw
	}
	if req.Title == nil && req.TTL == nil && req.Password == nil && info == nil && fb == nil {
		return a, nil
	}
	return c.UpdateArtifact(ctx, a.ID, req)
}

func printTunnel(a *api.Artifact, target *client.TunnelTarget, password string, quiet bool) {
	switch {
	case g.json:
		printJSONLine(tunnelEvent{Event: "tunnel", ID: a.ID, URL: a.URL, Target: target.String(), Password: password, At: time.Now().Format(time.RFC3339)})
	case quiet:
		fmt.Println(a.URL)
		if password != "" {
			fmt.Println(password)
		}
	default:
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "url:\t%s\n", a.URL)
		if password != "" {
			fmt.Fprintf(w, "password:\t%s\n", password)
		}
		fmt.Fprintf(w, "id:\t%s\n", a.ID)
		addrs := make([]string, len(target.Addrs))
		for i, ap := range target.Addrs {
			addrs[i] = ap.Addr().String()
		}
		fmt.Fprintf(w, "target:\t%s (%s)\n", target, strings.Join(addrs, ", "))
		fmt.Fprintf(w, "expires:\t%s (%s)\n", fmtTime(a.ExpiresAt), until(a.ExpiresAt))
		access := "anyone with the link"
		if a.HasPassword {
			access = "password"
		}
		fmt.Fprintf(w, "access:\t%s\n", access)
		w.Flush()
	}
}

// tunnelEvents reports connection changes: JSON lines on stdout with --json,
// otherwise short lines on stderr.
func tunnelEvents(id string) func(client.TunnelEvent) {
	return func(e client.TunnelEvent) {
		if g.json {
			ev := tunnelEvent{Event: string(e.Kind), ID: id, Attempt: e.Attempt, DelayMS: e.Delay.Milliseconds(), At: time.Now().Format(time.RFC3339)}
			if e.Err != nil {
				ev.Error = e.Err.Error()
			}
			printJSONLine(ev)
			return
		}
		switch e.Kind {
		case client.TunnelOnline:
			fmt.Fprintln(os.Stderr, "redrop: tunnel online; press Ctrl-C to stop it")
		case client.TunnelOffline:
			fmt.Fprintf(os.Stderr, "redrop: tunnel offline (%v); reconnecting in %s\n", e.Err, e.Delay.Round(100*time.Millisecond))
		case client.TunnelRetry:
			fmt.Fprintf(os.Stderr, "redrop: cannot connect (%v); retrying in %s\n", e.Err, e.Delay.Round(100*time.Millisecond))
		}
	}
}

// requestLogger prints --log lines: method, path, status and time, never
// the query, headers or bodies.
func requestLogger(on bool) func(client.TunnelRequest) {
	if !on {
		return nil
	}
	return func(r client.TunnelRequest) {
		fmt.Fprintf(os.Stderr, "%s %s %d %s\n", r.Method, r.Path, r.Status, r.Duration.Round(time.Millisecond))
	}
}
