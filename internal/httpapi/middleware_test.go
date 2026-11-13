package httpapi

import (
	"go/parser"
	"go/token"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/config"
)

type fakeCaller struct {
	token, admin, oidc bool
}

func (f fakeCaller) IsToken() bool { return f.token }
func (f fakeCaller) CanAdmin(requireSSO bool) bool {
	return f.admin && !f.token && (!requireSSO || f.oidc)
}

func TestAuthorize(t *testing.T) {
	var (
		token     = fakeCaller{token: true}
		adminTok  = fakeCaller{token: true, admin: true}
		user      = fakeCaller{}
		adminPW   = fakeCaller{admin: true}
		adminOIDC = fakeCaller{admin: true, oidc: true}
	)
	cases := []struct {
		name       string
		p          caller
		level      access
		requireSSO bool
		allowed    bool
	}{
		{"token any", token, anyAuth, false, true},
		{"token session-only", token, sessionOnly, false, false},
		{"admin token on admin route", adminTok, adminOnly, false, false},
		{"user session", user, sessionOnly, false, true},
		{"user on admin", user, adminOnly, false, false},
		{"admin password", adminPW, adminOnly, false, true},
		{"admin password, SSO required", adminPW, adminOnly, true, false},
		{"admin OIDC, SSO required", adminOIDC, adminOnly, true, true},
	}
	for _, c := range cases {
		err := authorize(c.p, c.level, c.requireSSO)
		if (err == nil) != c.allowed {
			t.Errorf("%s: err=%v, want allowed=%v", c.name, err, c.allowed)
		}
		if err != nil && !apperr.Is(err, apperr.KindForbidden) {
			t.Errorf("%s: want Forbidden, got %v", c.name, err)
		}
	}
}

func TestCheckCSRF(t *testing.T) {
	u, _ := url.Parse("https://app.example")
	a := &API{cfg: &config.Config{PublicURL: u}}
	cases := []struct {
		method, header, site, origin string
		ok                           bool
	}{
		{"GET", "", "cross-site", "https://evil.example", true}, // reads are not state changes
		{"POST", "", "", "", false},                             // header required
		{"POST", "0", "", "", false},
		{"POST", "1", "", "", true}, // non-browser client with the header
		{"POST", "1", "same-origin", "https://app.example", true},
		{"POST", "1", "none", "", true},
		{"POST", "1", "same-site", "", false},
		{"DELETE", "1", "cross-site", "", false},
		{"PATCH", "1", "same-origin", "https://evil.example", false},
	}
	for i, c := range cases {
		r := httptest.NewRequest(c.method, "/api/v1/tokens", nil)
		if c.header != "" {
			r.Header.Set(CSRFHeader, c.header)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if err := a.checkCSRF(r); (err == nil) != c.ok {
			t.Errorf("case %d %+v: err=%v", i, c, err)
		}
	}
}

func TestRedactPath(t *testing.T) {
	cases := map[string]string{
		"/api/v1/invites/secret123":        "/api/v1/invites/…",
		"/api/v1/invites/secret123/accept": "/api/v1/invites/…/accept",
		"/api/v1/reset/abc":                "/api/v1/reset/…",
		"/invite/abc":                      "/invite/…",
		"/reset/abc":                       "/reset/…",
		"/setup/abc":                       "/setup/…",
		"/api/v1/artifacts/x":              "/api/v1/artifacts/x",
	}
	for in, want := range cases {
		if got := redactPath(in); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTransportDoesNotImportStore keeps storage records out of the JSON
// transport: services return their own view types.
func TestTransportDoesNotImportStore(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); strings.HasSuffix(p, "/internal/store") {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
}

// TestAdminHandlersHoldOnlyAdminPorts pins what admin handlers can reach:
// the admin ports never return an artifact's secret link.
func TestAdminHandlersHoldOnlyAdminPorts(t *testing.T) {
	typ := reflect.TypeOf(adminAPI{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Type.String())
	}
	want := []string{"*accounts.Administration", "*sharing.Moderation"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("adminAPI fields = %v, want %v", got, want)
	}
}
