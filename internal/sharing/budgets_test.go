package sharing

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/store"
)

func budgetService(t *testing.T, quota, total int64) (*Service, Actor) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.DB.Exec(`INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES ('u','publisher','','','user',?,1)`, quota)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("https://redrop.example.com")
	content, _ := url.Parse("https://usercontent.example")
	s, err := New(db, bs, &config.Config{DataDir: dir, PublicURL: public, ContentURL: content, MaxVersionBytes: 1 << 20, MaxTotalBytes: total, MaxFiles: 20, ChunkSize: 1 << 20}, make([]byte, 32), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, Actor{UserID: "u", Quota: quota}
}
func budgetArchive(t *testing.T, headers []tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for i, h := range headers {
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if i < len(bodies) {
			if _, err := tw.Write(bodies[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func budgetContent(t *testing.T, s *Service, a Actor, b []byte) Content {
	t.Helper()
	ctx := context.Background()
	u, err := s.CreateUpload(ctx, a, int64(len(b)), UploadTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendChunk(ctx, a, u.ID, 0, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return Content{UploadID: u.ID, SHA256: hex.EncodeToString(sum[:]), Kind: "bundle"}
}
func TestUploadIdleExpiryAndStats(t *testing.T) {
	s, a := budgetService(t, 10000, 10000)
	ctx := context.Background()
	u, err := s.CreateUpload(ctx, a, 100, UploadTarget{})
	if err != nil {
		t.Fatal(err)
	}
	var lifetime int64
	if err := s.db.DB.QueryRow(`SELECT expires_at-created_at FROM uploads WHERE id=?`, u.ID).Scan(&lifetime); err != nil {
		t.Fatal(err)
	}
	if lifetime != 3600 {
		t.Errorf("idle lifetime = %d seconds, want 3600", lifetime)
	}
	if _, err := s.db.DB.Exec(`UPDATE uploads SET expires_at=created_at+300 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendChunk(ctx, a, u.ID, 0, bytes.NewReader([]byte("test"))); err != nil {
		t.Fatal(err)
	}
	if err := s.db.DB.QueryRow(`SELECT expires_at-created_at FROM uploads WHERE id=?`, u.ID).Scan(&lifetime); err != nil {
		t.Fatal(err)
	}
	if lifetime < 3600 {
		t.Errorf("successful chunk did not renew idle expiry: %d", lifetime)
	}
	stats, err := s.Moderation().Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(stats)
	var fields map[string]any
	json.Unmarshal(b, &fields)
	if fields["StagedBytes"] != float64(4) || fields["ReservedBytes"] != float64(100) {
		t.Errorf("stats omit staging: %s", b)
	}
	if _, err := s.db.DB.Exec(`UPDATE uploads SET expires_at=1 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	s.Sweep(ctx)
	if s.db.UploadExists(ctx, u.ID) {
		t.Error("expired upload reservation retained")
	}
	if _, err := s.CreateUpload(ctx, a, 10000, UploadTarget{}); err != nil {
		t.Fatalf("expired capacity not released: %v", err)
	}
}

func TestUploadSessionCapAndRelease(t *testing.T) {
	s, a := budgetService(t, 10000, 10000)
	s.cfg.MaxOpenUploads = 2
	ctx := context.Background()
	u, err := s.CreateUpload(ctx, a, 0, UploadTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUpload(ctx, a, 0, UploadTarget{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUpload(ctx, a, 0, UploadTarget{}); err == nil {
		t.Fatal("zero-byte uploads bypassed session cap")
	}
	s.discardUpload(ctx, u.ID)
	if _, err := s.CreateUpload(ctx, a, 0, UploadTarget{}); err != nil {
		t.Fatalf("abort failed to release capacity: %v", err)
	}
}

func TestPublicationKeepsDeduplicatedBlob(t *testing.T) {
	s, a := budgetService(t, 2<<20, 4<<20)
	ctx := context.Background()
	b := []byte("shared data")
	packed := budgetArchive(t, []tar.Header{{Name: "index.html", Size: int64(len(b)), Mode: 0600}}, [][]byte{b})
	good := budgetContent(t, s, a, packed)
	if _, err := s.Create(ctx, a, good, Settings{}); err != nil {
		t.Fatal(err)
	}
	if s.db.UploadExists(ctx, good.UploadID) {
		t.Fatal("commit retained upload reservation")
	}
	bad := budgetContent(t, s, a, packed)
	bad.Entry = "missing"
	if _, err := s.Create(ctx, a, bad, Settings{}); err == nil {
		t.Fatal("missing entry accepted")
	}
	if n, err := s.blobs.Usage(); err != nil || n != int64(len(b)) {
		t.Fatalf("rollback damaged shared blob: bytes=%d, err=%v", n, err)
	}
}

func TestPublicationCommitFailureReclaimsBlob(t *testing.T) {
	s, a := budgetService(t, 10000, 10000)
	ctx := context.Background()
	c := budgetContent(t, s, a, budgetArchive(t, []tar.Header{{Name: "index.html", Size: 4, Mode: 0600}}, [][]byte{[]byte("test")}))
	if _, err := s.db.DB.Exec(`CREATE TRIGGER reject_version BEFORE INSERT ON versions BEGIN SELECT RAISE(FAIL, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, a, c, Settings{}); err == nil {
		t.Fatal("transaction failure ignored")
	}
	if n, err := s.blobs.Usage(); err != nil || n != 0 {
		t.Fatalf("commit failure retained %d bytes: %v", n, err)
	}
	var reserved int64
	if err := s.db.DB.QueryRow(`SELECT materialized FROM uploads WHERE id=?`, c.UploadID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("failed reservation retained: %d, %v", reserved, err)
	}
}

func TestPendingUploadBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		quota, total int64
	}{{"user", 100, 1000}, {"server", 1000, 100}} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := budgetService(t, tc.quota, tc.total)
			var wg sync.WaitGroup
			results := make(chan error, 8)
			start := make(chan struct{})
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := s.CreateUpload(context.Background(), a, 60, UploadTarget{})
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				}
			}
			if accepted != 1 {
				t.Fatalf("accepted %d concurrent 60-byte uploads into 100-byte budget, want 1", accepted)
			}
		})
	}
}
func TestFailedPublicationReclaimsBlobs(t *testing.T) {
	s, a := budgetService(t, 2<<20, 4<<20)
	for i := range 3 {
		b := bytes.Repeat([]byte{byte('a' + i)}, 1<<20)
		packed := budgetArchive(t, []tar.Header{{Name: "data.txt", Size: int64(len(b)), Mode: 0600}}, [][]byte{b})
		c := budgetContent(t, s, a, packed)
		c.Entry = "missing.html"
		if _, err := s.Create(context.Background(), a, c, Settings{}); err == nil {
			t.Fatal("missing entry accepted")
		}
		if n, err := s.blobs.Usage(); err != nil || n != 0 {
			t.Fatalf("failed publication retained %d blob bytes: %v", n, err)
		}
	}
}
func TestInflatedPublicationBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		quota, total int64
	}{{"user", 4096, 4 << 20}, {"server", 4 << 20, 4096}} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := budgetService(t, tc.quota, tc.total)
			b := bytes.Repeat([]byte{'x'}, 8192)
			c := budgetContent(t, s, a, budgetArchive(t, []tar.Header{{Name: "index.html", Size: int64(len(b)), Mode: 0600}}, [][]byte{b}))
			if _, err := s.Create(context.Background(), a, c, Settings{}); err == nil {
				t.Fatal("publication exceeded inflated budget")
			}
			if n, err := s.blobs.Usage(); err != nil || n != 0 {
				t.Fatalf("over-budget publication retained %d bytes: %v", n, err)
			}
		})
	}
}
