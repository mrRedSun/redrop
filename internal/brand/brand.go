// Package brand serves the redrop icons. The files under icons/ are
// generated from iconsrc; regenerate them with `go generate ./internal/brand`.
package brand

//go:generate go run ./iconsrc/gen icons

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"path"
	"time"
)

//go:embed icons/favicon.svg icons/favicon.ico icons/apple-touch-icon.png
var iconFS embed.FS

// CacheShared lets browsers and shared caches keep server-owned icons for a day.
const CacheShared = "public, max-age=86400"

type icon struct {
	data        []byte
	contentType string
	etag        string
}

var icons = load(map[string]string{
	"favicon.svg":          "image/svg+xml",
	"favicon.ico":          "image/x-icon",
	"apple-touch-icon.png": "image/png",
})

func load(types map[string]string) map[string]icon {
	out := make(map[string]icon, len(types))
	for name, ct := range types {
		b, err := iconFS.ReadFile("icons/" + name)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(b)
		out[name] = icon{data: b, contentType: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	}
	return out
}

// SVGDataURL is the redrop mark as a data: URL, for pages that must not
// fetch it.
func SVGDataURL() string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(icons["favicon.svg"].data)
}

// Has reports whether name (e.g. "favicon.ico") is a redrop icon.
func Has(name string) bool {
	_, ok := icons[name]
	return ok
}

// Serve writes the named icon with the given Cache-Control. It reports false
// without writing anything when name is not a redrop icon.
func Serve(w http.ResponseWriter, r *http.Request, name, cacheControl string) bool {
	ic, ok := icons[name]
	if !ok {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", ic.contentType)
	h.Set("Cache-Control", cacheControl)
	h.Set("ETag", ic.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(ic.data))
	return true
}

// Handler serves every icon at /<name> with CacheShared.
func Handler(w http.ResponseWriter, r *http.Request) {
	if !Serve(w, r, path.Base(r.URL.Path), CacheShared) {
		http.NotFound(w, r)
	}
}

// Paths are the root paths that Handler answers.
var Paths = []string{"/favicon.svg", "/favicon.ico", "/apple-touch-icon.png"}
