package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrRedSun/redrop/pkg/client"
)

// selfUpdateRepo is the repository whose releases carry the CLI binaries.
// It is the same source the release workflow publishes to.
const selfUpdateRepo = "mrRedSun/redrop"

// semverTagRE validates a --version argument: a strict vX.Y.Z tag.
var semverTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// selfUpdateTimeout bounds the whole download; CLI binaries are tens of MB.
const selfUpdateTimeout = 10 * time.Minute

// errManaged is returned when the running binary appears to be owned by a
// package manager; self-update would break its database.
var errManaged = errors.New("the binary is managed by a package manager; update it with that package manager")

// managedPrefixes are installation paths that belong to a package manager.
var managedPrefixes = []string{
	"/usr/bin/", "/usr/sbin/", "/bin/", "/sbin/", "/usr/local/bin/", // system packages, Homebrew on Linux
	"/opt/homebrew/", // Homebrew on Apple Silicon
}

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
// non-prerelease release when tag is empty. It needs no authentication on a
// public repository; a private token may be supplied through ghToken.
func fetchLatestRelease(ctx context.Context, apiBase, repo, tag, ghToken string) (*releaseInfo, error) {
	path := "/repos/" + repo + "/releases/latest"
	if tag != "" {
		path = "/repos/" + repo + "/releases/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if ghToken != "" {
		req.Header.Set("Authorization", "Bearer "+ghToken)
	}
	resp, err := http.DefaultClient.Do(req)
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

// lookupSibling maps an asset's browser_download_url to the SHA256SUMS asset
// uploaded beside it in the same release directory.
func lookupSibling(assetURL string) string {
	i := strings.LastIndex(assetURL, "/")
	if i < 0 {
		return ""
	}
	return assetURL[:i+1] + "SHA256SUMS"
}

// verifyChecksum parses a `sha256  filename`-style SHA256SUMS file and
// reports whether file's digest matches the entry for its name.
func verifyChecksum(sums []byte, name string, sum [32]byte) bool {
	want := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Accept both "hash  name" and "hash name".
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == name && strings.EqualFold(parts[0], want) {
			return true
		}
	}
	return false
}

// selfUpdateResult reports what self-update did (or, with --check, would do).
type selfUpdateResult struct {
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	URL         string `json:"url"`
	CheckOnly   bool   `json:"check_only"`
	Updated     bool   `json:"updated"`
	Replacement string `json:"replacement,omitempty"`
	Old         string `json:"old,omitempty" description:"Windows only: where the previous binary was moved"`
}

// checkWritable verifies the binary's directory can take a rename: the
// directory is writable and the binary is not inside a package-manager tree.
// A writable directory is required even for the staging file, because the
// replacement must land in the same directory for the rename to be atomic.
func checkWritable(exe string) error {
	if !filepath.IsAbs(exe) {
		var err error
		if exe, err = filepath.Abs(exe); err != nil {
			return err
		}
	}
	for _, p := range managedPrefixes {
		if strings.HasPrefix(filepath.Clean(exe), p) {
			return withCode(exitUsage, "%s: %w", exe, errManaged)
		}
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

// httpGetBytes downloads url with a bounded size and a redrop user agent.
func httpGetBytes(ctx context.Context, url, ua string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

// stageDownload downloads the asset and the release's SHA256SUMS, verifies
// the checksum, and returns the path of the verified staged file in dir.
// The staged file has a random name and mode 0755; the caller renames it.
func stageDownload(ctx context.Context, dir, assetURL, assetName, ua string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	defer cancel()

	body, err := httpGetBytes(ctx, assetURL, ua)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", assetName, err)
	}
	sumsURL := lookupSibling(assetURL)
	if sumsURL == "" {
		return "", errors.New("cannot locate SHA256SUMS next to the asset")
	}
	sums, err := httpGetBytes(ctx, sumsURL, ua)
	if err != nil {
		return "", fmt.Errorf("download SHA256SUMS: %w", err)
	}
	h := sha256.Sum256(body)
	if !verifyChecksum(sums, assetName, h) {
		return "", withCode(exitGeneral, "checksum mismatch for %s; the download is not trusted and was discarded", assetName)
	}
	staged, err := os.CreateTemp(dir, ".redrop-update-*")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(staged.Name())
		}
	}()
	if _, err := staged.Write(body); err != nil {
		staged.Close()
		return "", err
	}
	if err := staged.Chmod(0o755); err != nil {
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
		if err := os.Rename(target, old); err != nil {
			return "", fmt.Errorf("move the running binary aside: %w", err)
		}
		if err := os.Rename(newExe, target); err != nil {
			os.Rename(old, target) // put the old binary back
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
	ghToken      string
	checkOnly    bool
	current      string // version of the running CLI
	goos, goarch string
	exe          string // absolute path of the binary to replace
	ua           string
}

// selfUpdate runs the whole flow. It prints nothing; the caller reports.
func selfUpdate(ctx context.Context, o selfUpdateOptions) (selfUpdateResult, error) {
	rel, err := fetchLatestRelease(ctx, o.apiBase, o.repo, o.tag, o.ghToken)
	if err != nil {
		return selfUpdateResult{}, err
	}
	asset := assetName(o.goos, o.goarch)
	url := lookupAsset(rel, asset)
	if url == "" {
		return selfUpdateResult{}, withCode(exitNotFound, "release %s has no %s asset", rel.TagName, asset)
	}
	res := selfUpdateResult{Current: o.current, Latest: strings.TrimPrefix(rel.TagName, "v"), URL: url, CheckOnly: o.checkOnly}

	// An unpinned update to a release we already have is a no-op. Dev and
	// preview builds (no parseable version) always take the latest.
	if o.tag == "" {
		if cur, ok := parseSemver(o.current); ok {
			if relTag, ok2 := parseSemver(rel.TagName); ok2 && compareSemver(cur, relTag) >= 0 {
				return res, nil
			}
		}
	}
	if o.checkOnly {
		res.CheckOnly = true
		return res, nil
	}
	if err := checkWritable(o.exe); err != nil {
		return res, err
	}
	staged, err := stageDownload(ctx, filepath.Dir(o.exe), url, asset, o.ua)
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
one. --check only reports the latest version and whether an update is needed.

The update is refused when the binary lives in a package-manager directory
(/usr/bin, Homebrew, ...) or its directory is read-only, so it never fights
the system that installed it. Re-run the installer script instead:
  curl -fsSL https://your-server/install.sh | sh`,
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
			// REDROP_GITHUB_API redirects the release lookup (tests, proxies);
			// REDROP_GITHUB_TOKEN authenticates it for private repositories.
			apiBase := client.DefaultGitHubAPI
			if v := os.Getenv("REDROP_GITHUB_API"); v != "" {
				apiBase = v
			}
			res, err := selfUpdate(ctx, selfUpdateOptions{
				apiBase:   apiBase,
				repo:      selfUpdateRepo,
				tag:       versionFlag,
				ghToken:   os.Getenv("REDROP_GITHUB_TOKEN"),
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
			if g.json {
				return printJSON(res)
			}
			switch {
			case res.Updated:
				fmt.Printf("updated %s: %s -> v%s\n", res.Replacement, res.Current, res.Latest)
				if res.Old != "" {
					fmt.Printf("the previous binary was left at %s (delete it once the new one works)\n", res.Old)
				}
			case res.CheckOnly:
				fmt.Printf("current: %s\nlatest:  v%s\nupdate available: %s\n", res.Current, res.Latest, res.URL)
			default:
				fmt.Printf("redrop %s is up to date (latest release v%s)\n", res.Current, res.Latest)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&versionFlag, "version", "", "install this release instead of the latest (vX.Y.Z)")
	cmd.Flags().BoolVar(&checkFlag, "check", false, "only report the latest release and exit")
	return cmd
}
