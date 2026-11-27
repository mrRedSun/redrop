package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The end-to-end migration runs on a database at the previous schema
// version and leaves every existing artifact and version plaintext.
func TestE2EMigrationFromPreviousVersion(t *testing.T) {
	const version = 9
	prev := version - 1
	if len(migrations) != version {
		t.Fatalf("end-to-end migration must be last: got %d, want %d", len(migrations), version)
	}
	path := filepath.Join(t.TempDir(), "migration.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(strings.Join(migrations[:prev], ";\n") + `;PRAGMA user_version=` + strconv.Itoa(prev) + `;
 INSERT INTO users(id,username,quota_bytes,created_at) VALUES('u','publisher',0,1);
 INSERT INTO artifacts(id,slug,user_id,created_at,updated_at,current_version,last_version) VALUES('a','amber-otter-quilt-river','u',1,1,1,1);
 INSERT INTO versions(artifact_id,number,kind,size,files,created_at) VALUES('a',1,'html',1,1,1);
 INSERT INTO reports(id,source,artifact_id,owner_id,category,created_at) VALUES('r','visitor','a','u','other',1);
 INSERT INTO audit(at,actor,action,report_id) VALUES(1,'u','report.resolved','r');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotVersion, reports, audit int
	if err := db.DB.QueryRow(`PRAGMA user_version`).Scan(&gotVersion); err != nil || gotVersion != version {
		t.Fatalf("schema %d, want %d: %v", gotVersion, version, err)
	}
	if err := db.DB.QueryRow(`SELECT (SELECT count(*) FROM reports), (SELECT count(*) FROM audit)`).Scan(&reports, &audit); err != nil || reports != 1 || audit != 1 {
		t.Fatalf("schema 8 reports/audit changed: %d/%d: %v", reports, audit, err)
	}
	a, err := db.ArtifactByID(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if a.E2E || a.E2EKeyCheck != "" || a.CurrentE2E {
		t.Fatalf("migrated artifact is not plaintext: %+v", a)
	}
	v, err := db.VersionAt(context.Background(), "a", 1)
	if err != nil || v.E2E {
		t.Fatalf("migrated version: %+v %v", v, err)
	}
}

func TestE2EArtifactsAndVersionNumbers(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := createUser(ctx, db.DB, "alice", "Alice", "", "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	version := func(expect int) *NewVersion {
		return &NewVersion{Kind: "bundle", E2E: true, Expect: expect, Files: []File{{Path: "manifest", Blob: "b", Size: 1, ContentType: "application/octet-stream"}}}
	}
	const id = "k2j4m9qx7d4abcde"
	got, err := db.CreateArtifact(ctx, &NewArtifact{ID: id, UserID: u.ID, E2E: true, E2EKeyCheck: "check"}, version(1), 0)
	if err != nil || got != id {
		t.Fatalf("create with a reserved ID: %q %v", got, err)
	}
	if _, err := db.CreateArtifact(ctx, &NewArtifact{ID: id, UserID: u.ID, E2E: true, E2EKeyCheck: "check"}, version(1), 0); !errors.Is(err, ErrIDTaken) {
		t.Fatalf("reusing an ID: %v", err)
	}
	if _, err := db.CreateArtifact(ctx, &NewArtifact{UserID: u.ID}, version(2), 0); !errors.Is(err, ErrVersionTaken) {
		t.Fatalf("a new artifact's version expecting 2: %v", err)
	}
	a, err := db.ArtifactByID(ctx, id)
	if err != nil || !a.E2E || a.E2EKeyCheck != "check" || !a.CurrentE2E || a.LastVersion != 1 {
		t.Fatalf("stored artifact: %+v %v", a, err)
	}
	if _, err := db.AddVersion(ctx, id, u.ID, version(3), 0, 0); !errors.Is(err, ErrVersionTaken) {
		t.Fatalf("version expecting 3 when 2 is next: %v", err)
	}
	if n, err := db.AddVersion(ctx, id, u.ID, version(2), 0, 0); err != nil || n != 2 {
		t.Fatalf("version expecting 2: %d %v", n, err)
	}
	// Numbers are never reused, so a deleted version's number stays taken.
	if err := db.SetCurrentVersion(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteVersion(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddVersion(ctx, id, u.ID, version(2), 0, 0); !errors.Is(err, ErrVersionTaken) {
		t.Fatalf("reusing a deleted version's number: %v", err)
	}
	if n, err := db.AddVersion(ctx, id, u.ID, version(3), 0, 0); err != nil || n != 3 {
		t.Fatalf("version expecting 3: %d %v", n, err)
	}
	v, err := db.VersionAt(ctx, id, 3)
	if err != nil || !v.E2E {
		t.Fatalf("version 3: %+v %v", v, err)
	}
	// Preserve main's suspension-at-commit rule alongside encrypted fields.
	if _, err := db.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	blockedID, err := db.CreateArtifact(ctx, &NewArtifact{UserID: u.ID, E2E: true, E2EKeyCheck: "check"}, version(1), 0)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := db.ArtifactByID(ctx, blockedID)
	if err != nil || !blocked.AdminRevoked || !blocked.E2E || !blocked.CurrentE2E || blocked.E2EKeyCheck != "check" {
		t.Fatalf("encrypted publication by suspended owner: %+v %v", blocked, err)
	}
}
