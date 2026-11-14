package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

func TestWatchRejectsInvalidInputsBeforeConfiguration(t *testing.T) {
	t.Setenv("REDROP_URL", "")
	t.Setenv("REDROP_TOKEN", "")
	t.Setenv("REDROP_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	for _, tc := range []struct {
		name, path, message string
		flags               []string
	}{
		{name: "stdin", path: "-", message: "stdin cannot be watched"},
		{name: "name", path: "does-not-exist", flags: []string{"--name", "other.html"}, message: "--name cannot be combined with watch"},
		{name: "empty name", path: "does-not-exist", flags: []string{"--name="}, message: "--name cannot be combined with watch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureCLIOutput(t, func() {
				cmd := newRootCmd()
				cmd.SetArgs(append([]string{"watch", "artifact-id", tc.path}, tc.flags...))
				err := cmd.Execute()
				requireExitCode(t, err, exitUsage)
				if !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("error = %q; want %q", err, tc.message)
				}
			})
		})
	}
}

func TestWatchRejectsInvalidInputsWithoutNetwork(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	path := writeCLIFile(t, t.TempDir(), "page.html", "original")
	for _, args := range [][]string{
		{"watch", "https://amber-otter-quilt-river.usercontent.example/", "-"},
		{"watch", "https://amber-otter-quilt-river.usercontent.example/", path, "--name", "other.html"},
	} {
		captureCLIOutput(t, func() {
			cmd := newRootCmd()
			cmd.SetArgs(append([]string{"--url", srv.URL, "--token", "test-token"}, args...))
			requireExitCode(t, cmd.Execute(), exitUsage)
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid inputs made %d requests; want none", got)
	}
}

func TestWatchCanceledContextSucceeds(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	path := writeCLIFile(t, t.TempDir(), "page.html", "original")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, errOut := captureCLIOutput(t, func() {
		cmd := newRootCmd()
		cmd.SetArgs([]string{"--url", srv.URL, "--token", "test-token", "--json", "watch", "artifact-id", path})
		if err := cmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("canceled watch returned %v; want success", err)
		}
	})
	if out != "" || errOut != "" {
		t.Fatalf("canceled JSON watch wrote stdout=%q stderr=%q", out, errOut)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("canceled watch made %d requests; want none", got)
	}
}

func TestWatchJSONLines(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	notices := []client.Notice{
		{Kind: client.NoticeRetry, Attempt: 2, Delay: 1500 * time.Millisecond, Err: errors.New("temporary failure")},
		{Kind: client.NoticeWatchError, Err: errors.New("watch overflow")},
		{Kind: client.NoticeRootVanished, Err: os.ErrNotExist},
		{Kind: client.NoticeRewatchFailed, Err: errors.New("permission denied")},
		{Kind: client.NoticeTreeChanged, Err: client.ErrSourceChanged},
		{Kind: client.NoticeEmptyTree, Err: client.ErrEmptyDirectory},
		{Kind: client.NoticeKind("future"), Err: errors.New("new kind")},
	}
	// The wire names are the CLI contract, written out literally here.
	wireNames := []string{"retry", "watch_error", "root_vanished", "rewatch_failed", "tree_changed", "empty_folder", "notice"}
	out, errOut := captureCLIOutput(t, func() {
		g.json = true
		opts := watchOptions("artifact-id", "site", time.Second)
		opts.OnReady()
		opts.OnPublish(client.WatchResult{
			Artifact: &api.Artifact{URL: "https://amber-otter-quilt-river.usercontent.example/"},
			Version:  3, At: at,
		})
		for _, n := range notices {
			opts.OnNotice(n)
		}
	})
	if errOut != "" {
		t.Fatalf("JSON mode wrote stderr: %q", errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1+len(notices) {
		t.Fatalf("got %d lines; want %d: %q", len(lines), 1+len(notices), out)
	}
	for i, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i, err)
		}
		stamp, ok := got["at"].(string)
		if !ok {
			t.Fatalf("line %d has no timestamp: %v", i, got)
		}
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			t.Fatalf("line %d timestamp: %v", i, err)
		}
		want := map[string]any{"id": "artifact-id", "at": stamp}
		if i == 0 {
			want["event"], want["version"], want["url"] = "publish", float64(3), "https://amber-otter-quilt-river.usercontent.example/"
			want["at"] = at.Format(time.RFC3339)
		} else {
			n := notices[i-1]
			want["event"], want["error"] = wireNames[i-1], n.Err.Error()
			if n.Kind == client.NoticeRetry {
				want["attempt"], want["delay_ms"] = float64(n.Attempt), float64(1500)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("line %d = %#v; want %#v", i, got, want)
		}
	}
}

func TestWatchTextNotices(t *testing.T) {
	for _, tc := range []struct {
		notice client.Notice
		want   string
	}{
		{client.Notice{Kind: client.NoticeRetry, Attempt: 2, Delay: 1530 * time.Millisecond, Err: errors.New("offline")}, "redrop: retrying in 1.5s (attempt 2): offline\n"},
		{client.Notice{Kind: client.NoticeRootVanished, Err: errors.New("stat site: no such file or directory")}, "redrop: site disappeared; waiting for it to come back\n"},
		{client.Notice{Kind: client.NoticeTreeChanged, Err: client.ErrSourceChanged}, "redrop: site changed while publishing; trying again once it settles: source changed while it was being read\n"},
		{client.Notice{Kind: client.NoticeEmptyTree, Err: client.ErrEmptyDirectory}, "redrop: site has no files to publish; waiting for files\n"},
		{client.Notice{Kind: client.NoticeKind("future"), Err: errors.New("new kind")}, "redrop: watch notice future: new kind\n"},
	} {
		out, errOut := captureCLIOutput(t, func() {
			g.json = false
			watchOptions("artifact-id", "site", time.Second).OnNotice(tc.notice)
		})
		if out != "" || errOut != tc.want {
			t.Errorf("%s: stdout=%q stderr=%q; want %q", tc.notice.Kind, out, errOut, tc.want)
		}
	}
}

// Run the real CLI entry point in a child test process to check os.Exit codes.
func TestCLIProcess(t *testing.T) {
	if os.Getenv("REDROP_TEST_CLI_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"redrop"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing CLI arguments")
}

func TestWatchUnauthorizedExitCode(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/uploads" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"status":401,"title":"unauthorized","detail":"token rejected"}`)
	}))
	defer srv.Close()
	path := writeCLIFile(t, t.TempDir(), "page.html", "original")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIProcess$", "--",
		"--url", srv.URL, "--token", "test-token", "watch", "artifact-id", path, "--debounce", "1ms")
	cmd.Env = append(os.Environ(), "REDROP_TEST_CLI_PROCESS=1", "REDROP_CONFIG="+filepath.Join(t.TempDir(), "missing.json"))
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	}()
	reader := bufio.NewReader(stderr)
	ready, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(ready, "watching ") {
		t.Fatalf("watch did not become ready: %q, %v", ready, err)
	}
	writeCLIFile(t, filepath.Dir(path), filepath.Base(path), "changed")
	errOut, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitAuth {
		t.Fatalf("exit = %v; want %d; stderr=%s%s", err, exitAuth, ready, errOut)
	}
	if !strings.Contains(string(errOut), "token rejected") {
		t.Fatalf("missing auth error: %s", errOut)
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed publish wrote stdout: %q", stdout.String())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("made %d requests; auth failure must stop without retry", got)
	}
}

// An empty folder fails locally with the exit code the server's 400 gave.
func TestEmptyFolderExitCode(t *testing.T) {
	_, err := client.Prepare(&client.Source{Path: t.TempDir()})
	var ee *exitError
	if !errors.As(mapErr(err), &ee) || ee.code != exitUsage || !errors.Is(err, client.ErrEmptyDirectory) {
		t.Fatalf("empty folder: %v", err)
	}
}

// A fatal local error from watch, such as a missing temp directory, exits 1.
func TestWatchLocalErrorExitCode(t *testing.T) {
	var ee *exitError
	err := mapErr(&os.PathError{Op: "open", Path: "/missing/redrop-source-1", Err: os.ErrNotExist})
	if !errors.As(err, &ee) || ee.code != exitGeneral {
		t.Fatalf("local error: %v", err)
	}
}

// watch builds its source like share and update, so --render and --entry
// reach the uploader, and an invalid mode is a usage error before any
// network access.
func TestWatchPassesRenderOptions(t *testing.T) {
	t.Setenv("REDROP_URL", "http://redrop.example.com")
	t.Setenv("REDROP_TOKEN", "rdp_test")
	t.Setenv("REDROP_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	dir := t.TempDir()
	note := writeCLIFile(t, dir, "notes.md", "# Notes\n")
	site := filepath.Join(dir, "site")
	if err := os.Mkdir(site, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, site, "README.md", "# Readme\n")
	var got *client.Source
	orig := watchArtifact
	watchArtifact = func(_ context.Context, up client.Uploader, id string, src *client.Source, _ client.WatchOptions) error {
		if _, ok := up.(*client.Client); !ok || id != "artifact-id" {
			t.Errorf("uploader=%T id=%q", up, id)
		}
		got = src
		return nil
	}
	t.Cleanup(func() { watchArtifact = orig })

	for _, tc := range []struct {
		name  string
		args  []string
		check func(*client.Prepared) bool
	}{
		{"none keeps the file raw", []string{note, "--render", "none"}, func(p *client.Prepared) bool {
			return p.Kind == api.KindFile && p.Filename == "notes.md"
		}},
		{"markdown with an entry renders the folder", []string{site, "--render", "markdown", "--entry", "README.md"}, func(p *client.Prepared) bool {
			return p.Kind == api.KindBundle && p.Entry == "index.html"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got = nil
			captureCLIOutput(t, func() {
				cmd := newRootCmd()
				cmd.SetArgs(append([]string{"watch", "artifact-id"}, tc.args...))
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
			})
			if got == nil {
				t.Fatal("watch did not start")
			}
			if got.Warn == nil || got.OnSkipped != nil {
				t.Errorf("warnings must be printed and skip counts not: Warn=%v OnSkipped=%v", got.Warn != nil, got.OnSkipped != nil)
			}
			p, err := client.Prepare(got)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if !tc.check(p) {
				t.Fatalf("prepared kind=%s entry=%s filename=%s render=%q", p.Kind, p.Entry, p.Filename, got.Render)
			}
		})
	}

	t.Run("bogus mode", func(t *testing.T) {
		got = nil
		captureCLIOutput(t, func() {
			cmd := newRootCmd()
			cmd.SetArgs([]string{"watch", "artifact-id", note, "--render", "bogus"})
			requireExitCode(t, cmd.Execute(), exitUsage)
		})
		if got != nil {
			t.Fatal("watch started with an invalid render mode")
		}
	})
}
