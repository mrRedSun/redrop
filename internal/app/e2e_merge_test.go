package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/client"
)

// The encrypted frame uses pinned version paths, not the plaintext live
// reloader. A lease must not claim live mode when no viewer can use it.
func TestE2EMergeViewerPolicies(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	page := writeFile(t, t.TempDir(), "page.html", "<p>encrypted</p>")
	a, _, err := c.ShareE2E(context.Background(), &client.Source{Path: page}, client.PublishOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var problem *client.Error
	if _, err := c.HoldLive(context.Background(), a.ID, liveSession); !errors.As(err, &problem) || problem.Status != 409 || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("encrypted live lease: %v, want explicit conflict", err)
	}
	b := body(h.fetchContent(a.URL, "/", nil, docHeaders...))
	want := `href="http://app.test/report#url=` + url.QueryEscape(a.URL) + `"`
	if !strings.Contains(b, want) || strings.Contains(b, `id="rd-live"`) {
		t.Fatalf("encrypted viewer must link the report form and not advertise live reload: %s", b)
	}
}
