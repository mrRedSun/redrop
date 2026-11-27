package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/client"
)

// watch holds live reload while it runs (DESIGN.md "Live reload"): the lease
// starts once the watcher is ready, its state is printed, and it is
// released before watch exits.

type liveServer struct {
	mu       sync.Mutex
	refuse   bool
	requests []string
}

func (s *liveServer) handler(t *testing.T) http.Handler {
	return plaintextArtifact(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		refuse := s.refuse
		s.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/api/v1/artifacts/artifactidabcdef/live/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch {
		case refuse:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"status":409,"title":"conflict","detail":"live reload needs the info button"}`)
		case r.Method == http.MethodPut:
			fmt.Fprintf(w, `{"expires_at":%q,"renew_after_seconds":20}`, time.Now().Add(time.Minute).Format(time.RFC3339))
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	})
}

func (s *liveServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// runLiveWatch runs watch until the live session reports its first notice.
func runLiveWatch(t *testing.T, s *liveServer, args ...string) (string, string, error) {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	page := writeCLIFile(t, t.TempDir(), "page.html", "<h1>Page</h1>")
	first := make(chan client.Notice, 1)
	origLive, origWatch := keepLive, watchArtifact
	t.Cleanup(func() { keepLive, watchArtifact = origLive, origWatch })
	keepLive = func(ctx context.Context, h client.LiveHolder, id string, opts client.LiveOptions) {
		report := opts.OnNotice
		opts.OnNotice = func(n client.Notice) {
			report(n)
			select {
			case first <- n:
			default:
			}
		}
		client.KeepLive(ctx, h, id, opts)
	}
	// The watcher becomes ready, then stops once live reload answered.
	watchArtifact = func(ctx context.Context, _ client.Uploader, _ string, _ *client.Source, opts client.WatchOptions) error {
		opts.OnReady()
		select {
		case <-first:
		case <-time.After(30 * time.Second): // only a hang guard
			t.Error("live reload reported nothing within 30s")
		}
		return nil
	}
	var err error
	out, errOut := captureCLIOutput(t, func() {
		cmd := newRootCmd()
		cmd.SetArgs(append([]string{"--url", srv.URL, "--token", "test-token", "watch", "artifactidabcdef", page}, args...))
		err = cmd.Execute()
	})
	return out, errOut, err
}

func TestWatchHoldsLiveReload(t *testing.T) {
	s := &liveServer{}
	_, errOut, err := runLiveWatch(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "live reload on") || strings.Index(errOut, "watching") > strings.Index(errOut, "live reload on") {
		t.Fatalf("stderr %q: want \"watching\", then \"live reload on\"", errOut)
	}
	got := s.seen()
	if len(got) != 2 || !strings.HasPrefix(got[0], "PUT ") || !strings.HasPrefix(got[1], "DELETE ") || got[0][4:] != got[1][7:] {
		t.Fatalf("requests %q: want one lease and its release", got)
	}
}

func TestWatchLiveRefusedJSON(t *testing.T) {
	s := &liveServer{refuse: true}
	out, _, err := runLiveWatch(t, s, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var ev watchEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ev); err != nil || ev.Event != "live_off" || ev.ID != "artifactidabcdef" || !strings.Contains(ev.Error, "info button") {
		t.Fatalf("stdout %q: want one live_off event with the reason (%v)", out, err)
	}
	if got := s.seen(); len(got) != 1 {
		t.Fatalf("requests %q: the first refusal's notice stopped watch before another request", got)
	}
}

func TestWatchNoLive(t *testing.T) {
	srv := httptest.NewServer(plaintextArtifact(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	page := writeCLIFile(t, t.TempDir(), "page.html", "<h1>Page</h1>")
	orig := watchArtifact
	t.Cleanup(func() { watchArtifact = orig })
	watchArtifact = func(_ context.Context, _ client.Uploader, _ string, _ *client.Source, opts client.WatchOptions) error {
		opts.OnReady()
		return nil
	}
	var err error
	_, errOut := captureCLIOutput(t, func() {
		cmd := newRootCmd()
		cmd.SetArgs([]string{"--url", srv.URL, "--token", "test-token", "watch", "artifactidabcdef", page, "--no-live"})
		err = cmd.Execute()
	})
	if err != nil || strings.Contains(errOut, "live") {
		t.Fatalf("--no-live: %v, stderr %q", err, errOut)
	}

	captureCLIOutput(t, func() {
		cmd := newRootCmd()
		cmd.SetArgs([]string{"--url", srv.URL, "--token", "test-token", "watch", "artifactidabcdef", page, "--live", "--no-live"})
		err = cmd.Execute()
	})
	requireExitCode(t, err, exitUsage)
}
