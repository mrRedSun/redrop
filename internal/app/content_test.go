package app

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// share publishes a small HTML page and returns it.
func (h *harness) share(c *client.Client, text string, opts client.PublishOptions) *api.Artifact {
	h.t.Helper()
	page := filepath.Join(h.t.TempDir(), "page.html")
	os.WriteFile(page, []byte("<!doctype html><title>T</title><p>"+text+"</p>"), 0o600)
	a, err := c.Share(context.Background(), &client.Source{Path: page}, opts, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

// get requests host on the content domain with extra headers.
func (h *harness) get(host string, hdr map[string]string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func hostOf(t *testing.T, link string) string {
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// fingerprint is everything a client can observe about a response except Date.
func fingerprint(r *http.Response) string {
	var keys []string
	for k := range r.Header {
		if k != "Date" {
			keys = append(keys, k+": "+strings.Join(r.Header.Values(k), ", "))
		}
	}
	sort.Strings(keys)
	return r.Status + "\n" + strings.Join(keys, "\n") + "\n\n" + body(r)
}

// TestLegacySlugStillResolves: links minted before word slugs keep working.
func TestLegacySlugStillResolves(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, "legacy content", client.PublishOptions{})
	const legacy = "s3eybdkc67r7zegmb7qbugpmvy"
	if _, err := h.app.db.DB.Exec(`UPDATE artifacts SET slug = ? WHERE id = ?`, legacy, a.ID); err != nil {
		t.Fatal(err)
	}
	r := h.get(legacy+".content.test", nil)
	expectStatus(t, r, 200, "legacy slug")
	if !strings.Contains(body(r), `src="http://`+legacy+`--raw.content.test/__redrop/enter?`) {
		t.Fatal("legacy slug did not frame its artifact from its raw host")
	}
	got, err := c.GetArtifact(context.Background(), a.ID)
	if err != nil || got.URL != "http://"+legacy+".content.test/" {
		t.Fatalf("legacy URL reported as %q (%v)", got.URL, err)
	}
	// Rotating a legacy link mints a word link.
	rot, err := c.RotateArtifact(context.Background(), a.ID)
	if err != nil || !wordLinkRE.MatchString(rot.URL) {
		t.Fatalf("rotate legacy: %q %v", rot.URL, err)
	}
	expectStatus(t, h.get(legacy+".content.test", nil), 404, "legacy slug after rotate")
}

// TestMissesLookIdentical: unknown, malformed, deleted, expired, exhausted
// and revoked links are indistinguishable to the visitor.
func TestMissesLookIdentical(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()

	deleted := h.share(c, "x", client.PublishOptions{})
	if err := c.DeleteArtifact(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}
	expired := h.share(c, "x", client.PublishOptions{TTL: "1h"})
	if _, err := h.app.db.DB.Exec(`UPDATE artifacts SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), expired.ID); err != nil {
		t.Fatal(err)
	}
	exhausted := h.share(c, "x", client.PublishOptions{MaxViews: 1})
	expectStatus(t, h.content("POST", exhausted.URL, "/__redrop/open", url.Values{"next": {"/"}}, ""), 303, "use the one view")
	revoked := h.share(c, "x", client.PublishOptions{})
	yes := true
	if _, err := c.UpdateArtifact(ctx, revoked.ID, &api.UpdateArtifactRequest{Revoked: &yes}); err != nil {
		t.Fatal(err)
	}
	live := h.share(c, "x", client.PublishOptions{})

	misses := map[string]string{
		"unknown word slug":   "amber-otter-quilt-river.content.test",
		"unknown legacy slug": "s3eybdkc67r7zegmb7qbugpmvy.content.test",
		"three words":         "amber-otter-quilt.content.test",
		"uppercase":           "AMBER-otter-quilt-river.content.test",
		"nested label":        "a." + hostOf(t, live.URL),
		"deleted":             hostOf(t, deleted.URL),
		"expired":             hostOf(t, expired.URL),
		"exhausted":           hostOf(t, exhausted.URL),
		"revoked":             hostOf(t, revoked.URL),
	}
	var want, wantName string
	for name, host := range misses {
		got := fingerprint(h.get(host, nil))
		if want == "" {
			want, wantName = got, name
			if !strings.HasPrefix(got, "404 ") {
				t.Fatalf("%s: miss is not a 404:\n%s", name, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s differs from %s:\n--- %s\n%s\n--- %s\n%s", name, wantName, name, got, wantName, want)
		}
	}
	// Opening an unavailable link through the gate form is a miss too.
	r := h.content("POST", exhausted.URL, "/__redrop/open", url.Values{"next": {"/"}}, "")
	if got := fingerprint(r); got != want {
		t.Errorf("open on exhausted link differs from a miss:\n%s", got)
	}

	// Timing class: every miss is at most one indexed lookup; none takes a
	// slow path (password hashing, blob reads) that a hit-like miss could.
	median := func(host string) time.Duration {
		var d []time.Duration
		for range 15 {
			start := time.Now()
			body(h.get(host, nil))
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		return d[len(d)/2]
	}
	base := median(misses["unknown word slug"])
	for name, host := range misses {
		if m := median(host); m > 5*base+5*time.Millisecond {
			t.Errorf("%s: median %v vs unknown %v", name, m, base)
		}
	}
}

// TestMissLimiter: misses are limited per client IP as resolved through the
// trusted proxy, hits are never charged, and spent clients get 429.
func TestMissLimiter(t *testing.T) {
	for _, tc := range []struct{ name, header string }{
		{"traefik", "X-Forwarded-For"},
		{"cloudflare", "CF-Connecting-IP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REDROP_CONTENT_MISS_BURST", "3")
			t.Setenv("REDROP_CONTENT_MISS_PER_MINUTE", "1")
			t.Setenv("REDROP_CLIENT_IP_HEADER", tc.header)
			h := newHarness(t) // the test client connects from 127.0.0.1, a trusted proxy by default
			c := client.New(h.srv.URL, h.setupAdmin())
			live := hostOf(t, h.share(c, "hit", client.PublishOptions{}).URL)
			from := func(ip string) map[string]string { return map[string]string{tc.header: ip} }
			const unknown = "amber-otter-quilt-river.content.test"

			// Hits are never charged, however many.
			for i := range 20 {
				expectStatus(t, h.get(live, from("192.0.2.10")), 200, "hit "+string(rune('a'+i)))
			}
			for i := range 3 {
				expectStatus(t, h.get(unknown, from("192.0.2.10")), 404, "miss within burst "+string(rune('1'+i)))
			}
			r := h.get(unknown, from("192.0.2.10"))
			expectStatus(t, r, 429, "miss over burst")
			if r.Header.Get("Retry-After") != "60" {
				t.Errorf("Retry-After %q", r.Header.Get("Retry-After"))
			}
			// A spent client cannot probe: every artifact-host request is 429
			// until it refills, so the limiter is not a hit/miss oracle.
			expectStatus(t, h.get(live, from("192.0.2.10")), 429, "spent client on a live link")

			// Other clients behind the same proxy are unaffected: the socket
			// address (127.0.0.1 for everyone here) is not the key.
			expectStatus(t, h.get(unknown, from("192.0.2.20")), 404, "other client")
			expectStatus(t, h.get(live, from("192.0.2.20")), 200, "other client hit")

			// One IPv6 /64 is one client.
			for i := range 3 {
				expectStatus(t, h.get(unknown, from("2001:db8:1:2::"+string(rune('a'+i)))), 404, "v6 miss")
			}
			expectStatus(t, h.get(unknown, from("2001:db8:1:2::ffff")), 429, "v6 same /64")
			expectStatus(t, h.get(unknown, from("2001:db8:1:3::1")), 404, "v6 other /64")
		})
	}

	t.Run("untrusted peer", func(t *testing.T) {
		t.Setenv("REDROP_CONTENT_MISS_BURST", "2")
		t.Setenv("REDROP_TRUSTED_PROXIES", "198.51.100.0/24") // not the test client
		h := newHarness(t)
		const unknown = "amber-otter-quilt-river.content.test"
		for _, ip := range []string{"192.0.2.10", "192.0.2.11"} {
			expectStatus(t, h.get(unknown, map[string]string{"X-Forwarded-For": ip}), 404, "miss "+ip)
		}
		// Spoofed headers from an untrusted peer are ignored: all three
		// requests were charged to the socket address.
		expectStatus(t, h.get(unknown, map[string]string{"X-Forwarded-For": "192.0.2.12"}), 429, "spoofed client")
	})
}
