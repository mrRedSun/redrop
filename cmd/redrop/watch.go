package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrRedSun/redrop/pkg/client"
)

// watchEvent is one --json output line per publish or non-fatal notice.
type watchEvent struct {
	Event   string `json:"event"`
	ID      string `json:"id"`
	Version int    `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	DelayMS int64  `json:"delay_ms,omitempty"`
	Error   string `json:"error,omitempty"`
	At      string `json:"at"`
}

// noticeEvents is the --json wire name of each notice kind. It is the CLI
// contract, kept here so renaming a library constant cannot change it.
var noticeEvents = map[client.NoticeKind]string{
	client.NoticeRetry:         "retry",
	client.NoticeWatchError:    "watch_error",
	client.NoticeRootVanished:  "root_vanished",
	client.NoticeRewatchFailed: "rewatch_failed",
	client.NoticeTreeChanged:   "tree_changed",
	client.NoticeEmptyTree:     "empty_folder",
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

Edits are debounced (--debounce, default 500ms). Directory uploads use the
skip rule listed under Share in "redrop guide"; explicitly named files are
always uploaded. A watched .md file is re-rendered on each change (see
--render). Identical content never publishes a version. Each publish
prints the version number and time. With --json, each publish or typed
notice (retry, watch_error, root_vanished, rewatch_failed, tree_changed,
empty_folder) is one JSON line; retries include the upcoming attempt number
and the delay before it.

If files change while a version is being packed or uploaded, or the folder
is empty (a build tool cleaned it), watch waits for the next change instead
of exiting. Ctrl-C or SIGTERM stops watching and exits 0. Network errors,
rate limiting, and server errors are retried with nominal exponential
backoff from 1s to 60s, with jitter below the cap. Rate-limit responses
honor Retry-After up to 10 minutes. Auth or not-found errors stop with the
CLI's usual exit codes.`,
		Example: `  redrop watch k2j4m9qx7d4a report.html
  redrop watch https://amber-otter-quilt-river.usercontent.example/ ./dist --spa
  redrop watch k2j4m9qx7d4a ./site --debounce 1s --json
  redrop watch k2j4m9qx7d4a notes.md`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[1] == "-" {
				return withCode(exitUsage, "watch needs a path on disk; stdin cannot be watched")
			}
			// Same content flags and source rules as update; --name is
			// meaningless here (no stdin), so reject it before any lookup.
			if cmd.Flags().Changed("name") {
				return withCode(exitUsage, "--name cannot be combined with watch; watch uploads the file at PATH")
			}
			if _, err := os.Stat(args[1]); err != nil {
				return withCode(exitUsage, "cannot watch %s: %v", args[1], err)
			}
			c, err := newClient()
			if err != nil {
				return err
			}
			id, err := resolveID(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			src := &client.Source{Path: args[1], Kind: cf.as, Entry: cf.entry, SPA: cf.spa, Note: cf.note}

			opts := watchOptions(id, args[1], debounce)
			err = client.Watch(cmd.Context(), c, id, src, opts)
			if err != nil {
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

// watchOptions keeps streaming output separate from command validation.
func watchOptions(id, path string, debounce time.Duration) client.WatchOptions {
	return client.WatchOptions{
		Debounce: debounce,
		Progress: progressFn(),
		OnPublish: func(r client.WatchResult) {
			if g.json {
				printJSONLine(watchEvent{
					Event: "publish", ID: id, Version: r.Version,
					URL: r.Artifact.URL, At: r.At.Format(time.RFC3339),
				})
				return
			}
			fmt.Printf("version %d published at %s\n", r.Version, r.At.Format("2006-01-02 15:04:05 MST"))
			fmt.Println(r.Artifact.URL)
		},
		OnNotice: func(n client.Notice) {
			if g.json {
				event := watchEvent{
					Event: noticeEvents[n.Kind], ID: id, Attempt: n.Attempt,
					DelayMS: n.Delay.Milliseconds(), At: time.Now().Format(time.RFC3339),
				}
				if event.Event == "" {
					event.Event = "notice"
				}
				if n.Err != nil {
					event.Error = n.Err.Error()
				}
				printJSONLine(event)
				return
			}
			switch n.Kind {
			case client.NoticeRetry:
				fmt.Fprintf(os.Stderr, "redrop: retrying in %s (attempt %d): %v\n", n.Delay.Round(100*time.Millisecond), n.Attempt, n.Err)
			case client.NoticeWatchError:
				fmt.Fprintf(os.Stderr, "redrop: watch error: %v\n", n.Err)
			case client.NoticeRootVanished:
				fmt.Fprintf(os.Stderr, "redrop: %s disappeared; waiting for it to come back\n", path)
			case client.NoticeRewatchFailed:
				fmt.Fprintf(os.Stderr, "redrop: re-watching %s failed: %v\n", path, n.Err)
			case client.NoticeTreeChanged:
				fmt.Fprintf(os.Stderr, "redrop: %s changed while publishing; trying again once it settles: %v\n", path, n.Err)
			case client.NoticeEmptyTree:
				fmt.Fprintf(os.Stderr, "redrop: %s has no files to publish; waiting for files\n", path)
			default:
				fmt.Fprintf(os.Stderr, "redrop: watch notice %s: %v\n", n.Kind, n.Err)
			}
		},
		// Announce only once the watches are registered and the initial
		// digest is seeded, so subsequent changes are guaranteed to be seen.
		OnReady: func() {
			if !g.json {
				fmt.Fprintf(os.Stderr, "watching %s -> %s (Ctrl-C to stop)\n", path, id)
			}
		},
	}
}
