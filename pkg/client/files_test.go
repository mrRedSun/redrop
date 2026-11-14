package client

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
)

func TestExplicitSingleFileSymlinks(t *testing.T) {
	base := t.TempDir()
	links, target := filepath.Join(base, "links"), filepath.Join(base, "target")
	for _, dir := range []string{links, target} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const original = "# Linked notes\n"
	file := filepath.Join(target, "notes.md")
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "private.txt"), []byte("not requested"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, absolute := range []bool{false, true} {
		name := "relative.md"
		to := filepath.Join("..", "target", "notes.md")
		if absolute {
			name, to = "absolute.md", file
		}
		link := filepath.Join(links, name)
		if err := os.Symlink(to, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		for _, kind := range []string{"", api.KindBundle} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				p, err := Prepare(&Source{Path: link, Kind: kind, Render: RenderAuto})
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				files := unpackPrepared(t, p)
				wantCount := 1
				if kind == "" {
					wantCount++
					if !strings.Contains(files["index.html"], `<h1 id="md-linked-notes">Linked notes`) {
						t.Fatal("auto-rendered symlinked source lost its heading")
					}
				}
				if files[name] != original || len(files) != wantCount {
					t.Fatalf("symlinked source lost bytes or uploaded siblings: %v", files)
				}
			})
		}
	}
}

func TestBrokenSelectedSymlinkNamesTheMissingTarget(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "notes.md")
	if err := os.Symlink("missing.md", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, src := range []Source{{Path: link}, {Path: dir, Render: RenderMarkdown, Entry: "notes.md"}} {
		_, err := Prepare(&src)
		var usage *UsageError
		if !errors.As(err, &usage) || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), "target that does not exist") {
			t.Fatalf("broken selected symlink needs a named missing-target usage error: %v", err)
		}
	}
}

func TestMissingTopLevelSourceIsUsage(t *testing.T) {
	for _, name := range []string{"missing.md", "missing-directory"} {
		file := filepath.Join(t.TempDir(), name)
		_, err := Prepare(&Source{Path: file})
		var usage *UsageError
		if !errors.As(err, &usage) || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "symlink") {
			t.Fatalf("missing source must report a missing-path usage error: %v", err)
		}
	}
}
