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
- Artifacts can't register service workers (a worker would outlive
  revocation). Everything else behaves as a normal web page on its own origin.

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

### Watch for changes

`redrop watch ID|URL PATH` republishes the artifact as a new version
whenever the file or folder changes, debounced and deduplicated by content
hash. It prints each version as it publishes; `--json` streams publish and
typed notice events. Ctrl-C stops it. It takes `update`'s content options
(`--render`, `--entry`, `--as`, `--spa`, `--note`), so a watched `.md` file is
re-rendered on every change and `--render none` keeps it raw. Folders follow
the [folder rules](#folder-uploads).

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
