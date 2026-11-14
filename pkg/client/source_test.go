package client

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseRenderMode(t *testing.T) {
	for value, want := range map[string]RenderMode{"": RenderAuto, "auto": RenderAuto, "markdown": RenderMarkdown, "none": RenderNone} {
		got, err := ParseRenderMode(value)
		if err != nil || got != want {
			t.Errorf("ParseRenderMode(%q) = %q, %v", value, got, err)
		}
	}
	_, err := ParseRenderMode("invalid")
	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("invalid mode must return a typed usage error: %v", err)
	}
}

func TestMarkdownEntryMustBeRegular(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "guides"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"guides", "missing.md"} {
		_, err := Prepare(&Source{Path: dir, Render: RenderMarkdown, Entry: entry})
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), entry) {
			t.Fatalf("--entry %s must return a named usage error: %v", entry, err)
		}
		if entry == "missing.md" && (!errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "symlinks must be relative and stay inside the directory")) {
			t.Fatalf("missing path must preserve its cause and explain the rule: %v", err)
		}
	}
}

func TestRejectedImageWarningsForFilesAndDirectories(t *testing.T) {
	for _, directory := range []bool{false, true} {
		var warnings []string
		source := "![Remote](https://example.com/a.png) ![Data](data:image/png;base64,AAAA) ![Up](../up.png)\n"
		src := &Source{Name: "notes.md", Reader: strings.NewReader(source), Warn: func(w string) { warnings = append(warnings, w) }}
		if directory {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			src.Path, src.Reader, src.Render, src.Entry = dir, nil, RenderMarkdown, "notes.md"
		}
		p, err := Prepare(src)
		if err != nil {
			t.Fatal(err)
		}
		p.Close()
		want := []string{
			`image "https://example.com/a.png" removed: images must be files inside the bundle`,
			`image "data:image/png;base64,AAAA" removed: images must be files inside the bundle`,
			`image "../up.png" removed: images must be files inside the bundle`,
		}
		if !reflect.DeepEqual(warnings, want) || !reflect.DeepEqual(p.Warnings, want) {
			t.Fatalf("directory=%v rejected image warnings = %v / %v, want %v", directory, warnings, p.Warnings, want)
		}
	}
}

func TestSingleMarkdownWarnsAboutMissingReferences(t *testing.T) {
	var warnings []string
	p, err := Prepare(&Source{
		Name: "notes.md", Reader: strings.NewReader("# Notes\n\n![One](pic.png) ![Again](pic.png) [Data](data.csv) [Source](notes.md) [Page](index.html) [Section](#notes) [Docs](https://example.com)\n"),
		Warn: func(warning string) { warnings = append(warnings, warning) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	want := []string{
		"pic.png is not uploaded; share the directory with --render markdown --entry notes.md",
		"data.csv is not uploaded; share the directory with --render markdown --entry notes.md",
	}
	if !reflect.DeepEqual(p.Warnings, want) || !reflect.DeepEqual(warnings, want) {
		t.Fatalf("missing-reference warning mismatch: Prepared %v, callback %v", p.Warnings, warnings)
	}
	files := unpackPrepared(t, p)
	if len(files) != 2 {
		t.Error("warning must not cause sibling files to be uploaded")
	}
}
