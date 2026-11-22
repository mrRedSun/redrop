package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrRedSun/redrop/internal/app"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// CLI tests of end-to-end links against a real server that records every
// request it receives (method, raw path, headers, body) and every log line.

type recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// mark returns the recording's length, to look at what came after.
func (r *recorder) mark() int { r.mu.Lock(); defer r.mu.Unlock(); return r.buf.Len() }
func (r *recorder) since(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()[n:]
}

type cliServer struct {
	url, token string
	requests   recorder
	logs       recorder
}

func newCLIServer(t *testing.T) *cliServer {
	t.Helper()
	s := &cliServer{}
	t.Setenv("REDROP_DATA_DIR", t.TempDir())
	t.Setenv("REDROP_PUBLIC_URL", "http://app.test")
	t.Setenv("REDROP_CONTENT_URL", "http://content.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(&s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		s.requests.Write([]byte(r.Method + " " + r.RequestURI + "\n"))
		r.Header.Write(&s.requests)
		s.requests.Write(append(body, '\n'))
		a.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL

	link, err := a.Accounts.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, in any, cookies []*http.Cookie) *http.Response {
		b, _ := json.Marshal(in)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Redrop-Request", "1")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	setup := post("/api/v1/setup", api.SetupRequest{Token: link[strings.LastIndex(link, "/")+1:], Username: "admin", Password: "long-enough-password"}, nil)
	var tok api.CreatedToken
	if err := json.NewDecoder(post("/api/v1/tokens", api.CreateTokenRequest{Name: "cli"}, setup.Cookies()).Body).Decode(&tok); err != nil || tok.Secret == "" {
		t.Fatalf("token: %v", err)
	}
	s.token = tok.Secret
	return s
}

// run runs the CLI with this server, a config at cfg (its key store sits
// next to it), and stdin from the given text.
func (s *cliServer) run(t *testing.T, cfg, stdin string, args ...string) (string, string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(stdin)
	f.Seek(0, io.SeekStart)
	defer f.Close()
	oldIn := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = oldIn }()
	var runErr error
	out, errOut := captureCLIOutput(t, func() {
		cmd := newRootCmd()
		cmd.SetArgs(append([]string{"--url", s.url, "--token", s.token, "--config", cfg}, args...))
		runErr = cmd.Execute()
	})
	return out, errOut, runErr
}

func shareE2E(t *testing.T, s *cliServer, cfg, page string) *api.Artifact {
	t.Helper()
	out, _, err := s.run(t, cfg, "", "share", page, "--e2e", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var a api.Artifact
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	return &a
}

// B-1 of review 33: update and watch of an end-to-end encrypted artifact
// encrypt with its key when one is available, flag or not, and otherwise
// refuse before any upload; the plaintext never reaches the server.
func TestCLINeverUploadsPlaintextForAnEncryptedArtifact(t *testing.T) {
	s := newCLIServer(t)
	dir := t.TempDir()
	owner := filepath.Join(dir, "owner", "cli.json")
	stranger := filepath.Join(dir, "stranger", "cli.json")
	page := writeCLIFile(t, dir, "page.html", "<p>first</p>")
	a := shareE2E(t, s, owner, page)
	const marker = "plaintext-marker-5512"
	writeCLIFile(t, dir, "page.html", "<p>"+marker+"</p>")

	// With the key in this machine's store, no flag needed: encrypted.
	start := s.requests.mark()
	out, errOut, err := s.run(t, owner, "", "update", a.ID, page, "--json")
	if err != nil {
		t.Fatalf("update without --e2e, key stored: %v %s", err, errOut)
	}
	if !strings.Contains(errOut, "encrypting with the key from this machine's key store") {
		t.Fatalf("stderr: %q", errOut)
	}
	var b api.Artifact
	if json.Unmarshal([]byte(out), &b) != nil || b.Version != 2 || !b.E2E || !strings.Contains(b.URL, "#k=") {
		t.Fatalf("update output: %s", out)
	}
	if got := s.requests.since(start); strings.Contains(got, marker) || !strings.Contains(got, `"e2e":true`) {
		t.Fatal("the update sent plaintext, or an undeclared upload")
	}

	// Without any key: refused, exit 2, and not one upload request.
	for _, args := range [][]string{{"update", a.ID, page}, {"update", a.ID, page, "--info"}, {"watch", a.ID, page}} {
		start := s.requests.mark()
		_, errOut, err := s.run(t, stranger, "", args...)
		requireExitCode(t, err, exitUsage)
		if !strings.Contains(errOut+err.Error(), "nothing was uploaded") {
			t.Fatalf("%v: %v", args, err)
		}
		if got := s.requests.since(start); strings.Contains(got, "/api/v1/uploads") || strings.Contains(got, marker) || strings.Contains(got, "PATCH") {
			t.Fatalf("%v sent an upload, plaintext or settings:\n%s", args, got)
		}
	}

	// watch with the key stored encrypts every publish.
	orig := watchArtifact
	t.Cleanup(func() { watchArtifact = orig })
	var up client.Uploader
	watchArtifact = func(_ context.Context, u client.Uploader, _ string, _ *client.Source, _ client.WatchOptions) error {
		up = u
		return nil
	}
	if _, _, err := s.run(t, owner, "", "watch", a.ID, page); err != nil {
		t.Fatal(err)
	}
	if _, ok := up.(*client.E2EUploader); !ok {
		t.Fatalf("watch of an encrypted artifact uses %T", up)
	}
	if strings.Contains(s.requests.String(), marker) {
		t.Fatal("plaintext reached the server")
	}
}

// M2 of review 33: a #k= key given with an ID or link, as an argument or on
// stdin, is used locally and never reaches the server or its log.
func TestCLIKeyInArgumentsNeverReachesTheServer(t *testing.T) {
	s := newCLIServer(t)
	dir := t.TempDir()
	owner := filepath.Join(dir, "owner", "cli.json")
	page := writeCLIFile(t, dir, "page.html", "<p>one</p>")
	a := shareE2E(t, s, owner, page)
	_, frag, _ := strings.Cut(a.URL, "#")
	key := strings.TrimPrefix(frag, "k=")
	fresh := filepath.Join(dir, "fresh", "cli.json") // no key store: the key comes from the argument
	orig := watchArtifact
	t.Cleanup(func() { watchArtifact = orig })
	watchArtifact = func(context.Context, client.Uploader, string, *client.Source, client.WatchOptions) error { return nil }

	for _, tc := range []struct {
		stdin string
		args  []string
	}{
		{"", []string{"info", a.ID + "#k=" + key}},
		{"", []string{"info", a.URL}},
		{"", []string{"update", a.ID + "#k=" + key, page}},
		{"", []string{"info", a.ID + "#" + key}},
		{a.URL + "\n", []string{"update", "-", page}},
		{a.ID + "#k=" + key, []string{"info", "-"}},
		{"", []string{"watch", a.ID + "#k=" + key, page}},
		{"", []string{"set", a.ID + "#k=" + key, "--title", "renamed"}},
		{"", []string{"versions", a.ID + "#k=" + key}},
	} {
		if _, errOut, err := s.run(t, fresh, tc.stdin, tc.args...); err != nil {
			t.Fatalf("%v: %v %s", tc.args, err, errOut)
		}
	}
	if !strings.Contains(s.requests.String(), "GET /api/v1/artifacts/"+a.ID+"\n") {
		t.Fatal("the recorder missed the artifact reads")
	}
	for name, text := range map[string]string{"requests": s.requests.String(), "log": s.logs.String()} {
		if strings.Contains(text, key) {
			t.Errorf("the key reached the server's %s", name)
		}
	}
	// Uploads are random bytes, so only the log is searched for "%23".
	if strings.Contains(s.logs.String(), "%23") || strings.Contains(s.logs.String(), "#k=") {
		t.Error("a fragment reached the server's log")
	}
	if _, _, err := s.run(t, fresh, "", "update", "-", "-"); err == nil {
		t.Fatal("stdin as both the link and the content was accepted")
	}
}
