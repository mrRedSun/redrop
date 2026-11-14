package content

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/sharing"
)

// TestProductionCookies pins the attributes used on HTTPS deployments (the
// integration tests run on plain HTTP, where SameSite=None is impossible).
func TestProductionCookies(t *testing.T) {
	h := &Handler{cfg: &config.Config{Insecure: false}}
	g := &sharing.Grant{Value: "v", TTL: time.Hour}
	for _, tc := range []struct {
		name        string
		set         func(http.ResponseWriter)
		cookie      string
		sameSite    http.SameSite
		partitioned bool
	}{
		{"raw (in a frame)", func(w http.ResponseWriter) { h.setRawCookie(w, g) }, "__Host-rd_raw", http.SameSiteNoneMode, true},
		{"visit", func(w http.ResponseWriter) { h.setCookie(w, cookieVisit, g, false) }, "__Host-rd_visit", http.SameSiteLaxMode, false},
		{"direct choice", func(w http.ResponseWriter) { h.setCookie(w, cookieDirect, g, false) }, "__Host-rd_direct", http.SameSiteLaxMode, false},
	} {
		rec := httptest.NewRecorder()
		tc.set(rec)
		cs := rec.Result().Cookies()
		if len(cs) != 1 {
			t.Fatalf("%s: %d cookies", tc.name, len(cs))
		}
		c := cs[0]
		if c.Name != tc.cookie || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != tc.sameSite || c.Partitioned != tc.partitioned || c.MaxAge != 3600 {
			t.Errorf("%s: %+v", tc.name, c)
		}
	}
}
