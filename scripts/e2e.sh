#!/usr/bin/env bash
# End-to-end smoke test against a running dev server (see dev-server.sh).
# Creates the admin via the setup link on a fresh data dir, then exercises the CLI.
# Set REDROP_CLI to test a CLI binary other than the dev server's build.
set -euo pipefail
BASE=${BASE:-http://$(hostname -I | awk '{print $1}'):8080}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
CJ="$WORK/cookies"
H=(-H 'X-Redrop-Request: 1' -H 'Content-Type: application/json')
R=${REDROP_CLI:-/root/redrop-dev/cli/redrop-linux-amd64}
export REDROP_CONFIG="$WORK/cli.json"
pass() { printf '  ok  %s\n' "$*"; }
die() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }
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
curl -fsSL "$BASE/install.sh" | iso REDROP_TOKEN="$STOKEN" sh > "$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; die "one-line setup"; }
grep -q 'redrop is set up' "$WORK/install.log" || die "setup success message"
[ "$(stat -c %a "$IH/.config/redrop/config.json")" = 600 ] || die "saved config mode"
iso "$IH/bin/redrop" whoami --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["me"]["token_name"]=="e2e-setup",d' \
  || die "saved login from one-line setup"
pass "one-line setup: install.sh installs, logs in, and saves a working login"
set +e
curl -fsSL "$BASE/install.sh" | iso REDROP_TOKEN=rdp_bad sh > "$WORK/install-bad.log" 2>&1; rc=$?
set -e
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
echo "$SLUG" | grep -Eq '^[a-z]{3,9}(-[a-z]{3,9}){3}$' || die "new link is not four words: $URL"
[ "$($R info "$SLUG" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')" = "$ID" ] || die "info by bare slug"
pass "four-word link; CLI resolves it by link and by slug"
body=$(curl -fsS "$URL"); echo "$body" | grep -q '<h1>hello</h1>' || die "html content"
hdr=$(curl -sSI "$URL")
echo "$hdr" | grep -qi 'x-robots-tag: noindex' || die "noindex header"
echo "$hdr" | grep -qi 'referrer-policy: no-referrer' || die "referrer header"
echo "$hdr" | grep -qi 'cache-control: private' || die "cache header"
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
echo "$OID" | grep -q " exhausted 1 1" || die "status after one-time: $OID"
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
echo "$vp" | grep -q '<iframe id="rd-frame"[^>]* src="/?q=1"' || die "viewer frames the same path and query"
echo "$vp" | grep -q 'id="rd-info"' || die "info panel"
echo "$vp" | grep -q '<h1>hello</h1>' && die "artifact bytes in the viewer page"
echo "$vp" | grep -q '<script>' && die "inline script in the viewer page"
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
echo "$vp" | grep -q 'Version 2' && echo "$vp" | grep -q 'v2' || die "versions listed with notes"
echo "$vp" | grep -q 'redrop-version=2' && die "version 2 offered while closed"
[ "$(code_of "${DOC[@]}" "${URL}?redrop-version=2")" = 404 ] || die "closed version is the miss"
[ "$(code_of "${FRAME[@]}" "${URL}__redrop/v/2/")" = 404 ] || die "version path while closed"
$R versions "$ID" | grep -q '\* v2' || die "versions marks the new public note"
$R set "$ID" --old-versions | grep -q 'older versions: *viewers can open them' || die "set --old-versions"
vp=$(curl -fsS "${DOC[@]}" "${URL}?redrop-version=2")
echo "$vp" | grep -q 'Viewing version 2 of 2' || die "version bar"
echo "$vp" | grep -q '<iframe id="rd-frame"[^>]* src="/__redrop/v/2/"' || die "version 2 frame src"
curl -fsS "${FRAME[@]}" "${URL}__redrop/v/2/" | grep -q second || die "version 2 content"
[ "$(redirect_of "${DOC[@]}" "${URL}__redrop/v/2/")" = "303 ${URL}?redrop-version=2" ] || die "top-level version path goes to the viewer form"
$R set "$ID" --no-old-versions >/dev/null
[ "$(code_of "${FRAME[@]}" "${URL}__redrop/v/2/")" = 404 ] || die "version path after closing"
pass "older versions: metadata only, then openable under /__redrop/v/N/, then refused"

# --no-info and ?redrop-info=0: served directly; files keep the file page
NURL=$($R share "$WORK/page.html" --no-info -q)
curl -fsS "${DOC[@]}" "$NURL" | cmp -s - "$WORK/page.html" || die "--no-info is not the direct view"
$R set "$NURL" --info | grep -q 'info button: *shown' || die "set --info output"
curl -fsS "${DOC[@]}" "$NURL" | grep -q 'rd-frame' || die "frame after set --info"
curl -fsS "${DOC[@]}" "${URL}?redrop-info=0" | cmp -s - "$WORK/page-v1.html" || die "redrop-info=0 direct view"
J6="$WORK/c6"
curl -fsS -c "$J6" -o /dev/null "${DOC[@]}" -H 'Sec-Fetch-Site: none' "${BURL}?redrop-info=0"
curl -fsS -c "$WORK/c7" -o /dev/null -H 'Sec-Fetch-Dest: image' -H 'Sec-Fetch-Site: same-site' "${BURL}?redrop-info=0"
grep -q rd_direct "$WORK/c7" && die "a same-site image recorded the direct choice"
curl -fsS -b "$J6" "${DOC[@]}" "${BURL}some/route" | grep -q 'rd-frame' && die "direct choice not kept for the visit"
fp=$(curl -fsS "${DOC[@]}" "$FURL")
echo "$fp" | grep -q 'id="rd-info"' && echo "$fp" | grep -q 'Download' || die "file page with the info panel"
echo "$fp" | grep -q 'rd-frame' && die "file page framed"
pass "--no-info and ?redrop-info=0 (kept for the visit) are direct; file page has the panel natively"

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
REDROP_TOKEN=rdp_bad $R list >/dev/null 2>&1; [ $? = 3 ] || die "exit code 3 for bad token"
set -e
pass "exit codes 2/3/4"

$R delete "$SID" --yes >/dev/null
[ "$(curl -s -o /dev/null -w '%{http_code}' "$NEW")" = 404 ] || die "deleted"
pass "delete"
$R list
echo "ALL E2E CHECKS PASSED"
