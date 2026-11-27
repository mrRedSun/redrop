package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/e2e"
	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

func TestTunnelE2ERefusedByServer(t *testing.T) {
	f := newTunnelFixture(t, nil)
	a := f.create(api.CreateTunnelRequest{})
	if a.E2E || a.E2ELink != "" {
		t.Fatalf("a tunnel advertises encryption: %+v", a)
	}
	for _, value := range []any{true, map[string]any{"version": 1, "key_check": "not-a-key"}} {
		expectStatus(t, f.h.do("POST", "/api/v1/tunnels", map[string]any{"e2e": value}, true), 400, "encrypted tunnel creation")
		expectStatus(t, f.h.do("PATCH", "/api/v1/artifacts/"+a.ID, map[string]any{"e2e": value}, true), 400, "encrypted tunnel settings")
	}
	expectStatus(t, f.h.do("POST", "/api/v1/uploads", map[string]any{"size": 1, "artifact_id": a.ID, "e2e": true}, true), 400, "encrypted upload for tunnel")

	// A valid encrypted upload cannot become a tunnel or one of its versions.
	ctx := context.Background()
	reservation, err := f.c.ReserveArtifact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var key e2e.Key
	file := writeFile(t, t.TempDir(), "sealed.tar.gz", encryptedBundle(t, key, reservation.ID, 1))
	for _, creating := range []bool{true, false} {
		upload, sum, err := f.c.UploadFileFor(ctx, file, client.UploadTarget{E2E: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		pub := api.PublishRequest{UploadID: upload, SHA256: sum, Kind: api.KindTunnel,
			E2E: &api.E2EPublish{KeyCheck: key.Check(reservation.ID), Version: 1, Reservation: reservation.Token}}
		if creating {
			_, err = f.c.CreateArtifact(ctx, &api.CreateArtifactRequest{PublishRequest: pub})
		} else {
			_, err = f.c.CreateVersion(ctx, a.ID, &pub)
		}
		if client.StatusOf(err) != http.StatusBadRequest {
			t.Fatalf("encrypted tunnel publication (creating=%v): %v", creating, err)
		}
	}
	got, err := f.c.GetArtifact(ctx, a.ID)
	if err != nil || got.E2E || got.Versions != 1 || got.E2ELink != "" {
		t.Fatalf("tunnel changed after encrypted requests: %+v %v", got, err)
	}
}

func TestTunnelGetsNoE2EHelperViewerOrWorker(t *testing.T) {
	f := newTunnelFixture(t, nil)
	a := f.create(api.CreateTunnelRequest{})
	f.connect(f.c, a)
	origin, _ := url.Parse(a.URL)
	slug := strings.Split(origin.Hostname(), ".")[0]

	// App intake remains syntax-only even for tunnels and unknown slugs.
	for _, s := range []string{slug, "amber-otter-quilt-river"} {
		r := f.h.raw("GET", "/e/"+s, "", nil)
		b := body(r)
		if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" || !strings.Contains(b, `id="rd-intake"`) {
			t.Fatalf("syntax-only app intake: %d %s", r.StatusCode, b)
		}
	}
	for _, method := range []string{"GET", "HEAD"} {
		r, b := f.h.send(method, a.URL, "/__redrop/e2e-intake", nil, doc)
		if r.StatusCode != 404 || strings.Contains(b, "rd-e2e-intake") {
			t.Fatalf("tunnel helper %s: %d %s", method, r.StatusCode, b)
		}
	}
	// Unknown and plaintext slugs get the same miss, so the helper is no
	// free oracle for which slugs are tunnels.
	plainFile := writeFile(t, t.TempDir(), "plain.html", "<p>plain</p>")
	plain, err := f.c.Share(context.Background(), &client.Source{Path: plainFile}, client.PublishOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(a.URL, slug, "amber-otter-quilt-river", 1)
	for _, link := range []string{plain.URL, unknown} {
		r, b := f.h.send("GET", link, "/__redrop/e2e-intake", nil, doc)
		if r.StatusCode != 404 || strings.Contains(b, "rd-e2e-intake") {
			t.Fatalf("helper for a non-encrypted slug: %d %s", r.StatusCode, b)
		}
	}
	worker := f.h.fetchContent(a.URL, "/__redrop/e2e-sw.js", nil, "Sec-Fetch-Dest", "serviceworker", "Service-Worker", "script")
	expectStatus(t, worker, 404, "tunnel worker script")
	b := body(f.h.fetchContent(a.URL, "/", nil, docHeaders...))
	if !strings.Contains(b, "Live app") || strings.Contains(b, "data-e2e") || strings.Contains(b, "End-to-end encrypted") || strings.Contains(b, `id="rd-size"`) || strings.Contains(b, "rd-revs") {
		t.Fatalf("tunnel panel and viewer fields clashed: %s", b)
	}
	f.mu.Lock()
	forwarded := len(f.seen)
	f.mu.Unlock()
	if forwarded != 0 {
		t.Fatal("an intake, worker or viewer request reached the tunneled app")
	}
}
