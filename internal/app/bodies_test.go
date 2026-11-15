package app

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// sessionRaw sends a body with the harness session and CSRF header.
func (h *harness) sessionRaw(method, path, contentType string, body io.Reader) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Redrop-Request", "1")
	req.AddCookie(h.session)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// chunked hides a body's length, so it is sent without Content-Length.
func chunked(s string) io.Reader { return io.MultiReader(strings.NewReader(s)) }

// Security review A10: a JSON route answers a body of another type with
// 415 before decoding it (Fuego used to panic on text/plain), and an
// oversized one with 413 (it used to be 500).
func TestJSONRoutesCheckBodies(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	// The review's case: anonymous text/plain sign-in.
	expectStatus(t, h.raw("POST", "/api/v1/auth/login", "text/plain", strings.NewReader(`{"username":"admin","password":"x"}`)), 415, "text/plain sign-in")

	// Every operation that takes a JSON body, read from the OpenAPI
	// document, so a new route cannot miss the check.
	spec, err := h.app.API.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody *struct {
				Content map[string]any `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	checked := 0
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if op.RequestBody == nil {
				continue
			}
			if op.RequestBody.Content["application/json"] == nil {
				if path != "/api/v1/uploads/{id}" { // raw upload chunks
					t.Errorf("%s %s takes a body that is not declared application/json: %v", method, path, op.RequestBody.Content)
				}
				continue
			}
			url := param.ReplaceAllString(path, "x")
			method = strings.ToUpper(method)
			for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "application/xml", ""} {
				r := h.sessionRaw(method, url, ct, strings.NewReader(`{}`))
				if r.StatusCode != 415 {
					t.Errorf("%s %s with Content-Type %q: %d, want 415: %s", method, path, ct, r.StatusCode, body(r))
				}
			}
			checked++
		}
	}
	if checked < 15 {
		t.Fatalf("only %d JSON-body operations found in the OpenAPI document", checked)
	}

	// application/json with parameters is JSON.
	expectStatus(t, h.sessionRaw("POST", "/api/v1/tokens", "application/json; charset=utf-8", strings.NewReader(`{"name":"ok"}`)), 201, "JSON with charset")
	// Malformed JSON is the client's error, not the server's.
	expectStatus(t, h.sessionRaw("POST", "/api/v1/tokens", "application/json", strings.NewReader(`{`)), 400, "malformed JSON")
	expectStatus(t, h.sessionRaw("POST", "/api/v1/tokens", "application/json", strings.NewReader(`{"nope":1}`)), 400, "unknown field")

	// Oversized bodies, with and without a declared length.
	big := `{"name":"` + strings.Repeat("a", 1<<20) + `"}`
	expectStatus(t, h.sessionRaw("POST", "/api/v1/tokens", "application/json", strings.NewReader(big)), 413, "1 MiB+ body with a length")
	expectStatus(t, h.sessionRaw("POST", "/api/v1/tokens", "application/json", chunked(big)), 413, "1 MiB+ body without a length")
	login := `{"username":"admin","password":"` + strings.Repeat("p", 4<<10) + `"}`
	expectStatus(t, h.raw("POST", "/api/v1/auth/login", "application/json", chunked(login)), 413, "4 KiB+ sign-in without a length")

	// Routes without a body still take requests without one.
	expectStatus(t, h.raw("POST", "/api/v1/auth/logout", "", nil), 200, "logout without a body")
}
