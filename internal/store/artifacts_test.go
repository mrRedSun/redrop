package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestSlugCollisionRetry forces the slug generator to repeat a taken slug and
// checks that creation and rotation retry with a fresh one, and give up
// with ErrSlugExhausted instead of looping forever.
func TestSlugCollisionRetry(t *testing.T) {
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
	version := func() *NewVersion {
		return &NewVersion{Kind: "html", Entry: "index.html", Files: []File{{Path: "index.html", Blob: "b", Size: 1, ContentType: "text/html"}}}
	}
	queue := func(slugs ...string) {
		db.newSlug = func() string {
			if len(slugs) == 0 {
				t.Fatal("slug generator called more often than expected")
			}
			s := slugs[0]
			slugs = slugs[1:]
			return s
		}
	}
	slugOf := func(id string) string {
		var s string
		if err := db.DB.QueryRowContext(ctx, `SELECT slug FROM artifacts WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	queue("amber-otter-quilt-river")
	first, err := db.CreateArtifact(ctx, &NewArtifact{UserID: u.ID}, version(), 0)
	if err != nil {
		t.Fatal(err)
	}

	queue("amber-otter-quilt-river", "amber-otter-quilt-river", "cedar-heron-maple-stone")
	second, err := db.CreateArtifact(ctx, &NewArtifact{UserID: u.ID}, version(), 0)
	if err != nil {
		t.Fatalf("create after collisions: %v", err)
	}
	if got := slugOf(second); got != "cedar-heron-maple-stone" {
		t.Fatalf("second slug %q", got)
	}

	title := "renamed"
	queue("amber-otter-quilt-river", "dune-fern-glade-lake")
	if err := db.UpdateArtifact(ctx, second, ArtifactUpdate{RotateSlug: true, Title: &title}); err != nil {
		t.Fatalf("rotate after collision: %v", err)
	}
	if got, _ := db.ArtifactBySlug(ctx, "dune-fern-glade-lake"); got == nil || got.ID != second || got.Title != "renamed" || got.AccessGen != 2 { // starts at 1; one bump, not one per attempt
		t.Fatalf("rotated artifact: %+v", got)
	}

	taken := make([]string, slugAttempts)
	for i := range taken {
		taken[i] = "amber-otter-quilt-river"
	}
	queue(taken...)
	if _, err := db.CreateArtifact(ctx, &NewArtifact{UserID: u.ID}, version(), 0); !errors.Is(err, ErrSlugExhausted) {
		t.Fatalf("create with only collisions: %v, want ErrSlugExhausted", err)
	}
	queue(taken...)
	if err := db.UpdateArtifact(ctx, second, ArtifactUpdate{RotateSlug: true}); !errors.Is(err, ErrSlugExhausted) {
		t.Fatalf("rotate with only collisions: %v, want ErrSlugExhausted", err)
	}
	if got := slugOf(second); got != "dune-fern-glade-lake" {
		t.Fatalf("failed rotation changed the slug to %q", got)
	}
	var n int
	db.DB.QueryRowContext(ctx, `SELECT count(*) FROM artifacts`).Scan(&n)
	if n != 2 || slugOf(first) != "amber-otter-quilt-river" {
		t.Fatalf("artifacts=%d first=%q", n, slugOf(first))
	}
}
