package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestInfoButtonMigration: artifacts from before the setting existed follow
// the server default (NULL); new and updated ones store an explicit choice.
func TestInfoButtonMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schemaV1 + `; PRAGMA user_version = 1;
		INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES ('u', 'alice', '', '', 'admin', 0, 1);
		INSERT INTO artifacts (id, slug, user_id, title, current_version, created_at, updated_at) VALUES ('a', 's', 'u', 'old', 1, 1, 1);
		INSERT INTO versions (artifact_id, number, kind, size, files, created_at) VALUES ('a', 1, 'html', 3, 1, 42);`); err != nil {
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
	if a.InfoButton != nil || a.Published.Unix() != 42 {
		t.Fatalf("migrated artifact: info %v published %v", a.InfoButton, a.Published)
	}

	off := false
	id, err := db.CreateArtifact(ctx, &NewArtifact{UserID: "u", InfoButton: &off}, &NewVersion{Kind: "html"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := db.ArtifactByID(ctx, id); b.InfoButton == nil || *b.InfoButton {
		t.Fatalf("created with off: %v", b.InfoButton)
	}
	on := true
	if err := db.UpdateArtifact(ctx, "a", ArtifactUpdate{InfoButton: &on}); err != nil {
		t.Fatal(err)
	}
	if a, _ = db.ArtifactByID(ctx, "a"); a.InfoButton == nil || !*a.InfoButton {
		t.Fatalf("updated to on: %v", a.InfoButton)
	}
}
