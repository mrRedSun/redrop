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
navigations.

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
(invite, reset and setup tokens) is written as `…`. Fuego's own access log
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
| `url` | required; an `http(s)` URL whose host is `<slug>.<content domain>` with a well-formed slug; at most 2048 bytes. Stored without its fragment |
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
   stored only when it names an existing artifact (in any state: active,
   expired within retention, revoked, used up), the artifact has fewer than
   `REDROP_REPORTS_MAX_OPEN_PER_ARTIFACT` (default 20) open visitor
   reports, and the same network has no open report on it.

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
  `.msi`, `.scr`, `.apk`, `.dmg`, `.deb`, `.bat`, `.ps1`, … see
  `sharing/scan.go`). Category `malware`. It applies to bundles and to
  single-file artifacts.

The detail names the file and, for a form, the host it posts to, for
example `login.html: a password form posts to evil.example`. That is all
an administrator learns of the content; it is cleaned and shown as text.

**Bounds.** The scan runs after the publication has released its slot and
its upload, so it never holds up other publishers, and its result is never
returned to the publisher. It reads at most 4 KiB of each file for magic
numbers, at most 1 MiB of each HTML file, at most 32 MiB in all, and stops
after 5 seconds; it stops early at the first finding of each kind. A
version that is cut short is logged (`upload check incomplete`). At most one
open system report per artifact and finding kind exists: a later version
with the same finding adds nothing until the first is closed.

**Known limits.** Forms built by scripts, `fetch()` posts and password
fields associated with a form through `form=` from outside it are not
seen. Executables inside nested archives are not opened. These checks
catch the careless case and give an operator a lead, not a guarantee.

### Admin queue

Admins see a **Reports** tab: open reports by default, with resolved and
dismissed as filters, newest first, at most 500.

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
reports`). The report shows its rows as its history, and the server logs
each action too. Rows are never edited or deleted by redrop.

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
Expires: <one year from the request, in UTC>
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
  than the content host's miss limiter.
* **Cross-site filing.** Required same-origin `Origin`, the custom header
  (which forces a CORS preflight that is never approved) and JSON only.
* **Untrusted text in the admin UI.** Reporter text and contact, and the
  file names and hosts in system findings, are cleaned on input and shown
  as React text nodes only. The contact is never a `mailto:` or other link.
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
