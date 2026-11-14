package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

func isTTY(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func progressFn() client.Progress {
	if g.json || !isTTY(os.Stderr) {
		return nil
	}
	start := time.Now()
	return func(sent, total int64) {
		if total < 4<<20 {
			return
		}
		pct := float64(sent) / float64(total) * 100
		fmt.Fprintf(os.Stderr, "\ruploading %5.1f%%  %s / %s  ", pct, human(sent), human(total))
		if sent >= total {
			fmt.Fprintf(os.Stderr, "\r%-60s\r", "")
			_ = start
		}
	}
}

func human(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func until(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Until(*t)
	if d <= 0 {
		return "expired"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

func generatePassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	var b strings.Builder
	for i := 0; i < 20; i++ {
		if i > 0 && i%5 == 0 {
			b.WriteByte('-')
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}

func readSecretFile(p string) (string, error) {
	var b []byte
	var err error
	if p == "-" {
		b, err = io.ReadAll(io.LimitReader(os.Stdin, 4096))
	} else {
		b, err = os.ReadFile(p)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// resolveID accepts an artifact ID, its link, or the link's slug.
func resolveID(ctx context.Context, c *client.Client, arg string) (string, error) {
	slug, err := artifactSlug(arg)
	if err != nil || slug == "" {
		return strings.TrimSpace(arg), err
	}
	list, err := c.ListArtifacts(ctx, "", "all")
	if err != nil {
		return "", mapErr(err)
	}
	for _, a := range list {
		if s, _ := artifactSlug(a.URL); s == slug {
			return a.ID, nil
		}
	}
	return "", withCode(exitNotFound, "no artifact of yours has the link %s", arg)
}

// artifactSlug returns the slug named by a link (https://<slug>.<content-domain>/,
// scheme optional) or a bare slug, and "" for anything else, which is taken
// as an artifact ID. A link whose first label is not a slug is a usage error.
func artifactSlug(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if ids.ValidSlug(arg) {
		return arg, nil
	}
	if !strings.Contains(arg, "://") && !strings.Contains(arg, ".") {
		return "", nil
	}
	raw := arg
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err == nil {
		label, _, _ := strings.Cut(strings.ToLower(u.Hostname()), ".")
		if ids.ValidSlug(label) {
			return label, nil
		}
	}
	return "", withCode(exitUsage, "not an artifact ID or link: %s (links look like https://amber-otter-quilt-river.usercontent.example/)", arg)
}

func printArtifact(a *api.Artifact, extra map[string]string) error {
	if g.json {
		out := map[string]any{}
		b, _ := jsonRoundTrip(a)
		out = b
		for k, v := range extra {
			out[k] = v
		}
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "url:\t%s\n", a.URL)
	if p, ok := extra["password"]; ok {
		fmt.Fprintf(w, "password:\t%s\n", p)
	}
	fmt.Fprintf(w, "id:\t%s\n", a.ID)
	fmt.Fprintf(w, "title:\t%s\n", a.Title)
	fmt.Fprintf(w, "status:\t%s\n", a.Status)
	fmt.Fprintf(w, "kind:\t%s (%d file%s, %s)\n", a.Kind, a.Files, plural(a.Files), human(a.Size))
	fmt.Fprintf(w, "version:\t%d of %d\n", a.Version, a.Versions)
	fmt.Fprintf(w, "expires:\t%s (%s)\n", fmtTime(a.ExpiresAt), until(a.ExpiresAt))
	prot := []string{}
	if a.HasPassword {
		prot = append(prot, "password")
	}
	if a.MaxViews > 0 {
		prot = append(prot, fmt.Sprintf("max %d view%s", a.MaxViews, plural(a.MaxViews)))
	}
	if len(prot) == 0 {
		prot = append(prot, "anyone with the link")
	}
	fmt.Fprintf(w, "access:\t%s\n", strings.Join(prot, ", "))
	views := strconv.Itoa(a.Views)
	if a.MaxViews > 0 {
		views += "/" + strconv.Itoa(a.MaxViews)
	}
	if a.Previews > 0 {
		views += fmt.Sprintf(" (+%d link previews)", a.Previews)
	}
	fmt.Fprintf(w, "views:\t%s\n", views)
	info := "shown"
	if !a.InfoButton {
		info = "hidden (direct view)"
	}
	fmt.Fprintf(w, "info button:\t%s\n", info)
	old := "viewers see the list only"
	if a.OldVersions {
		old = "viewers can open them"
	}
	fmt.Fprintf(w, "older versions:\t%s\n", old)
	if a.PurgeAt != nil {
		fmt.Fprintf(w, "deleted at:\t%s\n", fmtTime(a.PurgeAt))
	}
	return w.Flush()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------- share / update ----------

type contentFlags struct {
	as, name, entry, note string
	spa                   bool
}

func (f *contentFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.as, "as", "", "force the kind: html (render the page), file (viewer + download), bundle (folder/static site)")
	cmd.Flags().StringVar(&f.name, "name", "", "file name to use (required when PATH is - for stdin)")
	cmd.Flags().StringVar(&f.entry, "entry", "", "start page inside a bundle (default index.html; without one a file listing is shown)")
	cmd.Flags().BoolVar(&f.spa, "spa", false, "bundle is a single-page app: unknown extension-less paths serve the entry page")
	cmd.Flags().StringVar(&f.note, "note", "", "note stored with this version; viewers see it in the info panel's version list")
}

// viewerFlags are the viewer settings: --info / --no-info (the info button,
// and the viewer frame for HTML) and --old-versions / --no-old-versions.
type viewerFlags struct{ info, noInfo, old, noOld bool }

func (f *viewerFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.BoolVar(&f.info, "info", false, "show the info button (expiry, versions, size); HTML opens in redrop's viewer frame")
	fl.BoolVar(&f.noInfo, "no-info", false, "hide the info button; HTML is served directly, without the viewer frame")
	fl.BoolVar(&f.old, "old-versions", false, "let viewers open older versions from the info panel")
	fl.BoolVar(&f.noOld, "no-old-versions", false, "viewers see the version list only (the default)")
}

// values are nil for a pair when neither flag was given (keep the current
// setting, or for new shares use the default).
func (f *viewerFlags) values() (info, old *bool, err error) {
	if info, err = pair(f.info, f.noInfo, "--info", "--no-info"); err != nil {
		return nil, nil, err
	}
	old, err = pair(f.old, f.noOld, "--old-versions", "--no-old-versions")
	return info, old, err
}

func pair(on, off bool, onName, offName string) (*bool, error) {
	if on && off {
		return nil, withCode(exitUsage, "%s conflicts with %s", onName, offName)
	}
	if !on && !off {
		return nil, nil
	}
	return &on, nil
}

func (f *contentFlags) source(arg string) (*client.Source, error) {
	src := &client.Source{Kind: f.as, Name: f.name, Entry: f.entry, SPA: f.spa, Note: f.note}
	if arg == "-" {
		if f.name == "" {
			return nil, withCode(exitUsage, "--name is required when reading from stdin (e.g. --name report.html)")
		}
		if isTTY(os.Stdin) {
			return nil, withCode(exitUsage, "stdin is a terminal; pipe content in, e.g. `cat report.html | redrop share - --name report.html`")
		}
		src.Reader = os.Stdin
	} else {
		src.Path = arg
	}
	return src, nil
}

func shareCmd() *cobra.Command {
	var cf contentFlags
	var vf viewerFlags
	var title, ttl, password, passwordFile string
	var genPassword, once, quiet bool
	var maxViews int
	cmd := &cobra.Command{
		Use:     "share PATH",
		Aliases: []string{"push", "put"},
		Short:   "Share a file, HTML page, or folder and print its link",
		Long: `Upload PATH and print a private link.

What gets shared depends on PATH (override with --as):
  *.html / *.htm   rendered as a web page (inline JS/CSS and CDN scripts work)
  a directory      a bundle: a static site served from its index.html, or a
                   browsable file listing when there is no index.html
  anything else    a file page with an inline preview (images, video, audio,
                   PDF, text) and a download button
  -                read stdin; requires --name to pick the kind and file name

Use --as bundle with a .zip or .tar.gz to publish an archived site.

Access control (combine freely):
  --ttl 2d                 expire after 2 days ("never" if the server allows)
  --password-generate      random password, printed with the link
  --password-file FILE     password from a file ("-" reads stdin)
  --once / --max-views N   link stops working after N opens; viewers click
                           "Open" first, so chat link previews don't use it up

Shared pages get a small "i" button with the title, version history,
expiry, remaining views and size; HTML opens in redrop's viewer with the
artifact untouched in a frame. --no-info serves pages directly instead;
--old-versions lets viewers open earlier versions. curl, scripts and very
old browsers (no Fetch Metadata) always get the artifact directly. Inside an
older version, a root-absolute link (/x.html) leads to the current version.

The link is printed on the first line; it is the only way in, so share it
deliberately. Use "redrop update ID PATH" to publish changes to the same link.`,
		Example: `  redrop share report.html
  redrop share ./dist --spa --ttl 30d --title "Dashboard prototype"
  redrop share data.csv --once
  redrop share notes.md --password-generate --ttl 12h
  echo '<h1>Hello</h1>' | redrop share - --name hello.html
  redrop share site.zip --as bundle
  redrop share report.html --json | jq -r .url
  redrop share slides.html --no-info`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			src, err := cf.source(args[0])
			if err != nil {
				return err
			}
			info, old, err := vf.values()
			if err != nil {
				return err
			}
			if once {
				if cmd.Flags().Changed("max-views") && maxViews != 1 {
					return withCode(exitUsage, "--once conflicts with --max-views %d", maxViews)
				}
				maxViews = 1
			}
			n := 0
			for _, set := range []bool{password != "", passwordFile != "", genPassword} {
				if set {
					n++
				}
			}
			if n > 1 {
				return withCode(exitUsage, "use only one of --password, --password-file, --password-generate")
			}
			if passwordFile != "" {
				if passwordFile == "-" && args[0] == "-" {
					return withCode(exitUsage, "stdin cannot provide both content and password")
				}
				if password, err = readSecretFile(passwordFile); err != nil {
					return withCode(exitUsage, "read password: %v", err)
				}
				if password == "" {
					return withCode(exitUsage, "password file is empty")
				}
			}
			extra := map[string]string{}
			if genPassword {
				password = generatePassword()
				extra["password"] = password
			}
			a, err := c.Share(cmd.Context(), src, client.PublishOptions{Title: title, TTL: ttl, Password: password, MaxViews: maxViews, InfoButton: info, OldVersions: old != nil && *old}, progressFn())
			if err != nil {
				return mapErr(err)
			}
			if quiet && !g.json {
				fmt.Println(a.URL)
				if p, ok := extra["password"]; ok {
					fmt.Println(p)
				}
				return nil
			}
			return printArtifact(a, extra)
		},
	}
	cf.register(cmd)
	vf.register(cmd)
	f := cmd.Flags()
	f.StringVarP(&title, "title", "t", "", "title shown in the dashboard and the info button (default: the page <title> or file name)")
	f.StringVar(&ttl, "ttl", "", "lifetime such as 30m, 12h, 7d, 2w, or never (default: server default, usually 7d)")
	f.StringVarP(&password, "password", "p", "", "require this password (visible in process lists; prefer --password-file or --password-generate)")
	f.StringVar(&passwordFile, "password-file", "", "read the password from FILE (- for stdin)")
	f.BoolVarP(&genPassword, "password-generate", "g", false, "generate a strong password and print it with the link")
	f.IntVar(&maxViews, "max-views", 0, "stop working after N opens (0 = unlimited)")
	f.BoolVar(&once, "once", false, "one-time link (same as --max-views 1)")
	f.BoolVarP(&quiet, "quiet", "q", false, "print only the URL (and generated password)")
	return cmd
}

func updateCmd() *cobra.Command {
	var cf contentFlags
	var vf viewerFlags
	cmd := &cobra.Command{
		Use:   "update ID|URL PATH",
		Short: "Publish a new version of an artifact at the same link",
		Long: `Upload PATH as a new version of an existing artifact. The link, expiry,
password, and view limit stay the same; previous versions are kept and can be
restored with "redrop rollback". --info / --no-info and --old-versions /
--no-old-versions also change those viewer settings; they are saved before
the upload.`,
		Example: `  redrop update k2j4m9qx7d4a report.html --note "fixed totals"
  redrop update https://amber-otter-quilt-river.usercontent.example/ ./dist --spa`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			src, err := cf.source(args[1])
			if err != nil {
				return err
			}
			info, old, err := vf.values()
			if err != nil {
				return err
			}
			// The settings are saved before the upload (see UpdateWithOptions).
			a, err := c.UpdateWithOptions(cmd.Context(), id, src, client.VersionOptions{InfoButton: info, OldVersions: old}, progressFn())
			if err != nil {
				return mapErr(err)
			}
			return printArtifact(a, nil)
		},
	}
	cf.register(cmd)
	vf.register(cmd)
	return cmd
}

// ---------- manage ----------

func listCmd() *cobra.Command {
	var status, search string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your artifacts",
		Example: `  redrop list
  redrop list --status active
  redrop list --search report --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			list, err := c.ListArtifacts(cmd.Context(), search, status)
			if err != nil {
				return mapErr(err)
			}
			if g.json {
				return printJSON(list)
			}
			if len(list) == 0 {
				fmt.Println("No artifacts.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATUS\tKIND\tSIZE\tVIEWS\tEXPIRES\tACCESS\tTITLE")
			for _, a := range list {
				views := strconv.Itoa(a.Views)
				if a.MaxViews > 0 {
					views += "/" + strconv.Itoa(a.MaxViews)
				}
				acc := "link"
				if a.HasPassword {
					acc = "password"
				}
				title := a.Title
				if len(title) > 48 {
					title = title[:47] + "…"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Status, a.Kind, human(a.Size), views, until(a.ExpiresAt), acc, title)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "filter: active, expired, exhausted, revoked")
	cmd.Flags().StringVarP(&search, "search", "s", "", "search title, ID, or token name")
	return cmd
}

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "info ID|URL",
		Aliases: []string{"show", "get"},
		Short:   "Show details of an artifact",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			a, err := c.GetArtifact(cmd.Context(), id)
			if err != nil {
				return mapErr(err)
			}
			return printArtifact(a, nil)
		},
	}
}

func setCmd() *cobra.Command {
	var vf viewerFlags
	var title, ttl, password, passwordFile string
	var genPassword, noPassword, resetViews, unlimited, spa, noSPA bool
	var maxViews int
	cmd := &cobra.Command{
		Use:   "set ID|URL",
		Short: "Change title, expiry, password, view limit, or viewer settings",
		Long: `Change settings of an existing artifact. Only the flags you pass change.
Changing the password signs out everyone who already unlocked the link.`,
		Example: `  redrop set k2j4m9qx7d4a --ttl 30d
  redrop set k2j4m9qx7d4a --ttl never
  redrop set k2j4m9qx7d4a --password-generate
  redrop set k2j4m9qx7d4a --no-password --unlimited
  redrop set k2j4m9qx7d4a --max-views 5 --reset-views
  redrop set k2j4m9qx7d4a --no-info`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			req := &api.UpdateArtifactRequest{}
			f := cmd.Flags()
			extra := map[string]string{}
			if f.Changed("title") {
				req.Title = &title
			}
			if f.Changed("ttl") {
				req.TTL = &ttl
			}
			pwSources := 0
			for _, n := range []string{"password", "password-file", "password-generate", "no-password"} {
				if f.Changed(n) {
					pwSources++
				}
			}
			if pwSources > 1 {
				return withCode(exitUsage, "use only one of --password, --password-file, --password-generate, --no-password")
			}
			switch {
			case f.Changed("password"):
				req.Password = &password
			case passwordFile != "":
				p, err := readSecretFile(passwordFile)
				if err != nil {
					return withCode(exitUsage, "read password: %v", err)
				}
				req.Password = &p
			case genPassword:
				p := generatePassword()
				req.Password = &p
				extra["password"] = p
			case noPassword:
				empty := ""
				req.Password = &empty
			}
			if f.Changed("max-views") && unlimited {
				return withCode(exitUsage, "--max-views conflicts with --unlimited")
			}
			if f.Changed("max-views") {
				req.MaxViews = &maxViews
			}
			if unlimited {
				zero := 0
				req.MaxViews = &zero
			}
			req.ResetViews = resetViews
			if spa && noSPA {
				return withCode(exitUsage, "--spa conflicts with --no-spa")
			}
			if spa || noSPA {
				v := spa
				req.SPA = &v
			}
			if req.InfoButton, req.OldVersions, err = vf.values(); err != nil {
				return err
			}
			if req.Title == nil && req.TTL == nil && req.Password == nil && req.MaxViews == nil && !req.ResetViews && req.SPA == nil && req.InfoButton == nil && req.OldVersions == nil {
				return withCode(exitUsage, "nothing to change; see `redrop set --help`")
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			a, err := c.UpdateArtifact(cmd.Context(), id, req)
			if err != nil {
				return mapErr(err)
			}
			return printArtifact(a, extra)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&title, "title", "t", "", "new title")
	f.StringVar(&ttl, "ttl", "", "new lifetime counted from now: 30m, 12h, 7d, 2w, never")
	f.StringVarP(&password, "password", "p", "", "set a password")
	f.StringVar(&passwordFile, "password-file", "", "read the new password from FILE (- for stdin)")
	f.BoolVarP(&genPassword, "password-generate", "g", false, "set a generated password and print it")
	f.BoolVar(&noPassword, "no-password", false, "remove the password")
	f.IntVar(&maxViews, "max-views", 0, "total number of opens allowed")
	f.BoolVar(&unlimited, "unlimited", false, "remove the view limit")
	f.BoolVar(&resetViews, "reset-views", false, "reset the view counter to zero")
	f.BoolVar(&spa, "spa", false, "enable single-page-app fallback")
	f.BoolVar(&noSPA, "no-spa", false, "disable single-page-app fallback")
	vf.register(cmd)
	return cmd
}

func revokeCmd(revoke bool) *cobra.Command {
	use, short := "revoke ID|URL", "Disable a link immediately (reversible with restore)"
	if !revoke {
		use, short = "restore ID|URL", "Re-enable a revoked link"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			a, err := c.UpdateArtifact(cmd.Context(), id, &api.UpdateArtifactRequest{Revoked: &revoke})
			if err != nil {
				return mapErr(err)
			}
			return printArtifact(a, nil)
		},
	}
}

func rotateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate ID|URL",
		Short: "Replace the link with a new four-word one; the old URL stops working",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			a, err := c.RotateArtifact(cmd.Context(), id)
			if err != nil {
				return mapErr(err)
			}
			return printArtifact(a, nil)
		},
	}
}

func deleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete ID|URL...",
		Aliases: []string{"rm"},
		Short:   "Permanently delete artifacts and all their versions",
		Long:    "Permanently delete artifacts. This cannot be undone. Non-interactive use requires --yes.",
		Example: "  redrop delete k2j4m9qx7d4a --yes",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if !yes {
				if !isTTY(os.Stdin) {
					return withCode(exitUsage, "refusing to delete without --yes when not interactive")
				}
				fmt.Fprintf(os.Stderr, "Permanently delete %d artifact%s? [y/N] ", len(args), plural(len(args)))
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return withCode(exitGeneral, "cancelled")
				}
			}
			deleted := []string{}
			for _, arg := range args {
				id, err := resolveID(cmd.Context(), c, arg)
				if err != nil {
					return err
				}
				if err := c.DeleteArtifact(cmd.Context(), id); err != nil {
					return mapErr(fmt.Errorf("%s: %w", arg, err))
				}
				deleted = append(deleted, id)
				if !g.json {
					fmt.Println("deleted", id)
				}
			}
			if g.json {
				return printJSON(map[string]any{"deleted": deleted})
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func versionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "versions ID|URL",
		Short: "List versions of an artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			list, err := c.ListVersions(cmd.Context(), id)
			if err != nil {
				return mapErr(err)
			}
			if g.json {
				return printJSON(list)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "VERSION\tCURRENT\tKIND\tFILES\tSIZE\tCREATED\tNOTE (* shown to viewers)")
			for _, v := range list {
				cur := ""
				if v.Current {
					cur = "*"
				}
				t := v.CreatedAt
				note := v.Note
				if note != "" && v.NotePublic {
					note = "* " + note
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\t%s\n", v.Number, cur, v.Kind, v.Files, human(v.Size), fmtTime(&t), note)
			}
			return w.Flush()
		},
	}
}

func rollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rollback ID|URL VERSION",
		Aliases: []string{"activate"},
		Short:   "Make an earlier (or later) version current",
		Example: "  redrop rollback k2j4m9qx7d4a 2",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				return withCode(exitUsage, "VERSION must be a positive number")
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			a, err := c.ActivateVersion(cmd.Context(), id, n)
			if err != nil {
				return mapErr(err)
			}
			return printArtifact(a, nil)
		},
	}
}

func filesCmd() *cobra.Command {
	var version int
	cmd := &cobra.Command{
		Use:   "files ID|URL",
		Short: "List files in an artifact version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			fl, err := c.ListFiles(cmd.Context(), id, version)
			if err != nil {
				return mapErr(err)
			}
			if g.json {
				return printJSON(fl)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SIZE\tTYPE\tPATH")
			for _, f := range fl.Items {
				fmt.Fprintf(w, "%s\t%s\t%s\n", human(f.Size), strings.Split(f.ContentType, ";")[0], f.Path)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&version, "version", 0, "version number (default: current)")
	return cmd
}

func viewsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "views ID|URL",
		Short: "Show recent opens, link previews, and failed password attempts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			list, err := c.ListViews(cmd.Context(), id)
			if err != nil {
				return mapErr(err)
			}
			if g.json {
				return printJSON(list)
			}
			if len(list) == 0 {
				fmt.Println("No views yet.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tEVENT\tCOUNTRY\tCLIENT")
			for _, v := range list {
				t := v.At
				country := v.Country
				if country == "" {
					country = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", fmtTime(&t), v.Outcome, country, v.Client)
			}
			return w.Flush()
		},
	}
}

// ---------- setup ----------

func loginCmd() *cobra.Command {
	var tokenStdin bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save the server URL and an API token",
		Long: `Verify and save the server URL and API token to the config file (mode 0600).
Create a token in the dashboard under Tokens. Without --token, the token is
read from stdin (--token-stdin) or prompted for without echo.

Alternatively set REDROP_URL and REDROP_TOKEN in the environment; they
override the saved login.

When you create a token, the dashboard shows a one-line command that installs
the CLI and runs this login for you (it contains the token, so it ends up in
your shell history).`,
		Example: `  redrop login --url https://redrop.example.com
  redrop login --url https://redrop.example.com --token rdp_…
  printf %s "$TOKEN" | redrop login --url https://redrop.example.com --token-stdin

  # Install and log in at once (macOS/Linux, then Windows PowerShell):
  curl -fsSL https://redrop.example.com/install.sh | REDROP_TOKEN='rdp_…' sh
  $env:REDROP_TOKEN='rdp_…'; irm https://redrop.example.com/install.ps1 | iex`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			u := firstNonEmpty(g.url, os.Getenv("REDROP_URL"), cfg.URL)
			if u == "" {
				return withCode(exitUsage, "--url is required")
			}
			nu, err := normalizeURL(u)
			if err != nil {
				return withCode(exitUsage, "%v", err)
			}
			tok := g.token
			switch {
			case tok != "":
			case tokenStdin:
				b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
				if err != nil {
					return err
				}
				tok = strings.TrimSpace(string(b))
			case isTTY(os.Stdin):
				fmt.Fprintf(os.Stderr, "API token for %s (create one at %s/tokens): ", nu, nu)
				b, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
				if err != nil {
					return err
				}
				tok = strings.TrimSpace(string(b))
			default:
				return withCode(exitUsage, "--token or --token-stdin is required when not interactive")
			}
			if !strings.HasPrefix(tok, "rdp_") {
				return withCode(exitUsage, "that does not look like a redrop API token (they start with rdp_)")
			}
			c := client.New(nu, tok)
			c.UserAgent = "redrop/" + version
			me, err := c.Me(cmd.Context())
			if err != nil {
				return mapErr(fmt.Errorf("token check failed: %w", err))
			}
			if err := saveConfig(fileConfig{URL: nu, Token: tok}); err != nil {
				return err
			}
			if g.json {
				return printJSON(map[string]any{"url": nu, "user": me.User.Username, "token": me.TokenName, "config": configPath()})
			}
			fmt.Printf("Logged in to %s as %s (token %q). Saved to %s\n", nu, me.User.Username, me.TokenName, configPath())
			return nil
		},
	}
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the token from stdin")
	return cmd
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the saved token (the token itself stays valid until revoked in the dashboard)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			cfg.Token = ""
			if err := saveConfig(cfg); err != nil {
				return err
			}
			fmt.Println("Saved token removed. Revoke it in the dashboard if it may be compromised.")
			return nil
		},
	}
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the server, user, token, and storage usage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			me, err := c.Me(cmd.Context())
			if err != nil {
				return mapErr(err)
			}
			if g.json {
				return printJSON(map[string]any{"url": c.BaseURL, "me": me})
			}
			quota := "unlimited"
			if me.User.QuotaBytes > 0 {
				quota = human(me.User.QuotaBytes)
			}
			fmt.Printf("server:   %s\nuser:     %s (%s)\ntoken:    %s\nstorage:  %s of %s, %d artifact%s\n",
				c.BaseURL, me.User.Username, me.User.Role, me.TokenName, human(me.User.UsedBytes), quota, me.User.Artifacts, plural(me.User.Artifacts))
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version (and the server version when configured)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := map[string]string{"cli": version}
			if c, err := newClient(); err == nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				defer cancel()
				if ac, err := c.AuthConfig(ctx); err == nil {
					out["server"] = ac.Version
					out["url"] = c.BaseURL
				}
			}
			if g.json {
				return printJSON(out)
			}
			fmt.Printf("redrop %s\n", version)
			if s, ok := out["server"]; ok {
				fmt.Printf("server %s (%s)\n", s, out["url"])
			}
			return nil
		},
	}
}
