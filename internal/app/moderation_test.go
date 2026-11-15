package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
)

// tokenFor creates an API token with a user's session.
func (h *harness) tokenFor(u testUser) string {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/tokens", strings.NewReader(`{"name":"cli"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Redrop-Request", "1")
	req.AddCookie(u.session)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	expectStatus(h.t, resp, 201, "create token")
	return decode[api.CreatedToken](h.t, resp).Secret
}

// Security review A8, replayed: an owner (here through an API token) cannot
// undo an administrator's revocation; only an administrator can.
func TestOwnerCannotUndoAdminRevocation(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin() // h.do uses the admin's session
	bob := client.New(h.srv.URL, h.tokenFor(h.inviteUser("bob")))
	ctx := context.Background()
	a := h.share(bob, "phishing", client.PublishOptions{})
	host := hostOf(t, a.URL)
	expectStatus(t, h.get(host, nil), 200, "visitor before")

	r := h.do("PATCH", "/api/v1/admin/artifacts/"+a.ID, api.AdminArtifactUpdate{Revoked: true}, true)
	expectStatus(t, r, 200, "admin revokes")
	if got := decode[api.Artifact](t, r); got.Status != api.StatusRevoked || !got.RevokedByAdmin {
		t.Fatalf("after the admin's revoke: status=%s revoked_by_admin=%v", got.Status, got.RevokedByAdmin)
	}
	expectStatus(t, h.get(host, nil), 404, "visitor after the admin's revoke")

	restore := false
	_, err := bob.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{Revoked: &restore})
	if client.StatusOf(err) != 403 || !strings.Contains(err.Error(), "only an administrator") {
		t.Fatalf("owner restore: %v, want 403", err)
	}
	expectStatus(t, h.get(host, nil), 404, "visitor after the owner's attempt")
	got, err := bob.GetArtifact(ctx, a.ID)
	if err != nil || got.Status != api.StatusRevoked || !got.RevokedByAdmin {
		t.Fatalf("owner's view: %+v %v", got, err)
	}
	// Other owner settings still work, and do not bring the link back.
	title := "renamed"
	if got, err := bob.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{Title: &title}); err != nil || got.Status != api.StatusRevoked {
		t.Fatalf("owner rename while revoked: %+v %v", got, err)
	}

	r = h.do("GET", "/api/v1/admin/artifacts", nil, false)
	if list := decode[api.ArtifactList](t, r); len(list.Items) != 1 || !list.Items[0].RevokedByAdmin {
		t.Fatalf("admin list: %+v", list.Items)
	}
	r = h.do("PATCH", "/api/v1/admin/artifacts/"+a.ID, api.AdminArtifactUpdate{Revoked: false}, true)
	expectStatus(t, r, 200, "admin restores")
	if got := decode[api.Artifact](t, r); got.Status != api.StatusActive || got.RevokedByAdmin {
		t.Fatalf("after the admin's restore: %+v", got)
	}
	expectStatus(t, h.get(host, nil), 200, "visitor after the admin's restore")
}

// The two revocations are independent: an administrator's restore leaves
// the owner's own revocation in place.
func TestAdminRestoreKeepsOwnerRevocation(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()
	bob := client.New(h.srv.URL, h.tokenFor(h.inviteUser("bob")))
	ctx := context.Background()
	a := h.share(bob, "page", client.PublishOptions{})
	revoke := true
	if _, err := bob.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{Revoked: &revoke}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, h.do("PATCH", "/api/v1/admin/artifacts/"+a.ID, api.AdminArtifactUpdate{Revoked: true}, true), 200, "admin revokes")
	r := h.do("PATCH", "/api/v1/admin/artifacts/"+a.ID, api.AdminArtifactUpdate{Revoked: false}, true)
	if got := decode[api.Artifact](t, r); got.Status != api.StatusRevoked || got.RevokedByAdmin {
		t.Fatalf("admin restore of an owner-revoked artifact: %+v", got)
	}
	restore := false
	if got, err := bob.UpdateArtifact(ctx, a.ID, &api.UpdateArtifactRequest{Revoked: &restore}); err != nil || got.Status != api.StatusActive {
		t.Fatalf("owner restores their own revocation: %+v %v", got, err)
	}
}
