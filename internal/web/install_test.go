package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// runInstaller serves the real installer and a fake CLI build, then runs the
// installer the way the dashboard's command does.
func runInstaller(t *testing.T, token, loginExit string) (string, string, error) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh targets Linux and macOS")
	}
	cliDir := t.TempDir()
	name := "redrop-" + runtime.GOOS + "-" + runtime.GOARCH
	if err := os.WriteFile(filepath.Join(cliDir, name), []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(fakeCLI))
	if err := os.WriteFile(filepath.Join(cliDir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	h = New(nil, cliDir, srv.URL).Installers()

	res, err := http.Get(srv.URL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	work := t.TempDir()
	logPath := filepath.Join(work, "log")
	cmd := exec.Command("sh")
	cmd.Stdin = res.Body
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + work,
		"REDROP_INSTALL_DIR=" + filepath.Join(work, "bin"),
		"FAKE_LOG=" + logPath,
		"FAKE_LOGIN_EXIT=" + loginExit,
	}
	if token != "" {
		cmd.Env = append(cmd.Env, "REDROP_TOKEN="+token)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	log, _ := os.ReadFile(logPath)
	return string(log), stderr.String(), runErr
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
