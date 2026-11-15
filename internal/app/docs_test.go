package app

import (
	"strings"
	"testing"
)

// Security review A9: the docs page runs a third-party script without a
// pinned version or the dashboard's CSP, so it is off unless
// REDROP_API_DOCS=true. The OpenAPI document is always served.
func TestAPIDocsPageIsOptIn(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/api/docs", "/api/docs/"} {
		r := h.raw("GET", path, "", nil)
		expectStatus(t, r, 404, "docs page by default "+path)
		if b := body(r); strings.Contains(b, "<script") || !strings.Contains(r.Header.Get("Content-Type"), "json") {
			t.Fatalf("%s: %s %s", path, r.Header.Get("Content-Type"), b)
		}
		if r.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s served without the dashboard CSP", path)
		}
	}
	r := h.raw("GET", "/api/openapi.json", "", nil)
	expectStatus(t, r, 200, "OpenAPI document")
	if !strings.Contains(body(r), `"openapi"`) {
		t.Fatal("OpenAPI document is not JSON")
	}

	t.Setenv("REDROP_API_DOCS", "true")
	h = newHarness(t)
	r = h.raw("GET", "/api/docs/", "", nil)
	expectStatus(t, r, 200, "docs page when enabled")
	if !strings.Contains(body(r), "<script") {
		t.Fatal("enabled docs page has no UI")
	}
}
