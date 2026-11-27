# Changelog

All notable changes to redrop are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). Release notes are generated from
pull-request titles when a `vX.Y.Z` tag is cut (see
[docs/RELEASING.md](docs/RELEASING.md)).

## Unreleased

No version has been tagged yet, so everything merged so far is listed here.
It becomes the v0.1.0 entry when that tag is cut. Entries summarize the merged
pull requests.

### Added

- SMTP invite emails and batched abuse-report alerts, off by default. Supports
  required STARTTLS, implicit TLS, authentication and private relay CAs, with a
  durable bounded queue, send status, resend, throttling, and
  `redrop-server mail-test --to ADDR`. (#40)

- Release process: after the CI checks pass, a `vX.Y.Z` tag builds CLI
  binaries for Linux, macOS and Windows (amd64/arm64) with `SHA256SUMS` and
  GitHub build-provenance attestations, and publishes them to a GitHub
  Release with generated notes. Tags with a `-` become prereleases. (#31)
- Real versions: CI stamps the version, commit and build date into the server
  and the CLI. `GET /api/v1/version` reports them plus the minimum supported
  CLI version. `redrop version` shows both sides and warns when a release CLI
  is older than the server's minimum. `/healthz` is unchanged. (#31)
- `redrop self-update` downloads a release (latest or `--version vX.Y.Z`),
  verifies it against `SHA256SUMS`, and replaces the running binary
  atomically; `--check` only reports. It refuses system and package-manager
  locations and unwritable directories, and asks for `--version` before
  replacing a `main` build. (#31)

- Go server (Fuego + SQLite), CLI, and design doc: sessions, API tokens,
  invites, OIDC, TOTP, chunked resumable uploads, per-artifact-subdomain
  serving with password and view-limit gates, versioning, janitor, e2e
  script. Restructured into services (`accounts`, `sharing`) with thin
  transports (`httpapi`, `content`). (#1)
- Multi-arch distroless image with the dashboard embedded and CLI binaries
  served by `/install.sh`; CI with frontend/Go checks, Trivy scanning and
  GHCR publishing on `main` and tags. (#2)
- Four-word artifact links (`amber-otter-quilt-river`); legacy 26-character
  base32 slugs keep resolving. (#3)
- MIT license. (#4)
- redrop favicons on every origin, never counted or logged as views. (#5)
- One-line CLI setup on token creation: the dashboard shows
  `curl … /install.sh | REDROP_TOKEN=… sh`, which installs, verifies the
  checksum, and logs in. (#6)
- Same-origin viewer frame for HTML/bundle artifacts with an info panel:
  title, expiry countdown, version history and notes; `--no-info`,
  `--old-versions`, and `?redrop-info=0` control it. In-frame
  pushState/replaceState is followed in the address bar. (#17, #21)
- Markdown rendering in the CLI: `redrop share notes.md` publishes a styled,
  self-contained document page with the original kept for download.
  (#18)
- `redrop watch` republishes an artifact when its source changes, debounced
  and deduplicated by content hash. (#20)
- Viewer feedback: per-artifact comments in the info panel, read back with
  `redrop feedback`; nothing is injected into artifacts. (#24)
- End-to-end encrypted links (`--e2e`): the CLI encrypts before upload, the
  key travels in the link's `#k=` fragment, and the server stores and serves
  only ciphertext. The link (`https://<server>/e/<slug>#k=…`) opens a key
  intake page on the server's own origin, which hands the artifact's origin
  only a non-extractable key; redrop's viewer decrypts in the browser, and
  the key's text never reaches the page's own scripts. (#33)
- `docs`: artifacts never contain anything redrop-specific. (#19)

### Fixed

- go-jose denial of service (CVE-2026-116587); dependency bump. (#2)
- Deterministic test suite: bounded waits, no timing or chance in tests. (#22)
- Hardening follow-ups for watch, the rate limiter, and install.sh staging. (#23)
- Security review A: identity, access and limits (bounded limiter tables,
  trusted-proxy hop counting, single-use TOTP, no secret paths in logs). (#26)
- Security review B: upload budgets and atomic disk reservations, archive
  framing checks, secret-path exclusions in folder uploads, atomic CLI
  config writes. (#27)

### Changed

- The CLI checks an `ID|URL` argument's shape before any request: anything
  that isn't an artifact ID, a slug or a link exits 2 (usage) without
  echoing it. A mistyped ID used to reach the server and exit 4 (not
  found); scripts that treated 4 as "no such artifact" for typos see 2.
  Unknown IDs of the right shape still exit 4. (#33)
- CI and supply-chain hardening: Dependabot, SHA-pinned actions,
  least-privilege checkouts, govulncheck in CI, weekly rescans, SECURITY.md. (#28)
- Go dependency bumps: fsnotify 1.10.1 and kin-openapi 0.153.0. (#29)
