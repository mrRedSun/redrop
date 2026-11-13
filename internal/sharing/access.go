package sharing

import (
	"context"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

const (
	seenWindow    = 30 * time.Minute
	previewWindow = 30 * time.Minute
	lockThreshold = 50 // wrong passwords per artifact per hour before lockout
)

// Visitor is an anonymous client of a shared link.
type Visitor struct {
	IP        string
	Country   string
	UserAgent string
}

var previewBotRE = regexp.MustCompile(`(?i)(bot\b|bot/|crawler|spider|facebookexternalhit|slack|discord|telegram|whatsapp|twitter|skype|linkedin|embedly|vkshare|pinterest|redditbot|mastodon|iframely|outlook|teams|google-?(read|page|structured)|bingpreview|preview)`)

// IsLinkPreview reports whether the client is a chat/social link unfurler.
func (v Visitor) IsLinkPreview() bool { return previewBotRE.MatchString(v.UserAgent) }

// Label is a coarse, non-identifying client description for the access log.
func (v Visitor) Label() string {
	ua := v.UserAgent
	l := strings.ToLower(ua)
	if ua == "" {
		return "unknown"
	}
	if v.IsLinkPreview() {
		for _, n := range []string{"Telegram", "Slack", "Discord", "WhatsApp", "Twitter", "facebook", "LinkedIn", "Skype", "Teams", "Google", "Bing"} {
			if strings.Contains(l, strings.ToLower(n)) {
				return n + " link preview"
			}
		}
		return "link preview bot"
	}
	switch {
	case strings.HasPrefix(l, "curl/"):
		return "curl"
	case strings.HasPrefix(l, "wget"):
		return "wget"
	case strings.HasPrefix(l, "redrop/"):
		return "redrop CLI"
	case strings.Contains(l, "python"):
		return "Python"
	}
	browser := "other"
	switch {
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
	}
	for _, o := range []struct{ needle, name string }{{"iphone", "iOS"}, {"ipad", "iOS"}, {"android", "Android"}, {"mac os", "macOS"}, {"windows", "Windows"}, {"linux", "Linux"}} {
		if strings.Contains(l, o.needle) {
			return browser + " on " + o.name
		}
	}
	return browser
}

// Lookup finds an artifact by its secret slug.
func (s *Service) Lookup(ctx context.Context, slug string) (*store.Artifact, error) {
	if !ids.ValidSlug(slug) {
		return nil, apperr.NotFound("not found")
	}
	a, err := s.db.ArtifactBySlug(ctx, slug)
	if err != nil {
		return nil, apperr.NotFound("not found")
	}
	return a, nil
}

// Gated reports whether visitors must pass the Open/password page.
func Gated(a *store.Artifact) bool { return a.PasswordHash != "" || a.MaxViews > 0 }

// Grants are the signed cookie values a visitor presented.
type Grants struct {
	Visit   string
	Preview string
}

func grantPayload(a *store.Artifact, kind string) string {
	return a.ID + "|" + strconv.Itoa(a.AccessGen) + "|" + kind
}

func (s *Service) validGrant(signer *security.Signer, value string, a *store.Artifact, kind string) bool {
	p, ok := signer.Verify(value)
	return ok && p == grantPayload(a, kind)
}

// Decision is what to do with a request for artifact content.
type Decision int

const (
	Serve Decision = iota
	ServeOwnerPreview
	ShowGate
	ShowGone
)

// Decide applies lifecycle and gate rules. Grants are invalidated whenever
// the access generation changes (password change, rotation, revocation).
func (s *Service) Decide(a *store.Artifact, g Grants, now time.Time) Decision {
	status := a.Status(now)
	if status != api.StatusRevoked && s.validGrant(s.previewSigner, g.Preview, a, "preview") {
		return ServeOwnerPreview
	}
	visit := s.validGrant(s.accessSigner, g.Visit, a, "visit")
	switch {
	case status == api.StatusRevoked || status == api.StatusExpired:
		return ShowGone
	case status == api.StatusExhausted && !visit:
		return ShowGone
	case Gated(a) && !visit:
		return ShowGate
	}
	return Serve
}

// Grant is a cookie value with its lifetime.
type Grant struct {
	Value string
	TTL   time.Duration
}

// RecordView counts an ungated document view at most once per seenWindow per
// browser. It returns the new de-duplication cookie, or nil.
func (s *Service) RecordView(ctx context.Context, a *store.Artifact, v Visitor, seen string) *Grant {
	if v.IsLinkPreview() {
		s.db.RecordView(ctx, a.ID, store.OutcomePreview, v.Country, v.Label())
		return nil
	}
	if p, ok := s.seenSigner.Verify(seen); ok && p == a.ID {
		return nil
	}
	s.db.RecordView(ctx, a.ID, store.OutcomeView, v.Country, v.Label())
	return &Grant{Value: s.seenSigner.Sign(a.ID, time.Now().Add(seenWindow)), TTL: seenWindow}
}

// RecordGateImpression logs link-preview fetches of gated links.
func (s *Service) RecordGateImpression(ctx context.Context, a *store.Artifact, v Visitor) {
	if v.IsLinkPreview() {
		s.db.RecordView(ctx, a.ID, store.OutcomePreview, v.Country, v.Label())
	}
}

// Open handles the gate: verifies the password (rate limited), consumes a
// view when limited, and returns a visit grant.
func (s *Service) Open(ctx context.Context, a *store.Artifact, v Visitor, password string) (*Grant, error) {
	if a.Status(time.Now()) != api.StatusActive {
		return nil, apperr.Gone("this link is no longer available")
	}
	if a.PasswordHash != "" {
		if fails, _ := s.db.RecentFailures(ctx, a.ID, time.Now().Add(-time.Hour)); fails >= lockThreshold {
			return nil, apperr.RateLimited("Too many wrong passwords. This link is temporarily locked; try again later.")
		}
		if !s.unlockIPLimiter.Allow(v.IP) || !s.unlockLimiter.Allow(v.IP+"|"+a.ID) {
			return nil, apperr.RateLimited("Too many attempts. Wait a few minutes and try again.")
		}
		if !security.CheckPassword(a.PasswordHash, password) {
			s.db.RecordView(ctx, a.ID, store.OutcomeBadPassword, v.Country, v.Label())
			return nil, apperr.Unauthorized("Wrong password.")
		}
	}
	window := s.cfg.ViewWindowPassword
	if a.MaxViews > 0 {
		window = s.cfg.ViewWindowLimited
		ok, err := s.db.ConsumeView(ctx, a.ID, v.Country, v.Label())
		if err != nil {
			return nil, apperr.Internal(err)
		}
		if !ok {
			return nil, apperr.Gone("this link is no longer available")
		}
	} else {
		s.db.RecordView(ctx, a.ID, store.OutcomeView, v.Country, v.Label())
	}
	return &Grant{Value: s.accessSigner.Sign(grantPayload(a, "visit"), time.Now().Add(window)), TTL: window}, nil
}

// RedeemPreview exchanges an owner preview token for a preview cookie.
func (s *Service) RedeemPreview(a *store.Artifact, token string) (*Grant, bool) {
	p, ok := s.previewSigner.Verify(token)
	if !ok || p != a.ID+"|"+strconv.Itoa(a.AccessGen) {
		return nil, false
	}
	return &Grant{Value: s.previewSigner.Sign(grantPayload(a, "preview"), time.Now().Add(previewWindow)), TTL: previewWindow}, true
}

// --- content resolution ---

// TargetKind is how a request path maps onto artifact content.
type TargetKind int

const (
	TargetNotFound TargetKind = iota
	TargetFile                // serve File
	TargetViewer              // file-kind landing page for File
	TargetListing             // directory listing of Dir
	TargetRedirect            // redirect to Location
)

type Target struct {
	Kind     TargetKind
	File     *store.File
	Dir      string // "" for root, else "dir/"
	Location string
}

func (s *Service) fileAt(ctx context.Context, a *store.Artifact, p string) *store.File {
	f, err := s.db.FileAt(ctx, a.ID, a.CurrentVersion, p)
	if err != nil {
		return nil
	}
	return f
}

// ResolvePath maps a URL path onto the current version's content.
func (s *Service) ResolvePath(ctx context.Context, a *store.Artifact, urlPath string) Target {
	switch a.Kind {
	case api.KindHTML:
		if urlPath == "/" || urlPath == "/index.html" {
			if f := s.fileAt(ctx, a, "index.html"); f != nil {
				return Target{Kind: TargetFile, File: f}
			}
		}
		return Target{}
	case api.KindFile:
		f := s.fileAt(ctx, a, a.Entry)
		switch {
		case f == nil:
			return Target{}
		case urlPath == "/":
			return Target{Kind: TargetViewer, File: f}
		case strings.TrimPrefix(urlPath, "/") == a.Entry:
			return Target{Kind: TargetFile, File: f}
		}
		return Target{}
	}
	// Bundles.
	p := strings.TrimPrefix(path.Clean(urlPath), "/")
	trailing := strings.HasSuffix(urlPath, "/")
	if p == "" || p == "." {
		if a.Entry != "" {
			if f := s.fileAt(ctx, a, a.Entry); f != nil {
				return Target{Kind: TargetFile, File: f}
			}
		}
		return Target{Kind: TargetListing}
	}
	if f := s.fileAt(ctx, a, p); f != nil {
		return Target{Kind: TargetFile, File: f}
	}
	if f := s.fileAt(ctx, a, p+"/index.html"); f != nil {
		if !trailing {
			return Target{Kind: TargetRedirect, Location: "/" + p + "/"}
		}
		return Target{Kind: TargetFile, File: f}
	}
	if files, _ := s.db.DirFiles(ctx, a.ID, a.CurrentVersion, p+"/"); len(files) > 0 {
		if !trailing {
			return Target{Kind: TargetRedirect, Location: "/" + p + "/"}
		}
		return Target{Kind: TargetListing, Dir: p + "/"}
	}
	if a.SPA && a.Entry != "" && path.Ext(p) == "" {
		if f := s.fileAt(ctx, a, a.Entry); f != nil {
			return Target{Kind: TargetFile, File: f}
		}
	}
	return Target{}
}

// OpenBlob opens stored content for streaming.
func (s *Service) OpenBlob(f *store.File) (*os.File, error) {
	fh, err := s.blobs.Open(f.Blob)
	if err != nil {
		s.log.Error("stored content missing", "blob", f.Blob)
		return nil, apperr.Internal(err)
	}
	return fh, nil
}

// TextPreview returns up to max bytes of a text file as valid UTF-8.
func (s *Service) TextPreview(f *store.File, max int64) (text string, truncated, ok bool) {
	fh, err := s.blobs.Open(f.Blob)
	if err != nil {
		return "", false, false
	}
	defer fh.Close()
	b, _ := io.ReadAll(io.LimitReader(fh, max))
	if !utf8.Valid(b) && int64(len(b)) < max {
		return "", false, false
	}
	return strings.ToValidUTF8(string(b), "�"), f.Size > max, true
}

// DirEntry is one row of a directory listing.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// List returns the immediate children of dir in the current version.
func (s *Service) ListDir(ctx context.Context, a *store.Artifact, dir string) []DirEntry {
	files, _ := s.db.DirFiles(ctx, a.ID, a.CurrentVersion, dir)
	seen := map[string]bool{}
	var out []DirEntry
	for _, f := range files {
		rest := strings.TrimPrefix(f.Path, dir)
		if name, _, isDir := strings.Cut(rest, "/"); isDir {
			if !seen[name] {
				seen[name] = true
				out = append(out, DirEntry{Name: name, IsDir: true})
			}
			continue
		}
		out = append(out, DirEntry{Name: rest, Size: f.Size})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out
}
