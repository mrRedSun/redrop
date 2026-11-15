package app

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/client"
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

// retryAfter checks a 429's Retry-After: whole seconds, within the window.
// Review 26 n-B: every limiter refusal says when to try again.
func retryAfter(t *testing.T, r *http.Response, maxSeconds int, what string) {
	t.Helper()
	n, err := strconv.Atoi(r.Header.Get("Retry-After"))
	if err != nil || n < 1 || n > maxSeconds+1 {
		t.Errorf("%s: Retry-After %q, want 1..%d seconds", what, r.Header.Get("Retry-After"), maxSeconds+1)
	}
}

// Every per-client limiter treats one IPv6 /64 as one client (security
// review A3): rotating through the addresses of a /64 does not reset them.
// The test client connects from 127.0.0.1, a trusted proxy by default, so
// X-Forwarded-For chooses the client address.
func TestPerClientLimitersGroupIPv6(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	addr := func(i int) string { return fmt.Sprintf("2001:db8:77::%x", i+1) }

	// Sign-in: 5 per network and username per 5 minutes. The sixth guess
	// meets the network's limit, before the account's own backoff.
	login := `{"username":"admin","password":"wrong-password"}`
	for i := range 5 {
		expectStatus(t, h.raw("POST", "/api/v1/auth/login", "application/json", strings.NewReader(login), "X-Forwarded-For", addr(i)), 401, "guess "+addr(i))
	}
	r := h.raw("POST", "/api/v1/auth/login", "application/json", strings.NewReader(login), "X-Forwarded-For", addr(5))
	expectStatus(t, r, 429, "sixth guess from the same /64")
	if b := body(r); !strings.Contains(b, "too many sign-in attempts; wait") {
		t.Errorf("sixth guess was not stopped by the network limit: %s", b)
	}
	retryAfter(t, r, 5*60, "sign-in limit")

	// One-time link lookups: 60 per network per 10 minutes.
	for i := range 60 {
		expectStatus(t, h.raw("GET", "/api/v1/invites/no-such-invite", "", nil, "X-Forwarded-For", addr(100+i)), 200, "invite lookup")
	}
	r = h.raw("GET", "/api/v1/invites/no-such-invite", "", nil, "X-Forwarded-For", addr(200))
	expectStatus(t, r, 429, "61st lookup from the same /64")
	retryAfter(t, r, 10*60, "link lookup limit")
	expectStatus(t, h.raw("GET", "/api/v1/invites/no-such-invite", "", nil, "X-Forwarded-For", "2001:db8:78::1"), 200, "lookup from another /64")

	// Artifact passwords: 5 per network and artifact per 5 minutes.
	a := h.share(c, "gated", client.PublishOptions{Password: "right-password"})
	unlock := func(ip string) *http.Response {
		u, _ := url.Parse(a.URL)
		req, _ := http.NewRequest("POST", h.srv.URL+"/__redrop/open", strings.NewReader(url.Values{"password": {"wrong"}, "next": {"/"}}.Encode()))
		req.Host = u.Host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	for i := range 5 {
		expectStatus(t, unlock(addr(300+i)), 401, "wrong artifact password")
	}
	r = unlock(addr(400))
	expectStatus(t, r, 429, "sixth artifact password from the same /64")
	retryAfter(t, r, 5*60, "artifact password limit")
	expectStatus(t, unlock("2001:db8:79::1"), 401, "artifact password from another /64")
}

// Security review A4 through the whole server: requests shaped like
// Cloudflare Worker subrequests ("<forged>, <Worker egress>, <edge>", from
// the reverse proxy) with a new forged value each time. With
// REDROP_TRUSTED_HOPS=2 they all count as the Worker egress address and
// share one bucket; the old walk over trusted CDN ranges gives each forged
// value its own.
func TestTrustedHopsDefeatForgedForwardedFor(t *testing.T) {
	worker := func(i int) string { return fmt.Sprintf("198.51.100.%d, 2a06:98c0:3600::103, 172.70.1.1", i%250+1) }
	lookups := func(h *harness) map[int]int {
		counts := map[int]int{}
		for i := range 61 {
			counts[h.raw("GET", "/api/v1/invites/no-such-invite", "", nil, "X-Forwarded-For", worker(i)).StatusCode]++
		}
		return counts
	}

	t.Setenv("REDROP_TRUSTED_PROXIES", "127.0.0.1/32,172.64.0.0/13,2a06:98c0::/29")
	if got := lookups(newHarness(t)); got[200] != 61 {
		t.Fatalf("walk over CDN ranges: %v, want every forged value let through", got)
	}

	t.Setenv("REDROP_TRUSTED_PROXIES", "127.0.0.1/32") // the reverse proxy only
	t.Setenv("REDROP_TRUSTED_HOPS", "2")
	if got := lookups(newHarness(t)); got[200] != 60 || got[429] != 1 {
		t.Fatalf("two trusted hops: %v, want 60 lookups for the one Worker address, then 429", got)
	}
}
