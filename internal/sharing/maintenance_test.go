package sharing

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepReleasesSafeUploadsWhenBlobGCFails(t *testing.T) {
	s, actor := budgetService(t, 10000, 10000)
	ctx := context.Background()
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	newUpload := func() string {
		t.Helper()
		u, err := s.CreateUpload(ctx, actor, 100, UploadTarget{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendChunk(ctx, actor, u.ID, 0, strings.NewReader("data")); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	safe, materialized, active, fresh := newUpload(), newUpload(), newUpload(), newUpload()
	if err := s.db.ReserveMaterialization(ctx, actor.UserID, materialized, 4, 10000); err != nil {
		t.Fatal(err)
	}
	hash, _, err := s.blobs.Put(strings.NewReader("blob"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.DB.Exec(`UPDATE uploads SET expires_at = 1 WHERE id IN (?, ?, ?)`, safe, materialized, active); err != nil {
		t.Fatal(err)
	}
	if !s.claimUpload(active) {
		t.Fatal("failed to claim active expired upload")
	}
	defer s.releaseUpload(active)
	if _, err := s.db.DB.Exec(`INSERT INTO sessions (hash, user_id, method, created_at, last_seen_at, expires_at) VALUES ('expired', ?, 'password', 1, 1, 1)`, actor.UserID); err != nil {
		t.Fatal(err)
	}
	// Missing storage fails under root too, without changing filesystem permissions.
	root := filepath.Join(s.cfg.DataDir, "blobs")
	offline := filepath.Join(s.cfg.DataDir, "blobs-offline")
	if err := os.Rename(root, offline); err != nil {
		t.Fatal(err)
	}
	s.Sweep(ctx)
	if !strings.Contains(logs.String(), "expired upload blobs") {
		t.Errorf("GC failure was not logged: %s", logs.String())
	}
	if s.db.UploadExists(ctx, safe) {
		t.Error("GC failure retained expired upload with materialized=0")
	}
	if _, err := os.Stat(s.stagedPath(safe)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("safe expired staging not removed: %v", err)
	}
	for _, id := range []string{materialized, active, fresh} {
		if !s.db.UploadExists(ctx, id) {
			t.Errorf("GC failure released protected upload %s", id)
		}
		if _, err := os.Stat(s.stagedPath(id)); err != nil {
			t.Errorf("protected staging %s lost: %v", id, err)
		}
	}
	var reserved int64
	if err := s.db.DB.QueryRow(`SELECT materialized FROM uploads WHERE id = ?`, materialized).Scan(&reserved); err != nil || reserved != 4 {
		t.Errorf("GC failure released materialized reservation: %d, %v", reserved, err)
	}
	var sessions int
	if err := s.db.DB.QueryRow(`SELECT count(*) FROM sessions WHERE hash = 'expired'`).Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("GC failure stalled session cleanup: %d, %v", sessions, err)
	}
	if err := os.Rename(offline, root); err != nil {
		t.Fatal(err)
	}
	s.releaseUpload(active)
	s.Sweep(ctx)
	for _, id := range []string{safe, materialized, active} {
		if s.db.UploadExists(ctx, id) {
			t.Errorf("recovered GC retained expired upload %s", id)
		}
	}
	if f, err := s.blobs.Open(hash); err == nil {
		f.Close()
		t.Error("recovered GC retained orphan blob")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	stats, err := s.db.Stats(ctx)
	if err != nil || stats.ReservedBytes != 100 || stats.StagedBytes != 4 {
		t.Fatalf("recovered sweep changed fresh upload: %+v, %v", stats, err)
	}
}

func TestSweepReleasesSafeUploadsWhenReferenceLookupFails(t *testing.T) {
	s, actor := budgetService(t, 10000, 10000)
	ctx := context.Background()
	u, err := s.CreateUpload(ctx, actor, 100, UploadTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.DB.Exec(`UPDATE uploads SET expires_at = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.DB.Exec(`ALTER TABLE files RENAME TO offline_files`); err != nil {
		t.Fatal(err)
	}
	s.Sweep(ctx)
	if s.db.UploadExists(ctx, u.ID) {
		t.Fatal("reference lookup failure retained zero-materialized expired upload")
	}
}

func TestSweepRetainsUploadWhenStagingRemovalFails(t *testing.T) {
	s, actor := budgetService(t, 10000, 10000)
	ctx := context.Background()
	u, err := s.CreateUpload(ctx, actor, 100, UploadTarget{})
	if err != nil {
		t.Fatal(err)
	}
	path := s.stagedPath(u.ID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// A nonempty directory cannot be removed with os.Remove, even as root.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "retained"), []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.DB.Exec(`UPDATE uploads SET expires_at = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	s.Sweep(ctx)
	if !s.db.UploadExists(ctx, u.ID) {
		t.Fatal("failed staging removal released upload reservation")
	}
	stats, err := s.db.Stats(ctx)
	if err != nil || stats.ReservedBytes != 100 {
		t.Fatalf("failed staging removal changed reservation: %+v, %v", stats, err)
	}
}
