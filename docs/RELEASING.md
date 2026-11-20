# Cutting a release

How a `vX.Y.Z` redrop release is built and published. Everything runs in
GitHub Actions on a tag push; nothing is built or uploaded locally.

## What happens on a `v*` tag

One workflow, `ci.yml`, runs on the tag. Nothing is published unless its
`test` job (frontend checks, `gofmt`, `go vet`, the race suite, govulncheck)
passes first:

| Job | Needs | Permissions | Publishes |
|---|---|---|---|
| `test` | | `contents: read` | nothing |
| `image` | `test` | `packages: write` | Multi-arch image on `ghcr.io/mrredsun/redrop`, tags `vX.Y.Z` and `sha-<full commit>` (plus `main` on main pushes), after a Trivy scan |
| `release-build` | `test` | `id-token: write`, `attestations: write` | Builds the six CLI binaries and `SHA256SUMS`, and attests their provenance; hands `dist/` to the next job as a workflow artifact |
| `release-publish` | `release-build` | `contents: write` | The GitHub Release with the binaries, `SHA256SUMS`, and notes generated from PR titles |

The release jobs run on tags only. Building and publishing are separate jobs
so the third-party release action never runs next to a token that can sign
attestations. The release build is hermetic: no Go build cache, no VCS build
info.

A tag containing `-` (`v1.0.0-rc1`) is published as a **prerelease**. GitHub's
"latest release", which `redrop self-update` installs by default, never
points at a prerelease, so a release candidate reaches only those who ask for
it with `--version`.

### Version stamps

`-ldflags` stamps three values into the server and the CLI:

| Build | `version` | `commit` | `date` |
|---|---|---|---|
| `go build` locally | `dev` | (empty) | (empty) |
| `scripts/dev-server.sh` | `0.0.0-main+<short sha>` | `<short sha>` | commit time |
| CI on `main` (image) | `0.0.0-main+<12-char sha>` | `<12-char sha>` | commit time, UTC |
| CI on a `v*` tag (image and release) | `vX.Y.Z` | `<12-char sha>` | commit time, UTC |

For one tag, the image's server, the image's CLIs and the release binaries
report the same version, commit and date. The binaries themselves are not
byte-identical: the image and the release are separate builds, possibly with
different Go patch releases. Compare a binary against the `SHA256SUMS` it was
published with, not against the other build.

Builds from `main` never report a bare `main`. A `0.0.0-main+…` CLI, which is
what a server's `/install.sh` serves for a `main` deployment, is not compared
with releases: `redrop version` never warns about it, and `redrop self-update`
needs an explicit `--version` to replace it with a release.

## Trust model

What `SHA256SUMS` protects, and what it does not:

- `redrop self-update` takes both the binary and `SHA256SUMS` from the
  release's own asset list, over HTTPS. The checksum catches corruption,
  truncation, and a mismatched binary/sums pair. Downloads that are not HTTPS
  are refused (loopback excepted, for tests).
- Both files come from the same GitHub release, so the checksum does **not**
  protect against a replaced release: a stolen token with `contents: write`
  or a compromised maintainer account could upload a matching pair.
  Authenticity rests on HTTPS to GitHub and on GitHub account security.
- The installers (`/install.sh`, `/install.ps1`) have the same model with the
  redrop server as the root; `install.sh` keeps HTTPS through redirects when
  the server URL is HTTPS.

For an independent check, verify the build-provenance attestation. It binds
the binary's digest to this repository's workflow run on the tag:

```sh
gh attestation verify redrop-linux-amd64 --repo mrRedSun/redrop
sha256sum -c --ignore-missing SHA256SUMS
```

A pinned signing key for `SHA256SUMS`, verified by the CLI itself, is a
planned follow-up.

`REDROP_GITHUB_API` points `self-update` at another API root (a mirror, or a
local fake in tests). It must be HTTPS unless the host is loopback.

## Cutting vX.Y.Z

1. **Decide the version.** The project is pre-1.0: breaking API changes bump
   the minor, anything else the patch. `MinCLIVersion` in
   `internal/httpapi/api.go` (served as `min_cli_version`) makes older release
   CLIs warn; raise it only when the server stops working with them.
2. **Update `CHANGELOG.md`.** Rename `## Unreleased` to
   `## [X.Y.Z] - YYYY-MM-DD` and start a new, empty `## Unreleased` above it.
   For v0.1.0 that heading takes all the history so far. The GitHub Release
   notes are generated from PR titles; the changelog is the curated version.
3. **Merge the changelog PR** into `main` through the normal PR flow.
4. **Tag the merged commit on `main` and push the tag:**

   ```sh
   git fetch origin
   git tag -a v0.1.0 -m "v0.1.0" origin/main
   git push origin v0.1.0
   ```

5. **Watch the tag's `CI and image` run** until `test`, `image`,
   `release-build` and `release-publish` are green.
6. **Check the release page**: six binaries, `SHA256SUMS`, generated notes
   listing the PRs since the previous tag, and attestations for the assets.
   Download one binary and verify it as in [Trust model](#trust-model).
7. **Deployment pinning is unchanged.** Production compose files pin
   `sha-<commit>@<digest>` images. A release adds the `vX.Y.Z` image tag and
   the GitHub binaries; it does not change what deployments pin.

## If something goes wrong

Version tags are immutable: never delete and re-push a tag, or reuse a version
number for different bytes.

- **`test` fails on the tag:** nothing is published. Fix the problem on
  `main` and cut the next patch version (`vX.Y.Z+1`). The failed tag stays as
  a record and has no release.
- **`release-publish` fails after `release-build` succeeded** (a GitHub
  outage, for example): use **Re-run failed jobs** on the same run. The built
  `dist/` artifact is kept for 7 days, and the tag has not moved.
- **A published release is bad:** edit it on GitHub and tick *Set as a
  pre-release*, which takes it out of "latest" so `self-update` stops
  installing it, and add a note to its description. Then fix the problem and
  cut `vX.Y.Z+1`.
- **Rolling back a server deployment** does not involve releases: repin the
  compose file to the previous `sha-<commit>@<digest>` image and redeploy.
  Users roll a CLI back with `redrop self-update --version vA.B.C`.

## Conventions

- Never build release artifacts on a developer machine. `ci.yml` is the only
  publisher, of images and of binaries.
- Every action is pinned by full commit SHA; Dependabot proposes bumps.
- Each job holds only the permissions in the table above. The workflow's
  default is `contents: read`.
- Release names come from the tag. Generated notes may be edited by hand
  afterwards if the wording matters.
