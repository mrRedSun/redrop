#!/usr/bin/env bash
# End-to-end smoke test against a running dev server (see dev-server.sh).
# Creates the admin via the setup link on a fresh data dir, then exercises the CLI.
# Set REDROP_CLI to test a CLI binary other than the dev server's build.
set -euo pipefail
BASE=${BASE:-http://$(hostname -I | awk '{print $1}'):8080}
WORK=$(mktemp -d)
# A watcher started below is ours: stop it if a check fails first.
trap '[ -z "${WPID:-}" ] || kill "$WPID" 2>/dev/null; rm -rf "$WORK"' EXIT
CJ="$WORK/cookies"
H=(-H 'X-Redrop-Request: 1' -H 'Content-Type: application/json')
R=${REDROP_CLI:-/root/redrop-dev/cli/redrop-linux-amd64}
export REDROP_CONFIG="$WORK/cli.json"
pass() { printf '  ok  %s\n' "$*"; }
die() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }
# Every request is bounded, so a stuck server fails the check that made it
# (curl exits 28 and says "timed out") instead of hanging the run. The limits
# are far above any healthy response time. The CLI's own client has a
# 10-minute timeout; the installer runs under `timeout` below.
#
# `curl ... | grep -q X` must not depend on scheduling: grep -q exits at the
# first match, and if curl still has bytes to write it fails with 23 (write
# error), which pipefail turns into a failed check. When stdout is a pipe the
# reader chose to stop, and its own status decides the check, so 23 is not
# an error there.
curl() {
  local rc=0
  command curl --connect-timeout 10 --max-time 60 "$@" || rc=$?
  if [ "$rc" = 23 ] && [ -p /dev/stdout ]; then rc=0; fi
  return "$rc"
}
# wait_until SECONDS CMD...: poll CMD until it succeeds; fail after SECONDS.
# The bound only has to catch a real hang.
wait_until() {
  local end=$((SECONDS + $1)); shift
  until "$@"; do
    [ "$SECONDS" -lt "$end" ] || return 1
    sleep 0.2
  done
}
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# code and resolved redirect target of a request
redirect_of() { curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$@"; }
views_of() { $R info "$1" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["views"])'; }
# A browser's top-level page load, and its frame's load of the same URL.
DOC=(-H 'Sec-Fetch-Dest: document' -H 'Sec-Fetch-Mode: navigate')
FRAME=(-H 'Sec-Fetch-Dest: iframe' -H 'Sec-Fetch-Mode: navigate')

USER=${E2E_USER:-admin}
PW=${E2E_PASSWORD:-correct-horse-battery}
if [ -n "${SETUP_TOKEN:-}" ]; then
  curl -fsS -c "$CJ" "${H[@]}" -d "{\"token\":\"$SETUP_TOKEN\",\"username\":\"$USER\",\"password\":\"$PW\"}" "$BASE/api/v1/setup" >/dev/null
  pass "setup created admin"
else
  curl -fsS -c "$CJ" "${H[@]}" -d "{\"username\":\"$USER\",\"password\":\"$PW\"}" "$BASE/api/v1/auth/login" >/dev/null
  pass "password login"
fi

# CSRF: missing header must be rejected
code=$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ" -H 'Content-Type: application/json' -d '{"name":"x"}' "$BASE/api/v1/tokens")
[ "$code" = 403 ] || die "CSRF check expected 403 got $code"; pass "CSRF header enforced"

TOKEN=$(curl -fsS -b "$CJ" "${H[@]}" -d '{"name":"e2e"}' "$BASE/api/v1/tokens" | python3 -c 'import json,sys;print(json.load(sys.stdin)["secret"])')
pass "token created"
printf %s "$TOKEN" | $R login --url "$BASE" --token-stdin >/dev/null; pass "cli login"

# One-line setup from the token dialog: the real install.sh with REDROP_TOKEN,
# in an isolated HOME/config/install dir with no redrop variables inherited.
IH="$WORK/install-home"
iso() { env -u REDROP_CONFIG -u REDROP_URL -u REDROP_TOKEN -u XDG_CONFIG_HOME HOME="$IH" REDROP_INSTALL_DIR="$IH/bin" "$@"; }
mkdir -p "$IH"
STOKEN=$(curl -fsS -b "$CJ" "${H[@]}" -d '{"name":"e2e-setup"}' "$BASE/api/v1/tokens" | python3 -c 'import json,sys;print(json.load(sys.stdin)["secret"])')
curl -fsSL "$BASE/install.sh" | iso REDROP_TOKEN="$STOKEN" timeout 120s sh > "$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; die "one-line setup (exit 124 means it hung for 120s)"; }
grep -q 'redrop is set up' "$WORK/install.log" || die "setup success message"
[ "$(stat -c %a "$IH/.config/redrop/config.json")" = 600 ] || die "saved config mode"
iso "$IH/bin/redrop" whoami --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["me"]["token_name"]=="e2e-setup",d' \
  || die "saved login from one-line setup"
pass "one-line setup: install.sh installs, logs in, and saves a working login"
set +e
curl -fsSL "$BASE/install.sh" | iso REDROP_TOKEN=rdp_bad timeout 120s sh > "$WORK/install-bad.log" 2>&1; rc=$?
set -e
[ "$rc" != 124 ] || die "one-line setup with a bad token hung for 120s"
[ "$rc" != 0 ] || die "one-line setup with a bad token should fail"
grep -q 'redrop is installed at' "$WORK/install-bad.log" || die "bad-token message"
pass "one-line setup with a bad token fails with a retry hint"

# tokens cannot manage tokens
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"name":"x"}' "$BASE/api/v1/tokens")
[ "$code" = 403 ] || die "token->tokens expected 403 got $code"; pass "API token cannot create tokens"

cat > "$WORK/page.html" <<'EOF'
<!doctype html><html><head><title>E2E Report</title></head><body><h1>hello</h1><script>localStorage.x=1</script></body></html>
EOF
cp "$WORK/page.html" "$WORK/page-v1.html"
URL=$($R share "$WORK/page.html" -q)
ID=$($R info "$URL" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
pass "share html -> $URL"
# New links are four words (legacy 26-char links are covered by the Go tests).
SLUG=$(python3 -c 'import sys,urllib.parse;print(urllib.parse.urlsplit(sys.argv[1]).hostname.split(".")[0])' "$URL")
grep -Eq '^[a-z]{3,9}(-[a-z]{3,9}){3}$' <<<"$SLUG" || die "new link is not four words: $URL"
[ "$($R info "$SLUG" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')" = "$ID" ] || die "info by bare slug"
pass "four-word link; CLI resolves it by link and by slug"
body=$(curl -fsS "$URL"); grep -q '<h1>hello</h1>' <<<"$body" || die "html content"
hdr=$(curl -sSI "$URL")
grep -qi 'x-robots-tag: noindex' <<<"$hdr" || die "noindex header"
grep -qi 'referrer-policy: no-referrer' <<<"$hdr" || die "referrer header"
grep -qi 'cache-control: private' <<<"$hdr" || die "cache header"
pass "html served with noindex/no-referrer/private cache"
[ "$($R info "$ID" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["title"])')" = "E2E Report" ] || die "title from <title>"
pass "title taken from <title>"

# favicons: dashboard, content apex, and an artifact origin without its own icon
curl -fsS "$BASE/favicon.ico" -o "$WORK/default.ico"
curl -fsSI "$BASE/favicon.ico" | grep -qi 'content-type: image/x-icon' || die "dashboard favicon type"
curl -fsSI "$BASE/favicon.svg" | grep -qi 'cache-control: public, max-age=86400' || die "dashboard favicon cache"
curl -fsS "$BASE/apple-touch-icon.png" >/dev/null || die "dashboard apple-touch-icon"
curl -fsS "$BASE/" | grep -q 'rel="icon" href="/favicon.svg"' || die "dashboard links favicon"
APEX=$(echo "$URL" | sed -E 's#//[^.]+\.#//#')
curl -fsS "${APEX}favicon.ico" | cmp -s - "$WORK/default.ico" || die "apex favicon"
curl -fsS "$APEX" | grep -q 'rel="icon" href="/favicon.svg"' || die "apex links favicon"
VIEWS=$($R info "$ID" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["views"])')
for _ in 1 2 3; do
  curl -fsS -H 'Accept: text/html' "${URL}favicon.ico" | cmp -s - "$WORK/default.ico" || die "artifact fallback favicon"
done
curl -fsSI "${URL}favicon.ico" | grep -qi 'cache-control: private, max-age=86400' || die "artifact fallback favicon cache"
[ "$($R info "$ID" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["views"])')" = "$VIEWS" ] || die "favicon counted as a view"
pass "favicon on dashboard, apex and artifact origin; not counted as a view"

# update keeps link
echo '<!doctype html><title>v2</title><p>second</p>' > "$WORK/page.html"
$R update "$ID" "$WORK/page.html" --note v2 >/dev/null
curl -fsS "$URL" | grep -q second || die "v2 served"
$R rollback "$ID" 1 >/dev/null
curl -fsS "$URL" | grep -q hello || die "rollback"
pass "update + rollback keep the same link"

# watch republishes on change and exits cleanly on SIGTERM. It uses its own
# artifact, so URL keeps serving page-v1.html for the viewer-frame checks.
echo '<!doctype html><title>w1</title><p>first</p>' > "$WORK/watched.html"
WATCH_URL=$($R share "$WORK/watched.html" -q)
WID=$($R info "$WATCH_URL" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
$R watch "$WID" "$WORK/watched.html" --debounce 300ms >"$WORK/watch.log" 2>"$WORK/watch.err" &
WPID=$!
w_versions() { $R info "$WID" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["versions"])'; }
w_ready() { grep -q "^watching" "$WORK/watch.err"; }
w_published() { grep -q "version 2" "$WORK/watch.log"; }
w_exited() { ! kill -0 "$WPID" 2>/dev/null; }
# "watching" is printed only after the watches are registered, so an edit
# made after it cannot fall into the startup gap.
wait_until 60 w_ready || { cat "$WORK/watch.err"; die "watch did not start within 60s"; }
# Save with a rename, so the change is one event: "echo > file" truncates
# and then writes, and a stall between the two longer than the debounce
# would publish the empty file as a version of its own.
echo '<!doctype html><title>w2</title><p>third</p>' > "$WORK/watched.html.tmp"
mv "$WORK/watched.html.tmp" "$WORK/watched.html"
# The CLI prints the version after the server accepted it, so wait for the
# line, not for the content.
wait_until 60 w_published || { cat "$WORK/watch.log" "$WORK/watch.err"; die "watch did not report version 2 within 60s"; }
curl -fsS "$WATCH_URL" | grep -q third || die "watch published new content"
kill -TERM "$WPID" 2>/dev/null
wait_until 60 w_exited || die "watch did not exit within 60s of SIGTERM"
WRC=0; wait "$WPID" || WRC=$?
WPID=
[ "$WRC" = 0 ] || die "watch exit code $WRC after SIGTERM, want 0"
# The watcher has exited, so the count is final: no wait is needed to
# show that nothing else was published.
[ "$(w_versions)" = 2 ] || die "watch made exactly one new version, got $(w_versions)"
pass "watch: content change published, version printed, clean SIGTERM exit"

# one-time link with password
P=$($R share "$WORK/page.html" --once --password-generate --json)
OURL=$(echo "$P" | python3 -c 'import json,sys;print(json.load(sys.stdin)["url"])')
OPW=$(echo "$P" | python3 -c 'import json,sys;print(json.load(sys.stdin)["password"])')
curl -fsS -A 'TelegramBot (like TwitterBot)' "$OURL" | grep -q 'password protected' || die "gate for bot"
curl -fsS "$OURL" | grep -q 'href="/__redrop/favicon.svg"' || die "gate links favicon"
curl -fsS "${OURL}__redrop/favicon.svg" | grep -q '<svg' || die "gate favicon"
curl -fsS "${OURL}favicon.ico" | cmp -s - "$WORK/default.ico" || die "locked artifact favicon"
curl -fsS "$OURL" | grep -q 'password protected' || die "gate"
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Sec-Fetch-Site: same-origin' --data-urlencode "password=wrong" "${OURL}__redrop/open")
[ "$code" = 401 ] || die "wrong password expected 401 got $code"
C2="$WORK/c2"
code=$(curl -s -o /dev/null -w '%{http_code}' -c "$C2" -H 'Sec-Fetch-Site: same-origin' --data-urlencode "password=$OPW" "${OURL}__redrop/open")
[ "$code" = 303 ] || die "open expected 303 got $code"
curl -fsS -b "$C2" "$OURL" | grep -q second || die "content after unlock"
curl -fsS -b "$C2" "$OURL" | grep -q second || die "reload within visit window"
curl -fsS -b "$C2" "${OURL}favicon.ico" >/dev/null || die "favicon during visit"
code=$(curl -s -o /dev/null -w '%{http_code}' "$OURL")
[ "$code" = 404 ] || die "second visitor expected 404 got $code"
curl -fsS "${OURL}favicon.ico" | cmp -s - "$WORK/default.ico" || die "exhausted artifact favicon"
OID=$($R info "$OURL" --json | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["id"], d["status"], d["views"], d["previews"])')
grep -q " exhausted 1 1" <<<"$OID" || die "status after one-time: $OID"
pass "one-time + password: bot preview and favicons not counted, wrong pw rejected, second visitor gets 404"

# bundle with SPA and listing
mkdir -p "$WORK/site/assets" "$WORK/site/docs"
echo '<!doctype html><title>Site</title><div id=app>spa</div>' > "$WORK/site/index.html"
echo 'body{}' > "$WORK/site/assets/app.css"
echo 'doc' > "$WORK/site/docs/readme.txt"
printf 'own-icon' > "$WORK/site/favicon.ico"
BURL=$($R share "$WORK/site" --spa -q)
curl -fsS "$BURL" | grep -q spa || die "bundle index"
curl -fsSI "${BURL}assets/app.css" | grep -qi 'content-type: text/css' || die "css type"
curl -fsS "${BURL}some/route" | grep -q spa || die "spa fallback"
curl -fsS "${BURL}docs/" | grep -q readme.txt || die "dir listing"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BURL}missing.js")
[ "$code" = 404 ] || die "missing asset expected 404 got $code"
# A browser fetches the icon after the page, carrying the cookie the page set.
C3="$WORK/c3"
curl -fsS -c "$C3" "$BURL" >/dev/null
[ "$(curl -fsS -b "$C3" "${BURL}favicon.ico")" = own-icon ] || die "bundle's own favicon after a page view"
# Without that proof every slug gets the default, so free icon probes reveal nothing.
curl -fsS "${BURL}favicon.ico" | cmp -s - "$WORK/default.ico" || die "own favicon leaked without a page view"
UNKNOWN=$(echo "$BURL" | sed -E 's#//[a-z-]+\.#//amber-otter-quilt-river.#')
curl -fsS "${UNKNOWN}favicon.ico" | cmp -s - "$WORK/default.ico" || die "unknown slug favicon"
pass "bundle: index, types, SPA fallback, listing, 404, own favicon only after a page view"

# Markdown: local rendering, original download, update, opt-out, and bundled images.
printf '# Markdown report\n\n## Details\n\nA **styled** document. Returns List<String>.\n\n[Docs](https://example.com/docs) ![Missing](pic.png) ![Blocked](https://example.com/pixel.png) ![Repeated](https://example.com/pixel.png) ![Empty]() ![Empty again]()\n\n- a\n\n<div>Literal</div>\n\n<!-- hidden-note -->\n\n<!-- note --> important text\n\n<!--\nmulti\n--> visible tail\n' > "$WORK/notes.md"
MURL=$($R share "$WORK/notes.md" -q 2> "$WORK/markdown-warnings")
grep -q 'pic.png is not uploaded; share the directory with --render markdown --entry notes.md' "$WORK/markdown-warnings" || die "Markdown missing-asset warning"
grep -q 'image "https://example.com/pixel.png" removed: images must be files inside the bundle' "$WORK/markdown-warnings" || die "single-file rejected image warning"
[ "$(grep -c 'image "https://example.com/pixel.png" removed:' "$WORK/markdown-warnings")" = 1 ] || die "duplicate rejected-image warning"
[ "$(grep -c 'empty image source removed:' "$WORK/markdown-warnings")" = 1 ] || die "missing or duplicate empty-image warning"
curl -fsS "$MURL" -o "$WORK/rendered.html"
grep -q 'important text' "$WORK/rendered.html" || die "comment-block trailing text lost"
grep -q 'visible tail' "$WORK/rendered.html" || die "multiline comment trailing text lost"
grep -q '<li>a</li>' "$WORK/rendered.html" || die "Markdown single-letter text corruption"
grep -q '<p>&lt;div&gt;Literal&lt;/div&gt;' "$WORK/rendered.html" || die "raw HTML paragraph"
if grep -q 'hidden-note\|<pre><code>' "$WORK/rendered.html"; then die "comments or raw HTML code blocks survived"; fi
grep -q '<h1 id="md-markdown-report">Markdown report' "$WORK/rendered.html" || die "Markdown heading"
grep -q '<strong>styled</strong>' "$WORK/rendered.html" || die "Markdown formatting"
curl -fsSI "$MURL" | grep -qi 'content-type: text/html' || die "Markdown entry type"
curl -fsS "${MURL}notes.md" | cmp -s - "$WORK/notes.md" || die "Markdown original bytes"
grep -q 'href="./notes.md" download' "$WORK/rendered.html" || die "portable Markdown download link"
grep -q 'List&lt;String&gt;' "$WORK/rendered.html" || die "Markdown generic type preserved"
grep -q 'href="https://example.com/docs" rel="noopener noreferrer nofollow" target="_blank"' "$WORK/rendered.html" || die "safe Markdown external link"
if grep -Eq 'rel="icon"|name="robots"|name="referrer"|\?download' "$WORK/rendered.html"; then die "Markdown contains hosting-specific content"; fi
MID=$($R info "$MURL" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
printf '# Revised report\n' > "$WORK/notes.md"
$R update "$MID" "$WORK/notes.md" >/dev/null
curl -fsS "$MURL" | grep -q '<h1 id="md-revised-report">Revised report' || die "Markdown update"
curl -fsS "${MURL}notes.md" | cmp -s - "$WORK/notes.md" || die "updated Markdown source"
RAW=$($R share "$WORK/notes.md" --render none -q)
[ "$($R info "$RAW" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["kind"])')" = file ] || die "Markdown render none kind"
curl -fsS "${RAW}notes.md" | cmp -s - "$WORK/notes.md" || die "raw Markdown bytes"
if curl -fsSI "${RAW}notes.md" | grep -qi 'content-type: text/html'; then die "raw Markdown became HTML"; fi
FORCED=$($R share "$WORK/notes.md" --name notes.txt --render markdown -q)
curl -fsS "$FORCED" | grep -q '<h1 id="md-revised-report">Revised report' || die "forced Markdown"
# Explicit single-file selections follow links, even outside the link's parent.
mkdir "$WORK/md-links"
ln -s "$WORK/notes.md" "$WORK/md-links/absolute.md"
ln -s ../notes.md "$WORK/md-links/relative.md"
for LINK in "$WORK/md-links/absolute.md" "$WORK/md-links/relative.md"; do
  LINKNAME=${LINK##*/}
  LINKURL=$($R share "$LINK" -q)
  curl -fsS "$LINKURL" | grep -q '<h1 id="md-revised-report">Revised report' || die "auto-rendered explicit symlink"
  curl -fsS "${LINKURL}${LINKNAME}" | cmp -s - "$WORK/notes.md" || die "explicit symlink original bytes"
  LINKBUNDLE=$($R share "$LINK" --as bundle -q)
  curl -fsS "${LINKBUNDLE}${LINKNAME}" | cmp -s - "$WORK/notes.md" || die "single-file bundle symlink bytes"
done
pass "explicit absolute and relative source symlinks: auto-render and single-file bundle"
mkdir -p "$WORK/md-bundle/docs/images"
printf '# Bundled report\n\n![Sketch](images/sketch.png) ![Blocked](https://example.com/pixel.png)\n' > "$WORK/md-bundle/docs/report.md"
printf 'image-fixture' > "$WORK/md-bundle/docs/images/sketch.png"
ln -s images/sketch.png "$WORK/md-bundle/docs/image-link.png"
ln -s missing.png "$WORK/md-bundle/docs/broken-image.png"
MBURL=$($R share "$WORK/md-bundle" --render markdown --entry docs/report.md -q 2> "$WORK/bundle-warnings")
grep -q 'image "https://example.com/pixel.png" removed: images must be files inside the bundle' "$WORK/bundle-warnings" || die "directory rejected image warning"
grep -q 'docs/broken-image.png.*skipped: broken symlink' "$WORK/bundle-warnings" || die "Markdown broken-symlink warning"
curl -fsS "$MBURL" | grep -q 'src="./docs/images/sketch.png"' || die "Markdown bundle relative image"
[ "$(curl -fsS "${MBURL}docs/images/sketch.png")" = image-fixture ] || die "Markdown bundle asset"
curl -fsS "${MBURL}docs/report.md" | cmp -s - "$WORK/md-bundle/docs/report.md" || die "Markdown bundle original"
[ "$(curl -fsS "${MBURL}docs/image-link.png")" = image-fixture ] || die "Markdown bundle symlink asset"
ORDINARY=$($R share "$WORK/md-bundle" -q 2> "$WORK/ordinary-warnings")
grep -q 'docs/broken-image.png.*skipped: broken symlink' "$WORK/ordinary-warnings" || die "ordinary broken-symlink warning"
[ "$(curl -fsS "${ORDINARY}docs/image-link.png")" = image-fixture ] || die "ordinary bundle symlink policy"
pass "Markdown: portable HTML, literal generics, safe links, asset warning, source download, update, raw opt-out, bundle symlinks"

# file kind + range request
head -c 3000000 /dev/urandom > "$WORK/blob.bin"
FURL=$($R share "$WORK/blob.bin" --ttl 1h -q)
curl -fsS "$FURL" | grep -q 'Download' || die "file viewer"
n=$(curl -fsS -H 'Range: bytes=0-99' "${FURL}blob.bin" | wc -c)
[ "$n" = 100 ] || die "range request returned $n bytes"
curl -fsSI "${FURL}blob.bin?download" | grep -qi 'attachment' || die "download disposition"
pass "file viewer, range requests, download"

# viewer frame: a top-level page load gets redrop's viewer page; its frame
# (same origin), subresources and clients without Fetch Metadata get the
# artifact's bytes, untouched.
vp=$(curl -fsS "${DOC[@]}" "${URL}?q=1")
grep -q '<iframe id="rd-frame"[^>]* src="/?q=1"' <<<"$vp" || die "viewer frames the same path and query"
grep -q 'id="rd-info"' <<<"$vp" || die "info panel"
grep -q '<h1>hello</h1>' <<<"$vp" && die "artifact bytes in the viewer page"
grep -q '<script>' <<<"$vp" && die "inline script in the viewer page"
curl -fsSI "${DOC[@]}" "$URL" | grep -qi "content-security-policy: default-src 'none'.*frame-ancestors 'none'" || die "viewer CSP"
curl -fsS "${FRAME[@]}" "$URL" | cmp -s - "$WORK/page-v1.html" || die "frame bytes differ from the upload"
curl -fsS -H 'Sec-Fetch-Dest: script' "$URL" | cmp -s - "$WORK/page-v1.html" || die "subresource bytes differ from the upload"
curl -fsS "$URL" | cmp -s - "$WORK/page-v1.html" || die "bytes without Fetch Metadata differ from the upload"
curl -fsSI "${FRAME[@]}" "$URL" | grep -qi "content-security-policy: frame-ancestors 'self' $BASE" || die "content frame-ancestors"
[ "$(code_of -H 'Sec-Fetch-Dest: serviceworker' "${URL}sw.js")" = 404 ] || die "service worker served (Sec-Fetch-Dest)"
[ "$(code_of -H 'Service-Worker: script' "${URL}sw.js")" = 404 ] || die "service worker served (Service-Worker)"
pass "viewer page for documents; untouched bytes for frames, subresources, and no Fetch Metadata"

# paths starting with // or /\ are the miss: never a protocol-relative
# redirect or a viewer framing another site
[ "$(redirect_of --path-as-is "${DOC[@]}" "${URL}/evil.example/x?redrop-version=zz")" = "404 " ] || die "//host path redirected"
[ "$(redirect_of --path-as-is "${DOC[@]}" "${BURL}/evil.example/phish")" = "404 " ] || die "//host path on an SPA got a page"
[ "$(code_of --path-as-is "${DOC[@]}" "${URL}%5Cevil.example")" = 404 ] || die "/\\host path served"
pass "//host and /\\host paths are the miss"

# exactly one view per visit
VURL=$($R share "$WORK/page-v1.html" -q)
J8="$WORK/c8"
curl -fsS -c "$J8" -b "$J8" "${DOC[@]}" "$VURL" | grep -q 'rd-frame' || die "viewer for the view test"
curl -fsS -b "$J8" "${FRAME[@]}" "$VURL" >/dev/null
curl -fsS "${FRAME[@]}" "$VURL" >/dev/null
[ "$(views_of "$VURL")" = 1 ] || die "viewer visit counted $(views_of "$VURL") views"
WURL=$($R share "$WORK/page-v1.html" -q)
curl -fsS "$WURL" >/dev/null
[ "$(views_of "$WURL")" = 1 ] || die "direct load without Fetch Metadata counted $(views_of "$WURL") views"
pass "one view per visit: the viewer load counts, its frame doesn't; a direct load counts once"

# gates guard the viewer page and its frame on the same origin
printf 'e2e-gate-password' > "$WORK/gpw"
GURL=$($R share "$WORK/page-v1.html" --password-file "$WORK/gpw" -q)
curl -fsS "${DOC[@]}" "$GURL" | grep -q 'name="password"' || die "gate before the viewer"
curl -fsS "${FRAME[@]}" "$GURL" | grep -q 'name="password"' || die "gate in the frame"
J9="$WORK/c9"
[ "$(code_of -c "$J9" -H 'Sec-Fetch-Site: same-origin' --data-urlencode "password=e2e-gate-password" "${GURL}__redrop/open")" = 303 ] || die "gate unlock"
curl -fsS -b "$J9" "${DOC[@]}" "$GURL" | grep -q 'rd-frame' || die "viewer after unlock"
curl -fsS -b "$J9" "${FRAME[@]}" "$GURL" | cmp -s - "$WORK/page-v1.html" || die "frame after unlock"
pass "password gate before the viewer and in the frame; both open after unlock"

# older versions: listed always, openable only when the owner allows it
# ($URL is at version 1 after the rollback above; version 2 is older-but-newer)
vp=$(curl -fsS "${DOC[@]}" "$URL")
grep -q 'Version 2' <<<"$vp" && grep -q 'v2' <<<"$vp" || die "versions listed with notes"
grep -q 'redrop-version=2' <<<"$vp" && die "version 2 offered while closed"
[ "$(code_of "${DOC[@]}" "${URL}?redrop-version=2")" = 404 ] || die "closed version is the miss"
[ "$(code_of "${FRAME[@]}" "${URL}__redrop/v/2/")" = 404 ] || die "version path while closed"
grep -q '\* v2' <<<"$($R versions "$ID")" || die "versions marks the new public note"
grep -q 'older versions: *viewers can open them' <<<"$($R set "$ID" --old-versions)" || die "set --old-versions"
vp=$(curl -fsS "${DOC[@]}" "${URL}?redrop-version=2")
grep -q 'Viewing version 2 of 2' <<<"$vp" || die "version bar"
grep -q '<iframe id="rd-frame"[^>]* src="/__redrop/v/2/"' <<<"$vp" || die "version 2 frame src"
curl -fsS "${FRAME[@]}" "${URL}__redrop/v/2/" | grep -q second || die "version 2 content"
[ "$(redirect_of "${DOC[@]}" "${URL}__redrop/v/2/")" = "303 ${URL}?redrop-version=2" ] || die "top-level version path goes to the viewer form"
$R set "$ID" --no-old-versions >/dev/null
[ "$(code_of "${FRAME[@]}" "${URL}__redrop/v/2/")" = 404 ] || die "version path after closing"
pass "older versions: metadata only, then openable under /__redrop/v/N/, then refused"

# --no-info and ?redrop-info=0: served directly; files keep the file page
NURL=$($R share "$WORK/page.html" --no-info -q)
curl -fsS "${DOC[@]}" "$NURL" | cmp -s - "$WORK/page.html" || die "--no-info is not the direct view"
grep -q 'info button: *shown' <<<"$($R set "$NURL" --info)" || die "set --info output"
curl -fsS "${DOC[@]}" "$NURL" | grep -q 'rd-frame' || die "frame after set --info"
curl -fsS "${DOC[@]}" "${URL}?redrop-info=0" | cmp -s - "$WORK/page-v1.html" || die "redrop-info=0 direct view"
J6="$WORK/c6"
curl -fsS -c "$J6" -o /dev/null "${DOC[@]}" -H 'Sec-Fetch-Site: none' "${BURL}?redrop-info=0"
curl -fsS -c "$WORK/c7" -o /dev/null -H 'Sec-Fetch-Dest: image' -H 'Sec-Fetch-Site: same-site' "${BURL}?redrop-info=0"
grep -q rd_direct "$WORK/c7" && die "a same-site image recorded the direct choice"
curl -fsS -b "$J6" "${DOC[@]}" "${BURL}some/route" | grep -q 'rd-frame' && die "direct choice not kept for the visit"
fp=$(curl -fsS "${DOC[@]}" "$FURL")
grep -q 'id="rd-info"' <<<"$fp" && grep -q 'Download' <<<"$fp" || die "file page with the info panel"
grep -q 'rd-frame' <<<"$fp" && die "file page framed"
pass "--no-info and ?redrop-info=0 (kept for the visit) are direct; file page has the panel natively"

# viewer feedback: closed by default (the endpoint is an unknown path), then
# opened: a browser's post from the artifact's page, read back with
# redrop feedback --json, then resolved.
FBURL=$($R share "$WORK/page-v1.html" -q)
FBORIGIN=${FBURL%/}
FB=(-H "Origin: $FBORIGIN" -H 'Sec-Fetch-Site: same-origin' -H 'Sec-Fetch-Mode: cors' -H 'Sec-Fetch-Dest: empty' -H 'Content-Type: application/json')
FBBODY='{"body":"The heading is wrong","name":"e2e","version":1,"page":"/?q=1#top","quote":{"exact":"hello","prefix":"","suffix":""}}'
[ "$(code_of "${FB[@]}" -d "$FBBODY" "${FBURL}__redrop/feedback")" = 405 ] || die "feedback endpoint answered while closed"
curl -fsS "${DOC[@]}" "$FBURL" | grep -q 'id="rd-fb"' && die "feedback form while closed"
grep -q 'feedback: *on' <<<"$($R set "$FBURL" --feedback)" || die "set --feedback"
curl -fsS "${DOC[@]}" "$FBURL" | grep -q '<form id="rd-fb"' || die "feedback form in the info panel"
curl -fsSI "${DOC[@]}" "$FBURL" | grep -qi "content-security-policy:.*connect-src $FBORIGIN/__redrop/feedback" || die "feedback connect-src"
curl -fsS "${FRAME[@]}" "$FBURL" | cmp -s - "$WORK/page-v1.html" || die "feedback changed the artifact's bytes"
[ "$(code_of -H 'Origin: https://evil.example' -H 'Sec-Fetch-Site: cross-site' -H 'Content-Type: application/json' -d "$FBBODY" "${FBURL}__redrop/feedback")" = 403 ] || die "cross-site feedback accepted"
FBV=$(views_of "$FBURL")
[ "$(code_of "${FB[@]}" -d "$FBBODY" "${FBURL}__redrop/feedback")" = 201 ] || die "feedback post from the page"
[ "$(views_of "$FBURL")" = "$FBV" ] || die "a feedback post counted a view"
CID=$($R feedback "$FBURL" --json | python3 -c 'import json,sys
d=json.load(sys.stdin)
assert len(d)==1 and d[0]["body"]=="The heading is wrong" and d[0]["name"]=="e2e" and d[0]["version"]==1, d
assert d[0]["page"]=="/?q=1#top" and d[0]["quote"]["exact"]=="hello" and d[0]["resolved_at"] is None, d
print(d[0]["id"])') || die "feedback --json"
$R feedback "$FBURL" | grep -q '^  | The heading is wrong$' || die "feedback plain output frames the comment"
$R feedback resolve "$FBURL" "$CID" >/dev/null
[ "$($R feedback "$FBURL" --json | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')" = 0 ] || die "resolved comment still listed as open"
[ "$($R feedback "$FBURL" --all --json | python3 -c 'import json,sys;print(json.load(sys.stdin)[0]["resolved_at"] is not None)')" = True ] || die "resolved comment in --all"
pass "feedback: closed by default, opened with set --feedback, posted as a browser would, read with --json, resolved"

# End-to-end encryption (--e2e): the CLI encrypts, the server stores and
# serves only ciphertext, and Node's WebCrypto with the viewer's own
# decryptor (scripts/e2e-decrypt.mjs) reads it back with the link's key.
# Full links (keys included) travel in files under $WORK and on stdin, never
# in arguments or this script's output.
# Optional: E2E_DATA_DIR (the server's data dir) also greps the stored blobs;
# SERVER_LOG_CMD (e.g. "journalctl -u redrop-preview -o cat") greps the
# server's log for keys and refused uploads. With Chromium installed
# (CHROMIUM, default chromium), scripts/e2e-browser.mjs runs the viewer in a
# real browser.
command -v node >/dev/null || die "node (24+) is needed for the end-to-end decryption check"
SCRIPTS=$(cd "$(dirname "$0")" && pwd)
# decrypt LINKFILE [PATH|--list]: decrypt the link in LINKFILE like a viewer.
decrypt() { local f=$1; shift; node "$SCRIPTS/e2e-decrypt.mjs" "$@" < "$f"; }
logged_uploads() { if [ -n "${SERVER_LOG_CMD:-}" ]; then bash -c "$SERVER_LOG_CMD" 2>/dev/null | grep -c 'path=/api/v1/uploads' || true; fi; }
EMARK="e2e-secret-$RANDOM-$RANDOM"
mkdir -p "$WORK/e2e-site/docs"
printf '<!doctype html><title>E2E title</title><p>%s</p><a href="docs/n.txt">n</a>\n' "$EMARK" > "$WORK/e2e-site/index.html"
printf 'note %s\n' "$EMARK" > "$WORK/e2e-site/docs/n.txt"
EOUT=$($R share "$WORK/e2e-site" --e2e --json 2>"$WORK/e2e-share.err")
ELINKF="$WORK/e2e-link"
python3 -c 'import json,sys;print(json.load(sys.stdin)["url"])' <<<"$EOUT" > "$ELINKF"
EID=$(python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])' <<<"$EOUT")
EKEY=$(sed 's/.*#k=//' "$ELINKF")
EURL=$(sed 's/#.*//' "$ELINKF")
[ "${#EKEY}" = 43 ] || die "share --e2e link has no 43-character key"
grep -q 'end-to-end encrypted' "$WORK/e2e-share.err" || die "share --e2e notice"
KS="$(dirname "$REDROP_CONFIG")/e2e-keys.json"
[ "$(stat -c %a "$KS")" = 600 ] || die "key store mode"
grep -q "$EKEY" "$KS" || die "key saved in the key store"
$R list --json | python3 -c '
import json,sys
a=[x for x in json.load(sys.stdin) if x["id"]==sys.argv[1]][0]
assert a["e2e"] and "#" not in a["url"] and a["title"]=="", a' "$EID" || die "server view of an e2e artifact"
pass "share --e2e: link carries the key, key saved (0600), server has no key and no title"

page=$(curl -fsS "${DOC[@]}" "$EURL")
grep -q 'data-e2e' <<<"$page" || die "e2e viewer page"
grep -q "$EMARK" <<<"$page" && die "viewer page holds plaintext"
decrypt "$ELINKF" > "$WORK/e2e-index.out" || die "decrypting the start page"
cmp -s "$WORK/e2e-index.out" "$WORK/e2e-site/index.html" || die "decrypted start page differs"
[ "$(decrypt "$ELINKF" docs/n.txt)" = "note $EMARK" ] || die "decrypted docs/n.txt"
printf '%s#k=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n' "$EURL" > "$WORK/e2e-wrong-link"
decrypt "$WORK/e2e-wrong-link" >/dev/null 2>&1 && die "a wrong key decrypted"
# Every stored object, as the server serves it, is ciphertext.
EVER=$(decrypt "$ELINKF" --list | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')
for obj in manifest $(decrypt "$ELINKF" --list | python3 -c 'import json,sys;print(" ".join(f["object"] for f in json.load(sys.stdin)["files"]))'); do
  curl -fsS -H 'Sec-Fetch-Dest: empty' "${EURL}__redrop/v/$EVER/$obj" -o "$WORK/e2e-object"
  head -c 4 "$WORK/e2e-object" | grep -q '^RDE' || die "object $obj lacks the format magic"
  grep -q "$EMARK" "$WORK/e2e-object" && die "object $obj holds plaintext"
  grep -q 'E2E title' "$WORK/e2e-object" && die "object $obj holds the title"
done
if [ -n "${E2E_DATA_DIR:-}" ]; then
  grep -rqF "$EMARK" "${E2E_DATA_DIR:?}" && die "the data dir holds the plaintext"
  grep -rqF "$EKEY" "${E2E_DATA_DIR:?}" && die "the data dir holds the key"
  pass "e2e: the data dir holds neither plaintext nor key"
fi
[ "$(code_of -H 'Service-Worker: script' -H 'Sec-Fetch-Dest: serviceworker' "${EURL}__redrop/e2e-sw.js")" = 200 ] || die "worker script on the e2e host"
[ "$(code_of -H 'Service-Worker: script' -H 'Sec-Fetch-Dest: serviceworker' "${URL}__redrop/e2e-sw.js")" = 404 ] || die "worker script on a plaintext host"
pass "e2e: viewer page, Node WebCrypto decryption, ciphertext-only objects, worker on e2e hosts only"

# Plaintext never goes to an encrypted artifact (review 33 B-1). With the key
# in this machine's store, update encrypts without --e2e; from the full link
# on stdin, a machine without the store does too; without any key, update
# and watch refuse before uploading anything.
sed -i "s/<p>/<p>v2 /" "$WORK/e2e-site/index.html"
$R update "$EID" "$WORK/e2e-site" >/dev/null 2>"$WORK/e2e-auto.err" || { cat "$WORK/e2e-auto.err"; die "update of an e2e artifact without --e2e"; }
grep -q "encrypting with the key from this machine's key store" "$WORK/e2e-auto.err" || die "update without --e2e did not say it encrypts"
decrypt "$ELINKF" | grep -q "v2 $EMARK" || die "update without --e2e: the link opens version 2"
mkdir -p "$WORK/e2e-other" "$WORK/e2e-nokey"
cp "$REDROP_CONFIG" "$WORK/e2e-other/cli.json"
cp "$REDROP_CONFIG" "$WORK/e2e-nokey/cli.json"
sed -i "s/<p>v2 /<p>v3 /" "$WORK/e2e-site/index.html"
REDROP_CONFIG="$WORK/e2e-other/cli.json" $R update - "$WORK/e2e-site" < "$ELINKF" >/dev/null 2>&1 || die "update with the full link on stdin"
decrypt "$ELINKF" | grep -q "v3 $EMARK" || die "the link opens version 3"
grep -q "$EKEY" "$WORK/e2e-other/e2e-keys.json" || die "a key that worked is remembered"
before=$(logged_uploads)
for cmd in "update $EID $WORK/page.html" "update $EID $WORK/e2e-site --e2e" "watch $EID $WORK/e2e-site"; do
  set +e
  # shellcheck disable=SC2086 # the words are the command's arguments
  REDROP_CONFIG="$WORK/e2e-nokey/cli.json" timeout 60s $R $cmd >/dev/null 2>"$WORK/e2e-nokey.err"; rc=$?
  set -e
  [ "$rc" = 2 ] && grep -q 'nothing was uploaded' "$WORK/e2e-nokey.err" || die "'redrop ${cmd%% *}' without a key must exit 2 before uploading (got $rc)"
done
[ "$(logged_uploads)" = "$before" ] || die "a refused update or watch started an upload"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"size\":10,\"artifact_id\":\"$EID\"}" "$BASE/api/v1/uploads")
[ "$code" = 400 ] || die "the server must refuse a plaintext upload declared for an e2e artifact, got $code"
$R info "$EID" | grep -q "#k=$EKEY" || die "info prints the full link"
$R watch "$EID" "$WORK/e2e-site" --debounce 300ms >"$WORK/e2e-watch.log" 2>"$WORK/e2e-watch.err" &
WPID=$!
e_ready() { grep -q "^watching" "$WORK/e2e-watch.err"; }
e_published() { grep -q "#k=$EKEY" "$WORK/e2e-watch.log"; }
w_exited() { ! kill -0 "$WPID" 2>/dev/null; }
wait_until 60 e_ready || { cat "$WORK/e2e-watch.err"; die "watch of an e2e artifact did not start within 60s"; }
sed "s/<p>v3 /<p>v4 /" "$WORK/e2e-site/index.html" > "$WORK/e2e-index.tmp"
mv "$WORK/e2e-index.tmp" "$WORK/e2e-site/index.html"
wait_until 60 e_published || { cat "$WORK/e2e-watch.log" "$WORK/e2e-watch.err"; die "watch of an e2e artifact did not publish within 60s"; }
decrypt "$ELINKF" | grep -q "v4 $EMARK" || die "watch: the link opens the watched change"
kill -TERM "$WPID" 2>/dev/null
wait_until 60 w_exited || die "watch did not exit within 60s of SIGTERM"
WRC=0; wait "$WPID" || WRC=$?
WPID=
[ "$WRC" = 0 ] || die "watch exit code $WRC after SIGTERM"
pass "e2e: update and watch encrypt with the stored key or the link on stdin, flag or not; without a key nothing is uploaded"

# Gates still apply to the viewer page, the ciphertext and the worker.
GOUT=$($R share "$WORK/e2e-site/docs/n.txt" --e2e --once --password-generate --json 2>/dev/null)
GLINKF="$WORK/e2e-gated-link"
python3 -c 'import json,sys;print(json.load(sys.stdin)["url"])' <<<"$GOUT" > "$GLINKF"
GPW=$(python3 -c 'import json,sys;print(json.load(sys.stdin)["password"])' <<<"$GOUT")
GURL=$(sed 's/#.*//' "$GLINKF")
curl -fsS "${DOC[@]}" "$GURL" | grep -q 'password protected' || die "e2e gate"
[ "$(code_of -H 'Sec-Fetch-Dest: empty' "${GURL}__redrop/v/1/manifest")" = 403 ] || die "ciphertext behind the gate"
[ "$(code_of -H 'Service-Worker: script' -H 'Sec-Fetch-Dest: serviceworker' "${GURL}__redrop/e2e-sw.js")" = 403 ] || die "worker behind the gate"
GC="$WORK/e2e-gate-cookies"
curl -fsS -o /dev/null -c "$GC" -H 'Sec-Fetch-Site: same-origin' --data-urlencode "password=$GPW" "${GURL}__redrop/open"
GCOOKIE=$(awk '$6 ~ /visit/ {print $6"="$7}' "$GC")
[ -n "$GCOOKIE" ] || die "visit cookie after open"
[ "$(REDROP_COOKIE="$GCOOKIE" decrypt "$GLINKF")" = "note $EMARK" ] || die "e2e content during the visit"
[ "$(code_of "${DOC[@]}" "$GURL")" = 404 ] || die "second visitor of a one-time e2e link"
pass "e2e: password and one-time gates hold for viewer, ciphertext and worker"

if [ -n "${SERVER_LOG_CMD:-}" ]; then
  for f in "$ELINKF" "$GLINKF"; do
    bash -c "$SERVER_LOG_CMD" 2>/dev/null | grep -qF -- "$(sed 's/.*#k=//' "$f")" && die "the server log holds an e2e key"
  done
  bash -c "$SERVER_LOG_CMD" 2>/dev/null | grep -q '%23\|#k=' && die "the server log holds a fragment"
  pass "e2e: no key or fragment in the server log"
fi

# The viewer in a real browser (review 33 B-2, M1, m-A): no route open to a
# script in the artifact holds the key, gates keep it, SVG files are inert,
# and revoking stops an open page. Needs Chromium; the links go in a file.
if command -v "${CHROMIUM:-chromium}" >/dev/null; then
  B="$WORK/e2e-browser"
  mkdir -p "$B/probe/docs"
  cp "$SCRIPTS/e2e-browser-probe.html" "$B/probe/index.html"
  printf '<!doctype html><meta charset="utf-8"><title>Two</title><h1 id="sec">two-8842</h1><p><a href="../index.html">back</a></p>\n' > "$B/probe/docs/two.html"
  printf '<!doctype html><title>pw</title><p>pw-page-5150</p>\n' > "$B/pw.html"
  printf '<!doctype html><title>once</title><p>once-page-6160</p>\n' > "$B/once.html"
  printf '%s\n' '<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><rect width="40" height="40" fill="teal"/><script>parent.document.title="svg-script-ran"</script></svg>' > "$B/logo.svg"
  printf 'pw-%s\n' "$RANDOM$RANDOM" > "$B/password"
  P_OUT=$($R share "$B/probe" --e2e --json 2>/dev/null)
  W_OUT=$($R share "$B/pw.html" --e2e --password-file "$B/password" --json 2>/dev/null)
  O_OUT=$($R share "$B/once.html" --e2e --once --json 2>/dev/null)
  S_OUT=$($R share "$B/logo.svg" --e2e --json 2>/dev/null)
  python3 - "$B/password" "$P_OUT" "$W_OUT" "$O_OUT" "$S_OUT" > "$B/links.json" <<'PY'
import json, sys
pw = open(sys.argv[1]).read().strip()
names = ["probe", "password", "once", "svg"]
out = {n: {k: json.loads(raw)[k] for k in ("id", "url")} for n, raw in zip(names, sys.argv[2:])}
out["password"]["password"] = pw
print(json.dumps(out))
PY
  CHROMIUM="${CHROMIUM:-chromium}" REDROP_CLI="$R" node "$SCRIPTS/e2e-browser.mjs" "$B/links.json" > "$B/out.txt" 2>&1 || { cat "$B/out.txt"; die "the viewer failed in the browser"; }
  sed 's/^/    /' "$B/out.txt"
  pass "e2e: in Chromium, no script in the artifact can read the key; gates keep it; SVG files are inert; revocation stops an open page"
else
  printf '  --  e2e: browser checks skipped (no Chromium; set CHROMIUM)\n'
fi

# stdin + revoke/restore + rotate
SURL=$(echo '# notes' | $R share - --name notes.md -q)
SID=$($R info "$SURL" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
$R revoke "$SID" >/dev/null
[ "$(curl -s -o /dev/null -w '%{http_code}' "$SURL")" = 404 ] || die "revoked"
$R restore "$SID" >/dev/null
NEW=$($R rotate "$SID" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["url"])')
[ "$(curl -s -o /dev/null -w '%{http_code}' "$SURL")" = 404 ] || die "old link after rotate"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$NEW")" = 200 ] || die "new link after rotate"
python3 -c 'import sys,urllib.parse;print(urllib.parse.urlsplit(sys.argv[1]).hostname.split(".")[0])' "$NEW" | grep -Eq '^[a-z]{3,9}(-[a-z]{3,9}){3}$' || die "rotated link is not four words: $NEW"
pass "stdin share, revoke/restore, rotate to a four-word link"

# exit codes
set +e
$R info doesnotexist00 >/dev/null 2>&1; [ $? = 4 ] || die "exit code 4 for not found"
$R share "$WORK/page.html" --ttl banana >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for bad ttl"
$R share "$WORK/notes.md" --render invalid >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for render mode"
$R share "$WORK/notes.md" --render markdown --as file >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for render-kind conflict"
$R share "$WORK/notes.md" --spa >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for Markdown SPA"
$R share "$WORK/md-bundle" --render markdown >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for missing Markdown entry"
$R share "$WORK/notes.md" --name ../notes.md >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for Markdown name"
$R share "$WORK/notes.md" --name site.zip --render markdown >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for Markdown archive"
$R share "$WORK/md-bundle" --render markdown --entry docs >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for directory Markdown entry"
$R share - </dev/null >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for missing stdin name"
$R share "$WORK/md-bundle" --as file >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for directory forced as file"
$R share "$WORK/notes.md" --as invalid >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for unknown kind"
$R share "$WORK/missing.md" >/dev/null 2> "$WORK/missing-source-error"; [ $? = 2 ] || die "exit code 2 for missing source file"
grep -q 'does not exist' "$WORK/missing-source-error" || die "missing source diagnostic"
if grep -q 'symlink' "$WORK/missing-source-error"; then die "missing source blamed on symlink"; fi
$R share "$WORK/missing-directory" >/dev/null 2>&1; [ $? = 2 ] || die "exit code 2 for missing source directory"
$R share "$WORK/md-bundle" --render markdown --entry missing.md >/dev/null 2> "$WORK/missing-entry-error"; [ $? = 2 ] || die "exit code 2 for missing entry"
grep -q 'does not exist' "$WORK/missing-entry-error" || die "missing entry diagnostic"
if grep -q 'symlink' "$WORK/missing-entry-error"; then die "missing entry blamed on symlink"; fi
mkdir "$WORK/loop-site"
ln -s loop "$WORK/loop-site/loop"
$R share "$WORK/loop-site" >/dev/null 2> "$WORK/loop-error"; [ $? = 2 ] || die "exit code 2 for self-loop symlink"
grep -q 'loop.*symlink cycle' "$WORK/loop-error" || die "self-loop diagnostic"
mkfifo "$WORK/input.md"
# A blocked open never returns; the bound only has to catch that hang.
timeout 60s $R share "$WORK/input.md" >/dev/null 2>&1; rc=$?
[ $rc != 124 ] || die "non-regular Markdown source blocked for 60s"
[ $rc = 2 ] || die "non-regular Markdown source must exit 2, got $rc"
ln -s "$WORK/notes.md" "$WORK/md-bundle/outside.md"
$R share "$WORK/md-bundle" >/dev/null 2> "$WORK/symlink-error"; [ $? = 2 ] || die "exit code 2 for outside-root symlink"
grep -q 'outside.md.*symlinks must be relative and stay inside the directory' "$WORK/symlink-error" || die "symlink error must name path and rule"
REDROP_TOKEN=rdp_bad $R list >/dev/null 2>&1; [ $? = 3 ] || die "exit code 3 for bad token"
set -e
pass "exit codes 2/3/4"

$R delete "$SID" --yes >/dev/null
[ "$(curl -s -o /dev/null -w '%{http_code}' "$NEW")" = 404 ] || die "deleted"
pass "delete"
$R list
echo "ALL E2E CHECKS PASSED"
