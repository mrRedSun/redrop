package client

import (
	"errors"
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
