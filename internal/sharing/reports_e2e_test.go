package sharing

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
)

func TestReportedLinkE2E(t *testing.T) {
	public, _ := url.Parse("https://app.example:8443")
	content, _ := url.Parse("https://usercontent.example:9443")
	s := &Service{cfg: &config.Config{PublicURL: public, ContentURL: content}}
	const slug = "amber-otter-quilt-river"
	const fragment = "#k=report-test-key"
	for _, tc := range []struct{ path, slug string }{
		{"/e/" + slug, slug},
		{"/e/" + slug + "/", slug},
		{"/e/" + slug + "?from=share", slug},
		{"/e/abcdefghijklmnopqrstuvwxyz", "abcdefghijklmnopqrstuvwxyz"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, hash := range []string{"", fragment} {
				gotSlug, link, err := s.reportedLink(s.cfg.PublicOrigin() + tc.path + hash)
				if err != nil {
					t.Fatal("configured app intake URL rejected")
				}
				if gotSlug != tc.slug || link != s.cfg.ArtifactURL(tc.slug) {
					t.Fatal("intake URL was not normalized to the canonical content URL")
				}
			}
		})
	}
	for name, raw := range map[string]string{
		"hostile origin":    "https://evil.example:8443/e/" + slug,
		"host suffix":       "https://app.example.evil.example:8443/e/" + slug,
		"content apex":      "https://usercontent.example:9443/e/" + slug,
		"wrong scheme":      "http://app.example:8443/e/" + slug,
		"wrong port":        "https://app.example/e/" + slug,
		"credentials":       "https://user:pass@app.example:8443/e/" + slug,
		"missing slug":      s.cfg.PublicOrigin() + "/e/",
		"invalid slug":      s.cfg.PublicOrigin() + "/e/not-a-slug",
		"uppercase slug":    s.cfg.PublicOrigin() + "/e/Amber-otter-quilt-river",
		"extra segment":     s.cfg.PublicOrigin() + "/e/" + slug + "/page.html",
		"double slash":      s.cfg.PublicOrigin() + "/e/" + slug + "//",
		"wrong prefix":      s.cfg.PublicOrigin() + "/other/" + slug,
		"encoded separator": s.cfg.PublicOrigin() + "/e%2f" + slug,
		"encoded slug":      s.cfg.PublicOrigin() + "/e/%61mber-otter-quilt-river",
		"dot segment":       s.cfg.PublicOrigin() + "/e/../e/" + slug,
		"invalid escape":    s.cfg.PublicOrigin() + "/e/%zz",
		"oversized URL":     s.cfg.PublicOrigin() + "/e/" + slug + "?" + strings.Repeat("x", maxReportURL),
	} {
		t.Run(name, func(t *testing.T) {
			gotSlug, link, err := s.reportedLink(raw + fragment)
			if err == nil {
				t.Fatal("invalid intake URL accepted")
			}
			if gotSlug != "" || link != "" || strings.Contains(err.Error(), "report-test-key") {
				t.Fatal("invalid intake URL exposed its input")
			}
		})
	}
}

func TestSubmitReportE2EStoresKeylessURL(t *testing.T) {
	s, owner := budgetService(t, 10000, 10000)
	s.cfg.ReportsMaxOpen, s.cfg.ReportsMaxOpenPerArtifact = 10, 10
	s.reportLimiter.Close()
	s.reportLimiter = security.NewLimiter(10, time.Hour, 100, security.EvictOldest, time.Now)
	ctx := context.Background()
	id, err := s.db.CreateArtifact(ctx, &store.NewArtifact{UserID: owner.UserID, E2E: true, E2EKeyCheck: "check"},
		&store.NewVersion{Kind: "bundle", E2E: true, Expect: 1, Files: []store.File{{Path: "manifest", Blob: "b", Size: 1, ContentType: "application/octet-stream"}}}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.db.ArtifactByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for i, link := range []string{s.cfg.E2ELink(a.Slug), s.cfg.E2ELink(a.Slug) + "/", s.cfg.ArtifactURL(a.Slug)} {
		if err := s.SubmitReport(ctx, fmt.Sprintf("192.0.2.%d", i+1), ReportInput{URL: link + "#k=report-test-key", Category: CategoryPhishing}); err != nil {
			t.Fatal("e2e report rejected")
		}
	}
	rows, total, err := s.db.ListReports(ctx, "", store.ReportVisitor, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatal("e2e reports were not stored")
	}
	for _, row := range rows {
		if row.ArtifactID != id || row.URL != s.cfg.ArtifactURL(a.Slug) {
			t.Fatal("stored report contains an unexpected URL or artifact")
		}
	}
}
