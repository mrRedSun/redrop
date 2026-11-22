package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/httpapi"
	"github.com/mrRedSun/redrop/pkg/api"
)

// GET /api/v1/version is public, needs no token, and reports the server's
// stamped build plus the minimum supported CLI version.
func TestVersionEndpoint(t *testing.T) {
	oldV, oldC, oldD := httpapi.Version, httpapi.Commit, httpapi.Date
	t.Cleanup(func() { httpapi.Version, httpapi.Commit, httpapi.Date = oldV, oldC, oldD })
	httpapi.Version, httpapi.Commit, httpapi.Date = "v1.2.3", "abc1234defgh", "2026-11-18T00:00:00Z"

	h := newHarness(t)
	r := h.do(http.MethodGet, "/api/v1/version", nil, false)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", r.StatusCode)
	}
	var v api.ServerVersion
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Version != "v1.2.3" || v.Commit != "abc1234defgh" || v.Date != "2026-11-18T00:00:00Z" {
		t.Errorf("decoded = %+v", v)
	}
	if v.MinCLIVersion == "" {
		t.Error("min_cli_version is empty")
	}
}

// A plain `go build` leaves the commit and date stamps empty; the endpoint
// must still answer with omittable fields rather than fail.
func TestVersionEndpointUnstamped(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodGet, "/api/v1/version", nil, false)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", r.StatusCode)
	}
	b := readAll(t, r)
	var v api.ServerVersion
	if err := json.Unmarshal([]byte(b), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version == "" {
		t.Error("version is empty")
	}
	if strings.Contains(string(b), `"commit":"`) && v.Commit == "" {
		t.Errorf("commit field present but empty: %s", b)
	}
}

// /healthz keeps its plain-text body: deployment health checks must not
// depend on the version endpoint or JSON parsing.
func TestHealthzUnchanged(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodGet, "/healthz", nil, false)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", r.StatusCode)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	b := strings.TrimSpace(readAll(t, r))
	if b != "ok" {
		t.Errorf("body = %q", b)
	}
}

func readAll(t *testing.T, r *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
