package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrRedSun/redrop/pkg/client"
)

// watchEvent is one --json output line per publish or retry.
type watchEvent struct {
	Event   string `json:"event"` // "publish" | "retry"
	ID      string `json:"id"`
	Version int    `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Error   string `json:"error,omitempty"`
	At      string `json:"at"`
}

func watchCmd() *cobra.Command {
	var cf contentFlags
	var debounce time.Duration
	cmd := &cobra.Command{
		Use:   "watch ID|URL PATH",
		Short: "Republish PATH as a new version whenever it changes",
		Long: `Watch PATH (a file or a folder, same rules as "redrop update") and publish a
new version of the artifact whenever its content changes. The link, expiry,
password, and view limit stay the same.

Edits are debounced (--debounce, default 500ms); editor temp/swap/backup
files, .git, and node_modules are ignored; identical content never
publishes a version. Each publish prints the version number and time (with
--json, one JSON line per publish or retry). Ctrl-C or SIGTERM stops
watching and exits 0. Network errors, rate limiting, and server errors are
retried with exponential backoff while watching continues; auth or
not-found errors stop with the CLI's usual exit codes.`,
		Example: `  redrop watch k2j4m9qx7d4a report.html
  redrop watch https://amber-otter-quilt-river.usercontent.example/ ./dist --spa
  redrop watch k2j4m9qx7d4a ./site --debounce 1s --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if args[1] == "-" {
				return withCode(exitUsage, "watch needs a path on disk; stdin cannot be watched")
			}
			if _, err := os.Stat(args[1]); err != nil {
				return withCode(exitUsage, "cannot watch %s: %v", args[1], err)
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			// Same content flags and source rules as update; --name is
			// meaningless here (no stdin), so it is rejected.
			if cmd.Flags().Changed("name") {
				return withCode(exitUsage, "--name cannot be combined with watch; watch uploads the file at PATH")
			}
			src := &client.Source{Path: args[1], Kind: cf.as, Entry: cf.entry, SPA: cf.spa, Note: cf.note}

			opts := client.WatchOptions{
				Debounce: debounce,
				Progress: progressFn(),
			}
			opts.OnPublish = func(r client.WatchResult) {
				if g.json {
					printJSONLine(watchEvent{
						Event: "publish", ID: id, Version: r.Version,
						URL: r.Artifact.URL, At: r.At.Format(time.RFC3339),
					})
					return
				}
				fmt.Printf("version %d published at %s\n", r.Version, r.At.Format("2006-01-02 15:04:05 MST"))
				fmt.Println(r.Artifact.URL)
			}
			opts.Log = func(format string, a ...any) {
				if g.json {
					printJSONLine(watchEvent{Event: "retry", ID: id, At: time.Now().Format(time.RFC3339), Error: fmt.Sprintf(format, a...)})
					return
				}
				fmt.Fprintf(os.Stderr, "redrop: "+format+"\n", a...)
			}
			// Announce only once changes are guaranteed to be seen: the
			// watches are registered and the initial digest is seeded.
			opts.OnReady = func() {
				if !g.json {
					fmt.Fprintf(os.Stderr, "watching %s -> %s (Ctrl-C to stop)\n", args[1], id)
				}
			}
			err = client.Watch(cmd.Context(), c, id, src, opts)
			if err != nil && !errors.Is(err, context.Canceled) && err.Error() != "" {
				return mapErr(err)
			}
			if !g.json {
				fmt.Fprintln(os.Stderr, "stopped")
			}
			return nil
		},
	}
	cf.register(cmd)
	cmd.Flags().DurationVar(&debounce, "debounce", client.DefaultDebounce, "how long the tree must be quiet before publishing (e.g. 500ms, 2s)")
	return cmd
}
