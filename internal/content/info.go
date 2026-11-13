package content

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/brand"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

// The info button (DESIGN.md "Info button") is a small script appended to
// artifact HTML pages as they are served. Stored bytes never change; the
// response is a separate representation with its own ETag and length.

//go:embed infowidget.js
var infoWidget string

// infoOffParam disables the button for one request, e.g. for screenshots.
const infoOffParam = "redrop-info"

// maxInfoPage bounds how much HTML is read into memory to add the button.
// Larger pages are streamed unchanged.
const maxInfoPage = 8 << 20

// infoData is the JSON the widget receives. Its fields are exactly
// sharing.ViewerInfo plus the redrop mark; info_test.go pins them.
type infoData struct {
	Title     string     `json:"title"`
	Version   int        `json:"version"`
	Published time.Time  `json:"published"`
	Expires   *time.Time `json:"expires"`
	MaxViews  int        `json:"max_views,omitempty"`
	ViewsLeft *int       `json:"views_left,omitempty"`
	Password  bool       `json:"password"`
	Size      int64      `json:"size"`
	Files     int        `json:"files"`
	Mark      string     `json:"mark"`
}

var infoMark = brand.SVGDataURL()

// infoSnippet renders the <script> element for info. encoding/json escapes
// <, >, &, U+2028 and U+2029, so no field can end the script element or
// break out of the JavaScript expression.
func infoSnippet(info sharing.ViewerInfo) []byte {
	d := infoData{
		Title: info.Title, Version: info.Version, Published: info.Published, Expires: info.ExpiresAt,
		Password: info.Password, Size: info.Size, Files: info.Files, Mark: infoMark,
	}
	if info.MaxViews > 0 {
		left := info.ViewsLeft
		d.MaxViews, d.ViewsLeft = info.MaxViews, &left
	}
	data, err := json.Marshal(d)
	if err != nil {
		panic(err) // plain types only
	}
	var b bytes.Buffer
	b.WriteString("<script>(")
	b.WriteString(strings.TrimSpace(infoWidget))
	b.WriteString(")(")
	b.Write(data)
	b.WriteString(")</script>")
	return b.Bytes()
}

// insertBeforeBody puts snippet just before the last </body>, or at the end
// when there is none. The rest of the document is not parsed or changed.
func insertBeforeBody(page, snippet []byte) []byte {
	at := lastIndexFold(page, "</body")
	if at < 0 {
		at = len(page)
	}
	out := make([]byte, 0, len(page)+len(snippet))
	out = append(out, page[:at]...)
	out = append(out, snippet...)
	return append(out, page[at:]...)
}

func lastIndexFold(b []byte, sub string) int {
	for i := len(b) - len(sub); i >= 0; i-- {
		if b[i] == sub[0] && bytes.EqualFold(b[i:i+len(sub)], []byte(sub)) {
			return i
		}
	}
	return -1
}

// topLevelNavigation reports whether the request loads a page into a tab
// rather than a frame, fetch, or subresource. Browsers without Fetch
// Metadata fall back to the Accept check; the widget re-checks window.top.
func topLevelNavigation(r *http.Request) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return isDocument(r)
}

func isHTML(contentType string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.Split(contentType, ";")[0]), "text/html")
}

// infoFor decides whether this response gets the button, and with what.
func (h *Handler) infoFor(w http.ResponseWriter, r *http.Request, a *store.Artifact, f *store.File, download bool) ([]byte, bool) {
	if download || !isHTML(f.ContentType) {
		return nil, false
	}
	info, on := h.sharing.ViewerInfo(a)
	if !on {
		return nil, false
	}
	// Same URL, different bytes depending on these request headers.
	w.Header().Add("Vary", "Sec-Fetch-Dest, Accept")
	switch {
	case !topLevelNavigation(r),
		r.Header.Get("Range") != "", // ranges always address the stored bytes
		r.URL.Query().Get(infoOffParam) == "0",
		f.Size > maxInfoPage:
		return nil, false
	}
	return infoSnippet(info), true
}

// sendPageWithInfo serves an HTML file with the info button added. It
// reports false, having written nothing, when the stored page can't be read
// within the size bound; the caller then serves the file unchanged.
func (h *Handler) sendPageWithInfo(w http.ResponseWriter, r *http.Request, f *store.File, snippet []byte) bool {
	fh, err := h.sharing.OpenBlob(f)
	if err != nil {
		return false
	}
	defer fh.Close()
	page, err := io.ReadAll(io.LimitReader(fh, maxInfoPage+1))
	if err != nil || len(page) > maxInfoPage {
		return false
	}
	sum := sha256.Sum256(snippet)
	h.fileHeaders(w, f, `"`+f.Blob+"-i"+hex.EncodeToString(sum[:8])+`"`, false)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(insertBeforeBody(page, snippet)))
	return true
}
