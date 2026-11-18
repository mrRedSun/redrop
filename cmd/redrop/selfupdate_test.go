package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeReleaseServer stands in for GitHub: /repos/<repo>/releases/... for the
// API and /download/... for the assets, hosted on one httptest server so
// asset URLs resolve against it.
type fakeReleaseServer struct {
	tag    string
	binary []byte // served as the asset; "" means 404 on the asset
	sums   string // overrides the generated SHA256SUMS when non-empty
	noSums bool   // 404 on SHA256SUMS
	// apiRequests records API paths served.
	apiRequests []string
}

func (f *fakeReleaseServer) assetName() string {
	return assetName("linux", "amd64")
}

func (f *fakeReleaseServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			f.apiRequests = append(f.apiRequests, r.URL.Path)
			assets := "[]"
			if f.binary != nil {
				name := f.assetName()
				assets = fmt.Sprintf(`[{"name":%q,"browser_download_url":"%s/download/%s"}]`, name, serverBase(r), name)
			}
			fmt.Fprintf(w, `{"tag_name":%q,"assets":%s}`, f.tag, assets)
		case strings.HasPrefix(r.URL.Path, "/download/"):
			file := strings.TrimPrefix(r.URL.Path, "/download/")
			if file == "SHA256SUMS" {
				if f.noSums {
					http.NotFound(w, r)
					return
				}
				if f.sums != "" {
					fmt.Fprint(w, f.sums)
					return
				}
				sum := sha256.Sum256(f.binary)
				fmt.Fprintf(w, "%x  %s\n", sum, f.assetName())
				return
			}
			if file == f.assetName() && f.binary != nil {
				w.Write(f.binary)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

// serverBase reconstructs the server's own base URL from an incoming request.
func serverBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func updateOpts(t *testing.T, apiBase string) selfUpdateOptions {
	t.Helper()
	return selfUpdateOptions{
		apiBase: apiBase,
		repo:    "example/redrop",
		current: "v1.0.0",
		goos:    "linux",
		goarch:  "amd64",
		exe:     filepath.Join(t.TempDir(), "redrop"),
		ua:      "redrop-test",
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// selfUpdate downloads, verifies the checksum, and atomically replaces the
// binary: the old inode's content survives for an open reader, and the new
// path has the downloaded bytes.
func TestSelfUpdateAtomicReplace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX rename semantics")
	}
	newBinary := "#!/bin/sh\necho v1.1.0\n"
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte(newBinary)}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	o := updateOpts(t, srv.URL)
	mustWrite(t, o.exe, "old binary")
	old, err := os.Open(o.exe)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	res, err := selfUpdate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Latest != "1.1.0" {
		t.Fatalf("result = %+v", res)
	}
	got, err := os.ReadFile(o.exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != newBinary {
		t.Fatalf("replaced binary = %q", got)
	}
	st, err := os.Stat(o.exe)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("replaced mode = %04o, want 0755", st.Mode().Perm())
	}
	// The open file descriptor still reads the old bytes: the replacement
	// was a rename, not an in-place write.
	buf := make([]byte, len("old binary"))
	if _, err := old.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "old binary" {
		t.Errorf("old inode was overwritten in place: %q", buf)
	}
	// No staging litter beside the binary.
	entries, err := os.ReadDir(filepath.Dir(o.exe))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "redrop" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %v; staging file leaked", names)
	}
}

// A checksum mismatch refuses to install anything, and the old binary stays.
func TestSelfUpdateChecksumMismatch(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("malicious replacement")}
	// SHA256SUMS claims a digest for a different file.
	sum := sha256.Sum256([]byte("something else"))
	f.sums = fmt.Sprintf("%x  %s\n", sum, f.assetName())
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	o := updateOpts(t, srv.URL)
	mustWrite(t, o.exe, "old binary")
	_, err := selfUpdate(context.Background(), o)
	requireExitCode(t, err, exitGeneral)
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v", err)
	}
	got, _ := os.ReadFile(o.exe)
	if string(got) != "old binary" {
		t.Fatalf("binary was replaced despite the mismatch: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(o.exe))
	if len(entries) != 1 {
		t.Errorf("staging leaked after mismatch: %d entries", len(entries))
	}
}

// --check reports the release but never touches the binary.
func TestSelfUpdateCheckOnly(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new")}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	o := updateOpts(t, srv.URL)
	o.checkOnly = true
	mustWrite(t, o.exe, "old binary")
	res, err := selfUpdate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated || !res.CheckOnly || res.Latest != "1.1.0" {
		t.Fatalf("result = %+v", res)
	}
	got, _ := os.ReadFile(o.exe)
	if string(got) != "old binary" {
		t.Fatalf("--check replaced the binary: %q", got)
	}
}

// An unwritable directory is refused before anything is downloaded.
func TestSelfUpdateRefusesUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new")}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	o := updateOpts(t, srv.URL)
	o.exe = filepath.Join(dir, "redrop")
	mustWrite(t, o.exe, "old binary")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, err := selfUpdate(context.Background(), o)
	var ee *exitError
	if !asExit(err, &ee) || ee.code != exitGeneral {
		t.Fatalf("error = %v, want exitGeneral", err)
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("error = %v", err)
	}
	if len(f.apiRequests) != 1 {
		t.Errorf("download continued after the writable check: %v", f.apiRequests)
	}
	got, _ := os.ReadFile(o.exe)
	if string(got) != "old binary" {
		t.Fatalf("binary changed: %q", got)
	}
}

func asExit(err error, target **exitError) bool {
	if e, ok := err.(*exitError); ok {
		*target = e
		return true
	}
	return false
}

// A binary in a package-manager location is refused.
func TestSelfUpdateRefusesManagedLocations(t *testing.T) {
	for _, prefix := range managedPrefixes {
		exe := filepath.Join(strings.TrimSuffix(prefix, "/"), "redrop")
		err := checkWritable(exe)
		var ee *exitError
		if !asExit(err, &ee) || ee.code != exitUsage {
			t.Errorf("checkWritable(%s) = %v, want exitUsage", exe, err)
		}
	}
}

// A pinned --version that equals the running version still reports the
// release without rewriting the binary; an unpinned call on the latest
// version is reported as up to date.
func TestSelfUpdateUpToDate(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.0.0", binary: []byte("same")}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	o := updateOpts(t, srv.URL)
	mustWrite(t, o.exe, "old binary")
	res, err := selfUpdate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatalf("already-current release was installed: %+v", res)
	}
	got, _ := os.ReadFile(o.exe)
	if string(got) != "old binary" {
		t.Fatalf("binary changed on a no-op update: %q", got)
	}
}

// The API asks /releases/tags/vX.Y.Z for a pinned --version and
// /releases/latest otherwise.
func TestSelfUpdateReleaseLookup(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want string
	}{
		{"", "/repos/example/redrop/releases/latest"},
		{"v1.2.3", "/repos/example/redrop/releases/tags/v1.2.3"},
	} {
		f := &fakeReleaseServer{tag: "v1.1.0", binary: nil}
		srv := httptest.NewServer(f.handler(t))
		o := updateOpts(t, srv.URL)
		o.tag = tc.tag
		o.checkOnly = true // enough to reach the lookup
		if _, err := selfUpdate(context.Background(), o); err != nil {
			t.Logf("tag=%q: %v", tc.tag, err)
		}
		srv.Close()
		if len(f.apiRequests) == 0 || f.apiRequests[0] != tc.want {
			t.Errorf("tag=%q asked %v, want %s", tc.tag, f.apiRequests, tc.want)
		}
	}
}

// A release without this platform's asset is a clean not-found.
func TestSelfUpdateMissingAsset(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.1.0", binary: nil} // asset 404s
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	o := updateOpts(t, srv.URL)
	mustWrite(t, o.exe, "old binary")
	_, err := selfUpdate(context.Background(), o)
	requireExitCode(t, err, exitNotFound)
	if !strings.Contains(err.Error(), "no redrop-linux-amd64 asset") {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyChecksum(t *testing.T) {
	body := []byte("the binary")
	sum := sha256.Sum256(body)
	name := "redrop-linux-amd64"
	ok := fmt.Sprintf("%x  %s\n", sum, name)
	single := fmt.Sprintf("%x %s\n", sum, name)
	wrong := fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("other")), name)
	missing := fmt.Sprintf("%x  redrop-darwin-arm64\n", sum)

	for _, tc := range []struct {
		sums string
		want bool
	}{
		{ok, true},
		{single, true},             // single-space separator
		{wrong, false},             // digest of different bytes
		{missing, false},           // entry for another file
		{"", false},                // empty sums
		{"garbage\n\n" + ok, true}, // leading junk lines are skipped
	} {
		if got := verifyChecksum([]byte(tc.sums), name, sum); got != tc.want {
			t.Errorf("verifyChecksum(%q) = %v, want %v", tc.sums, got, tc.want)
		}
	}
}
