package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// Viewer feedback (DESIGN.md "Viewer feedback"): the endpoint on the
// artifact's own host, its access rules and limits, and the owner API.

const feedbackHTML = "<!doctype html><title>Report</title><p>The totals are wrong.</p>"

// sameOrigin is what a browser sends with the viewer's fetch on the
// artifact's own page (plain HTTP sends no Fetch Metadata, so tests add it
// explicitly where it matters).
func sameOrigin(artifactURL string) map[string]string {
	return map[string]string{
		"Origin":         strings.TrimSuffix(artifactURL, "/"),
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
		"Content-Type":   "application/json",
	}
}

// with returns hdr with changes applied; an empty value removes a header.
func with(hdr map[string]string, changes ...string) map[string]string {
	out := map[string]string{}
	for k, v := range hdr {
		out[k] = v
	}
	for i := 0; i+1 < len(changes); i += 2 {
		if changes[i+1] == "" {
			delete(out, changes[i])
		} else {
			out[changes[i]] = changes[i+1]
		}
	}
	return out
}

// send makes a request to path on an artifact host with a body (a string
// is sent as is, anything else as JSON).
func (h *harness) send(method, artifactURL, path string, payload any, hdr map[string]string, cookies ...*http.Cookie) (*http.Response, string) {
	h.t.Helper()
	u, _ := url.Parse(artifactURL)
	var rd io.Reader
	switch p := payload.(type) {
	case nil:
	case string:
		rd = strings.NewReader(p)
	default:
		b, _ := json.Marshal(p)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Host = u.Host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	b := body(resp)
	resp.Body.Close()
	return resp, b
}

// comment posts a comment from the artifact's own page.
func (h *harness) comment(artifactURL string, payload any, cookies ...*http.Cookie) (*http.Response, string) {
	h.t.Helper()
	return h.send("POST", artifactURL, "/__redrop/feedback", payload, sameOrigin(artifactURL), cookies...)
}

func okComment(version int) map[string]any {
	return map[string]any{"body": "Section 3 is wrong", "version": version, "page": "/"}
}

// sameResponse fails unless a and b are the same answer: status, headers
// (but the date) and body.
func sameResponse(t *testing.T, what string, a *http.Response, ab string, b *http.Response, bb string) {
	t.Helper()
	ha, hb := a.Header.Clone(), b.Header.Clone()
	ha.Del("Date")
	hb.Del("Date")
	if a.StatusCode != b.StatusCode || ab != bb || !reflect.DeepEqual(ha, hb) {
		t.Fatalf("%s differs from an unknown reserved path:\n got %d %v %q\nwant %d %v %q", what, a.StatusCode, ha, ab, b.StatusCode, hb, bb)
	}
}

func noCORS(t *testing.T, r *http.Response, what string) {
	t.Helper()
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Fatalf("%s: CORS header %s", what, k)
		}
	}
}

func (h *harness) comments(c *client.Client, id, status string) []api.Comment {
	h.t.Helper()
	list, err := c.ListFeedback(context.Background(), id, status, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

// expire moves an artifact's expiry into the past.
func (h *harness) expire(id string) {
	h.t.Helper()
	if _, err := h.app.db.DB.Exec(`UPDATE artifacts SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Hour).Unix(), id); err != nil {
		h.t.Fatal(err)
	}
}

func TestFeedbackClosedIsAnUnknownPath(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	off := h.share(c, feedbackHTML, client.PublishOptions{})
	hidden := false
	noPanel := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true, InfoButton: &hidden})
	revoked := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
	expired := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
	open := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
	yes := true
	if _, err := c.UpdateArtifact(context.Background(), revoked.ID, &api.UpdateArtifactRequest{Revoked: &yes}); err != nil {
		t.Fatal(err)
	}
	h.expire(expired.ID)
	unknown := strings.Replace(open.URL, open.URL[len("http://"):strings.Index(open.URL, ".")], "amber-otter-quilt-river", 1)

	for name, a := range map[string]string{"off": off.URL, "info button off": noPanel.URL, "revoked": revoked.URL, "expired": expired.URL, "unknown slug": unknown} {
		hdr := sameOrigin(a)
		want, wantBody := h.send("POST", off.URL, "/__redrop/nope", okComment(1), with(hdr, "Origin", strings.TrimSuffix(off.URL, "/")))
		if want.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("reference POST: %d", want.StatusCode)
		}
		got, gotBody := h.send("POST", a, "/__redrop/feedback", okComment(1), hdr)
		sameResponse(t, "POST feedback, "+name, got, gotBody, want, wantBody)
	}
	// GET is never routed to the endpoint, open or not.
	for _, a := range []string{off.URL, open.URL} {
		want, wantBody := h.send("GET", a, "/__redrop/nope", nil, nav)
		got, gotBody := h.send("GET", a, "/__redrop/feedback", nil, nav)
		sameResponse(t, "GET feedback", got, gotBody, want, wantBody)
	}
	// A preflight gets the generic answer, without CORS headers.
	r, _ := h.send("OPTIONS", open.URL, "/__redrop/feedback", nil, map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	expectStatus(t, r, http.StatusMethodNotAllowed, "preflight")
	noCORS(t, r, "preflight")

	for _, a := range []*api.Artifact{off, noPanel, revoked, expired} {
		if n := len(h.comments(c, a.ID, "all")); n != 0 {
			t.Fatalf("closed artifact stored %d comments", n)
		}
	}
	// Turning it off closes it at once.
	r, _ = h.comment(open.URL, okComment(1))
	expectStatus(t, r, http.StatusCreated, "open artifact")
	no := false
	if _, err := c.UpdateArtifact(context.Background(), open.ID, &api.UpdateArtifactRequest{Feedback: &no}); err != nil {
		t.Fatal(err)
	}
	r, _ = h.comment(open.URL, okComment(1))
	expectStatus(t, r, http.StatusMethodNotAllowed, "after turning feedback off")
}

// Posts to links that don't resolve spend miss budget like every miss;
// posts to live artifacts don't, whether feedback is open or not.
func TestFeedbackMissesSpendBudget(t *testing.T) {
	t.Setenv("REDROP_CONTENT_MISS_BURST", "2")
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	off := h.share(c, feedbackHTML, client.PublishOptions{})
	for range 5 {
		r, _ := h.comment(off.URL, okComment(1))
		expectStatus(t, r, http.StatusMethodNotAllowed, "closed live artifact")
	}
	unknown := strings.Replace(off.URL, off.URL[len("http://"):strings.Index(off.URL, ".")], "amber-otter-quilt-river", 1)
	for range 2 {
		r, _ := h.comment(unknown, okComment(1))
		expectStatus(t, r, http.StatusMethodNotAllowed, "unknown slug")
	}
	r, _ := h.send("GET", off.URL, "/", nil, nav)
	expectStatus(t, r, http.StatusTooManyRequests, "after spending the miss budget")
}

func TestFeedbackRejectsOtherOrigins(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
	other := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
	hdr := sameOrigin(a.URL)
	for name, tc := range map[string]struct {
		hdr  map[string]string
		want int
	}{
		"no Origin":                      {with(hdr, "Origin", ""), http.StatusForbidden},
		"another artifact (same-site)":   {with(hdr, "Origin", strings.TrimSuffix(other.URL, "/"), "Sec-Fetch-Site", "same-site"), http.StatusForbidden},
		"another artifact's Origin":      {with(hdr, "Origin", strings.TrimSuffix(other.URL, "/")), http.StatusForbidden},
		"another site":                   {with(hdr, "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"), http.StatusForbidden},
		"cross-site with this Origin":    {with(hdr, "Sec-Fetch-Site", "cross-site"), http.StatusForbidden},
		"the dashboard":                  {with(hdr, "Origin", "http://app.test", "Sec-Fetch-Site", "cross-site"), http.StatusForbidden},
		"null Origin":                    {with(hdr, "Origin", "null"), http.StatusForbidden},
		"form post":                      {with(hdr, "Content-Type", "application/x-www-form-urlencoded"), http.StatusUnsupportedMediaType},
		"text/plain":                     {with(hdr, "Content-Type", "text/plain"), http.StatusUnsupportedMediaType},
		"no content type":                {with(hdr, "Content-Type", ""), http.StatusUnsupportedMediaType},
		"JSON with charset":              {with(hdr, "Content-Type", "application/json; charset=utf-8"), http.StatusCreated},
		"no Fetch Metadata (plain HTTP)": {with(hdr, "Sec-Fetch-Site", "", "Sec-Fetch-Mode", "", "Sec-Fetch-Dest", ""), http.StatusCreated},
	} {
		r, b := h.send("POST", a.URL, "/__redrop/feedback", okComment(1), tc.hdr)
		expectStatus(t, r, tc.want, name)
		noCORS(t, r, name)
		if tc.want != http.StatusCreated && !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s: answer is not JSON: %q", name, b)
		}
	}
	if n := len(h.comments(c, a.ID, "all")); n != 2 {
		t.Fatalf("stored %d comments, want the 2 same-origin ones", n)
	}
}

func TestFeedbackAccess(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())

	// Ungated: anyone with the link; posting counts no view and sets no cookie.
	u := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
	r, b := h.comment(u.URL, okComment(1))
	expectStatus(t, r, http.StatusCreated, "ungated")
	if b != "{\"ok\":true}\n" || len(r.Cookies()) != 0 {
		t.Fatalf("answer %q, cookies %v", b, r.Cookies())
	}
	if v := h.views(c, u.ID); v != 0 {
		t.Fatalf("posting counted %d views", v)
	}

	// Password: only after the password was entered.
	p := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true, Password: "hunter22"})
	r, b = h.comment(p.URL, okComment(1))
	expectStatus(t, r, http.StatusForbidden, "password, locked")
	if !strings.Contains(b, `"access_required"`) {
		t.Fatalf("locked answer: %s", b)
	}
	r = h.content("POST", p.URL, "/__redrop/open", url.Values{"password": {"hunter22"}, "next": {"/"}}, "")
	expectStatus(t, r, http.StatusSeeOther, "unlock")
	visit := r.Cookies()
	r, _ = h.comment(p.URL, okComment(1), visit...)
	expectStatus(t, r, http.StatusCreated, "password, unlocked")

	// One-time link: the visitor who opened it may comment, repeatedly,
	// without using views; everyone else gets the miss.
	o := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true, MaxViews: 1})
	r = h.content("POST", o.URL, "/__redrop/open", url.Values{"next": {"/"}}, "")
	expectStatus(t, r, http.StatusSeeOther, "open once")
	visit = r.Cookies()
	for range 2 {
		r, _ = h.comment(o.URL, okComment(1), visit...)
		expectStatus(t, r, http.StatusCreated, "one-time, with the visit")
	}
	if v := h.views(c, o.ID); v != 1 {
		t.Fatalf("one-time link has %d views after comments, want 1", v)
	}
	r, _ = h.comment(o.URL, okComment(1))
	expectStatus(t, r, http.StatusMethodNotAllowed, "one-time, used up, no visit")

	// A visit grant stops working when access changes (revoked here).
	yes := true
	if _, err := c.UpdateArtifact(context.Background(), p.ID, &api.UpdateArtifactRequest{Revoked: &yes}); err != nil {
		t.Fatal(err)
	}
	r, _ = h.comment(p.URL, okComment(1), visit...)
	expectStatus(t, r, http.StatusMethodNotAllowed, "revoked, with an old visit")
}

func TestFeedbackValidation(t *testing.T) {
	t.Setenv("REDROP_FEEDBACK_BURST", "50")
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
	for name, tc := range map[string]struct {
		payload any
		want    int
	}{
		"empty body":     {map[string]any{"body": "  ", "version": 1, "page": "/"}, http.StatusBadRequest},
		"long body":      {map[string]any{"body": strings.Repeat("x", 4001), "version": 1, "page": "/"}, http.StatusBadRequest},
		"long name":      {map[string]any{"body": "x", "name": strings.Repeat("n", 81), "version": 1, "page": "/"}, http.StatusBadRequest},
		"future version": {map[string]any{"body": "x", "version": 2, "page": "/"}, http.StatusBadRequest},
		"reserved page":  {map[string]any{"body": "x", "version": 1, "page": "/__redrop/feedback"}, http.StatusBadRequest},
		"other site":     {map[string]any{"body": "x", "version": 1, "page": "//evil.example/"}, http.StatusBadRequest},
		"long quote":     {map[string]any{"body": "x", "version": 1, "page": "/", "quote": map[string]string{"exact": strings.Repeat("q", 501)}}, http.StatusBadRequest},
		"unknown field":  {map[string]any{"body": "x", "version": 1, "page": "/", "ip": "192.0.2.1"}, http.StatusBadRequest},
		"not JSON":       {"body=x", http.StatusBadRequest},
		"two objects":    {`{"body":"x","version":1,"page":"/"} {}`, http.StatusBadRequest},
		"array":          {`[]`, http.StatusBadRequest},
		"too large":      {map[string]any{"body": strings.Repeat("x", 40<<10), "version": 1, "page": "/"}, http.StatusRequestEntityTooLarge},
	} {
		r, b := h.comment(a.URL, tc.payload)
		expectStatus(t, r, tc.want, name)
		var e struct{ Error, Message string }
		if json.Unmarshal([]byte(b), &e) != nil || e.Error == "" || e.Message == "" {
			t.Fatalf("%s: answer %q is not a JSON error", name, b)
		}
	}
	if n := len(h.comments(c, a.ID, "all")); n != 0 {
		t.Fatalf("invalid posts stored %d comments", n)
	}
}

func TestFeedbackLimits(t *testing.T) {
	t.Run("per client", func(t *testing.T) {
		t.Setenv("REDROP_FEEDBACK_BURST", "2")
		h := newHarness(t)
		c := client.New(h.srv.URL, h.setupAdmin())
		a := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
		b := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
		for _, u := range []string{a.URL, b.URL} {
			r, _ := h.comment(u, okComment(1))
			expectStatus(t, r, http.StatusCreated, "within the burst")
		}
		// The bucket is per client, across artifacts. The refill (1 per
		// minute) cannot land within this test.
		r, body := h.comment(a.URL, okComment(1))
		expectStatus(t, r, http.StatusTooManyRequests, "past the burst")
		if r.Header.Get("Retry-After") != "60" || !strings.Contains(body, `"rate_limited"`) {
			t.Fatalf("Retry-After %q, answer %s", r.Header.Get("Retry-After"), body)
		}
		// The page itself is unaffected.
		r, _ = h.send("GET", a.URL, "/", nil, nav)
		expectStatus(t, r, http.StatusOK, "page while rate limited")
	})
	t.Run("per artifact", func(t *testing.T) {
		t.Setenv("REDROP_FEEDBACK_BURST", "20")
		t.Setenv("REDROP_FEEDBACK_MAX_PER_ARTIFACT", "2")
		h := newHarness(t)
		c := client.New(h.srv.URL, h.setupAdmin())
		a := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true})
		for range 2 {
			r, _ := h.comment(a.URL, okComment(1))
			expectStatus(t, r, http.StatusCreated, "under the cap")
		}
		r, b := h.comment(a.URL, okComment(1))
		expectStatus(t, r, http.StatusConflict, "at the cap")
		if !strings.Contains(b, `"feedback_full"`) || !strings.Contains(b, "isn't accepting more feedback") {
			t.Fatalf("cap answer: %s", b)
		}
		list := h.comments(c, a.ID, "all")
		if err := c.DeleteComment(context.Background(), a.ID, list[0].ID); err != nil {
			t.Fatal(err)
		}
		r, _ = h.comment(a.URL, okComment(1))
		expectStatus(t, r, http.StatusCreated, "after the owner deleted one")
	})
}

// secondUser creates a user through an invite and returns an API token.
func (h *harness) secondUser(name string) string {
	h.t.Helper()
	r := h.do("POST", "/api/v1/admin/invites", api.CreateInviteRequest{Note: name, TTL: "1d"}, true)
	expectStatus(h.t, r, 201, "invite")
	inv := decode[api.Invite](h.t, r)
	token := inv.URL[strings.LastIndex(inv.URL, "/")+1:]
	admin := h.session
	defer func() { h.session = admin }()
	h.session = nil
	r = h.do("POST", "/api/v1/invites/"+token+"/accept", api.AcceptInviteRequest{Username: name, Password: "long-enough-password"}, true)
	expectStatus(h.t, r, 200, "accept invite")
	h.session = cookieNamed(r, "redrop_session")
	r = h.do("POST", "/api/v1/tokens", api.CreateTokenRequest{Name: name}, true)
	expectStatus(h.t, r, 201, "token")
	return decode[api.CreatedToken](h.t, r).Secret
}

func TestFeedbackOwnerAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := client.New(h.srv.URL, h.setupAdmin())
	a := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true, OldVersions: true})

	quoted := map[string]any{"body": "Totals?\nPlease check.", "name": "Sam", "version": 1, "page": "/?x=1#totals",
		"quote": map[string]string{"exact": "totals are wrong", "prefix": "The ", "suffix": "."}}
	for _, p := range []any{quoted, okComment(1)} {
		r, _ := h.comment(a.URL, p)
		expectStatus(t, r, http.StatusCreated, "post")
	}
	if _, err := c.Update(ctx, a.ID, &client.Source{Reader: strings.NewReader(feedbackHTML), Name: "v2.html"}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := h.comment(a.URL, okComment(2))
	expectStatus(t, r, http.StatusCreated, "post on version 2")
	r, _ = h.comment(a.URL, okComment(1))
	expectStatus(t, r, http.StatusCreated, "post on the older version still on screen")

	all := h.comments(c, a.ID, "all")
	if len(all) != 4 || all[0].Body != "Totals?\nPlease check." || all[0].Name != "Sam" || all[0].Page != "/?x=1#totals" || all[0].Version != 1 ||
		all[0].Quote == nil || *all[0].Quote != (api.TextQuote{Exact: "totals are wrong", Prefix: "The ", Suffix: "."}) || all[0].ResolvedAt != nil {
		t.Fatalf("listed: %+v", all)
	}
	if all[1].Quote != nil || all[2].Version != 2 {
		t.Fatalf("second and third: %+v %+v", all[1], all[2])
	}
	if v2, err := c.ListFeedback(ctx, a.ID, "", 2); err != nil || len(v2) != 1 || v2[0].ID != all[2].ID {
		t.Fatalf("version filter: %+v %v", v2, err)
	}

	res, err := c.ResolveComment(ctx, a.ID, all[0].ID, true)
	if err != nil || res.ResolvedAt == nil {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	if open := h.comments(c, a.ID, "open"); len(open) != 3 {
		t.Fatalf("open after resolving one: %d", len(open))
	}
	if res, err = c.ResolveComment(ctx, a.ID, all[0].ID, false); err != nil || res.ResolvedAt != nil {
		t.Fatalf("reopen: %+v %v", res, err)
	}
	if err := c.DeleteComment(ctx, a.ID, all[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteComment(ctx, a.ID, all[1].ID); client.StatusOf(err) != 404 {
		t.Fatalf("deleting twice: %v", err)
	}
	if _, err := c.ListFeedback(ctx, a.ID, "bogus", 0); client.StatusOf(err) != 400 {
		t.Fatalf("bad status: %v", err)
	}
	r = h.do("GET", "/api/v1/artifacts/"+a.ID+"/feedback?version=0", nil, false)
	expectStatus(t, r, 400, "version 0")

	// Another user can't read or change them, and admins have no route to them.
	bob := client.New(h.srv.URL, h.secondUser("bob"))
	if _, err := bob.ListFeedback(ctx, a.ID, "", 0); client.StatusOf(err) != 404 {
		t.Fatalf("another user listing: %v", err)
	}
	if _, err := bob.ResolveComment(ctx, a.ID, all[0].ID, true); client.StatusOf(err) != 404 {
		t.Fatalf("another user resolving: %v", err)
	}
	if err := bob.DeleteComment(ctx, a.ID, all[0].ID); client.StatusOf(err) != 404 {
		t.Fatalf("another user deleting: %v", err)
	}
	// A comment is reached only through its own artifact.
	mine := h.share(c, feedbackHTML, client.PublishOptions{})
	if _, err := c.ResolveComment(ctx, mine.ID, all[0].ID, true); client.StatusOf(err) != 404 {
		t.Fatalf("comment through another artifact: %v", err)
	}
	expectStatus(t, h.do("GET", "/api/v1/admin/artifacts/"+a.ID+"/feedback", nil, false), 404, "admin route")
	r = h.do("GET", "/api/v1/admin/artifacts", nil, false)
	for _, it := range decode[api.ArtifactList](t, r).Items {
		if it.ID == a.ID && !it.Feedback {
			t.Fatal("admin metadata lacks the feedback setting")
		}
	}

	// Deleting the artifact deletes its comments.
	if err := c.DeleteArtifact(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.app.db.DB.QueryRow(`SELECT count(*) FROM feedback WHERE artifact_id = ?`, a.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("comments after deleting the artifact: %d %v", n, err)
	}
}

func TestFeedbackForm(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := client.New(h.srv.URL, h.setupAdmin())
	on := h.share(c, feedbackHTML, client.PublishOptions{Feedback: true, OldVersions: true})
	off := h.share(c, feedbackHTML, client.PublishOptions{})
	connect := func(u string) string { return "connect-src " + strings.TrimSuffix(u, "/") + "/__redrop/feedback" }
	uploaded := "<!doctype html><title>T</title><p>" + feedbackHTML + "</p>" // what h.share uploads

	r, b := h.page("GET", on.URL, "/", nav)
	if !strings.Contains(b, `<form id="rd-fb" class="rd-fb" data-version="1" data-current="1" hidden>`) {
		t.Fatalf("viewer page without the hidden form: %s", b)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, connect(on.URL)) || !strings.Contains(csp, "form-action 'none'") {
		t.Fatalf("viewer CSP: %s", csp)
	}
	// The artifact's own bytes are untouched.
	if _, b = h.page("GET", on.URL, "/", inFrame); b != uploaded {
		t.Fatalf("frame bytes changed: %q", b)
	}
	if _, b = h.page("GET", on.URL, "/?redrop-info=0", nav); b != uploaded {
		t.Fatalf("direct view changed: %q", b)
	}

	r, b = h.page("GET", off.URL, "/", nav)
	if strings.Contains(b, "rd-fb") || strings.Contains(r.Header.Get("Content-Security-Policy"), "connect-src") {
		t.Fatalf("form or connect-src with feedback off: %s %s", r.Header.Get("Content-Security-Policy"), b)
	}

	// An older version: the form knows which version is on screen.
	if _, err := c.Update(ctx, on.ID, &client.Source{Reader: strings.NewReader(feedbackHTML), Name: "v2.html"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, b = h.page("GET", on.URL, "/?redrop-version=1", nav); !strings.Contains(b, `data-version="1" data-current="2"`) {
		t.Fatalf("older version form: %s", b)
	}

	// The file page holds the form natively.
	f := shareFile(t, c, "notes.txt", "plain notes", client.PublishOptions{Feedback: true})
	r, b = h.page("GET", f.URL, "/", nav)
	if !strings.Contains(b, `id="rd-fb"`) || !strings.Contains(r.Header.Get("Content-Security-Policy"), connect(f.URL)) {
		t.Fatalf("file page: %s %s", r.Header.Get("Content-Security-Policy"), b)
	}
	g := shareFile(t, c, "notes.txt", "plain notes", client.PublishOptions{})
	if r, b = h.page("GET", g.URL, "/", nav); strings.Contains(b, "rd-fb") || strings.Contains(r.Header.Get("Content-Security-Policy"), "connect-src") {
		t.Fatal("file page form with feedback off")
	}
}
