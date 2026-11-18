// Package contenttype picks the Content-Type of an artifact file: by
// extension, then by sniffing its first bytes. The server uses it for plain
// uploads, and the CLI for end-to-end encrypted ones, which the server can't
// sniff (DESIGN.md "End-to-end encrypted links"), so both follow one table.
package contenttype

import (
	"mime"
	"net/http"
	"path"
	"strings"
)

// SniffLen is how many leading bytes For looks at.
const SniffLen = 512

func init() {
	// Distroless images ship no /etc/mime.types; register what artifacts use.
	for ext, typ := range map[string]string{
		".md": "text/markdown; charset=utf-8", ".markdown": "text/markdown; charset=utf-8",
		".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
		".csv": "text/csv; charset=utf-8", ".tsv": "text/tab-separated-values; charset=utf-8",
		".yaml": "text/yaml; charset=utf-8", ".yml": "text/yaml; charset=utf-8", ".toml": "text/plain; charset=utf-8",
		".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8", ".cjs": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
		".json": "application/json", ".map": "application/json", ".webmanifest": "application/manifest+json",
		".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
		".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/mp4", ".flac": "audio/flac",
		".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
		".ico": "image/x-icon", ".avif": "image/avif", ".webp": "image/webp", ".svg": "image/svg+xml",
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
		".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar", ".7z": "application/x-7z-compressed",
		".pdf": "application/pdf", ".wasm": "application/wasm", ".xml": "application/xml",
		".py": "text/plain; charset=utf-8", ".go": "text/plain; charset=utf-8", ".ts": "text/plain; charset=utf-8",
		".tsx": "text/plain; charset=utf-8", ".sh": "text/plain; charset=utf-8", ".rs": "text/plain; charset=utf-8",
	} {
		mime.AddExtensionType(ext, typ)
	}
}

// For picks a type for the file name by extension, falling back to sniffing
// head (its first SniffLen bytes). Text types get a UTF-8 charset.
func For(name string, head []byte) string {
	t := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if t == "" {
		t = http.DetectContentType(head)
	}
	if (strings.HasPrefix(t, "text/") || t == "application/json" || t == "image/svg+xml") && !strings.Contains(t, "charset") {
		t += "; charset=utf-8"
	}
	return t
}
