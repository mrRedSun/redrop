package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
	"github.com/mrRedSun/redrop/pkg/client"
	"github.com/spf13/cobra"
)

// captureCLIOutput is deliberately non-parallel: the CLI writes to process
// stdout/stderr and reads global options.
func captureCLIOutput(t *testing.T, run func()) (string, string) {
	t.Helper()
	t.Setenv("REDROP_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	oldOut, oldErr, oldOpts := os.Stdout, os.Stderr, g
	os.Stdout, os.Stderr = stdout, stderr
	defer func() { os.Stdout, os.Stderr, g = oldOut, oldErr, oldOpts }()
	run()
	out, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	errOut, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(errOut)
}

func writeCLIFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireExitCode(t *testing.T, err error, code int) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != code {
		t.Fatalf("error = %v; want exit code %d", err, code)
	}
}

func TestFolderUploadSkippedNotice(t *testing.T) {
	for _, command := range []string{"share", "update"} {
		for _, skipped := range []bool{false, true} {
			for _, jsonOutput := range []bool{false, true} {
				name := command
				if skipped {
					name += "/skipped"
				} else {
					name += "/clean"
				}
				if jsonOutput {
					name += "/json"
				}
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					// Pruned directory contents are omitted, but do not count
					// toward the skipped file-entry notice.
					if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
						t.Fatal(err)
					}
					writeCLIFile(t, dir, ".git/config", "excluded metadata")
					wantFiles := []string{"#", "##", "4913", "index.html", "x.swp"}
					for _, file := range wantFiles {
						writeCLIFile(t, dir, file, "content")
					}
					if skipped {
						writeCLIFile(t, dir, ".index.html.swo", "private swap")
						writeCLIFile(t, dir, "notes~", "private backup")
					}
					var uploaded bytes.Buffer
					publishPath := "/api/v1/artifacts"
					if command == "update" {
						publishPath += "/artifact-id/versions"
					}
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch {
						case r.Method == http.MethodPost && r.URL.Path == "/api/v1/uploads":
							json.NewEncoder(w).Encode(api.Upload{ID: "upload-id", ChunkSize: 1 << 20})
						case r.Method == http.MethodPut && r.URL.Path == "/api/v1/uploads/upload-id":
							if _, err := io.Copy(&uploaded, r.Body); err != nil {
								t.Error(err)
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							json.NewEncoder(w).Encode(api.Upload{ID: "upload-id", Received: int64(uploaded.Len())})
						case r.Method == http.MethodPost && r.URL.Path == publishPath:
							json.NewEncoder(w).Encode(api.Artifact{
								ID: "artifact-id", URL: "https://amber-otter-quilt-river.usercontent.example/",
								Kind: "bundle", Version: 2, Files: len(wantFiles),
							})
						default:
							t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
						}
					}))
					defer srv.Close()
					out, errOut := captureCLIOutput(t, func() {
						cmd := newRootCmd()
						args := []string{"--url", srv.URL, "--token", "test-token", command}
						if command == "update" {
							args = append(args, "artifact-id")
						}
						args = append(args, dir)
						if jsonOutput {
							args = append(args, "--json")
						}
						cmd.SetArgs(args)
						if err := cmd.Execute(); err != nil {
							t.Fatal(err)
						}
					})
					wantErr := ""
					if skipped {
						wantErr = "redrop: skipped 2 files from folder upload\n"
					}
					if errOut != wantErr {
						t.Errorf("stderr = %q; want %q", errOut, wantErr)
					}
					if jsonOutput && !json.Valid([]byte(out)) {
						t.Errorf("stdout is not JSON: %q", out)
					}
					gz, err := gzip.NewReader(bytes.NewReader(uploaded.Bytes()))
					if err != nil {
						t.Fatal(err)
					}
					defer gz.Close()
					tr := tar.NewReader(gz)
					var gotFiles []string
					for {
						h, err := tr.Next()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						gotFiles = append(gotFiles, h.Name)
					}
					sort.Strings(gotFiles)
					if !reflect.DeepEqual(gotFiles, wantFiles) {
						t.Errorf("uploaded entries = %v; want %v", gotFiles, wantFiles)
					}
				})
			}
		}
	}
}

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
			} else if err != nil || src.Render != client.RenderMode(mode) {
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
	src, err := f.source("notes.md")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(src)
	var ee *exitError
	if !errors.As(mapErr(err), &ee) || ee.code != exitUsage {
		t.Errorf("client conflict should map to usage exit code, got %v", err)
	}
}

func TestMarkdownUsageExitCodes(t *testing.T) {
	for _, src := range []client.Source{
		{Render: client.RenderMarkdown, Path: t.TempDir()},
		{Render: client.RenderMarkdown, Name: "notes.md", Reader: strings.NewReader("x"), SPA: true},
		{Render: client.RenderMarkdown, Name: "site.zip", Reader: strings.NewReader("x")},
		{Render: client.RenderMarkdown, Name: "../notes.md", Reader: strings.NewReader("x")},
	} {
		_, err := client.Prepare(&src)
		var ee *exitError
		if !errors.As(mapErr(err), &ee) || ee.code != exitUsage {
			t.Errorf("invalid Markdown options must exit 2: %v", err)
		}
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
