package content

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	// fileCSP is the file page's policy with the viewer script and stylesheet
	// of origin %[1]s allowed.
	fileCSP string
}

func newPages(cfg *config.Config) *pages {
	css, err := templateFS.ReadFile("templates/style.css")
	if err != nil {
		panic(err)
	}
	t := template.Must(template.New("").Funcs(template.FuncMap{
		"bytes": humanBytes,
		"iso":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"utc":   func(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04 UTC") },
	}).ParseFS(templateFS, "templates/*.html"))
	return &pages{
		tmpl:    t,
		css:     template.CSS(css),
		csp:     "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; media-src 'self'; frame-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'self' " + cfg.PublicOrigin(),
		fileCSP: "default-src 'none'; style-src 'unsafe-inline' %[1]s/__redrop/viewer.css; script-src %[1]s/__redrop/viewer.js; img-src 'self'; media-src 'self'; frame-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'self' " + cfg.PublicOrigin(),
	}
}

type pageData struct {
	Title    string
	CSS      template.CSS
	IconBase string // "" leaves the icon to the browser's /favicon.ico request
	Data     any
}

// Icon links for server-rendered pages.
const (
	serverIcons   = reservedPrefix // redrop's own pages: always the redrop icon (reserved, so never an artifact file)
	artifactIcons = ""             // pages showing artifact content: /favicon.ico decides
)

// render writes a server page. csp replaces the default policy when set.
func (p *pages) render(w http.ResponseWriter, status int, name, title, icons string, data any, csp string) {
	var buf bytes.Buffer
	if err := p.tmpl.ExecuteTemplate(&buf, name, pageData{Title: title, CSS: p.css, IconBase: icons, Data: data}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	if csp == "" {
		csp = p.csp
	}
	h.Set("Content-Security-Policy", csp)
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (p *pages) message(w http.ResponseWriter, status int, heading, body string) {
	p.render(w, status, "message.html", "redrop", serverIcons, map[string]string{"Heading": heading, "Body": body}, "")
}

func (p *pages) notFound(w http.ResponseWriter) {
	p.message(w, http.StatusNotFound, "Not found", "This link doesn't exist, or it has expired.")
}

// gate never reveals the artifact title or kind (link previews see it too).
func (p *pages) gate(w http.ResponseWriter, status int, a *store.Artifact, errMsg, next string) {
	remaining := 0
	if a.MaxViews > 0 {
		remaining = a.MaxViews - a.Views
	}
	p.render(w, status, "gate.html", "Shared link", serverIcons, map[string]any{
		"Password": a.PasswordHash != "", "Remaining": remaining, "Error": errMsg, "Next": next,
		"Action": "/" + sharing.ReservedPrefix + "/open",
	}, "")
}

type fileData struct {
	Name, Type, Raw, Download string
	Size                      int64
	Image, Video, Audio, PDF  bool
	Text                      string
	Truncated                 bool
	Info                      *panelData // nil without the info button
	OldVersion                int        // the older version shown, 0 for the current one
	Latest                    string
}

// fileViewer renders redrop's own page for a file artifact, with the info
// panel rendered natively when info is set. base is where the shown
// version's files are served ("" for the current version); self is this
// origin, for the policy that allows the viewer's script and stylesheet.
func (p *pages) fileViewer(w http.ResponseWriter, sh *sharing.Service, f *store.File, self, base string, info *sharing.ViewerInfo) {
	ct := f.ContentType
	raw := base + "/" + url.PathEscape(f.Path)
	d := fileData{Name: f.Path, Type: strings.Split(ct, ";")[0], Raw: raw, Download: raw + "?download", Size: f.Size, Latest: "/"}
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
	csp := ""
	if info != nil {
		// The file page lives at "/" only; its version links point there.
		panel := panelFor(*info, &url.URL{Path: "/"})
		d.Info = &panel
		if info.Version != info.Current {
			d.OldVersion = info.Version
		}
		csp = fmt.Sprintf(p.fileCSP, self)
	}
	p.render(w, http.StatusOK, "file.html", f.Path, artifactIcons, d, csp)
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
	p.render(w, http.StatusOK, "listing.html", "Files", artifactIcons, map[string]any{"Dir": dir, "Items": items}, "")
}

// Apex renders the content-domain landing page.
func (p *pages) apex(w http.ResponseWriter) {
	p.render(w, http.StatusOK, "apex.html", "redrop", "/", nil, "")
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
