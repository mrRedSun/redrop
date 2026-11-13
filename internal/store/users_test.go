package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestUsageAggregates checks the single-query user listing against the
// per-user numbers: bytes sum every retained version, artifacts are counted
// once regardless of version count, and users without artifacts get zeros.
func TestUsageAggregates(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	alice, err := createUser(ctx, db.DB, "alice", "Alice", "", "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createUser(ctx, db.DB, "bob", "Bob", "", "user", 0); err != nil {
		t.Fatal(err)
	}
	version := func(size int64) *NewVersion {
		return &NewVersion{Kind: "html", Entry: "index.html", Files: []File{{Path: "index.html", Blob: "b", Size: size, ContentType: "text/html"}}}
	}
	id, err := db.CreateArtifact(ctx, &NewArtifact{UserID: alice.ID, Title: "one"}, version(100), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddVersion(ctx, id, alice.ID, version(50), 0, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateArtifact(ctx, &NewArtifact{UserID: alice.ID, Title: "two"}, version(7), 0); err != nil {
		t.Fatal(err)
	}

	list, err := db.ListUsersWithUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Usage{}
	for _, u := range list {
		got[u.Username] = u.Usage
	}
	if want := (Usage{UsedBytes: 157, Artifacts: 2}); got["alice"] != want {
		t.Errorf("alice = %+v, want %+v", got["alice"], want)
	}
	if want := (Usage{}); got["bob"] != want || len(list) != 2 {
		t.Errorf("bob = %+v (users %d)", got["bob"], len(list))
	}
	one, err := db.UserUsage(ctx, alice.ID)
	if err != nil || one != got["alice"] {
		t.Errorf("UserUsage = %+v, %v; list says %+v", one, err, got["alice"])
	}
	if _, err := db.UserUsage(ctx, "missing"); err != ErrNotFound {
		t.Errorf("UserUsage(missing) err = %v", err)
	}
}
