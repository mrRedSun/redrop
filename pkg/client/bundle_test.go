package client

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	for _, kind := range []string{"outside-file", "outside-directory", "outside-relative", "absolute-inside", "cycle", "self-loop"} {
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
					"outside-relative": relativeOutside, "absolute-inside": filepath.Join(dir, "notes.md"), "cycle": ".", "self-loop": "asset-link",
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
				if kind != "cycle" && kind != "self-loop" && !strings.Contains(err.Error(), "symlinks must be relative and stay inside the directory") {
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
	_, err := packFS(fsys, &out, walkHooks{},
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
	if _, err := packFS(fsys, &out, walkHooks{}, bundleFile{Name: "index.html", Content: strings.NewReader("new"), Size: 3}); err == nil {
		t.Fatal("generated index.html replaced an existing file")
	}
}

func TestPackFSDepthBoundAndWriteErrors(t *testing.T) {
	deep := strings.Repeat("nested/", 65) + "file.txt"
	if _, err := packFS(fstest.MapFS{deep: &fstest.MapFile{Data: []byte("content")}}, io.Discard, walkHooks{}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("unbounded directory nesting accepted: %v", err)
	}
	if _, err := packFS(fstest.MapFS{"file.txt": &fstest.MapFile{Data: []byte("content")}}, brokenBundleWriter{}, walkHooks{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("archive write failure not propagated: %v", err)
	}
}

// All markers are invented. The expected paths are independent of the skip rule.
var secretFixturePaths = []string{
	".env", ".env.local", ".env.production.local", "nested/.ENV", ".env.example.local",
	".ssh/identity", ".aws/credentials", ".gnupg/private/identity",
	"nested/.ssh/identity", ".docker/config.json", "nested/.docker/CONFIG.JSON", ".kube/config",
	".netrc", ".npmrc", ".pypirc", ".git-credentials", ".pgpass",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "nested/ID_RSA",
	"server.key", "nested/client.P12", "client.pfx",
}

var secretFixtureExcluded = []string{
	".env", ".env.local", ".env.production.local", "nested/.ENV", ".env.example.local",
	".ssh", ".aws", ".gnupg", "nested/.ssh", ".docker/config.json", "nested/.docker/CONFIG.JSON", ".kube/config",
	".netrc", ".npmrc", ".pypirc", ".git-credentials", ".pgpass",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "nested/ID_RSA",
	"server.key", "nested/client.P12", "client.pfx",
}

func TestSecretFolderPacking(t *testing.T) {
	for _, mode := range []RenderMode{RenderNone, RenderMarkdown} {
		for _, include := range []bool{false, true} {
			label := string(mode) + "/default"
			if include {
				label = string(mode) + "/include"
			}
			t.Run(label, func(t *testing.T) {
				dir := t.TempDir()
				allowed := []string{"notes.md", ".env.example", ".env.sample", ".env.template", "nested/.ENV.EXAMPLE",
					"id_rsa.pub", "id_dsa.pub", "id_ecdsa.pub", "id_ed25519.pub", "file.key.sig", ".logo.svg", ".schema.sql", ".subs.srt",
					".docker/readme.txt", ".kube/config.yaml", ".SSH/readme.txt", "drafts~/notes.txt", "x.swp", "4913", "#", "##"}
				for _, name := range append(append([]string(nil), allowed...), secretFixturePaths...) {
					write(t, dir, name, "synthetic-marker")
				}
				for _, name := range []string{".git/config", "node_modules/pkg/lib.js", "notes~", ".#lock", ".notes.swp", "#notes#", "x___jb_tmp___", ".goutputstream-x"} {
					write(t, dir, name, "editor-marker")
				}
				var warnings []string
				src := &Source{Path: dir, Render: mode, Entry: "notes.md", Warn: func(w string) { warnings = append(warnings, w) }}
				if include {
					src.IncludeSecrets = true
				}
				p, err := Prepare(src)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				files := unpackPrepared(t, p)
				want := append([]string(nil), allowed...)
				if mode == RenderMarkdown {
					want = append(want, "index.html")
				}
				if include {
					want = append(want, secretFixturePaths...)
				}
				sort.Strings(want)
				if got := sortedKeys(files); !reflect.DeepEqual(got, want) {
					t.Errorf("folder contains excluded paths or loses allowed files: got %v; want %v", got, want)
				}
				var wantWarnings []string
				if !include {
					for _, name := range secretFixtureExcluded {
						wantWarnings = append(wantWarnings, `bundle path "`+name+`": name excluded by the secret rule (use --include-secrets to include)`)
					}
				}
				sort.Strings(warnings)
				sort.Strings(wantWarnings)
				if !reflect.DeepEqual(warnings, wantWarnings) {
					t.Errorf("secret warning count=%d; want %d relative-path warnings", len(warnings), len(wantWarnings))
				}
				preparedWarnings := append([]string(nil), p.Warnings...)
				sort.Strings(preparedWarnings)
				if !reflect.DeepEqual(preparedWarnings, wantWarnings) {
					t.Error("Prepared.Warnings differs from reported warnings")
				}
				if !include {
					before, err := hashTree(dir)
					if err != nil || before != p.Digest {
						t.Fatalf("hashTree differs from folder pack: %v", err)
					}
					for _, name := range secretFixturePaths {
						write(t, dir, name, "changed-synthetic-marker")
					}
					after, err := hashTree(dir)
					if err != nil || after != before {
						t.Error("secret edits changed the default content digest")
					}
				}
			})
		}
	}
}

func TestSecretMarkdownEntry(t *testing.T) {
	for _, entry := range []string{".env", ".ssh/notes.md", ".aws/notes.md", ".gnupg/notes.md", ".docker/config.json", ".kube/config", "nested/.npmrc", "id_ed25519", "notes.key"} {
		t.Run(entry, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, entry, "# Synthetic document\n")
			src := &Source{Path: dir, Render: RenderMarkdown, Entry: entry}
			p, err := Prepare(src)
			if p != nil {
				p.Close()
			}
			var usage *UsageError
			if !errors.As(err, &usage) || !strings.Contains(err.Error(), "skip rule") {
				t.Fatalf("secret Markdown entry accepted: %v", err)
			}
			src.IncludeSecrets = true
			p, err = Prepare(src)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if files := unpackPrepared(t, p); files[entry] != "# Synthetic document\n" || files["index.html"] == "" {
				t.Fatal("opt-in did not preserve the Markdown source")
			}
		})
	}
}

func TestSecretSingleFileWarning(t *testing.T) {
	for _, name := range []string{".env", ".ssh/identity", ".docker/config.json", "id_rsa", "server.key", "client.p12", "client.pfx"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, name, "synthetic-marker\x00\xff\n")
			var warnings []string
			src := &Source{Path: filepath.Join(dir, filepath.FromSlash(name)), Render: RenderNone, Warn: func(w string) { warnings = append(warnings, w) }}
			p, err := Prepare(src)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			content, err := os.ReadFile(p.File)
			if err != nil || string(content) != "synthetic-marker\x00\xff\n" {
				t.Fatal("explicit secret file bytes changed")
			}
			want := `explicit file "` + filepath.Base(name) + `" matches a conventional secret path; publishing as requested`
			if len(warnings) != 1 || warnings[0] != want || !reflect.DeepEqual(p.Warnings, warnings) {
				t.Fatalf("explicit secret file lacks relative-path warning: %v", warnings)
			}
		})
	}
}

func TestSecretExplicitSourceOptions(t *testing.T) {
	for _, tc := range []struct {
		label, source, name, kind string
		render                    RenderMode
		stdin, include            bool
	}{
		{label: "renamed", source: ".env", name: "safe.txt"},
		{label: "secret name override", source: "safe.txt", name: ".env"},
		{label: "stdin", name: ".env", stdin: true},
		{label: "stdin opt-in", name: ".env", stdin: true, include: true},
		{label: "bundle", source: ".env", kind: api.KindBundle},
		{label: "bundle opt-in", source: ".env", kind: api.KindBundle, include: true},
		{label: "Markdown", source: ".env", render: RenderMarkdown},
	} {
		t.Run(tc.label, func(t *testing.T) {
			const original = "# Synthetic source\n"
			var warnings []string
			src := &Source{Name: tc.name, Kind: tc.kind, Render: tc.render, IncludeSecrets: tc.include, Warn: func(w string) { warnings = append(warnings, w) }}
			if tc.stdin {
				src.Reader = strings.NewReader(original)
			} else {
				dir := t.TempDir()
				write(t, dir, tc.source, original)
				src.Path = filepath.Join(dir, tc.source)
			}
			p, err := Prepare(src)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			var content string
			if p.Kind == api.KindBundle {
				name := tc.name
				if name == "" {
					name = tc.source
				}
				content = unpackPrepared(t, p)[name]
			} else {
				body, err := os.ReadFile(p.File)
				if err != nil {
					t.Fatal(err)
				}
				content = string(body)
			}
			if content != original || len(warnings) != 1 || !strings.Contains(warnings[0], `explicit file ".env"`) {
				t.Fatalf("explicit source content=%q warnings=%v; want original bytes and secret-name warning", content, warnings)
			}
		})
	}
}

func TestSecretMarkdownOptInKeepsEditorFilter(t *testing.T) {
	for _, entry := range []string{"notes~", ".#notes.md", "node_modules/readme.md", "docs/.git"} {
		dir := t.TempDir()
		write(t, dir, entry, "# Synthetic source\n")
		p, err := Prepare(&Source{Path: dir, Render: RenderMarkdown, Entry: entry, IncludeSecrets: true})
		if p != nil {
			p.Close()
		}
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "skip rule") {
			t.Errorf("secret opt-in allowed editor/VCS entry %s: %v", entry, err)
		}
	}
}

func TestSecretSymlinksSkippedBeforeFollowing(t *testing.T) {
	for _, name := range []string{".env", ".ssh", ".aws", ".gnupg", ".docker/config.json", ".kube/config", "private.key"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "index.html", "public")
			link := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/nonexistent-synthetic-target", link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			p, err := Prepare(&Source{Path: dir})
			if err != nil {
				t.Fatalf("secret symlink was followed: %v", err)
			}
			defer p.Close()
			if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], `"`+name+`": name excluded by the secret rule`) {
				t.Fatalf("secret symlink not excluded before following: %v", p.Warnings)
			}
		})
	}
}

func TestSecretOnlyFolderWarnsBeforeFailing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", "synthetic-marker")
	var warnings []string
	p, err := Prepare(&Source{Path: dir, Warn: func(w string) { warnings = append(warnings, w) }})
	if p != nil {
		p.Close()
	}
	if !errors.Is(err, ErrEmptyDirectory) || len(warnings) != 1 || !strings.Contains(warnings[0], `".env": name excluded`) {
		t.Fatalf("secret-only folder err=%v warning count=%d; want empty folder and one warning", err, len(warnings))
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
