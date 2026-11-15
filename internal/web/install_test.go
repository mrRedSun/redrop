package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

const testBase = "https://redrop.example.com"

func render(t *testing.T, name, base string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := installers.ExecuteTemplate(&buf, name, map[string]string{"Base": base}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestInstallShParses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	script := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(script, []byte(render(t, "install.sh.tmpl", testBase)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sh, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// pwsh is not available on the development host or in CI, so the PowerShell
// installer is only checked for the properties the one-line setup relies on.
func TestInstallPs1ClearsToken(t *testing.T) {
	ps := render(t, "install.ps1.tmpl", testBase)
	for _, want := range []string{
		"$base = '" + testBase + "'",
		"$token = $env:REDROP_TOKEN\nRemove-Item Env:REDROP_TOKEN",
		"} finally {\n  $token = $null\n  Remove-Item Env:REDROP_TOKEN",
		"$token | & $exe login --url $base --token-stdin",
		"& $exe --url $base whoami",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("install.ps1 is missing %q", want)
		}
	}
}

// fakeCLI is served as the downloaded binary. It records its argv and
// REDROP_TOKEN, and for login also its stdin, then exits with $FAKE_LOGIN_EXIT.
const fakeCLI = `#!/bin/sh
log="$FAKE_LOG"
printf 'argv:%s env:%s\n' "$*" "${REDROP_TOKEN-unset}" >> "$log"
if [ "$1" = login ]; then
  printf 'stdin:%s\n' "$(cat)" >> "$log"
  exit "${FAKE_LOGIN_EXIT:-0}"
fi
`

// installFixture is a fake CLI build: the binary plus SHA256SUMS, served by
// the real Installers handler, and a work directory to install into.
type installFixture struct {
	cliDir, name, work, bin, log string
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh targets Linux and macOS")
	}
	f := &installFixture{cliDir: t.TempDir(), name: "redrop-" + runtime.GOOS + "-" + runtime.GOARCH, work: t.TempDir()}
	f.bin = filepath.Join(f.work, "bin")
	f.log = filepath.Join(f.work, "log")
	if err := os.WriteFile(filepath.Join(f.cliDir, f.name), []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(fakeCLI))
	f.writeSums(t, hex.EncodeToString(sum[:]))
	return f
}

func (f *installFixture) writeSums(t *testing.T, sum string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.cliDir, "SHA256SUMS"), []byte(sum+"  "+f.name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// serve starts a server for the installers with the script's base URL set
// to the server itself, and counts its requests.
func (f *installFixture) serve(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			requests.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h = New(nil, f.cliDir, srv.URL).Installers()
	return srv
}

// run pipes script into sh the way the dashboard's command does.
func (f *installFixture) run(script io.Reader, token, loginExit string, env ...string) (string, string, error) {
	cmd := exec.Command("sh")
	cmd.Stdin = script
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.work,
		"REDROP_INSTALL_DIR=" + f.bin,
		"FAKE_LOG=" + f.log,
		"FAKE_LOGIN_EXIT=" + loginExit,
	}, env...)
	if token != "" {
		cmd.Env = append(cmd.Env, "REDROP_TOKEN="+token)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	log, _ := os.ReadFile(f.log)
	return string(log), stderr.String(), runErr
}

// binEntries lists the install directory, to catch leftover staged downloads.
func (f *installFixture) binEntries(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.bin)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// runInstaller serves the real installer and a fake CLI build, then runs the
// installer the way the dashboard's command does.
func runInstaller(t *testing.T, token, loginExit string) (string, string, error) {
	t.Helper()
	f := newInstallFixture(t)
	res, err := http.Get(f.serve(t, nil).URL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return f.run(res.Body, token, loginExit)
}

func TestInstallShLogsInWithToken(t *testing.T) {
	const tok = "rdp_test-token"
	log, stderr, err := runInstaller(t, tok, "0")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	want := []string{
		"argv:version env:unset",
		"argv:login --url http://", // server URL follows
		"stdin:" + tok,
		"argv:--url http://",
	}
	if len(lines) != len(want) {
		t.Fatalf("unexpected CLI calls:\n%s", log)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("call %d = %q, want prefix %q", i, lines[i], w)
		}
	}
	if !strings.HasSuffix(lines[3], " whoami env:unset") || !strings.HasSuffix(lines[1], " --token-stdin env:unset") {
		t.Errorf("unexpected login/whoami calls:\n%s", log)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "argv:") && strings.Contains(l, tok) {
			t.Errorf("token reached argv or the environment: %q", l)
		}
	}
}

func TestInstallShLoginFailure(t *testing.T) {
	log, stderr, err := runInstaller(t, "rdp_test-token", "3")
	if err == nil {
		t.Fatal("installer succeeded although login failed")
	}
	if !strings.Contains(stderr, "redrop is installed at") || !strings.Contains(stderr, "login --url") {
		t.Errorf("failure message lacks install path or retry hint:\n%s", stderr)
	}
	if strings.Contains(log, "whoami") {
		t.Error("whoami ran after a failed login")
	}
}

func TestInstallShWithoutToken(t *testing.T) {
	log, stderr, err := runInstaller(t, "", "0")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, stderr)
	}
	if strings.TrimSpace(log) != "argv:version env:unset" {
		t.Errorf("installer without a token should only print the version:\n%s", log)
	}
}

// The download is staged next to its destination and renamed into place, so
// a successful install leaves only the binary behind.
func TestInstallShLeavesOnlyTheBinary(t *testing.T) {
	f := newInstallFixture(t)
	res, err := http.Get(f.serve(t, nil).URL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, stderr, err := f.run(res.Body, "", "0"); err != nil {
		t.Fatalf("installer failed: %v\n%s", err, stderr)
	}
	if got := f.binEntries(t); !slices.Equal(got, []string{"redrop"}) {
		t.Fatalf("install dir holds %q; want only redrop", got)
	}
	st, err := os.Stat(filepath.Join(f.bin, "redrop"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary: %v, mode %v", err, st.Mode())
	}
}

// A download that does not match SHA256SUMS never replaces the installed
// binary and leaves no staged file behind.
func TestInstallShChecksumMismatch(t *testing.T) {
	f := newInstallFixture(t)
	f.writeSums(t, strings.Repeat("0", 64))
	if err := os.MkdirAll(f.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(f.bin, "redrop")
	if err := os.WriteFile(old, []byte("previous build"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(f.serve(t, nil).URL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	log, stderr, err := f.run(res.Body, "rdp_test-token", "0")
	if err == nil || !strings.Contains(stderr, "checksum mismatch") {
		t.Fatalf("installer = %v; want a checksum mismatch\n%s", err, stderr)
	}
	if log != "" {
		t.Errorf("the unverified download ran:\n%s", log)
	}
	if got, _ := os.ReadFile(old); string(got) != "previous build" {
		t.Errorf("installed binary was replaced: %q", got)
	}
	if got := f.binEntries(t); !slices.Equal(got, []string{"redrop"}) {
		t.Fatalf("install dir holds %q; want only the previous redrop", got)
	}
}

// Every prefix of the script that stops before its last line, as a cut-off
// "curl | sh" would deliver, runs nothing: no download, no CLI, no files.
func TestInstallShTruncatedRunsNothing(t *testing.T) {
	f := newInstallFixture(t)
	var requests atomic.Int32
	srv := f.serve(t, &requests)
	script := render(t, "install.sh.tmpl", srv.URL)
	lines := strings.SplitAfter(script, "\n")
	if last := lines[len(lines)-2]; last != "main \"$@\"\n" || lines[len(lines)-1] != "" {
		t.Fatalf("script must end with the main call, got %q", last)
	}
	for i := range len(lines) - 2 {
		prefix := strings.Join(lines[:i+1], "")
		f.run(strings.NewReader(prefix), "rdp_test-token", "0")
		if n := requests.Load(); n != 0 {
			t.Fatalf("script cut after line %d made %d requests", i+1, n)
		}
		if _, err := os.Stat(f.log); err == nil {
			t.Fatalf("script cut after line %d ran the CLI", i+1)
		}
		if _, err := os.Stat(f.bin); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("script cut after line %d created the install dir: %v", i+1, err)
		}
	}
}

// With an HTTPS base URL, curl refuses a redirect to plain HTTP. The
// control run proves the TLS setup itself works.
func TestInstallShRefusesHTTPSDowngrade(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	f := newInstallFixture(t)
	var plainRequests atomic.Int32
	plain := f.serve(t, &plainRequests)
	var downgrade atomic.Bool
	files := New(nil, f.cliDir, "").Installers()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if downgrade.Load() {
			http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(tlsSrv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	script := render(t, "install.sh.tmpl", tlsSrv.URL)

	if _, stderr, err := f.run(strings.NewReader(script), "", "0", "CURL_CA_BUNDLE="+ca); err != nil {
		t.Fatalf("install over HTTPS failed: %v\n%s", err, stderr)
	}
	if err := os.RemoveAll(f.bin); err != nil {
		t.Fatal(err)
	}
	downgrade.Store(true)
	_, stderr, err := f.run(strings.NewReader(script), "", "0", "CURL_CA_BUNDLE="+ca)
	if err == nil || !strings.Contains(stderr, "download failed") {
		t.Fatalf("installer = %v; want the downgraded download to fail\n%s", err, stderr)
	}
	if n := plainRequests.Load(); n != 0 {
		t.Fatalf("curl followed the redirect to HTTP (%d requests)", n)
	}
	if _, err := os.Stat(filepath.Join(f.bin, "redrop")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a binary was installed: %v", err)
	}
}
