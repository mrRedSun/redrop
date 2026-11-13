package content

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

//go:embed templates/*.html templates/style.css
var templateFS embed.FS

type pages struct {
	tmpl *template.Template
	css  template.CSS
	csp  string
}

func newPages(cfg *config.Config) *pages {
	css, err := templateFS.ReadFile("templates/style.css")
	if err != nil {
		panic(err)
	}
	t := template.Must(template.New("").Funcs(template.FuncMap{"bytes": humanBytes}).ParseFS(templateFS, "templates/*.html"))
	return &pages{
		tmpl: t,
		css:  template.CSS(css),
		csp:  "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; media-src 'self'; frame-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'self' " + cfg.PublicOrigin(),
	}
}

type pageData struct {
	Title string
	CSS   template.CSS
	Data  any
}

func (p *pages) render(w http.ResponseWriter, status int, name, title string, data any) {
	var buf bytes.Buffer
	if err := p.tmpl.ExecuteTemplate(&buf, name, pageData{Title: title, CSS: p.css, Data: data}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", p.csp)
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (p *pages) message(w http.ResponseWriter, status int, heading, body string) {
	p.render(w, status, "message.html", "redrop", map[string]string{"Heading": heading, "Body": body})
}

func (p *pages) notFound(w http.ResponseWriter) {
	p.message(w, http.StatusNotFound, "Not found", "This link doesn't exist, or it has expired.")
}

func (p *pages) gone(w http.ResponseWriter) {
	p.message(w, http.StatusGone, "This link is no longer available", "It may have expired, reached its view limit, or been revoked by the person who shared it.")
}

// gate never reveals the artifact title or kind (link previews see it too).
func (p *pages) gate(w http.ResponseWriter, status int, a *store.Artifact, errMsg, next string) {
	remaining := 0
	if a.MaxViews > 0 {
		remaining = a.MaxViews - a.Views
	}
	p.render(w, status, "gate.html", "Shared link", map[string]any{
		"Password": a.PasswordHash != "", "Remaining": remaining, "Error": errMsg, "Next": next,
		"Action": "/" + sharing.ReservedPrefix + "/open",
	})
}

type viewerData struct {
	Name, Type, Raw          string
	Size                     int64
	Image, Video, Audio, PDF bool
	Text                     string
	Truncated                bool
}

func (p *pages) fileViewer(w http.ResponseWriter, sh *sharing.Service, f *store.File) {
	ct := f.ContentType
	d := viewerData{Name: f.Path, Type: strings.Split(ct, ";")[0], Raw: "/" + url.PathEscape(f.Path), Size: f.Size}
	switch {
	case strings.HasPrefix(ct, "image/"):
		d.Image = true
	case strings.HasPrefix(ct, "video/"):
		d.Video = true
	case strings.HasPrefix(ct, "audio/"):
		d.Audio = true
	case strings.HasPrefix(ct, "application/pdf"):
		d.PDF = true
	case strings.HasPrefix(ct, "text/") || strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "application/xml"):
		if text, truncated, ok := sh.TextPreview(f, maxInlineText); ok {
			d.Text, d.Truncated = text, truncated
		}
	}
	p.render(w, http.StatusOK, "file.html", f.Path, d)
}

type listItem struct {
	Name, Href string
	Size       int64
	IsDir      bool
}

func (p *pages) listing(w http.ResponseWriter, dir string, entries []sharing.DirEntry) {
	items := make([]listItem, 0, len(entries))
	for _, e := range entries {
		it := listItem{Name: e.Name, Href: url.PathEscape(e.Name), Size: e.Size, IsDir: e.IsDir}
		if e.IsDir {
			it.Name += "/"
			it.Href += "/"
		}
		items = append(items, it)
	}
	p.render(w, http.StatusOK, "listing.html", "Files", map[string]any{"Dir": dir, "Items": items})
}

// Apex renders the content-domain landing page.
func (p *pages) apex(w http.ResponseWriter) {
	p.render(w, http.StatusOK, "apex.html", "redrop", nil)
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
