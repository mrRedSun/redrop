#!/usr/bin/env bash
# Build and (re)start the LAN-only development server as a transient systemd unit.
# Dashboard: http://$IP:8080   Artifacts: http://<slug>.$DASHED.sslip.io:8080
set -euo pipefail
cd "$(dirname "$0")/.."

IP=${REDROP_DEV_IP:-$(hostname -I | awk '{print $1}')}
DASHED=${IP//./-}
DATA=${REDROP_DEV_DATA:-/root/redrop-dev/data}
CLI=${REDROP_DEV_CLI:-/root/redrop-dev/cli}
mkdir -p "$DATA" "$CLI" /root/redrop-dev/bin

VERSION="dev-$(git rev-parse --short HEAD 2>/dev/null || echo local)"
LDFLAGS="-s -w -X github.com/mrRedSun/redrop/internal/server.Version=$VERSION"
go build -trimpath -ldflags "$LDFLAGS" -o /root/redrop-dev/bin/redrop-server ./cmd/redrop-server

# Cross-compile CLI builds served by /install.sh
CLILDFLAGS="-s -w -X main.version=$VERSION"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}; arch=${target#*/}; ext=""; [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$CLILDFLAGS" -o "$CLI/redrop-$os-$arch$ext" ./cmd/redrop
done
(cd "$CLI" && sha256sum redrop-* > SHA256SUMS)

systemctl stop redrop-dev 2>/dev/null || true
systemctl reset-failed redrop-dev 2>/dev/null || true
systemd-run --unit=redrop-dev --collect --quiet \
  -E REDROP_LISTEN=:8080 \
  -E REDROP_DATA_DIR="$DATA" \
  -E REDROP_CLI_DIR="$CLI" \
  -E REDROP_PUBLIC_URL="http://$IP:8080" \
  -E REDROP_CONTENT_URL="http://$DASHED.sslip.io:8080" \
  /root/redrop-dev/bin/redrop-server serve
sleep 1
systemctl --no-pager --lines=5 status redrop-dev | tail -6
echo "Dashboard: http://$IP:8080"
