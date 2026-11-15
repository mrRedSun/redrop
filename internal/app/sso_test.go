package app

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/pkg/api"
)

// mockIdP is a minimal OIDC provider: discovery, JWKS, RS256 ID tokens, PKCE
// S256 and nonce. Whoever is "signed in" at the IdP is chosen with setUser;
// /authorize issues a code for that user without a login form.
type mockIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu      sync.Mutex
	current string
	codes   map[string]idpGrant
}

type idpGrant struct{ sub, nonce, challenge string }

const mockClientID = "redrop-test"

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockIdP{key: key, codes: map[string]idpGrant{}}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		iss := m.srv.URL
		writeJSON(w, map[string]any{"issuer": iss, "authorization_endpoint": iss + "/authorize", "token_endpoint": iss + "/token",
			"jwks_uri": iss + "/jwks", "response_types_supported": []string{"code"},
			"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64url(key.N.Bytes()), "e": b64url(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != mockClientID || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		code := make([]byte, 16)
		rand.Read(code)
		m.mu.Lock()
		m.codes[b64url(code)] = idpGrant{sub: m.current, nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
		m.mu.Unlock()
		u, _ := url.Parse(q.Get("redirect_uri"))
		v := u.Query()
		v.Set("code", b64url(code))
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		m.mu.Lock()
		g, ok := m.codes[r.PostForm.Get("code")]
		delete(m.codes, r.PostForm.Get("code"))
		m.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || b64url(sum[:]) != g.challenge {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant"})
			return
		}
		now := time.Now().Unix()
		writeJSON(w, map[string]any{"access_token": "at-" + g.sub, "token_type": "Bearer", "expires_in": 300,
			"id_token": m.sign(map[string]any{"iss": m.srv.URL, "sub": g.sub, "aud": mockClientID, "iat": now, "exp": now + 300,
				"nonce": g.nonce, "email": g.sub + "@idp.test", "preferred_username": g.sub, "name": g.sub})})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockIdP) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, _ := json.Marshal(claims)
	in := b64url(h) + "." + b64url(p)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, m.key, crypto.SHA256, sum[:])
	return in + "." + b64url(sig)
}

// setUser chooses who is signed in at the IdP.
func (m *mockIdP) setUser(sub string) {
	m.mu.Lock()
	m.current = sub
	m.mu.Unlock()
}

// newSSOHarness is a server with the mock IdP as provider "mock" and
// REDROP_ADMIN_REQUIRE_OIDC=true, as in the review's setup.
func newSSOHarness(t *testing.T) (*harness, *mockIdP) {
	t.Helper()
	idp := newMockIdP(t)
	t.Setenv("REDROP_OIDC_PROVIDERS", "mock")
	t.Setenv("REDROP_OIDC_MOCK_ISSUER", idp.srv.URL)
	t.Setenv("REDROP_OIDC_MOCK_CLIENT_ID", mockClientID)
	t.Setenv("REDROP_OIDC_MOCK_CLIENT_SECRET", "test-client-secret")
	t.Setenv("REDROP_ADMIN_REQUIRE_OIDC", "true")
	return newHarness(t), idp
}

// browser is one cookie jar talking to the app; it never follows redirects
// on its own.
type browser struct {
	h       *harness
	cookies map[string]*http.Cookie
}

func (h *harness) browser(session *http.Cookie) *browser {
	b := &browser{h: h, cookies: map[string]*http.Cookie{}}
	if session != nil {
		b.cookies[session.Name] = session
	}
	return b
}

// get requests target: an app path, an absolute app URL (http://app.test/…),
// or an absolute URL elsewhere (the IdP). It keeps the app's cookies.
func (b *browser) get(target string) *http.Response {
	b.h.t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		b.h.t.Fatal(err)
	}
	app := u.Host == "" || u.Host == "app.test"
	if app {
		target = b.h.srv.URL + u.RequestURI()
	}
	req, _ := http.NewRequest("GET", target, nil)
	if app {
		for _, c := range b.cookies {
			req.AddCookie(c)
		}
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		b.h.t.Fatal(err)
	}
	b.h.t.Cleanup(func() { resp.Body.Close() })
	if app {
		for _, c := range resp.Cookies() {
			if c.MaxAge < 0 {
				delete(b.cookies, c.Name)
			} else {
				b.cookies[c.Name] = c
			}
		}
	}
	return resp
}

// sso runs a whole single sign-on round trip in mode and returns the final
// redirect from the app (the IdP step is skipped when the start refuses).
func (b *browser) sso(mode string) *http.Response {
	b.h.t.Helper()
	r := b.get("/api/v1/auth/oidc/mock/start?mode=" + mode)
	loc := r.Header.Get("Location")
	if r.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "http://127.0.0.1") {
		return r // refused before the IdP
	}
	r = b.get(loc)
	if r.StatusCode != http.StatusFound {
		b.h.t.Fatalf("IdP authorize: %d %s", r.StatusCode, body(r))
	}
	return b.get(r.Header.Get("Location"))
}

func (b *browser) me() api.Me {
	b.h.t.Helper()
	r := b.get("/api/v1/me")
	expectStatus(b.h.t, r, 200, "me")
	return decode[api.Me](b.h.t, r)
}

type testUser struct {
	id      string
	session *http.Cookie
}

// inviteUser creates a regular password account through an invite link and
// returns its session.
func (h *harness) inviteUser(name string) testUser {
	h.t.Helper()
	inv, err := h.app.Accounts.Administration().CreateInvite(h.t.Context(), "", accounts.InviteRequest{})
	if err != nil {
		h.t.Fatal(err)
	}
	tok := inv.URL[strings.LastIndex(inv.URL, "/")+1:]
	b, _ := json.Marshal(api.AcceptInviteRequest{Username: name, Password: name + "-password-123"})
	r := h.raw("POST", "/api/v1/invites/"+tok+"/accept", "application/json", strings.NewReader(string(b)))
	expectStatus(h.t, r, 200, "accept invite")
	u := testUser{id: decode[api.Me](h.t, r).User.ID}
	for _, c := range r.Cookies() {
		if c.Name == "redrop_session" {
			u.session = c
		}
	}
	if u.session == nil {
		h.t.Fatal("no session after accepting the invite")
	}
	return u
}

func inviteAdmin() accounts.InviteRequest { return accounts.InviteRequest{Role: api.RoleAdmin} }

func redirectError(t *testing.T, r *http.Response) string {
	t.Helper()
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want a 303 redirect: %s", r.StatusCode, body(r))
	}
	u, _ := url.Parse(r.Header.Get("Location"))
	return u.Path + " " + u.Query().Get("error")
}

// Security review A2, replayed step by step: someone with the admin's local
// password links an unrelated IdP account ("mallory") from the password
// session. The link must neither grant admin in that browser (step 4) nor
// leave a standing SSO backdoor (step 5). Steps report with Errorf so a
// regression shows every step that breaks.
func TestAdminSSOCannotBeBypassedByLinking(t *testing.T) {
	h, idp := newSSOHarness(t)
	h.setupAdmin()
	status := func(r *http.Response, want int, what string) {
		t.Helper()
		if r.StatusCode != want {
			t.Errorf("%s: status %d, want %d: %s", what, r.StatusCode, want, body(r))
		}
	}

	// 1. The attacker signs in with the admin's local password.
	attacker := h.browser(h.session)
	status(attacker.get("/api/v1/admin/users"), 403, "step 1: admin API with a password session")

	// 2. The IdP session belongs to an unrelated IdP account.
	idp.setUser("mallory")

	// 3. Link mode from the password session is refused before the IdP.
	if got := redirectError(t, attacker.sso("link")); !strings.HasPrefix(got, "/account ") || !strings.Contains(got, "first sign in with single sign-on") {
		t.Errorf("step 3: link from an admin password session redirected to %q, want the refusal", got)
	}

	// 4. Same browser: still a password session, still no admin.
	if me := attacker.me(); me.AuthMethod != "password" || me.CanAdmin {
		t.Errorf("step 4: auth_method=%s can_admin=%v, want password and false", me.AuthMethod, me.CanAdmin)
	}
	status(attacker.get("/api/v1/admin/users"), 403, "step 4: admin API after the link attempt")
	r := attacker.get("/api/v1/me/identities")
	if ids := decode[api.IdentityList](t, r); len(ids.Items) != 0 {
		t.Errorf("step 4: mallory was linked to the admin: %+v", ids.Items)
	}

	// 5. Later, a fresh browser signs in through SSO as mallory. The
	// callback refuses it (Forbidden: not linked), which the browser sees
	// as a redirect to the login page with that error and no session.
	fresh := h.browser(nil)
	if got := redirectError(t, fresh.sso("login")); !strings.HasPrefix(got, "/login ") || !strings.Contains(got, "not linked") {
		t.Errorf("step 5: SSO login as mallory redirected to %q, want the not-linked refusal", got)
	}
	if _, ok := fresh.cookies["redrop_session"]; ok {
		t.Error("step 5: SSO login as mallory set a session cookie")
	}
	status(fresh.get("/api/v1/admin/users"), 401, "step 5: admin API in mallory's browser")
}

// The callback re-checks the rule: a link started by a regular user whose
// account becomes an admin before the callback is refused too.
func TestAdminSSOLinkRecheckedAtCallback(t *testing.T) {
	h, idp := newSSOHarness(t)
	h.setupAdmin()
	bob := h.inviteUser("bob")
	b := h.browser(bob.session)
	start := b.get("/api/v1/auth/oidc/mock/start?mode=link")
	if start.StatusCode != http.StatusFound {
		t.Fatalf("link start for a regular user: %d %s", start.StatusCode, body(start))
	}
	// The admin promotes bob through the database while the round trip is
	// in flight (admin routes need SSO here).
	if _, err := h.app.db.DB.Exec(`UPDATE users SET role = 'admin' WHERE id = ?`, bob.id); err != nil {
		t.Fatal(err)
	}
	idp.setUser("mallory")
	r := b.get(start.Header.Get("Location"))
	if got := redirectError(t, b.get(r.Header.Get("Location"))); !strings.Contains(got, "first sign in with single sign-on") {
		t.Fatalf("callback after promotion: %q", got)
	}
	r = b.get("/api/v1/me/identities")
	if ids := decode[api.IdentityList](t, r); len(ids.Items) != 0 {
		t.Fatalf("identity linked after promotion: %+v", ids.Items)
	}
}

// Linking still works where it is safe, and never changes the session's
// sign-in method: a regular user's password session stays a password
// session, and the linked identity opens SSO sessions on its own.
func TestLinkKeepsSessionMethod(t *testing.T) {
	h, idp := newSSOHarness(t)
	h.setupAdmin()
	bob := h.inviteUser("bob")
	b := h.browser(bob.session)
	idp.setUser("bob-at-idp")
	r := b.sso("link")
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/account?linked=1" {
		t.Fatalf("link: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	for _, c := range r.Cookies() {
		if c.Name == "redrop_session" {
			t.Fatal("linking replaced the session cookie")
		}
	}
	if me := b.me(); me.AuthMethod != "password" || me.User.Username != "bob" {
		t.Fatalf("after linking: %+v", me)
	}
	fresh := h.browser(nil)
	if r := fresh.sso("login"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("SSO login with the linked identity: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if me := fresh.me(); me.AuthMethod != "oidc" || me.User.Username != "bob" || me.CanAdmin {
		t.Fatalf("SSO session: %+v", me)
	}
}

// An admin who signed in through SSO may link another identity.
func TestAdminWithSSOSessionMayLink(t *testing.T) {
	h, idp := newSSOHarness(t)
	h.setupAdmin()
	// An admin invite accepted through SSO gives an admin with an SSO session.
	inv, err := h.app.Accounts.Administration().CreateInvite(t.Context(), "", inviteAdmin())
	if err != nil {
		t.Fatal(err)
	}
	idp.setUser("carol-at-idp")
	b := h.browser(nil)
	r := b.get("/api/v1/auth/oidc/mock/start?mode=invite&token=" + inv.URL[strings.LastIndex(inv.URL, "/")+1:])
	r = b.get(r.Header.Get("Location"))
	r = b.get(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("invite through SSO: %d %s", r.StatusCode, body(r))
	}
	if me := b.me(); me.AuthMethod != "oidc" || !me.CanAdmin {
		t.Fatalf("SSO admin: %+v", me)
	}
	idp.setUser("carol-second")
	if r := b.sso("link"); r.Header.Get("Location") != "/account?linked=1" {
		t.Fatalf("link from an admin SSO session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if me := b.me(); me.AuthMethod != "oidc" || !me.CanAdmin {
		t.Fatalf("after linking: %+v", me)
	}
}
