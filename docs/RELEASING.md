# Cutting a release

How a `vX.Y.Z` redrop release is built and published. Everything below runs
in GitHub Actions on a tag push; nothing is built or uploaded locally.

## What happens on a `v*` tag

Two workflows run, both triggered by the same tag:

| Workflow | Publishes |
|---|---|
| `ci.yml` (job `image`) | Multi-arch image on `ghcr.io/mrRedSun/redrop`, tags `vX.Y.Z`, `sha-<full-commit>`, and `main` (main pushes only), after tests and a Trivy scan pass |
| `release.yml` | GitHub Release: CLI binaries for linux/darwin/windows on amd64/arm64, `SHA256SUMS`, build-provenance attestations, generated release notes |

`release.yml` stamps the tag (`vX.Y.Z`), the commit SHA and the commit
timestamp into the binaries with `-ldflags`. The Dockerfile does the same for
the image, using the semver tag that `docker/metadata-action` derives. Both
binaries therefore report the same triple: `redrop version` on a downloaded
CLI shows `vX.Y.Z` against a server reporting `vX.Y.Z`.

Version stamps, in short:

| Build | `version` | `commit` | `date` |
|---|---|---|---|
| `go build` locally | `dev` | (empty) | (empty) |
| CI on `main` | `0.0.0-main+<short sha>` | `<short sha>` | build time |
| CI on a `v*` tag | `vX.Y.Z` | `<short sha>` | tag commit time |

Builds from `main` never report `main`; they report `0.0.0-main+<short sha>`
or a real tag, so `redrop version` output is always parseable.

Provenance: `actions/attest-build-provenance` signs every `dist/*` artifact
with the workflow's OIDC identity. Anyone can verify a downloaded binary
against the release with [`gh attestation verify`](https://cli.github.com/manual/gh_attestation_verify):

```sh
gh attestation verify redrop-linux-amd64 --repo mrRedSun/redrop
```

The SHA256SUMS file published beside the binaries is the same format the
installers and `redrop self-update` verify against.

## Cutting vX.Y.Z

1. **Decide the version.** The project is pre-1.0; breaking API changes bump
   the minor, anything else the patch. `GET /api/v1/version`'s
   `min_cli_version` tells old CLIs to update; raise it only when the server
   stops working with them.
2. **Update `CHANGELOG.md`.** Move the Unreleased entries under the new
   `## vX.Y.Z - YYYY-MM-DD` heading and start a fresh `## Unreleased`.
   The GitHub Release notes are generated from PR titles; the changelog is
   the curated version.
3. **Merge the changelog PR** into `main` and wait for CI to pass.
4. **Tag and push from `main`:**

   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

5. **Watch both workflows** to green: Actions → the tag's `CI and image` and
   `Release` runs. The release is created as published (not draft) once
   binaries, attestations and notes are uploaded.
6. **Check the release page**: six binaries, `SHA256SUMS`, generated notes
   listing the PRs since the previous tag, and attestations on the assets.
   Verify one binary: `sha256sum -c` against `SHA256SUMS`, and
   `gh attestation verify` as above.
7. **Rolling deploys forward** stays as it is: production composes files pin
   `sha-<commit>@<digest>` images. A release adds the `vX.Y.Z` tag and
   GitHub binaries; it does not change what deployment pinning uses.

## If something goes wrong

- **A workflow failed after the release exists:** fix forward — delete the
  release and the tag, then re-tag from the fixed commit. Never re-run a
  failed publish job against a moved tag.
- **Bad assets on an otherwise good release:** delete the release (not the
  tag) and re-run the `Release` workflow from the tag's run page
  (`workflow_dispatch` is not enabled; push the tag again after deleting the
  release).
- **Rolling back a server deployment** does not involve releases at all:
  repin the compose file to the previous `sha-<commit>@<digest>` image and
  redeploy. See README.md and docs/ARCHITECTURE.md for the deploy model.

## Conventions

- Never build release artifacts on a developer machine; the workflows are
  the only publisher (binaries via `release.yml`, images via `ci.yml`).
- Every action is pinned by full commit SHA; bump them through Dependabot.
- The `Release` workflow's permissions are exactly `contents: write`,
  `id-token: write`, `attestations: write`, and it only runs on tags.
- Release names come from the tag; notes are generated, then may be edited
  by hand afterwards if wording matters.
