package content

import "testing"

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
