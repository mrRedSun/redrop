package server

import (
	"bytes"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

// spa serves the embedded dashboard, falling back to index.html for client routes.
func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeErr(w, r, errNotFound("no such API endpoint"))
		return
	}
	if s.Assets == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("redrop API is running; the dashboard was not built into this binary.\n"))
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p != "" && p != "index.html" {
		if st, err := fs.Stat(s.Assets, p); err == nil && !st.IsDir() {
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, s.Assets, p)
			return
		}
		if path.Ext(p) != "" && !strings.HasSuffix(p, ".html") {
			http.NotFound(w, r)
			return
		}
	}
	b, err := fs.ReadFile(s.Assets, "index.html")
	if err != nil {
		http.Error(w, "dashboard missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(b))
}

var cliFileRE = regexp.MustCompile(`^redrop-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$|^SHA256SUMS$`)

func (s *Server) downloadCLI(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if s.CLIDir == "" || !cliFileRE.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.CLIDir, name)
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if name == "SHA256SUMS" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+name)
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

var installShTmpl = template.Must(template.New("sh").Parse(`#!/bin/sh
# redrop CLI installer. Usage: curl -fsSL {{.Base}}/install.sh | sh
# Options (env): REDROP_INSTALL_DIR=/custom/bin
set -eu

BASE="{{.Base}}"

err() { printf 'redrop install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS $(uname -s); on Windows run: irm $BASE/install.ps1 | iex" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) err "unsupported CPU $(uname -m)" ;;
esac

if [ -n "${REDROP_INSTALL_DIR:-}" ]; then
  dir="$REDROP_INSTALL_DIR"
elif [ "$(id -u)" = 0 ]; then
  dir=/usr/local/bin
else
  dir="$HOME/.local/bin"
fi
mkdir -p "$dir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
file="redrop-$os-$arch"

fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
  else err "curl or wget is required"; fi
}

fetch "$BASE/download/$file" "$tmp/$file" || err "download failed"
fetch "$BASE/download/SHA256SUMS" "$tmp/SHA256SUMS" || err "checksum download failed"

want=$(grep " $file\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
[ -n "$want" ] || err "no checksum for $file"
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$file" | cut -d' ' -f1)
else got=$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1); fi
[ "$want" = "$got" ] || err "checksum mismatch for $file"

chmod 0755 "$tmp/$file"
mv "$tmp/$file" "$dir/redrop"
printf 'Installed redrop to %s/redrop\n' "$dir"
"$dir/redrop" version || true

case ":$PATH:" in
  *":$dir:"*) ;;
  *) printf '\nNote: %s is not on your PATH. Add it, e.g.:\n  echo '\''export PATH="%s:$PATH"'\'' >> ~/.profile\n' "$dir" "$dir" ;;
esac

printf '\nNext: create an API token in the dashboard (%s/tokens), then run:\n  redrop login --url %s\n' "$BASE" "$BASE"
`))

var installPs1Tmpl = template.Must(template.New("ps1").Parse(`# redrop CLI installer for Windows. Usage: irm {{.Base}}/install.ps1 | iex
$ErrorActionPreference = 'Stop'
$base = '{{.Base}}'
$arch = 'amd64'
if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { $arch = 'arm64' }
$file = "redrop-windows-$arch.exe"
$dir = if ($env:REDROP_INSTALL_DIR) { $env:REDROP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'redrop\bin' }
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  Invoke-WebRequest -UseBasicParsing -Uri "$base/download/$file" -OutFile (Join-Path $tmp $file)
  $sums = (Invoke-WebRequest -UseBasicParsing -Uri "$base/download/SHA256SUMS").Content
  if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
  $line = ($sums -split "\r?\n") | Where-Object { $_ -match "\s$([regex]::Escape($file))$" } | Select-Object -First 1
  if (-not $line) { throw "no checksum for $file" }
  $want = ($line -split '\s+')[0].ToLower()
  $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $file)).Hash.ToLower()
  if ($want -ne $got) { throw "checksum mismatch for $file" }
  Move-Item -Force (Join-Path $tmp $file) (Join-Path $dir 'redrop.exe')
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not ($userPath -split ';' | Where-Object { $_ -eq $dir })) {
  [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $dir), 'User')
  $env:Path = $env:Path + ';' + $dir
  Write-Host "Added $dir to your user PATH (restart other terminals to pick it up)."
}
Write-Host "Installed redrop to $dir\redrop.exe"
& (Join-Path $dir 'redrop.exe') version
Write-Host ""
Write-Host "Next: create an API token in the dashboard ($base/tokens), then run:"
Write-Host "  redrop login --url $base"
`))

func (s *Server) installSh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	installShTmpl.Execute(w, map[string]string{"Base": s.cfg.PublicOrigin()})
}

func (s *Server) installPs1(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	installPs1Tmpl.Execute(w, map[string]string{"Base": s.cfg.PublicOrigin()})
}
