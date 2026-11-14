package content

import (
	"net/url"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/sharing"
)

// TestSafeNext keeps the post-gate redirect on the artifact's own host and
// out of the reserved gate path space.
func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                      "/",
		"/":                     "/",
		"/docs/page.html?x=1":   "/docs/page.html?x=1",
		"https://evil.example/": "/",
		"//evil.example/":       "/",
		"/\\evil.example":       "/",
		"javascript:alert(1)":   "/",
		"relative/path":         "/",
		"/__redrop/open":        "/",
		"/__redrop/preview?t=x": "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSelfRefSinks: every URL the content handler emits for its own origin
// has exactly one leading slash, even when the request path does not (the
// handler refuses such paths first; these sinks must not rely on it).
func TestSelfRefSinks(t *testing.T) {
	oneSlash := func(ref string) bool {
		return strings.HasPrefix(ref, "/") && (len(ref) == 1 || (ref[1] != '/' && ref[1] != '\\'))
	}
	for in, want := range map[string]string{
		"":                       "/",
		"/":                      "/",
		"/a/b?x=1":               "/a/b?x=1",
		"//evil.example/x":       "/evil.example/x",
		"/\\evil.example":        "/evil.example",
		"\\/\\/evil.example/x?y": "/evil.example/x?y",
		"?q=1":                   "/?q=1",
	} {
		if got := selfRef(in); got != want {
			t.Errorf("selfRef(%q) = %q, want %q", in, got, want)
		}
	}
	for _, p := range []string{"//evil.example/x", "/\\evil.example", "///evil.example", "/ok"} {
		u := &url.URL{Path: p, RawQuery: "redrop-version=1&redrop-info=0&x=1"}
		if got := withoutParams(u, paramVersion, paramInfo); !oneSlash(got) {
			t.Errorf("withoutParams(%q) = %q", p, got)
		}
		info := sharing.ViewerInfo{Version: 2, CanOpen: true, Revisions: []sharing.Revision{{Number: 2, Current: true}, {Number: 1}}}
		for _, row := range panelFor(info, u).Revisions {
			if !oneSlash(row.Href) {
				t.Errorf("panel link for %q: %q", p, row.Href)
			}
		}
	}
	if !singleSlash("/a") || singleSlash("//a") || singleSlash("/\\a") || singleSlash("a") || singleSlash("") {
		t.Error("singleSlash")
	}
}
