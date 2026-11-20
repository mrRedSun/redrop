package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestFeedbackMigration: artifacts from before migration 7 have feedback
// off and no comments; the switch can then be set either way. Main's
// migrations 4 to 6 ran before it and stay applied.
func TestFeedbackMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(strings.Join([]string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6}, ";\n") + `; PRAGMA user_version = 6;
		INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES ('u', 'alice', '', '', 'admin', 0, 1);
		INSERT INTO artifacts (id, slug, user_id, title, current_version, last_version, created_at, updated_at) VALUES ('a', 's', 'u', 'old', 2, 3, 1, 1);
		INSERT INTO versions (artifact_id, number, kind, size, files, created_at) VALUES ('a', 2, 'html', 3, 1, 42);`); err != nil {
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
	if a.Feedback || a.LastVersion != 3 || a.AdminRevoked {
		t.Fatalf("migrated artifact: feedback %v, last version %d, admin revoked %v", a.Feedback, a.LastVersion, a.AdminRevoked)
	}
	var v int
	if err := db.DB.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version %d (%v), want %d", v, err, len(migrations))
	}
	if list, err := db.ListComments(ctx, "a", CommentFilter{}); err != nil || len(list) != 0 {
		t.Fatalf("migrated comments: %v %v", list, err)
	}
	on := true
	if err := db.UpdateArtifact(ctx, "a", ArtifactUpdate{Feedback: &on}); err != nil {
		t.Fatal(err)
	}
	if a, _ = db.ArtifactByID(ctx, "a"); !a.Feedback {
		t.Fatal("feedback not turned on")
	}
	id, err := db.CreateArtifact(ctx, &NewArtifact{UserID: "u", Feedback: true}, &NewVersion{Kind: "html"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := db.ArtifactByID(ctx, id); !b.Feedback {
		t.Fatal("created with feedback on, stored off")
	}
}

func feedbackStore(t *testing.T) (*Store, string) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, err := createUser(context.Background(), db.DB, "alice", "", "", "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateArtifact(context.Background(), &NewArtifact{UserID: u.ID, Feedback: true}, &NewVersion{Kind: "html"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return db, id
}

func TestComments(t *testing.T) {
	ctx := context.Background()
	db, id := feedbackStore(t)
	other, err := db.CreateArtifact(ctx, &NewArtifact{UserID: mustArtifact(t, db, id).UserID}, &NewVersion{Kind: "html"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	first := &Comment{ArtifactID: id, Version: 1, Page: "/", Body: "one", QuoteExact: "x", QuotePrefix: "a", QuoteSuffix: "b"}
	if err := db.AddComment(ctx, first, 10); err != nil {
		t.Fatal(err)
	}
	second := &Comment{ArtifactID: id, Version: 2, Page: "/b.html#c", Body: "two", Name: "Sam"}
	if err := db.AddComment(ctx, second, 10); err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID == second.ID || first.CreatedAt.IsZero() {
		t.Fatalf("ids/times not set: %+v %+v", first, second)
	}

	all, err := db.ListComments(ctx, id, CommentFilter{})
	if err != nil || len(all) != 2 || all[0].ID != first.ID || all[1].ID != second.ID {
		t.Fatalf("list oldest first: %+v %v", all, err)
	}
	if all[0].QuoteExact != "x" || all[0].QuotePrefix != "a" || all[0].QuoteSuffix != "b" || all[1].Name != "Sam" || all[1].Page != "/b.html#c" {
		t.Fatalf("fields round trip: %+v", all)
	}
	if v2, _ := db.ListComments(ctx, id, CommentFilter{Version: 2}); len(v2) != 1 || v2[0].ID != second.ID {
		t.Fatalf("version filter: %+v", v2)
	}

	// Resolve, resolve again (keeps the time), reopen.
	c, err := db.SetCommentResolved(ctx, id, first.ID, true)
	if err != nil || c.ResolvedAt == nil {
		t.Fatalf("resolve: %+v %v", c, err)
	}
	if again, _ := db.SetCommentResolved(ctx, id, first.ID, true); again.ResolvedAt == nil || !again.ResolvedAt.Equal(*c.ResolvedAt) {
		t.Fatalf("resolving twice changed the time: %v", again.ResolvedAt)
	}
	if open, _ := db.ListComments(ctx, id, CommentFilter{OpenOnly: true}); len(open) != 1 || open[0].ID != second.ID {
		t.Fatalf("open only: %+v", open)
	}
	if c, _ = db.SetCommentResolved(ctx, id, first.ID, false); c.ResolvedAt != nil {
		t.Fatal("reopen kept the resolved time")
	}

	// A comment is reached only through its own artifact.
	if _, err := db.SetCommentResolved(ctx, other, first.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve through another artifact: %v", err)
	}
	if err := db.DeleteComment(ctx, other, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete through another artifact: %v", err)
	}
	if err := db.DeleteComment(ctx, id, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteComment(ctx, id, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}

	// Deleting the artifact deletes its comments.
	if err := db.DeleteArtifact(ctx, id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM feedback`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("comments left after the artifact was deleted: %d %v", n, err)
	}
}

func TestCommentCap(t *testing.T) {
	ctx := context.Background()
	db, id := feedbackStore(t)
	for i := range 3 {
		if err := db.AddComment(ctx, &Comment{ArtifactID: id, Version: 1, Page: "/", Body: fmt.Sprint(i)}, 3); err != nil {
			t.Fatal(err)
		}
	}
	// Resolved comments count too.
	list, _ := db.ListComments(ctx, id, CommentFilter{})
	if _, err := db.SetCommentResolved(ctx, id, list[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.AddComment(ctx, &Comment{ArtifactID: id, Version: 1, Page: "/", Body: "over"}, 3); !errors.Is(err, ErrFeedbackFull) {
		t.Fatalf("fourth comment: %v", err)
	}
	if err := db.DeleteComment(ctx, id, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AddComment(ctx, &Comment{ArtifactID: id, Version: 1, Page: "/", Body: "room again"}, 3); err != nil {
		t.Fatalf("after a delete: %v", err)
	}
}

func mustArtifact(t *testing.T, db *Store, id string) *Artifact {
	t.Helper()
	a, err := db.ArtifactByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
