package server

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/auth"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

const (
	seenWindow     = 30 * time.Minute
	previewWindow  = 30 * time.Minute
	artifactLockN  = 50 // bad passwords per artifact per hour before lockout
	maxInlineText  = 1 << 20
	reservedPrefix = "/__redrop/"
)

func (s *Server) contentHandler() http.Handler {
	contentHost := s.cfg.ContentHost()
	return s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(host, ".")
		s.contentHeaders(w)
		if host == contentHost {
			s.serveApex(w, r)
			return
		}
		slug := strings.TrimSuffix(host, "."+contentHost)
		if strings.Contains(slug, ".") || !ids.ValidSlug(slug) {
			s.contentNotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && r.URL.Path == reservedPrefix+"open") {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/robots.txt" {
			robotsTxt(w, r)
			return
		}
		a, err := s.db.ArtifactBySlug(r.Context(), slug)
		if err != nil {
			s.contentNotFound(w, r)
			return
		}
		s.serveArtifact(w, r, a)
	}))
}

func (s *Server) contentHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Robots-Tag", "noindex, nofollow, noarchive, nosnippet, noimageindex")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "frame-ancestors 'self' "+s.cfg.PublicOrigin())
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
	h.Set("Cross-Origin-Resource-Policy", "same-site")
	if s.cfg.ContentURL.Scheme == "https" {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (s *Server) cookieName(base string) string {
	if s.cfg.Insecure {
		return "rd_" + base
	}
	return "__Host-rd_" + base
}

func (s *Server) setContentCookie(w http.ResponseWriter, name, value string, ttl time.Duration, crossSite bool) {
	c := &http.Cookie{Name: s.cookieName(name), Value: value, Path: "/", MaxAge: int(ttl.Seconds()), HttpOnly: true, Secure: !s.cfg.Insecure, SameSite: http.SameSiteLaxMode}
	if crossSite && !s.cfg.Insecure {
		// Lets the dashboard iframe preview work (partitioned per top-level site).
		c.SameSite, c.Partitioned = http.SameSiteNoneMode, true
	}
	http.SetCookie(w, c)
}

func isDocumentRequest(r *http.Request) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document" || d == "iframe"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html") || r.Header.Get("Accept") == "" || r.Header.Get("Accept") == "*/*"
}

var previewBotRE = regexp.MustCompile(`(?i)(bot\b|bot/|crawler|spider|facebookexternalhit|slack|discord|telegram|whatsapp|twitter|skype|linkedin|embedly|vkshare|pinterest|redditbot|mastodon|iframely|outlook|teams|google-?(read|page|structured)|bingpreview|preview)`)

func isPreviewBot(ua string) bool { return previewBotRE.MatchString(ua) }

// clientLabel turns a user agent into a coarse, non-identifying label.
func clientLabel(ua string) string {
	if ua == "" {
		return "unknown"
	}
	if isPreviewBot(ua) {
		for _, n := range []string{"Telegram", "Slack", "Discord", "WhatsApp", "Twitter", "facebook", "LinkedIn", "Skype", "Teams", "Google", "Bing"} {
			if strings.Contains(strings.ToLower(ua), strings.ToLower(n)) {
				return n + " link preview"
			}
		}
		return "link preview bot"
	}
	l := strings.ToLower(ua)
	browser := "other"
	switch {
	case strings.HasPrefix(l, "curl/"):
		return "curl"
	case strings.HasPrefix(l, "wget"):
		return "wget"
	case strings.HasPrefix(l, "redrop/"):
		return "redrop CLI"
	case strings.Contains(l, "edg/"):
		browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "firefox/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	case strings.Contains(l, "python"):
		return "Python"
	}
	osName := ""
	switch {
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad"):
		osName = "iOS"
	case strings.Contains(l, "android"):
		osName = "Android"
	case strings.Contains(l, "mac os"):
		osName = "macOS"
	case strings.Contains(l, "windows"):
		osName = "Windows"
	case strings.Contains(l, "linux"):
		osName = "Linux"
	}
	if osName != "" {
		return browser + " on " + osName
	}
	return browser
}

// accessPayload is what access cookies sign: artifact id, access generation, kind.
func accessPayload(a *store.Artifact, kind string) string {
	return a.ID + "|" + strconv.Itoa(a.AccessGen) + "|" + kind
}

func (s *Server) hasAccess(r *http.Request, a *store.Artifact, name string) (string, bool) {
	c, err := r.Cookie(s.cookieName(name))
	if err != nil {
		return "", false
	}
	p, ok := s.accessSigner.Verify(c.Value)
	if !ok {
		return "", false
	}
	parts := strings.Split(p, "|")
	if len(parts) != 3 || parts[0] != a.ID || parts[1] != strconv.Itoa(a.AccessGen) {
		return "", false
	}
	return parts[2], true
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	ctx := r.Context()
	now := time.Now()

	if strings.HasPrefix(r.URL.Path, reservedPrefix) {
		switch strings.TrimPrefix(r.URL.Path, reservedPrefix) {
		case "preview":
			s.handlePreview(w, r, a)
		case "open":
			s.handleOpen(w, r, a)
		default:
			s.contentNotFound(w, r)
		}
		return
	}

	_, preview := s.hasAccess(r, a, "p")
	status := a.Status(now)
	gated := a.PasswordHash != "" || a.MaxViews > 0
	_, granted := s.hasAccess(r, a, "a")

	switch {
	case preview && status != api.StatusRevoked:
		// Owner preview: always allowed (even expired/exhausted, while retained).
	case status == api.StatusRevoked || status == api.StatusExpired:
		s.gone(w, r)
		return
	case status == api.StatusExhausted && !granted:
		s.gone(w, r)
		return
	case gated && !granted:
		if !isDocumentRequest(r) {
			http.Error(w, "access required", http.StatusForbidden)
			return
		}
		if isPreviewBot(r.UserAgent()) {
			s.db.RecordView(ctx, a.ID, store.OutcomePreview, s.country(r), clientLabel(r.UserAgent()))
		}
		s.renderGate(w, r, a, "", http.StatusOK)
		return
	case !gated && isDocumentRequest(r) && r.Method == http.MethodGet:
		s.countUngatedView(w, r, a)
	}
	s.serveFile(w, r, a)
}

func (s *Server) countUngatedView(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	ua := r.UserAgent()
	if isPreviewBot(ua) {
		s.db.RecordView(r.Context(), a.ID, store.OutcomePreview, s.country(r), clientLabel(ua))
		return
	}
	if c, err := r.Cookie(s.cookieName("seen")); err == nil {
		if p, ok := s.seenSigner.Verify(c.Value); ok && p == a.ID {
			return
		}
	}
	s.setContentCookie(w, "seen", s.seenSigner.Sign(a.ID, time.Now().Add(seenWindow)), seenWindow, false)
	s.db.RecordView(r.Context(), a.ID, store.OutcomeView, s.country(r), clientLabel(ua))
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	p, ok := s.previewSigner.Verify(r.URL.Query().Get("t"))
	if !ok || p != a.ID+"|"+strconv.Itoa(a.AccessGen) {
		s.renderMessage(w, http.StatusForbidden, "Preview link expired", "Open the preview again from the dashboard.")
		return
	}
	s.setContentCookie(w, "p", s.accessSigner.Sign(accessPayload(a, "preview"), time.Now().Add(previewWindow)), previewWindow, true)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Same-origin form posts only.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	status := a.Status(time.Now())
	if status != api.StatusActive {
		s.gone(w, r)
		return
	}
	ip := s.clientIP(r)
	country, client := s.country(r), clientLabel(r.UserAgent())
	if a.PasswordHash != "" {
		if fails, _ := s.db.RecentFailures(ctx, a.ID, time.Now().Add(-time.Hour)); fails >= artifactLockN {
			s.renderGate(w, r, a, "Too many wrong passwords. This link is temporarily locked; try again later.", http.StatusTooManyRequests)
			return
		}
		if !s.unlockIPLimiter.Allow(ip) || !s.unlockLimiter.Allow(ip+"|"+a.ID) {
			s.renderGate(w, r, a, "Too many attempts. Wait a few minutes and try again.", http.StatusTooManyRequests)
			return
		}
		if !auth.CheckPassword(a.PasswordHash, r.PostForm.Get("password")) {
			s.db.RecordView(ctx, a.ID, store.OutcomeBadPassword, country, client)
			s.renderGate(w, r, a, "Wrong password.", http.StatusUnauthorized)
			return
		}
	}
	window := s.cfg.ViewWindowPassword
	if a.MaxViews > 0 {
		window = s.cfg.ViewWindowLimited
		ok, err := s.db.ConsumeView(ctx, a.ID, country, client)
		if err != nil {
			s.renderMessage(w, http.StatusInternalServerError, "Something went wrong", "Please try again.")
			return
		}
		if !ok {
			s.gone(w, r)
			return
		}
	} else {
		s.db.RecordView(ctx, a.ID, store.OutcomeView, country, client)
	}
	s.setContentCookie(w, "a", s.accessSigner.Sign(accessPayload(a, "visit"), time.Now().Add(window)), window, false)
	dest := r.PostForm.Get("next")
	if dest == "" || !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, "/\\") || strings.HasPrefix(dest, reservedPrefix) {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// --- file serving ---

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	ctx := r.Context()
	p := r.URL.Path
	switch a.Kind {
	case api.KindHTML:
		if p == "/" || p == "/index.html" {
			s.sendBlob(ctx, w, r, a, "index.html", false)
			return
		}
	case api.KindFile:
		if p == "/" {
			s.renderFileViewer(w, r, a)
			return
		}
		if strings.TrimPrefix(p, "/") == a.Entry {
			s.sendBlob(ctx, w, r, a, a.Entry, r.URL.Query().Has("download"))
			return
		}
	case api.KindBundle:
		s.serveBundle(w, r, a)
		return
	}
	s.contentNotFound(w, r)
}

func (s *Server) serveBundle(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	ctx := r.Context()
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" || p == "." {
		if a.Entry != "" {
			s.sendBlob(ctx, w, r, a, a.Entry, false)
			return
		}
		s.renderListing(w, r, a, "")
		return
	}
	if f, err := s.db.FileAt(ctx, a.ID, a.CurrentVersion, p); err == nil {
		s.sendFile(w, r, f, r.URL.Query().Has("download"))
		return
	}
	if _, err := s.db.FileAt(ctx, a.ID, a.CurrentVersion, p+"/index.html"); err == nil {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, "/"+p+"/", http.StatusMovedPermanently)
			return
		}
		s.sendBlob(ctx, w, r, a, p+"/index.html", false)
		return
	}
	if files, err := s.db.DirFiles(ctx, a.ID, a.CurrentVersion, p+"/"); err == nil && len(files) > 0 {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, "/"+p+"/", http.StatusMovedPermanently)
			return
		}
		s.renderListing(w, r, a, p+"/")
		return
	}
	if a.SPA && a.Entry != "" && path.Ext(p) == "" {
		s.sendBlob(ctx, w, r, a, a.Entry, false)
		return
	}
	s.contentNotFound(w, r)
}

func (s *Server) sendBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, a *store.Artifact, p string, download bool) {
	f, err := s.db.FileAt(ctx, a.ID, a.CurrentVersion, p)
	if err != nil {
		s.contentNotFound(w, r)
		return
	}
	s.sendFile(w, r, f, download)
}

func (s *Server) sendFile(w http.ResponseWriter, r *http.Request, f *store.File, download bool) {
	fh, err := s.blobs.Open(f.Blob)
	if err != nil {
		s.log.Error("blob missing", "blob", f.Blob)
		s.renderMessage(w, http.StatusInternalServerError, "Content unavailable", "The stored content could not be read.")
		return
	}
	defer fh.Close()
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("ETag", `"`+f.Blob+`"`)
	name := path.Base(f.Path)
	disp := "inline"
	if download {
		disp = "attachment"
	}
	h.Set("Content-Disposition", disp+"; filename*=UTF-8''"+url.PathEscape(name))
	http.ServeContent(w, r, "", time.Time{}, fh)
}

// --- pages ---

var pageCSS = template.CSS(`
:root{color-scheme:light dark;--bg:#fafafa;--fg:#18181b;--muted:#71717a;--card:#fff;--border:#e4e4e7;--accent:#18181b;--accentfg:#fafafa;--err:#dc2626}
@media(prefers-color-scheme:dark){:root{--bg:#09090b;--fg:#fafafa;--muted:#a1a1aa;--card:#18181b;--border:#27272a;--accent:#fafafa;--accentfg:#18181b}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:var(--bg);color:var(--fg);font:15px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;padding:24px}
.card{width:100%;max-width:420px;background:var(--card);border:1px solid var(--border);border-radius:14px;padding:28px;box-shadow:0 1px 2px rgba(0,0,0,.05)}
.wide{max-width:960px}h1{font-size:18px;margin:0 0 6px}p{margin:0 0 16px;color:var(--muted)}
input{width:100%;padding:10px 12px;border-radius:9px;border:1px solid var(--border);background:transparent;color:var(--fg);font:inherit;margin-bottom:12px}
button,.btn{display:inline-flex;align-items:center;justify-content:center;gap:6px;width:100%;padding:10px 14px;border:0;border-radius:9px;background:var(--accent);color:var(--accentfg);font:inherit;font-weight:600;cursor:pointer;text-decoration:none}
.btn.secondary{background:transparent;color:var(--fg);border:1px solid var(--border)}
.err{color:var(--err);font-size:14px;margin:-4px 0 12px}.foot{margin-top:18px;font-size:12px;color:var(--muted);text-align:center}
.meta{font-size:13px;color:var(--muted)}.row{display:flex;gap:10px;align-items:center;justify-content:space-between;flex-wrap:wrap;margin-bottom:16px}
.row .btn{width:auto}.preview{border:1px solid var(--border);border-radius:10px;overflow:hidden;background:var(--bg)}
.preview img,.preview video{display:block;max-width:100%;margin:0 auto}.preview iframe{width:100%;height:75vh;border:0}
pre{margin:0;padding:16px;overflow:auto;font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;max-height:75vh;white-space:pre-wrap;word-break:break-word}
ul{list-style:none;margin:0;padding:0}li a{display:flex;justify-content:space-between;padding:8px 10px;border-radius:8px;color:var(--fg);text-decoration:none}li a:hover{background:var(--bg)}
li span{color:var(--muted);font-size:13px}`)

var pages = template.Must(template.New("").Funcs(template.FuncMap{"bytes": humanBytes}).Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex,nofollow"><meta name="referrer" content="no-referrer"><title>{{.PageTitle}}</title><style>{{.CSS}}</style></head><body>{{end}}
{{define "foot"}}</body></html>{{end}}
{{define "gate"}}{{template "head" .}}<main class="card">
<h1>{{if .Password}}This link is password protected{{else}}Someone shared something with you{{end}}</h1>
<p>{{if .Remaining}}{{if eq .Remaining 1}}It can be opened only once. After you open it, the link stops working for everyone else.{{else}}It can be opened {{.Remaining}} more times.{{end}}{{else}}Enter the password to continue.{{end}}</p>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<form method="post" action="/__redrop/open">
<input type="hidden" name="next" value="{{.Next}}">
{{if .Password}}<input type="password" name="password" placeholder="Password" autocomplete="current-password" autofocus required>{{end}}
<button type="submit">Open</button>
</form><div class="foot">Shared with redrop</div></main>{{template "foot" .}}{{end}}
{{define "message"}}{{template "head" .}}<main class="card"><h1>{{.Heading}}</h1><p>{{.Body}}</p><div class="foot">Shared with redrop</div></main>{{template "foot" .}}{{end}}
{{define "file"}}{{template "head" .}}<main class="card wide">
<div class="row"><div><h1>{{.Name}}</h1><div class="meta">{{bytes .Size}} · {{.Type}}</div></div>
<div style="display:flex;gap:8px"><a class="btn secondary" href="{{.Raw}}" target="_blank" rel="noopener">Open raw</a><a class="btn" href="{{.Raw}}?download">Download</a></div></div>
{{if .Image}}<div class="preview"><img src="{{.Raw}}" alt=""></div>
{{else if .Video}}<div class="preview"><video src="{{.Raw}}" controls preload="metadata"></video></div>
{{else if .Audio}}<div class="preview" style="padding:16px"><audio src="{{.Raw}}" controls style="width:100%"></audio></div>
{{else if .PDF}}<div class="preview"><iframe src="{{.Raw}}" title="PDF"></iframe></div>
{{else if .Text}}<div class="preview"><pre>{{.Text}}</pre></div>{{if .Truncated}}<p class="meta" style="margin-top:8px">Showing the first 1 MB. Download for the full file.</p>{{end}}
{{end}}<div class="foot">Shared with redrop</div></main>{{template "foot" .}}{{end}}
{{define "listing"}}{{template "head" .}}<main class="card wide"><h1>/{{.Dir}}</h1><p class="meta">{{len .Items}} items</p><ul>
{{if .Dir}}<li><a href="../">../</a></li>{{end}}
{{range .Items}}<li><a href="{{.Href}}">{{.Name}}<span>{{if .IsDir}}folder{{else}}{{bytes .Size}}{{end}}</span></a></li>{{end}}
</ul><div class="foot">Shared with redrop</div></main>{{template "foot" .}}{{end}}
{{define "apex"}}{{template "head" .}}<main class="card"><h1>redrop</h1><p>This domain hosts shared links. Content here is uploaded by users of a redrop server and is not indexed.</p>
<div class="foot">To report abuse, contact the operator of the redrop server that shared the link.</div></main>{{template "foot" .}}{{end}}
`))

type pageBase struct {
	PageTitle string
	CSS       template.CSS
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("template", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; media-src 'self'; frame-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'self' "+s.cfg.PublicOrigin())
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) renderMessage(w http.ResponseWriter, status int, heading, body string) {
	s.render(w, status, "message", struct {
		pageBase
		Heading, Body string
	}{pageBase{"redrop", pageCSS}, heading, body})
}

func (s *Server) contentNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderMessage(w, http.StatusNotFound, "Not found", "This link doesn't exist, or it has expired.")
}

func (s *Server) gone(w http.ResponseWriter, r *http.Request) {
	s.renderMessage(w, http.StatusGone, "This link is no longer available", "It may have expired, reached its view limit, or been revoked by the person who shared it.")
}

func (s *Server) renderGate(w http.ResponseWriter, r *http.Request, a *store.Artifact, errMsg string, status int) {
	remaining := 0
	if a.MaxViews > 0 {
		remaining = a.MaxViews - a.Views
	}
	next := r.URL.RequestURI()
	if r.Method == http.MethodPost {
		next = r.PostForm.Get("next")
	}
	s.render(w, status, "gate", struct {
		pageBase
		Password  bool
		Remaining int
		Error     string
		Next      string
	}{pageBase{"Shared link", pageCSS}, a.PasswordHash != "", remaining, errMsg, next})
}

func (s *Server) renderFileViewer(w http.ResponseWriter, r *http.Request, a *store.Artifact) {
	f, err := s.db.FileAt(r.Context(), a.ID, a.CurrentVersion, a.Entry)
	if err != nil {
		s.contentNotFound(w, r)
		return
	}
	ct := f.ContentType
	data := struct {
		pageBase
		Name, Type, Raw          string
		Size                     int64
		Image, Video, Audio, PDF bool
		Text                     string
		Truncated                bool
	}{pageBase: pageBase{f.Path, pageCSS}, Name: f.Path, Type: strings.Split(ct, ";")[0], Raw: "/" + url.PathEscape(f.Path), Size: f.Size}
	switch {
	case strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "image/svg"):
		data.Image = true
	case strings.HasPrefix(ct, "image/svg"):
		data.Image = true
	case strings.HasPrefix(ct, "video/"):
		data.Video = true
	case strings.HasPrefix(ct, "audio/"):
		data.Audio = true
	case strings.HasPrefix(ct, "application/pdf"):
		data.PDF = true
	case strings.HasPrefix(ct, "text/") || strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "application/xml"):
		if fh, err := s.blobs.Open(f.Blob); err == nil {
			b, _ := io.ReadAll(io.LimitReader(fh, maxInlineText))
			fh.Close()
			if utf8.Valid(b) || len(b) == maxInlineText {
				data.Text = strings.ToValidUTF8(string(b), "�")
				data.Truncated = f.Size > maxInlineText
			}
		}
	}
	s.render(w, http.StatusOK, "file", data)
}

type listItem struct {
	Name, Href string
	Size       int64
	IsDir      bool
}

func (s *Server) renderListing(w http.ResponseWriter, r *http.Request, a *store.Artifact, dir string) {
	files, err := s.db.DirFiles(r.Context(), a.ID, a.CurrentVersion, dir)
	if err != nil {
		s.contentNotFound(w, r)
		return
	}
	seen := map[string]bool{}
	var items []listItem
	for _, f := range files {
		rest := strings.TrimPrefix(f.Path, dir)
		if i := strings.Index(rest, "/"); i >= 0 {
			d := rest[:i]
			if !seen[d] {
				seen[d] = true
				items = append(items, listItem{Name: d + "/", Href: url.PathEscape(d) + "/", IsDir: true})
			}
			continue
		}
		items = append(items, listItem{Name: rest, Href: url.PathEscape(rest), Size: f.Size})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return items[i].Name < items[j].Name
	})
	s.render(w, http.StatusOK, "listing", struct {
		pageBase
		Dir   string
		Items []listItem
	}{pageBase{"Files", pageCSS}, dir, items})
}

func (s *Server) serveApex(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		s.render(w, http.StatusOK, "apex", pageBase{"redrop", pageCSS})
	case "/robots.txt":
		robotsTxt(w, r)
	case "/install.sh":
		s.installSh(w, r)
	case "/install.ps1":
		s.installPs1(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/download/") {
			r.SetPathValue("file", strings.TrimPrefix(r.URL.Path, "/download/"))
			s.downloadCLI(w, r)
			return
		}
		s.contentNotFound(w, r)
	}
}
