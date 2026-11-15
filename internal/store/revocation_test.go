package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mrRedSun/redrop/pkg/api"
)

// Migration 4 keeps revocations made before it as the owner's: nobody can
// tell who made them. An administrator's revocation is stored apart.
func TestAdminRevocationMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schemaV1 + `;` + schemaV2 + `;` + schemaV3 + `; PRAGMA user_version = 3;
		INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES ('u', 'alice', '', '', 'user', 0, 1);
		INSERT INTO artifacts (id, slug, user_id, title, current_version, revoked, created_at, updated_at) VALUES ('a', 's', 'u', 'old', 1, 1, 1, 1);`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := db.ArtifactByID(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Revoked || a.AdminRevoked || a.Status(time.Now()) != api.StatusRevoked {
		t.Fatalf("migrated revocation: owner %v admin %v", a.Revoked, a.AdminRevoked)
	}

	// Each flag alone revokes; clearing one leaves the other.
	no, yes := false, true
	gen := a.AccessGen
	if err := db.UpdateArtifact(ctx, "a", ArtifactUpdate{AdminRevoked: &yes, Revoked: &no}); err != nil {
		t.Fatal(err)
	}
	a, _ = db.ArtifactByID(ctx, "a")
	if a.Revoked || !a.AdminRevoked || a.Status(time.Now()) != api.StatusRevoked || a.AccessGen <= gen {
		t.Fatalf("admin revocation: %+v", a)
	}
	if err := db.UpdateArtifact(ctx, "a", ArtifactUpdate{AdminRevoked: &no}); err != nil {
		t.Fatal(err)
	}
	if a, _ = db.ArtifactByID(ctx, "a"); a.Status(time.Now()) != api.StatusActive {
		t.Fatalf("after clearing both: %s", a.Status(time.Now()))
	}
}
