package content

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/ids"
)

// Key intake on the app origin (DESIGN.md "End-to-end encrypted links",
// "Where the key goes in the browser"). An end-to-end encrypted link is
// https://<app>/e/<slug>#k=<key>: the key arrives on the app origin, where
// no artifact runs and no window an artifact holds can read a URL. The
// intake page imports it as a non-extractable CryptoKey and, after the
// visitor's click, hands that to a helper page it opens on the artifact's
// origin (/__redrop/e2e-intake), which stores it where the viewer finds it.
// The raw key never reaches the artifact's origin.

//go:embed static/e2e-intake.js static/e2e-helper.js
var intakeFS embed.FS

const (
	intakeScriptPath = "/e2e/intake.js"              // app origin: the decryptor, then the intake
	helperPath       = reservedPrefix + "e2e-intake" // content origin: the helper page
	helperScriptPath = helperPath + ".js"            // content origin: the decryptor, then the helper
)

var (
	intakeScript = joinedAsset("static/e2e-intake.js")
	helperScript = joinedAsset("static/e2e-helper.js")
)

// joinedAsset is the decryptor followed by the script at name, as one asset.
func joinedAsset(name string) viewerAsset {
	lib, err := e2eFS.ReadFile("static/e2e.js")
	if err != nil {
		panic(err)
	}
	b, err := intakeFS.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return newViewerAsset(append(append(lib, '\n'), b...), "text/javascript; charset=utf-8")
}

// Intake serves the app origin's intake page and script.
type Intake struct {
	cfg   *config.Config
	pages *pages
	csp   string
}

// NewIntake builds the intake transport for the app origin.
func NewIntake(cfg *config.Config) *Intake {
	p := newPages(cfg)
	csp := strings.Join([]string{
		"default-src 'none'",
		"script-src " + cfg.PublicOrigin() + intakeScriptPath,
		"style-src '" + styleHash(p) + "'",
		"base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'",
	}, "; ")
	return &Intake{cfg: cfg, pages: p, csp: csp}
}

// styleHash is the CSP source of the pages' one inline style element.
func styleHash(p *pages) string {
	sum := sha256.Sum256([]byte(p.css))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// intakeData is what the intake script needs: where the artifact is.
type intakeData struct{ Content string }

// Page answers GET /e/{slug}. It looks nothing up, so it says nothing about
// the slug, counts no view and opens no gate: the artifact's own origin
// does all of that after the hand-off. It is never cached, never framed,
// sends no referrer, and keeps a handle only on the popup it opens itself
// (same-origin-allow-popups cuts any window that opened this one).
func (i *Intake) Page(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !ids.ValidSlug(slug) {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
	i.pages.renderWithCSP(w, http.StatusOK, "intake.html", "redrop", "", intakeData{Content: i.cfg.ArtifactURL(slug)}, i.csp)
}

// Script answers GET /e2e/intake.js.
func (i *Intake) Script(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, intakeScript)
}

// serveAsset writes a, revalidated on every use like the viewer's assets.
func serveAsset(w http.ResponseWriter, r *http.Request, a viewerAsset) {
	hd := w.Header()
	hd.Set("Content-Type", a.contentType)
	hd.Set("Cache-Control", "public, no-cache")
	hd.Set("ETag", a.etag)
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(a.body)))
}

// helperData is what the helper script needs: the one origin it takes a key
// from.
type helperData struct{ App string }

// e2eIntakeHelper answers /__redrop/e2e-intake on an artifact's host, for
// a live end-to-end encrypted artifact only (ServeHTTP gives every other
// slug the identical miss). The app-origin intake remains syntax-only.
// This page holds no artifact data: it only takes a CryptoKey from the app
// origin
// and stores it for this origin's viewer.
func (h *Handler) e2eIntakeHelper(w http.ResponseWriter, r *http.Request, slug string) {
	self := strings.TrimSuffix(h.cfg.ArtifactURL(slug), "/")
	csp := strings.Join([]string{
		"default-src 'none'",
		"script-src " + self + helperScriptPath,
		"style-src '" + styleHash(h.pages) + "'",
		"base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'",
	}, "; ")
	w.Header().Set("Cache-Control", "no-store")
	h.pages.renderWithCSP(w, http.StatusOK, "e2e-intake.html", "redrop", "", helperData{App: h.cfg.PublicOrigin()}, csp)
}
