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
| Encryption at rest | None by default (rely on host isolation); opt-in end-to-end encryption per artifact, key in the link (see [End-to-end encrypted links](#end-to-end-encrypted-links)) |
| Limits | 500 MB per version, 50 GB server total, 10 GB default per-user quota |
| Agent integration | CLI with thorough `--help`, `redrop guide`, `--json` output; no MCP server |
| Viewer feedback | Off by default; per-artifact comments from the info panel, read back by the owner and agents (see [Viewer feedback](#viewer-feedback)) |

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
navigations. The one exception is redrop's own decrypting worker on an
end-to-end encrypted artifact's host, which is built to avoid both (see
[End-to-end encrypted links](#end-to-end-encrypted-links)).

On a content domain that is not on the Public Suffix List, an artifact can set
ordinary parent-domain cookies sent to sibling artifact hosts. Host-only
`__Host-` access cookies cannot be forged this way, but cookie flooding can
make a visitor's requests to other artifacts too large. The owner accepts this
per-browser availability limitation because publishing is invite-only. Keep
invitations restricted to trusted accounts. Separate origins do not make an
unlisted content domain a cookie suffix boundary.

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
  `REDROP_CLIENT_IP_HEADER`, `REDROP_TRUSTED_HOPS`; see "Rate limits"),
  never a proxy's socket address; IPv6 clients are grouped by /64. At most 100,000 clients are tracked.
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
assets, icons, older versions' files under `/__redrop/v/<N>/`, and the
feedback endpoint, see [Viewer feedback](#viewer-feedback)) and cannot be
uploaded.

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
  artifact can't install one that answers the viewer page's navigations.
  (End-to-end encrypted artifacts get redrop's own worker, scoped to
  `/__redrop/v/`, which never sees the viewer page's navigations.) A
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
* Password attempts are rate limited per client network and per artifact;
  failures are recorded in the view log. An artifact takes about 50 wrong
  passwords per hour from all networks together (up to twice that across a
  window boundary: the budget is a fixed one-hour window), then its link is
  locked for everyone until the hour has passed. Each attempt is charged to that
  budget before the password is checked, and a right password gives its
  charge back, so concurrent guesses cannot exceed it and visitors who know
  the password never use it up. The view log keeps the count across
  restarts.
* Owners preview through a short-lived signed preview URL created from the
  dashboard; previews never count as views and bypass gates.

### Lifecycle

* `active` → `expired` (TTL elapsed) or `exhausted` (max views reached) or
  `revoked` (manually, reversible).
* An owner's and an administrator's revocations are stored apart
  (`revoked`, `admin_revoked`); either one makes the artifact `revoked`.
  The owner can undo only their own: restoring an artifact an
  administrator revoked is refused (`403`), and the API (`revoked_by_admin`),
  the dashboard and `redrop info` say "revoked by an administrator". Other
  owner changes still work and do not bring the link back. An
  administrator's restore clears only the administrator's revocation.
  Revocations made before migration 4 count as the owner's.
* Expired artifacts keep content for `REDROP_EXPIRED_RETENTION` (default 72 h)
  so they can be extended/revived, then are deleted. Exhausted artifacts are
  deleted after the visit window ends. Deletion is immediate when requested.
* Blobs are content-addressed (SHA-256) and reference counted, so versions and
  identical files are deduplicated on disk. Quota counts logical bytes of all
  retained versions.

## Viewer feedback

Viewers can leave comments on an artifact, and the owner (or the owner's
agent) reads them back with `redrop feedback`. It answers issue #10. One
part of the issue is replaced by the principle above: the issue says "the
server injects a widget", but nothing is injected. The form is part of the
info panel on redrop's own viewer page, outside the frame.

### Decisions

* **Off by default, per artifact.** The owner turns it on with
  `redrop share/update/set --feedback` (`--no-feedback` turns it off), the
  dashboard's settings switch, or `feedback` in the API. Existing artifacts
  are migrated with feedback off.
* **It needs the info panel.** Comments are open only while feedback *and*
  the info button are on, like opening older versions. With the info button
  off, HTML is served directly: there is no redrop page to hold a form, and
  nothing is ever added to the artifact. `?redrop-info=0` hides the form
  for that page; the endpoint stays open.
* **Who can comment:** anyone who can view the artifact right now, by the
  same decision as its content (`sharing.Decide`). The link must not be
  expired or revoked, a password must have been entered, and a view-limited
  link's visit grant must still be valid. An owner preview counts too.
  Posting never records or consumes a view.
* **Who can read:** only the owner, from the dashboard, the CLI or the API
  (token or session). Visitors never see other people's comments; after
  posting they only get a confirmation.
* **Admins: as for other artifact data, metadata only.** For other users'
  artifacts, admins see metadata ("Admin privacy"), never content, versions,
  files or the access log. Comments quote the artifact's text, so they count
  as content: admins see the `feedback` setting, not the comments. This is
  the owner's privacy default. Deleting an artifact (owner or admin)
  deletes its comments.

### What a comment holds

| Field | Rule |
|---|---|
| `body` | 1–4000 characters (Unicode code points) after trimming surrounding whitespace |
| `name` | optional, at most 80 characters, one line; unverified, chosen by the viewer |
| `version` | the version on screen, between 1 and the artifact's version high-water mark |
| `page` | `pathname + search + hash` inside the artifact, the value the address bar mirrors (`/x.html?redrop-version=2#intro`); starts with exactly one `/`, at most 2048 bytes, never under `/__redrop/` |
| `quote` | optional W3C text quote selector of the selection: `exact` 1–500 characters, `prefix` and `suffix` at most 64 each |
| `created_at`, `resolved_at` | server times; `resolved_at` is null for an open comment |

Text that renders differently from what a reader receives is removed on
input:

* **Line breaks.** In `body` and `quote`, every line break (`\r\n`, `\r`,
  U+2028 LINE SEPARATOR, U+2029 PARAGRAPH SEPARATOR, U+0085 NEXT LINE)
  becomes `\n`. They are removed from `name`.
* **Controls.** Control characters (C0, DEL, C1) are removed, except
  line feeds in multiline fields and tabs in `body` and `quote`.
* **Invisible characters.** The tag block (U+E0000–U+E007F, which can
  spell hidden ASCII text for an agent) and the invisible format
  characters U+200B, U+2060–U+2064, U+FEFF and U+180E are removed. ZERO
  WIDTH JOINER and NON-JOINER stay: emoji and several scripts need them.
* **The page** is not cleaned but checked: a `page` with any of these
  characters is rejected.

The server stores no IP address, user agent, country or cookie with a comment.
The per-client rate limiter keeps the client's address in memory only.

**Storage (migration 7).** `artifacts.feedback INTEGER NOT NULL DEFAULT 0`
and a `feedback` table: `id`, `artifact_id` (cascade on artifact delete),
`version`, `page`, `body`, `name`, `quote_exact`, `quote_prefix`,
`quote_suffix`, `created_at`, `resolved_at`, indexed by
`(artifact_id, created_at)`. Comments keep their version number when that
version is deleted or rolled back, and survive link rotation, since they
belong to the artifact, not the slug. Older servers ignore the column and
table, so rolling back the binary just turns comments off.

### Endpoint

`POST /__redrop/feedback` on the artifact's own host, JSON only:

```json
{"body": "The totals in section 3 are wrong", "name": "Sam", "version": 2,
 "page": "/report.html#totals", "quote": {"exact": "1,204", "prefix": "Total: ", "suffix": " units"}}
```

The request passes these checks in order. The first one that fails decides
the answer:

1. The client's miss budget is not spent (otherwise `429`, as for every
   request).
2. The slug is live: it resolves, and the link is not expired, revoked or
   used up (`ShowGone`). Otherwise the answer is the generic `405` below,
   and one token of miss budget is spent, as for any miss.
3. Feedback is open (setting and info button on). Otherwise: the generic
   `405`.
4. `Sec-Fetch-Site`, when present, is `same-origin`, and `Origin` equals the
   artifact's origin exactly. Otherwise `403`.
5. `Content-Type` is `application/json` (parameters such as `charset`
   allowed). Otherwise `415`.
6. The visitor has access (no gate to pass). Otherwise `403`.
7. A token from the per-client feedback bucket, keyed like the miss limiter
   (trusted-proxy address, IPv6 by /64): `REDROP_FEEDBACK_BURST` (default 5)
   refilled at `REDROP_FEEDBACK_PER_MINUTE` (default 1). Otherwise `429`
   with `Retry-After`.
8. A body of at most 64 KiB that decodes as one JSON object with only the
   known fields. Otherwise `413` or `400`.
9. Valid fields (table above). Otherwise `400`.
10. Fewer than `REDROP_FEEDBACK_MAX_PER_ARTIFACT` (default 500) comments
    stored for the artifact, open or resolved. Otherwise `409`: "This page
    isn't accepting more feedback". The owner frees room by deleting
    comments.

Success is `201 {"ok": true}`. Errors are `{"error": code, "message": text}`
with a short, user-safe message. **The generic 405** is byte for byte what a
`POST` to any other `/__redrop/` path gets (`Allow: GET, HEAD`, same body
and headers). So with feedback off, the endpoint can't be told apart from
an unknown path. A `GET` of `/__redrop/feedback` is always the generic
reserved-path `404`. No response carries `Access-Control-*` headers, and
preflight `OPTIONS` requests get the generic `405`, so a cross-origin page
can't post or read the answer.

Cloudflare's rule on the content zone (10 requests per 10 seconds per
client) also counts these POSTs.

### Viewer side

* The panel template renders a "Feedback" form (comment, optional name, a
  quote preview, Send) only when feedback is open. It is `hidden` until
  `viewer.js` runs, so without scripts there is no form that can't be sent.
  The page's CSP adds `connect-src <origin>/__redrop/feedback` only then; the
  form has no action and `form-action` stays `'none'`.
* **Anchoring.** When the panel opens, `viewer.js` reads the frame's
  selection passively: `frame.contentWindow.getSelection()`, `toString()`,
  and detached `Range` objects from `createRange()` for the 64 characters
  before and after. A selection longer than 500 characters is shortened, and
  its suffix is taken after the shortened text, so the triple stays a valid
  text quote. Strings are cut before they are split into code points, so the
  work doesn't grow with the page or the selection. `Range.toString()`
  includes the text of `<script>` and `<style>` elements, so `prefix` or
  `suffix` can hold code next to the selection. That is harmless for
  anchoring and only looks odd in the dashboard; it is left as is. Nothing is written to the frame: no elements, attributes,
  styles or listeners. Selection in the frame is read only on panel open, so
  there is no `selectionchange` listener on the artifact. The file page
  reads its own selection (the text preview), outside the panel.
* **Page and version** come from the frame's location, mapped as for the
  address bar (`/__redrop/v/N/p` becomes `p?redrop-version=N`, version N).
  Any other same-origin location is the current version. When the frame is
  on another origin, the address bar's last value and the page's version are
  used.
* After `201` the form clears and shows "Thanks, your feedback was sent."
  Error messages come from the server's `message`. Text is set with
  `textContent` only.

### Owner side

* **API** (`anyAuth`, owner only; other users' artifacts are `404`):
  * `GET /api/v1/artifacts/{id}/feedback?status=open|all&version=N` lists
    comments, oldest first. The default status is `all`.
  * `PATCH /api/v1/artifacts/{id}/feedback/{comment}` with
    `{"resolved": true|false}` returns the comment.
  * `DELETE /api/v1/artifacts/{id}/feedback/{comment}` deletes it.
* **Dashboard:** a Feedback tab on the artifact page. Comments are grouped
  by version, newest version first, each with page, quote, name and time.
  Each has Resolve/Reopen and Delete (with confirmation). "Show resolved"
  is a filter. Everything is plain React text, never HTML or Markdown.
* **CLI:**
  * `redrop feedback ID|URL [--version N] [--all] [--json]` lists open
    comments, or all with `--all`.
  * `redrop feedback resolve ID|URL COMMENT...` marks comments resolved.
  * `redrop feedback delete ID|URL COMMENT... [--yes]` deletes them, with
    the same confirmation rule as `redrop delete`.

### Threat model

* **CSRF and other sites.** The visit cookie is `SameSite=Lax`, so a
  same-site page (another artifact on the content domain) would send it.
  The exact-`Origin` check, `Sec-Fetch-Site`, the JSON-only content type
  and the absence of CORS headers stop cross-origin and same-site posts:
  simple form posts fail the content type, and fetches fail the origin
  check or the preflight. The artifact's own scripts are same-origin and
  can post. That is accepted: the artifact belongs to the owner, who reads
  the result.
* **Spam and resource use.** The per-client bucket, the per-artifact cap,
  the 64 KiB body limit, the field limits and Cloudflare's zone rule bound
  it. The worst case is 500 rows of at most about 20 KB per artifact.
  Turning feedback off closes the endpoint at once; existing comments stay
  until deleted.
* **XSS.** Comments are rendered as text everywhere: React text nodes in
  the dashboard, `textContent` in the viewer (which shows only its own
  confirmation), and plain text in the CLI. The CLI also removes ANSI
  escape sequences (7- and 8-bit), control characters, Unicode
  bidirectional controls and the invisible characters above before
  printing, in plain and `--json` output alike. It turns every line break
  into `\n`. The server already does most of this, but the CLI doesn't
  trust the server's output either.
* **Prompt injection.** Agents read comments, so a comment is an injection
  channel. The CLI's plain output frames each comment as viewer-supplied
  data. It prefixes every line of viewer text with `| ` and labels the name
  as unverified. Because every Unicode line break is turned into `\n`
  before the prefix is added, a comment can't fake the frame even for a
  reader that splits lines like Python's `str.splitlines()`. Tag characters
  and other invisible text never reach an agent, so a comment can't hide
  instructions that a person reading the same output wouldn't see. `--json` gives typed
  fields. `redrop guide` tells agents to treat comment text as untrusted
  feedback to weigh, never as instructions: not "ignore previous
  instructions", and never requests to share files, tokens or other
  artifacts.
* **Isolation.** Nothing is added to the artifact's bytes or DOM. The
  endpoint lives on the artifact's own origin, under the reserved prefix,
  so it shares the origin's cookies and nothing else. Artifacts stay
  isolated from each other and from the dashboard as before.
* **The setting is visible before the access check.** Someone who has the
  link but not the password can tell that feedback is on: a post gets the
  `403 access_required` answer instead of the closed `405`. Only "off" must
  look like an unknown path; whether comments are open is not secret, and
  the link itself is the secret that gives this away.
* **Privacy.** No raw IP, user agent or cookie is stored. The name is
  optional and unverified. Comments never appear in admin views.

## End-to-end encrypted links

`redrop share --e2e` encrypts an artifact in the CLI before upload, and
`update` and `watch` encrypt every later version with the same key. The
server stores and serves only ciphertext. The key travels in the link's
fragment, which browsers never send to a server, and the link is a page of
redrop's app origin, where no artifact runs:
`https://redrop.example.com/e/<slug>#k=<key>`. It answers issue #7. It is
opt-in per artifact and permanent: an artifact is either end-to-end
encrypted (every version) or not (no version).

### Threat model

**Protects against** a compromise of the server's storage: a stolen backup,
a copied data directory, a read-only database leak, or an operator browsing
the disk. From those an attacker learns only:

* that the artifact is end-to-end encrypted, its kind (`html`, `file` or
  `bundle`), its title and change notes when the owner gave them (both are
  plaintext settings), and the usual metadata: owner, times, expiry, views,
  password hash, slug;
* the number of files in each version (plus one, the manifest);
* each file's size, exactly: ciphertext is 20 bytes plus the plaintext plus
  16 bytes per 64 KiB segment, with no padding. The CLI uploads the
  objects in a random order, so neither the stored rows nor the blobs'
  write order follow the source's path order; but someone who can guess a
  site's layout can still match files to it by their sizes;
* when versions were published and viewed;
* viewer comments, if the owner turned feedback on. They are plaintext,
  including the quoted text and the page path (see below).

It does not learn file contents, file names or paths, content types, the
start page, or the page `<title>`. It never receives plaintext for an
end-to-end artifact either: `update` and `watch` read the artifact before
uploading and encrypt with its key, or refuse, and the server refuses an
upload declared for such an artifact unless it is declared encrypted,
before any byte of it arrives (see "Publishing"). **Paths are encrypted**:
the server stores an encrypted manifest and opaque random object names, and
the viewer resolves paths from the manifest (see "Viewing"), so the server
never needs them.

**Does not protect against a malicious or compromised live server.** The
page that decrypts is redrop's viewer, delivered by the same server. A
server that wants the key can serve a viewer script that sends it home, the
next time someone opens the link. This is the known limit of every
web-delivered end-to-end scheme, and redrop does not pretend otherwise. What
a malicious server can still do without changing scripts: withhold or
delete content, serve an older genuine version as the current one
(rollback), refuse to serve a version, and see request timing. Without the
key or derived object keys, it cannot alter content, swap files between
paths, versions or artifacts, or make a viewer show bytes the owner did not
encrypt: every object is authenticated (see "Format"). A script that ran
with the key can supply derived object keys to such a server, as below.

**Scripts inside the artifact** (its own code, an analytics tag, a CDN
library, an XSS in a generated report) run on the artifact's origin, as for
plain artifacts. They share the viewer's window, so they can read anything
it holds and rewrite its address, and can replace any function or getter on
it; they can open windows and keep handles on them, and on the visitor's
tab. They still can't obtain the key, because its text never reaches their
origin (review 33 third re-check B-2a″, closed by the owner's choice of key
intake on another origin):
* the link is the **app origin's intake page**,
  `https://<app>/e/<slug>#k=<key>` (see "Where the key goes in the
  browser"). No artifact runs on the app origin, and a window on the
  artifact's origin can't read the address of a page on another origin,
  so the key arrives where none of their code, and no window they hold
  (a popup, a tab they opened, the visitor's own tab), can read it;
* only a non-extractable `CryptoKey` crosses to the artifact's origin, by
  `postMessage` between two of redrop's own pages;
* a key-like fragment or query on the artifact's origin is a damaged link,
  never framed (see "Key attempts" and "A link whose `#` became `%23` or
  `?`");
* once the frame exists, the navigation guard refuses every same-document
  change of the viewer's address that the viewer didn't make (see "The
  navigation guard"), so the artifact can't make the visitor's later edits
  of the address reach it either.

They can *use* the non-extractable `CryptoKey` the viewer stores: decrypt
what they can fetch (the current version, and older ones only when the
owner lets viewers open them), and send out what they decrypted. They
can't copy the key itself, but they can derive and send out object keys.
Those keys decrypt those objects later and can make objects that pass
authentication for this artifact if they are ever stored on the server.
A script that ran with the key is a reason to share a new artifact:
neither `rotate` nor `update` changes the key. A browser test
(`scripts/e2e-browser.mjs`) checks
every place such a script could look, with a probe artifact that watches
and one that acts: it rewrites the address (also from inside a capturing
listener or the viewer's own update), replaces the functions the viewer
could use, replaces the viewer's document, opens a popup that holds the tab,
or opens a window the visitor then opens the link in, before the full link
is opened. Run against the build before intake moved, the popup and window
cases read the key; now none does.

**The key is permanent.** It is the same for every version, and `rotate`
keeps it (only the slug changes). There is no way to change an artifact's
key: whoever holds the full link, key included, can read every version,
past and future, for as long as the link works. To cut someone off,
rotate the link or share a new artifact. A lost key means lost content;
nobody can recover it.

**Also out of scope:** the visitor's device and browser extensions, and
anyone the link is forwarded to. The visitor's browser history and
address-bar suggestions keep the full link, key included, as they keep
any URL the visitor opened; the viewer can't remove that.

### Keys and links

* **Key:** 32 random bytes per artifact (`crypto/rand`), kept for every
  version, so `update` and `watch` never change the link. In links it is
  unpadded base64url, 43 characters.
* **Link:** `https://redrop.example.com/e/<slug>#k=<key>`: the key
  intake page on the app origin (`REDROP_PUBLIC_URL`), named by the
  artifact's slug. The API returns it, without the key, as `e2e_link` in
  the owner's views; the CLI prints it with the key (and `--json` returns
  that as `url`); the dashboard shows it. The artifact's own address
  (`url`, `https://<slug>.usercontent.example/`) opens nothing by itself.
  The server never had the key. The app origin must be reachable by
  everyone the link is for: a dashboard behind a private network can't
  serve end-to-end encrypted links.
* **Fragment grammar.** `#k=<43 characters>`, optionally followed by `&`
  and the rest. A rest that starts with `/` is a page inside the artifact
  with its own query and fragment (`#k=…&/docs/a.html?x=1#part`); any other
  rest is the artifact's own fragment for its start page (`#k=…&part`).
  After the intake, the viewer gets the rest as its own fragment. A
  fragment without a key is all rest.
* **Key attempts.** Any fragment that could hold a key is a key attempt
  (review 33 re-checks, B-2b, B-2b' and m-7). The fragment is
  percent-decoded as often as that changes it (up to 8 times; only escapes
  of ASCII characters, so a malformed one doesn't stop the rest). Every
  non-ASCII character, and every escape of a non-ASCII byte left, then
  reads as a space. The result is matched for:
  * `k` in either case, at the start or after any character that is not a
    letter or digit, then up to 3 characters that are neither, then `=` or
    `:` (`K=…`, `&k=…`, ` k=…`, `x&k=…`, `/a.html?k=…`, `k =…`, `k:…`, and
    encoded forms such as `k%3D…`, `%6B=…`, `k%253D…` and a zero-width
    space `k%E2%80%8B=…`);
  * a run of at least 40 base64url characters with both upper and lower
    case letters, such as a key that lost its `k=`.

  Unless the fragment is exactly the form above, with no key attempt in
  its rest and no 12 characters of its key in a row in the rest (in either
  case, however split: a second, damaged copy of the link), the link is
  damaged. The viewer says so, takes the fragment off
  the address bar, frames nothing (even with a key stored from an earlier
  visit), and passes no part of the fragment on. The cost: an artifact's
  own fragment or page path of either shape (an anchor `#k=2`, a page
  `?k=1`, a 40-character mixed-case anchor) can't be linked to directly.
  Lower-case anchors and file names of any length are unaffected. The CLI
  (`internal/e2e`) applies the same rule to links it is given, so a
  damaged key there is an error, not a link without a key;
  `internal/e2e/testdata/fragments.json` holds the cases both sides test.
  Residual: a link damaged twice over, its `k=` lost entirely (or
  `xk=`, `key=`) and its key split by a separator or case-folded, passes
  as the artifact's own fragment, key characters included. Catching that
  would also refuse ordinary mixed-case page paths. A case-folded key
  would still need about 2^35 tries against the public key check.
* **Where the key goes in the browser.** The artifact runs on its own
  origin, which the viewer shares (see "Viewer frame"), so the key's text
  never goes there (review 33 B-2 and its re-checks):
  1. The visitor opens the link: the intake page on the app origin. It
     holds no artifact data and looks nothing up, so it says nothing about
     the slug, counts no view and opens no gate. It is never cached
     (`no-store`), framed (`frame-ancestors 'none'`) or referred
     (`no-referrer`); its policy allows only its own script
     (`/e2e/intake.js`, the decryptor and the intake) and its pinned style.
     It has `Cross-Origin-Opener-Policy: same-origin-allow-popups`, which
     cuts any window that opened it and keeps only the popup it opens
     itself. Logs redact the slug in `/e/` paths like a token.
  2. Its script takes the fragment off the address bar first
     (`history.replaceState`, before anything asynchronous), applies the
     key-attempt rule, and imports the key as a non-extractable HKDF
     `CryptoKey`. The text stays on the app origin, in that page's memory
     and its navigation-timing entry; browser history and address-bar
     suggestions also keep the original full link.
  3. The visitor presses **Open**. That click lets the page open a small
     window on the artifact's origin, `/__redrop/e2e-intake`: redrop's
     helper page, with a policy that allows only its own script and no
     framing. The content handler serves it only for a live end-to-end
     encrypted artifact, after one slug lookup like any visit (no gate,
     no view). Every other slug, unknown, gone, plaintext or a tunnel,
     gets the identical miss and spends miss budget, so the helper is no
     cheaper oracle than the link itself, and no key is ever stored on a
     tunnel's or a plaintext artifact's origin. Someone who has the link
     but not its password can tell from the helper that it is encrypted;
     the link is the secret that gives this away. A refused slug gets
     "Not found" in the small window; the intake page offers Open again
     after its 30-second wait. A valid visit grant still permits the
     helper during a one-time visit, as it permits the content. The
     app-origin intake stays syntax-only and reveals nothing about its slug.
  4. The helper says it is ready to its opener, on the app origin only.
     The intake page sends it the `CryptoKey` by `postMessage`, to the
     artifact's origin only, and only to that popup. The helper takes it
     only from its opener on the app origin, and only a non-extractable
     HKDF `CryptoKey`. It stores it in the origin's IndexedDB (database
     `redrop-e2e`) as the intake key, answers, and closes.
  5. The intake tab goes to the artifact's address with the rest. The
     viewer adopts the intake key for the artifact once it matches the key
     check (a wrong one is dropped), gives it to the service worker
     (`postMessage`), and only then creates the frame. A key stored on an
     earlier visit is used when no fresh one is waiting.

  The helper gives up after 30 seconds without a valid hand-off and
  explains that it works only when an encrypted link opens it. Because the
  helper answers only live encrypted artifacts, an intake link for a
  plaintext, unknown or tunnel slug stores nothing (fourth re-check n-1).
  Scripts on the artifact's origin can also overwrite that slot with their own key: the
  next viewer refuses it once and drops it. This is an accepted same-origin
  denial of service, like tampering with the viewer itself; reopening the
  complete link supplies the right key again.

  Why a click and a window, not a hidden frame: the app origin and the
  content domain are different sites by design (see "Origins"), and
  browsers partition a cross-site frame's storage by the top-level site. A
  key stored from a frame inside the intake page would never be found by
  the viewer, which is top-level on the artifact's origin; a window opened
  by the intake page is top-level, so it stores where the viewer reads.
  Browsers open windows only on a click. A prototype in Chromium confirmed
  both: the hidden frame's key wasn't found, the window's was.

  The stored key lets reloads, the viewer's own links and the artifact's
  `target=_top` links keep working for 12 hours after the full link was
  last opened in that browser; after that, or after the site's data is
  cleared, the full link is needed again. If the browser refuses
  IndexedDB (some private windows), the page isn't opened rather than
  keeping the key any other way. A copied address bar has no key, so
  forward the original link instead. Key text in the viewer's own
  address (`https://<slug>…/#k=…`) is never a link redrop gives out: the
  viewer treats it as a damaged link and stores nothing.
* **The navigation guard** (review 33 re-checks, B-2a and B-2a'). Since
  the intake moved, a full link is always a page on the app origin, so
  opening it in a tab where the artifact runs is never a fragment change
  of the artifact's window. The guard covers what is left of that route:
  any same-document change of the viewer's address, such as a fragment the
  visitor types after it, and the artifact rewriting the address. A script
  in the artifact can do that with one `history.replaceState` (also
  through a fresh realm's `History.prototype`), with the Navigation API, or
  by replacing the viewer's document with `document.open()`, which moves
  the address without a navigation. So the viewer doesn't rely on its
  address:
  * Before any frame exists, it registers this window's first `navigate`
    listener, a capturing one (third re-check M5). At the target, the DOM
    standard runs capturing listeners before the others, so an artifact's
    capturing listener added later still runs after it, in any engine
    (Chromium runs them in the order added). It captures every function
    and getter that listener and the viewer's own address updates use
    (`Reflect.apply`, `Event.prototype.preventDefault`,
    `NavigateEvent.prototype.destination`, `History.prototype.replaceState`,
    `URL` and the rest). The artifact can later replace them on the
    window, not the captured ones.
  * Once the frame exists, every same-document navigation of the page that
    the viewer didn't start is cancelled (`preventDefault`, so the URL
    never changes), hidden from later listeners
    (`stopImmediatePropagation`), and ends the artifact: the guard removes
    every frame, so none of its code runs in the window again (a removed
    frame's callbacks don't run). The page then says to open the link in a
    new tab.
  * Cancelling is the defence; removing the frames only stops the
    artifact's ordinary code. The HTML standard makes every push and
    replace navigation cancelable, the address bar's fragment changes
    included, and Chromium does so (checked by `scripts/e2e-browser.mjs`,
    with the tampering and `document.open()` cases). Residual: in a
    browser that didn't, the frames would still go before the URL changed,
    but code the artifact left outside them (a frame inside a shadow root
    of the viewer's page, a popup holding a reference, a timer it set on
    the viewer's window) could see the new URL, which since the intake
    moved can hold the key only if the visitor typed it there. Firefox
    and Safari were not tested.
  * The viewer's own address updates go through the guard, which knows
    each by the exact URL it goes to, not by a flag, and hides it from
    later listeners too. So a write the artifact makes while one is
    running, from a listener or any other event, is refused like any other
    (third re-check m-6). They never hold a key.
  * An artifact can still make the visitor's reopen do nothing, for
    example by calling `parent.stop()` in a loop: a denial of service
    only; the key doesn't arrive.
  * A browser without the Navigation API gets no guard, so the viewer
    opens nothing there and says why (fail closed).

  The viewer's address also keeps the query `redrop-e2e=1`, which the
  server drops before it builds the frame's address or a redirect. It
  predates the intake on the app origin and stays as defence in depth: an
  address the artifact leaves alone is never the content address a
  damaged link would reach by a fragment change.
* **A link whose `#` became `%23` or `?`** (`https://<slug>…/%23k=…` or
  `…/?k=…`, review 33 re-checks M3 and B-2b″) sends its key to the server
  in the request path or query, which no browser can avoid. The frame must
  not get it as well: its address would carry the path and query. On an
  end-to-end host, a top-level load gets a fixed "This link is damaged"
  page (400, `Cache-Control: no-store`) before any gate when its path
  holds a `#` after decoding, or when its query, without redrop's own
  parameters, is a key attempt (as above). No viewer, frame address or
  gate form is built from the request, and no view counts. A plaintext
  artifact's query strings are untouched. An encrypted artifact's page
  whose own query matches (a `?k=1`) can't be linked to directly. Redrop
  logs no content-host paths. A reverse proxy in front of it may: operators
  should not log content-domain request paths and queries, or should strip
  them. The viewer refuses such a path or query too. An encrypted bundle's
  file whose name holds a `#` can't be opened by a top-level link to its
  path; links
  inside the frame still reach it. On the app origin's intake page,
  a key-like query gets the same explanation that `#` became `?`; the
  script removes the query from the address bar before checking it, opens
  no helper and forwards nothing. The key has already reached the app
  server in that malformed request, so reverse-proxy query logging must
  also be disabled there.
* **Gates stay where they were.** The intake page and its helper run no
  gate and count no view: the key is stored before the visitor reaches the
  artifact, whose gate then works as for any link and counts the view
  once, on the viewer page. The password and Open pages carry one small
  script, pinned in their policy by its hash, that adds the page's
  fragment to the form's action, so the link's rest (a page, or the
  artifact's own fragment) and a plain link's own `#part` survive the gate.
  Without scripts that fragment is lost, but not the key, which is already
  stored.
* **The key never reaches the server.** It is not in any query string,
  path, header, cookie, API call or log. Fragments are never sent in
  requests or in `Referer`. The CLI sends only the key check below. Of an
  argument given as `ID|URL` it uses the part after `#` (`ID#k=…`
  included) only locally, and sends only what names the artifact: an ID
  (16 characters of `a-z` and `2-7`), a slug, or a link's slug. Anything
  else is refused with exit code 2 before any request, and the error
  doesn't repeat it: `ID%23k=…`, a key whose `#` was percent-encoded,
  never reaches an API path. The server's log cuts every path at a `#`
  and at an encoded one (`%23` or `%2523` in the decoded path, and so
  on), for the clients that send one anyway. The intake page makes no
  request with the key, and its `/e/<slug>` path is redacted in the log
  like a token. Tests record every request the CLI sends and every server
  log line during end-to-end shares, updates and views, and check the key
  appears nowhere.
* **Key check.** `HKDF-SHA256(key, salt = "", info = "redrop-e2e/1
  key-check" ‖ 0x00 ‖ artifact ID)`, 16 bytes, stored with the artifact
  and sent with every publication. The server refuses a version whose key
  check differs, so a wrong key can never publish a version the link can't
  open. The viewer compares it before decrypting, to tell "wrong key" from
  "damaged content". It reveals nothing about the key.
* **CLI key store.** `share --e2e` saves the key in `e2e-keys.json` next
  to the CLI config file (`REDROP_CONFIG`'s directory, or
  `<user config dir>/redrop/`), keyed by server URL and artifact ID.
  `update`, `watch`, `info` and `rotate` find it there; `update` and
  `watch` also take it from a full link given as `ID|URL`, or, to keep the
  key out of shell history and process lists, on stdin with `-` as
  `ID|URL` (`redrop update - report.html < link.txt`). The file is
  written like the config: a fresh 0600 file renamed into place, never
  through a symlink, and a store path that is a symlink is refused, for
  reading too. Writers hold the operating system's lock on
  `e2e-keys.json.lock` (`flock`, or `LockFileEx` on Windows), so
  concurrent shares don't lose keys; the system releases it when a writer
  dies, so there is no stale lock to take over. A lock file that is a
  symlink is refused on every system: Unix opens it with `O_NOFOLLOW`, and
  every system checks it with `Lstat` before and after the open, so the
  lock never creates or holds a file a link points to. `delete` removes
  the key.
  The key is saved before anything is uploaded, so a failed save never
  leaves a link without a stored key.

### Format (version 1)

Every stored object (each file, and the manifest) is encrypted on its own.
All integers are big-endian.

```text
object   = magic salt segment*
magic    = 0x52 0x44 0x45 0x01        ("RDE", format version 1)
salt     = 16 random bytes, fresh for every object
segment  = AES-256-GCM ciphertext of up to 65536 plaintext bytes, then its 16-byte tag
```

* **Object key:** `HKDF-SHA256(key, salt = salt, info = "redrop-e2e/1
  object")`, 32 bytes. A fresh salt per object gives every object its own
  key, so nonces never repeat under one key, however many versions `watch`
  publishes.
* **Segments:** the plaintext is cut into 65536-byte segments. The last
  one has 1 to 65536 bytes, or 0 bytes when the plaintext is empty (then
  it is the only one). Segment `i` (from 0) uses nonce `uint88(i) ‖ last`,
  where `last` is `0x01` for the final segment and `0x00` otherwise. A
  dropped, added or reordered segment fails its tag (the STREAM
  construction).
* **Authenticated data** (the same for every segment of an object):

  ```text
  magic ‖ salt ‖ type ‖ uint16(len(id)) ‖ id ‖ uint32(version) ‖ uint16(len(path)) ‖ path
  type = 0x01 manifest, 0x02 file; path is "" for the manifest
  ```

  So the server can't swap blobs between artifacts (different ID, and a
  different key), versions (different number) or paths (different path),
  or present a file as the manifest. Every case fails closed.
* **Size:** `20 + n + 16 × max(1, ⌈n / 65536⌉)` for `n` plaintext bytes.
* **Manifest:** UTF-8 JSON, encrypted as type `0x01` with an empty path:

  ```json
  {"format": 1, "kind": "bundle", "title": "Report", "entry": "index.html",
   "files": [{"path": "index.html", "object": "q3h…", "type": "text/html; charset=utf-8", "size": 1234}]}
  ```

  `object` names are 26 characters of `[a-z2-7]` (16 random bytes), unique
  in the version. `type` is chosen by the CLI with the server's rules
  (extension, then sniffing the plaintext), from the shared
  `internal/contenttype` table. `entry` is the start page, or `""` for a
  bundle without one.
* **Stored version:** a bundle with exactly one file named `manifest` and
  one file per object name. The server checks this layout and the magic of
  every file, refuses anything else, and stores the files as
  `application/octet-stream`. It can't check the rest: only a key holder
  can.
* **Implementations:** `internal/e2e` (Go: the CLI encrypts, tests
  decrypt) and `internal/content/static/e2e.js` (browser and Node:
  decrypt, manifest, path resolution). `internal/e2e/testdata/vectors.json`
  holds vectors produced by the Go encryptor with fixed randomness; a Go
  test keeps the file current, and the web test suite decrypts every
  vector with the JavaScript code and checks that tampered variants fail.

### Publishing

* **Share.** The CLI asks for an artifact ID first
  (`POST /api/v1/artifacts/reservations`), because the ID is part of the
  authenticated data. The server mints the ID and returns it with a token
  signed for that user, valid 24 hours. The CLI generates the key, saves
  it, encrypts version 1 for that ID, uploads, and creates the artifact
  with `e2e: {reservation, key_check, version: 1}`. The server creates
  the artifact under the reserved ID. A forged, expired or another user's
  token is refused (`400`). A reserved ID that is somehow taken is a
  conflict. Minting the ID on the server keeps IDs unprobeable: a
  client-chosen ID would need a "taken" answer that reveals other users'
  IDs.
* **Plaintext never goes to an end-to-end artifact** (review 33 B-1).
  `update` and `watch` read the artifact before uploading anything, with
  or without `--e2e`. For an end-to-end artifact they take its key from
  the full link given, else the key store, check it, say so on stderr, and
  publish encrypted. Without a key they exit 2, and nothing was uploaded.
  `watch` checks at startup. `--e2e` now only insists that the artifact is
  encrypted. In `pkg/client`, a plaintext `Update`, `UpdateWithOptions`
  or `UpdateDigest` reads the artifact after preparing the source and
  before uploading or applying settings, and stops with
  `ErrEncryptedArtifact`.
* **Uploads declare their target.** `POST /api/v1/uploads` takes
  `artifact_id` (the artifact a new version is for) and `e2e`. The server
  refuses, before any byte is sent, an upload declared for an end-to-end
  artifact without `e2e` (and the reverse). A publication must match its
  upload's declaration: an end-to-end artifact's versions come only from
  uploads declared encrypted for it, an upload declared for one artifact
  can't become another's version or a new artifact, and encrypted and
  plaintext can't be swapped. Undeclared uploads still publish plaintext
  versions of plaintext artifacts, so older CLIs keep working. The server
  can't stop a client from uploading plaintext as a *new* upload: that is
  ordinary sharing; it can only refuse to let it near an encrypted
  artifact. That includes an older CLI's `update` of an end-to-end
  artifact: it doesn't declare its upload, so it still sends the
  plaintext to the server before the publication is refused. Anyone who
  uses `--e2e` should upgrade the CLI on every machine they publish from.
* **Update and watch.** The CLI reads the artifact (`e2e`,
  `e2e_key_check`, `next_version`), checks its key against the key check,
  encrypts for `next_version`, uploads, and publishes with
  `e2e: {key_check, version}`. The server assigns that number or refuses
  with `409` and the problem title `version_conflict` (another version
  took it first), deleting the upload. The CLI then encrypts again for the
  new number, up to three attempts.
* **Refusals.** The API refuses a plaintext version on an end-to-end
  artifact and an end-to-end version on a plaintext one (`400`), a key
  check that differs from the artifact's (`400`), `spa` on an end-to-end
  artifact or version (`400`), and a bundle that breaks the layout above
  (`400`).
* **Rendering in the CLI.** Everything `Prepare` does still happens
  first, unchanged: the folder rules, secret filtering, zip conversion,
  and Markdown rendering with the same renderer and sanitizer. The CLI
  then encrypts each resulting file. A `.md` file is therefore a rendered
  document plus its original, as without `--e2e`; the server never sees
  either.
* **Storage (migration 9).** `artifacts.e2e` and
  `artifacts.e2e_key_check`, `versions.e2e`, and `uploads.artifact_id`
  and `uploads.e2e`, all defaulting to plaintext (and undeclared) for
  existing rows. The API reports `e2e` on artifacts and
  versions, and `e2e_key_check` and `next_version` on the owner's artifact.

### Viewing

A browser can't run decrypted HTML at the artifact's own origin, with the
artifact's normal response headers and working relative URLs, without
something that answers requests on that origin. The options:

* **`srcdoc`, `about:blank` with `document.write`, or a `blob:` frame:**
  the document inherits the viewer page's CSP, so the viewer's policy
  would have to allow any script; its base URL is not the artifact's, so
  relative links and assets break; a `data:` or sandboxed frame gets a
  null origin, losing `localStorage` and same-origin `fetch`. Rejected.
* **A service worker:** the frame loads real URLs on the artifact's
  origin, and the worker answers them with decrypted bytes and the same
  headers a normal artifact gets. Chosen, for single files and bundles
  alike.

**Decision: bundles are supported, through redrop's own service worker.**
The design stays within the rules of "Viewer frame":

* **It is redrop's script**, `/__redrop/e2e-sw.js` (the shared
  `e2e.js` followed by the worker), served only on the host of an
  end-to-end artifact, only after that artifact's gates, with
  `Cache-Control: no-cache`. Every other content host, and every other
  service worker script request, still gets `404`. Artifacts still can't
  register a worker of their own.
* **Its scope is `/__redrop/v/`**, the version path space, so it never
  controls the viewer page, never sees a top-level navigation, and can't
  answer the viewer's own requests. A top-level load of a version path
  still goes to the server, which redirects it to the viewer form.
* **It keeps nothing that would outlive revocation.** It has no cache:
  every request fetches ciphertext from the server, through the gates, and
  a revoked, expired or used-up link answers the worker with the identical
  miss, which it passes on. It keeps the key in memory only; after the
  browser stops it, it asks an open viewer page for the key again. It
  accepts keys only from top-level windows of its origin. Nothing is
  injected into the artifact's bytes or DOM.
* **What it does:** for a request from the frame for
  `/__redrop/v/<N>/<path>`, it fetches and decrypts version N's manifest
  (cached in memory per N: numbers are never reused), resolves the path
  like the server resolves a bundle (exact file, `dir/` to
  `dir/index.html`, `dir` redirected to `dir/`, the entry for the root),
  fetches `/__redrop/v/<N>/<object>`, decrypts it with the path in the
  authenticated data, and answers with the manifest's content type and the
  security headers of the ciphertext response (`Content-Security-Policy:
  frame-ancestors …`, `Referrer-Policy`, `X-Content-Type-Options`,
  `Permissions-Policy`, `Cross-Origin-Resource-Policy`, `X-Robots-Tag`,
  `Cache-Control`). A decrypted page therefore has the same origin,
  isolation and CSP as a normal artifact. A root-absolute subresource
  (`/app.css`) from a frame inside version N resolves in version N. Other
  origins pass through untouched.
* **Fails closed.** Objects up to 64 MiB are decrypted completely before
  any byte is released; a damaged or swapped object answers an error page
  and nothing of it reaches the page. Larger objects (video) stream, one
  authenticated segment at a time, so memory stays bounded; a damaged
  stream stops with a network error after its last good segment. Released
  bytes are always authenticated; truncation is detected at the end.
* `?download` on a file answers it as an attachment with its file name.

**The viewer page.** Every top-level page load of an end-to-end artifact,
at any path, gets redrop's viewer page, after the usual gates and view
counting. It differs from the plain viewer:

* It always has the frame, even with the info button off (then without
  button and panel) or with `?redrop-info=0`: there is no direct view of
  ciphertext.
* It carries the artifact ID, the shown and current version numbers, and
  the key check as data attributes. The artifact ID is needed for the
  authenticated data; an ID grants nothing without the owner's token.
  This is the one exception to "the panel never shows IDs".
* Its script (`viewer.js`, with `e2e.js`) adopts the intake `CryptoKey`
  as described in "Where the key goes in the browser", or reads a key
  stored on an earlier visit. It checks the key and decrypts the
  manifest (which checks the content before anything is framed, and gives
  the tab title), registers the worker, gives it the key, and only then
  points the frame at `/__redrop/v/<N>/<page>`. The
  CSP adds `e2e.js` to `script-src`, `worker-src` for the worker script
  and `connect-src` for `/__redrop/v/`, nothing else.
* **A fragment the visitor changes** (an edited address, a pasted full
  link) reloads the viewer while no frame exists yet, for example while it
  shows "the key is missing", and the reloaded page reads it. Once the
  frame exists, the navigation guard refuses it and asks for a new tab.
  The viewer's own address bar updates use `history.replaceState`, which
  fires no `hashchange`, so only the visitor's changes do.
* **Address bar, paths kept private.** The address bar shows the frame's
  page in the fragment, not the path:
  `https://<slug>…/?redrop-e2e=1#/docs/a.html#part` (plus
  `&redrop-version=N` for an older version). A reload sends the server
  only `/` and the marker. The page inside the frame keeps its own
  fragment, and `#hash` navigation, Back/Forward and `pushState` inside
  the artifact work as for plain artifacts. Feedback still reports the page as
  `/docs/a.html#part` (see below).
* Errors (no key, a damaged key, a key in the path, wrong key, damaged
  content, an insecure context, a browser without service workers or the
  Navigation API, the guard's refusal) are shown as text on the viewer
  page; the frame stays empty.
* **Needs HTTPS and the Navigation API.** Browsers expose WebCrypto and
  service workers only to secure contexts (HTTPS, or `localhost`). On a
  plain-HTTP content domain the viewer explains that it can't decrypt. The
  navigation guard needs the Navigation API (current Chrome, Edge, Firefox
  and Safari have it); without it the viewer opens nothing.
* **Files.** A `file` artifact opens in the same frame. An image is shown
  on a small redrop page in an `<img>`, as the plain file page shows it, so
  an SVG's script can't run (review 33 m-A); the `<img>` request gets the
  file. Other files are served inline when the browser can show them
  (audio, video, PDF, text as plain text) and as a download otherwise. The
  panel has a Download button that decrypts in the viewer and saves under
  the original name.
* **Requests without Fetch Metadata** (curl, scripts) and frames loading a
  path outside the version space get a redirect to
  `/__redrop/v/<current>/<path>`; ciphertext objects are at
  `/__redrop/v/<N>/manifest` and `/__redrop/v/<N>/<object>`, as
  `application/octet-stream` attachments. A script with the key can
  decrypt them; `scripts/e2e-decrypt.mjs` does, with Node's WebCrypto.

### What still works

* **Gates and lifecycle:** passwords, expiry, view limits and `--once`,
  revoke and restore, rotate, delete, rollback and older versions. They
  are all enforced by the server on the viewer page and on every
  ciphertext and worker request, exactly as for plain artifacts, and the
  intake runs before them (see "Gates stay where they were"). Views count
  once per visit, on the viewer page; the intake page and its helper count
  none.
* **Rotate** gives a new slug; the key stays. The CLI prints the new link
  with the key when the key store has it.
* **Feedback**, with a caveat the viewer can't see: comments are
  **plaintext**. The body, the name, the page path and any quoted text
  (which comes from the decrypted page) are stored on the server in the
  clear. Owners who need the content kept private should leave feedback
  off.

### What stops working

* **Live reload** is not supported for encrypted artifacts. `watch` still
  publishes encrypted versions, but its live lease is refused explicitly;
  the viewer's worker uses pinned version paths, not the plaintext frame's
  reload flow.
* **SPA fallback** (refused) and path-based routing in general: inside the
  frame, `location.pathname` is `/__redrop/v/<N>/…`. Hash routers work.
* **Directory listings:** a bundle without a start page answers `404` at
  its root. Name the start page with `--entry` or add an `index.html`.
* **Server-side content handling:** `<title>` extraction (the dashboard
  shows the ID unless `--title` is given; viewers see the decrypted
  title), content-type sniffing on the server (the CLI does it before
  encrypting), and the artifact's own `favicon.ico` (visitors get
  redrop's).
* **Root-absolute links** (`<a href="/b.html">`) inside the frame are
  navigations outside the worker's scope: the server redirects them to the
  current version's path, so the server sees that path. Relative links
  never reach the server.
* **Dashboard:** an `e2e` badge, and no content preview, file names or
  working "open" link: the server has no key. The dashboard says so.
* **Size limits** count ciphertext, slightly larger than the plaintext;
  the manifest doesn't count against `REDROP_MAX_FILES`.
* **Seeking in large media.** The worker answers whole files and ignores
  `Range`: a video over 64 MiB plays as it streams, but can't seek ahead
  of what has arrived, unlike a plain file artifact.

### Rollback

Migration 9 adds five columns; an older server ignores them. Rolling back
the binary alone serves end-to-end artifacts as plain bundles: a file
listing of opaque names and ciphertext downloads. Nothing leaks, but the
links stop working until the new binary returns. Delete or keep such
artifacts deliberately before a rollback. The worker registered in
visitors' browsers keeps only the code; without the server's ciphertext
it serves nothing.

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

### Archive work limits

Duplicate normalized file paths are errors, not skipped bodies. A file path
that is also the parent directory of another file is an error too. Directory
entries carry no content: as before, their names are neither validated nor
kept in memory, and repeated directories (`tar -C dist . -C static .` with a
shared `css/`) are accepted. Only regular-file paths are retained, at most
`REDROP_MAX_FILES` of them.

A separate entry cap is `max(128, 4 * REDROP_MAX_FILES)` non-metadata
entries: every regular file, directory, link or special entry, including those
archive/tar would return. PAX, global and GNU long-name headers are not
counted as entries, so PAX-format archives get the same cap as others, but
more than 16 of them in a row are refused.

The decompressed stream has one byte budget covering content, discarded bodies,
headers, padding and metadata. Its limit is the per-version content limit plus
an overhead allowance of 4 KiB per permitted entry and 1 MiB of slack.
A raw-block counting reader runs before the tar parser so metadata hidden by
Go's tar reader is counted too. It re-implements only tar framing, so after
every header the tar reader returns, extraction checks that both agree on where
that entry's body starts and ends (`inSync`): a regular file must have exactly
its padded size left, a directory nothing, and a global header only padding.
Any disagreement, such as a name precedence or prefix the counter does not
model, rejects the archive as malformed before the next header is read. A
differential test and fuzz target check the counter against archive/tar.
PAX size overrides and sparse-file extensions are rejected because they can
change physical framing; normal PAX path and GNU long-name metadata remain
supported. Paths, checksums, entry types and tar semantics still pass through
the existing archive validator. Both compressed input and decompressed output
check the publishing request context on every read. Checksum verification and
blob copies check cancellation too. Publication's single admission slot bounds
active extraction; waiting requests can cancel.

A publication rejected for its content, settings, a limit or a quota deletes
its upload once cleanup has succeeded, so a replay does no archive work and the
upload no longer holds a session. A read error inside an entry's body, such as
a truncated or corrupt compressed stream, is a corrupt archive like a bad
header, not an internal error. These limits bound per-attempt work, not a
per-account request rate.

### Disk budgets and upload expiry

SQLite reserves each upload's declared bytes atomically with committed version
usage against both the owner's quota and `REDROP_MAX_TOTAL_SIZE`. Open sessions
also have a per-user cap, `REDROP_MAX_OPEN_UPLOADS`, default 8 and a positive
integer. Zero-byte sessions count. Expired sessions retain their reservation
until maintenance removes their disk bytes, then releases the row.

Uploads expire after one idle hour. Each completed nonempty chunk renews the
hour, so a slow upload can resume across many chunks without a 24-hour abandoned
session. The minute maintenance sweep reclaims expired sessions. Reading status,
empty chunks and failed publication attempts do not extend expiry. An active
chunk keeps its file and reservation until that request finishes; maintenance
never unlinks a file a slow writer still holds open. Simultaneous chunks or
publication for one upload return a conflict instead of modifying the same
file concurrently. Expiry is rechecked after claiming an idle session, since a
chunk may have just renewed it. Admin stats
show `staged_bytes`, the successfully received upload bytes, and
`reserved_bytes`, pending declared bytes plus unpacked publication reservations,
separately from committed logical bytes and deduplicated blob storage.

Before writing any materialized file, publication reserves its declared
unpacked size in the same quota transaction. The version limit applies to the
sum of unpacked file sizes. The staged upload and its unpacked content are the
same content to the owner, so the user quota counts each open upload once, as
the larger of its staged and unpacked bytes: a user with 6 MB of headroom can
publish a 6 MB file. `REDROP_MAX_TOTAL_SIZE` guards the disk and counts both,
so the server total needs temporary headroom for the staged and unpacked
copies. The commit converts the unpacked reservation to committed logical usage
atomically; staging is released only after its file has been removed.

Publication and blob garbage collection share one cancellable service admission
slot. Each account has at most one publication running or waiting for it; a
second one gets an immediate `409` with the problem title
`publication_in_progress`, and the CLI retries the same upload for up to five
minutes. Another account's publication therefore waits for at most one attempt
per other active account. Each attempt tracks only the hashes it newly
installed. On extraction, entry-validation or database failure, it looks up
only those hashes and removes the ones no file record references, before
releasing its unpacked reservation. Existing blobs are never deleted by
rollback, including deduplicated content shared with other artifacts. The slot
covers commit and rollback, so a concurrent publisher cannot be about to
reference a hash removed by another attempt. Failed cleanup keeps its
reservation and logs an operational error rather than releasing unreclaimed
capacity. Replays cannot clear a failed-cleanup reservation; they return a
conflict until expiry safely reclaims the bytes.

After a successful cleanup, a publication rejected for a deterministic client
error (invalid content, archive, entry or settings, a limit or quota, or an
unknown artifact) also deletes its upload, releasing its reservation and
session. The CLI never resumes a rejected upload. Cancellation, conflicts (an
incomplete or busy upload) and internal errors keep the upload until its idle
expiry. This design favors a small, bounded materialization path over parallel
extraction. Uploaded chunks remain independently resumable.

Expired uploads without unpacked bytes are released without blob GC. Only an
expired upload with unpacked bytes, left by a crash or a failed rollback, waits
for a full GC that removes unreferenced blobs. If that GC fails, it logs the
error and keeps those uploads, while the others are still released. GC keeps
walking past a blob it cannot remove and reports the failures together.

Migration 6 adds `uploads.materialized`, initialized to zero for existing
sessions. Open uploads from before the upgrade count against quotas from then
on: one that no longer fits its owner's quota is refused when published, and
deleted like any other quota rejection. Stop the old process before upgrading. Do not run multiple server
processes against the same data directory. Restore a whole-data-directory backup
for rollback; older binaries do not enforce pending-byte reservations.

## Markdown documents

Markdown is rendered **in the CLI's publishing client, before upload**, not by
HTTP handlers or content serving. The CLI is also the agents' interface. Both
`Share` and `Update` use `pkg/client.Prepare`, which calls the client-only
`internal/markdown` renderer and creates a normal bundle. No wire fields,
server dependencies, migrations, or artifact-origin changes are required. Stored
bytes are served unchanged. A version pins the rendered result, so rollback does
not depend on the renderer version installed on the server.

This is preferable to server-side publish-time rendering because it keeps parsing
and highlighting work off the shared server and preserves its existing upload,
quota, and isolation boundaries. The trade-off is that direct API uploads are
not automatically rendered and older CLIs keep their previous behavior. Rendering
is deterministic for a given source, bundle layout, and CLI build. There are no
network lookups or timestamps in the output.

### Publishing contract

- `--render auto`, the default, renders case-insensitive `.md` file names when
  no explicit `--as` is set, including stdin with `--name notes.md`.
- `--render markdown` forces rendering for other extensions. The resulting kind
  is `bundle`, its entry is `index.html`, and the original source remains at its
  original name with a download link. An explicit `--as file` or `--as html`
  conflicts with forced rendering. `--render none` keeps raw publishing.
- Directories require `--render markdown --entry PATH` to select the source.
  Assets and the original source keep their bundle paths; generated relative
  links/images are rebased from the source directory to the root entry. Single
  files never upload siblings. Rendering does not modify the input directory and
  refuses an existing root `index.html`. Ordinary and Markdown bundles share one
  `fs.FS` packer, with the symlink, skip and regular-file rules in
  [Directory uploads](#directory-uploads). `--entry` is opened through the
  same `os.OpenRoot`. An `--entry` with a path component the skip rule
  excludes is a usage error, since the watch digest and event filter could
  never see it. Archive rendering is unsupported.
- Sources are limited to 8 MiB. Unknown code languages and blocks over 64 KiB stay
  escaped plain text rather than invoking a lexer. Rendering does not support
  `--spa`. Invalid render options return a typed client usage error, mapped to
  CLI exit code 2. Missing top-level sources and missing entries follow the same
  rule, with missing-path messages distinct from symlink errors. OS-reported
  symlink loops are usage errors too. `client.RenderMode` and `ParseRenderMode`
  define the modes once.
- Single-file rendering reports local references that are not uploaded. The CLI
  warns on stderr and suggests directory publishing; it never uploads siblings
  implicitly. Rejected image sources are reported for single-file and directory
  rendering, with one warning per distinct source naming the source and the rule
  that images must be files inside the bundle. Empty image destinations receive
  an explicit empty-source warning. Library users may inspect `Prepared.Warnings` or set
  `Source.Warn`.
  Updating a raw Markdown artifact with auto-rendering creates a rendered bundle
  version; `--render none` keeps the new version raw.

### Rendering and safety

Goldmark with GFM handles tables, task lists, strikethrough, autolinks, fenced
code, and ordinary Markdown. Chroma supplies maintained, offline syntax lexers;
there are no CDN scripts, web fonts, or runtime highlighting dependencies.
`golang.org/x/net/html` parses the output for an explicit element/attribute
allowlist, rather than relying on regex-based HTML filtering. These dependencies
are pinned in the Go module graph and belong to the client build, not the server.

Raw HTML is never enabled. Custom Goldmark renderers omit inline and block HTML
comments, preserving visible text after a comment closes on the same line.
Comment-only blocks emit no empty paragraph. Other inline HTML remains escaped
inline; other HTML blocks become escaped text in normal paragraphs, not code
blocks with Copy buttons. This
preserves technical prose such as `List<String>` and tags without executing them. The parsed-output allowlist strips active
elements, event handlers, arbitrary CSS, and unsafe URLs after highlighting.
Images are local-only. Relative URLs cannot escape the bundle root and cannot
contain controls or backslashes. `data:`, `javascript:`, and other active schemes
are rejected. HTTP/HTTPS links with a host and nonempty `mailto:` links are allowed
only on anchors, with `rel="noopener noreferrer nofollow"` and `target="_blank"`.
This preserves deliberate navigation without allowing automatic external resource
requests or exposing the document URL via a referrer or opener. Rejected links
become plain spans, not dead link-styled anchors. The converter knows nothing
about server-reserved paths; upload validation alone uses `api.ReservedPrefix`.
The trusted template escapes titles, filenames, TOC labels, and download URLs
separately. Heading IDs use a document-specific prefix
and collision tracking, including collisions with suffixes and Unicode headings.
Duplicate headings retain the next suffix counter rather than rescanning earlier
IDs. Conventional authored fragment links such as `#setup` are translated to the
matching prefixed ID; explicit generated permalinks remain valid.

The generated page adds a CSP with `default-src 'none'`, local images, inline
styles, and only the hash-pinned document-interaction script. It blocks automatic
external resource requests, connections, forms, and base URL changes, while
allowing explicit link navigation. This is defense in depth for the document,
not a replacement for per-artifact origin isolation. The XSS corpus uses an
independent URL oracle rather than the converter's validation helpers.

Conversion produces a plain document with no hosting-specific conventions or
branding. There is no favicon, robots meta tag, or referrer meta tag. Downloads
use ordinary relative links and the HTML `download` attribute, without a special
query. Hosting policy remains in the content server's response headers and own
pages; the standalone document does not depend on them. Its CSP is intrinsic
resource-safety policy and works on other static hosts too. Original downloads
and other bundle files are deliberately not sanitized and remain untrusted.
No server-side HTML rewriting is introduced.

The page follows the existing document template's warm neutral colors, red
accent, system fonts, rounded code/table surfaces, sticky desktop TOC, and mobile
TOC disclosure. An `IntersectionObserver` marks the current section in the desktop
TOC without polling. Four or more headings trigger the TOC. Every heading gets a
permalink; code blocks get copy buttons with clipboard fallback and honest failure
feedback. Light/dark modes use `prefers-color-scheme`; tables and code scroll
inside their containers on narrow screens. The document is readable without JS.

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
* Linking keeps the current session and its sign-in method. Only a normal
  single sign-on login creates an SSO session, so linking cannot satisfy
  `REDROP_ADMIN_REQUIRE_OIDC`.
* While `REDROP_ADMIN_REQUIRE_OIDC=true`, an admin account can link another
  identity only from an SSO session; the start and the callback both check
  it. A new identity linked from a password session would otherwise open
  admin sessions without the identity provider's checks ever applying to
  the account. Unlinking follows the same rule: from a password session,
  someone with only the admin's password could otherwise remove the
  account's single sign-on, which that session could not link back. An
  admin who has only a password gets SSO by accepting an admin invite
  (`redrop-server invite --admin`) through single sign-on, or by linking
  before the setting is turned on.
* **After upgrading to this rule, review admin identities.** Before it, a
  password session could link any IdP account to an admin and turn the
  session into an SSO one. A link made that way survives the upgrade and
  still opens admin SSO sessions, and nothing records how a link was made
  (the `identity linked` log line starts with this version). After
  deploying, check the linked identities of every admin account and remove
  the ones nobody recognises: each admin on their Account page, or the
  operator with a read-only query on a copy or snapshot of the database:
  `SELECT u.username, i.provider, i.email, datetime(i.created_at, 'unixepoch') FROM identities i JOIN users u ON u.id = i.user_id WHERE u.role = 'admin';`

### Logging

The app origin's requests are logged once, by `httpapi.logRequests`: API
calls and failures, with method, status, size and duration, never query
strings. A path segment that follows one of `dashpaths.SecretPrefixes`
(invite, reset and setup tokens) is written as `…`, and anything from a
`#` or `?` on, or from an encoded one (`%23` or `%3F` in the decoded
path, from a client that sent `%2523`, or deeper), as `#…`: a fragment or
query a client sent encoded, such as an end-to-end link's key. Fuego's own access log
is off, and Fuego's other messages go to redrop's handler at `Info` or
above only: its debug output includes decoded request bodies, with
passwords and tokens. A test captures every log line, at debug level,
while it opens and redeems each kind of one-time link, and checks that no
token or password appears.

### Rate limits

* **Bounded tables.** Every fixed-window limiter (`security.Limiter`) tracks
  at most 50,000 keys and stores keys over 64 bytes as their SHA-256, so a
  full table costs about 10 MiB. Clients already in a table keep their own
  count. What a full table does with a new key depends on whether anything
  else bounds what it counts:
  * **Evict the oldest entry** (`security.EvictOldest`) in the per-network
    tables: sign-in and single sign-on starts, one-time link lookups, and
    artifact passwords per network. The account backoff, the per-artifact
    budget and the 256-bit link tokens bound guessing on their own, so a
    newcomer is never refused, and an attacker who floods the table to push
    others out gains nothing that rotating networks did not already give.
  * **Refuse the new key** (`security.RefuseNew`: `429` until a window
    expires) where the table is the only bound: the authenticator-code
    ceiling (per account) and the artifact password budget (per artifact).
    Their keys exist only for an account whose password was right, or for
    a link the client knows, so an attacker cannot fill them with junk.
  * The content miss limiter (`security.TokenBucket`, at most 100,000
    clients) drops clients whose budget has refilled and refuses new ones
    while every bucket is still refilling. It is the only bound on guessing
    links, and a refused newcomer loses only misses: hits are still served.
* **Retry-After.** Every limiter `429`, on the API and on the artifact
  password gate, carries `Retry-After`: the end of the refused key's window
  (or of the oldest window, for a full table that refuses), or the account
  backoff's wait.
* **Clients.** Every per-client limiter (sign-in, single sign-on starts,
  one-time link lookups, artifact passwords, content misses) keys on
  `netutil.Network`: the trusted-proxy-resolved address, with IPv6 grouped
  by /64, so one host cannot rotate through its /64 to reset them.
* **Trusted proxies.** Forwarded headers count only on requests whose
  socket peer is in `REDROP_TRUSTED_PROXIES`. Without a hop count, the
  client is the first `X-Forwarded-For` entry from the right that is not a
  trusted address. A trusted range that covers a whole CDN makes that
  spoofable: a CDN worker sends requests from inside the range with any
  `X-Forwarded-For`, and the walk skips both its hop and the edge's. The
  server logs a startup warning in this mode for a trusted range wider than
  a /24 (IPv4) or a /48 (IPv6) with `X-Forwarded-For`. The account backoff
  below does not depend on client addresses, so it holds even when they
  are spoofed.
* **Trusted hops.** `REDROP_TRUSTED_HOPS=N` reads the entry exactly N from
  the right instead (`netutil.ClientResolver.fromHops`). It is the only
  safe setting when the origin stays reachable without the CDN, so
  `CF-Connecting-IP` can't be trusted. The production chain is client,
  Cloudflare, Traefik, redrop, with `N=2`:
  * **What the proxies do.** Cloudflare's documentation: with no
    `X-Forwarded-For`, it sets one with the connecting address (equal to
    `CF-Connecting-IP`); with one, it appends the connecting address.
    Traefik v3 (`forwardedheaders` middleware and its proxy, read in the
    source): from a source in `forwardedHeaders.trustedIPs` it keeps the
    header and appends the source address; from any other source it deletes
    every `X-Forwarded-*` header, then appends the source address, so the
    header holds that one address. `notAppendXForwardedFor: true` would
    stop the append and change the count.
  * **Visitor through Cloudflare:** `[anything the visitor sent], visitor,
    edge`. Position 2 is the visitor, written by Cloudflare.
  * **Cloudflare Worker:** `[anything the Worker set], Worker egress,
    edge`. Position 2 is written by Cloudflare. For cross-zone Worker
    subrequests Cloudflare documents `CF-Connecting-IP: 2a06:98c0:3600::103`;
    that the appended entry is the same address is the review's reading of
    the documentation and was not exercised against Cloudflare. Either way
    the entry is Cloudflare's, not the Worker's, so all Workers share one
    bucket.
  * **Direct to Traefik, skipping Cloudflare:** the source is untrusted, so
    Traefik replaces the header with the source address. One entry, fewer
    than N: the leftmost (and only) entry is the real peer.
  * **Fallbacks.** No header, or an entry that is not an IP address at the
    chosen position, gives the socket peer (Traefik): such requests share
    Traefik's bucket rather than choosing their own.
  * **Requirements.** N must match the chain exactly. Too large, and a
    visitor's own entry is read (spoofable); too small, and the edge or
    proxy address is read (everyone behind it shares a bucket). Traefik
    must trust Cloudflare's ranges and keep appending, and Cloudflare's
    "Remove visitor IP headers" transform must stay off. Only the socket
    peer is checked against `REDROP_TRUSTED_PROXIES`. The setting needs
    `REDROP_CLIENT_IP_HEADER=X-Forwarded-For`, accepts 0 to 10, and turns
    the wide-range warning off.
* **Accounts.** Sign-in attempts are also slowed per account, from any
  network: five in a row, then each attempt waits twice as long as the one
  before, from 1 second up to 15 minutes (`429` with `Retry-After`). Once
  the attempts stop, the right password works again within 15 minutes. But
  while they continue, the account's password sign-in stays refused: about
  four attempts an hour, each timed to the end of the last wait, keep the
  account at the 15-minute maximum, and the owner's right password is
  refused without being checked. Single sign-on and API tokens are not
  affected. A successful sign-in, or a day without attempts, starts over.
  A possible follow-up is the usual device-cookie scheme: a browser that
  signed in successfully gets a signed cookie, and its attempts skip the
  shared backoff and are limited only by their own count, so the owner
  keeps signing in while guesses from elsewhere stay slowed. Unknown usernames back off exactly like real ones, so the
  backoff reveals nothing about which accounts exist. Each attempt is
  charged before the password is checked, so concurrent guesses queue
  instead of all reading the old count. The table is a fixed 65,536 slots
  hashed with a per-process random seed: it is never full, and an attacker
  cannot choose which accounts share a slot.
* **Authenticator codes.** An account takes at most 5 wrong codes per
  hour, at sign-in and when turning the authenticator off, from any
  network. Each code works once: the last accepted 30-second step is
  stored per user, and a code from that step or an earlier one is refused
  ("already used"), including when two requests race with one code. A new
  secret starts its own record.
* **Sign-in input.** A username that fails the account name rule (2–32
  characters) gets the wrong-login answer before any limiter or database
  call. The unauthenticated sign-in and onboarding routes (`/auth/login`,
  `/setup`, `/invites/{token}/accept`, `/reset/{token}`) accept at most
  4 KiB of body; larger bodies get `413`.
* **Request bodies.** Every route that takes a JSON body checks it before
  decoding: another `Content-Type` (or a body without one) gets `415`, and
  a body over its limit (4 KiB above, 1 MiB elsewhere) gets `413`, with or
  without a declared length. Malformed JSON gets `400`. Upload chunks
  (`PUT /uploads/{id}`) are raw bytes and keep their own limits.

## CLI

`redrop` is a single static Go binary for Linux, macOS, and Windows (amd64 and
arm64). The server embeds all builds and serves `install.sh`/`install.ps1`, so
`curl -fsSL https://<app>/install.sh | sh` installs a version matching the
server and verifies SHA-256 checksums. Configuration is
`redrop login --url … --token …` or the `REDROP_URL`/`REDROP_TOKEN`
environment variables. `login` and `logout` replace the config file atomically
through a fresh private (0600) temporary file in the same directory, never
writing through a symlink. A config path that is itself a symlink is refused
before anything changes ("pass `--config` with the real file"), so a linked
target never keeps an old token while the command reports success. Setting
`REDROP_TOKEN` for the installer (`curl … | REDROP_TOKEN=… sh`) also logs in;
the dashboard shows that one-line command when a token is created. Every command
supports `--json`; exit codes are stable (see `redrop guide`). The Go package `pkg/client` is the same client as a library.

### Directory uploads

Every directory bundle (ordinary or Markdown) is packed by one walk,
`bundleWalk` in `pkg/client/bundle.go`. The watch content digest
(`hashTree`) and the watch directory registration use the same walk, and the
watch event filter uses its skip predicate, so they always agree on which
files count.

Symlinks:

- The walk runs inside an `os.OpenRoot` of the selected directory. Relative
  file and directory symlinks are followed only within it. Absolute,
  outside-root, and cyclic links are usage errors (exit 2). Directory depth is
  bounded at 64.
- Broken asset symlinks and non-regular entries are skipped with warnings.
- A `PATH` that is itself a symlink is resolved once with
  `filepath.EvalSymlinks` before the walk, which is then confined to the
  resolved folder. A link to a file uploads the target's bytes under the
  link's name. `watch` resolves it at startup and watches the real path, so
  retargeting the link later does not change what is watched or published.
- An explicitly named single-file source follows normal filesystem symlinks,
  even into another directory, since it is not an implicit directory walk. It
  and `--entry` must resolve to regular files, checked before opening and
  again before reading. Unix source and entry opens use `O_NONBLOCK` so a FIFO
  swapped in between the checks cannot block the open.

When `share`, `update`, or `watch` packs a directory, it skips:

- Directories named `.git`, `.svn`, `.hg`, `node_modules`, `__pycache__`, or
  `.DS_Store`, including their contents.
- Files named `.git` (a worktree or submodule link, `gitdir: /abs/path`, which
  would publish a local path), `.DS_Store`, or `Thumbs.db`.
- Editor backups `*~`, Emacs locks `.#*`, and Emacs autosaves `#...#` only
  when the name is longer than two characters.
- Hidden Vim swaps: dot-prefixed names ending in `.swa` to `.swp`, or `.swx`,
  such as `.page.swp` and `.page.swo`.
- JetBrains suffixes `*___jb_tmp___` and `*___jb_old___`, and gedit
  `.goutputstream-*` files.
- Conventional secret files `.env` and `.env.*`, except `.env.example`,
  `.env.sample`, and `.env.template`; `.netrc`, `.npmrc`, `.pypirc`,
  `.git-credentials`, and `.pgpass`; private-key names `id_rsa`, `id_dsa`,
  `id_ecdsa`, and `id_ed25519`, plus `*.key`, `*.p12`, and `*.pfx`.
  The corresponding `.pub` files remain included.
- Secret directories `.ssh`, `.aws`, and `.gnupg`, including their contents;
  `.docker/config.json` and `.kube/config` files, without excluding other
  files under `.docker` or `.kube`. These rules apply at any depth.

`--include-secrets` on `share`, `update`, and `watch` opts conventional secret
paths back in; library callers set `Source.IncludeSecrets`. The editor, VCS,
and dependency filters remain active. A Markdown `--entry` that matches a
secret rule is a usage error unless secrets are included; opting in does not
allow an editor-filtered entry. Packing, hashing, directory registration, and
the event filter all use the same source option.

Every excluded secret entry emits a stderr warning containing only its relative
path, never its contents, and saying that its name is excluded by the secret
rule: a differently named symlink to the same file is not a secret name. An
excluded secret directory is named once per walk, without reading or listing
its contents. Watch reports each secret warning once per path per run, whether
it comes from registration, an event or a publish; a secret path created later
warns once too, and repeated hash scans stay quiet. An explicitly selected
single file still publishes as requested, and warns if its selected path or
name matches a secret rule, even with `--include-secrets` or a different
`--name`. Its source bytes are unchanged. A selected folder that is a secret
directory, or is inside one, by its absolute or resolved path (`share ~/.aws`,
`share .` inside `.ssh`), is a usage error unless `--include-secrets` is given,
because the walk filters names only below the selected root. Warnings stay on
stderr even with `--json`.

Use a dedicated output directory containing only files intended for recipients.
These are name-based exclusions, not secret-content detection. They do not
remove secrets from already published versions.

File-name filters are case-insensitive; directory names match exactly. The
walk applies the rule before it follows an entry: a listed directory's own
type picks the directory or file rule, and a symlink is skipped without being
followed when either rule matches its name. An Emacs lock (`.#x`, a dangling
symlink) is therefore skipped and counted rather than warned about, a
`node_modules` link out of the folder does not fail the upload, and a skipped
temp file that vanishes mid-walk is not a source change. One function,
`skipEntry`, makes this decision from a root-relative path and listed type for
the walk, the watch event filter, and the Markdown `--entry` check. The event
filter and entry check apply it to every component; the entry check obtains
types through the folder's `os.Root`. For an event on a name
that is already gone, the filter uses the file rule unless the name was a
registered directory, so editor temp-file churn neither rehashes the folder
nor resets the watch's retry budget.
Names `4913`, `#`, `##`, non-hidden `x.swp`, and hidden files such as
`.logo.svg`, `.schema.sql`, `.subs.srt`, and `.key.sig` remain valid. Filtering applies
only to entries inside directories. An explicitly named single file is always
uploaded, even when its name matches a filter. `share` and `update` print one
stderr line with the number of excluded file entries encountered, when
nonzero. They do not count files inside excluded directory trees, and print
no skipped-file line when the count is zero. This notice never changes their
stdout JSON. A folder with no files to upload fails locally (exit code 2)
instead of uploading an empty bundle.

`PATH` must be a regular file or a folder; pipes and devices (such as
`<(cmd)`) are rejected rather than uploaded as zero bytes. Use `-` with
`--name` for streamed input.

A single file that is uploaded as is (`html`, `file`, or a `.tar.gz` bundle) is
read from its own path, without a temporary copy. Its SHA-256 is taken before
the upload and checked against the bytes sent. When a failed chunk resumes
below what was already sent, the hash is rebuilt by re-reading the file up to
the resume offset. That catches an edit in the re-sent range, and reports an
edit before the offset as a change, conservatively. An edit that is reverted
before the re-read is not seen by the client; the server's checksum still
rejects any blob that differs from the declared hash. Each failed chunk costs
one local re-read of that prefix. A
mismatch means the file changed during the upload: `share` and `update` start
over once and then fail, and `watch` waits for the next settled change. A
retried upload abandons the first upload session; the server expires idle
sessions after 1h and reclaims their staging bytes. Directories, single files forced to
a bundle, and `.zip` input are packed into a temporary tar.gz first; stdin is
spooled to a temporary file.

Archive entries supplied with `--as bundle` are not editor- or secret-filtered. ZIP input
is converted to tar.gz using the existing archive conventions: strip a common
top-level folder and omit `._*` files, `__MACOSX/` entries, and non-regular
entries. tar.gz input is passed through. The server still validates archive
paths and entry types.

### Watching an artifact

`redrop watch ID|URL PATH` reuses the update path, including the CLI's source
construction, so `--render`, `--entry` and warnings behave as on `update` (the
skipped-file count is not printed per publish). It debounces edits for 500ms by
default, and publishes only changed content. Before announcing `watching`, it
prepares the source once without uploading, so a source every publish would
reject (a bad `--entry`, a folder with `--as html`) exits 2 with `share`'s
message instead of at the first change. A tree that changes during that read,
or an empty folder, is left to the watcher. Startup never publishes. The
directory upload rule above applies. An explicitly named single file is not
filtered. Uploads run one at a time, with one trailing publish for edits
received during an upload.

With `--json`, each publish or non-fatal notice is one compact JSON line.
Events are `publish`, `retry`, `watch_error`, `root_vanished`,
`rewatch_failed`, `tree_changed`, and `empty_folder`. The CLI maps library
notice kinds to these names, so they are a stable contract. Every event has
`id` and `at`. Publishes also have `version` and `url`; notices include
`error` when available, and retries include `attempt`, the upcoming upload
attempt number, starting at 2 for the first retry, and `delay_ms`, the wait
before it. Transient failures use nominal exponential backoff from 1s to 60s,
with jitter below the cap. Rate-limit responses honor `Retry-After` up to a
ceiling of 10 minutes (10x the backoff cap), so a proxy that sends a huge
value cannot park the watcher.

Local races are not fatal. Only errors that wrap `ErrSourceChanged` count as
a tree change. The packer, the zip conversion, and the uploader produce
them for a file that vanished, shrank, or was edited while being read. In
the walk, that means an entry its directory listed that no longer exists
(and is not a broken symlink), or a file that ends before its opened size. Such
an error marks the tree dirty, emits `tree_changed`, and the publish runs
again after the next debounce. At most 3 such retries run in a row without a
filesystem event in between; the next one is returned as fatal, so a
misclassified persistent error cannot loop. Other local errors (a missing
`TMPDIR`, permissions, a full disk) are fatal at once. A root that vanishes
between the scan and the publish emits `root_vanished` and waits for it to
come back. The startup scan gets the same treatment: a file removed while it
is scanned is waited out for up to 3 debounce intervals. A folder
with no packable files emits `empty_folder` and publishes nothing; the last
version stays live until the rebuilt content publishes as a normal change.
Authentication and not-found errors use the normal CLI exit codes. Cancellation
returns success. The command creates immutable versions; it does not squash
history or inject live-reload code into uploaded artifacts.

## Tunnels

`redrop tunnel 3000` shares an app that is still running on the owner's
computer (a dev server with hot reload, an API, a webhook receiver) through a
link of the same shape as an artifact link. It answers issue #25.

### Decisions

* **A tunnel is an artifact of kind `tunnel`.** It has a four-word slug on
  the content domain, an owner, a title, an expiry, an optional password,
  revoke and admin revocation, and it shows in `redrop list` and the
  dashboard. It uses the same code paths as other artifacts: slug lookup,
  `sharing.Decide`, the gate and `Open`, the identical miss, the view log,
  owner preview, rotation and deletion. There is no second access check.
  * The kind lives where every kind lives, on the version row. A tunnel has
    exactly one version, kind `tunnel`, with no files and zero bytes. No
    migration is needed.
  * Refused for tunnels, with `400`: a view limit (`max_views`, `--once`),
    older versions, the SPA flag, new versions, activating or deleting a
    version, and a live reload lease (`redrop watch`). They don't fit a
    live app.
  * Tunnels use no storage, so they never count against a quota. Counting
    their traffic against the quota is not done.
* **The link works while `redrop tunnel` runs.** The CLI keeps one
  WebSocket to the app origin open, and visitors' requests travel through
  it to the local target. When the CLI is not connected, the tunnel is
  *offline*.
* **Viewer frame: as for HTML.** `sharing.modeFor` gives tunnels the frame
  when the info button is on. A top-level `GET` or `HEAD` page load gets
  the viewer page, which frames the same path; the address bar follows the
  frame, and `--no-info` or `?redrop-info=0` is the direct view. The info
  panel says "Live app" instead of listing versions, size and files.
  Feedback works as for HTML. Other methods (a form that posts to
  `_top`) go straight to the app. Nothing is injected into a proxied
  response.
* **`/__redrop/` stays reserved.** Reserved paths are answered before a
  request can reach the proxy, so the app can never serve the viewer's
  script, the gate, `open`, `preview` or `feedback`. A path that only
  becomes reserved after cleaning (`/x/../__redrop/`) gets the identical
  miss instead of reaching the app.
* **Single node.** The registry of connected tunnels is in the server's
  memory. One redrop process serves every tunnel; several processes behind
  a load balancer would each see only the CLIs connected to them, and
  visitors would get the offline page from the others. Running several
  instances against one data directory is not supported anyway (see
  "Disk budgets and upload expiry"). Restarting the server drops every
  tunnel; the CLIs reconnect.

### Protocol

```text
visitor ──HTTPS──▶ Cloudflare ─▶ Traefik ─▶ redrop content handler
                                                │  gates, then the proxy
                                                ▼
                         yamux stream (one HTTP/1.1 exchange per stream)
                                                │
          WebSocket /api/v1/tunnels/{id}/connect on the app origin
                                                │
                                                ▼
                        redrop tunnel (CLI) ──HTTP──▶ 127.0.0.1:3000
```

1. `POST /api/v1/tunnels` creates the artifact (title, TTL, password,
   info button, feedback). `--id` reuses one instead, after checking that
   it is a tunnel.
2. The CLI opens `GET /api/v1/tunnels/{id}/connect` as a WebSocket with
   its API token in `Authorization: Bearer`. Session cookies are not
   accepted there: a browser can't set that header on a WebSocket, so no
   web page can open a tunnel with a visitor's session. The CLI also sends
   `X-Redrop-Tunnel-Session`, a random value chosen once per process.
3. Before upgrading, the server checks the token, ownership, kind, that the
   link is live (not expired or revoked), that tunnels are on, the
   per-user limit, and that no other process holds the tunnel. Failures
   are ordinary JSON problems: `401` (token), `403` (tunnels off, or a
   session instead of a token), `404`, `400` (not a tunnel), `410`
   (revoked or expired), `409` `tunnel_in_use`, `429` `tunnel_limit`.
4. After the upgrade, the WebSocket carries one
   [yamux](https://github.com/hashicorp/yamux) session. The server is the
   yamux client and opens one stream per visitor request; the CLI accepts
   streams. Only the server opens streams: the first stream the CLI opens
   ends the connection (`1008`). The server rejects it without serving or
   reading it; otherwise it could hold up to a window of data, outside
   every limit, for as long as the session lasts.
5. Each stream carries exactly one HTTP/1.1 exchange, written and read by
   Go's `net/http`: the server's `httputil.ReverseProxy` dials by opening a
   stream (keep-alives off), and the CLI serves the session as a
   `net.Listener` with `http.Server` and its own `ReverseProxy` to the
   target. A `101 Switching Protocols` answer turns the stream into a raw
   byte pipe, which is how WebSocket upgrades (dev-server hot reload) work
   end to end.

**Framing.** WebSocket binary messages carry the yamux byte stream; their
boundaries mean nothing. yamux frames have a 12-byte header (version,
type, flags, stream ID, length) and carry data, window updates, pings and
go-away. Inside a stream the bytes are plain HTTP/1.1, so chunked bodies,
trailers, `HEAD` and ranges behave as they would on a socket. No framing
was invented for this. A visitor's `Expect: 100-continue` is answered by
the server itself and is not sent on.

**Flow control.** yamux gives every stream a 256 KiB window (its
minimum). A sender may have at most one window of unacknowledged data per
stream, and the receiver grants more only as its reader consumes. A slow
visitor therefore stops the app's writes after 256 KiB, and a slow app
stops the visitor's upload the same way. Bodies are copied in 32 KiB
pieces and never held whole. A peer that sends past the window breaks the
session (yamux enforces it).

**Keepalive.** Both ends send a yamux ping every 30 seconds, as WebSocket
data, so Cloudflare (which drops a WebSocket after about 100 idle seconds)
keeps the connection. The answer may take 30 seconds (yamux's default is
10): it waits in the same send queue as data frames, and a slow uplink with
many streams sending must not look like a dead peer. A dead peer is noticed
within about a minute.

**Frames from the CLI.** The server reads the header of every frame the
CLI sends before yamux does (`internal/tunnel/frames.go`). yamux v0.1.2,
the latest release, answers each ping in a goroutine of its own that may
wait 30 seconds to send, so a ping flood is a goroutine flood: 400,000
pings, under 5 MB, took a 768 MB server down in under a second. A CLI may
send 10 pings at once and one a second after that, far more than its
keepalive; past that its connection ends (`1008`).

**Cancellation and stream ends.** The server never half-closes a stream
it still uses, so its end of a stream means the request is over: the
visitor left, the visitor stopped reading (below), or access changed.
* The CLI then closes its end at once. It cancels the request to the
  local app and ends a write blocked on a full window, for a WebSocket's
  other direction too. That takes about a round trip.
* The server, after closing its end, reads and discards what the CLI
  still sends until the CLI's end arrives. Only then is the stream gone,
  and only then are its slots released (see "Limits"), so up to a
  window of unread data per stream is always counted by a slot.
* The CLI sees the server's end only when something reads the stream.
  While it is still writing a request body to an app that hasn't read it
  (an upload handler that authenticates or queues first), nothing does.
  So a stream the CLI hasn't closed 10 seconds after the server's end is
  reset, alone: the session drops it, with anything it buffered, and the
  CLI is told to reset it too. Its slots go then; the connection and its
  other streams go on. yamux v0.1.2 has no reset of its own, so the
  server hands its session a reset frame between two of the CLI's frames,
  as if the CLI had sent it, and sends one to the CLI between two of its
  own; a ping makes the CLI send a frame soon.
* After a reset the CLI may still deliver what it had in flight, at most
  the stream's window (256 KiB). A CLI that sends more on a reset stream
  ignored the reset: its connection ends (`1008`).
* yamux's own close timer is off: it marks a timed-out stream closed
  before dropping it, and a FIN from the CLI in between tears the whole
  session down.
* Go's `http.Transport` lets a dial outlive the request that started it.
  Such a dial opens no stream once the request's slot is released;
  otherwise its stream would release the slot a second time, and the
  slots would stop counting the streams that exist.
* yamux v0.1.2 deadlocks when two goroutines read one stream at once, so
  reads of a stream are made one at a time.

**Ending a tunnel.** The server closes the WebSocket with a code the CLI
understands (`pkg/api`):

| Code | Meaning | CLI |
|---|---|---|
| `1001` going away | server shutdown | reconnects |
| `4001` | ended: revoked, deleted, expired, owner disabled, token deleted, or ended by an administrator | prints the reason and exits 7 |
| `4002` | replaced by a newer connection of the same process | reconnects |
| `1008` | protocol violation: the CLI opened a stream, pinged too often, or ignored a stream reset | reconnects (an honest CLI never gets it) |

**Reconnects.** The CLI reconnects after any other failure with jittered
exponential backoff from 1 to 30 seconds, reset after a connection that
lasted a minute. A `Retry-After` is honoured up to 10 minutes. A
connection with the same session value replaces the old one at once, so
a CLI is never locked out by its own dead connection. A different process
gets `409` while the tunnel is connected and keeps retrying with a note,
so a second `redrop tunnel --id` waits until the first stops. `401`,
`403`, `404`, `410` and the per-user limit end the command.

**Checks after registering.** Revokes, admin revocations, disabled
accounts and deleted tokens end a connected tunnel through the registry,
which only knows a tunnel once it is registered. A change that lands
between the connect checks and the registration would find nothing to
end, so the server runs the checks again once the link is registered:
the token, the account and the artifact. A failure gives the reservation
up; otherwise the newest access generation and expiry apply. Changes are
also applied to the registry in the order they were committed: each
reads the row again under one lock, so two TTL changes racing can't leave
the older expiry armed.

**Choice of libraries.**

* `github.com/hashicorp/yamux` v0.1.2: maintained by HashiCorp and used in
  Consul, Nomad, Vault and Boundary; per-stream flow control with the
  receive window enforced; keepalive; stream open and half-close
  timeouts; no dependencies. Each stream is a `net.Conn`, so the standard
  HTTP client, server and reverse proxy run over it unchanged. HTTP/2 over
  the WebSocket was the alternative: it also multiplexes with flow
  control, but proxying WebSocket upgrades over it needs extended
  `CONNECT` on both ends. `smux` is similar to yamux but less widely used.
* `github.com/coder/websocket` v1.8.15 (formerly `nhooyr.io/websocket`):
  maintained by Coder, context-aware, no dependencies, and it has a
  `NetConn` adapter made for tunnelling a protocol over a WebSocket. Coder
  runs yamux over it in production. `gorilla/websocket` has neither the
  adapter nor contexts.

### What the app receives

The server prepares each request before it enters the tunnel:

* **Removed:** redrop's own cookies (`__Host-rd_visit`, `_preview`,
  `_seen`, `_direct`; `rd_*` on plain-HTTP development servers); every
  hop-by-hop header (`Connection` and what it names, `Keep-Alive`,
  `Proxy-Connection`, `Proxy-Authorization`, `TE`, `Trailer`,
  `Transfer-Encoding`, `Upgrade` unless the request is an upgrade);
  `Forwarded` and every `X-Forwarded-*` header (the visitor's, and
  Traefik's own such as `X-Forwarded-Server` and `-Port`); `X-Real-Ip`,
  `True-Client-Ip`, `Client-Ip`, `X-Client-Ip`, `X-Cluster-Client-Ip`,
  `X-Forwarded` (no suffix), `Proxy-Client-Ip`, `WL-Proxy-Client-Ip`,
  `Fastly-Client-Ip`, `X-Original-Forwarded-For`, `X-ProxyUser-Ip`, `Via`
  (client addresses under the names proxies and app servers read),
  `X-Original-Url`, `X-Rewrite-Url`, `X-Original-Host`, `X-Host` (URL and
  host overrides some frameworks honour), `CDN-Loop`, every `CF-*`
  header, the configured client-IP and country headers; every header
  whose name has an underscore, which CGI-style app servers read as the
  dashed name (`X_Forwarded_For` would replace `X-Forwarded-For`); and
  `Expect`.
* **Set:** `X-Forwarded-For` to the client address the server resolved for
  its rate limits (`REDROP_TRUSTED_PROXIES`, `REDROP_TRUSTED_HOPS`), never
  the visitor's own list; `X-Forwarded-Host` to the tunnel's host;
  `X-Forwarded-Proto` to the content domain's scheme. This is the
  visitor's full address, as the issue asks: the owner's app sees what
  any web server would, while redrop's own view log keeps only a network
  label and a country.
* **Kept:** `Authorization` (redrop never uses it on content hosts, so it
  belongs to the app), the app's own cookies, `Origin`, `Referer`, and
  every other header.
* **Host.** The CLI sends the target as `Host` (`localhost:3000`), the name
  the owner typed, because dev servers such as Vite refuse unknown hosts.
  The public host is in `X-Forwarded-Host`. An app that checks `Origin`
  against `Host` has to read `X-Forwarded-Host`.
* **Query.** `redrop-info` and `redrop-version` are the link's own
  parameters and are not passed on.

### What visitors receive

The app's response goes back unchanged, apart from these headers and
informational responses:

* **1xx:** dropped. The server answers a visitor's `Expect:
  100-continue` itself, and `103 Early Hints` is only a hint. A `101`
  (WebSocket) goes through. redrop's headers are applied when the final
  response (or the `101`) is written, not set beforehand: Go's
  `ReverseProxy` clears the header map after each 1xx it forwards, which
  would otherwise take `Cache-Control: private` and the rest off the
  final response.
* **Trailers:** dropped. They would skip the rules below (a trailer
  `Set-Cookie` with a `Domain`), and browsers make no use of them.

* **`Set-Cookie`:** the `Domain` attribute is removed, so every cookie is
  host-only on the tunnel's own origin and can't reach other artifacts. A
  cookie named like one of redrop's is dropped. Nothing else in the cookie
  changes.
* **Same as artifacts, forced:** `X-Robots-Tag`, `Referrer-Policy:
  no-referrer` (the slug is the secret), `X-Content-Type-Options:
  nosniff`, and HSTS on HTTPS. The app's own values for them are dropped.
* **`Clear-Site-Data`:** dropped. Its `"cookies"` clears cookies for the
  whole registrable domain, and the content domain is one site: it would
  clear visitors' grants for every artifact.
* **Caching:** `Cache-Control: private, no-cache`, or `private, no-store`
  when the app said `no-store`. The app's `Expires`, `CDN-Cache-Control`,
  `Cloudflare-CDN-Cache-Control` and `Surrogate-Control` are dropped.
  Cloudflare would otherwise cache a `public` response and serve it
  without the password check. Browsers still revalidate with the app's
  `ETag` or `Last-Modified`.
* **`Content-Security-Policy`:** redrop's `frame-ancestors 'self'
  <dashboard>` is added next to the app's own policy, so the viewer frame
  and the dashboard preview work. An app that sends `frame-ancestors
  'none'` or `X-Frame-Options: DENY` can only be opened with `--no-info`.
* **Not forced (would break apps):** artifacts' `Permissions-Policy`
  (camera, microphone, geolocation, payment, USB off) is not sent, so an
  app under development can use them. `Cross-Origin-Resource-Policy:
  same-site` is sent only when the app sent none.
* No service worker: a content host never serves a service worker script,
  tunnels included.

### Visitor requests

The content handler answers a tunnel host like any artifact host. The
changes are in what follows the access check:

1. Misses, the miss budget, reserved paths, icons and robots are as for
   every artifact host. Before this change, any method but `GET` and
   `HEAD` on a non-reserved path got `405` without a lookup. It now gets a
   lookup: tunnels take every method. Other kinds still answer `405`, and
   gone links the identical miss, so a `POST` reveals no more than a `GET`
   and spends the miss budget the same way.
2. `sharing.Decide` and the gate, unchanged. A visitor without access
   gets the gate (pages) or `403` (anything else). Gone links get the
   identical miss.
3. `Sec-Fetch-Site: same-site` requests are refused (`403`), except `GET`
   and `HEAD` navigations. They come from another artifact or tunnel on
   the content domain, where `SameSite=Lax` cookies (redrop's visit
   cookie and the app's own) would ride along, so another user's page
   could act as the visitor (CSRF). A link from another artifact still
   opens. Requests from other sites keep the browser's usual rules, as on
   any website.
4. Not connected: `503` "This tunnel is offline", a plain redrop page.
   Only visitors who passed step 2 see it, so whether a tunnel is online
   is never visible without access.
5. Otherwise the proxy. The favicon follows the icon rule: with proof of
   access it comes from the app, otherwise the redrop icon. The icon paths
   get steps 3 and 4 too: a same-site request gets the redrop icon, and
   the link's own parameters are not passed on.

Views are counted as for HTML: one per top-level page load (ungated) or
per Open (gated). Subresources, frames, WebSockets and API calls never
count.

### Limits

| Setting | Default | Bounds |
|---|---|---|
| `REDROP_TUNNELS_PER_USER` | 3 | connected tunnels per user; `0` turns tunnels off |
| `REDROP_TUNNEL_ARTIFACTS_PER_USER` | 20 | tunnel artifacts a user keeps, in any state, until deleted |
| `REDROP_TUNNEL_STREAMS` | 32 | open requests and WebSockets per tunnel |
| `REDROP_TUNNEL_STREAMS_TOTAL` | 256 | the same across the server |
| `REDROP_TUNNEL_MAX_REQUEST_SIZE` | 100MB | request body, under Cloudflare's 100 MB limit |
| `REDROP_TUNNEL_MAX_RESPONSE_SIZE` | 1GB | response body |
| `REDROP_TUNNEL_BYTES_PER_HOUR` | 2GB | bytes both ways per user, over all their tunnels, fixed one-hour window |
| `REDROP_TUNNEL_RESPONSE_TIMEOUT` | 90s | one budget per request: the wait for a free slot, then (once the request is sent) the app's wait to send response headers |
| `REDROP_TUNNEL_IDLE_TIMEOUT` | 100s | a request or WebSocket with no bytes either way |

* **Streams.** A request waits for a free slot for up to the response
  timeout, then gets `503`. At most as many requests wait per tunnel as it has
  slots; more get `503` at once. A slot is held from the request's start
  until its stream is gone, after the request has ended too (see
  "Cancellation and stream ends"), so the slots count every stream that
  exists. The server-wide total therefore bounds memory: each stream
  holds at most a 256 KiB receive window plus about 40 KiB of copy
  buffers, so 256 streams stay under 80 MB, well inside the container's
  768 MB. The admin view's `streams` are the slots in use.
* **Visitors who stop reading.** Every write to the visitor, a hijacked
  WebSocket's included, must finish within the idle timeout, or the
  request ends and its slots go. A visitor who reads very slowly is not
  cut: a 32 KiB write within the idle timeout (about 330 bytes a second
  at 100 s) keeps the request going, and so does an upload trickled in a
  byte per idle period. Such a visitor holds that tunnel's slots, never
  more than its 32, and the waiting bound keeps the queue short.
* **Sizes.** A declared body over the limit gets `413` (request) or `502`
  (response) before anything is sent. A chunked body that grows past it
  is cut off there.
* **Bandwidth.** Counted as bytes flow, through `security.Limiter` keyed by
  the owner, so a fresh tunnel doesn't start a fresh budget. When the
  hour's budget runs out, open streams are cut and new requests to any of
  the owner's tunnels get `429` with `Retry-After` until the window ends.
* **Count.** Tunnels take no storage, so the quota doesn't bound them; a
  user keeps at most `REDROP_TUNNEL_ARTIFACTS_PER_USER` of them, and a
  new one past that gets `409 tunnel_count_limit` (reuse one with `--id`,
  or delete one).
* **Time.** A request gets the response timeout once: what it spent
  waiting for a slot is taken from what the app then has to send its
  headers, counted from when the whole request reached the app. Past it:
  `504`. Cloudflare gives up at about 100 seconds, so the default answers
  first. Upload time doesn't count, as it doesn't for Cloudflare.
  Idle streams are closed. Streaming responses and WebSockets may run for
  as long as bytes keep moving; behind Cloudflare, an idle WebSocket is
  closed after about 100 seconds anyway, and hot-reload clients reconnect.
* **The WebSocket itself** has no size limit per message (yamux bounds what
  a peer may send) and is kept alive by the pings above.

### Lifecycle and moderation

* **Owner.** Ctrl-C ends the tunnel (the CLI closes the WebSocket
  normally). Revoking, deleting or expiring the artifact ends the
  connection at once; the CLI exits. Changing the password or rotating the
  link closes the streams that were let in under the old access (open
  WebSockets included), so an open hot-reload connection can't outlive a
  password change.
* **Expiry** is a timer on the connection, set again when the TTL changes.
  Requests check it anyway (`Decide`).
* **Tokens.** Deleting the API token a tunnel connected with ends the
  tunnel, and so does the token's expiry.
* **Admins.** The admin artifact list shows tunnels with their owner and
  online state, like every artifact (metadata only). `GET
  /api/v1/admin/tunnels` lists the connected ones, and `DELETE
  /api/v1/admin/tunnels/{id}` ends one: the CLI exits with "ended by an
  administrator" and may connect again. To keep it off, revoke it; an
  admin revocation stays out of the owner's reach as for any artifact.
  Disabling a user ends their tunnels, and their token no longer connects.
* **Shutdown.** The server ends every tunnel with `1001` before it stops,
  and the CLIs reconnect to the next process.

### Dial safety (CLI)

* The CLI dials only the target the owner named. It resolves the target
  once at start and pins the addresses; every request goes to them, never
  to a host from the request (`Proxy` is off, so `HTTP_PROXY` is not used
  either). Redirects from the app go back to the visitor; the CLI follows
  none.
* Targets: `3000`, `host:3000`, `[::1]:3000`, `http://host:3000`. A bare
  port, `localhost` and `*.localhost` mean `127.0.0.1` and `::1` without
  asking DNS, tried in that order (dev servers listen on either). HTTPS
  targets, paths and other schemes are usage errors.
* **Classes.** Every address is loopback, private (RFC 1918, CGNAT, link
  local, IPv6 unique local and link local) or public. A link-local IPv6
  target keeps its zone (`[fe80::1%eth0]:3000`) for dialing; the `Host`
  header has none. Unspecified,
  multicast and broadcast addresses are refused. A name must resolve to a
  single class: a name that returns both a loopback and a LAN address is
  refused, so DNS can't move the target to another class than the one it
  resolved to. Only loopback is allowed by default. Anything else needs
  `--allow-remote-target`, and the CLI warns that anyone with the link can
  reach that address through the owner's computer.

### Logging

The server logs that a tunnel connected and disconnected (artifact ID,
user, reason, duration), admin ends, and a CLI ended for a protocol
violation. It never logs request headers, bodies, paths or query strings
of tunnel traffic; proxy failures are logged at debug level with the
artifact ID and the failure kind only. The connect endpoint is an
app-origin API call and is logged once, by path, like every API call. The
CLI prints one stderr line per request with `--log`: method, the path
percent-encoded as on the wire (so a visitor can't write terminal escapes
or fake lines to the owner's terminal), without the query, the final
status (not a `103`) and duration.

### Threat model

* **A tunnel serves anything the owner's computer returns,** including
  phishing pages and malware. Only invited accounts can open tunnels, the
  admin view shows the owner, and the moderation tools are the artifact
  ones: revoke, delete, disable the user, plus ending a connection.
  `REDROP_TUNNELS_PER_USER=0` turns the feature off.
* **Reaching the owner's network.** The server never connects to the
  owner's computer; the CLI connects out. The CLI dials one pinned target,
  loopback unless the owner opts in, so a link can't be turned into a way
  into the owner's LAN by accident. Everyone with the link (and the
  password) can use the app as the owner would locally: the app's own
  authentication is its business.
* **The server trusts the CLI no more than an artifact's bytes.** The
  response is untrusted content on an isolated origin with the headers
  above. A hostile CLI can waste its own tunnel's slots (and with them
  some of the server-wide ones) and its owner's bandwidth. What it can
  make the server buffer is bounded by the slots, since every stream holds
  one until it is gone. A stream it opens itself, a ping flood, or data
  on a stream it was told to reset ends its connection; a closed stream
  it keeps open is reset after 10 seconds.
* **Other artifacts on the content domain** are same-site. A tunnel can't
  set cookies for them (`Domain` removed). Their same-site requests to a
  tunnel are refused except navigations. They can still set
  parent-domain cookies that the browser sends to the tunnel, which the
  app then receives (cookie tossing), as for artifacts ("Domains and
  isolation"); a content domain on the Public Suffix List ends that.
* **Abuse of resources.** The per-user counts (connected and kept),
  per-tunnel and total streams, sizes, timeouts and the per-user hourly
  byte budget bound one user's cost. A visitor of a tunnel can hold that
  tunnel's slots, not other tunnels', and only while bytes move (a
  visitor who stops reading is cut after the idle timeout). The
  server-wide total is shared, so someone with links to eight tunnels can
  keep it full with requests that keep moving data.
* **Credentials.** The API token stays in the CLI and the WebSocket
  request header; the session value is not a secret. Visitors' cookies
  for redrop never reach the app.

### Testing and rollback

Go tests run the whole path in one process: a real server handler, the
CLI's agent over a real WebSocket, and a local `httptest` app. They cover
the multiplexer, reconnects and backoff, visitor cancellation, every
limit, a WebSocket upgrade end to end, password, expiry and revoke parity
with HTML artifacts, the offline page after the access check, header
hygiene, and 300 MB each way through the tunnel with the heap staying
bounded. They also cover a CLI that opens streams, floods pings (reading
the answers or not) or ignores a stream reset, 1xx responses and
`Expect`, response trailers, visitors who stop reading (responses and
WebSockets) or leave with a full window, a stream stuck behind an app that
doesn't read its upload (reset alone, the connection stays), a CLI that
keeps writing after the server's end, the shared response budget, an
access change before the headers, the checks after registering, and the
per-user budget. The e2e script tunnels a small HTTP and WebSocket server
with a password set.

Rolling back the binary leaves tunnel artifacts in the database. An older
server reads their kind as an unknown one and shows an empty file listing
on the link; an older dashboard fails to decode them. Delete tunnels, or
revoke them, before rolling back.
## Email

Issue #40 adds provider-neutral outbound mail for invite links and batched
operator alerts. Mail is off when `REDROP_SMTP_ADDR` is unset. Invite links
still work by copying them, and the existing report webhook is independent.
There are no password-reset emails or email verification in this feature.

### Transport and configuration

`internal/mail.Mailer` accepts a message and a context. `SMTP` implements it
using `net/smtp`; `Discard` does nothing. SMTP supports mandatory STARTTLS,
implicit TLS, optional AUTH, and a private CA file extending system trust.
Certificate and hostname checks never turn off. Plain SMTP requires the explicit
`REDROP_SMTP_INSECURE_ALLOW_PLAIN=true` test setting. Credentials come from a
password file. The [README](../README.md#email) lists all settings and the
Docker Compose mounts and test command.

Messages have plain-text and escaped HTML alternatives, base64 transfer
encoding, a random Message-ID, Date, and `Auto-Submitted: auto-generated`.
Every rendering gets a new Message-ID and Date, so a resent invite is a new
message that mailbox providers do not drop as a duplicate. Templates contain
no tracking. EHLO names the `REDROP_MAIL_FROM` domain, not `localhost`. AUTH
is refused without TLS unless the relay is on a loopback address, so test-only
plain mode can never send credentials across a network. Recipient addresses
must name a domain; literals such as `a@[192.0.2.1]` are refused. The domain
part is stored in lower case, so the bounce list and alert state match however
an address was typed. An SMTP error retains only its protocol stage and
numeric status. Remote text can echo a one-time link or credentials, so it is
never retained or logged. Recipient addresses are absent from delivery logs
too. `redrop-server mail-test --to ADDR` sends one test message without
opening the application database and reports SMTP acceptance or stage/status.

### Invites and durable delivery

Administrators can add an optional email address to a new invite. The field
appears only when `/auth/config` reports `mail: true`. The recipient sees the
note, inviter, and expiry, and the invite's one-time link. The invite list
shows recipient, queued/sending/sent/failed status, time, and safe failure
reason. Send again is available only for unused, unexpired emailed invites;
already queued or sending invites refuse duplicate resend requests.

**No invite token is stored, in any form.** Before this feature every live
credential (sessions, API tokens, invite, setup and reset links) was stored
only as a hash, so a copy of the data directory gave a thief data but no way
in. Emailed invites keep that property:

* Creating an emailed invite stores the invite with the hash of a secret
  nobody keeps, plus a queue row that holds the recipient and the inviter's
  name, and no body. The create response and `redrop-server invite --email`
  therefore carry **no link**: one shown there would stop working when the
  email is sent. The dialog says the link is only in the email.
* When the worker claims an invite row, it mints a new token and, in the
  same transaction, replaces the invite's hash with the token's hash. It
  renders the email in memory with that link, sends it, and drops it. The
  token exists only in process memory and in the sent email.
* Each attempt (first send, automatic retry, Send again, or a retry after a
  restart) gets its own link, and only the latest email's link works. Send
  again therefore also revokes the previous email's link, which is what an
  administrator wants when the reason is "wrong inbox" or "it leaked".
* Redemption looks the token up when the request arrives and consumes the
  invite only if its hash still matches that token. A signup request that
  passed the lookup just before Send again rotated the token is refused as
  used or expired (410) when it commits, so a revoked link cannot finish a
  signup it started. The new email's link still works.
* An administrator who wants to share a link by hand creates a link-only
  invite, which works exactly as before this feature.

Issue #40 asks that "the link in the email is the same one-time link as
today". This design reads that as the same kind of link: one-time, stored
only as a hash, the same redemption flow. It cannot be the string the
dashboard showed at creation, because that string would have to be stored
for the worker to send it. The owner should confirm this reading.

Creating an emailed invite atomically stores the invite, the queue row, and
the throttle charge. Rejection leaves no invite behind. Sends and manual
resends share persistent limits of 10 per administrator per hour and 60 globally.
The offline CLI uses the same rules with one shared offline-administrator
budget. `redrop-server invite --email ADDR` queues without starting a worker
and prints the invite ID; the serving process discovers its rows within a
minute.

One background worker handles a maximum backlog of 128 messages. HTTP requests
only commit queue state and wake that worker. Attempts have a 30-second
timeout. Transient failures schedule at most three retries, after 1, 5 and
30 minutes, without holding up other messages, so a relay restart or a short
rate limit does not fail invites for good. An invite is not retried past its
expiry: that attempt fails and says so. Permanent SMTP errors stop retries.

Only a permanent mailbox refusal suppresses an address: a 5xx at RCPT with an
RFC 3463 enhanced status of `5.1.x` (address) or `5.2.x` (mailbox). A bare
`550` without an enhanced status fails the message but does not suppress the
address, because relays also answer "relaying denied" and their own
misconfiguration that way. Authentication, sender and policy refusals never
suppress. Later asynchronous bounce messages are not processed. A suppressed
address is refused for new invites and Send again (409), and its report
digests are held, with one log line naming its position in
`REDROP_NOTIFY_EMAIL`. `redrop-server mail-unbounce ADDR` clears it.

Attempt counts, deadlines, and outcomes survive restart. Shutdown cancels an
in-flight attempt and returns its row to queued. A failed outcome write retries
the database commit without repeating SMTP. After an unclean exit, interrupted
rows are recovered, and a recovered invite gets a fresh token when it is sent.
A crash between SMTP acceptance and the commit sends the email again on
restart; there is no exactly-once guarantee. The first email's link is dead by
then, which is safer than two live copies. The exception is the last allowed
attempt: a recovered row that already used every attempt is failed ("interrupted
at the retry limit; use Send again") before any rotation, so an email that did
go out on that attempt keeps a working link.

### Report digests

The existing report announcement hook wakes the email worker in addition to
notifying the webhook. It does no SMTP or database work on the request path.
`REDROP_NOTIFY_EMAIL` accepts up to 16 deduplicated addresses. Each receives at
most one SMTP attempt per `REDROP_NOTIFY_EMAIL_INTERVAL`, including retries.
The default is ten minutes. The first digest waits one interval, while the
webhook remains immediate. Under a steady report stream a digest goes out
every interval: the next one waits for the interval after the previous
digest's attempt, not a whole interval from the moment it was due. A
recipient whose digest is held (a suppressed address, a full queue) does not
hold up the others.

Digests contain report IDs, category, source, time, and the moderation-queue
link. They exclude artifact content and links, reporter text and contacts,
owner names, and file names. A persistent report insertion cursor advances in
the same transaction as enqueue. Reports sharing a timestamp cannot be lost.
Up to 200 reports fit in a digest; any remainder enters later digests. Existing
historical reports are skipped when an address is first enabled. Removing an
address cancels its pending digests on the next server start. Queued or retrying
digests cannot multiply during a report flood.

### Storage and rollback

Migration 10 follows encryption migration 9. It adds `onetime.email`, the
`mail_out` queue with a unique cascading invite reference, `mail_sends` for
hourly throttle charges, `mail_bounced`, and `mail_alerts` for digest cursors and
deadlines.

Invite rows hold the recipient, the inviter's name and the send state, never a
body or a token, so a copy of the database (with or without `secret.key`)
yields no working invite link. Digest rows hold their rendered message, which
carries report IDs, categories and times and no secret; a finished digest's
body is cleared at once and its send metadata kept for seven days. An invite's
row goes with the invite when it is revoked. Recipient addresses are stored
in the clear.

An older server ignores the mail tables and new invite column. Rolling back
the image stops email delivery but keeps manual links working, and the link in
an email already sent keeps working too: it is an ordinary hashed invite.
Queued invite emails stay queued until the new binary returns. Preserve the
whole data directory. This feature's tests cover the fake relay, token
rotation, queue failure/restart paths, the HTTP boundary, and dashboard
schemas and rendering. Real relay delivery, sender-domain authentication,
certificate renewal, and production deployment remain operator
responsibilities.

## Abuse handling

The content domain serves arbitrary HTML from invited users. One phishing
page or piece of malware can get the whole domain flagged by Safe Browsing
or blocked by ISPs, which breaks every artifact. This section answers issue
#12: visitors can report a link, an operator gets a queue and a
notification, and publishing flags two obvious patterns. Nothing is added to
an artifact's bytes or DOM (see "Principles").

### Decisions

* **Report links live on redrop's own pages only.** The gate page (both the
  password and the view-limited form), the info panel (viewer page and file
  page) and the content apex link to the report form. The uniform miss page
  is not changed: expired, revoked and unknown links must keep answering
  byte for byte the same (see "Enumeration hardening"), and there is no
  separate offline or expired page.
* **The form is on the app origin**, `/report`, a public dashboard route.
  The content domain is a different registrable domain and the dashboard
  sends `frame-ancestors 'none'`, so an artifact can neither script the form
  nor frame it. The link carries the page's URL in the fragment,
  `/report#url=<page>`. The fragment never reaches a server or a proxy log;
  the form reads it and fills in the URL field.
* **No automatic action.** A report never revokes, hides or changes
  anything by itself. An administrator decides. Otherwise anyone could get
  any link taken down by reporting it.
* **Upload checks only flag.** They never block, change or delay a
  publication, and a failing or slow check is logged and ignored.
* **Notifications are optional and carry no free text.**

### Reporting

`POST /api/v1/reports`, no account, JSON only:

```json
{"url": "https://amber-otter-quilt-river.usercontent.example/page.html",
 "category": "phishing", "details": "Asks for a bank password", "contact": "sam@example.org"}
```

| Field | Rule |
|---|---|
| `url` | required; an `http(s)` URL whose host is `<slug>.<content domain>` with a well-formed slug, or the configured app origin's `/e/<slug>` intake link with one optional trailing slash; at most 2048 bytes. The form strips fragments before sending, and the server strips them again. Intake links are stored as the canonical content URL without query or fragment |
| `category` | `phishing`, `malware`, `illegal`, `spam` or `other` |
| `details` | optional, at most 2000 characters (code points), cleaned like a feedback comment (line breaks, controls and invisible characters) |
| `contact` | optional, at most 200 characters, one line; unverified and never turned into a link |

The checks, in order:

1. **Same origin.** The `X-Redrop-Request: 1` header, `Sec-Fetch-Site`
   (when present) `same-origin`, and an `Origin` that equals
   `REDROP_PUBLIC_URL` exactly. `Origin` is required: a report has no
   cookie to protect, but without the check any website could make its
   visitors' browsers file reports, from as many networks as it has
   visitors, past the per-network limit. Otherwise `403`.
2. A JSON body of at most 8 KiB (`415`, `413`, `400`).
3. **Rate limit**: `REDROP_REPORTS_PER_HOUR` (default 5) per client network
   (`netutil.Network`, IPv6 by /64), a fixed one-hour window in a
   `security.Limiter` of at most 50,000 networks that evicts the oldest.
   Otherwise `429` with `Retry-After`.
4. Valid fields (`400`). The URL check is syntax only; it says nothing
   about whether the link exists.
5. **Global cap**: fewer than `REDROP_REPORTS_MAX_OPEN` (default 1000)
   open visitor reports. Otherwise `429` "not accepting reports right
   now", with the abuse contact when one is configured. This does not
   depend on the URL.
6. The slug is looked up (one indexed query, as for a visit). The report is
   stored only when it names an **active** artifact (one the content host
   would serve now), the artifact has fewer than
   `REDROP_REPORTS_MAX_OPEN_PER_ARTIFACT` (default 20) open visitor
   reports, and the same network has no open report on it. A revoked,
   expired or used-up link is dropped like an unknown one: it cannot hurt
   visitors, and storing (or refusing) a report for it would turn the form
   into an oracle for which hidden links still exist.

Steps 1 to 5 answer the same whatever the URL names. Step 6 always answers
`202 {"ok": true}`: unknown links, full artifacts and duplicates are
dropped silently. So the form is not a way to find out which links exist,
and a reporter can't tell a dropped report from a stored one.

**Network key.** For the duplicate check, a report stores
`HMAC-SHA256(server secret, "report-network" ‖ network)`, never the address.
The key is cleared when the report is closed. The rate limiter keeps the
network in memory only, as every limiter does.

### Upload checks

After a publication commits (new artifact or new version), the service
scans the new version's files and stores a **system report** for each
finding. Findings:

* **Credential form**: an HTML file (`text/html`, `.html`, `.htm`) with a
  `<form>` that contains `<input type=password>` and whose `action` (or a
  `formaction` on one of its buttons) is an absolute or protocol-relative
  URL on another host. Category `phishing`.
* **Executable**: a file whose first bytes are a PE (`MZ` with a `PE\0\0`
  header), ELF, Mach-O or universal Mach-O image, or whose name ends in a
  Windows, Android, macOS or Linux installer or script extension (`.exe`,
  `.msi`, `.scr`, `.apk`, `.dmg`, `.deb`, `.bat`, `.hta`, … see
  `sharing/scan.go`). Category `malware`. It applies to bundles and to
  single-file artifacts.

The detail names the file and, for a form, the host it posts to, for
example `login.html: a password form posts to evil.example`. That is all
an administrator learns of the content; it is cleaned and shown as text.

**Bounds and scheduling.** The scan runs on one background worker, off the
publish request path entirely: a publication queues its version's files and
returns immediately, so the checks never delay a response. The queue holds
64 jobs; when it is full the scan is dropped and logged. A scan reads at
most 4 KiB of each file for magic numbers, at most 1 MiB of each HTML
file, at most 32 MiB in all, and stops after 5 seconds; it stops early at
the first finding of each kind. A version that is cut short is logged
(`upload check incomplete`). At most one open system report per artifact
and finding kind exists, and at most one per owner and kind: a later
version with the same finding, or another artifact by the same owner with
it, adds nothing until the first is closed. That second bound is what
keeps one account from filling the queue with upload flags, one artifact
at a time.

**Form parsing follows the browser.** A `<form>` start tag inside an open
form is ignored, as the HTML parser ignores it. The first `<base href>`
outside a form resolves relative actions. Before a URL is parsed, ASCII
tab and newline are removed and leading C0 controls and spaces are
trimmed, as the URL Standard requires, and `input type` is compared
case-insensitively without trimming.

**Known limits.** Forms built by scripts, `fetch()` posts and password
fields associated with a form through `form=` from outside it are not
seen. Executables inside nested archives are not opened. These checks
catch the careless case and give an operator a lead, not a guarantee.

### Admin queue

Admins see a **Reports** tab: open reports by default, with resolved and
dismissed as status filters and visitor/system as a source filter. The
list is paged, 50 to a page with a total count for the filter, so a full
queue hides nothing: with both sources shown, visitor reports come first
(newest first within each source), because a visitor took the trouble to
write what happened and must not be buried under machine flags.

Each report shows its source (visitor or system), category, time, the
reporter's text and contact (as text), the artifact (title, kind, owner,
current status, or "deleted"), and its history. Actions:

| Action | Effect |
|---|---|
| Resolve / Dismiss / Reopen | sets the report's status |
| Revoke artifact | the administrator's revocation (migration 4, "Lifecycle"): the owner can't undo it. Every open report on the artifact is resolved |
| Suspend owner | disables the account and revokes all of its artifacts as an administrator. Every open report on those artifacts is resolved |

**The reported link.** A visitor report keeps the URL the reporter
submitted, and the queue links to it ("Open reported link"), so an
administrator can judge the page before acting. This is the one place an
administrator sees an artifact link, and only one that a third party has
handed over, the same as an email to the abuse address would. Admin
results otherwise stay metadata only: `AdminArtifact` is unchanged, and a
reflection test pins `AdminReport`'s fields. System reports have no link.
Opening the link is an ordinary visit: it counts a view, a password still
applies, and the page may be hostile. The dashboard says so.

**Suspension** is the existing "disabled" account state plus revocation:

* the account's sessions end, password and single sign-on sign-in are
  refused, and its API tokens get `401`, so it can't publish;
* every artifact it owns gets the administrator's revocation (its links
  stop at once: the access generation changes). A publication that was
  already running when the account was suspended commits an artifact that
  is revoked from the start (`CreateArtifact` copies the owner's disabled
  flag into `admin_revoked`);
* an administrator account can't be suspended from the queue. Change its
  role first; this also rules out suspending yourself or the last admin.

Lifting a suspension is the Users page's "disabled" switch. It brings the
account and its tokens back, but not its artifacts: an administrator
restores each one from the Artifacts tab after reviewing it.

**Audit.** Every queue action writes one `audit` row in the same
transaction as its effect: time, administrator, action, report, artifact,
user, and a short detail (for example `revoked 3 artifacts, resolved 2
reports`). Undoing an action from elsewhere in the dashboard writes a row
too: restoring an artifact (`artifact.restore`) from the Artifacts tab,
and enabling an account (`user.unsuspend`) from the Users tab. Those rows
have no report of their own, so they attach to the history of every
report on the same artifact or user: a takedown that was quietly reversed
is still visible there. The server logs each action as well. Rows are
never edited or deleted by redrop.

### Notifications

When `REDROP_NOTIFY_URL` is set, redrop POSTs JSON to it for every stored
report, visitor or system:

```json
{"level": "warning", "title": "redrop: abuse report",
 "message": "Category phishing, artifact 01J…, owner alice.", "key": "redrop-report-01J…"}
```

The title is `redrop: upload flagged` for a system report. With
`REDROP_NOTIFY_TOKEN_FILE`, the file's content is sent as
`Authorization: Bearer …`. The message holds only the category, the
artifact ID and the owner's username, never the reporter's text, contact or
URL, nor a flagged file name: those are untrusted and would reach whatever
renders the notification (a chat client, a phone). `key` lets the receiver
deduplicate.

Delivery never blocks a request. Messages go into a queue of 64; when it
is full the message is dropped and logged. One worker sends them, each
attempt with a 10-second timeout, retrying a network error, `408`, `429` or
`5xx` up to 4 times in all with waits of 1, 4 and 16 seconds. Other answers
are logged and dropped. At shutdown the worker stops: an in-flight attempt
is cancelled and queued messages are logged as dropped. The URL and token
are never logged.

### security.txt

With `REDROP_ABUSE_CONTACT` set (an email address, a `mailto:` URI, or an
`https://` URL), `/.well-known/security.txt` (RFC 9116) is served on the
app origin and on the content apex:

```text
# Security and abuse contact for this redrop server.
Contact: mailto:abuse@example.com
# Report a shared link that is abusive: phishing, malware, illegal content, spam.
Contact: https://redrop.example.com/report
Expires: <180 days from the request, in UTC>
Preferred-Languages: en
Canonical: https://redrop.example.com/.well-known/security.txt
```

`Canonical` names the host it was served on. Without the setting the path
is a `404` on both hosts. Artifact hosts never serve it; an artifact may
contain its own file at that path on its own subdomain. The value is
checked at startup: anything else, `http://`, or a value with spaces or
line breaks, is a configuration error.

### Storage (migration 8)

```sql
CREATE TABLE reports (
    id          TEXT PRIMARY KEY,
    source      TEXT NOT NULL,              -- visitor | system
    artifact_id TEXT NOT NULL,              -- no foreign key: a report outlives its artifact
    owner_id    TEXT NOT NULL,              -- the owner when reported
    version     INTEGER NOT NULL DEFAULT 0, -- system reports: the flagged version
    kind        TEXT NOT NULL DEFAULT '',   -- system reports: form | executable
    url         TEXT NOT NULL DEFAULT '',   -- visitor reports: as submitted
    category    TEXT NOT NULL,
    details     TEXT NOT NULL DEFAULT '',
    contact     TEXT NOT NULL DEFAULT '',
    network     TEXT NOT NULL DEFAULT '',   -- keyed hash, cleared on close
    status      TEXT NOT NULL DEFAULT 'open',
    created_at  INTEGER NOT NULL,
    closed_at   INTEGER,
    closed_by   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE audit (id INTEGER PRIMARY KEY AUTOINCREMENT, at, actor, action,
    report_id, artifact_id, user_id, detail);
```

Indexes cover the queue (`status, created_at`), the per-artifact checks
(`artifact_id, status`) and a report's history (`audit.report_id`). An older
server ignores both tables, so rolling back the binary alone just hides the
queue; artifacts revoked and accounts disabled from it stay so.

### Threat model

* **Report spam.** The per-network limit, the global and per-artifact caps,
  the per-network duplicate check, the 8 KiB body and the field limits bound
  rows to about 1,000 open reports of at most about 10 KB. A full queue
  refuses new reports with a pointer to the abuse contact instead of
  growing. Webhook messages are bounded by the same limits and by the
  dropping queue.
* **Reporting to get content removed.** There is no automatic action, so
  a flood of reports only fills the queue. The per-artifact cap keeps one
  link from burying the rest, and duplicates from one network count once.
* **Enumeration.** The endpoint answers the same for every URL that passes
  the syntax check, and the limit of 5 an hour per network is far slower
  than the content host's miss limiter. Only active artifacts are
  reportable, so a dropped report for a revoked or expired link is
  indistinguishable from one for an unknown link even under cap pressure.
* **Cross-site filing.** Required same-origin `Origin`, the custom header
  (which forces a CORS preflight that is never approved) and JSON only.
* **Untrusted text in the admin UI.** Reporter text and contact, and the
  file names and hosts in system findings, are cleaned on input and shown
  as React text nodes only: controls, invisible characters and the
  bidirectional embedding, override and isolate controls are removed, so a
  name cannot render with a misleading order. The contact is never a
  `mailto:` or other link.
  The reported URL is a link only because it passed the content-domain
  check on input; it opens with `rel="noopener noreferrer"` in a new tab.
* **Untrusted text in the webhook.** None is sent: category, artifact ID
  and owner username only, all of them from server-controlled alphabets.
* **Hostile artifacts.** An artifact can draw a fake "report" link that
  goes elsewhere, as it can fake anything inside itself; the real links
  live outside the frame. It can't reach the form (other site, no framing).
* **Scanner input.** The HTML tokenizer and magic checks read bounded
  prefixes of stored blobs under a deadline; they never execute, render or
  fetch anything.
* **Privacy.** No IP address is stored; the duplicate key is a keyed hash,
  removed on close. The reporter's contact is optional.

### Operator notes

* Register the content domain (the apex and its subdomains) in Google
  Search Console, so Safe Browsing warnings for any artifact reach you
  before visitors see them. Do the same for the app origin.
* Set `REDROP_ABUSE_CONTACT` to an address someone reads, and consider
  `REDROP_NOTIFY_URL` so new reports don't wait for the dashboard.

## Live reload

While the owner runs `redrop watch`, viewers of the current version see each
new version without reloading by hand. It answers the rest of issue #15. The
issue's "injected script" is replaced by the principle above: nothing is added
to the artifact. redrop's own viewer page watches for a new current version
and reloads its frame.

### Decisions

* **Opt-in and temporary.** Live mode is on for an artifact only while an
  owner's `redrop watch` holds a **live lease** on it. `--live` is the
  default for `watch`; `--no-live` turns it off. The CLI renews the lease
  while it runs and releases it when it stops; a watcher that dies lets it
  lapse.
* **Viewer page only.** Live reload needs redrop's viewer page: an `html` or
  `bundle` artifact with the info button on, showing the current version.
  * `--no-info` and `?redrop-info=0` (direct views) get no live reload:
    there is no redrop page to do it.
  * `file` artifacts get no live reload: their page is redrop's file page,
    with no frame to reload, and reloading the page itself would be a new
    top-level load.
  * A viewer showing an older version (`?redrop-version=N`) is not live.
  * The server refuses a lease (`409`) for an artifact with the info button
    off or a `file` current version. `watch` reports live reload off and
    retries at the lease renewal interval, so enabling the info button or
    publishing a framed version can restore it without restarting watch.
* **When it is off, viewers make no extra requests.** The viewer page
  includes the live element only while a lease is active. A page whose
  stream ends is told so (`end`) and stops; it does not poll.
* **Views.** Reloading never uses a view, so live reload also works on
  view-limited and `--once` links:
  * the stream request is not a page load and never records a view;
  * the frame reload is a frame navigation (`Sec-Fetch-Dest: iframe`), which
    never counts (see "Gates and view counting");
  * gated links count only at Open, and a reload never presses Open. A visit
    grant that ran out while the page was open shows the gate in the frame,
    exactly as following a link in the frame would.

### Lease

* `PUT /api/v1/artifacts/{id}/live/{session}` creates or renews the lease of
  one watch session; `DELETE` on the same path releases it. Owner only (token
  or session); other users' artifacts are `404`. `session` is chosen by the
  CLI: 16 to 64 characters of `[A-Za-z0-9_-]`. The answer is
  `{"expires_at", "renew_after_seconds"}`.
* A lease lasts `REDROP_LIVE_LEASE` (default 60 s). The CLI renews it every
  third of that (20 s by default), so a missed renewal or two does not end
  it. Live mode is on while any session's lease is unexpired, so two
  watchers on one artifact don't switch each other off.
* **Bounds.** At most 8 sessions per artifact and 32 per owner across their
  artifacts (`409` beyond either), plus 10,000 leases on the server (`429`
  beyond). Renewals of existing sessions still work at capacity. Expired
  leases are pruned by the maintenance sweep, when a lease on the same
  artifact changes, and before refusing an owner or server whose share is
  full. One account cannot occupy every server lease slot.
* A `409` reports `live_off` once per consecutive conflict run and retries
  at the last successful renewal interval, 20 s before the first success.
  A later success reports `live_on`. Network failures, `5xx` and `429` keep
  their bounded retries; authentication and not-found errors stop live
  renewal without failing the file watcher.
* Leases live in the server's memory, not the database: there is no
  migration, and a restart drops them. At default settings the CLI attempts
  renewal every 20 s, or after 5 s following a transient failure, subject
  to `Retry-After`. Viewer restart recovery is best effort, not an update
  guarantee: graceful shutdown requests one reconnect after 25 s. An abrupt
  drop, a longer outage, or reconnecting before the lease is restored can
  require manually reloading the viewer. Rolling back removes the feature.

### Stream

`GET /__redrop/live` on the artifact's own host is a server-sent events
stream.

* **Access.** The request passes the same decision as the content
  (`sharing.Decide`: not expired, revoked or used up, password entered,
  visit grant valid; an owner preview counts), live mode must be on, and the
  current version must be framed. `Sec-Fetch-Site`, when present, must be
  `same-origin`. Every failure is **the identical miss**: byte for byte the
  answer to an unknown `/__redrop/` path, spending one token of miss budget.
  So with live mode off, the endpoint can't be told apart from an unknown
  path, and a visitor without access learns nothing. No response carries
  `Access-Control-*` headers.
* **Events.**
  * On connect: `retry: 25000` and a `version` event with the current
    version, so a page that missed an update between its load and the
    connection catches up. The viewer closes every failed EventSource;
    this advertised retry does not authorize automatic error reconnects.
  * `version` with `{"version", "published", "size", "files"}` whenever the
    current version changes (a publish or a rollback). These are the
    shown-version facts the info panel already gives a viewer.
  * `end` when live mode stops, access ends, or the link is revoked, rotated
    or deleted. The page closes the stream and makes no more requests.
  * A comment line (`: keep-alive`) every `REDROP_LIVE_KEEPALIVE` (default
    20 s, at most 29 s), because Cloudflare closes connections idle for about
    100 s.
* **Rechecks.** The stream re-reads the artifact and re-runs the access and
  live checks on every keep-alive and on every change to the artifact
  (publish, rollback, settings, revoke, rotate, delete, lease release).
  Revocation, a password change and a released lease end the stream at once;
  an expired visit grant or a lapsed lease within one keep-alive interval.
* **Notification.** The sharing service keeps, per artifact, the set of open
  streams. A change only "pokes" each stream through a one-slot channel and
  never blocks: pokes coalesce, and each stream reads the artifact itself.
* **Bounds.** At most `REDROP_LIVE_MAX_STREAMS` (default 1000) streams on
  the server, `REDROP_LIVE_MAX_STREAMS_PER_ARTIFACT` (default 100) per
  artifact, one quarter of the server cap per artifact owner (rounded down,
  minimum 1; default 250), and 16 per client network (keyed like the miss
  limiter, so a team behind one address still fits). Beyond any limit the
  answer is `503` with `Retry-After`; the page shows no live indicator.
  Each write has a 10 s deadline, retained through the terminating chunk;
  HTTP/1.1 streams close their connection rather than reuse its deadline.
  A reader is dropped only once writes block after socket buffers fill.
  With little traffic, a non-reading client can occupy a slot indefinitely,
  like an idle tab. The admission caps bound those clients. A stream holds
  about one goroutine and a few KB in the process, plus kernel send buffers
  that can remain allocated after close until TCP gives up.
* **Shutdown and reconnects.** Graceful shutdown sends
  `end` with `{"reconnect":true}` before closing. The viewer closes that
  EventSource, hides the indicator and makes one attempt after 25 s. Every
  `error` closes the source without retry, including an error on that one
  attempt. Terminal `end` events never retry. This prevents tabs behind a
  shared network from spending its miss budget on automatic reconnects
  when leases have disappeared. A refused initial or explicitly requested
  connection still spends one token, preserving the identical miss.

### Viewer side

* The viewer page renders a hidden "Live" row in the info panel
  (`#rd-live`, with the stream path and the shown version) only while live
  mode is on, and its CSP adds `connect-src <origin>/__redrop/live`.
  `viewer.js` opens an `EventSource`; the row shows while the stream is
  open and hides when it ends or fails for good.
* **Reload.** On a `version` event for a version other than the one shown,
  `viewer.js` calls `location.reload()` on the frame's window, so the frame
  keeps its path, query and fragment and adds no history entry. It skips the
  reload when the frame is on another origin or on a `/__redrop/` page (an
  older version's files). Events that arrive during a reload trigger one
  more reload once it finishes. A 30 s timeout clears pending reload state
  without making a request, so a cancelled `beforeunload` prompt cannot
  suppress every later version. A later distinct version retries the reload;
  a real load exceeding 30 s may lose saved scroll and trailing state.
* **Scroll.** Before reloading it reads the frame's scroll position. After
  the reload, if the browser left the page at the top, it scrolls the frame
  back. Reading the scroll position, `location.reload()` and `scrollTo()` are
  the only things the viewer does with the artifact's window beyond what
  "Address bar" lists; nothing is written to its document.
* **Panel.** The panel's version line, size, "current" mark and the feedback
  form's version follow the event, so comments are filed against the version
  on screen. The version list is otherwise as it was when the page loaded; a
  version published live appears as a new row without a note.

## Releases and versions

Versions are stamped at build time with `-ldflags` into both binaries: the
version (`vX.Y.Z` for a tag, `0.0.0-main+<sha>` for a `main` build, `dev`
for a local `go build`), the abbreviated commit, and the commit time. The
server reports them at `GET /api/v1/version`, which is public like
`/api/v1/auth/config` and also carries `min_cli_version`, the oldest release
CLI the server still supports. `/api/v1/auth/config` keeps its `version`
field, and `/healthz` keeps its plain `ok` body.

Only plain releases (`vX.Y.Z`) are compared. `redrop version` warns on
stderr when a release CLI is below the server's minimum. A `main` build,
which is what a server's `/install.sh` serves for a `main` deployment and
matches that server, never warns; nor do `dev` and preview builds,
prereleases, or an unparseable minimum.

A `v*` tag runs `ci.yml` (see `docs/RELEASING.md`). After the `test` job
passes, the `image` job publishes the `vX.Y.Z` and `sha-<commit>` image tags
as before, and the release jobs build the six CLI binaries and `SHA256SUMS`,
attest their provenance, and publish a GitHub Release with notes generated
from PR titles. Tags with a `-` are prereleases, so they never become the
"latest" release. Nothing release-related is built locally.

`redrop self-update [--check] [--version vX.Y.Z]` downloads the binary for
the current OS/architecture and `SHA256SUMS` from the release's asset list,
over HTTPS only (loopback excepted), redirects included, and replaces the running binary with a
rename in the same directory. The download streams through a SHA-256 hasher
into a synced staging file with the old binary's mode; on Windows the running
binary is renamed aside first. The old binary is never touched before the
checksum matches. Size caps (64 KiB for `SHA256SUMS`, 128 MiB for the binary)
bound a hostile release. self-update refuses system and package-manager
trees (`/usr/bin`, `/bin`, Homebrew's Cellar, Linuxbrew, the Nix store) and
unwritable directories before downloading. A `main` build is replaced only
with an explicit `--version`, since "latest" may be older than `main`;
`--version` to an older release is allowed (rollback) with a warning.
`--check` reports `update_available`.

The checksum protects integrity, not authenticity: both files come from the
same release, so authenticity rests on HTTPS to GitHub and on GitHub account
security. `gh attestation verify` gives an independent provenance check, and
a pinned signing key for `SHA256SUMS` is a planned follow-up.

`CHANGELOG.md` is kept in Keep-a-Changelog format with an Unreleased
section; release notes are generated from pull-request titles.
