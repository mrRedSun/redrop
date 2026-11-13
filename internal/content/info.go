package content

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// infoTailWindow is how much of the end of a page is read to find </body>.
// The page itself is streamed from the blob, never buffered.
const infoTailWindow = 64 << 10

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

// insertionPoint returns where the snippet goes in a page of size bytes
// whose last bytes are tail. It is the offset of a closing </body> tag only
// when no script or comment starts or ends after it, so it can't be text
// inside a script string or a comment. Otherwise it is the end of the page,
// where browsers still run a trailing script. Nothing else is parsed. tail is
// lowercased.
func insertionPoint(tail []byte, size int64) int64 {
	lower := bytes.ToLower(tail)
	at := bytes.LastIndex(lower, []byte("</body"))
	if at < 0 {
		return size
	}
	if rest := bytes.TrimLeft(lower[at+len("</body"):], " \t\n\r\f"); len(rest) == 0 || rest[0] != '>' {
		return size
	}
	last := func(m string) int { return bytes.LastIndex(lower, []byte(m)) }
	if max(last("<script"), last("</script"), last("<!--"), last("-->")) > at ||
		last("<script") > last("</script") || last("<!--") > last("-->") { // unclosed: </body> is inside it
		return size
	}
	return size - int64(len(tail)) + int64(at)
}

// splicedPage is the stored page with the snippet inserted at offset at, as
// an io.ReadSeeker over the blob. It reads the blob on demand, so serving it
// takes O(len(snippet)) memory regardless of the page size, and Range and
// HEAD requests address this representation consistently.
type splicedPage struct {
	src      io.ReaderAt
	size, at int64 // stored size and insertion offset
	snippet  []byte
	pos      int64
}

// newSplicedPage reads at most infoTailWindow bytes of src to place snippet.
func newSplicedPage(src io.ReaderAt, size int64, snippet []byte) (*splicedPage, error) {
	tail := make([]byte, min(size, infoTailWindow))
	if _, err := src.ReadAt(tail, size-int64(len(tail))); err != nil && err != io.EOF {
		return nil, err
	}
	return &splicedPage{src: src, size: size, at: insertionPoint(tail, size), snippet: snippet}, nil
}

func (p *splicedPage) length() int64 { return p.size + int64(len(p.snippet)) }

func (p *splicedPage) Read(b []byte) (int, error) {
	n := int64(len(p.snippet))
	var src, end int64 // blob offset to read from, and where this part ends
	switch {
	case p.pos >= p.length():
		return 0, io.EOF
	case p.pos >= p.at && p.pos < p.at+n:
		k := copy(b, p.snippet[p.pos-p.at:])
		p.pos += int64(k)
		return k, nil
	case p.pos < p.at:
		src, end = p.pos, p.at
	default:
		src, end = p.pos-n, p.length()
	}
	want := int(min(int64(len(b)), end-p.pos))
	k, err := p.src.ReadAt(b[:want], src)
	p.pos += int64(k)
	if k == want {
		return k, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF // the blob is shorter than its recorded size
	}
	return k, err
}

func (p *splicedPage) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += p.pos
	case io.SeekEnd:
		offset += p.length()
	}
	if offset < 0 {
		return 0, errors.New("seek before start")
	}
	p.pos = offset
	return offset, nil
}

// topLevelNavigation reports whether the request loads a page into a tab
// rather than a frame, fetch, or subresource. Without Fetch Metadata (older
// browsers, plain-HTTP origins, curl) only an explicit text/html Accept
// counts, so fetch() and */* downloads get the stored bytes. The widget
// re-checks window.top.
func topLevelNavigation(r *http.Request) bool {
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
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
	if !topLevelNavigation(r) || r.URL.Query().Get(infoOffParam) == "0" {
		return nil, false
	}
	return infoSnippet(info), true
}

// sendPageWithInfo serves an HTML file with the info button added, streamed
// from the blob. It reports false, having written nothing, when the blob
// can't be read; the caller then serves the file the usual way.
func (h *Handler) sendPageWithInfo(w http.ResponseWriter, r *http.Request, f *store.File, snippet []byte) bool {
	fh, err := h.sharing.OpenBlob(f)
	if err != nil {
		return false
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return false
	}
	page, err := newSplicedPage(fh, st.Size(), snippet)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(snippet)
	fileHeaders(w, f, `"`+f.Blob+"-i"+hex.EncodeToString(sum[:8])+`"`, false)
	http.ServeContent(w, r, "", time.Time{}, page)
	return true
}
