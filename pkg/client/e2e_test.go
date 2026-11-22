package client

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/mrRedSun/redrop/internal/e2e"
)

// Review 33 m-C: the upload holds the encrypted objects in a random order,
// so the stored rows and the blobs' write order don't show the source's
// path order. Every object still opens under its own path.
func TestEncryptedObjectsAreUploadedInRandomOrder(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := range 30 {
		p := fmt.Sprintf("dir%02d/file%02d.txt", i/10, i)
		paths = append(paths, p)
		write(t, dir, p, fmt.Sprintf("content %d", i))
	}
	write(t, dir, "index.html", "<title>Order</title>")
	paths = append(paths, "index.html")
	sort.Strings(paths)
	src := &Source{Path: dir}
	p, err := Prepare(src)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	key, err := NewE2EKey()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryptPrepared(p, src, key, "k2j4m9qx7d4abcde", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(enc.file)

	f, err := os.Open(enc.file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var order []string
	objects := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, hdr.Name)
		objects[hdr.Name] = b
	}
	plain, err := e2e.OpenBytes(objects[e2e.ManifestPath], key, e2e.ManifestBinding("k2j4m9qx7d4abcde", 1))
	if err != nil {
		t.Fatal(err)
	}
	m, err := e2e.ParseManifest(plain)
	if err != nil {
		t.Fatal(err)
	}
	pathOf := map[string]string{}
	for _, mf := range m.Files {
		pathOf[mf.Object] = mf.Path
		if got, err := e2e.OpenBytes(objects[mf.Object], key, e2e.FileBinding("k2j4m9qx7d4abcde", 1, mf.Path)); err != nil || filepath.Base(mf.Path) == "" || len(got) == 0 {
			t.Fatalf("%s: %v", mf.Path, err)
		}
	}
	var uploaded []string
	for _, name := range order {
		if name != e2e.ManifestPath {
			uploaded = append(uploaded, pathOf[name])
		}
	}
	if len(uploaded) != len(paths) {
		t.Fatalf("uploaded %d files, want %d", len(uploaded), len(paths))
	}
	if fmt.Sprint(uploaded) == fmt.Sprint(paths) {
		t.Fatal("objects were uploaded in path order")
	}
}
