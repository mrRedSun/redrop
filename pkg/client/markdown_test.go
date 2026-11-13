package client

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
)

func unpackPrepared(t *testing.T, p *Prepared) map[string]string {
	t.Helper()
	f, err := os.Open(p.File)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tw := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tw.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tw)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := files[h.Name]; exists {
			t.Fatalf("duplicate archive member %q", h.Name)
		}
		files[h.Name] = string(data)
	}
	return files
}

func TestPrepareMarkdownModes(t *testing.T) {
	for _, tc := range []struct {
		name, render, kind string
		wantRender         bool
	}{
		{"notes.md", "", "", true},
		{"NOTES.MD", "auto", "", true},
		{"notes.md", "none", "", false},
		{"notes.md", "", api.KindFile, false},
		{"notes.md", "auto", api.KindHTML, false},
		{"notes.txt", "auto", "", false},
		{"notes.txt", "markdown", "", true},
		{"notes.md", "markdown", api.KindBundle, true},
	} {
		t.Run(tc.name+tc.render+tc.kind, func(t *testing.T) {
			const original = "# Hello\r\n\r\nOriginal bytes.\n"
			p, err := Prepare(&Source{Reader: strings.NewReader(original), Name: tc.name, Render: tc.render, Kind: tc.kind})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if tc.wantRender {
				if p.Kind != api.KindBundle || p.Entry != "index.html" {
					t.Fatalf("rendered kind/entry = %s/%s", p.Kind, p.Entry)
				}
				files := unpackPrepared(t, p)
				if len(files) != 2 || files[tc.name] != original || !strings.Contains(files["index.html"], `<h1 id="md-hello">Hello`) {
					t.Fatal("rendered page or exact original missing")
				}
			} else {
				if p.Kind == api.KindBundle {
					t.Fatal("raw source became a bundle")
				}
				b, err := os.ReadFile(p.File)
				if err != nil || string(b) != original {
					t.Fatal("raw source changed")
				}
			}
			p.Close()
			if _, err := os.Stat(p.File); !os.IsNotExist(err) {
				t.Error("temporary upload not cleaned")
			}
		})
	}
}

func TestPrepareMarkdownFileAndBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs", "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "# A report\n\n![Sketch](images/sketch.png)\n"
	for name, data := range map[string]string{"docs/notes.md": original, "docs/images/sketch.png": "image bytes", "other.txt": "asset"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Sharing a file must not silently upload its siblings.
	p, err := Prepare(&Source{Path: filepath.Join(dir, "docs", "notes.md")})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	files := unpackPrepared(t, p)
	if len(files) != 2 || files["notes.md"] != original {
		t.Fatal("single-file rendering uploaded siblings or lost the original")
	}
	p, err = Prepare(&Source{Path: dir, Render: "markdown", Entry: "docs/notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	files = unpackPrepared(t, p)
	if len(files) != 4 || files["docs/notes.md"] != original || files["docs/images/sketch.png"] != "image bytes" || !strings.Contains(files["index.html"], `src="./docs/images/sketch.png"`) || !strings.Contains(files["index.html"], `href="docs/notes.md?download"`) {
		t.Fatal("bundle lost source, assets, relative image, or source download")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); !os.IsNotExist(err) {
		t.Error("rendering wrote into the source directory")
	}
}

func TestPrepareMarkdownInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []Source{
		{Render: "other"},
		{Render: "markdown", Kind: "file", Name: "notes.md", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "notes.md", SPA: true, Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "notes.md", Entry: "other.html", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "../notes.md", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "index.html", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: ".", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "site.zip", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "site.tar.gz", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "site.tgz", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: "__redrop/x.md", Reader: strings.NewReader("x")},
		{Render: "markdown", Name: `a\b.md`, Reader: strings.NewReader("x")},
		{Render: "markdown", Path: dir},
		{Render: "markdown", Path: dir, Entry: "../notes.md"},
		{Render: "markdown", Path: dir, Entry: "notes.md"},
	}
	for _, src := range cases {
		if p, err := Prepare(&src); err == nil {
			p.Close()
			t.Errorf("accepted invalid source %#v", src)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil || string(b) != "keep" {
		t.Fatal("existing index.html was changed")
	}
}

func TestMarkdownBundleSkipsAssetSymlinks(t *testing.T) {
	outside := t.TempDir()
	private := filepath.Join(outside, "private.txt")
	if err := os.WriteFile(private, []byte("private bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(dir, "file-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "dir-link")); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(&Source{Path: dir, Render: "markdown", Entry: "notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	files := unpackPrepared(t, p)
	if len(files) != 2 || files["notes.md"] != "# Notes\n" {
		t.Fatal("asset symlinks were not skipped")
	}
}

func TestMarkdownBundleSourceCannotEscape(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.md"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "private.md"), filepath.Join(dir, "notes.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if p, err := Prepare(&Source{Path: dir, Render: "markdown", Entry: "notes.md"}); err == nil {
		p.Close()
		t.Fatal("rendering followed a source symlink outside the bundle")
	}
}
