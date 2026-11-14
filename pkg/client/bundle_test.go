package client

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mrRedSun/redrop/pkg/api"
)

func TestBundleSymlinkPolicyMatchesMarkdown(t *testing.T) {
	dir := t.TempDir()
	for _, subdir := range []string{"shared", ".git", "node_modules"} {
		if err := os.Mkdir(filepath.Join(dir, subdir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"notes.md": "# Notes\n\n![Image](images/pic.png)\n", "shared/pic.png": "image bytes",
		".git/config": "not an asset", "node_modules/private.js": "not an asset", ".DS_Store": "not an asset", "Thumbs.db": "not an asset",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("shared", filepath.Join(dir, "images")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("shared/pic.png", filepath.Join(dir, "image-link.png")); err != nil {
		t.Fatal(err)
	}
	for _, markdownMode := range []bool{false, true} {
		name := "ordinary"
		src := &Source{Path: dir, Kind: api.KindBundle, Render: "none"}
		if markdownMode {
			name, src.Render, src.Entry = "markdown", "markdown", "notes.md"
		}
		t.Run(name, func(t *testing.T) {
			p, err := Prepare(src)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			files := unpackPrepared(t, p)
			for _, asset := range []string{"shared/pic.png", "images/pic.png", "image-link.png"} {
				if files[asset] != "image bytes" {
					t.Errorf("within-root symlink asset %s missing", asset)
				}
			}
			wantCount := 4
			if markdownMode {
				wantCount++
			}
			if len(files) != wantCount {
				t.Errorf("unexpected files, including skipped metadata: %v", files)
			}
		})
	}
}

func TestBundleRejectsUnsafeSymlinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.txt"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"outside-file", "outside-directory", "outside-relative", "absolute-inside", "cycle"} {
		for _, markdownMode := range []bool{false, true} {
			name := kind + "/ordinary"
			if markdownMode {
				name = kind + "/markdown"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				relativeOutside, err := filepath.Rel(dir, filepath.Join(outside, "private.txt"))
				if err != nil {
					t.Fatal(err)
				}
				target := map[string]string{
					"outside-file": filepath.Join(outside, "private.txt"), "outside-directory": outside,
					"outside-relative": relativeOutside, "absolute-inside": filepath.Join(dir, "notes.md"), "cycle": ".",
				}[kind]
				if err := os.Symlink(target, filepath.Join(dir, "asset-link")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				src := &Source{Path: dir, Kind: api.KindBundle, Render: "none"}
				if markdownMode {
					src.Render, src.Entry = "markdown", "notes.md"
				}
				p, err := Prepare(src)
				if err == nil {
					p.Close()
					t.Fatal("unsafe symlink was followed or silently omitted")
				}
				var usage *UsageError
				if !errors.As(err, &usage) || !strings.Contains(err.Error(), "asset-link") {
					t.Errorf("error must identify the bad bundle path and be a usage error: %v", err)
				}
				if kind != "cycle" && !strings.Contains(err.Error(), "symlinks must be relative and stay inside the directory") {
					t.Errorf("symlink error must explain the rule: %v", err)
				}
			})
		}
	}
}

func TestBrokenBundleSymlinksAreSkippedWithWarnings(t *testing.T) {
	for _, mode := range []RenderMode{RenderNone, RenderMarkdown} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("missing.png", filepath.Join(dir, "image-link.png")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			var warnings []string
			src := &Source{Path: dir, Kind: api.KindBundle, Render: mode, Warn: func(w string) { warnings = append(warnings, w) }}
			if mode == RenderMarkdown {
				src.Entry = "notes.md"
			}
			p, err := Prepare(src)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if len(warnings) != 1 || warnings[0] != `bundle path "image-link.png" skipped: broken symlink` || len(p.Warnings) != 1 || p.Warnings[0] != warnings[0] {
				t.Fatalf("missing broken-symlink warning: %v / %v", p.Warnings, warnings)
			}
			files := unpackPrepared(t, p)
			if _, exists := files["image-link.png"]; exists || files["notes.md"] != "# Notes\n" {
				t.Fatal("broken symlink was archived or valid source lost")
			}
		})
	}
}

func TestPackFSSnapshotsAndCollisions(t *testing.T) {
	fsys := fstest.MapFS{"notes.md": &fstest.MapFile{Data: []byte("newer source")}}
	var out bytes.Buffer
	err := packFS(fsys, &out, nil,
		bundleFile{Name: "index.html", Content: strings.NewReader("rendered snapshot"), Size: 17},
		bundleFile{Name: "notes.md", Content: strings.NewReader("original snapshot"), Size: 17, Replace: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(file, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	files := unpackPrepared(t, &Prepared{File: file})
	if len(files) != 2 || files["notes.md"] != "original snapshot" || files["index.html"] != "rendered snapshot" {
		t.Fatalf("source snapshot was replaced or duplicated: %v", files)
	}
	fsys["index.html"] = &fstest.MapFile{Data: []byte("keep existing")}
	out.Reset()
	if err := packFS(fsys, &out, nil, bundleFile{Name: "index.html", Content: strings.NewReader("new"), Size: 3}); err == nil {
		t.Fatal("generated index.html replaced an existing file")
	}
}

func TestPackFSDepthBoundAndWriteErrors(t *testing.T) {
	deep := strings.Repeat("nested/", 65) + "file.txt"
	if err := packFS(fstest.MapFS{deep: &fstest.MapFile{Data: []byte("content")}}, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("unbounded directory nesting accepted: %v", err)
	}
	if err := packFS(fstest.MapFS{"file.txt": &fstest.MapFile{Data: []byte("content")}}, brokenBundleWriter{}, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("archive write failure not propagated: %v", err)
	}
}

type brokenBundleWriter struct{}

func (brokenBundleWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestSingleFileAndZipUseBundleWriter(t *testing.T) {
	dir := t.TempDir()
	html := filepath.Join(dir, "page.html")
	if err := os.WriteFile(html, []byte("<h1>Page</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(&Source{Path: html, Kind: api.KindBundle, Render: "none"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if files := unpackPrepared(t, p); len(files) != 1 || files["index.html"] != "<h1>Page</h1>" {
		t.Fatalf("single-file bundle changed: %v", files)
	}
	zipPath := filepath.Join(dir, "site.zip")
	z, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(z)
	for name, data := range map[string]string{"site/index.html": "<h1>Zip</h1>", "site/style.css": "body{}"} {
		file, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = Prepare(&Source{Path: zipPath, Kind: api.KindBundle, Render: "none"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if files := unpackPrepared(t, p); len(files) != 2 || files["index.html"] != "<h1>Zip</h1>" || files["style.css"] != "body{}" {
		t.Fatalf("ZIP conversion changed: %v", files)
	}
}
