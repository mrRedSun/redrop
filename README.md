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
- Artifacts can't register service workers (a worker would outlive
  revocation). Everything else behaves as a normal web page on its own origin.

## Read first

- [`docs/DESIGN.md`](docs/DESIGN.md): product and security decisions.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): module map, data flow, and how
  to add a feature.

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
`index.html` and does not support `--spa`. Ordinary and Markdown bundles use the
same packer and follow relative symlinks only within the selected directory.
Outside-root, absolute, and cyclic symlinks fail with usage errors. Broken asset
symlinks and other non-regular bundle entries are skipped with stderr warnings.
Explicitly named single files follow normal filesystem symlinks, including links
into other directories; only directory walks and `--entry` are root-confined.
Selected sources must resolve to regular files. Unix source and entry opens use
nonblocking mode and re-check the descriptor to reject a swapped-in FIFO.
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

### Watch for changes

`redrop watch ID|URL PATH` republishes the artifact as a new version
whenever the file or folder changes, debounced and deduplicated by content
hash. It prints each version as it publishes; `--json` streams publish and
typed notice events. Ctrl-C stops it. It takes the same content options as
`update`, so a watched `.md` file is re-rendered on every change. Directory
uploads use the skip rule in `redrop guide` under Share, also documented in
[`docs/DESIGN.md`](docs/DESIGN.md#directory-uploads).

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
| `REDROP_TRUSTED_PROXIES`, `REDROP_CLIENT_IP_HEADER` | client IPs behind a reverse proxy |
| `REDROP_INFO_BUTTON_DEFAULT` | `true` (default): show the info button (and the viewer frame for HTML) on artifacts without their own setting |
| `REDROP_CONTENT_MISS_BURST`, `REDROP_CONTENT_MISS_PER_MINUTE` | `30`, `5`: unknown or unavailable links a client may request (burst, then per minute) before getting `429` |

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
Set `REDROP_TRUSTED_PROXIES` to the proxy's addresses and
`REDROP_CLIENT_IP_HEADER` to the header it sets (`X-Forwarded-For` for Traefik,
`CF-Connecting-IP` behind Cloudflare) so per-client limits see real clients.

Status: in development, not yet released.

## License

[MIT](LICENSE).
