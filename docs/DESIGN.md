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
each other's storage or cookies, while still allowing normal
`localStorage`/`fetch` inside one artifact. No content host serves a service
worker script, not even redrop's own `/__redrop/` scripts (requests with `Sec-Fetch-Dest: serviceworker` or
`Service-Worker: script` get `404`): a worker would keep answering after the
link is revoked or expires, and could take over the viewer page's own
navigations.

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

Paths under `/__redrop/` are reserved for redrop (gate, preview, viewer
assets, icons, and older versions' files under `/__redrop/v/<N>/`) and cannot
be uploaded.

A request path that starts with `//`, or with a backslash after the first
slash (`/\`), gets the identical miss before gates, view counting and
routing. Browsers read such a path as protocol-relative, so echoing it into a
redirect, a frame `src` or a link would send the visitor to another site.
Every URL the content handler emits for its own origin (`Location`, the
viewer's frame, panel and file-page links) is also built by one helper,
`selfRef`, that guarantees exactly one leading slash.

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
  depend on the slug either. Artifact bytes are never modified. Directory
  listings and file viewers do not link an icon, so the browser's own
  `/favicon.ico` request decides.

### Viewer frame

An artifact is exactly the bytes its owner uploaded (see "Principles"). The
info button lives on redrop's own viewer page, which frames the artifact.

**Decision: the viewer page and the framed artifact share the artifact's
origin, `<slug>.<content domain>`.** An earlier design served the frame from a
separate per-artifact raw origin, so the artifact's scripts could not reach
the panel. That protection was worth little:

* an artifact can always draw a fake panel inside itself;
* the owner can turn the info button off;
* the panel is a convenience, not a security indicator.

It cost most of the feature's complexity: a second host per artifact and per
version (and a reverse-proxy rule for them), single-use enter tokens with a
replay set and per-client quotas, a partitioned third-party cookie with
sliding refresh, and a server-side relay so the address bar could follow a
cross-origin frame. Same origin removes all of that. Isolation between
artifacts, and between artifacts and the dashboard, is unchanged: every
artifact still has its own origin on a separate registrable domain.

**What it costs.** The artifact runs on the viewer page's origin, so its
scripts can read or change the viewer page (`window.parent`), including the
panel. Nothing the panel shows is secret: it is what the visitor may see
anyway. Look-alikes were already possible.

* **Modes.** `sharing.modeFor(kind, info)` is the only kind policy:
  * `html` and `bundle` with the info button on are **framed**: a top-level
    page load gets the viewer page.
  * With the info button off, they are served **directly**, as before the
    frame existed.
  * `file` artifacts always get redrop's **file page**, which renders the
    info panel natively when the button is on.
* **Which requests get the viewer page.** Only `Sec-Fetch-Dest: document`
  for a framed artifact without a direct-view choice. Everything else gets
  the stored bytes, untouched, with the blob ETag, exactly as before:
  `Sec-Fetch-Dest: iframe` (the viewer's own frame, the artifact's nested
  frames, the dashboard preview), subresources, and requests without Fetch
  Metadata. Both answers carry `Vary: Sec-Fetch-Dest`.
  * **Fallback without Fetch Metadata.** Very old browsers, curl, scripts,
    and browsers on plain HTTP to a non-localhost host (Fetch Metadata is
    sent only to secure contexts) get the artifact directly, with no button.
    That is intended: such a request can't be told apart from a subresource,
    and the artifact is always the safe answer.
* **Viewer page.** Served after the usual gates. Directory redirects and
  missing paths answer as before (`301`, `404`); only real pages get the
  viewer.
  * A full-viewport `<iframe>` whose `src` is the same path and query
    (without `redrop-info` and `redrop-version`), with
    `allow="fullscreen; clipboard-write"` and no `sandbox`, so PDFs,
    downloads, modals and printing behave as in a tab.
  * The "i" button and the panel sit outside the frame.
  * No inline script; it loads only `/__redrop/viewer.js` and
    `/__redrop/viewer.css`. Its CSP is `default-src 'none'`, with
    `script-src` and `style-src` pinned to those two paths, `img-src 'self'`,
    `frame-src 'self' https:` (the parent's `frame-src` also governs where
    the frame navigates), `form-action 'none'`, `base-uri 'none'`, and
    `frame-ancestors 'none'`.
* **Content.** Unchanged from before the frame: `frame-ancestors 'self'`
  plus the dashboard origin, so the viewer (and an artifact's own frames) and
  the dashboard preview can frame it.
* **Service workers.** No artifact host serves a service worker script, so an
  artifact can't install one that answers the viewer page's navigations. A
  worker registered before this release keeps running in browsers that
  already have it until the browser drops it; operators who need a guarantee
  can move to a fresh content domain.
* **Views.** Exactly one per visit. The viewer page load counts; its frame's
  request doesn't, because only top-level loads count (`Sec-Fetch-Dest:
  document`, or, without Fetch Metadata, the `Accept` check). Ungated links
  still count once per browser per 30 minutes (`seen`); gated links count at
  Open. Frames, subresources, viewer assets and icons never count.
* **Gates.** Unchanged, on this origin: the frame's request carries the same
  visit cookie as the viewer page. A gate shown in the frame (a visit grant
  that ran out while the page was open) works there too.
* **Dashboard preview.** Unchanged: the dashboard frames
  `/__redrop/preview?t=…`, which sets the partitioned owner-preview cookie and
  redirects to `/`. That load is a frame, so it gets the content, without
  the viewer's chrome.
* **Direct views.** `?redrop-info=0` on a framed artifact, and every page of
  an artifact with the info button off, are served directly.
  * **When the choice is remembered.** `?redrop-info=0` is remembered for
    30 minutes in a signed, host-only cookie (bound to the artifact and its
    access generation) when the link is opened directly (typed, bookmarked,
    opened from another app) or from the same page. The pages that direct
    view loads then stay direct. `?redrop-info=1` returns to the viewer and
    clears it.
  * **Clicked on another website, only that page is direct.** The cookie is
    set or cleared only by a top-level navigation from the address bar or
    from this origin: `Sec-Fetch-Dest: document` with `Sec-Fetch-Site:
    none` or `same-origin`. A link clicked on another website arrives as
    `cross-site`: that page is direct, and the next page opened from it gets
    the viewer again. The cookie is never set without Fetch Metadata.
    * Allowing `cross-site` would let any page anywhere switch the viewer
      off. Allowing `same-site` would let another artifact on the content
      domain do it, with an image, a fetch, or a page load. Such requests
      still get a direct response for themselves, but can't switch this
      link's viewer off for the visitor's later visits.
    * A cookie that is already set is honoured on every request.
* **Behaviour notes.**
  * **Tab title:** the artifact's title, not the frame document's title.
  * **Popups:** opened from the frame, they keep `window.opener` as before.
  * **Printing:** the browser prints the viewer page, with the button,
    panel and version bar hidden and the frame filling the page. An
    artifact's own `window.print()` prints the frame. `?redrop-info=0`
    gives a frameless page for screenshots and exact printing.

### Info panel and versions

* **Shown:** the title; the shown version and when it was published (relative
  and absolute, in local time); the expiry as a live countdown or "never
  expires"; the views left of a view-limited link ("one-time link: this was
  the only view"); "password-protected"; the shown version's size and file
  count; and "Shared with redrop" with the redrop mark.
* **Versions:** every retained version is listed, newest first, with its
  number and publish time, and the current one marked.
* **Change notes:**
  * **New notes are public.** A version published with this release or later
    shows its change note (`--note`) to viewers.
  * **Older notes stay private.** Notes written before this release were
    owner-only, and they stay that way. Each version has a `note_public`
    flag, false for migrated rows and true for new ones.
  * **Where it's stated:** the CLI's `--note` help, `redrop versions` (`*`
    marks public notes), the dashboard's version list ("shown to viewers" or
    "private"), and the API (`note_public`).
  * **Known limitation (owner decision):** the flag is read-only. An owner
    can't publish an older note, or keep a new one private, except by
    leaving the note out. A per-version toggle may come later.
* **Never shown:** the owner's identity, usernames, emails, artifact or token
  IDs and names, the slug, other versions' sizes or contents (unless the
  owner lets viewers open them), or the view count of an unlimited link. The
  rule is: what the visitor can see or infer from the page they were let
  into, plus the owner's published version history. `sharing.ViewerInfo` and
  `sharing.Revision` hold exactly these fields, and tests pin them.
* **Older versions:** "Viewers can open older versions" is a per-artifact
  setting, off by default; it needs the info button.
  * **Off:** the list is metadata only, and `?redrop-version=N` gets the
    identical miss, like the version path. So does a version that isn't
    retained or a malformed N. The current version's own number shows the
    current version.
  * **On:** each version links to `?redrop-version=N`. The viewer frames
    `/__redrop/v/<N>/<path>` on the same origin and shows "Viewing version N
    of M · Back to latest" above the frame. M is the highest retained
    version number. Version numbers are never reused (a high-water mark per
    artifact), so a saved link never shows other bytes.
  * **Version paths.** `/__redrop/v/<N>/<path>` serves version N's files
    after the usual gates, only while the owner allows older versions (and
    the info button is on), and only for a retained N. Anything else (off,
    unknown N, malformed N such as `01`) gets the identical miss. A top-level
    load of a version path of a framed artifact redirects to
    `<path>?redrop-version=N`, so the address bar keeps one form; with a
    direct-view choice it is served directly. `?redrop-version=N` on a
    request that doesn't get the viewer (a direct view, a file's own bytes)
    redirects to the version path.
  * **Known limit:** relative links inside an older version stay in it, but
    a root-absolute link (`/x.html`) leads to the current version. The
    address bar then drops the version parameter, while the version bar
    stays until the next top-level load.
  * **Turning it off** closes version paths on their next request.
  * **File artifacts** use the same parameter on the file page; its links
    point at the version path.
* **Rendering:** the panel is server-rendered (`html/template`) and opens
  with the native `popover` attribute (Esc and outside clicks close it).
  `viewer.js` only adds local and relative times, the countdown while the
  panel is open, and the address bar below. The panel is keyboard-accessible,
  works at phone width and on touch, and respects `prefers-color-scheme` and
  `prefers-reduced-motion`.

### Address bar

The address bar follows the frame without anything from the artifact. The
frame is on the same origin, so `viewer.js` reads its location directly:

* on every frame `load`;
* on every same-document URL change in the frame's current document: a
  `#fragment`, Back/Forward, and `history.pushState` or `replaceState`
  (single-page-app routes, UI state kept in the fragment). The last two fire
  no window event, and the viewer never patches the artifact's `history`, so:
  * where the frame's window has the Navigation API, the viewer listens for
    `currententrychange` on its `navigation` object;
  * otherwise it listens for `hashchange` and `popstate`, and polls the
    frame's `location.href` every 250 ms, updating only when it changed;
  * this runs only while the viewer page is visible
    (`document.visibilityState`): hiding the page removes the listener and
    the timer, and showing it reads the frame once and resumes;
  * each document has a new window, so this is set up again after every
    load; the previous timer and `navigation` listener are removed first
    (the old window's own listeners go away with its document);
* it mirrors the frame's path, query and fragment;
* it maps `/__redrop/v/<N>/<path>` back to `<path>?redrop-version=N` and
  ignores other `/__redrop/` pages;
* it applies the result with `history.replaceState` (never `pushState`: the
  frame owns its history entries), so copying or reloading the address lands
  on the same page;
* reads are wrapped in `try`/`catch`: when the frame navigated to another
  origin, the address bar stays as it was;
* the server never sees a URL fragment, so `viewer.js` hands the visitor's
  `#fragment` to the frame when the page loads.

Nothing is added to the artifact and nothing in its window is written or
patched (`history` is never wrapped); the viewer only reads and listens. Not
reflected: navigations to another origin, whose location the viewer can't
read. Without the Navigation API, a `pushState` or `replaceState` shows up
within 250 ms rather than at once.

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
