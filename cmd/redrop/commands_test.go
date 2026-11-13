package main

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func TestRenderFlag(t *testing.T) {
	for _, mode := range []string{"auto", "markdown", "none", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			var f contentFlags
			cmd := &cobra.Command{}
			f.register(cmd)
			if err := cmd.ParseFlags([]string{"--render", mode}); err != nil {
				t.Fatal(err)
			}
			src, err := f.source("notes.md")
			if mode == "invalid" {
				var ee *exitError
				if !errors.As(err, &ee) || ee.code != exitUsage {
					t.Errorf("invalid mode should return a usage error, got %v", err)
				}
			} else if err != nil || src.Render != mode {
				t.Fatalf("render mode was not passed to the client: %+v, %v", src, err)
			}
		})
	}
	for _, cmd := range []*cobra.Command{shareCmd(), updateCmd()} {
		flag := cmd.Flags().Lookup("render")
		if flag == nil || flag.DefValue != "auto" {
			t.Errorf("%s must default --render to auto", cmd.Name())
		}
	}
	f := contentFlags{as: "file", render: "markdown"}
	if _, err := f.source("notes.md"); err == nil {
		t.Error("conflicting explicit kind accepted")
	}
}

func TestArtifactSlug(t *testing.T) {
	cases := map[string]string{
		// links, both formats, with or without scheme, port, path, case
		"https://amber-otter-quilt-river.usercontent.example/":      "amber-otter-quilt-river",
		"https://AMBER-otter-quilt-river.usercontent.example:8443/": "amber-otter-quilt-river",
		"amber-otter-quilt-river.usercontent.example/docs/a.txt":    "amber-otter-quilt-river",
		"http://s3eybdkc67r7zegmb7qbugpmvy.usercontent.example/":    "s3eybdkc67r7zegmb7qbugpmvy",
		// bare slugs
		"amber-otter-quilt-river":     "amber-otter-quilt-river",
		" s3eybdkc67r7zegmb7qbugpmvy": "s3eybdkc67r7zegmb7qbugpmvy",
		// artifact IDs and unknown words are IDs
		"hoctdvkdnl33xpx3":  "",
		"doesnotexist00":    "",
		"amber-otter-quilt": "",
	}
	for in, want := range cases {
		got, err := artifactSlug(in)
		if err != nil || got != want {
			t.Errorf("artifactSlug(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"https://redrop.example.com/artifacts/abc",
		"https://amber-otter-quilt.usercontent.example/",
		"https://amber-otter-quilt-river-stone.usercontent.example/",
		"https://s3eybdkc67r7zegmb7qbugpmv.usercontent.example/",
		"https://amber-0tter-quilt-river.usercontent.example/",
		"usercontent.example",
	} {
		_, err := artifactSlug(in)
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != exitUsage {
			t.Errorf("artifactSlug(%q) error %v, want a usage error", in, err)
		}
	}
}
