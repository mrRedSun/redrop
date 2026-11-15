package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// raw sends a request with an explicit body and content type to the app
// origin, with the CSRF header and no session.
func (h *harness) raw(method, path, contentType string, body io.Reader, header ...string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Redrop-Request", "1")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The sign-in and onboarding routes take only a few KiB, so an anonymous
// client cannot make the server hold megabyte-sized usernames (security
// review A1).
func TestAuthRoutesRefuseLargeBodies(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	huge := `{"username":"u` + strings.Repeat("a", 1_000_000) + `","password":"wrongpassword"}`
	justOver := `{"username":"admin","password":"` + strings.Repeat("p", 4<<10) + `"}`
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/setup", "/api/v1/invites/x/accept", "/api/v1/reset/x"} {
		expectStatus(t, h.raw("POST", path, "application/json", strings.NewReader(huge)), 413, "1 MB body to "+path)
		expectStatus(t, h.raw("POST", path, "application/json", strings.NewReader(justOver)), 413, "4 KiB+ body to "+path)
	}
	r := h.raw("POST", "/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"long-enough-password"}`))
	expectStatus(t, r, 200, "normal login")
}
