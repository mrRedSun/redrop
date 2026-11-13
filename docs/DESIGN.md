# redrop design

redrop is a self-hosted server for sharing agent-produced artifacts (HTML/React
pages, arbitrary files, and multi-file static bundles) through unguessable,
expiring, optionally password-protected or view-limited links, with a React
management dashboard and a cross-platform CLI that agents drive.

## Principles

- **An artifact is exactly what its owner uploaded, and nothing in it is specific
  to redrop.**
  - redrop never injects, rewrites or appends anything to an artifact's bytes.
  - An artifact never has to include anything for redrop to work: no scripts,
    markup, meta tags, messages, branding or naming conventions.
  - Everything redrop adds (gates, the info panel, revisions, navigation
    tracking) lives outside the artifact, in redrop's own pages, response
    headers or the viewer frame. It must work with an artifact that has never
    heard of redrop.
  - A feature that would need an artifact's cooperation is out of scope.
  - Client-side conversions (for example, Markdown to HTML) produce a plain,
    self-contained document with no redrop-specific code or branding.

## Decisions

| Topic | Decision |
|---|---|
| Backend | Go, [Fuego](https://github.com/go-fuego/fuego) (OpenAPI generated from code), SQLite (pure Go, no CGO) |
| Dashboard | React + Vite + TypeScript + Tailwind + shadcn/ui, embedded in the server binary |
| Content | `html` (single page), `file` (any file, inline viewer or download), `bundle` (static site / directory) |
| Accounts | Multiple users, invite-only, roles `admin` and `user` |
| Admin privacy | Admins see other users' artifact metadata (title, size, views, expiry) and can revoke/delete for abuse, but cannot preview content |
| Dashboard login | Built-in username + password (argon2id) with optional TOTP, and generic OIDC (e.g. Authelia). Optionally admin actions require an OIDC-authenticated session |
| Agent auth | Per-user named API tokens (`rdp_…`), shown once, stored as SHA-256 |
| Expiry | Default 7 days; `never` allowed (both configurable; max TTL configurable) |
| Access | Unguessable link; optional password; optional max views (1 = one-time) |
| Link format | Four EFF-wordlist words, e.g. `amber-otter-quilt-river` (~51.7 bits); legacy 26-char base32 links keep working |
| Versions | Same link, version history, rollback |
| Encryption at rest | None (rely on host isolation) |
| Limits | 500 MB per version, 50 GB server total, 10 GB default per-user quota |
| Agent integration | CLI with thorough `--help`, `redrop guide`, `--json` output; no MCP server |

## Domains and isolation

Two separately configured origins:

* **App origin** (`REDROP_PUBLIC_URL`, e.g. `https://redrop.example.com`): dashboard,
  JSON API, OIDC callbacks, install scripts and CLI downloads.
* **Content domain** (`REDROP_CONTENT_URL`, e.g. `https://usercontent.example`): every
  artifact is served from its own subdomain `https://<slug>.usercontent.example/`. The
  apex serves only a landing page, `robots.txt`, and install scripts.

The content domain must be a different registrable domain from the app origin
so untrusted HTML can never be same-site with the dashboard or other services
(cookie tossing, same-site CSRF, Safe Browsing blast radius). Per-artifact
subdomains give each artifact its own browser origin, so artifacts cannot read
each other's storage, cookies, or service workers, while still allowing normal
`localStorage`/`fetch` inside one artifact.

The slug is the access secret. "Rotate link" replaces the slug; the artifact ID
used by the API/CLI is stable. Artifact slugs appear in TLS SNI and DNS
lookups; that is accepted.

### Slug format

New slugs are four words joined by `-`, e.g.
`https://amber-otter-quilt-river.usercontent.example/`, so links can be read
aloud and typed. Older slugs keep working forever and are not migrated.

| Format | Pattern | Entropy | Minted |
|---|---|---|---|
| Word (current) | `^[a-z]{3,9}(-[a-z]{3,9}){3}$` | 4 × log2(7772) ≈ 51.70 bits | creation and rotation |
| Legacy | `^[a-z2-7]{26}$` | 130 bits | never again; still resolved |

* **Wordlist.** The EFF large diceware list (7,776 entries, CC BY 3.0 US, see
  `NOTICE`) is embedded unmodified in the binary. redrop keeps entries
  matching `^[a-z]{3,9}$` and drops the 4 hyphenated ones (`drop-down`,
  `felt-tip`, `t-shirt`, `yo-yo`), leaving **7,772 words**. A test pins the
  file's SHA-256, the count, and the derived vocabulary's hash.
* **Why four words.** The owner compared three words (~38.8 bits, guessable)
  with four (~51.7 bits) and chose four. The floor is 50 bits; the wordlist
  test fails below it.
* **Generation.** Each word is drawn with `crypto/rand.Int`, which uses
  rejection sampling, so there is no modulo bias. The longest slug is 39
  characters, within the 63-octet DNS label limit. A collision with an
  existing slug retries with a fresh one (bounded; failure is an internal
  error).
* **Validation.** `internal/ids.ValidSlug` is the single syntax check and
  accepts exactly the two patterns above. The content host parser and the
  CLI use it before any lookup. The JSON API never accepts slugs, only
  artifact IDs, so its contract is unchanged.
* **Reverse proxy.** The content router must match both formats (see the
  README), or new links will not reach the server.

### Enumeration hardening

* Every miss on an artifact host looks the same: malformed, unknown, deleted,
  expired, used-up, or revoked links all get the same `404` "Not found" page
  with the same headers. (Before word slugs, unavailable links got `410`.)
  Malformed slugs are rejected without a database query; every other miss is
  a single indexed lookup, so all misses are in the same timing class.
* Misses are limited per client: a token bucket of `REDROP_CONTENT_MISS_BURST`
  (default 30) refilled at `REDROP_CONTENT_MISS_PER_MINUTE` (default 5). The
  client is the trusted-proxy-resolved address (`REDROP_TRUSTED_PROXIES`,
  `REDROP_CLIENT_IP_HEADER`, e.g. `CF-Connecting-IP` or Traefik's
  `X-Forwarded-For`), never a proxy's socket address; IPv6 clients are
  grouped by /64. At most 100,000 clients are tracked.
* Hits never spend budget, so normal use is not throttled however much it
  loads. A client that has spent its budget gets `429` with `Retry-After` for
  **every** artifact-host request, hits included, until a token refills. If
  hits stayed open to a spent client, the difference between `429` and
  `200` would itself reveal which guesses are live links, and the limiter
  would not slow enumeration at all.

## Content serving rules

Every content response sets:

* `X-Robots-Tag: noindex, nofollow, noarchive, nosnippet, noimageindex`, and
  `/robots.txt` disallows everything.
* `Referrer-Policy: no-referrer` so the secret subdomain is not leaked to third
  party CDNs the artifact loads.
* `Cache-Control: private, no-cache` (Cloudflare will not cache, so revocation,
  expiry and view limits are immediate) plus a strong ETag.
* `X-Content-Type-Options: nosniff`, `Content-Security-Policy: frame-ancestors`
  limited to the app origin (dashboard preview), restrictive
  `Permissions-Policy`.

Paths under `/__redrop/` are reserved for gate pages and cannot be uploaded.

### Icons

* redrop's own pages (dashboard, content apex, gate and message pages) link
  the redrop icon. On the content domain they link it under `/__redrop/`, so
  it never depends on an artifact. These server-owned icons are served with
  `Cache-Control: public, max-age=86400`.
* On an artifact origin, `/favicon.ico` and `/apple-touch-icon.png` return
  the artifact's own file to a visitor who has **proven access**: a valid
  visit or owner-preview grant, or, for ungated artifacts, the `seen` cookie
  a page view sets. Browsers request the icon after the page, so visitors
  still see the artifact's icon. It is served as `private, no-cache` content.
  Everyone else gets the redrop icon with `Cache-Control: private,
  max-age=86400`, whatever the slug: unknown, malformed, expired, revoked,
  exhausted, locked, or live. It is private so a CDN cannot pin the default
  over an icon the visitor unlocks later. These requests never count,
  consume, or log a view.
* Fallback icon requests are exempt from the miss rules above. They never
  spend miss budget and are answered even for a client whose budget is
  spent. Because they are free, they must not reveal whether a slug exists:
  without proof of access no lookup is made and the response is identical
  for every slug. Serving an artifact's own icon to anyone who could see its
  content would let unlimited icon probes find live artifacts that ship an
  icon. The `/__redrop/` icons on server pages are server-owned and never
  depend on the slug either. Artifact HTML is never rewritten. Directory
  listings and file viewers do not link an icon, so the browser's own
  `/favicon.ico` request decides.

### Gates and view counting

* **No password, no view limit:** served directly. A human document navigation
  without a recent `rd_seen` cookie counts as one view. Known link-preview bots
  are recorded as previews, not views.
* **Password and/or max views:** any request without a valid signed access
  cookie gets a neutral interstitial page (no title/metadata leak, noindex).
  Pressing *Open* (and entering the password if set) atomically consumes a view
  and sets a host-only, HttpOnly access cookie for a visit window (1 h for
  view-limited links, 12 h for password-only links) so the page can load its
  assets and be reloaded. Link-preview fetchers never press *Open*, so they do
  not burn one-time links.
* Changing the password, rotating the link, or revoking bumps an access
  generation number that invalidates every outstanding access cookie.
* Password attempts are rate limited per client IP and per artifact; failures
  are recorded in the view log.
* Owners preview through a short-lived signed preview URL created from the
  dashboard; previews never count as views and bypass gates.

### Lifecycle

* `active` → `expired` (TTL elapsed) or `exhausted` (max views reached) or
  `revoked` (manually, reversible).
* Expired artifacts keep content for `REDROP_EXPIRED_RETENTION` (default 72 h)
  so they can be extended/revived, then are deleted. Exhausted artifacts are
  deleted after the visit window ends. Deletion is immediate when requested.
* Blobs are content-addressed (SHA-256) and reference counted, so versions and
  identical files are deduplicated on disk. Quota counts logical bytes of all
  retained versions.

## Uploads

Cloudflare's free plan rejects request bodies over 100 MB, so uploads are
chunked and resumable:

1. `POST /api/v1/uploads {size}` → `{id, chunk_size}` (32 MiB chunks).
2. `PUT /api/v1/uploads/{id}?offset=N` with raw bytes, sequentially; on a
   mismatch the server answers 409 with the received offset so clients resume.
3. `POST /api/v1/artifacts` or `/api/v1/artifacts/{id}/versions` with
   `{upload_id, sha256, kind, …}` finalizes; the server verifies the SHA-256.

Bundles are uploaded as `tar.gz`. Extraction rejects absolute paths, `..`,
links, devices, reserved paths, and enforces file-count and decompressed-size
limits.

## Auth details

* Sessions: random 32-byte token in a `__Host-` cookie (Secure, HttpOnly,
  SameSite=Lax), stored hashed, 30-day lifetime.
* Cookie-authenticated mutating API calls require the `X-Redrop-Request: 1`
  header and a same-origin `Sec-Fetch-Site`/`Origin`, so cross-site requests
  are rejected (CSRF).
* API tokens may only manage the owner's uploads and artifacts and read `/me`;
  account, token and admin endpoints require a dashboard session.
* First start with no users prints a one-time setup URL to the log. Users join
  through admin-created invite links (optionally via OIDC). Forgotten passwords
  are reset with an admin-created reset link; no email is required.
* OIDC providers are configured by environment; identities are linked
  explicitly from the account page or by accepting an invite through OIDC.
  Accounts are never auto-linked by email address.

## CLI

`redrop` is a single static Go binary for Linux, macOS, and Windows (amd64 and
arm64). The server embeds all builds and serves `install.sh`/`install.ps1`, so
`curl -fsSL https://<app>/install.sh | sh` installs a version matching the
server and verifies SHA-256 checksums. Configuration is
`redrop login --url … --token …` or the `REDROP_URL`/`REDROP_TOKEN`
environment variables. Setting `REDROP_TOKEN` for the installer
(`curl … | REDROP_TOKEN=… sh`) also logs in; the dashboard shows that one-line
command when a token is created. Every command supports `--json`; exit codes are stable
(see `redrop guide`). The Go package `pkg/client` is the same client as a library.
