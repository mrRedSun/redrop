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

# watch republishes on change and exits cleanly on SIGTERM
$R watch "$ID" "$WORK/page.html" --debounce 300ms >"$WORK/watch.log" 2>"$WORK/watch.err" &
WPID=$!
w_versions() { $R info "$ID" --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["versions"])'; }
echo '<!doctype html><title>v3</title><p>third</p>' > "$WORK/page.html"
for _ in $(seq 1 50); do
  curl -fsS "$URL" | grep -q third && break
  sleep 0.2
done
curl -fsS "$URL" | grep -q third || { cat "$WORK/watch.log" "$WORK/watch.err"; die "watch published new content"; }
grep -q "version 3" "$WORK/watch.log" || die "watch printed the version number"
kill -TERM $WPID 2>/dev/null
for _ in $(seq 1 50); do kill -0 $WPID 2>/dev/null || break; sleep 0.2; done
kill -0 $WPID 2>/dev/null && die "watch did not exit on SIGTERM"
wait $WPID; WRC=$?
[ "$WRC" = 0 ] || die "watch exit code $WRC after SIGTERM, want 0"
[ "$(w_versions)" = 3 ] || die "watch made exactly one new version, got $(w_versions)"
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
