package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func jsonRoundTrip(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	return out, json.Unmarshal(b, &out)
}

const guide = `# redrop guide (for people and AI agents)

redrop shares artifacts through private links on a redrop server. Use it
whenever you would otherwise paste a long file, send an attachment, or need
someone to open an HTML page you generated.

## Setup (once)

Creating a token in the dashboard (Tokens page) shows a one-line command that
installs the CLI and logs in with that token:

    curl -fsSL https://SERVER/install.sh | REDROP_TOKEN='rdp_...' sh          # macOS/Linux
    $env:REDROP_TOKEN='rdp_...'; irm https://SERVER/install.ps1 | iex       # Windows

If the CLI is already installed:

    redrop login --url https://SERVER --token rdp_...   # or --token-stdin
    redrop whoami                                       # verify

Or, without saving anything: export REDROP_URL=https://SERVER REDROP_TOKEN=rdp_...
Flags --url/--token override the environment, which overrides the saved login.

## Share

    redrop share PATH [--ttl 7d] [--title T] [--once | --max-views N]
                      [--password-generate | --password-file F]
                      [--no-info] [--old-versions] [--json]

PATH decides the kind (override with --as html|file|bundle):

- *.html / *.htm  -> "html": rendered as a live web page. Inline <script> and
  CDN libraries (e.g. React/ESM from a CDN) work. Each artifact has its own
  origin, so localStorage etc. are isolated.
- *.md           -> "bundle": a styled Markdown document, with the exact
  original kept for download. Rendering happens locally before upload.
- directory      -> "bundle": a static site. index.html is the start page
  (change with --entry); without one, visitors get a file listing. Use --spa
  for client-side routers.
- any other file -> "file": a page with inline preview (image, video, audio,
  PDF, text/code) and a Download button.
- "-"            -> read stdin; --name is required (e.g. --name report.html).
  Other non-regular paths, such as a pipe from <(cmd), are rejected.
- site.zip / site.tar.gz with --as bundle -> unpacked as a static site.

Folder rules (share, update, and watch; plain and Markdown folders alike).
Folder uploads skip these entries:
- Directories .git, .svn, .hg, node_modules, __pycache__, and .DS_Store,
  including their contents.
- Files .git (a worktree or submodule link, which names a local path),
  .DS_Store, and Thumbs.db.
- Editor backups ending in ~, Emacs locks starting with .#, and Emacs
  autosaves #...# only when the name is longer than two characters.
- Hidden Vim swaps: dot-prefixed names ending in .swa to .swp, or .swx,
  such as .page.swp and .page.swo.
- JetBrains files ending in ___jb_tmp___ or ___jb_old___, and gedit files
  starting with .goutputstream-.
File-name filters are case-insensitive; directory names match exactly.
A symlink with a skipped name is skipped without being followed.
Legitimate names 4913, #, ##, non-hidden x.swp, and hidden files such as
.logo.svg, .schema.sql, .subs.srt, and .key.sig are kept. These rules apply
only inside directories: an explicitly named single file is always uploaded.
Share and update print one stderr line with the number of excluded file
entries encountered, when nonzero. Files inside excluded directory trees
are not counted. No skipped-file line is printed when the count is zero.
A folder with no files to upload fails with exit code 2.

Symlinks inside a folder: relative links that stay inside it are followed;
absolute, outside-root, and cyclic links are usage errors (exit 2). Broken
links and non-regular entries (pipes, devices) are skipped with stderr
warnings. A PATH that is itself a symlink is resolved once: a link to a
folder uploads that folder under these rules, and a link to a file uploads
the target's bytes under the link's name. Explicitly named single files
follow symlinks normally, including absolute links or links into other
directories. Sources and Markdown entries must resolve to regular files;
Unix opens use nonblocking mode and re-check the descriptor before reading.

Supplied .zip/.tar.gz bundle entries are not editor-filtered. A .zip is
converted to tar.gz using the existing archive conventions: strip a common
top-level folder and omit ._* files, __MACOSX/ entries, and non-regular
entries. A .tar.gz is passed through. The server still validates archive
paths and entry types.

Markdown options (also available on update and watch):

    redrop share notes.md                         # auto-render .md
    redrop share notes.txt --render markdown       # force Markdown
    redrop share notes.md --render none            # raw file, as before
    redrop share ./report --render markdown --entry docs/notes.md
    cat notes.md | redrop share - --name notes.md

--render auto is the default. An explicit --as disables auto-rendering;
--as file keeps Markdown raw. Forced --render markdown requires no --as or
--as bundle. Directory rendering requires --entry and preserves bundle assets;
relative image/link paths are rebased from that source to the root index.html.
A single .md file does not upload siblings. Use a directory for local images.
Archives are not rendered: unpack them into a directory first. Existing
index.html files are never overwritten. --spa is not supported with rendering.
Markdown folders follow the folder rules above, and --entry is confined to
the folder the same way. An --entry that the skip rule excludes (such as
notes.md~ or a path under node_modules) is a usage error.
Missing local references in single-file uploads produce a stderr warning
suggesting directory publishing. Rejected image sources produce a warning for
both files and directories, once per distinct source, naming it and the local-image
rule. Empty sources have an explicit warning. Invalid options, missing sources
or entries, and symlink cycles exit with 2.

The page has heading anchors, a TOC at four headings, offline syntax highlighting,
copy buttons, GFM tables/task lists, blockquotes, and responsive light/dark styles.
Unknown languages and code blocks over 64 KiB stay plain. Sources over 8 MiB must
use --render none. HTML comments are omitted while visible text after them stays.
Comment-only blocks emit nothing. Other raw HTML becomes escaped
inline text or normal paragraphs, not Copy-enabled code; List<String> stays intact. There is no raw-HTML opt-in. HTTP/HTTPS and mailto
navigation links use a new tab with no opener/referrer. Images stay local-only;
active schemes, data URIs and unsafe attributes are rejected. A page CSP blocks
automatic external requests. The document has no hosting-specific branding, icon,
policy meta tags, or download-query convention. The original is intentionally
unmodified, so treat downloaded sources and other bundle files as untrusted
content, not sanitized documents.

The first line of normal output is "url: <link>". With --quiet, only the URL
(and a generated password on the next line) is printed. With --json you get
the artifact object: {"id","url","title","kind","status","expires_at",...}
plus "password" when one was generated.

Limits are set by the server (typically 500 MB per version). Large uploads are
chunked and resume automatically after network errors. A single file is
uploaded from its own path, without a temporary copy; if it changes during
the upload, share and update start over once, then fail with exit code 1.

## Access control

- Expiry: --ttl 30m | 12h | 7d | 2w | never (server default, usually 7d).
- Password: --password-generate (prints it; give it to the recipient
  separately), --password-file FILE, or --password VALUE (visible to other
  local processes).
- View limit: --once or --max-views N. Recipients first see an "Open" page;
  link-preview bots in chat apps cannot consume views. After the last view the
  link answers "Not found" and the content is deleted shortly after.
- Combine freely, e.g. --once --password-generate --ttl 1d.
- Links look like https://amber-otter-quilt-river.CONTENT-DOMAIN/: four random
  words (about 52 bits), so they can be read out or typed. Older links are 26
  random letters and digits and keep working. Rotate replaces either kind
  with a new four-word link.
- Info button and viewer: shared pages show a small "i" button in the
  bottom-right corner with the title, version and publish time, the expiry
  countdown, remaining views, whether a password is set, the size, and the
  list of versions with their --note text. Notes you add from now on are shown
  to viewers; notes written before this release stay private ("redrop
  versions" marks public notes with *). It never shows who shared the page.
  HTML artifacts open in redrop's viewer: the artifact itself loads, byte for
  byte as uploaded, in a full-page frame on the same link. The address bar
  follows the frame, including #fragments and Back/Forward inside it, but not
  single-page-app route changes made with pushState. Files keep redrop's own
  file page. Your artifact needs nothing redrop-specific. Because the frame
  is on the same origin, the info panel is a convenience, not a security
  boundary: a page could draw a fake one.
  --no-info on share, update, or set serves pages directly instead (no button,
  no frame); --info turns it back on (the server default is usually on).
  Append ?redrop-info=0 to a link for a direct, frameless view, e.g. for
  screenshots. It is remembered for 30 minutes when the link is opened
  directly (typed, bookmarked, opened from another app) or from the same page;
  when it is clicked on another website, only that page is direct.
  ?redrop-info=1 goes back to the viewer. curl, scripts and very old browsers
  (no Fetch Metadata headers) always get the artifact directly.
- Service workers are not available to shared pages.
- Older versions: --old-versions lets viewers open earlier versions from the
  info panel (?redrop-version=N); --no-old-versions (the default) shows them
  the list only. Inside an older version, a root-absolute link (/x.html)
  leads to the current version; relative links stay in the older one.
- Links are unguessable, not indexed (noindex + robots.txt), and send no
  Referer to third parties. Unknown, expired, used-up and revoked links all
  answer the same "Not found", and clients that hit many of them are
  temporarily blocked.

## Update and manage

    redrop update ID|URL PATH [--note "what changed"]   # same link, new version
    redrop watch ID|URL PATH [--debounce 500ms]         # republish on change
    redrop list [--status active|expired|exhausted|revoked] [--search TEXT]
    redrop info ID|URL
    redrop set ID|URL --ttl 30d | --password-generate | --no-password |
                     --max-views 3 | --unlimited | --reset-views | --title T |
                     --info | --no-info | --old-versions | --no-old-versions
    redrop revoke ID|URL      # disable now (restore with: redrop restore ID)
    redrop rotate ID|URL      # new link; the old one stops working
    redrop versions ID|URL    # history
    redrop rollback ID|URL N  # make version N current
    redrop files ID|URL       # files in a bundle
    redrop views ID|URL       # opens, link previews, failed passwords
    redrop delete ID|URL --yes

Updating an artifact first shared raw with a .md source now renders the new
version by default. Add --render none to keep that version raw.

Anywhere an ID is accepted, the artifact link (or just its four-word part,
e.g. amber-otter-quilt-river) works too.

## Watch

    redrop watch ID|URL PATH [--debounce 500ms] [--json]

Watches PATH (a file or folder, same rules as update) and publishes a new
version of the same link whenever the content changes: ideal while iterating
on a page with an agent. Edits settle first (--debounce, default 500ms).
The folder rules in Share apply; explicitly named files are still
uploaded. --render, --entry, --as, --spa and --note work as on update, so a
watched .md file is re-rendered on each change and --render none keeps it
raw. Warnings print on stderr; the skipped-file count does not. Identical
content publishes nothing. Each publish prints its
version number and time. With --json, each publish or non-fatal notice is
one JSON line:

    {"event":"publish","id":"…","version":3,"url":"https://…","at":"…"}
    {"event":"retry","id":"…","attempt":2,"delay_ms":1000,"error":"cannot reach server: …","at":"…"}

Other notice events are watch_error, root_vanished, rewatch_failed,
tree_changed, and empty_folder. Each includes id and at, plus error when
available; only retry has attempt, the upcoming upload attempt number (2 for
the first retry), and delay_ms, the wait before it.

Build tools that clean and rebuild the folder are handled: if files change
or vanish while a version is packed or uploaded, watch prints tree_changed
and tries again once the folder settles; an empty folder prints
empty_folder and publishes nothing until files appear. After 3 tree_changed
retries in a row with no new file event, the error is fatal (exit 1). Other
local errors, such as a missing temp directory, stop watch at once. A PATH
that is a symlink is resolved when watch starts, and the real folder or
file is watched.

Never two uploads at once; changes that land during an upload are published
once more afterwards. Atomic editor saves (write temp file, rename) and
delete+recreate are handled. Ctrl-C or SIGTERM stops watching and exits 0.
Network errors, rate limiting, and server errors are retried with nominal
exponential backoff from 1s to 60s, with jitter below the cap. Rate-limit
responses honor Retry-After when supplied, up to 10 minutes (10x the backoff
cap). Auth or not-found errors stop
with exit codes 3/4. New subdirectories are picked up automatically. A watch
session creates many versions; rely on the server's version retention
(REDROP_KEEP_VERSIONS) or roll back with "redrop rollback". There is
deliberately no way to rewrite a published version.

## Exit codes

    0  success
    1  other error
    2  usage error (bad flag/argument, invalid TTL, ...)
    3  authentication/permission error (bad or revoked token)
    4  not found
    5  too large / quota exceeded
    6  network error (server unreachable)
    7  conflict or gone (e.g. link already used up)
    8  not configured (run redrop login or set REDROP_URL/REDROP_TOKEN)

With --json, errors are written to stderr as {"error": "...", "exit_code": N}.

## Good practice for agents

- Share Markdown directly for documents and reports. Use self-contained HTML
  for custom layouts and visualizations.
- Pick a TTL that matches the purpose; use --once for secrets or sensitive
  files and pass the password through a different channel than the link.
- Give the artifact a clear --title; it is what the owner sees in the
  dashboard.
- Use "redrop update" for revisions instead of creating new links.
- Use "redrop watch" while iterating on one artifact so every edit goes to
  the same link; stop it before polishing by hand.
- Never print the API token.
`

func guideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "Print a complete usage guide (written for AI agents and scripts)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Print(guide)
		},
	}
}
