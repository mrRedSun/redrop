package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/client"
)

// Review 27b M-1, through the client the CLI uses: a .tar.gz cut inside a file
// body is the archive's fault. Each share is refused as corrupt and releases
// its upload, so eight of them do not use up the default eight sessions.
func TestTruncatedBundlesDoNotExhaustUploadSessions(t *testing.T) {
	h := newHarness(t)
	c := client.New(h.srv.URL, h.setupAdmin())
	body := make([]byte, 64<<10)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Size: int64(len(body)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(t.TempDir(), "site.tar.gz")
	if err := os.WriteFile(truncated, raw.Bytes()[:raw.Len()/2], 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		_, err := c.Share(context.Background(), &client.Source{Path: truncated, Kind: "bundle"}, client.PublishOptions{}, nil)
		if client.StatusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "corrupt bundle archive") {
			t.Fatalf("truncated share %d: %v, want 400 corrupt bundle archive", i+1, err)
		}
	}
	h.share(c, "after eight truncated bundles", client.PublishOptions{})
}
