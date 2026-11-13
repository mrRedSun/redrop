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

func watchCmd() *cobra.Command {
	var cf contentFlags
	var debounce time.Duration
	cmd := &cobra.Command{
		Use:   "watch ID|URL PATH",
		Short: "Republish PATH as a new version whenever it changes",
		Long: `Watch PATH (a file or a folder, same rules as "redrop update") and publish a
new version of the artifact whenever its content changes. The link, expiry,
password, and view limit stay the same.

Edits are debounced (--debounce, default 500ms), editor temp/swap files and
.git are ignored, and identical content never publishes a version. Each
publish prints the version number and time (with --json, one JSON line per
publish). Ctrl-C or SIGTERM stops watching and exits 0. Transient network or
server errors are retried with backoff while watching continues.`,
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
			// cmd.Context() cancels on Ctrl-C and SIGTERM (see main).

			opts := client.WatchOptions{
				Debounce: debounce,
				Kind:     cf.as,
				Entry:    cf.entry,
				SPA:      cf.spa,
				Note:     cf.note,
				Progress: progressFn(),
				Log: func(format string, a ...any) {
					fmt.Fprintf(os.Stderr, "redrop: "+format+"\n", a...)
				},
			}
			opts.OnPublish = func(r client.WatchResult) {
				if g.json {
					printJSONLine(map[string]any{
						"event": "publish", "id": id, "version": r.Version,
						"url": r.Artifact.URL, "at": r.At.Format(time.RFC3339),
					})
					return
				}
				fmt.Printf("version %d published at %s\n", r.Version, r.At.Format("2006-01-02 15:04:05 MST"))
				fmt.Println(r.Artifact.URL)
			}
			// Announce only once changes are guaranteed to be seen: the
			// watches are registered and the initial hash is seeded.
			opts.OnReady = func() {
				if !g.json {
					fmt.Fprintf(os.Stderr, "watching %s -> %s (Ctrl-C to stop)\n", args[1], id)
				}
			}
			err = client.Watch(cmd.Context(), c, id, args[1], opts)
			if err == nil || errors.Is(err, context.Canceled) {
				if !g.json {
					fmt.Fprintln(os.Stderr, "stopped")
				}
				return nil
			}
			return mapErr(err)
		},
	}
	cmd.Flags().StringVar(&cf.as, "as", "", "force the kind: html (render the page), file (viewer + download), bundle (folder/static site)")
	cmd.Flags().StringVar(&cf.entry, "entry", "", "start page inside a bundle (default index.html; without one a file listing is shown)")
	cmd.Flags().BoolVar(&cf.spa, "spa", false, "bundle is a single-page app: unknown extension-less paths serve the entry page")
	cmd.Flags().StringVar(&cf.note, "note", "", "short note stored with each version")
	cmd.Flags().DurationVar(&debounce, "debounce", client.DefaultDebounce, "how long the tree must be quiet before publishing (e.g. 500ms, 2s)")
	return cmd
}
