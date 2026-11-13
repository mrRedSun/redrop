package sharing

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/security"
	"github.com/mrRedSun/redrop/internal/store"
)

func testService() *Service {
	secret := []byte("0123456789abcdef0123456789abcdef")
	return &Service{
		cfg:           &config.Config{ExpiredRetention: 72 * time.Hour, ViewWindowLimited: time.Hour},
		accessSigner:  security.NewSigner(secret, "access"),
		previewSigner: security.NewSigner(secret, "preview"),
		seenSigner:    security.NewSigner(secret, "seen"),
	}
}

// TestDecide covers the gate matrix: lifecycle × gating × presented grants.
func TestDecide(t *testing.T) {
	s := testService()
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	type art = store.Artifact
	open := art{ID: "a1", AccessGen: 3, ExpiresAt: &future}
	password := art{ID: "a1", AccessGen: 3, PasswordHash: "x", ExpiresAt: &future}
	limited := art{ID: "a1", AccessGen: 3, MaxViews: 1}
	exhausted := art{ID: "a1", AccessGen: 3, MaxViews: 1, Views: 1}
	expired := art{ID: "a1", AccessGen: 3, ExpiresAt: &past}
	revoked := art{ID: "a1", AccessGen: 3, Revoked: true}

	exp := now.Add(time.Hour)
	visit := func(a art) string { return s.accessSigner.Sign(grantPayload(&a, "visit"), exp) }
	preview := func(a art) string { return s.previewSigner.Sign(grantPayload(&a, "preview"), exp) }
	stale := func(a art) art { a.AccessGen--; return a }
	other := func(a art) art { a.ID = "a2"; return a }

	cases := []struct {
		name string
		a    art
		g    Grants
		want Decision
	}{
		{"open, no grant", open, Grants{}, Serve},
		{"password, no grant", password, Grants{}, ShowGate},
		{"password, visit grant", password, Grants{Visit: visit(password)}, Serve},
		{"password, grant from older generation", password, Grants{Visit: visit(stale(password))}, ShowGate},
		{"password, grant for another artifact", password, Grants{Visit: visit(other(password))}, ShowGate},
		{"password, preview cookie used as visit", password, Grants{Visit: preview(password)}, ShowGate},
		{"password, garbage grant", password, Grants{Visit: "garbage"}, ShowGate},
		{"limited, no grant", limited, Grants{}, ShowGate},
		{"exhausted, no grant", exhausted, Grants{}, ShowGone},
		{"exhausted, visit grant within window", exhausted, Grants{Visit: visit(exhausted)}, Serve},
		{"expired, visit grant", expired, Grants{Visit: visit(expired)}, ShowGone},
		{"revoked, visit grant", revoked, Grants{Visit: visit(revoked)}, ShowGone},
		{"password, owner preview", password, Grants{Preview: preview(password)}, ServeOwnerPreview},
		{"expired, owner preview", expired, Grants{Preview: preview(expired)}, ServeOwnerPreview},
		{"revoked, owner preview", revoked, Grants{Preview: preview(revoked)}, ShowGone},
		{"preview from older generation", password, Grants{Preview: preview(stale(password))}, ShowGate},
		{"visit cookie used as preview", password, Grants{Preview: visit(password)}, ShowGate},
	}
	for _, c := range cases {
		a := c.a
		if got := s.Decide(&a, c.g, now); got != c.want {
			t.Errorf("%s: Decide = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRedeemPreviewBindsGeneration(t *testing.T) {
	s := testService()
	a := store.Artifact{ID: "a1", AccessGen: 2}
	tok := s.previewSigner.Sign(previewTokenPayload(&a), time.Now().Add(time.Minute))
	if _, ok := s.RedeemPreview(&a, tok); !ok {
		t.Fatal("valid preview token rejected")
	}
	a.AccessGen++ // link rotated or revoked
	if _, ok := s.RedeemPreview(&a, tok); ok {
		t.Fatal("preview token survived an access generation bump")
	}
}

// TestAdminArtifactCarriesNoSecrets pins the admin view's fields. Adding a
// link, slug, or password field here would break DESIGN.md's admin privacy.
func TestAdminArtifactCarriesNoSecrets(t *testing.T) {
	var names []string
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			names = append(names, f.Name)
		}
	}
	walk(reflect.TypeOf(AdminArtifact{}))
	sort.Strings(names)
	want := []string{"CreatedAt", "ExpiresAt", "Files", "HasPassword", "ID", "InfoButton", "Kind", "LastViewedAt", "MaxViews", "OwnerUsername",
		"Previews", "PurgeAt", "Size", "Status", "Title", "TokenName", "TotalSize", "UpdatedAt", "Version", "Versions", "Views"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("AdminArtifact fields changed:\n got %v\nwant %v", names, want)
	}
}

func TestMetaStatusAndPurge(t *testing.T) {
	s := testService()
	now := time.Now()
	past := now.Add(-time.Hour)
	expired := s.meta(&store.Artifact{ExpiresAt: &past}, now)
	if expired.Status != "expired" || expired.PurgeAt == nil || !expired.PurgeAt.Equal(past.Add(72*time.Hour)) {
		t.Errorf("expired: %+v", expired)
	}
	exhausted := s.meta(&store.Artifact{MaxViews: 1, Views: 1, ExhaustedAt: &past}, now)
	if exhausted.Status != "exhausted" || exhausted.PurgeAt == nil || !exhausted.PurgeAt.Equal(past.Add(time.Hour)) {
		t.Errorf("exhausted: %+v", exhausted)
	}
	active := s.meta(&store.Artifact{PasswordHash: "h"}, now)
	if active.Status != "active" || active.PurgeAt != nil || !active.HasPassword {
		t.Errorf("active: %+v", active)
	}
}
