package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const quotaMB int64 = 1_000_000

func quotaStore(t *testing.T, quota int64) (*Store, string) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.DB.Exec(`INSERT INTO users (id, username, quota_bytes, created_at) VALUES ('owner', 'owner', ?, 1)`, quota); err != nil {
		t.Fatal(err)
	}
	return db, "owner"
}

func quotaUpload(t *testing.T, db *Store, user string, size int64) *Upload {
	t.Helper()
	u, err := db.CreateUpload(context.Background(), user, size, time.Hour, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func quotaFile(uploadID string, size int64) *NewVersion {
	return &NewVersion{UploadID: uploadID, Kind: "file", Entry: "six-mb.bin", Files: []File{{Path: "six-mb.bin", Blob: "test-blob", Size: size, ContentType: "application/octet-stream"}}}
}

func TestMaterializationQuotaUsesMaxPerUpload(t *testing.T) {
	ctx := context.Background()
	db, user := quotaStore(t, 10*quotaMB)
	u := quotaUpload(t, db, user, 6*quotaMB)
	for _, size := range []int64{3 * quotaMB, 3 * quotaMB, 4 * quotaMB} {
		if err := db.ReserveMaterialization(ctx, user, u.ID, size, 100*quotaMB); err != nil {
			t.Fatalf("reserve %d with 6 MB staging and a 10 MB user quota: %v", size, err)
		}
	}
	if err := db.ReserveMaterialization(ctx, user, u.ID, 1, 100*quotaMB); !errors.Is(err, ErrQuota) {
		t.Fatalf("materialization beyond 10 MB: %v, want ErrQuota", err)
	}
	got, err := db.UploadByID(ctx, user, u.ID)
	if err != nil || got.Materialized != 10*quotaMB || got.Size != 6*quotaMB {
		t.Fatalf("rejected reservation changed upload: %+v, %v", got, err)
	}
}

func TestSixMBFileCommitsWithinTenMBQuota(t *testing.T) {
	for _, operation := range []string{"create", "add-version"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			db, user := quotaStore(t, 10*quotaMB)
			var artifactID string
			if operation == "add-version" {
				var err error
				artifactID, err = db.CreateArtifact(ctx, &NewArtifact{UserID: user}, &NewVersion{Kind: "file"}, 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			stats, err := db.Stats(ctx)
			if err != nil || stats.LogicalBytes != 0 {
				t.Fatalf("initial logical usage = %d, %v", stats.LogicalBytes, err)
			}
			u := quotaUpload(t, db, user, 6*quotaMB)
			pending := quotaUpload(t, db, user, 2*quotaMB)
			// Seed the already-materialized state to test commit independently of reservation.
			if _, err := db.DB.Exec(`UPDATE uploads SET materialized = CASE id WHEN ? THEN ? ELSE ? END`, u.ID, 6*quotaMB, 4*quotaMB); err != nil {
				t.Fatal(err)
			}
			v := quotaFile(u.ID, 6*quotaMB)
			if operation == "create" {
				_, err = db.CreateArtifact(ctx, &NewArtifact{UserID: user}, v, 18*quotaMB)
			} else {
				_, err = db.AddVersion(ctx, artifactID, user, v, 18*quotaMB, 0)
			}
			if err != nil {
				t.Fatalf("commit 6 MB file with logical usage 0, 4 MB other pending and 10 MB quota: %v", err)
			}
			got, err := db.UploadByID(ctx, user, u.ID)
			if err != nil || got.Size != 6*quotaMB || got.Materialized != 0 {
				t.Fatalf("commit lost physical staging reservation: %+v, %v", got, err)
			}
			other, err := db.UploadByID(ctx, user, pending.ID)
			if err != nil || other.Size != 2*quotaMB || other.Materialized != 4*quotaMB {
				t.Fatalf("commit changed another pending upload: %+v, %v", other, err)
			}
			stats, err = db.Stats(ctx)
			if err != nil || stats.LogicalBytes != 6*quotaMB || stats.ReservedBytes != 12*quotaMB {
				t.Fatalf("commit accounting: %+v, %v", stats, err)
			}
			if err := db.DeleteUpload(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.CreateUpload(ctx, user, 1, time.Hour, 18*quotaMB, 20); !errors.Is(err, ErrQuota) {
				t.Fatalf("other upload's max(size, materialized) was not retained: %v", err)
			}
		})
	}
}

func TestCommitQuotaRetainsOtherPendingUploads(t *testing.T) {
	for _, operation := range []string{"create", "add-version"} {
		for _, cap := range []string{"user", "server"} {
			t.Run(operation+"/"+cap, func(t *testing.T) {
				ctx := context.Background()
				db, user := quotaStore(t, 100*quotaMB)
				var artifactID string
				if operation == "add-version" {
					var err error
					artifactID, err = db.CreateArtifact(ctx, &NewArtifact{UserID: user}, &NewVersion{Kind: "file"}, 0)
					if err != nil {
						t.Fatal(err)
					}
				}
				u := quotaUpload(t, db, user, 6*quotaMB)
				other := quotaUpload(t, db, user, 5*quotaMB)
				if _, err := db.DB.Exec(`UPDATE uploads SET materialized = ? WHERE id = ?`, 6*quotaMB, u.ID); err != nil {
					t.Fatal(err)
				}
				total := 16 * quotaMB // 6 MB staging + 6 MB committed + 5 MB other upload exceeds this.
				if cap == "user" {
					total = 100 * quotaMB
					if _, err := db.DB.Exec(`UPDATE users SET quota_bytes = ? WHERE id = ?`, 10*quotaMB, user); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if operation == "create" {
					_, err = db.CreateArtifact(ctx, &NewArtifact{UserID: user}, quotaFile(u.ID, 6*quotaMB), total)
				} else {
					_, err = db.AddVersion(ctx, artifactID, user, quotaFile(u.ID, 6*quotaMB), total, 0)
				}
				if !errors.Is(err, ErrQuota) {
					t.Fatalf("commit ignoring another pending upload: %v, want ErrQuota", err)
				}
				got, err := db.UploadByID(ctx, user, u.ID)
				if err != nil || got.Materialized != 6*quotaMB {
					t.Fatalf("failed commit released materialization: %+v, %v", got, err)
				}
				got, err = db.UploadByID(ctx, user, other.ID)
				if err != nil || got.Size != 5*quotaMB || got.Materialized != 0 {
					t.Fatalf("failed commit changed another upload: %+v, %v", got, err)
				}
				stats, err := db.Stats(ctx)
				if err != nil || stats.LogicalBytes != 0 || stats.ReservedBytes != 17*quotaMB {
					t.Fatalf("failed commit changed accounting: %+v, %v", stats, err)
				}
			})
		}
	}
}

func TestServerQuotaCountsStagingAndMaterialization(t *testing.T) {
	ctx := context.Background()
	db, user := quotaStore(t, 100*quotaMB)
	u := quotaUpload(t, db, user, 6*quotaMB)
	if err := db.ReserveMaterialization(ctx, user, u.ID, 6*quotaMB, 10*quotaMB); !errors.Is(err, ErrQuota) {
		t.Fatalf("physical 12 MB into server cap 10 MB: %v, want ErrQuota", err)
	}
	if err := db.ReserveMaterialization(ctx, user, u.ID, 6*quotaMB, 12*quotaMB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateArtifact(ctx, &NewArtifact{UserID: user}, quotaFile(u.ID, 6*quotaMB), 12*quotaMB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUpload(ctx, user, 1, time.Hour, 12*quotaMB, 20); !errors.Is(err, ErrQuota) {
		t.Fatalf("commit released staging before deletion: %v", err)
	}
	if err := db.DeleteUpload(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUpload(ctx, user, 6*quotaMB, time.Hour, 12*quotaMB, 20); err != nil {
		t.Fatalf("deleted staging still charged: %v", err)
	}
}

func TestQuotaCapsSerializeConcurrentReservations(t *testing.T) {
	for _, operation := range []string{"upload", "materialization"} {
		for _, cap := range []string{"user", "server"} {
			t.Run(operation+"/"+cap, func(t *testing.T) {
				quota, total := int64(100), int64(1000)
				if cap == "server" {
					quota, total = 1000, 100
				}
				db, user := quotaStore(t, quota)
				var uploads []*Upload
				if operation == "materialization" {
					for range 8 {
						uploads = append(uploads, quotaUpload(t, db, user, 0))
					}
				}
				start := make(chan struct{})
				results := make(chan error, 8)
				var wg sync.WaitGroup
				for i := range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						ctx := context.Background()
						if operation == "materialization" {
							results <- db.ReserveMaterialization(ctx, user, uploads[i].ID, 60, total)
						} else {
							_, err := db.CreateUpload(ctx, user, 60, time.Hour, total, 20)
							results <- err
						}
					}()
				}
				close(start)
				wg.Wait()
				close(results)
				accepted := 0
				for err := range results {
					if err == nil {
						accepted++
					} else if !errors.Is(err, ErrQuota) {
						t.Errorf("unexpected reservation error: %v", err)
					}
				}
				if accepted != 1 {
					t.Fatalf("accepted %d concurrent 60-byte reservations into 100-byte cap, want 1", accepted)
				}
			})
		}
	}
}
