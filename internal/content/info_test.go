package content

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
)

func TestInsertBeforeBody(t *testing.T) {
	s := []byte("<S>")
	for in, want := range map[string]string{
		"<html><body><p>x</p></body></html>":    "<html><body><p>x</p><S></body></html>",
		"<BODY>x</BODY >\n":                     "<BODY>x<S></BODY >\n",
		"<p>no body element":                    "<p>no body element<S>",
		"":                                      "<S>",
		"<body>'</body>' in text</body></html>": "<body>'</body>' in text<S></body></html>",
	} {
		if got := string(insertBeforeBody([]byte(in), s)); got != want {
			t.Errorf("insertBeforeBody(%q) = %q, want %q", in, got, want)
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
