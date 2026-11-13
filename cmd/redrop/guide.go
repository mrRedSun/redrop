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
                      [--password-generate | --password-file F] [--json]

PATH decides the kind (override with --as html|file|bundle):

- *.html / *.htm  -> "html": rendered as a live web page. Inline <script> and
  CDN libraries (e.g. React/ESM from a CDN) work. Each artifact has its own
  origin, so localStorage etc. are isolated.
- directory      -> "bundle": a static site. index.html is the start page
  (change with --entry); without one, visitors get a file listing. Use --spa
  for client-side routers. .git and node_modules are skipped.
- any other file -> "file": a page with inline preview (image, video, audio,
  PDF, text/code) and a Download button.
- "-"            -> read stdin; --name is required (e.g. --name report.html).
- site.zip / site.tar.gz with --as bundle -> unpacked as a static site.

The first line of normal output is "url: <link>". With --quiet, only the URL
(and a generated password on the next line) is printed. With --json you get
the artifact object: {"id","url","title","kind","status","expires_at",...}
plus "password" when one was generated.

Limits are set by the server (typically 500 MB per version). Large uploads are
chunked and resume automatically after network errors.

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
- Links are unguessable, not indexed (noindex + robots.txt), and send no
  Referer to third parties. Unknown, expired, used-up and revoked links all
  answer the same "Not found", and clients that hit many of them are
  temporarily blocked.

## Update and manage

    redrop update ID|URL PATH [--note "what changed"]   # same link, new version
    redrop list [--status active|expired|exhausted|revoked] [--search TEXT]
    redrop info ID|URL
    redrop set ID|URL --ttl 30d | --password-generate | --no-password |
                     --max-views 3 | --unlimited | --reset-views | --title T
    redrop revoke ID|URL      # disable now (restore with: redrop restore ID)
    redrop rotate ID|URL      # new link; the old one stops working
    redrop versions ID|URL    # history
    redrop rollback ID|URL N  # make version N current
    redrop files ID|URL       # files in a bundle
    redrop views ID|URL       # opens, link previews, failed passwords
    redrop delete ID|URL --yes

Anywhere an ID is accepted, the artifact link (or just its four-word part,
e.g. amber-otter-quilt-river) works too.

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

- Prefer sharing one self-contained HTML file for reports and visualizations.
- Pick a TTL that matches the purpose; use --once for secrets or sensitive
  files and pass the password through a different channel than the link.
- Give the artifact a clear --title; it is what the owner sees in the
  dashboard.
- Use "redrop update" for revisions instead of creating new links.
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
