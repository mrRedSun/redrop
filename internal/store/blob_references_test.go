package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestReferencedBlobsAmongBatchesAndFilters(t *testing.T) {
	db, user := quotaStore(t, 0)
	ctx := context.Background()
	// Exceed SQLite's variable limit, so an unbounded IN query cannot pass.
	hashes := make([]string, 33_001)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("%064x", i)
	}
	selected := []string{hashes[0], hashes[500], hashes[len(hashes)-1], "' OR 1=1 --"}
	hashes = append(hashes, selected[0], selected[1], selected[3])
	v := &NewVersion{Kind: "bundle"}
	for i, hash := range append(append([]string{}, selected...), selected[0], "unrequested") {
		v.Files = append(v.Files, File{Path: fmt.Sprintf("file-%d", i), Blob: hash, Size: 1, ContentType: "text/plain"})
	}
	if _, err := db.CreateArtifact(ctx, &NewArtifact{UserID: user}, v, 0); err != nil {
		t.Fatal(err)
	}
	got, err := db.ReferencedBlobsAmong(ctx, hashes)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]struct{})
	for _, hash := range selected {
		want[hash] = struct{}{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("referenced subset = %v, want %v", got, want)
	}
	if _, err := db.DB.Exec(`DELETE FROM files WHERE blob = ?`, selected[1]); err != nil {
		t.Fatal(err)
	}
	got, err = db.ReferencedBlobsAmong(ctx, []string{selected[1], "absent"})
	if err != nil || len(got) != 0 {
		t.Fatalf("deleted or absent references returned: %v, %v", got, err)
	}
}

func TestReferencedBlobsAmongHandlesEmptyInputAndErrors(t *testing.T) {
	db, _ := quotaStore(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := db.ReferencedBlobsAmong(ctx, []string{"hash"}); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("cancelled lookup = %v, %v, want nil, context.Canceled", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, hashes := range [][]string{nil, {}} {
		got, err := db.ReferencedBlobsAmong(context.Background(), hashes)
		if err != nil || len(got) != 0 {
			t.Fatalf("empty lookup queried closed database: %v, %v", got, err)
		}
	}
	if got, err := db.ReferencedBlobsAmong(context.Background(), []string{"hash"}); err == nil || got != nil {
		t.Fatalf("closed database = %v, %v, want nil, error", got, err)
	}
}
