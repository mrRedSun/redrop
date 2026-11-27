package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// selfUpdateRepo is the repository whose releases carry the CLI binaries.
// It is the same source the release workflow publishes to.
const selfUpdateRepo = "mrRedSun/redrop"

// defaultGitHubAPI is the public GitHub REST API root. REDROP_GITHUB_API
// overrides it (HTTPS only, except on loopback).
const defaultGitHubAPI = "https://api.github.com"

// semverTagRE validates a --version argument: a strict vX.Y.Z tag.
var semverTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// selfUpdateTimeout bounds the whole download; CLI binaries are tens of MB.
const selfUpdateTimeout = 10 * time.Minute

// errManaged is returned when the running binary appears to be owned by a
// package manager; self-update would overwrite files it owns.
var errManaged = errors.New("the binary was installed by the system or a package manager; update it the same way, or reinstall with the redrop installer")

// managedPrefixes are installation paths that belong to a system or
// package-manager tree. /usr/local/bin is deliberately absent: the project's
// own installer uses it for root installs, and for anyone else the directory
// already fails the writability probe.
var managedPrefixes = []string{
	"/usr/bin/", "/usr/sbin/", "/bin/", "/sbin/", // system packages
	"/opt/homebrew/", // Homebrew on Apple Silicon
}

// managedSegments are path segments that mark a package-manager tree
// wherever they appear: Homebrew's Cellar on any platform, Nix stores.
var managedSegments = []string{"/Cellar/", "/.linuxbrew/", "/nix/store/"}

func exeSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// assetName is the release-asset file name for an OS/architecture pair.
func assetName(goos, goarch string) string {
	return "redrop-" + goos + "-" + goarch + exeSuffix(goos)
}

// releaseAsset is one entry of a GitHub release's asset list.
type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseInfo struct {
	TagName string         `json:"tag_name"`
	Name    string         `json:"name"`
	Assets  []releaseAsset `json:"assets"`
}

// fetchLatestRelease asks the GitHub API for a tag's release, or the newest
// non-prerelease release when tag is empty. The repository is public, so no
// authentication is sent.
func fetchLatestRelease(ctx context.Context, apiBase, repo, tag string) (*releaseInfo, error) {
	path := "/repos/" + repo + "/releases/latest"
	if tag != "" {
		path = "/repos/" + repo + "/releases/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, withCode(exitNotFound, "no GitHub release %s in %s (releases are created by tagging vX.Y.Z)", releaseLabel(tag), repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API %s", resp.Status)
	}
	var rel releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func releaseLabel(tag string) string {
	if tag == "" {
		return "latest"
	}
	return tag
}

// lookupAsset finds the download URL for name in the release's assets.
func lookupAsset(rel *releaseInfo, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// parseSums reads a `sha256sum`-format file ("<hex>  <name>" or
// "<hex> <name>" per line) into a map of file name to lower-case hex digest.
// Lines that are not exactly two fields are ignored.
func parseSums(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) == 2 {
			sums[parts[1]] = strings.ToLower(parts[0])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sums, nil
}

// selfUpdateResult reports what self-update did (or, with --check, would do).
type selfUpdateResult struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	URL       string `json:"url"`
	CheckOnly bool   `json:"check_only"`
	// UpdateAvailable is true when self-update would install the fetched
	// release. With --check it is the answer to "should I update?".
	UpdateAvailable bool `json:"update_available"`
	// MainBuild marks a 0.0.0-main+sha CLI, which tracks its server rather
	// than the releases; switching needs an explicit --version.
	MainBuild   bool   `json:"main_build,omitempty"`
	Updated     bool   `json:"updated"`
	Downgraded  bool   `json:"downgraded,omitempty" description:"The installed release is older than the running version (--version only)"`
	Replacement string `json:"replacement,omitempty"`
	Old         string `json:"old,omitempty" description:"Windows only: where the previous binary was moved"`
}

// checkWritable verifies the binary's directory can take a rename: the
// directory is writable and the binary is not inside a system or
// package-manager tree. A writable directory is required even for the
// staging file, because the replacement must land in the same directory for
// the rename to be atomic.
func checkWritable(exe string) error {
	if !filepath.IsAbs(exe) {
		var err error
		if exe, err = filepath.Abs(exe); err != nil {
			return err
		}
	}
	exe = filepath.Clean(exe)
	if isManagedPath(exe) {
		return withCode(exitUsage, "%s: %w", exe, errManaged)
	}
	dir := filepath.Dir(exe)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return withCode(exitGeneral, "cannot stat %s: %v", dir, err)
	}
	// Probe with the same operation the update performs: create and remove a
	// file in the directory. A read-only mount fails here, before any download.
	probe, err := os.CreateTemp(dir, ".redrop-write-test-*")
	if err != nil {
		return withCode(exitGeneral, "%s is not writable: %v", dir, err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return nil
}

// isManagedPath reports whether a cleaned absolute path lies in a system or
// package-manager tree.
func isManagedPath(exe string) bool {
	for _, p := range managedPrefixes {
		if strings.HasPrefix(exe, p) {
			return true
		}
	}
	for _, s := range managedSegments {
		if strings.Contains(exe, s) {
			return true
		}
	}
	return false
}

// requireHTTPS accepts only https:// URLs, except for loopback hosts, so a
// reconfigured API or a crafted release cannot move the download to plain
// HTTP. Loopback is allowed for local development and tests.
func requireHTTPS(u string) error {
	parsed, err := url.Parse(u)
	if err != nil {
		return withCode(exitGeneral, "invalid URL %q: %v", u, err)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return withCode(exitGeneral, "refusing non-HTTPS download from %s", u)
}

// maxRedirects bounds a redirect chain, as net/http's default policy does.
const maxRedirects = 10

// updateHTTP sends every self-update request (the release lookup, the
// checksum file and the binary). Each redirect target must pass the same
// HTTPS-or-loopback rule as the first URL, so an HTTPS response cannot hand
// any part of the update to plain HTTP.
var updateHTTP = &http.Client{CheckRedirect: checkUpdateRedirect}

func checkUpdateRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return withCode(exitGeneral, "stopped after %d redirects", maxRedirects)
	}
	if requireHTTPS(req.URL.String()) != nil {
		return withCode(exitGeneral, "refusing redirect from %s to non-HTTPS %s", via[len(via)-1].URL.Redacted(), req.URL.Redacted())
	}
	return nil
}

// httpGet starts a GET with a redrop user agent and fails on non-200. The
// caller closes the body and bounds what it reads.
func httpGet(ctx context.Context, url, ua string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return resp, nil
}

// maxSumsSize caps SHA256SUMS (the real file is a few hundred bytes) so a
// hostile sums file cannot exhaust memory; the asset streams to disk.
const maxSumsSize = 64 << 10

// maxAssetSize caps the streamed asset download (a var so a test can lower
// it instead of serving 128 MiB).
var maxAssetSize int64 = 128 << 20

// downloadSums fetches and parses the release's SHA256SUMS into a map of
// file name to expected hex digest.
func downloadSums(ctx context.Context, sumsURL, ua string) (map[string]string, error) {
	resp, err := httpGet(ctx, sumsURL, ua)
	if err != nil {
		return nil, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSumsSize+1))
	if err != nil {
		return nil, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	if len(b) > maxSumsSize {
		return nil, withCode(exitGeneral, "SHA256SUMS is larger than %d bytes; the release is not trusted", maxSumsSize)
	}
	sums, err := parseSums(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	return sums, nil
}

// stageDownload streams the asset into dir, hashes while writing, verifies
// the digest against the release's SHA256SUMS, and returns the staged file's
// path. The file has a random name, the old binary's mode (0755 fallback),
// and is synced before the caller renames it into place.
func stageDownload(ctx context.Context, dir, assetURL, sumsURL, assetName, ua string, oldMode os.FileMode) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	defer cancel()

	sums, err := downloadSums(ctx, sumsURL, ua)
	if err != nil {
		return "", err
	}
	want, ok := sums[assetName]
	if !ok {
		return "", withCode(exitGeneral, "no checksum for %s in SHA256SUMS; the download is not trusted and was discarded", assetName)
	}

	resp, err := httpGet(ctx, assetURL, ua)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", assetName, err)
	}
	defer resp.Body.Close()
	staged, err := os.CreateTemp(dir, ".redrop-update-*")
	if err != nil {
		return "", err
	}
	ok = false
	defer func() {
		if !ok {
			os.Remove(staged.Name())
		}
	}()
	h := sha256.New()
	// One byte beyond the cap means the asset is too large, not truncated.
	n, err := io.Copy(io.MultiWriter(staged, h), io.LimitReader(resp.Body, maxAssetSize+1))
	if err != nil {
		staged.Close()
		return "", fmt.Errorf("download %s: %w", assetName, err)
	}
	if n > maxAssetSize {
		staged.Close()
		return "", withCode(exitGeneral, "%s is larger than %d MiB; the download was discarded", assetName, maxAssetSize>>20)
	}
	if got := h.Sum(nil); hex.EncodeToString(got) != want {
		staged.Close()
		return "", withCode(exitGeneral, "checksum mismatch for %s; the download is not trusted and was discarded", assetName)
	}
	mode := oldMode
	if mode == 0 {
		mode = 0o755
	}
	if err := staged.Chmod(mode.Perm()); err != nil {
		staged.Close()
		return "", err
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return "", err
	}
	if err := staged.Close(); err != nil {
		return "", err
	}
	ok = true
	return staged.Name(), nil
}

// replaceBinary atomically installs newExe at target. On Windows, where an
// executing binary cannot be renamed over, the old binary is renamed aside
// first; it is removed when possible, and its path returned when it had to
// stay (a running binary can often not be deleted there).
func replaceBinary(target, newExe string) (string, error) {
	if runtime.GOOS == "windows" {
		old := target + ".old"
		// A .old left by an earlier update (the binary was still running
		// then) is no longer in use; clear it so the rename can take its name.
		os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return "", fmt.Errorf("move the running binary aside: %w", err)
		}
		if err := os.Rename(newExe, target); err != nil {
			// Put the old binary back; if that fails too, say where it is.
			if backErr := os.Rename(old, target); backErr != nil {
				return "", fmt.Errorf("install %s failed (%v) and restoring %s from %s also failed (%v); the previous binary is still at %s", target, err, target, old, backErr, old)
			}
			return "", err
		}
		if err := os.Remove(old); err != nil {
			return old, nil
		}
		return "", nil
	}
	if err := os.Rename(newExe, target); err != nil {
		return "", err
	}
	return "", nil
}

// selfUpdateOptions carries everything selfUpdate needs; the command fills
// it from flags and the environment so tests can inject a target binary,
// an API base, and the platform pair instead of updating the test process.
type selfUpdateOptions struct {
	apiBase      string // GitHub API root
	repo         string
	tag          string // requested vX.Y.Z, or "" for latest
	checkOnly    bool
	current      string // version of the running CLI
	goos, goarch string
	exe          string // absolute path of the binary to replace
	ua           string
}

// selfUpdate runs the whole flow. It prints nothing; the caller reports.
func selfUpdate(ctx context.Context, o selfUpdateOptions) (selfUpdateResult, error) {
	rel, err := fetchLatestRelease(ctx, o.apiBase, o.repo, o.tag)
	if err != nil {
		return selfUpdateResult{}, err
	}
	asset := assetName(o.goos, o.goarch)
	url := lookupAsset(rel, asset)
	if url == "" {
		return selfUpdateResult{}, withCode(exitNotFound, "release %s has no %s asset", rel.TagName, asset)
	}
	sumsURL := lookupAsset(rel, "SHA256SUMS")
	if sumsURL == "" {
		return selfUpdateResult{}, withCode(exitNotFound, "release %s has no SHA256SUMS asset", rel.TagName)
	}
	if err := requireHTTPS(url); err != nil {
		return selfUpdateResult{}, err
	}
	if err := requireHTTPS(sumsURL); err != nil {
		return selfUpdateResult{}, err
	}
	res := selfUpdateResult{Current: strings.TrimPrefix(o.current, "v"), Latest: strings.TrimPrefix(rel.TagName, "v"), URL: url, CheckOnly: o.checkOnly}

	// A main build (0.0.0-main+sha) tracks the server it was installed from,
	// not the GitHub releases, and cannot be ordered against them. "latest"
	// could be older, so switching needs an explicit --version; --check only
	// reports.
	if o.tag == "" && isMainBuild(o.current) {
		res.MainBuild = true
		if o.checkOnly {
			return res, nil
		}
		return res, withCode(exitUsage,
			"this CLI is a main build (%s) that matches its server, not a release; pass --version v%s to switch to the latest release", o.current, res.Latest)
	}

	// An unpinned update to a release we already have is a no-op. Versions
	// that do not carry a comparable vX.Y.Z (dev, preview-…) always take
	// the latest.
	if o.tag == "" {
		if cur, ok := parseSemver(o.current); ok {
			if relTag, ok2 := parseSemver(rel.TagName); ok2 && compareSemver(cur, relTag) >= 0 {
				return res, nil
			}
		}
	}
	res.UpdateAvailable = true
	if cur, ok := parseSemver(o.current); ok {
		if relTag, ok2 := parseSemver(rel.TagName); ok2 && compareSemver(cur, relTag) > 0 {
			res.Downgraded = true
		}
	}
	if o.checkOnly {
		return res, nil
	}
	if err := checkWritable(o.exe); err != nil {
		return res, err
	}
	// Keep the installed file's mode (0755 when there is no previous file).
	mode := os.FileMode(0)
	if st, err := os.Stat(o.exe); err == nil {
		mode = st.Mode()
	}
	staged, err := stageDownload(ctx, filepath.Dir(o.exe), url, sumsURL, asset, o.ua, mode)
	if err != nil {
		return res, err
	}
	old, err := replaceBinary(o.exe, staged)
	if err != nil {
		os.Remove(staged)
		return res, withCode(exitGeneral, "replace %s: %v", o.exe, err)
	}
	res.Updated = true
	res.Replacement = o.exe
	res.Old = old
	return res, nil
}

func selfUpdateCmd() *cobra.Command {
	var (
		versionFlag string
		checkFlag   bool
	)
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update the redrop CLI from the project's GitHub releases",
		Long: `Download the latest CLI release for this platform from GitHub, verify it
against the release's SHA256SUMS, and replace the running binary atomically.

By default the newest release is installed; --version vX.Y.Z picks a specific
one (an older one too, with a warning: that is the rollback path). --check
only reports the latest version and whether an update is available.

A CLI installed from a server's /install.sh is usually a main build
(0.0.0-main+<commit>) matching that server. It is not compared with the
releases: self-update without --version refuses, and --check only reports.

The update is refused when the binary lives in a system or package-manager
directory (/usr/bin, Homebrew Cellar, Nix store, ...) or its directory is not
writable, so it never fights the system that installed it. Re-run the
installer script instead:
  curl -fsSL https://your-server/install.sh | sh

Trust: the binary is checked against the release's SHA256SUMS, which catches
corruption and truncation. Both files come from the same GitHub release, so
authenticity rests on HTTPS to GitHub and on the repository's account
security. To check provenance independently, run
  gh attestation verify <binary> --repo mrRedSun/redrop
REDROP_GITHUB_API points the release lookup at another HTTPS API root.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if versionFlag != "" && !strings.HasPrefix(versionFlag, "v") {
				versionFlag = "v" + versionFlag
			}
			if versionFlag != "" && !semverTagRE.MatchString(versionFlag) {
				return withCode(exitUsage, "--version must look like vX.Y.Z, got %q", versionFlag)
			}
			exe, err := os.Executable()
			if err != nil {
				return withCode(exitGeneral, "locate the running binary: %v", err)
			}
			exe, err = filepath.EvalSymlinks(exe)
			if err != nil {
				return withCode(exitGeneral, "resolve %s: %v", exe, err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), selfUpdateTimeout)
			defer cancel()
			// REDROP_GITHUB_API redirects the release lookup (a mirror, or
			// tests); it must be HTTPS unless the host is loopback.
			apiBase := defaultGitHubAPI
			if v := os.Getenv("REDROP_GITHUB_API"); v != "" {
				if err := requireHTTPS(v); err != nil {
					return withCode(exitConfig, "REDROP_GITHUB_API: %v", err)
				}
				apiBase = strings.TrimRight(v, "/")
			}
			res, err := selfUpdate(ctx, selfUpdateOptions{
				apiBase:   apiBase,
				repo:      selfUpdateRepo,
				tag:       versionFlag,
				checkOnly: checkFlag,
				current:   version,
				goos:      runtime.GOOS,
				goarch:    runtime.GOARCH,
				exe:       exe,
				ua:        "redrop-self-update/" + version,
			})
			if err != nil {
				return mapErr(err)
			}
			return reportSelfUpdate(cmd.ErrOrStderr(), res)
		},
	}
	cmd.Flags().StringVar(&versionFlag, "version", "", "install this release instead of the latest (vX.Y.Z)")
	cmd.Flags().BoolVar(&checkFlag, "check", false, "only report the latest release and exit")
	return cmd
}

// reportSelfUpdate prints the result: JSON or text on stdout, and a
// downgrade warning on stderr in both modes, so stdout stays valid JSON.
func reportSelfUpdate(stderr io.Writer, res selfUpdateResult) error {
	if res.Downgraded {
		fmt.Fprintf(stderr, "warning: downgrading from %s to %s\n", res.Current, res.Latest)
	}
	if g.json {
		return printJSON(res)
	}
	switch {
	case res.Updated:
		fmt.Printf("updated %s: %s -> %s\n", res.Replacement, res.Current, res.Latest)
		if res.Old != "" {
			fmt.Printf("the previous binary was left at %s (delete it once the new one works)\n", res.Old)
		}
	case res.MainBuild:
		fmt.Printf("current: %s (a main build that matches its server)\nlatest release: %s\nto switch to it: redrop self-update --version v%s\n", res.Current, res.Latest, res.Latest)
	case res.CheckOnly && res.UpdateAvailable:
		fmt.Printf("current: %s\nlatest:  %s\nupdate available: %s\n", res.Current, res.Latest, res.URL)
	default:
		fmt.Printf("redrop %s is up to date (latest release %s)\n", res.Current, res.Latest)
	}
	return nil
}
