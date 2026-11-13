// Package web serves the embedded dashboard SPA, CLI installers, and CLI
// binaries for download.
package web

import (
	"bytes"
	"embed"
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

//go:embed install/install.sh.tmpl install/install.ps1.tmpl
var installFS embed.FS

var installers = template.Must(template.ParseFS(installFS, "install/*.tmpl"))

type Web struct {
	assets       fs.FS  // built dashboard; nil when not embedded
	cliDir       string // directory of prebuilt CLI binaries
	publicOrigin string
	index        []byte
}

func New(assets fs.FS, cliDir, publicOrigin string) *Web {
	w := &Web{assets: assets, cliDir: cliDir, publicOrigin: publicOrigin}
	if assets != nil {
		if b, err := fs.ReadFile(assets, "index.html"); err == nil {
			w.index = b
		} else {
			w.assets = nil
		}
	}
	return w
}

// SPA serves static assets, falling back to index.html for client routes.
func (wb *Web) SPA(w http.ResponseWriter, r *http.Request) {
	if wb.assets == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("redrop API is running; this binary was built without the dashboard.\n"))
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p != "" && p != "index.html" {
		if st, err := fs.Stat(wb.assets, p); err == nil && !st.IsDir() {
			if strings.HasPrefix(p, "assets/") {
				// Vite fingerprints asset names.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, wb.assets, p)
			return
		}
		if ext := path.Ext(p); ext != "" && ext != ".html" {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(wb.index))
}

// Installers serves /install.sh, /install.ps1 and /download/{file}.
func (wb *Web) Installers() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /install.sh", wb.script("install.sh.tmpl", "text/x-shellscript; charset=utf-8"))
	mux.HandleFunc("GET /install.ps1", wb.script("install.ps1.tmpl", "text/plain; charset=utf-8"))
	mux.HandleFunc("GET /download/{file}", wb.download)
	return mux
}

func (wb *Web) script(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := installers.ExecuteTemplate(&buf, name, map[string]string{"Base": wb.publicOrigin}); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(buf.Bytes())
	}
}

var cliFileRE = regexp.MustCompile(`^(redrop-(linux|darwin|windows)-(amd64|arm64)(\.exe)?|SHA256SUMS)$`)

func (wb *Web) download(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if wb.cliDir == "" || !cliFileRE.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(wb.cliDir, name))
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
