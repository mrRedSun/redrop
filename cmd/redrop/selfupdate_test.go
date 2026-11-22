package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
// asset URLs resolve against it. SHA256SUMS is listed in the release's
// assets under /sums/, a different directory from the binary, so a client
// that guesses the sums URL from the binary's URL fails.
type fakeReleaseServer struct {
	tag         string
	binary      []byte // served as the asset; nil means the asset is not listed
	sums        string // overrides the generated SHA256SUMS when non-empty
	noSumsAsset bool   // SHA256SUMS not listed in the release
	assetURL    string // overrides the binary's browser_download_url
	// redirect answers one kind of request ("api", "sums" or "binary") with
	// a 302: to redirectLoopback (the same content under /mirror on this
	// loopback server), redirectLoop (itself), or any other URL as given.
	redirect map[string]string
	// apiRequests and downloads record the paths served.
	apiRequests []string
	downloads   []string
}

const (
	redirectLoopback = "loopback"
	redirectLoop     = "loop"
)

// requestKind names what a path asks the fake release server for.
func (f *fakeReleaseServer) requestKind(path string) string {
	switch {
	case strings.HasPrefix(path, "/repos/"):
		return "api"
	case path == "/sums/SHA256SUMS":
		return "sums"
	case strings.HasPrefix(path, "/download/"):
		return "binary"
	}
	return ""
}

func (f *fakeReleaseServer) assetName() string {
	return assetName("linux", "amd64")
}

func (f *fakeReleaseServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		path, mirrored := strings.CutPrefix(r.URL.Path, "/mirror")
		if target, ok := f.redirect[f.requestKind(path)]; ok && !mirrored {
			switch target {
			case redirectLoopback:
				target = base + "/mirror" + path
			case redirectLoop:
				target = base + path
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		switch {
		case strings.HasPrefix(path, "/repos/"):
			f.apiRequests = append(f.apiRequests, path)
			var assets []map[string]string
			if f.binary != nil {
				u := f.assetURL
				if u == "" {
					u = base + "/download/" + f.assetName()
				}
				assets = append(assets, map[string]string{"name": f.assetName(), "browser_download_url": u})
			}
			if !f.noSumsAsset {
				assets = append(assets, map[string]string{"name": "SHA256SUMS", "browser_download_url": base + "/sums/SHA256SUMS"})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "assets": assets})
		case path == "/sums/SHA256SUMS":
			f.downloads = append(f.downloads, path)
			if f.sums != "" {
				fmt.Fprint(w, f.sums)
				return
			}
			fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(f.binary), f.assetName())
		case path == "/download/"+f.assetName() && f.binary != nil:
			f.downloads = append(f.downloads, path)
			w.Write(f.binary)
		default:
			http.NotFound(w, r)
		}
	}
}

func newFakeRelease(t *testing.T, f *fakeReleaseServer) string {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return srv.URL
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

// requireUntouched checks that the binary still holds its old bytes and that
// no staging file was left beside it.
func requireUntouched(t *testing.T, exe, want string) {
	t.Helper()
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("binary changed: %q", got)
	}
	requireDirectoryNames(t, filepath.Dir(exe), []string{filepath.Base(exe)})
}

// selfUpdate downloads, verifies the checksum, and atomically replaces the
// binary: the old inode's content survives for an open reader, the new path
// has the downloaded bytes, and the old file's mode is kept.
func TestSelfUpdateAtomicReplace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX rename semantics")
	}
	newBinary := "#!/bin/sh\necho v1.1.0\n"
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte(newBinary)}
	o := updateOpts(t, newFakeRelease(t, f))
	mustWrite(t, o.exe, "old binary")
	if err := os.Chmod(o.exe, 0o750); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(o.exe)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	res, err := selfUpdate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || !res.UpdateAvailable || res.Downgraded || res.Latest != "1.1.0" || res.Current != "1.0.0" {
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
	if st.Mode().Perm() != 0o750 {
		t.Errorf("replaced mode = %04o, want the old 0750", st.Mode().Perm())
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
	requireDirectoryNames(t, filepath.Dir(o.exe), []string{"redrop"})
}

// A checksum mismatch refuses to install anything, and the old binary stays.
func TestSelfUpdateChecksumMismatch(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("malicious replacement")}
	f.sums = fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("something else")), f.assetName())
	o := updateOpts(t, newFakeRelease(t, f))
	mustWrite(t, o.exe, "old binary")
	_, err := selfUpdate(context.Background(), o)
	requireExitCode(t, err, exitGeneral)
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v", err)
	}
	requireUntouched(t, o.exe, "old binary")
}

// SHA256SUMS that names other files only is refused, before the asset is
// downloaded at all.
func TestSelfUpdateSumsMissingEntry(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new")}
	f.sums = fmt.Sprintf("%x  redrop-darwin-arm64\n", sha256.Sum256([]byte("new")))
	o := updateOpts(t, newFakeRelease(t, f))
	mustWrite(t, o.exe, "old binary")
	_, err := selfUpdate(context.Background(), o)
	requireExitCode(t, err, exitGeneral)
	if !strings.Contains(err.Error(), "no checksum for redrop-linux-amd64") {
		t.Fatalf("error = %v", err)
	}
	if len(f.downloads) != 1 {
		t.Errorf("downloads = %v; the binary must not be fetched without a checksum", f.downloads)
	}
	requireUntouched(t, o.exe, "old binary")
}

// m-7: an oversized SHA256SUMS is refused without being buffered whole, and
// an asset over the cap is an error, not a truncated "checksum mismatch".
func TestSelfUpdateSizeCaps(t *testing.T) {
	t.Run("sums", func(t *testing.T) {
		f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new")}
		f.sums = strings.Repeat("\n", maxSumsSize+1)
		o := updateOpts(t, newFakeRelease(t, f))
		mustWrite(t, o.exe, "old binary")
		_, err := selfUpdate(context.Background(), o)
		requireExitCode(t, err, exitGeneral)
		if !strings.Contains(err.Error(), "SHA256SUMS is larger than") {
			t.Fatalf("error = %v", err)
		}
		requireUntouched(t, o.exe, "old binary")
	})
	t.Run("asset", func(t *testing.T) {
		old := maxAssetSize
		maxAssetSize = 16
		t.Cleanup(func() { maxAssetSize = old })
		f := &fakeReleaseServer{tag: "v1.1.0", binary: bytes.Repeat([]byte("x"), 17)}
		o := updateOpts(t, newFakeRelease(t, f))
		mustWrite(t, o.exe, "old binary")
		_, err := selfUpdate(context.Background(), o)
		requireExitCode(t, err, exitGeneral)
		if !strings.Contains(err.Error(), "is larger than") {
			t.Fatalf("error = %v", err)
		}
		requireUntouched(t, o.exe, "old binary")
	})
}

// M-1: --check never touches the binary, and update_available says whether
// a newer release exists.
func TestSelfUpdateCheckOnly(t *testing.T) {
	for _, tc := range []struct {
		name, tag string
		available bool
	}{
		{"newer release", "v1.1.0", true},
		{"already current", "v1.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeReleaseServer{tag: tc.tag, binary: []byte("new")}
			o := updateOpts(t, newFakeRelease(t, f))
			o.checkOnly = true
			mustWrite(t, o.exe, "old binary")
			res, err := selfUpdate(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if res.Updated || !res.CheckOnly || res.UpdateAvailable != tc.available {
				t.Fatalf("result = %+v; want update_available=%v", res, tc.available)
			}
			if len(f.downloads) != 0 {
				t.Errorf("--check downloaded %v", f.downloads)
			}
			requireUntouched(t, o.exe, "old binary")
		})
	}
}

// M-1: the command's own output for --check, in text and JSON, against an
// up-to-date and an outdated CLI. --check never writes, so the command can
// run in-process on the test binary.
func TestSelfUpdateCheckOutput(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v1.0.0"
	for _, tc := range []struct {
		tag, wantText string
		available     bool
	}{
		{"v1.0.0", "redrop 1.0.0 is up to date (latest release 1.0.0)", false},
		{"v1.1.0", "update available: ", true},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			f := &fakeReleaseServer{tag: tc.tag, binary: []byte("new")}
			t.Setenv("REDROP_GITHUB_API", newFakeRelease(t, f))
			var err error
			out, _ := captureCLIOutput(t, func() {
				c := selfUpdateCmd()
				c.SetArgs([]string{"--check"})
				err = c.Execute()
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.wantText) {
				t.Errorf("text output %q lacks %q", out, tc.wantText)
			}
			out, _ = captureCLIOutput(t, func() {
				g.json = true
				c := selfUpdateCmd()
				c.SetArgs([]string{"--check"})
				err = c.Execute()
			})
			if err != nil {
				t.Fatal(err)
			}
			var res selfUpdateResult
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("json: %v\n%s", err, out)
			}
			if res.UpdateAvailable != tc.available || res.Current != "1.0.0" || !strings.HasPrefix(tc.tag, "v"+res.Latest) {
				t.Errorf("json = %+v", res)
			}
		})
	}
}

// M-2: a main build is not compared with the releases. Without --version
// self-update refuses and names the flag; --check reports without offering
// an update; with --version it installs.
func TestSelfUpdateMainBuild(t *testing.T) {
	const mainVersion = "0.0.0-main+abc123456789"
	t.Run("refuses without --version", func(t *testing.T) {
		f := &fakeReleaseServer{tag: "v0.5.0", binary: []byte("release")}
		o := updateOpts(t, newFakeRelease(t, f))
		o.current = mainVersion
		mustWrite(t, o.exe, "main build")
		_, err := selfUpdate(context.Background(), o)
		requireExitCode(t, err, exitUsage)
		if !strings.Contains(err.Error(), "--version v0.5.0") {
			t.Fatalf("error = %v", err)
		}
		requireUntouched(t, o.exe, "main build")
	})
	t.Run("check reports only", func(t *testing.T) {
		f := &fakeReleaseServer{tag: "v0.5.0", binary: []byte("release")}
		o := updateOpts(t, newFakeRelease(t, f))
		o.current = mainVersion
		o.checkOnly = true
		mustWrite(t, o.exe, "main build")
		res, err := selfUpdate(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if !res.MainBuild || res.UpdateAvailable || res.Updated {
			t.Fatalf("result = %+v", res)
		}
		requireUntouched(t, o.exe, "main build")
	})
	t.Run("--version switches", func(t *testing.T) {
		f := &fakeReleaseServer{tag: "v0.5.0", binary: []byte("release")}
		o := updateOpts(t, newFakeRelease(t, f))
		o.current = mainVersion
		o.tag = "v0.5.0"
		mustWrite(t, o.exe, "main build")
		res, err := selfUpdate(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		// Not comparable, so not a downgrade either.
		if !res.Updated || res.Downgraded {
			t.Fatalf("result = %+v", res)
		}
	})
}

// m-5: --version to an older release installs it (the rollback path) and
// reports the downgrade.
func TestSelfUpdateDowngrade(t *testing.T) {
	f := &fakeReleaseServer{tag: "v0.5.0", binary: []byte("older")}
	o := updateOpts(t, newFakeRelease(t, f))
	o.tag = "v0.5.0"
	mustWrite(t, o.exe, "old binary")
	res, err := selfUpdate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || !res.Downgraded {
		t.Fatalf("result = %+v", res)
	}
}

// An unwritable directory is refused before anything is downloaded.
func TestSelfUpdateRefusesUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new")}
	dir := t.TempDir()
	o := updateOpts(t, newFakeRelease(t, f))
	o.exe = filepath.Join(dir, "redrop")
	mustWrite(t, o.exe, "old binary")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, err := selfUpdate(context.Background(), o)
	requireExitCode(t, err, exitGeneral)
	if !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("error = %v", err)
	}
	if len(f.downloads) != 0 {
		t.Errorf("downloaded %v after the writable check failed", f.downloads)
	}
	got, _ := os.ReadFile(o.exe)
	if string(got) != "old binary" {
		t.Fatalf("binary changed: %q", got)
	}
}

// m-1: system and package-manager trees are refused (Homebrew's Cellar,
// which is what EvalSymlinks yields for a brew install, Linuxbrew, Nix);
// the installer's own root location is not.
func TestManagedPaths(t *testing.T) {
	for _, exe := range []string{
		"/usr/bin/redrop", "/bin/redrop", "/sbin/redrop", "/usr/sbin/redrop",
		"/opt/homebrew/bin/redrop",
		"/usr/local/Cellar/redrop/1.0.0/bin/redrop",
		"/home/linuxbrew/.linuxbrew/Cellar/redrop/1.0.0/bin/redrop",
		"/home/u/.linuxbrew/bin/redrop",
		"/nix/store/abc-redrop-1.0.0/bin/redrop",
	} {
		if !isManagedPath(exe) {
			t.Errorf("%s is not treated as managed", exe)
		}
		var ee *exitError
		if err := checkWritable(exe); !asExit(err, &ee) || ee.code != exitUsage {
			t.Errorf("checkWritable(%s) = %v, want exitUsage", exe, err)
		}
	}
	for _, exe := range []string{
		"/usr/local/bin/redrop", // install.sh as root
		"/home/u/.local/bin/redrop",
		"/opt/tools/redrop",
	} {
		if isManagedPath(exe) {
			t.Errorf("%s is wrongly treated as managed", exe)
		}
	}
}

func asExit(err error, target **exitError) bool {
	if e, ok := err.(*exitError); ok {
		*target = e
		return true
	}
	return false
}

// An unpinned call on the latest version is reported as up to date and
// leaves the binary alone.
func TestSelfUpdateUpToDate(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.0.0", binary: []byte("same")}
	o := updateOpts(t, newFakeRelease(t, f))
	mustWrite(t, o.exe, "old binary")
	res, err := selfUpdate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated || res.UpdateAvailable {
		t.Fatalf("already-current release was installed or offered: %+v", res)
	}
	requireUntouched(t, o.exe, "old binary")
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
		f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new")}
		o := updateOpts(t, newFakeRelease(t, f))
		o.tag = tc.tag
		o.checkOnly = true
		if _, err := selfUpdate(context.Background(), o); err != nil {
			t.Fatalf("tag=%q: %v", tc.tag, err)
		}
		if len(f.apiRequests) != 1 || f.apiRequests[0] != tc.want {
			t.Errorf("tag=%q asked %v, want %s", tc.tag, f.apiRequests, tc.want)
		}
	}
}

// A release without this platform's binary, or without SHA256SUMS, is a
// clean not-found.
func TestSelfUpdateMissingAssets(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fakeReleaseServer
		want string
	}{
		{"binary", &fakeReleaseServer{tag: "v1.1.0"}, "no redrop-linux-amd64 asset"},
		{"sums", &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new"), noSumsAsset: true}, "no SHA256SUMS asset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := updateOpts(t, newFakeRelease(t, tc.f))
			mustWrite(t, o.exe, "old binary")
			_, err := selfUpdate(context.Background(), o)
			requireExitCode(t, err, exitNotFound)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
			requireUntouched(t, o.exe, "old binary")
		})
	}
}

// m-2: a release whose asset URL is plain HTTP on another host is refused
// before anything is downloaded.
func TestSelfUpdateRefusesNonHTTPSAsset(t *testing.T) {
	f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new"), assetURL: "http://downloads.example/redrop-linux-amd64"}
	o := updateOpts(t, newFakeRelease(t, f))
	mustWrite(t, o.exe, "old binary")
	_, err := selfUpdate(context.Background(), o)
	requireExitCode(t, err, exitGeneral)
	if !strings.Contains(err.Error(), "non-HTTPS") {
		t.Fatalf("error = %v", err)
	}
	if len(f.downloads) != 0 {
		t.Errorf("downloaded %v", f.downloads)
	}
	requireUntouched(t, o.exe, "old binary")
}

// m-2: the HTTPS rule holds through redirects. A release lookup, checksum
// or binary request redirected to plain HTTP on another host is refused
// before that host is contacted, with the binary untouched; a redirect that
// stays on loopback is followed; a redirect loop stops at maxRedirects.
func TestSelfUpdateRedirectPolicy(t *testing.T) {
	const hostile = "http://release-mirror.invalid/redrop"
	for _, tc := range []struct {
		kind, target string
		want         string // error substring; empty means the update succeeds
		downloads    int    // downloads the fake served before the refusal
	}{
		{"api", hostile, "refusing redirect", 0},
		{"sums", hostile, "refusing redirect", 0},
		{"binary", hostile, "refusing redirect", 1}, // the sums came first
		{"api", redirectLoopback, "", 2},
		{"sums", redirectLoopback, "", 2},
		{"binary", redirectLoopback, "", 2},
		{"binary", redirectLoop, "stopped after 10 redirects", 1},
	} {
		t.Run(tc.kind+"->"+tc.target, func(t *testing.T) {
			f := &fakeReleaseServer{tag: "v1.1.0", binary: []byte("new release"), redirect: map[string]string{tc.kind: tc.target}}
			o := updateOpts(t, newFakeRelease(t, f))
			mustWrite(t, o.exe, "old binary")
			res, err := selfUpdate(context.Background(), o)
			if len(f.downloads) != tc.downloads {
				t.Errorf("downloads = %v, want %d", f.downloads, tc.downloads)
			}
			if tc.want == "" {
				if err != nil || !res.Updated {
					t.Fatalf("allowed redirect: %+v, %v", res, err)
				}
				got, _ := os.ReadFile(o.exe)
				if string(got) != "new release" {
					t.Fatalf("installed %q", got)
				}
				return
			}
			requireExitCode(t, err, exitGeneral)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			requireUntouched(t, o.exe, "old binary")
		})
	}
}

func TestCheckUpdateRedirect(t *testing.T) {
	req := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	from := []*http.Request{req("https://api.github.com/repos/x/y/releases/latest")}
	for u, ok := range map[string]bool{
		"https://objects.githubusercontent.com/x": true,
		"http://127.0.0.1:8080/x":                 true,
		"http://objects.githubusercontent.com/x":  false,
		"ftp://objects.githubusercontent.com/x":   false,
	} {
		if err := checkUpdateRedirect(req(u), from); (err == nil) != ok {
			t.Errorf("redirect to %s: %v, want ok=%v", u, err, ok)
		}
	}
	long := make([]*http.Request, maxRedirects)
	for i := range long {
		long[i] = from[0]
	}
	if err := checkUpdateRedirect(req("https://objects.githubusercontent.com/x"), long); err == nil {
		t.Errorf("redirect %d was followed", maxRedirects+1)
	}
}

// m-5: the downgrade warning goes to stderr in both output modes, and with
// --json stdout stays exactly one JSON document.
func TestReportSelfUpdateDowngradeWarning(t *testing.T) {
	res := selfUpdateResult{Current: "1.0.0", Latest: "0.5.0", UpdateAvailable: true, Updated: true, Downgraded: true, Replacement: "/tmp/redrop"}
	for _, jsonMode := range []bool{false, true} {
		var err error
		out, errOut := captureCLIOutput(t, func() {
			g.json = jsonMode
			err = reportSelfUpdate(os.Stderr, res)
		})
		if err != nil {
			t.Fatal(err)
		}
		if errOut != "warning: downgrading from 1.0.0 to 0.5.0\n" {
			t.Errorf("json=%v: stderr = %q", jsonMode, errOut)
		}
		if jsonMode {
			var got selfUpdateResult
			if err := json.Unmarshal([]byte(out), &got); err != nil || !got.Downgraded {
				t.Errorf("stdout is not the result JSON: %v\n%s", err, out)
			}
		} else if !strings.Contains(out, "updated /tmp/redrop: 1.0.0 -> 0.5.0") {
			t.Errorf("text stdout = %q", out)
		}
	}
	// No warning without a downgrade.
	_, errOut := captureCLIOutput(t, func() {
		g.json = true
		reportSelfUpdate(os.Stderr, selfUpdateResult{Current: "1.0.0", Latest: "1.1.0", Updated: true})
	})
	if errOut != "" {
		t.Errorf("stderr without a downgrade = %q", errOut)
	}
}

func TestRequireHTTPS(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://api.github.com":         true,
		"https://mirror.example/api":     true,
		"http://127.0.0.1:8080":          true,
		"http://localhost:9000/x":        true,
		"http://[::1]:9000":              true,
		"http://api.github.com":          false,
		"http://192.0.2.1":               false,
		"ftp://example.com/redrop":       false,
		"http://localhost.example.com/x": false,
	} {
		if err := requireHTTPS(u); (err == nil) != ok {
			t.Errorf("requireHTTPS(%q) = %v, want ok=%v", u, err, ok)
		}
	}
}

// REDROP_GITHUB_API must be HTTPS unless it is loopback; it is a
// configuration error otherwise, before any request.
func TestSelfUpdateRejectsPlainHTTPAPI(t *testing.T) {
	t.Setenv("REDROP_GITHUB_API", "http://api.github.com")
	var err error
	captureCLIOutput(t, func() {
		c := selfUpdateCmd()
		c.SetArgs([]string{"--check"})
		err = c.Execute()
	})
	requireExitCode(t, err, exitConfig)
}

func TestParseSums(t *testing.T) {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("the binary")))
	in := "garbage\n\n" +
		sum + "  redrop-linux-amd64\n" +
		strings.ToUpper(sum) + " redrop-darwin-arm64\n" +
		"too many fields here\n"
	got, err := parseSums(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"redrop-linux-amd64": sum, "redrop-darwin-arm64": sum}
	if len(got) != len(want) {
		t.Fatalf("parsed %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
