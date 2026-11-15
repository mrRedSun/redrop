package content

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/brand"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/netutil"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

const routeTestSecret = "reserved-route-test-signing-secret"
const routeTestContent = "<!doctype html><p>owner preview bytes</p>"

// Exercise the real handler and services without opening a listening socket.
func reservedRouteFixture(t *testing.T) (*Handler, *store.Store, map[string]*store.Artifact) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "redrop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.DB.Exec(`INSERT INTO users (id, username, quota_bytes, created_at) VALUES ('owner', 'owner', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash, size, err := blobs.Put(strings.NewReader(routeTestContent), int64(len(routeTestContent)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir: dir, PublicURL: &url.URL{Scheme: "https", Host: "redrop.example.com"},
		ContentURL:       &url.URL{Scheme: "https", Host: "usercontent.example"},
		ContentMissBurst: 10_000, ContentMissPerMinute: 1,
		ViewWindowLimited: time.Hour, ViewWindowPassword: 12 * time.Hour,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sh, err := sharing.New(db, blobs, cfg, []byte(routeTestSecret), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	artifacts := map[string]*store.Artifact{}
	for i, state := range []string{"active", "expired", "revoked", "exhausted"} {
		id := state
		// Fixed legacy-format slugs keep the fixture and unknown host disjoint.
		slug := strings.Repeat(string(rune('a'+i)), 26)
		if _, err := db.DB.Exec(`INSERT INTO artifacts (id, slug, user_id, current_version, created_at, updated_at, last_version) VALUES (?, ?, 'owner', 1, 1, 1, 1)`, id, slug); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.Exec(`INSERT INTO versions (artifact_id, number, kind, size, files, entry, created_at) VALUES (?, 1, 'html', ?, 1, 'index.html', 1)`, id, size); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.Exec(`INSERT INTO files (artifact_id, version, path, blob, size, content_type) VALUES (?, 1, 'index.html', ?, ?, 'text/html')`, id, hash, size); err != nil {
			t.Fatal(err)
		}
	}
	// Absolute fixture dates avoid waits and expiry-boundary assertions.
	if _, err := db.DB.Exec(`UPDATE artifacts SET expires_at = 946684800 WHERE id = 'expired'; UPDATE artifacts SET revoked = 1 WHERE id = 'revoked'; UPDATE artifacts SET max_views = 1, views = 1 WHERE id = 'exhausted'`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"active", "expired", "revoked", "exhausted"} {
		a, err := db.ArtifactByID(context.Background(), state)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[state] = a
	}
	return New(sh, cfg, &netutil.ClientResolver{}, log, http.NotFoundHandler()), db, artifacts
}

func reservedRequest(h *Handler, a *store.Artifact, method, target, body, site string) *httptest.ResponseRecorder {
	host := "zzzzzzzzzzzzzzzzzzzzzzzzzz.usercontent.example"
	if a != nil {
		host = a.Slug + ".usercontent.example"
	}
	r := httptest.NewRequest(method, "https://"+host+target, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:12345"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", site)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sameReservedResponse(t *testing.T, got, want *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != want.Code || !reflect.DeepEqual(got.Header(), want.Header()) || got.Body.String() != want.Body.String() {
		t.Errorf("response differs: status=%d want=%d; headers_equal=%t; body_equal=%t; Location=%q; body_bytes=%d want=%d",
			got.Code, want.Code, reflect.DeepEqual(got.Header(), want.Header()), got.Body.String() == want.Body.String(),
			got.Header().Get("Location"), got.Body.Len(), want.Body.Len())
	}
}

func TestReservedRoutesUnavailableUniform(t *testing.T) {
	h, _, artifacts := reservedRouteFixture(t)
	// Every dynamic route, malformed version form and unknown reserved path
	// must be the ordinary miss. Static assets intentionally remain available.
	paths := []string{
		reservedPrefix, reservedPrefix + "missing", reservedPrefix + "preview", reservedPrefix + "preview?t=invalid",
		reservedPrefix + "open", reservedPrefix + "v", reservedPrefix + "v/", reservedPrefix + "v/1",
		reservedPrefix + "v/1/", reservedPrefix + "v/1/index.html", reservedPrefix + "v/0/",
		reservedPrefix + "v/01/", reservedPrefix + "v/nope/",
	}
	invariant := map[string]bool{}
	for p := range viewerAssets {
		paths = append(paths, p)
		invariant[p] = true
	}
	for _, p := range brand.Paths {
		p = reservedPrefix + strings.TrimPrefix(p, "/")
		paths = append(paths, p)
		invariant[p] = true
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+p, func(t *testing.T) {
				want := reservedRequest(h, nil, method, p, "", "")
				if invariant[p] {
					if want.Code != http.StatusOK {
						t.Fatalf("slug-independent asset: status=%d, want 200", want.Code)
					}
				} else {
					sameReservedResponse(t, want, reservedRequest(h, nil, method, "/", "", ""))
					if want.Code != http.StatusNotFound {
						t.Fatalf("unknown reserved route: status=%d, want 404", want.Code)
					}
				}
				for _, state := range []string{"expired", "revoked", "exhausted"} {
					t.Run(state, func(t *testing.T) {
						sameReservedResponse(t, reservedRequest(h, artifacts[state], method, p, "", ""), want)
					})
				}
			})
		}
	}
	for _, tc := range []struct{ name, body, site string }{
		{"form", "next=%2F&password=invalid", "same-origin"},
		{"cross-site", "next=%2F", "cross-site"},
		{"malformed form", "password=%zz", "same-origin"},
		{"oversized form", "password=" + strings.Repeat("x", 4096), "same-origin"},
	} {
		t.Run("POST open/"+tc.name, func(t *testing.T) {
			want := reservedRequest(h, nil, http.MethodPost, reservedPrefix+"open", tc.body, tc.site)
			sameReservedResponse(t, want, reservedRequest(h, nil, http.MethodGet, "/", "", ""))
			for _, state := range []string{"expired", "revoked", "exhausted"} {
				t.Run(state, func(t *testing.T) {
					sameReservedResponse(t, reservedRequest(h, artifacts[state], http.MethodPost, reservedPrefix+"open", tc.body, tc.site), want)
				})
			}
		})
	}
}

func TestReservedRouteMissBudget(t *testing.T) {
	h, _, artifacts := reservedRouteFixture(t)
	for _, p := range []string{reservedPrefix, reservedPrefix + "missing", reservedPrefix + "preview?t=invalid", reservedPrefix + "open", reservedPrefix + "v/1/", reservedPrefix + "v/01/"} {
		for _, state := range []string{"unknown", "expired", "revoked", "exhausted"} {
			t.Run(p+"/"+state, func(t *testing.T) {
				// No refill in this test, so exhaustion is independent of time.
				h.misses = security.NewTokenBucket(1, 0, maxMissClients)
				first := reservedRequest(h, artifacts[state], http.MethodGet, p, "", "")
				if first.Code != http.StatusNotFound {
					t.Errorf("first miss: status=%d, want 404", first.Code)
				}
				second := reservedRequest(h, artifacts[state], http.MethodGet, p, "", "")
				if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") == "" {
					t.Errorf("miss did not spend budget: status=%d, want 429 with Retry-After", second.Code)
				}
			})
		}
	}
}

func TestInvalidPreviewIsUniformMiss(t *testing.T) {
	h, _, artifacts := reservedRouteFixture(t)
	signer := security.NewSigner([]byte(routeTestSecret), "preview")
	active := artifacts["active"]
	payload := fmt.Sprintf("%s|%d", active.ID, active.AccessGen)
	future := time.Unix(4102444800, 0) // 2100-01-01
	for name, token := range map[string]string{
		"missing": "", "invalid": "invalid",
		"expired":         signer.Sign(payload, time.Unix(946684800, 0)),
		"other artifact":  signer.Sign("other|1", future),
		"old generation":  signer.Sign("active|0", future),
		"cookie as token": signer.Sign(payload+"|preview", future),
	} {
		t.Run(name, func(t *testing.T) {
			want := reservedRequest(h, nil, http.MethodGet, "/", "", "")
			for _, state := range []string{"active", "expired", "revoked", "exhausted"} {
				t.Run(state, func(t *testing.T) {
					sameReservedResponse(t, reservedRequest(h, artifacts[state], http.MethodGet, reservedPrefix+"preview?t="+url.QueryEscape(token), "", ""), want)
				})
			}
		})
	}
}

func TestOwnerPreviewExpiredContent(t *testing.T) {
	h, db, artifacts := reservedRouteFixture(t)
	a := artifacts["expired"]
	link, err := h.sharing.Preview(context.Background(), sharing.Actor{UserID: "owner"}, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	w := reservedRequest(h, a, http.MethodGet, u.RequestURI(), "", "")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("owner preview redemption: status=%d Location=%q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != h.cookieName(cookiePreview) {
		t.Fatalf("owner preview did not set its grant cookie")
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+a.Slug+".usercontent.example/", nil)
	r.AddCookie(cookies[0])
	r.Header.Set("Sec-Fetch-Dest", "iframe")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != routeTestContent {
		t.Fatalf("expired owner content: status=%d exact_bytes=%t", w.Code, w.Body.String() == routeTestContent)
	}
	after, err := db.ArtifactByID(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Views != a.Views || after.Previews != a.Previews {
		t.Fatalf("owner preview changed counters: views=%d previews=%d", after.Views, after.Previews)
	}
}
