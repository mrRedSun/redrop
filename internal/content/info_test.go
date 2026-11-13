package content

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
)

// splice returns the whole page as served with snippet s inserted.
func splice(t *testing.T, page string, s []byte) string {
	t.Helper()
	p, err := newSplicedPage(strings.NewReader(page), int64(len(page)), s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInsertionPoint(t *testing.T) {
	s := []byte("<S>")
	for in, want := range map[string]string{
		"<html><body><p>x</p></body></html>": "<html><body><p>x</p><S></body></html>",
		"<BODY>x</BODY >\n":                  "<BODY>x<S></BODY >\n",
		"<p>no body element":                 "<p>no body element<S>",
		"":                                   "<S>",
		// Text after the closing tag is fine; only real tags count.
		"<body>a</body>\n<!-- c -->\n</html>": "<body>a</body>\n<!-- c -->\n</html><S>",
		"<body>a</body></html>\n":             "<body>a<S></body></html>\n",
		"<body>a</bodyx>":                     "<body>a</bodyx><S>",
		// Review reproductions: a </body> inside a script or comment is not
		// the insertion point; the snippet goes at the very end instead.
		`<html><body><p>hi</p><script>var s="</body>"</script>`:         `<html><body><p>hi</p><script>var s="</body>"</script><S>`,
		`<body>x</body></html><script>var t='</body>'</script>`:         `<body>x</body></html><script>var t='</body>'</script><S>`,
		"<body>x</body><!-- </body> -->":                                "<body>x</body><!-- </body> --><S>",
		"<body>x<script>document.write('</body>')":                      "<body>x<script>document.write('</body>')<S>",
		"<body><script>a()</script><!-- note --><p>y</p></body></html>": "<body><script>a()</script><!-- note --><p>y</p><S></body></html>",
	} {
		if got := splice(t, in, s); got != want {
			t.Errorf("splice(%q) = %q, want %q", in, got, want)
		}
	}
}

// countingReaderAt counts how many bytes of the stored page are read.
type countingReaderAt struct {
	r *bytes.Reader
	n atomic.Int64
}

func (c *countingReaderAt) ReadAt(b []byte, off int64) (int, error) {
	k, err := c.r.ReadAt(b, off)
	c.n.Add(int64(k))
	return k, err
}

// TestSplicedPageStreams: placing the snippet reads only the page's tail,
// a Range request reads only its range, and a full read allocates far less
// than the page (the page is never held in memory).
func TestSplicedPageStreams(t *testing.T) {
	const size = 8 << 20
	page := bytes.Repeat([]byte("<p>lorem ipsum</p>\n"), size/10)[:size-len("</body></html>")]
	page = append(page, "</body></html>"...)
	snippet := infoSnippet(sharing.ViewerInfo{Title: "big"})
	want := string(page[:size-14]) + string(snippet) + "</body></html>"

	src := &countingReaderAt{r: bytes.NewReader(page)}
	p, err := newSplicedPage(src, size, snippet)
	if err != nil {
		t.Fatal(err)
	}
	if got := src.n.Load(); got > infoTailWindow {
		t.Fatalf("placing the snippet read %d bytes, want at most %d", got, infoTailWindow)
	}

	// Range through http.ServeContent reads just the range.
	src.n.Store(0)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Range", "bytes=1000-1099")
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/html") // as fileHeaders does; else ServeContent sniffs
	http.ServeContent(rec, req, "", time.Time{}, p)
	if rec.Code != 206 || rec.Body.String() != want[1000:1100] || src.n.Load() > 100 {
		t.Fatalf("range: %d, %q, read %d bytes", rec.Code, rec.Body.String(), src.n.Load())
	}
	if cl := rec.Header().Get("Content-Range"); cl != fmt.Sprintf("bytes 1000-1099/%d", len(want)) {
		t.Fatalf("Content-Range %q", cl)
	}

	// A full streamed read allocates O(snippet), not O(page).
	p, _ = newSplicedPage(src, size, snippet)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	h := sha256.New()
	if _, err := io.CopyBuffer(h, p, make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Fatalf("streaming an 8 MiB page allocated %d bytes", alloc)
	}
	if sum := sha256.Sum256([]byte(want)); !bytes.Equal(h.Sum(nil), sum[:]) {
		t.Fatal("streamed page differs from the expected splice")
	}
}

// TestSplicedPageRanges: every slice of the representation, around and
// across the snippet, matches the spliced bytes.
func TestSplicedPageRanges(t *testing.T) {
	page := "<html><body>" + strings.Repeat("x", 300) + "</body></html>"
	snippet := []byte("<script>s()</script>")
	want := splice(t, page, snippet)
	p, _ := newSplicedPage(strings.NewReader(page), int64(len(page)), snippet)
	for start := 0; start < len(want); start += 7 {
		for _, n := range []int{1, 5, 19, 40, len(want)} {
			if _, err := p.Seek(int64(start), io.SeekStart); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, n)
			k, _ := io.ReadFull(p, buf)
			if got, exp := string(buf[:k]), want[start:min(start+n, len(want))]; got != exp {
				t.Fatalf("read %d at %d = %q, want %q", n, start, got, exp)
			}
		}
	}
	if end, _ := p.Seek(0, io.SeekEnd); end != int64(len(want)) {
		t.Fatalf("length %d, want %d", end, len(want))
	}
	// A blob shorter than recorded is an error, not a silently short page.
	short, _ := newSplicedPage(strings.NewReader(page[:100]), int64(len(page)), snippet)
	if _, err := io.ReadAll(short); err == nil {
		t.Fatal("truncated blob read without error")
	}
}

func TestTopLevelNavigation(t *testing.T) {
	for _, tc := range []struct {
		hdr  map[string]string
		want bool
	}{
		{map[string]string{"Sec-Fetch-Dest": "document"}, true},
		{map[string]string{"Sec-Fetch-Dest": "document", "Accept": "*/*"}, true},
		{map[string]string{"Sec-Fetch-Dest": "iframe", "Accept": "text/html"}, false},
		{map[string]string{"Sec-Fetch-Dest": "empty", "Accept": "text/html"}, false},
		{map[string]string{"Accept": "text/html,application/xhtml+xml,*/*;q=0.8"}, true},
		{map[string]string{"Accept": "*/*"}, false},
		{map[string]string{}, false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		if got := topLevelNavigation(r); got != tc.want {
			t.Errorf("%v: %v, want %v", tc.hdr, got, tc.want)
		}
	}
}

func snippetData(t *testing.T, snippet []byte) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`\)\((\{.*\})\)</script>$`).FindSubmatch(snippet)
	if m == nil {
		t.Fatalf("no data in snippet tail: %q", snippet[max(0, len(snippet)-200):])
	}
	var d map[string]any
	if err := json.Unmarshal(m[1], &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestInfoSnippetEscaping: artifact titles are attacker-controlled text. The
// snippet must stay one script element whatever the title contains.
func TestInfoSnippetEscaping(t *testing.T) {
	evil := "</script><script>alert(1)</script><!--   & </SCRIPT"
	snippet := infoSnippet(sharing.ViewerInfo{Title: evil, Version: 1})
	lower := bytes.ToLower(snippet)
	if n := bytes.Count(lower, []byte("<script")); n != 1 {
		t.Errorf("snippet has %d <script openings, want 1", n)
	}
	if n := bytes.Count(lower, []byte("</script")); n != 1 || !bytes.HasSuffix(snippet, []byte("</script>")) {
		t.Errorf("snippet must close its script exactly once, at the end: %d", n)
	}
	if bytes.Contains(snippet, []byte("<!--")) || bytes.ContainsRune(snippet, ' ') {
		t.Error("snippet contains raw <!-- or U+2028")
	}
	if got := snippetData(t, snippet)["title"]; got != evil {
		t.Errorf("title round-trip = %q", got)
	}
	if strings.Contains(strings.ToLower(infoWidget), "</script") || strings.Contains(infoWidget, "<!--") {
		t.Error("widget source must not contain </script or <!--")
	}
}

// TestInfoSnippetFields pins the JSON the page receives to the privacy rule
// in DESIGN.md ("Info button").
func TestInfoSnippetFields(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	keys := func(info sharing.ViewerInfo) []string {
		var out []string
		for k := range snippetData(t, infoSnippet(info)) {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	base := []string{"expires", "files", "mark", "password", "published", "size", "title", "version"}
	if got := keys(sharing.ViewerInfo{Title: "x"}); !reflect.DeepEqual(got, base) {
		t.Errorf("unlimited link keys = %v, want %v", got, base)
	}
	limited := append([]string{"max_views", "views_left"}, base...)
	sort.Strings(limited)
	if got := keys(sharing.ViewerInfo{ExpiresAt: &exp, MaxViews: 3, ViewsLeft: 0}); !reflect.DeepEqual(got, limited) {
		t.Errorf("limited link keys = %v, want %v", got, limited)
	}
	if d := snippetData(t, infoSnippet(sharing.ViewerInfo{MaxViews: 3, ViewsLeft: 0})); d["views_left"] != float64(0) {
		t.Errorf("views_left = %v, want 0", d["views_left"])
	}
	if mark, _ := snippetData(t, infoSnippet(sharing.ViewerInfo{}))["mark"].(string); !strings.HasPrefix(mark, "data:image/svg+xml;base64,") {
		t.Errorf("mark must be inline, got %.40q", mark)
	}
}
