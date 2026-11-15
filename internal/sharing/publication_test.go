package sharing

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

// uploadContent stages b in chunks and returns publishable content of kind.
func uploadContent(t *testing.T, s *Service, a Actor, kind string, b []byte) Content {
	t.Helper()
	ctx := context.Background()
	u, err := s.CreateUpload(ctx, a, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	for off := int64(0); off < int64(len(b)); off += s.cfg.ChunkSize {
		end := min(off+s.cfg.ChunkSize, int64(len(b)))
		if _, err := s.AppendChunk(ctx, a, u.ID, off, bytes.NewReader(b[off:end])); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(b)
	return Content{UploadID: u.ID, SHA256: hex.EncodeToString(sum[:]), Kind: kind, Filename: "data.bin"}
}

func addUser(t *testing.T, s *Service, id string, quota int64) Actor {
	t.Helper()
	if _, err := s.db.DB.Exec(`INSERT INTO users (id, username, display_name, password_hash, role, quota_bytes, created_at) VALUES (?,?,'','','user',?,1)`, id, "user-"+id, quota); err != nil {
		t.Fatal(err)
	}
	return Actor{UserID: id, Quota: quota}
}

// assertReleased checks that an upload's row, reservation and staged file are gone.
func assertReleased(t *testing.T, s *Service, id string, released bool) {
	t.Helper()
	_, statErr := os.Stat(s.stagedPath(id))
	exists := s.db.UploadExists(context.Background(), id)
	if released && (exists || !errors.Is(statErr, fs.ErrNotExist)) {
		t.Errorf("upload %s kept after a deterministic rejection: row=%v staged=%v", id, exists, statErr)
	}
	if !released && (!exists || statErr != nil) {
		t.Errorf("upload %s released: row=%v staged=%v", id, exists, statErr)
	}
}

func TestPublicationOneTurnPerAccount(t *testing.T) {
	s, a := budgetService(t, 10000, 10000)
	b := addUser(t, s, "v", 10000)
	ctx := context.Background()
	upload := func(actor Actor) string {
		u, err := s.CreateUpload(ctx, actor, 0)
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	first, second, other := upload(a), upload(a), upload(b)
	running, err := s.beginPublication(ctx, a, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := running.admit(); err != nil {
		t.Fatal(err)
	}

	// The same account's second publication is refused at once, without
	// waiting for the slot.
	done := make(chan error, 1)
	go func() {
		p, err := s.beginPublication(ctx, a, second)
		if err == nil {
			p.finish(nil)
		}
		done <- err
	}()
	select {
	case err := <-done:
		var app *apperr.Error
		if !errors.As(err, &app) || app.Kind != apperr.KindConflict || app.Code != api.CodePublicationBusy {
			t.Fatalf("second publication by the same account: %v, want an immediate %s conflict", err, api.CodePublicationBusy)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second publication by the same account waited")
	}
	assertReleased(t, s, second, false) // a busy refusal keeps the upload for a retry

	// Another account may wait for the slot, but only until its context ends.
	queued, err := s.beginPublication(ctx, b, other)
	if err != nil {
		t.Fatalf("other account refused: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	queued.ctx = short
	if err := queued.admit(); apperr.KindOf(err) != apperr.KindInternal {
		t.Fatalf("admit while another publication runs: %v, want context error", err)
	}
	queued.finish(err)
	assertReleased(t, s, other, false)

	running.finish(nil)
	again, err := s.beginPublication(ctx, a, second)
	if err != nil {
		t.Fatalf("account turn not released: %v", err)
	}
	if err := again.admit(); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	again.finish(nil)
}

// truncatedBundle is a valid tar.gz cut inside its file body, so the error
// arrives while the body is read, after tar.Reader.Next succeeded.
func truncatedBundle(t *testing.T) []byte {
	t.Helper()
	body := make([]byte, 3000)
	if _, err := rand.Read(body); err != nil { // incompressible
		t.Fatal(err)
	}
	b := budgetArchive(t, []tar.Header{{Name: "index.html", Size: int64(len(body)), Mode: 0o600}}, [][]byte{body})
	return b[:len(b)-1000]
}

// corruptDeflateBundle is a gzip stream of two stored deflate blocks. The
// first holds the tar header and the start of the body; the second has a
// wrong NLEN, so decompression fails in the middle of the body.
func corruptDeflateBundle(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Size: 1000, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("x"), 1000)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tarBytes := raw.Bytes()
	out := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff} // gzip header
	block := func(final byte, data []byte, nlen uint16) {
		out = append(out, final, byte(len(data)), byte(len(data)>>8), byte(nlen), byte(nlen>>8))
		out = append(out, data...)
	}
	first, rest := tarBytes[:612], tarBytes[612:]
	block(0, first, ^uint16(len(first)))
	block(1, rest, ^uint16(len(rest))^1)
	return out
}

func TestRejectedPublicationReleasesUpload(t *testing.T) {
	site := func(t *testing.T) []byte {
		return budgetArchive(t, []tar.Header{{Name: "index.html", Size: 5, Mode: 0o600}}, [][]byte{[]byte("hello")})
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *Service, a Actor) (Content, error)
		kind apperr.Kind
		msg  string
	}{
		{"truncated body", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, truncatedBundle(t))
			_, err := s.Create(context.Background(), a, c, Settings{})
			return c, err
		}, apperr.KindInvalid, "corrupt bundle archive"},
		{"corrupt deflate block in body", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, corruptDeflateBundle(t))
			_, err := s.Create(context.Background(), a, c, Settings{})
			return c, err
		}, apperr.KindInvalid, "corrupt bundle archive"},
		{"missing entry", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, site(t))
			c.Entry = "missing.html"
			_, err := s.Create(context.Background(), a, c, Settings{})
			return c, err
		}, apperr.KindInvalid, ""},
		{"corrupt archive", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, []byte("not gzip"))
			_, err := s.Create(context.Background(), a, c, Settings{})
			return c, err
		}, apperr.KindInvalid, ""},
		{"checksum mismatch", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, site(t))
			c.SHA256 = hex.EncodeToString(make([]byte, 32))
			_, err := s.Create(context.Background(), a, c, Settings{})
			return c, err
		}, apperr.KindInvalid, ""},
		{"quota", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, budgetArchive(t, []tar.Header{{Name: "index.html", Size: 8192, Mode: 0o600}}, [][]byte{bytes.Repeat([]byte{'x'}, 8192)}))
			_, err := s.Create(context.Background(), a, c, Settings{})
			return c, err
		}, apperr.KindTooLarge, ""},
		{"invalid ttl", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, site(t))
			_, err := s.Create(context.Background(), a, c, Settings{TTL: "soon"})
			return c, err
		}, apperr.KindInvalid, ""},
		{"unknown artifact", func(t *testing.T, s *Service, a Actor) (Content, error) {
			c := budgetContent(t, s, a, site(t))
			_, err := s.AddVersion(context.Background(), a, "missing", c)
			return c, err
		}, apperr.KindNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := budgetService(t, 4096, 1<<20)
			c, err := tc.run(t, s, a)
			if apperr.KindOf(err) != tc.kind || !strings.Contains(fmt.Sprint(err), tc.msg) {
				t.Fatalf("error %v, want kind %d %q", err, tc.kind, tc.msg)
			}
			assertReleased(t, s, c.UploadID, true)
			stats, err := s.db.Stats(context.Background())
			if err != nil || stats.ReservedBytes != 0 || stats.StagedBytes != 0 {
				t.Fatalf("reservation kept: %+v, %v", stats, err)
			}
			if n, err := s.blobs.Usage(); err != nil || n != 0 {
				t.Fatalf("rejected publication kept %d blob bytes: %v", n, err)
			}
		})
	}
}

func TestPublicationKeepsUploadAfterCancellationOrConflict(t *testing.T) {
	ctx := context.Background()
	t.Run("incomplete", func(t *testing.T) {
		s, a := budgetService(t, 10000, 10000)
		u, err := s.CreateUpload(ctx, a, 10)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Create(ctx, a, Content{UploadID: u.ID, Kind: "html"}, Settings{})
		if apperr.KindOf(err) != apperr.KindConflict {
			t.Fatalf("incomplete upload: %v, want conflict", err)
		}
		assertReleased(t, s, u.ID, false)
	})
	t.Run("blob store failure", func(t *testing.T) {
		s, a := budgetService(t, 10000, 10000)
		c := budgetContent(t, s, a, budgetArchive(t, []tar.Header{{Name: "index.html", Size: 5, Mode: 0o600}}, [][]byte{[]byte("hello")}))
		// The blob store's temporary directory is gone: a server-side write
		// failure, not the archive's fault.
		if err := os.RemoveAll(s.blobs.TempDir()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(ctx, a, c, Settings{}); apperr.KindOf(err) != apperr.KindInternal {
			t.Fatalf("blob store failure: %v, want internal", err)
		}
		assertReleased(t, s, c.UploadID, false)
	})
	t.Run("cancelled while waiting", func(t *testing.T) {
		s, a := budgetService(t, 10000, 10000)
		c := uploadContent(t, s, a, "html", []byte("<p>hi"))
		s.publication <- struct{}{} // maintenance holds the slot
		defer func() { <-s.publication }()
		short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if _, err := s.Create(short, a, c, Settings{}); apperr.KindOf(err) != apperr.KindInternal {
			t.Fatalf("cancelled publication: %v, want internal", err)
		}
		assertReleased(t, s, c.UploadID, false)
	})
}

// Review 27 M3: eight mistyped entries used to hold all eight upload sessions.
func TestBadEntriesDoNotExhaustUploadSessions(t *testing.T) {
	s, a := budgetService(t, 10000, 10000)
	ctx := context.Background()
	packed := budgetArchive(t, []tar.Header{{Name: "index.html", Size: 5, Mode: 0o600}}, [][]byte{[]byte("hello")})
	for i := range 8 {
		c := budgetContent(t, s, a, packed)
		c.Entry = "missing.html"
		if _, err := s.Create(ctx, a, c, Settings{}); apperr.KindOf(err) != apperr.KindInvalid {
			t.Fatalf("bad entry %d: %v", i, err)
		}
	}
	if _, err := s.Create(ctx, a, budgetContent(t, s, a, packed), Settings{}); err != nil {
		t.Fatalf("valid share after 8 bad entries: %v", err)
	}
}

// Review 27 M2: the staged upload and its unpacked copy are the same content
// for the user's quota, so a 6 MB publish fits a 10 MB quota.
func TestSixMBPublishWithinTenMBQuota(t *testing.T) {
	data := make([]byte, 6_000_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"file", "html", "bundle"} {
		t.Run(kind, func(t *testing.T) {
			s, a := budgetService(t, 10_000_000, 0)
			s.cfg.MaxVersionBytes = 8 << 20
			s.cfg.ChunkSize = 4 << 20
			body := data
			if kind == "bundle" {
				body = budgetArchive(t, []tar.Header{{Name: "index.html", Size: int64(len(data)), Mode: 0o600}}, [][]byte{data})
			}
			art, err := s.Create(context.Background(), a, uploadContent(t, s, a, kind, body), Settings{})
			if err != nil {
				t.Fatalf("6 MB %s with 0 used of a 10 MB quota: %v", kind, err)
			}
			if art.Size != int64(len(data)) {
				t.Fatalf("published %d bytes", art.Size)
			}
		})
	}
}
