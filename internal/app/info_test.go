package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// page fetches path on an artifact host with exactly the given headers.
func (h *harness) page(method, artifactURL, path string, hdr map[string]string, cookies ...*http.Cookie) (*http.Response, string) {
	h.t.Helper()
	u, _ := url.Parse(artifactURL)
	req, _ := http.NewRequest(method, h.srv.URL+path, nil)
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

func shareFile(t *testing.T, c *client.Client, name, content string, opts client.PublishOptions) *api.Artifact {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := c.Share(context.Background(), &client.Source{Path: p}, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var nav = map[string]string{"Sec-Fetch-Dest": "document", "Accept": "text/html"}

// TestUpdateWithInfoSetting: `redrop update --info/--no-info` saves the
// setting before uploading, never after a published version, and says so
// when publishing then fails.
func TestUpdateWithInfoSetting(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	ctx := context.Background()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<body>v1</body>"), 0o600)
	a, err := c.Share(ctx, &client.Source{Path: dir}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	off, on := false, true

	// Bad source: nothing changes.
	if _, err := c.UpdateWithOptions(ctx, a.ID, &client.Source{Path: filepath.Join(dir, "missing")}, client.VersionOptions{InfoButton: &off}, nil); err == nil {
		t.Fatal("missing source accepted")
	}
	if got, _ := c.GetArtifact(ctx, a.ID); !got.InfoButton || got.Versions != 1 {
		t.Fatalf("bad source changed the artifact: %+v", got)
	}

	// Publishing fails after the setting was saved: an explicit error, no version.
	_, err = c.UpdateWithOptions(ctx, a.ID, &client.Source{Path: dir, Entry: "nope.html"}, client.VersionOptions{InfoButton: &off}, nil)
	var partial *client.ErrSettingsOnly
	if !errors.As(err, &partial) || client.StatusOf(err) != 400 {
		t.Fatalf("want ErrSettingsOnly wrapping a 400, got %v", err)
	}
	if got, _ := c.GetArtifact(ctx, a.ID); got.InfoButton || got.Versions != 1 {
		t.Fatalf("after failed publish: %+v", got)
	}

	// Success: both apply.
	got, err := c.UpdateWithOptions(ctx, a.ID, &client.Source{Path: dir}, client.VersionOptions{InfoButton: &on}, nil)
	if err != nil || !got.InfoButton || got.Versions != 2 {
		t.Fatalf("update with --info: %v %+v", err, got)
	}
}
