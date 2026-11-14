# redrop design

redrop is a self-hosted server for sharing agent-produced artifacts (HTML/React
pages, arbitrary files, and multi-file static bundles) through unguessable,
expiring, optionally password-protected or view-limited links, with a React
management dashboard and a cross-platform CLI that agents drive.

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
  artifact is served from its own subdomain `https://<slug>.usercontent.example/` (the
  shared link). HTML artifacts also have **raw hosts** for the viewer frame:
  `<slug>--raw` (current version) and `<slug>--v<N>` (version N); see "Viewer frame".
  The apex serves only a landing page, `robots.txt`, and install scripts.

The content domain must be a different registrable domain from the app origin
so untrusted HTML can never be same-site with the dashboard or other services
(cookie tossing, same-site CSRF, Safe Browsing blast radius). Per-artifact
subdomains give each artifact its own browser origin, so artifacts cannot read
each other's storage or cookies, while still allowing normal
`localStorage`/`fetch` inside one artifact. No artifact host serves a service
worker script (requests with `Sec-Fetch-Dest: serviceworker` or
`Service-Worker: script` get `404`): a worker would keep answering after the
link is revoked or expires, and could answer the viewer page's own
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
assets, frame entry, address-bar stream) and cannot be uploaded.

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

An artifact is exactly the bytes its owner uploaded. redrop never modifies
them and never asks them to contain anything redrop-specific. The info button
lives on redrop's own pages instead.

* **Modes.** `sharing.modeFor(kind, info)` is the only kind policy:
  * `html` and `bundle` with the info button on are **framed**: the shared
    link serves redrop's viewer page, which frames the artifact from a raw
    host.
  * With the info button off, they are served **directly** on the shared
    link, exactly as before the frame existed.
  * `file` artifacts always get redrop's **file page**, which renders the info
    panel natively when the button is on.
* **Hosts.** `ids.Host` parses and formats artifact host labels, next to
  `ids.ValidSlug`:
  * `<slug>` is the shared link (viewer, gates, direct and file views).
  * `<slug>--raw` serves the current version. It is one stable origin, so an
    artifact's storage survives updates.
  * `<slug>--v<N>` (N from 1 to 999999, no leading zero) serves version N on
    its own origin. Two tabs on different versions never mix files, and
    root-absolute URLs inside artifacts keep working.

  Valid slugs never contain `--`, so the suffix is unambiguous; the longest
  label is 48 octets. Version numbers are never reused (a high-water mark per
  artifact), so a pinned host never serves different bytes later.
* **Viewer page.** A top-level page navigation to a framed artifact gets
  redrop's viewer page after the usual gates, in this order:
  1. Directory redirects and missing paths answer as before, with `301` and
     `404`.
  2. The page is a full-viewport `<iframe>` of the raw host at the same path
     and query, with `allow="fullscreen; clipboard-write"` and no `sandbox`,
     so PDFs, downloads, modals and printing behave as before.
  3. The "i" button and the panel sit outside the frame, so the artifact can
     neither reach nor restyle them.

  The page runs no inline script; it loads only `/__redrop/viewer.js` and
  `/__redrop/viewer.css`. It sends these headers:
  * CSP `default-src 'none'`, with `script-src` and `style-src` pinned to
    those two paths;
  * `connect-src 'self'`;
  * `frame-src` of the raw host plus `https:`, because the parent's
    `frame-src` also governs where the frame navigates;
  * `frame-ancestors 'none'`;
  * `Cross-Origin-Opener-Policy: same-origin`;
  * `Cache-Control: private, no-store`, because the page carries grants.

  Non-document requests on the shared link (the subresources of a direct
  view) are served as before. Bytes fetched as `<object>`/`<embed>` there get
  an opaque-origin `CSP: sandbox allow-scripts`. A file artifact's own HTML, SVG
  or XML file gets `CSP: sandbox` when opened, since the file page shares its
  origin.
* **Raw hosts.** A raw host serves the stored bytes exactly, with the blob
  ETag and normal Range/conditional handling. Its headers:
  * `frame-ancestors 'self' <shared link>` (`'self'` lets an artifact frame
    its own pages), plus the dashboard on the `--raw` host only, for
    previews;
  * `Cross-Origin-Resource-Policy: same-origin`, so another artifact on the
    same site can't include its bytes as scripts or images with the
    visitor's raw cookie. Frames are navigations and are not affected.

  It handles requests as follows:
  * A top-level navigation (`Sec-Fetch-Dest: document`, as from
    `target=_top` links) gets `303` to the same path on the shared link, plus
    `redrop-version=N` for a pinned host. This is decided from the host label
    alone, before any lookup. Clients without Fetch Metadata are
    indistinguishable from curl, so for them the grant check below is the
    guard.
  * Every other request needs a raw cookie. Without a valid one, the request
    gets the identical miss (the same `404` as an unknown link) and spends
    miss budget.
  * Malformed raw labels, unknown slugs, bad enter or preview tokens, a wrong
    host version, the info button turned off, and older versions closed all
    give that same miss.
* **Grants.** The viewer page mints one frame per load:
  * **Enter token:** artifact ID, access generation, host version, kind
    (open, visit or preview), frame id, and the expiry of the visit or
    preview grant it came from; signed, for 2 minutes.
    `/__redrop/enter?t=…&next=…` on the raw host verifies it, sets the raw
    cookie, and redirects to `next`.
  * **Single use:** the token can be redeemed once. The server remembers
    redeemed frame ids for 2 minutes, in a set bounded at 100,000, so a
    copy from the page source or a proxy log is useless once the frame has
    loaded.
    * **Per-client quota:** one client (keyed like the miss budget: the
      trusted-proxy-resolved address, IPv6 by /64) may redeem at most 200
      frames per 2 minutes. Over that, a valid token gets `429` with
      `Retry-After`. That is not a miss, so it spends no miss budget, and
      other clients are unaffected.
    * **When the set is full** of unexpired ids (more than 500 clients at
      their quota at once), a valid token gets `503` with `Retry-After`,
      also not a miss. Ids are never forgotten early, because that would
      reopen replay.
  * **Raw cookie** (`__Host-rd_raw`): the same claim, valid for an hour.
    **Sliding refresh:** an admitted raw request renews it once less than
    half its life is left, so a page left open keeps loading. The renewal
    never goes past the visit (or preview) grant the claim came from, so a
    view-limited or password-protected visit still ends on time. An open
    link renews indefinitely, but every request is re-checked live.
  * **Live checks:** every raw request re-checks the claim against the
    artifact. A changed access generation (password change, rotation,
    revocation), expiry, an "open" grant after a password or view limit was
    added, the info button turned off, and older versions turned off each
    end access on the next request.
* **Cookies in a frame.** The raw host runs in a frame of the shared link.
  * **Registrable content domain:** the two are same-site.
  * **Content domain on the Public Suffix List:** every artifact host is its
    own site, and the frame is third-party.
  * **Choice:** the raw cookie is `Secure; HttpOnly; SameSite=None;
    Partitioned` (CHIPS), which works in both cases. The cookie is keyed to
    the top-level site, the viewer (or the dashboard, for previews), so it
    survives third-party cookie blocking where CHIPS is supported. Where a
    browser blocks partitioned third-party cookies, the frame fails closed
    with the miss page.
  * **Plain-HTTP development:** `SameSite=None` needs `Secure`, so it uses
    `SameSite=Lax`, and the viewer and raw hosts must be same-site there.
* **Views.** Counting is unchanged: only a page load on the shared link
  counts. Ungated links count once per browser per 30 minutes (`seen`);
  gated links count once at Open. Raw-host requests, enter, viewer assets,
  icons, and the address-bar stream never count.
* **Dashboard preview.** For framed artifacts the dashboard frames
  `<slug>--raw/__redrop/preview?t=…` directly. The owner token sets an
  owner-preview raw cookie (partitioned under the dashboard), which never
  counts and skips gates. The viewer page therefore never needs a framing
  ancestor and keeps `frame-ancestors 'none'`. The preview shows the
  content without the viewer's chrome.
* **Which requests get the viewer.** Only a top-level page load. A viewer
  page in a frame could never render (its `frame-ancestors` is `'none'`), so
  a frame on the shared link gets content, as do subresources.
  * **Decision:** a top-level page load is `Sec-Fetch-Dest: document`.
    Without Fetch Metadata (older browsers, curl, scripts), the `Accept`
    check used for view counting decides: an `Accept` containing
    `text/html`, `*/*`, or none at all. Such clients can't be told apart
    from a page load, and getting the viewer is the safe default for them.
* **Direct views.** `?redrop-info=0` on a framed artifact, and every page of
  an artifact with the info button off, are served directly on the shared
  link, as before.
  * **The choice lasts for the visit.** `?redrop-info=0` sets a signed,
    host-only cookie (30 minutes, bound to the artifact and its access
    generation), so the pages and frames a direct view loads stay direct.
    `?redrop-info=1` returns to the viewer and clears it.
  * **Only the visitor makes the choice.** The cookie is set or cleared only
    by a top-level navigation the visitor started: `Sec-Fetch-Dest:
    document` with `Sec-Fetch-Site: none` (typed, bookmarked) or
    `same-origin`. It is never set without Fetch Metadata.
    * A subresource, fetch, or page load from another artifact on the same
      site still gets a direct response for itself, but can't switch this
      link's viewer off for the visitor's later visits.
    * A cookie that is already set is honoured on every request.
  * **Isolation.** These pages run on the same origin as the viewer page, so
    four measures keep them from reaching it. Another site's pages can't
    turn the viewer off for a visitor, because of the rule above:
    * the viewer page's `Cross-Origin-Opener-Policy: same-origin` (no window
      reference);
    * `frame-ancestors 'none'` (they can't frame it);
    * the service-worker refusal (they can't intercept its loads);
    * the viewer reads no storage or cookies they could write.

  A service worker registered by an artifact before this release keeps
  running in browsers that already have it until the browser drops it.
  Operators who need a guarantee for such browsers can move to a fresh
  content domain.
* **Behaviour notes.**
  * **Address bar:** it follows in-frame navigation through the relay below.
  * **Popups:** the viewer page's `Cross-Origin-Opener-Policy: same-origin`
    also applies to popups the framed artifact opens. They get no
    `window.opener`, so sign-in flows that report back through the opener
    don't work inside the frame. Use `?redrop-info=0` or turn the info
    button off for such artifacts.
  * **Look-alikes:** the artifact can't touch the real button or panel, but
    a full-page artifact can draw something that looks like them inside its
    own frame. Only the real button opens the panel; nothing it shows comes
    from the artifact.
  * **Tab title:** the artifact's title, not the frame document's title.
  * **Printing:** the browser prints the viewer page, with the button,
    panel and version bar hidden and the frame filling the page. An artifact's
    own `window.print()` prints the frame. `?redrop-info=0` gives a
    frameless page for screenshots and exact printing.

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
  * **Off:** the list is metadata only, and `?redrop-version=N` redirects to
    the current version.
  * **On:** each version links to `?redrop-version=N`. The frame then loads
    `<slug>--v<N>` with a grant for exactly that host, and the viewer shows
    "Viewing version N of M · Back to latest" above the frame. M is the
    highest retained version number.
  * **Turning it off** closes pinned hosts on their next request.
  * **File artifacts** use the same parameter on the file page.
* **Rendering:** the panel is server-rendered (`html/template`) and opens
  with the native `popover` attribute (Esc and outside clicks close it).
  `viewer.js` only adds local and relative times, the countdown while the
  panel is open, and the address-bar relay. The panel is keyboard-accessible,
  works at phone width and on touch, and respects `prefers-color-scheme` and
  `prefers-reduced-motion`.

### Address bar

The address bar follows navigation inside the frame without anything from the
artifact.

* **Publishing:** a raw-host navigation the visitor made (`Sec-Fetch-Mode:
  navigate`, `Sec-Fetch-Dest: iframe`, `Sec-Fetch-User: ?1`, for a click or
  a sent form) admitted with a frame grant publishes its path and query for
  that frame id. These are never published:
  * subresources and fetches;
  * the frame's first load;
  * scripted navigations;
  * frames that load by themselves;
  * requests without Fetch Metadata.
* **Stream:** the viewer subscribes to
  `/__redrop/location?s=<token>`, a server-sent-events stream on the shared
  link. The token is minted with the frame and bound to the artifact, its
  access generation and the frame id. It lasts as long as the visit or
  preview grant behind it, or `streamMax` (12 hours) for an open link. A
  viewer page kept open longer still works (the raw cookie slides), but its
  address bar stops following until the page is reloaded.
* **Applying:** `viewer.js` applies a reported path only when this page's
  own frame fires `load` for it: the report arrives first and the frame
  loads within 5 seconds, or the frame loaded at most 1 second before the
  report arrived. Each load is used once. A report from a frame nested in
  the artifact, or from another tab, finds no matching load of this frame and
  expires unused. The path is mirrored with `history.replaceState` (never
  `pushState`, because the frame owns its history entries), keeping
  `redrop-version`, so copying or reloading lands on the same page.
* **Bounds:** paths up to 2 KiB; 10,000 remembered frames (least recently
  used without subscribers dropped, an hour at most); 4 streams per client
  and 1,000 in all (`429` beyond); a stream closes after 5 idle minutes,
  when its token expires, or at shutdown; a 25 s heartbeat.
* **Deployment:** the relay is in process memory, so it needs a single
  server instance (as SQLite already does).
* **Limits:**
  * **Tabs:** one raw cookie serves every tab of an artifact version in a
    browser, and the newest tab's frame id wins. An older tab's clicks
    reach the newest tab's stream and are ignored there, so the older tab's
    address bar stops following.
  * **Nested frames:** a click inside a nested frame followed within 5
    seconds by an unreported load of the outer frame (for example a
    script-driven one) can still be mistaken for it.
  * **Not reflected:**
    * single-page-app `pushState` and hash changes, because they don't load
      a document;
    * Back and Forward inside the frame, because a history traversal is not
      a click (`Sec-Fetch-User` is absent), so it isn't published. After
      going back, the address bar still shows the later page until the next
      click.
  * **Fairness:** the per-client and global stream limits mean that many
    clients holding streams open can deny other viewers the address bar,
    but nothing else.

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
