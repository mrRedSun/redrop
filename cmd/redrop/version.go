package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrRedSun/redrop/pkg/client"
)

// semverRE matches a plain release version, `vX.Y.Z` or `X.Y.Z`, and nothing
// else. Suffixed versions (0.0.0-main+sha, v1.2.0-rc1) and stamps without a
// numeric triple (dev, preview builds) are not comparable: main builds track
// their server rather than the release list, and prereleases do not order
// like the plain comparison here.
var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// parseSemver returns the numeric triple of a release version, or false when
// it is not a plain release: dev and preview stamps, main builds
// (0.0.0-main+sha) and prereleases (v1.2.0-rc1) never compare.
func parseSemver(v string) ([3]int, bool) {
	m := semverRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// isMainBuild reports whether v is a main-branch build stamp
// (0.0.0-main+sha), the form the server's own /install.sh serves.
func isMainBuild(v string) bool {
	return strings.Contains(v, "-main")
}

// compareSemver compares two numeric triples: -1, 0, or 1.
func compareSemver(a, b [3]int) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// versionServerInfo is what `redrop version` could learn from the server.
type versionServerInfo struct {
	base            string
	version, commit string
	minCLI          string
}

// fetchServerVersion asks the configured server for its build. A server that
// predates GET /api/v1/version falls back to the version in /auth/config,
// which carries no commit or minimum CLI version.
func fetchServerVersion(ctx context.Context, c *client.Client) (versionServerInfo, error) {
	sv, err := c.ServerVersion(ctx)
	if err == nil {
		return versionServerInfo{base: c.BaseURL, version: sv.Version, commit: sv.Commit, minCLI: sv.MinCLIVersion}, nil
	}
	if client.StatusOf(err) == 404 {
		if ac, err := c.AuthConfig(ctx); err == nil {
			return versionServerInfo{base: c.BaseURL, version: ac.Version}, nil
		}
	}
	return versionServerInfo{}, fmt.Errorf("cannot reach server: %w", err)
}

// versionOutdated reports whether the CLI is older than the server's stated
// minimum. Only plain releases compare: dev and preview stamps, main builds
// (which match their own server) and prereleases never warn, and an
// unparseable minimum never warns.
func versionOutdated(cliVersion, minCLI string) bool {
	if minCLI == "" {
		return false
	}
	c, okC := parseSemver(cliVersion)
	m, okM := parseSemver(minCLI)
	return okC && okM && compareSemver(c, m) < 0
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version (and the server version when configured)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := map[string]string{"cli": version}
			if commit != "" {
				out["commit"] = commit
			}
			if date != "" {
				out["built"] = date
			}
			var warn string
			if c, err := newClient(); err == nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				defer cancel()
				if sv, err := fetchServerVersion(ctx, c); err == nil {
					out["server"] = sv.version
					out["url"] = sv.base
					if sv.commit != "" {
						out["server_commit"] = sv.commit
					}
					if sv.minCLI != "" {
						out["min_cli"] = sv.minCLI
					}
					if versionOutdated(version, sv.minCLI) {
						warn = fmt.Sprintf("this CLI (%s) is older than the minimum the server supports (%s); run `redrop self-update`", version, sv.minCLI)
						out["update"] = "required"
					}
				}
			}
			if g.json {
				return printJSON(out)
			}
			fmt.Printf("redrop %s", version)
			if commit != "" {
				fmt.Printf(" (commit %s)", commit)
			}
			fmt.Println()
			if s, ok := out["server"]; ok {
				fmt.Printf("server %s (%s)\n", s, out["url"])
			}
			if warn != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
			}
			return nil
		},
	}
}
