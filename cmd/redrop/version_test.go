package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndCompareSemver(t *testing.T) {
	cases := []struct {
		v    string
		want [3]int
		ok   bool
	}{
		{"v1.2.3", [3]int{1, 2, 3}, true},
		{"1.2.3", [3]int{1, 2, 3}, true},
		{"v0.0.0", [3]int{0, 0, 0}, true},
		{"0.0.0-main+abc1234", [3]int{0, 0, 0}, true},
		{"v10.20.30", [3]int{10, 20, 30}, true},
		{"dev", [3]int{}, false},
		{"preview-abc1234", [3]int{}, false},
		{"", [3]int{}, false},
		{"v1.2", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.v)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSemver(%q) = %v, %v; want %v, %v", c.v, got, ok, c.want, c.ok)
		}
	}
	if c := compareSemver([3]int{1, 2, 3}, [3]int{1, 2, 4}); c != -1 {
		t.Errorf("1.2.3 < 1.2.4, got %d", c)
	}
	if c := compareSemver([3]int{2, 0, 0}, [3]int{1, 9, 9}); c != 1 {
		t.Errorf("2.0.0 > 1.9.9, got %d", c)
	}
	if c := compareSemver([3]int{1, 2, 3}, [3]int{1, 2, 3}); c != 0 {
		t.Errorf("1.2.3 == 1.2.3, got %d", c)
	}
}

func TestVersionOutdated(t *testing.T) {
	cases := []struct {
		cli, min string
		want     bool
	}{
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.1", "v1.0.0", false},
		{"v0.9.9", "v1.0.0", true},
		{"dev", "v1.0.0", false},       // unknown CLI versions never warn
		{"v1.0.0", "", false},          // no stated minimum
		{"v1.0.0", "preview-x", false}, // unparseable minimum never warns
	}
	for _, c := range cases {
		if got := versionOutdated(c.cli, c.min); got != c.want {
			t.Errorf("versionOutdated(%q, %q) = %v, want %v", c.cli, c.min, got, c.want)
		}
	}
}

// versionServer serves /api/v1/version and records whether it was asked.
type versionServer struct {
	body   string
	status int
	asked  int
}

func (s *versionServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/version" {
		s.asked++
		w.Header().Set("Content-Type", "application/json")
		if s.status != 0 {
			w.WriteHeader(s.status)
			fmt.Fprint(w, `{"status":`+fmt.Sprint(s.status)+`,"title":"not found"}`)
			return
		}
		fmt.Fprint(w, s.body)
		return
	}
	http.NotFound(w, r)
}

// `redrop version` prints both sides and warns when the CLI is older than
// the server's minimum supported version.
func TestVersionCommandWarnsOnOldCLI(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	for _, tc := range []struct {
		name       string
		cli        string
		serverBody string
		wantWarn   bool
	}{
		{"older than minimum", "v0.9.0", `{"version":"v1.1.0","commit":"abc1234defg","date":"2026-11-18T00:00:00Z","min_cli_version":"v1.0.0"}`, true},
		{"equal is fine", "v1.0.0", `{"version":"v1.1.0","commit":"abc1234defg","date":"2026-11-18T00:00:00Z","min_cli_version":"v1.0.0"}`, false},
		{"newer is fine", "v1.2.0", `{"version":"v1.1.0","commit":"abc1234defg","date":"2026-11-18T00:00:00Z","min_cli_version":"v1.0.0"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version = tc.cli
			srv := httptest.NewServer(http.HandlerFunc((&versionServer{body: tc.serverBody}).handler))
			t.Cleanup(srv.Close)
			t.Setenv("REDROP_URL", srv.URL)
			t.Setenv("REDROP_TOKEN", "rdp_test")
			var err error
			out, errOut := captureCLIOutput(t, func() {
				err = versionCmd().Execute()
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "redrop "+tc.cli) {
				t.Errorf("stdout missing CLI version: %q", out)
			}
			if !strings.Contains(out, "server v1.1.0") {
				t.Errorf("stdout missing server version: %q", out)
			}
			if tc.wantWarn && !strings.Contains(errOut, "older than the minimum") {
				t.Errorf("stderr missing the warning: %q", errOut)
			}
			if !tc.wantWarn && strings.Contains(errOut, "older than the minimum") {
				t.Errorf("unexpected warning: %q", errOut)
			}
		})
	}
}

// The fallback for servers predating /api/v1/version (a 404 there) reads the
// version from /auth/config and never warns, since it carries no minimum.
func TestVersionCommandFallsBackToAuthConfig(t *testing.T) {
	old := version
	version = "v0.9.0"
	t.Cleanup(func() { version = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"status":404,"title":"not found"}`)
		case "/api/v1/auth/config":
			fmt.Fprint(w, `{"version":"v1.5.0","setup_required":false,"password_login":true,"providers":null,"admin_requires_sso":false,"public_url":"x","content_url":"y"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("REDROP_URL", srv.URL)
	t.Setenv("REDROP_TOKEN", "rdp_test")
	var err error
	out, errOut := captureCLIOutput(t, func() {
		err = versionCmd().Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "server v1.5.0") {
		t.Errorf("fallback version missing: %q", out)
	}
	if strings.Contains(errOut, "warning") {
		t.Errorf("fallback must not warn about a minimum it cannot know: %q", errOut)
	}
}

// --json output carries the fields scripts need, including the update flag.
func TestVersionCommandJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc((&versionServer{
		body: `{"version":"v1.1.0","commit":"abc1234defg","date":"2026-11-18T00:00:00Z","min_cli_version":"v1.0.0"}`,
	}).handler))
	t.Cleanup(srv.Close)
	t.Setenv("REDROP_URL", srv.URL)
	t.Setenv("REDROP_TOKEN", "rdp_test")
	old := version
	version = "v0.9.0"
	t.Cleanup(func() { version = old })
	var err error
	out, _ := captureCLIOutput(t, func() {
		g.json = true
		t.Cleanup(func() { g.json = false })
		err = versionCmd().Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("json output: %v\n%s", err, out)
	}
	for _, k := range []string{"cli", "server", "url", "min_cli"} {
		if _, ok := decoded[k]; !ok {
			t.Errorf("json output missing %q: %s", k, out)
		}
	}
	if decoded["update"] != "required" {
		t.Errorf("update flag = %q, want \"required\"", decoded["update"])
	}
}

// The ldflags stamping: a build with -X must carry the values into the
// binary's reported version. This compiles and runs the CLI with stamps.
func TestVersionStamping(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the CLI again")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "redrop-stamped")
	build := exec.Command("go", "build", "-ldflags",
		"-X main.version=v9.9.9 -X main.commit=deadbeefcafe -X main.date=2026-11-18T00:00:00Z",
		"-o", bin, ".")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("go build unavailable: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "version")
	cmd.Env = append(os.Environ(), "REDROP_CONFIG="+filepath.Join(dir, "missing.json"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run stamped binary: %v\n%s", err, out)
	}
	for _, want := range []string{"redrop v9.9.9", "deadbeefcafe"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("stamped output missing %q: %s", want, out)
		}
	}
	// No config and no --url: the command printed only the CLI lines, which
	// is what the assertions above checked, without contacting a server.
}

// repoRoot is the directory holding go.mod: the package under test, or its
// parent when the test binary is invoked from a subdirectory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
