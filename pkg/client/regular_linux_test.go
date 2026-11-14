package client

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNonRegularPublishingSourcesDoNotBlock(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "input.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"markdown", "bundle", "entry"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNonRegularSourceHelper$")
			cmd.Env = append(os.Environ(), "REDROP_SOURCE_HELPER="+mode, "REDROP_SOURCE_PATH="+fifo)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("non-regular source blocked instead of returning a usage error")
			}
			if err != nil {
				t.Fatalf("non-regular source rejected incorrectly: %v, %s", err, out)
			}
		})
	}
}

func TestNonRegularBundleEntriesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []RenderMode{RenderNone, RenderMarkdown} {
		var warnings []string
		src := &Source{Path: dir, Kind: "bundle", Render: mode, Warn: func(w string) { warnings = append(warnings, w) }}
		if mode == RenderMarkdown {
			src.Entry = "notes.md"
		}
		p, err := Prepare(src)
		if err != nil {
			t.Fatal(err)
		}
		files := unpackPrepared(t, p)
		p.Close()
		if _, exists := files["pipe"]; exists || len(warnings) != 1 || warnings[0] != `bundle path "pipe" skipped: not a regular file or directory` {
			t.Fatalf("mode=%s special entry not skipped with a warning: %v", mode, warnings)
		}
	}
}

func TestNonRegularSourceHelper(t *testing.T) {
	mode := os.Getenv("REDROP_SOURCE_HELPER")
	if mode == "" {
		return
	}
	file := os.Getenv("REDROP_SOURCE_PATH")
	src := &Source{Path: file, Render: RenderMarkdown}
	if mode == "bundle" {
		src.Kind, src.Render = "bundle", RenderNone
	} else if mode == "entry" {
		src.Path, src.Entry = filepath.Dir(file), filepath.Base(file)
	}
	p, err := Prepare(src)
	if err == nil {
		p.Close()
		t.Fatal("non-regular source accepted")
	}
	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("want usage error, got %v", err)
	}
}
