package brand

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrRedSun/redrop/internal/brand/iconsrc"
)

// TestIconsAreGenerated fails when the committed icons differ from the
// generator output; run `go generate ./internal/brand` to refresh them.
func TestIconsAreGenerated(t *testing.T) {
	want := iconsrc.Files()
	if len(want) != len(icons) {
		t.Fatalf("generator makes %d files, %d are embedded", len(want), len(icons))
	}
	for name, data := range want {
		if !bytes.Equal(icons[name].data, data) {
			t.Errorf("icons/%s is stale; run go generate ./internal/brand", name)
		}
	}
}

func TestICOHasThreePNGSizes(t *testing.T) {
	b := icons["favicon.ico"].data
	le := binary.LittleEndian
	if le.Uint16(b[0:]) != 0 || le.Uint16(b[2:]) != 1 || le.Uint16(b[4:]) != 3 {
		t.Fatalf("bad ICONDIR % x", b[:6])
	}
	for i, want := range []int{16, 32, 48} {
		e := b[6+16*i:]
		size, off := le.Uint32(e[8:]), le.Uint32(e[12:])
		img, err := png.Decode(bytes.NewReader(b[off : off+size]))
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if int(e[0]) != want || img.Bounds().Dx() != want || img.Bounds().Dy() != want {
			t.Errorf("entry %d: header %d, image %v, want %d", i, e[0], img.Bounds(), want)
		}
	}
}

func TestAppleTouchIconIsOpaque180(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(icons["apple-touch-icon.png"].data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 180 || b.Dy() != 180 {
		t.Fatalf("size %v", b)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0xffff {
		t.Error("corner is transparent; iOS would render it black")
	}
}

func TestHandler(t *testing.T) {
	for _, c := range []struct{ path, ct string }{
		{"/favicon.svg", "image/svg+xml"}, {"/favicon.ico", "image/x-icon"}, {"/apple-touch-icon.png", "image/png"},
	} {
		w := httptest.NewRecorder()
		Handler(w, httptest.NewRequest("GET", c.path, nil))
		if w.Code != 200 || w.Header().Get("Content-Type") != c.ct || w.Header().Get("Cache-Control") != CacheShared || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q %q", c.path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
		}
		// Revalidation is answered without a body.
		r := httptest.NewRequest("GET", c.path, nil)
		r.Header.Set("If-None-Match", w.Header().Get("ETag"))
		w = httptest.NewRecorder()
		Handler(w, r)
		if w.Code != http.StatusNotModified {
			t.Errorf("%s revalidation: %d", c.path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	Handler(w, httptest.NewRequest("GET", "/other.png", nil))
	if w.Code != 404 {
		t.Errorf("unknown icon: %d", w.Code)
	}
}
