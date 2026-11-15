package sharing

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
)

type cancelArchiveReader struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (r cancelArchiveReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.cancel()
	return n, err
}

type heldChunkReader struct {
	started, release chan struct{}
	once             sync.Once
	r                io.Reader
}

func (r *heldChunkReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started); <-r.release })
	return r.r.Read(p)
}

func TestUploadExpiryWaitsForActiveChunk(t *testing.T) {
	s, a := budgetService(t, 100, 100)
	ctx := context.Background()
	up, err := s.CreateUpload(ctx, a, 100)
	if err != nil {
		t.Fatal(err)
	}
	r := &heldChunkReader{started: make(chan struct{}), release: make(chan struct{}), r: strings.NewReader("x")}
	var once sync.Once
	release := func() { once.Do(func() { close(r.release) }) }
	defer release()
	done := make(chan error, 1)
	go func() { _, err := s.AppendChunk(ctx, a, up.ID, 0, r); done <- err }()
	select {
	case <-r.started:
	case <-time.After(30 * time.Second):
		t.Fatal("chunk did not begin")
	}
	if _, err := s.AppendChunk(ctx, a, up.ID, 0, strings.NewReader("y")); !apperr.Is(err, apperr.KindConflict) {
		t.Errorf("concurrent chunk was not refused: %v", err)
	}
	if _, err := s.db.DB.Exec(`UPDATE uploads SET expires_at=1 WHERE id=?`, up.ID); err != nil {
		t.Fatal(err)
	}
	s.Sweep(ctx)
	if !s.db.UploadExists(ctx, up.ID) {
		t.Error("expiry released disk budget while a chunk still held its file open")
	}
	if _, err := s.CreateUpload(ctx, a, 1); err == nil {
		t.Error("active upload reservation was released")
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("slow chunk could not resume: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("chunk did not finish")
	}
	s.Sweep(ctx)
	if !s.db.UploadExists(ctx, up.ID) {
		t.Fatal("renewed upload was discarded")
	}
}

func TestArchiveCancellation(t *testing.T) {
	s, a := budgetService(t, 4<<20, 8<<20)
	packed := budgetArchive(t, []tar.Header{{Name: "index.html", Size: 4, Mode: 0600}}, [][]byte{[]byte("test")})
	for _, mid := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var r io.Reader = bytes.NewReader(packed)
		if mid {
			r = cancelArchiveReader{r: r, cancel: cancel}
		} else {
			cancel()
		}
		_, err := s.extractBundle(r, &publication{s: s, ctx: ctx, actor: a, created: make(map[string]struct{})})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("midstream=%v: cancellation lost: %v", mid, err)
		}
	}
}

func TestArchiveByteBudgetCountsMetadataAndSkippedBodies(t *testing.T) {
	packed := budgetArchive(t, []tar.Header{{Name: "pax", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("x", 8192)}}}, nil)
	gz, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	work := &archiveWorkReader{r: gz, remaining: 2048, maxEntries: 100}
	tr := tar.NewReader(work)
	if _, err := tr.Next(); !apperr.Is(err, apperr.KindTooLarge) {
		t.Fatalf("metadata byte limit: %v", err)
	}
	if work.remaining != 0 {
		t.Fatalf("work budget did not stop at limit: %d", work.remaining)
	}
	// A caller discarding bodies cannot escape the same raw-stream limit.
	packed = budgetArchive(t, []tar.Header{{Name: "large", Size: 8192, Mode: 0600}}, [][]byte{bytes.Repeat([]byte{'x'}, 8192)})
	gz2, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	defer gz2.Close()
	work = &archiveWorkReader{r: gz2, remaining: 2048, maxEntries: 100}
	tr = tar.NewReader(work)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Next(); !apperr.Is(err, apperr.KindTooLarge) {
		t.Fatalf("discarded body byte limit: %v", err)
	}
}

func TestArchiveFramingCannotHideEntries(t *testing.T) {
	for _, pax := range []bool{false, true} {
		t.Run(fmt.Sprintf("pax=%v", pax), func(t *testing.T) {
			s, a := budgetService(t, 4<<20, 8<<20)
			var headers []tar.Header
			if pax {
				headers = append(headers, tar.Header{Name: "pax", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"size": "0"}}, tar.Header{Name: "empty", Mode: 0600})
			} else {
				headers = append(headers, tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0700})
			}
			for i := range 200 {
				headers = append(headers, tar.Header{Name: fmt.Sprintf("d%d/", i), Typeflag: tar.TypeDir, Mode: 0700})
			}
			headers = append(headers, tar.Header{Name: "index.html", Mode: 0600})
			packed := budgetArchive(t, headers, nil)
			gz, err := gzip.NewReader(bytes.NewReader(packed))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(gz)
			gz.Close()
			if err != nil {
				t.Fatal(err)
			}
			checksum := func(block []byte) {
				for i := 148; i < 156; i++ {
					block[i] = ' '
				}
				sum := 0
				for _, b := range block {
					sum += int(b)
				}
				copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
			}
			offset := 0
			if pax {
				raw[156] = tar.TypeXHeader
				checksum(raw[:512])
				offset = 1024
			}
			copy(raw[offset+124:offset+136], fmt.Sprintf("%011o\x00", 1<<20))
			checksum(raw[offset : offset+512])
			var out bytes.Buffer
			zw := gzip.NewWriter(&out)
			zw.Write(raw)
			zw.Close()
			c := budgetContent(t, s, a, out.Bytes())
			if _, err := s.Create(context.Background(), a, c, Settings{}); err == nil {
				t.Fatal("forged physical size hid archive entries")
			}
		})
	}
}

func TestArchivePhysicalSizes(t *testing.T) {
	for _, tc := range []struct {
		value []byte
		want  int64
		ok    bool
	}{
		{[]byte("00000001000\x00"), 512, true},
		{[]byte("            "), 0, true},
		{[]byte("-0000000001\x00"), -1, false},
		{[]byte{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0}, 512, true},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 0, false},
	} {
		n, ok := tarPhysicalSize(tc.value)
		if ok != tc.ok || ok && n != tc.want {
			t.Errorf("%x: %d,%v want %d,%v", tc.value, n, ok, tc.want, tc.ok)
		}
	}
}

func TestArchiveReplayCannotReleaseFailedCleanupBudget(t *testing.T) {
	s, a := budgetService(t, 4<<20, 8<<20)
	ctx := context.Background()
	c := budgetContent(t, s, a, budgetArchive(t, []tar.Header{{Name: "index.html", Size: 4, Mode: 0600}}, [][]byte{[]byte("test")}))
	if _, err := s.db.DB.Exec(`UPDATE uploads SET materialized=4 WHERE id=?`, c.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, a, c, Settings{}); !apperr.Is(err, apperr.KindConflict) {
		t.Fatalf("retained failed-cleanup reservation replay: %v", err)
	}
	up, err := s.db.UploadByID(ctx, a.UserID, c.UploadID)
	if err != nil || up.Materialized != 4 {
		t.Fatalf("replay released unreclaimed budget: %+v %v", up, err)
	}
}

func TestArchiveWorkBudget(t *testing.T) {
	for _, name := range []string{"duplicate", "directories", "metadata", "replay"} {
		t.Run(name, func(t *testing.T) {
			s, a := budgetService(t, 4<<20, 8<<20)
			headers := []tar.Header{{Name: "same", Size: 1, Mode: 0600}}
			bodies := [][]byte{[]byte("x")}
			want := "entries"
			switch name {
			case "duplicate", "replay":
				headers = append(headers, tar.Header{Name: "./same", Size: 1, Mode: 0600})
				bodies = append(bodies, []byte("y"))
				want = "duplicate"
				if name == "replay" {
					headers = append(headers, tar.Header{Name: "invalid", Typeflag: tar.TypeSymlink, Linkname: "outside"})
					bodies = append(bodies, nil)
				}
			case "directories":
				for i := range 200 {
					headers = append(headers, tar.Header{Name: strings.Repeat("d", i+1) + "/", Typeflag: tar.TypeDir, Mode: 0700})
					bodies = append(bodies, nil)
				}
			case "metadata":
				want = "metadata headers"
				for range 200 {
					headers = append(headers, tar.Header{Name: "pax", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "bounded metadata"}})
					bodies = append(bodies, nil)
				}
			}
			c := budgetContent(t, s, a, budgetArchive(t, headers, bodies))
			attempts := 1
			if name == "replay" {
				attempts = 2
			}
			for i := range attempts {
				_, err := s.Create(context.Background(), a, c, Settings{})
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("attempt %d: %v, want %s rejection", i+1, err, want)
				}
			}
		})
	}
}
