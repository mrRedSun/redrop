package client

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mrRedSun/redrop/pkg/api"
)

func readPack(file string) (map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readTarGzip(f)
}
func readTarGzip(input io.Reader) (map[string]string, error) {
	gz, err := gzip.NewReader(input)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[h.Name] = string(b)
	}
}
func sortedKeys(files map[string]string) []string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func sumBytes(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }

// Implements only the real client's upload protocol. It verifies the
// finalization checksum against the captured bytes rather than supplying
// a precomputed digest to Watch.
type uploadServer struct {
	mu       sync.Mutex
	uploads  map[string][]byte
	versions int
	files    map[string]string
	onStart  func()
}

func newUploadClient(t *testing.T) (*Client, *uploadServer) {
	t.Helper()
	state := &uploadServer{uploads: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/uploads":
			var req api.CreateUploadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			id := fmt.Sprint(len(state.uploads) + 1)
			state.uploads[id] = nil
			if state.onStart != nil {
				state.onStart()
			}
			json.NewEncoder(w).Encode(api.Upload{ID: id, ChunkSize: 1 << 20})
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/uploads/")
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			state.uploads[id] = append(state.uploads[id], data...)
			json.NewEncoder(w).Encode(api.Upload{ID: id, Received: int64(len(state.uploads[id]))})
		case r.Method == "POST" && (r.URL.Path == "/api/v1/artifacts" || strings.HasSuffix(r.URL.Path, "/versions")):
			var req api.PublishRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			data := state.uploads[req.UploadID]
			if actual := sumBytes(data); actual != req.SHA256 {
				t.Errorf("client finalized mismatched blob checksum: %s != %s", req.SHA256, actual)
				http.Error(w, "bad checksum", 400)
				return
			}
			if req.Kind == api.KindBundle {
				files, err := readTarGzip(bytes.NewReader(data))
				if err != nil {
					t.Errorf("invalid prepared bundle: %v", err)
					http.Error(w, "invalid pack", 400)
					return
				}
				state.files = files
			} else {
				state.files = map[string]string{req.Filename: string(data)}
			}
			state.versions++
			json.NewEncoder(w).Encode(api.Artifact{ID: "art", Version: state.versions})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return New(server.URL, "test-token"), state
}
func (s *uploadServer) count() int        { s.mu.Lock(); defer s.mu.Unlock(); return len(s.uploads) }
func (s *uploadServer) versionCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.versions }
func (s *uploadServer) lastFiles() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.files {
		out[k] = v
	}
	return out
}

func TestRealClientWatchUnchangedAfterPublish(t *testing.T) {
	for _, kind := range []string{"directory", "html", "file_bundle", "tar_bundle", "zip_bundle"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			root := dir
			contents := "before"
			write(t, dir, "index.html", contents)
			src := &Source{Path: dir}
			switch kind {
			case "html":
				root = filepath.Join(dir, "index.html")
				src.Path = root
			case "file_bundle":
				root = filepath.Join(dir, "index.html")
				src.Path = root
				src.Kind = api.KindBundle
			case "tar_bundle":
				root = filepath.Join(dir, "site.tar.gz")
				src.Path = root
				src.Kind = api.KindBundle
				makeArchive(t, root, false, contents)
			case "zip_bundle":
				root = filepath.Join(dir, "site.zip")
				src.Path = root
				src.Kind = api.KindBundle
				makeArchive(t, root, true, contents)
			}
			c, state := newUploadClient(t)
			h := newHarness(t, c, root)
			h.w.src = src // set before the first event; event barrier synchronizes access
			contents = "after"
			if kind == "tar_bundle" || kind == "zip_bundle" {
				makeArchive(t, root, kind == "zip_bundle", contents)
			} else {
				write(t, dir, "index.html", contents)
			}
			h.event(filepath.Join(dir, "index.html"), fsnotify.Write)
			// For an explicit archive root, the event belongs to that file.
			if kind == "tar_bundle" || kind == "zip_bundle" {
				h.event(root, fsnotify.Write)
			}
			h.advance(time.Second)
			if state.count() != 1 {
				t.Fatalf("first real edit uploads=%d", state.count())
			}
			if files := state.lastFiles(); files["index.html"] != contents {
				t.Fatalf("served content=%v", files)
			}
			// A touch AFTER publication must not create another upload.
			target := filepath.Join(dir, "index.html")
			if kind == "tar_bundle" || kind == "zip_bundle" {
				target = root
			}
			at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(target, at, at); err != nil {
				t.Fatal(err)
			}
			h.event(target, fsnotify.Chmod)
			h.advance(time.Second)
			if state.count() != 1 {
				t.Fatal("touch created another upload after first publish")
			}
			// Rewriting the exact same bytes is also a no-op.
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0600); err != nil {
				t.Fatal(err)
			}
			h.event(target, fsnotify.Write)
			h.advance(time.Second)
			if state.count() != 1 {
				t.Fatal("identical rewrite uploaded after first publish")
			}
		})
	}
}
func makeArchive(t *testing.T, file string, zipFormat bool, contents string) {
	t.Helper()
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if zipFormat {
		zw := zip.NewWriter(f)
		entry, err := zw.Create("index.html")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(entry, contents); err != nil {
			t.Fatal(err)
		}
		if err = zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0600, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, contents); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSkipEntryNames(t *testing.T) {
	skip := []string{".index.html.swp", ".index.html.swo", ".index.html.swn", ".index.html.swa", ".index.html.swx", ".INDEX.HTML.SWP", ".#page", "#page#", "page~", "page___jb_tmp___", "page___jb_old___", ".goutputstream-abcdef", ".DS_Store", "Thumbs.db"}
	keep := []string{"4913", "#", "##", "x.swp", "x.swo", "notes.saa", ".sample.sxa", ".sample.swa.txt", ".logo.svg", ".schema.sql", ".subs.srt", ".key.sig", ".index.html.saa", ".page.swq", ".swp", "file___jb_tmp___notes", "plain.html", "api/items/4913"}
	for _, name := range skip {
		if !skipEntryName(name, false) {
			t.Errorf("failed to skip %s", name)
		}
	}
	for _, name := range keep {
		if skipEntryName(name, false) {
			t.Errorf("dropped legitimate %s", name)
		}
	}
	// Cover the whole Vim range, .swa to .swp plus .swx, and nothing else
	// in .sw*.
	for c := 'a'; c <= 'z'; c++ {
		name := fmt.Sprintf(".page.sw%c", c)
		if want := c <= 'p' || c == 'x'; skipEntryName(name, false) != want {
			t.Errorf("%s skipped=%v; want %v", name, !want, want)
		}
	}
}
func TestShareAndUpdateExcludeSwapFiles(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(fmt.Sprint(update), func(t *testing.T) {
			dir := t.TempDir()
			keep := []string{"index.html", "4913", "#", "##", "x.swp", "file___jb_tmp___notes", ".logo.svg", ".schema.sql", ".subs.srt", ".key.sig"}
			for _, name := range keep {
				write(t, dir, name, "legitimate")
			}
			skip := []string{".index.html.swp", ".index.html.swo", ".index.html.swa", ".index.html.swx", "index.html~", ".#index.html", "#index.html#", "page___jb_tmp___", "page___jb_old___", ".goutputstream-abcd"}
			for _, name := range skip {
				write(t, dir, name, "private-buffer")
			}
			write(t, dir, ".git/config", "private-git")
			c, state := newUploadClient(t)
			skipped := 0
			src := &Source{Path: dir, OnSkipped: func(n int) { skipped = n }}
			var err error
			if update {
				_, err = c.Update(context.Background(), "art", src, nil)
			} else {
				_, err = c.Share(context.Background(), src, PublishOptions{}, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(keep)
			if names := sortedKeys(state.lastFiles()); !reflect.DeepEqual(names, keep) {
				t.Fatalf("actual upload files=%v; want %v", names, keep)
			}
			if skipped != len(skip) {
				t.Fatalf("skipped notice=%d; want %d", skipped, len(skip))
			}
		})
	}
}
func TestPreparedDigestMatchesPackedBytes(t *testing.T) {
	dir := t.TempDir()
	// Prefixes and spaces exercise path sorting separately from hash text.
	for _, name := range []string{"a", "a b", "a space/x", "index.html"} {
		write(t, dir, name, "contents:"+name)
	}
	p, err := Prepare(&Source{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	expected, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest != expected {
		t.Fatalf("prepared digest %s differs from tree %s", p.Digest, expected)
	}
	// After preparing, change live disk. The recorded digest must remain a
	// digest of the tar snapshot, never a later hash of the live tree.
	write(t, dir, "a", "edited after Prepare")
	files, err := readPack(p.File)
	if err != nil {
		t.Fatal(err)
	}
	db := &treeDigestBuilder{}
	for name, data := range files {
		db.add(name, sumBytes([]byte(data)))
	}
	if actual := db.digest(); actual != p.Digest {
		t.Fatalf("digest does not cover packed bytes: %s != %s", actual, p.Digest)
	}
	changed, _ := hashTree(dir)
	if changed == p.Digest {
		t.Fatal("live-tree mutation was absorbed into snapshot digest")
	}
}
func TestHashTreeUsesSameSymlinkAndMtimeRulesAsPacking(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "page")
	outside := t.TempDir()
	write(t, outside, "shared.css", "before")
	if err := os.Symlink(filepath.Join(outside, "shared.css"), filepath.Join(dir, "shared.css")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	before, err := hashTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "index.html"), at, at); err != nil {
		t.Fatal(err)
	}
	afterTouch, err := hashTree(dir)
	if err != nil || before != afterTouch {
		t.Fatalf("mtime changed digest: %v", err)
	}
	write(t, outside, "shared.css", "after")
	p, err := Prepare(&Source{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	after, err := hashTree(dir)
	if err != nil || p.Digest != after || after == before {
		t.Fatalf("symlink digest mismatch: %v", err)
	}
	files, err := readPack(p.File)
	if err != nil || files["shared.css"] != "after" {
		t.Fatalf("symlink target not packed: %v, %v", files, err)
	}
}

func TestPrepareFailureCleansSnapshot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	_, err := Prepare(&Source{Reader: failingReader{}, Name: "page.html"})
	if err == nil {
		t.Fatal("expected read failure")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed Prepare leaked temp files: %v, %v", entries, err)
	}
	_, err = Prepare(&Source{Path: t.TempDir(), Kind: api.KindHTML})
	if err == nil {
		t.Fatal("accepted directory as html")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, fmt.Errorf("source read failed") }

// A single file is uploaded from its own path, not a copy. An edit during
// the upload must never publish a version whose bytes or recorded digest
// mix the old and new content: UpdateDigest fails with ErrSourceChanged,
// Update retries once, and a bundle is unaffected because it was packed.
func TestSingleFileMidUploadEdit(t *testing.T) {
	for _, edit := range []string{"after", "after!", "after, and longer"} { // shorter, same size, longer
		t.Run(edit, func(t *testing.T) {
			setup := func(t *testing.T, kind string, edits int) (*Client, *uploadServer, *Source) {
				dir := t.TempDir()
				write(t, dir, "page.html", "before")
				src := &Source{Path: filepath.Join(dir, "page.html"), Kind: kind}
				c, state := newUploadClient(t)
				state.onStart = func() {
					if edits == 0 {
						return
					}
					edits--
					content := edit
					if edits > 0 {
						content = strings.ToUpper(edit) // differs from the next edit
					}
					if err := os.WriteFile(src.Path, []byte(content), 0600); err != nil {
						t.Error(err)
					}
				}
				return c, state, src
			}
			t.Run("UpdateDigest fails without a version", func(t *testing.T) {
				c, state, src := setup(t, "", 1)
				_, _, err := c.UpdateDigest(context.Background(), "art", src, nil)
				if !errors.Is(err, ErrSourceChanged) {
					t.Fatalf("err=%v; want ErrSourceChanged", err)
				}
				if state.versionCount() != 0 {
					t.Fatal("published a version of a file edited mid-upload")
				}
			})
			t.Run("Update retries once", func(t *testing.T) {
				c, state, src := setup(t, "", 1)
				if _, err := c.Update(context.Background(), "art", src, nil); err != nil {
					t.Fatal(err)
				}
				if got := state.lastFiles()["page.html"]; got != edit || state.versionCount() != 1 {
					t.Fatalf("published %q in %d versions; want %q once", got, state.versionCount(), edit)
				}
			})
			t.Run("Share gives up after the retry", func(t *testing.T) {
				c, state, src := setup(t, "", 2)
				_, err := c.Share(context.Background(), src, PublishOptions{}, nil)
				if !errors.Is(err, ErrSourceChanged) || !strings.Contains(err.Error(), "tried twice") {
					t.Fatalf("err=%v; want a clear ErrSourceChanged", err)
				}
				if state.versionCount() != 0 {
					t.Fatal("published a version of a file edited mid-upload")
				}
			})
			t.Run("bundle is packed before upload", func(t *testing.T) {
				c, state, src := setup(t, api.KindBundle, 1)
				digest, _, err := c.UpdateDigest(context.Background(), "art", src, nil)
				if err != nil {
					t.Fatal(err)
				}
				if digest != sumBytes([]byte("before")) || state.lastFiles()["index.html"] != "before" {
					t.Fatalf("digest=%s files=%v", digest, state.lastFiles())
				}
			})
		})
	}
}

func TestSingleFileIsNotCopied(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	file := filepath.Join(t.TempDir(), "video.bin")
	write(t, filepath.Dir(file), "video.bin", "large")
	p, err := Prepare(&Source{Path: file})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if entries, _ := os.ReadDir(tmp); p.File != file || len(entries) != 0 {
		t.Fatalf("single file copied: File=%s temp=%v", p.File, entries)
	}
}

func TestExplicitSkippedNameIsUploaded(t *testing.T) {
	for _, name := range []string{"notes~", ".file.swo", "#draft#"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, name, "explicit")
			c, state := newUploadClient(t)
			src := &Source{Path: filepath.Join(dir, name), OnSkipped: func(int) { t.Error("explicit file reported skipped") }}
			digest, _, err := c.UpdateDigest(context.Background(), "art", src, nil)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := hashTree(src.Path)
			if err != nil {
				t.Fatal(err)
			}
			if digest != expected || state.lastFiles()[name] != "explicit" {
				t.Fatal("explicit file not hashed/published")
			}
		})
	}
}
func TestArchiveEntriesNotEditorFiltered(t *testing.T) {
	for _, zipFormat := range []bool{false, true} {
		t.Run(fmt.Sprint(zipFormat), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "site.tar.gz")
			var raw bytes.Buffer
			if zipFormat {
				file = filepath.Join(t.TempDir(), "site.zip")
				zw := zip.NewWriter(&raw)
				for _, name := range []string{"index.html", ".index.html.swo"} {
					e, err := zw.Create(name)
					if err != nil {
						t.Fatal(err)
					}
					io.WriteString(e, name)
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				gz := gzip.NewWriter(&raw)
				tw := tar.NewWriter(gz)
				for _, name := range []string{"index.html", ".index.html.swo"} {
					if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(name)), Typeflag: tar.TypeReg}); err != nil {
						t.Fatal(err)
					}
					io.WriteString(tw, name)
				}
				tw.Close()
				gz.Close()
			}
			if err := os.WriteFile(file, raw.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := Prepare(&Source{Path: file, Kind: api.KindBundle})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			files, err := readPack(p.File)
			if err != nil {
				t.Fatal(err)
			}
			if files[".index.html.swo"] == "" || p.skipped != 0 {
				t.Fatal("archive entries were editor-filtered")
			}
			// A zip is converted, so its source hash is recorded. A tar.gz
			// is uploaded as is, so the upload checksum is its digest.
			if zipFormat && p.Digest != sumBytes(raw.Bytes()) || !zipFormat && (p.Digest != "" || p.File != file) {
				t.Fatalf("archive digest=%q file=%s", p.Digest, p.File)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"17", 17 * time.Second}, {now.Add(time.Minute).Format(http.TimeFormat), time.Minute}, {now.Add(-time.Minute).Format(http.TimeFormat), 0}, {"-1", 0}, {"9223372036854775807", 0}, {"invalid", 0}, {"", 0}} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("Retry-After %q = %v; want %v", tc.value, got, tc.want)
		}
	}
}
func TestClientPreservesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(429)
		io.WriteString(w, `{"status":429,"detail":"rate limited"}`)
	}))
	defer server.Close()
	_, err := New(server.URL, "").GetArtifact(context.Background(), "id")
	apiErr, ok := err.(*Error)
	if !ok || apiErr.Status != 429 || apiErr.RetryAfter != 37*time.Second {
		t.Fatalf("header lost: %#v", err)
	}
}
