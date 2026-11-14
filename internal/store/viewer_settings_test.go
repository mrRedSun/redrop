package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestViewerSettingsMigration: artifacts from before the settings existed
// follow the server default for the info button (NULL) and keep older
// versions closed; new and updated ones store an explicit choice.
func TestViewerSettingsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schemaV1 + `; PRAGMA user_version = 1;
		INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES ('u', 'alice', '', '', 'admin', 0, 1);
		INSERT INTO artifacts (id, slug, user_id, title, current_version, created_at, updated_at) VALUES ('a', 's', 'u', 'old', 1, 1, 1);
		INSERT INTO versions (artifact_id, number, kind, size, files, note, created_at) VALUES ('a', 1, 'html', 3, 1, 'private before', 42);`); err != nil {
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
	if a.InfoButton != nil || a.Published.Unix() != 42 || a.OldVersions {
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
	if err := db.UpdateArtifact(ctx, "a", ArtifactUpdate{OldVersions: &on}); err != nil {
		t.Fatal(err)
	}
	if a, _ = db.ArtifactByID(ctx, "a"); !a.OldVersions {
		t.Fatal("old versions not enabled")
	}
	// The migrated artifact continues numbering after its existing version.
	if n, err := db.AddVersion(ctx, "a", "u", &NewVersion{Kind: "html", Note: "public now"}, 0, 0); err != nil || n != 2 {
		t.Fatalf("AddVersion after migration = %d, %v", n, err)
	}
	// Notes written before viewers could see them stay private; new ones are public.
	rows, err := db.ListVersions(ctx, "a")
	if err != nil || len(rows) != 2 || rows[0].NotePublic != true || rows[1].NotePublic != false || rows[1].Note != "private before" {
		t.Fatalf("note visibility after migration: %+v %v", rows, err)
	}
}

// TestVersionNumbersNeverReused: a raw origin is pinned to a version number,
// so deleting the newest version must not let the next publish reuse it.
func TestVersionNumbersNeverReused(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := createUser(ctx, db.DB, "alice", "", "", "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateArtifact(ctx, &NewArtifact{UserID: u.ID}, &NewVersion{Kind: "html"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for want := 2; want <= 3; want++ {
		if n, _ := db.AddVersion(ctx, id, u.ID, &NewVersion{Kind: "html"}, 0, 0); n != want {
			t.Fatalf("version %d, want %d", n, want)
		}
	}
	if err := db.SetCurrentVersion(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteVersion(ctx, id, 3); err != nil {
		t.Fatal(err)
	}
	if n, err := db.AddVersion(ctx, id, u.ID, &NewVersion{Kind: "html"}, 0, 0); err != nil || n != 4 {
		t.Fatalf("after deleting v3, next version = %d (%v), want 4", n, err)
	}
}
