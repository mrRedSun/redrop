# redrop

Self-hosted sharing for agent-produced artifacts: HTML pages, files, and static
sites behind private links that can expire, need a password, or work only once.
A Go server (Fuego), a React dashboard (Effect v4 + shadcn/ui), and a
cross-platform CLI that agents drive.

- Every artifact is served from its own origin (`https://<slug>.<content-domain>/`)
  on a domain separate from the dashboard, so shared HTML/JS is isolated.
- Links are four random words (`https://amber-otter-quilt-river.<content-domain>/`),
  unguessable, never indexed, and send no Referer.
- Optional password and view limit. One-time links show an "Open" page first, so
  chat link previews don't use them up. Same-link versions with rollback.
- Multi-user and invite-only; built-in login with TOTP, plus OIDC single sign-on.
- Shared pages get a small info button showing viewers the version history,
  expiry countdown, and remaining views, never who shared them. HTML opens in
  redrop's viewer, with the artifact untouched in a frame on the same origin,
  and the address bar follows the frame. Turn it off per artifact (`--no-info`,
  or in the dashboard), or with `?redrop-info=0` (remembered for 30 minutes
  when the link is opened directly; when it's clicked on another website,
  only that page is direct). Clients without Fetch Metadata (curl, very old
  browsers) always get the artifact directly. Owners can let viewers open
  older versions (`--old-versions`); inside an older version, a root-absolute
  link (`/x.html`) leads to the current version. Change notes on new versions are shown
  to viewers; notes written before this release stay private.
- Optional viewer feedback: a comment form in the info panel that agents read
  back with `redrop feedback` (see [Feedback](#feedback)). Off by default.
- Optional end-to-end encryption (`--e2e`): the CLI encrypts before upload
  and the key travels in the link's `#k=` fragment, so the server stores only
  ciphertext (see [End-to-end encryption](#end-to-end-encryption)).
- Artifacts can't register service workers (a worker would outlive
  revocation). Everything else behaves as a normal web page on its own origin.
- `redrop tunnel 3000` shares an app still running on your computer (a dev
  server with hot reload, an API, a webhook receiver) through the same kind of
  private link, for as long as the command runs (see [Tunnels](#tunnels)).

## Read first

- [`docs/DESIGN.md`](docs/DESIGN.md): product and security decisions.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): module map, data flow, and how
  to add a feature.
- [`CHANGELOG.md`](CHANGELOG.md) and [`docs/RELEASING.md`](docs/RELEASING.md):
  what has landed and how a release is cut.

## Layout

| Path | What |
|---|---|
| `cmd/redrop-server` | Server entrypoint and admin subcommands (`invite`, `reset`, `openapi`) |
| `cmd/redrop` | CLI (`redrop share`, `update`, `list`, `guide`, ...) |
| `pkg/api`, `pkg/client` | Wire types and the Go client library used by the CLI |
| `internal/accounts`, `internal/sharing` | Services holding the business rules |
| `internal/httpapi`, `internal/content` | HTTP transports for the API and artifact domains |
| `internal/tunnel` | Live tunnel connections: the registry, the yamux sessions, and the reverse proxy |
| `internal/store`, `internal/blob` | SQLite persistence and content-addressed file storage |
| `web/` | Dashboard, built into `internal/webui/dist` and embedded in the server |
| `scripts/` | Dev server and end-to-end test |

## CLI

When you create an API token on the dashboard's Tokens page, it shows a
one-line command that installs the CLI and logs it in with that token:

```sh
# macOS / Linux
curl -fsSL https://redrop.example.com/install.sh | REDROP_TOKEN='rdp_…' sh
# Windows (PowerShell)
$env:REDROP_TOKEN='rdp_…'; irm https://redrop.example.com/install.ps1 | iex
```

The command contains the token, so it ends up in your shell history. To avoid
that, run the installer without `REDROP_TOKEN`, then
`redrop login --url https://redrop.example.com` and paste the token at the
prompt. `redrop guide` explains everything else.

### Share Markdown

```sh
redrop share notes.md                              # styled document + original download
redrop share notes.txt --render markdown            # force rendering for another extension
redrop share notes.md --render none                 # raw file, as before
redrop share ./report --render markdown --entry docs/notes.md
cat notes.md | redrop share - --name notes.md
```

The CLI renders `.md` files locally by default. The version is a bundle with a
self-contained `index.html` and the byte-for-byte original. `update` accepts the
same options. `--render auto` respects explicit `--as` choices, so `--as file`
also keeps Markdown raw. No API or content-server rendering is involved.

Pages have heading links, a table of contents at four headings, offline syntax
highlighting and copy buttons, tables, task lists, blockquotes, and responsive
light/dark styles. Share a directory with `--render markdown --entry PATH` to
include images and links relative to the selected Markdown file. A single file
never uploads its siblings. Directory rendering refuses an existing root
`index.html` and does not support `--spa`. Markdown folders and `--entry` follow
the [folder rules](#folder-uploads).
Unpack archives before rendering.
Missing local references in a single-file upload produce a stderr warning and a
directory-publishing suggestion. Rejected image sources produce warnings for
both single files and directories, naming the source and the local-image rule.

HTML comments are omitted. Other raw HTML is displayed as escaped inline text
or normal paragraphs, not Copy-enabled code blocks. Prose such as `List<String>`
survives without becoming markup. Unsafe attributes and active URL schemes are removed.
HTTP/HTTPS and mailto links open with no opener or referrer; images stay local-only.
A restrictive CSP prevents automatic external resource requests. Generated HTML
has no branding, favicon, hosting-policy meta tags, or special download queries.
The original download and other bundle files remain unmodified and untrusted. Markdown sources are limited
to 8 MiB. Code blocks larger than 64 KiB and unknown languages stay plain text.
Use `--render none` for larger sources.

### Folder uploads

`share`, `update` and `watch` pack plain and Markdown folders the same way.
They skip VCS and dependency directories, OS metadata, editor temp files, and
conventional secret paths at any depth:

- `.env` and `.env.*`, except `.env.example`, `.env.sample`, and `.env.template`.
- `.ssh/`, `.aws/`, `.gnupg/`, `.docker/config.json`, and `.kube/config`.
- `.netrc`, `.npmrc`, `.pypirc`, `.git-credentials`, and `.pgpass`.
- `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`, `*.key`, `*.p12`, and `*.pfx`.
  Public keys such as `id_ed25519.pub` remain included.

Each excluded secret entry prints a stderr warning with its relative path,
never its contents. Excluded directories are named without traversing them.
`--include-secrets` on `share`, `update`, or `watch` opts these paths back in,
including a secret Markdown `--entry`; editor and VCS filters still apply.
File-name matching is case-insensitive; directory names match exactly. Other
useful hidden files remain included. Explicitly named single files still publish
as requested, with unchanged source bytes and a warning if their path or name
matches a secret rule. Selecting a secret folder itself, or a folder inside one
(`redrop share ~/.aws`), is refused unless `--include-secrets` is given. `watch`
prints each secret warning once per path. Supplied ZIP/tar.gz archives are not
secret-filtered.

Use a dedicated output folder containing only files intended for recipients.
Name-based exclusions cannot identify every secret, and they do not remove files
from versions already published. The full skip rule is in `redrop guide` under
Share and in [`docs/DESIGN.md`](docs/DESIGN.md#directory-uploads).
Relative symlinks are
followed only within the selected folder. Outside-root, absolute, and cyclic
symlinks fail with usage errors. Broken symlinks and other non-regular entries
are skipped with stderr warnings. A `PATH` that is itself a symlink is resolved
once and then walked as that folder. Explicitly named single files follow normal
filesystem symlinks, including links into other directories. Selected sources
must resolve to regular files. Unix source and entry opens use nonblocking mode
and re-check the descriptor to reject a swapped-in FIFO.

### Share and update options

Besides the content options above, `share` and `update` take the viewer
settings (`set` takes them too):

| Flag | Effect |
|---|---|
| `--info` / `--no-info` | Show or hide the info button; HTML opens in the viewer frame only while it is shown |
| `--old-versions` / `--no-old-versions` | Let viewers open older versions from the info panel (default off) |
| `--feedback` / `--no-feedback` | Let viewers leave comments from the info panel (default off; needs the info button) |
| `--e2e` | End-to-end encrypt (`share`, `update`, `watch`; see [End-to-end encryption](#end-to-end-encryption)) |

On `update`, these are saved before the upload starts.

### Feedback

```sh
redrop share report.html --feedback        # or: redrop set ID --feedback
redrop feedback ID                         # open comments; --all, --version N, --json
redrop feedback resolve ID COMMENT...      # done with these
redrop feedback delete ID COMMENT... --yes
```

With feedback on, the info panel holds a comment form. Viewers can select
text on the page before opening the panel to quote it. Nothing is added to
the artifact: the form is on redrop's viewer page, outside the frame. Anyone
who can view the artifact can comment (after the password, or within a
one-time link's visit), and commenting never uses a view. Only the owner
reads comments, in the dashboard's Feedback tab or with the CLI; admins see
only whether feedback is on.

Each comment records the version and page on screen, the quoted text, an
optional name, and the time; no IP address or user agent. Comments are
untrusted text: the CLI frames them as viewer-supplied and strips terminal
escapes, and `redrop guide` tells agents never to follow instructions in
them. Per-client rate limits and a per-artifact cap (500) bound spam; see
[`docs/DESIGN.md`](docs/DESIGN.md#viewer-feedback).

### End-to-end encryption

```sh
redrop share report.pdf --e2e      # url: https://redrop.example.com/e/amber-otter-quilt-river#k=…
redrop update ID report.pdf        # same link, encrypted with the key saved on this machine
redrop watch ID ./site
redrop update - report.pdf < link.txt   # elsewhere: the full link on stdin, not in argv
```

`--e2e` encrypts files, folders and rendered Markdown on your machine
(AES-256-GCM). The key is the part of the link after `#k=`, which browsers
never send to a server. The server stores only ciphertext, plus an
encrypted manifest instead of file names. Someone who reads its disk or a
backup learns sizes, file counts and timing, never content, names or titles.

- **The link is the secret.** Anyone with the full link can read every
  version, past and future: the key never changes, not even on `rotate`.
  A lost link means lost content: `share --e2e` keeps a copy of the key in
  `e2e-keys.json` next to the CLI config (0600). The dashboard and `list`
  show links without the key. Pass a full link on stdin (`-` as `ID|URL`),
  not as an argument, so it stays out of shell history and process lists.
- **Plaintext never reaches the server.** `update` and `watch` check the
  artifact first and encrypt every new version with its key, with or
  without `--e2e`. Without a key (from the store or the link) they upload
  nothing and exit 2, and the server refuses a plaintext upload declared
  for an encrypted artifact before any byte of it arrives. Upgrade the CLI
  on every machine you publish from: an older CLI's `update` still
  uploads the plaintext before the server refuses the version.
- **Still works:** passwords, expiry, view limits and `--once` (the key is
  kept before the gate), revoke, rotate, versions, the info panel, and
  feedback.
  Feedback is **not** encrypted: comments, quotes and page paths reach the
  server in plaintext.
- **Stops working:** `--spa`, file listings, the dashboard preview and file
  names, live reload, the server reading `<title>` (pass `--title` to name it in the
  dashboard; titles and notes are plaintext), the artifact's own favicon,
  and seeking in videos over 64 MiB.
- **Viewing** needs HTTPS, and the server's own address must be reachable
  by everyone the link is for: the link opens a small page there first.
  That page takes the key off the address bar, turns it into a
  non-extractable `CryptoKey` and, when the visitor presses **Open**, hands
  only that to the artifact's page through a small window that closes by
  itself. The key's text never reaches the artifact's origin, so scripts
  in the page, or windows they open, can't read the key itself. They can
  derive and send out object keys that decrypt those objects later and can
  forge objects for this artifact if those objects are ever stored on the
  server. A script that ran with the key is a reason to share a new artifact;
  neither `rotate` nor `update` changes the key. redrop's viewer decrypts in the
  browser through redrop's own service worker. The browser keeps the key
  for 12 hours, so reloads work; its history keeps the full link. A link
  whose key is damaged, encoded or cased differently, or whose `#` became
  `%23` or `?`, opens nothing. Viewing also needs a browser with the
  Navigation API (current Chrome, Edge, Firefox and Safari) and pop-ups
  allowed for the click.
- **Limit:** this protects stored data, not visits through a compromised
  live server, which could serve a viewer that leaks the key. Design and
  format: [`docs/DESIGN.md`](docs/DESIGN.md#end-to-end-encrypted-links).

### Watch for changes

`redrop watch ID|URL PATH` republishes the artifact as a new version
whenever the file or folder changes, debounced and deduplicated by content
hash. It prints each version as it publishes; `--json` streams publish and
typed notice events. Ctrl-C stops it. It takes `update`'s content options
(`--render`, `--entry`, `--as`, `--spa`, `--note`), so a watched `.md` file is
re-rendered on every change and `--render none` keeps it raw. Folders follow
the [folder rules](#folder-uploads).

**Live reload** is on while `watch` runs (`--no-live` turns it off). People
who have the link open in redrop's viewer page see each new version without
reloading: the viewer page reloads its frame, keeping the page and scroll
position, and the info panel shows a "Live" row. Nothing is added to the
artifact. It needs the info button and an HTML page or folder: `--no-info`
links, direct views (`?redrop-info=0`) and file artifacts are not reloaded.
Reloads never use a view, so view-limited and `--once` links work too. Live
mode ends within a minute after `watch` stops or dies; while it is off,
viewer pages make no extra requests. A temporary refusal (the info button
off, or all sessions in use) is retried at the renewal interval, so fixing
the cause restores live reload without restarting `watch`. See
[`docs/DESIGN.md`](docs/DESIGN.md#live-reload).

### Tunnels

```sh
redrop tunnel 3000                                  # http://localhost:3000 -> a private link
redrop tunnel 5173 --password-generate --ttl 2h     # Vite, with hot reload
redrop tunnel 127.0.0.1:8080 --title "API" --log    # one stderr line per request
redrop tunnel 5173 --id k2j4m9qx7d4a                # the same link as last time
```

A tunnel is an artifact of kind `tunnel` with its own four-word link. Requests
to the link travel over one WebSocket that `redrop tunnel` keeps open to the
server, to the app on your computer, and back; the server never connects to
your computer. Ctrl-C ends it. While it isn't connected, visitors who get past
the password see "This tunnel is offline", and `redrop list` and the dashboard
show it offline. It reconnects by itself after network trouble; `--id` keeps
the link across restarts.

The link has the artifact protections: expiry, password, revoke, the info
panel with the viewer frame (`--no-info` serves the app directly), and admin
moderation. A view limit and `--once` don't apply. WebSockets work end to end,
so dev-server hot reload does. Nothing is added to the app's pages.

- **Only this computer by default.** A port means `localhost` (`127.0.0.1`,
  then `::1`). Another address needs `--allow-remote-target` and prints a
  warning: everyone with the link can reach it through your computer. The
  target is resolved once and pinned.
- **What the app sees:** `Host` is the target you named (`localhost:5173`, so
  Vite's host check passes); `X-Forwarded-Host` is the link's host,
  `X-Forwarded-For` the visitor's full address as the server resolved it,
  and `X-Forwarded-Proto` the link's scheme. Other forwarding and client-IP
  headers (`Forwarded`, `Via`, `X-Real-Ip`, `Proxy-Client-Ip` and the like),
  URL and host overrides (`X-Original-Url`, `X-Host`, ...), and header names
  with an underscore, are removed. redrop's own cookies are never sent to the
  app, and a `Domain` on the app's cookies is removed, as is a
  `Clear-Site-Data` from the app. `1xx` responses from the app (such as
  `103 Early Hints`) and response trailers are not passed on.
- **Limits** (server settings, below): 3 connected and 20 kept tunnels per
  user, 32 open requests per tunnel, 100 MB requests, 1 GB responses, 2 GB per
  user per hour over all their tunnels, and about 90 seconds for the app to
  start answering. A visitor who stops reading is cut after the idle
  timeout. A request stuck behind an app that hasn't read its upload is
  reset on its own after the idle timeout; the tunnel stays connected.

The server keeps connected tunnels in memory: run one redrop process. See
[`docs/DESIGN.md`](docs/DESIGN.md#tunnels) for the protocol and threat model.

## Develop

Requires Go 1.26 and Node 24.

```sh
cd web && npm ci && npm run build && cd ..   # dashboard into the embed dir
go test ./...
./scripts/dev-server.sh                        # LAN dev server (see the script header)
```

On first start the server logs a one-time setup link for the admin account.
`scripts/e2e.sh` exercises the server and CLI end to end against the dev server.

Frontend checks: `cd web && npm run typecheck && npm run lint && npm test`.
`npm run check:contract` verifies the Effect schemas against a running server.

## Configure

All settings are `REDROP_*` environment variables, loaded in
[`internal/config/config.go`](internal/config/config.go). The essentials:

| Variable | Example |
|---|---|
| `REDROP_PUBLIC_URL` | `https://redrop.example.com` (dashboard and API) |
| `REDROP_CONTENT_URL` | `https://usercontent.example` (a separate registrable domain) |
| `REDROP_DATA_DIR` | `/data` |
| `REDROP_OIDC_PROVIDERS`, `REDROP_OIDC_<ID>_ISSUER`, `_CLIENT_ID`, `_CLIENT_SECRET_FILE` | optional single sign-on |
| `REDROP_TRUSTED_PROXIES`, `REDROP_CLIENT_IP_HEADER`, `REDROP_TRUSTED_HOPS` | client IPs behind a reverse proxy (see [Client addresses](#client-addresses)) |
| `REDROP_INFO_BUTTON_DEFAULT` | `true` (default): show the info button (and the viewer frame for HTML) on artifacts without their own setting |
| `REDROP_API_DOCS` | `false` (default): `/api/docs`, the interactive API page, is off. It loads a third-party script without a pinned version and outside the dashboard's CSP, so enable it only on development servers. `/api/openapi.json` is always served |
| `REDROP_CONTENT_MISS_BURST`, `REDROP_CONTENT_MISS_PER_MINUTE` | `30`, `5`: unknown or unavailable links a client may request (burst, then per minute) before getting `429` |
| `REDROP_FEEDBACK_BURST`, `REDROP_FEEDBACK_PER_MINUTE` | `5`, `1`: viewer comments a client may post (burst, then per minute) |
| `REDROP_FEEDBACK_MAX_PER_ARTIFACT` | `500`: comments one artifact keeps, open or resolved |
| `REDROP_ABUSE_CONTACT` | unset (default): no `security.txt`. An email address, `mailto:` URI or `https://` URL; serves `/.well-known/security.txt` on the dashboard host and the content apex (see [Abuse handling](#abuse-handling)) |
| `REDROP_REPORTS_PER_HOUR` | `5`: abuse reports one client network may file per hour |
| `REDROP_REPORTS_MAX_OPEN`, `REDROP_REPORTS_MAX_OPEN_PER_ARTIFACT` | `1000`, `20`: open visitor reports in all, and on one artifact; past them reports are refused or dropped |
| `REDROP_NOTIFY_URL`, `REDROP_NOTIFY_TOKEN_FILE` | unset (default): no notifications. A webhook that gets a JSON POST for every new report and upload flag, with the file's token as `Authorization: Bearer` |
| `REDROP_LIVE_LEASE` | `60s` (15s to 10m): how long a `redrop watch` session's live reload lease lasts; the CLI renews it every third of that |
| `REDROP_LIVE_KEEPALIVE` | `20s` (1s to 29s): keep-alive interval of live reload streams; keep it under your proxies' idle timeout (Cloudflare: about 100 s) |
| `REDROP_LIVE_MAX_STREAMS`, `REDROP_LIVE_MAX_STREAMS_PER_ARTIFACT` | `1000`, `100`: open live reload streams on the server and per artifact (plus one quarter of the server cap per artifact owner, 32 watch leases per owner, and 16 per client network); beyond them viewers get no live reload |
| `REDROP_TUNNELS_PER_USER` | `3`: tunnels one user may have connected; `0` turns tunnels off |
| `REDROP_TUNNEL_ARTIFACTS_PER_USER` | `20`: tunnels one user may keep, connected or not, until deleted |
| `REDROP_TUNNEL_STREAMS`, `REDROP_TUNNEL_STREAMS_TOTAL` | `32`, `256`: open requests and WebSockets per tunnel, and across the server (each holds at most about 300 KB) |
| `REDROP_TUNNEL_MAX_REQUEST_SIZE`, `REDROP_TUNNEL_MAX_RESPONSE_SIZE` | `100MB`, `1GB`: one request or response body through a tunnel |
| `REDROP_TUNNEL_BYTES_PER_HOUR` | `2GB`: traffic per user, over all their tunnels, both ways, per hour |
| `REDROP_TUNNEL_RESPONSE_TIMEOUT`, `REDROP_TUNNEL_IDLE_TIMEOUT` | `90s`, `100s`: how long a request waits for a free slot, and then for the app to start answering; a request or WebSocket that moves no bytes is closed after the idle time |

## Email

Mail is off unless `REDROP_SMTP_ADDR` is set. Manual invite links and the
webhook keep working without it. With mail configured, **Administration →
Invites** has an optional email field, delivery status, and **Send again**.
The recipient sees the invite's note, inviter, and expiry. The emailed link
is the same one-time link the administrator can copy.

| Variable | Default / meaning |
|---|---|
| `REDROP_SMTP_ADDR` | Unset, mail off. Relay `host:port`, for example `smtp.example.com:587` |
| `REDROP_SMTP_TLS` | `starttls`, required and never downgraded. `implicit` uses TLS from connection start, typically on port 465 |
| `REDROP_SMTP_USERNAME` | Optional relay login; needs a password file |
| `REDROP_SMTP_PASSWORD_FILE` | Path to a non-empty single-line password file. No direct password environment variable |
| `REDROP_SMTP_CA_FILE` | Optional PEM file containing a private CA. Extends system trust; hostname and certificate verification remain enabled |
| `REDROP_MAIL_FROM` | Required with SMTP. Bare address or display name, such as `Redrop <share@example.com>` |
| `REDROP_NOTIFY_EMAIL` | Comma-separated report-alert recipients, up to 16 distinct bare addresses. Ignored while mail is off |
| `REDROP_NOTIFY_EMAIL_INTERVAL` | `10m`, between `1s` and `24h`. Minimum interval per alert recipient, including delivery retries |
| `REDROP_SMTP_INSECURE_ALLOW_PLAIN` | `false`. Allows `REDROP_SMTP_TLS=plain` only for disposable test relays; never use in production |

Mount a private CA and password file read-only in your Docker Compose service.
For example, add these settings and mounts to the existing service:

```yaml
environment:
  REDROP_SMTP_ADDR: smtp.example.com:587
  REDROP_SMTP_TLS: starttls
  REDROP_SMTP_CA_FILE: /run/secrets/smtp-ca.pem
  REDROP_SMTP_USERNAME: redrop
  REDROP_SMTP_PASSWORD_FILE: /run/secrets/smtp-password
  REDROP_MAIL_FROM: 'Redrop <share@example.com>'
  REDROP_NOTIFY_EMAIL: ops@example.com
volumes:
  - ./smtp-ca.pem:/run/secrets/smtp-ca.pem:ro
  - ./smtp-password:/run/secrets/smtp-password:ro
```

Make the password readable only by the service identity. Do not commit it or
bake it into an image. Restart with `docker compose up -d`, then test the relay
without touching the application database:

```sh
docker compose exec redrop redrop-server mail-test --to ops@example.com
```

The command reports SMTP acceptance or a safe stage/status failure. Acceptance
is not proof of final inbox delivery. The operator owns relay reachability,
certificate renewal, sender-domain authentication, and relay-side delivery logs.
Use `docker compose logs redrop` for queue outcomes and the Invites page for
recipient-specific failures. SMTP replies, credentials, recipient addresses,
and one-time links do not appear in redrop's delivery logs.

`redrop-server invite --email recipient@example.com --note 'Welcome'` works
offline and queues the email. The serving process sends it on its next queue
check, within one minute. It does not start another worker against the live
database. Admin sends are limited to 10 per hour per administrator and 60 per
hour globally, across restarts and resends. The queue accepts at most 128
pending messages; refusals leave no orphan invite.

Report alerts batch new visitor reports and upload flags, listing report IDs,
categories, and times with a link to the moderation queue. They never contain
artifact content, reporter text, contact information, or reported URLs. The
first digest waits one interval; reports predating the first configuration of
an address are not mailed. Large backlogs drain in batches of 200 without
skipping reports. The existing webhook still fires immediately.

Delivery uses a 30-second timeout and at most four attempts. Transient failures
retry after 1, 4, and 16 seconds. Alert retries wait at least the configured
interval. Permanent SMTP failures are not retried automatically. A permanent
recipient refusal also suppresses later sends to that address; authentication,
sender, and relay policy failures do not. Suppression does not process later
asynchronous delivery-status notifications. Fix the address and create a new
invite, or investigate the relay before changing persisted suppression state.

Back up the full data directory, including `secret.key`, before upgrading.
Migration 10 adds mail state and encrypts stored email bodies using a key derived
from that secret. Keep it with database backups; losing it makes queued bodies
unreadable. Used or expired invite bodies are cleared by the next worker
cleanup, at most one hour later; revoking an invite removes its queued body.
Finished alert bodies are cleared immediately. Send status survives restarts.
Delivery is at least once: a process crash after SMTP accepts a message but
before SQLite records success can cause a duplicate. The one-time link still
creates only one account.

Rollback the image through the existing Docker Compose workflow. Older binaries
ignore the added column and mail tables, so links still work manually but queued
emails stop sending. Preserve the database and signing secret for a later
upgrade; do not reset volumes or downgrade the schema.

## Abuse handling

One phishing page can get the whole content domain flagged by Safe Browsing,
which breaks every link. redrop gives you the pieces to catch that early;
[DESIGN.md](docs/DESIGN.md#abuse-handling) has the details and threat model.

- **Register the content domain in [Google Search Console](https://search.google.com/search-console)**
  (a Domain property covers the apex and every artifact subdomain), and the
  dashboard's domain too. Safe Browsing then tells you about a flagged page
  before your visitors see a warning.
- **Set `REDROP_ABUSE_CONTACT`** to an address someone reads. It appears in
  `/.well-known/security.txt` on both hosts, next to the report form.
- **Reports.** Gate pages, the info panel and the content apex link to
  `/report` on the dashboard host. Anyone can report a link there, without an
  account. Reports land in **Administration → Reports**, where you can
  resolve or dismiss them, revoke the artifact (its owner can't undo that), or
  suspend the owner (the account is disabled and all of its artifacts are
  revoked). Every action is recorded in the report's history. Nothing happens
  without an administrator: a report never takes a link down by itself.
- **Upload checks.** A published page with a password form that posts to
  another site, and executables in a bundle or shared as a file, are flagged
  in the same queue. They are only flagged: the content is published and
  served unchanged.
- **Notifications.** With `REDROP_NOTIFY_URL`, each new report or flag POSTs
  `{"level":"warning","title":"…","message":"…","key":"…"}`. The message has
  the category, artifact ID and owner only, never the reporter's text.

## Reverse proxy

The dashboard host and the content domain both go to the same server. Artifact
links come in two formats: four words (`amber-otter-quilt-river`, current) and
26 base32 characters (older links, still valid). The content router must accept
both. As a regex on the example domain:

```text
^([a-z2-7]{26}|[a-z]{3,9}(-[a-z]{3,9}){3})\.usercontent\.example$
```

With Traefik v3 labels, plus the apex for the landing page and installers:

```yaml
traefik.http.routers.redrop-content.rule: >-
  Host(`usercontent.example`) ||
  HostRegexp(`^([a-z2-7]{26}|[a-z]{3,9}(-[a-z]{3,9}){3})\.usercontent\.example$`)
```

Update this rule **before** deploying a version that mints word links; a rule
that only matches the 26-character format sends new links nowhere. The content
domain needs a wildcard DNS record and certificate (`*.usercontent.example`).

Live reload streams (`/__redrop/live`, `text/event-stream`) stay open while
`redrop watch` runs. Proxies must pass them unbuffered (Traefik and Cloudflare
do; redrop also sends `X-Accel-Buffering: no`) and must not close a
connection that is idle for less than `REDROP_LIVE_KEEPALIVE`.

Tunnels need WebSockets on both hosts: `redrop tunnel` connects to
`/api/v1/tunnels/{id}/connect` on the dashboard host, and visitors' own
WebSockets (hot reload) reach tunnel links on the content domain. Traefik and
Cloudflare pass them without extra settings. The CLI and server ping every 30
seconds, under Cloudflare's 100-second idle limit.
### Client addresses

Sign-in, link lookups, artifact passwords and link misses are limited per
client. Behind a reverse proxy, redrop reads the client address from
`REDROP_CLIENT_IP_HEADER` (default `X-Forwarded-For`), but only on requests
whose socket peer is in `REDROP_TRUSTED_PROXIES`.

- **`REDROP_TRUSTED_HOPS=N`** (default `0`): the client is the
  `X-Forwarded-For` entry exactly N positions from the right, counting the
  rightmost as 1. Set N to the number of proxies that append to the header
  on the way in. Anything a client writes into the header sits further left
  and is never read. A header with fewer than N entries gives its leftmost
  entry; an entry that is not an IP address gives the socket peer. Only the
  socket peer is checked against `REDROP_TRUSTED_PROXIES`, so list just the
  proxy redrop receives requests from.
- **Cloudflare in front of Traefik**, origin reachable directly:

  ```sh
  REDROP_TRUSTED_PROXIES=192.0.2.10/32   # Traefik, as redrop sees it
  REDROP_TRUSTED_HOPS=2                  # Cloudflare appends, then Traefik
  ```

  Traefik's entry point must trust Cloudflare's published ranges
  (`forwardedHeaders.trustedIPs`) and keep `notAppendXForwardedFor` off
  (the default); Cloudflare's "Remove visitor IP headers" transform must be
  off. A visitor then resolves to their own address, every Cloudflare Worker
  to Cloudflare's Worker egress address (one bucket for all Workers), and a
  request sent to Traefik directly to its real source address, because
  Traefik replaces the header from untrusted sources.
- **`REDROP_TRUSTED_HOPS=0`** walks the header from the right and skips
  every trusted entry, which suits a reverse proxy alone. Never list a
  whole CDN's ranges for it: a CDN worker sends requests from inside those
  ranges with any `X-Forwarded-For` it likes, the walk skips the worker and
  the CDN edge, and every limit sees whatever address the worker chose. In
  this mode the server logs a warning at startup when
  `REDROP_TRUSTED_PROXIES` has a range wider than a /24 (IPv4) or a /48
  (IPv6).
- **`REDROP_CLIENT_IP_HEADER=CF-Connecting-IP`** is safe only when the
  origin is reachable through Cloudflare alone (a firewall that admits only
  Cloudflare's ranges, or Authenticated Origin Pulls); otherwise anyone who
  reaches the reverse proxy directly can send their own
  `CF-Connecting-IP`.

Status: in development, not yet released. No `vX.Y.Z` tag has been cut; the
release process ([`docs/RELEASING.md`](docs/RELEASING.md)) is ready for the
first one. Builds from `main` report `0.0.0-main+<commit>`; `redrop version`
shows the CLI and server versions and warns about a too-old CLI;
`redrop self-update --check` reports the latest release. See
[`CHANGELOG.md`](CHANGELOG.md) for what has landed.

## License

[MIT](LICENSE).
