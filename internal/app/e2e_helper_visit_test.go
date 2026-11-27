package app

import (
	"context"
	"net/url"
	"testing"

	"github.com/mrRedSun/redrop/pkg/client"
)

// Reopening the full link during a one-time visit must still hand off the
// key. An exhausted link is gone only for a visitor without its visit grant.
func TestE2EHelperHonorsOneTimeVisit(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	file := writeFile(t, t.TempDir(), "once.html", "<p>one-time</p>")
	a, _, err := c.ShareE2E(context.Background(), &client.Source{Path: file}, client.PublishOptions{MaxViews: 1}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/e2e-intake", nil, docHeaders...), 200, "helper before Open")
	r := h.content("POST", a.URL, "/__redrop/open", url.Values{"next": {"/"}}, "")
	expectStatus(t, r, 303, "Open one-time link")
	visit := r.Cookies()
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/e2e-intake", nil, docHeaders...), 404, "helper for second visitor")
	expectStatus(t, h.fetchContent(a.URL, "/__redrop/e2e-intake", visit, docHeaders...), 200, "helper during the same visit")
	got, err := c.GetArtifact(context.Background(), a.ID)
	if err != nil || got.Views != 1 {
		t.Fatalf("helper changed one-time views: %+v %v", got, err)
	}
}
