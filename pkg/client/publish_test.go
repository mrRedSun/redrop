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
	// An in-root relative link: packing and hashing follow it the same way.
	outside := filepath.Join(dir, "assets")
	write(t, outside, "shared.css", "before")
	if err := os.Symlink(filepath.Join("assets", "shared.css"), filepath.Join(dir, "shared.css")); err != nil {
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

// A zip rewritten while it is converted is a source change, whether the
// conversion then fails or succeeds; an unchanged bad zip stays an error.
func TestZipChangedDuringConversion(t *testing.T) {
	for _, tc := range []struct {
		name, rewrite string
		changed       bool
	}{
		{"rewritten to garbage", "not a zip", true},
		{"rewritten to another zip", "other", true},
		{"unchanged", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "site.zip")
			makeArchive(t, file, true, "v1")
			src := &Source{Path: file, Kind: api.KindBundle, beforeRead: func(string) {
				switch tc.rewrite {
				case "":
				case "other":
					makeArchive(t, file, true, "v2")
				default:
					if err := os.WriteFile(file, []byte(tc.rewrite), 0600); err != nil {
						t.Error(err)
					}
				}
			}}
			p, err := Prepare(src)
			if tc.changed != errors.Is(err, ErrSourceChanged) {
				t.Fatalf("err=%v; want ErrSourceChanged=%v", err, tc.changed)
			}
			if err == nil {
				p.Close()
			}
		})
	}
	// A zip that was never valid fails as is, not as a change.
	file := filepath.Join(t.TempDir(), "bad.zip")
	write(t, filepath.Dir(file), "bad.zip", "not a zip")
	if _, err := Prepare(&Source{Path: file, Kind: api.KindBundle}); err == nil || errors.Is(err, ErrSourceChanged) {
		t.Fatalf("bad zip: %v", err)
	}
}

// A root that is a symlink is resolved once: a link to a folder is shared
// as that folder, and a link to a file keeps the link's name.
func TestSymlinkRootIsResolved(t *testing.T) {
	dist := t.TempDir()
	write(t, dist, "index.html", "page")
	write(t, dist, "v3.html", "report")
	links := t.TempDir()
	if err := os.Symlink(dist, filepath.Join(links, "site")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(dist, "v3.html"), filepath.Join(links, "report.html")); err != nil {
		t.Fatal(err)
	}
	c, state := newUploadClient(t)
	if _, err := c.Share(context.Background(), &Source{Path: filepath.Join(links, "site")}, PublishOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if names := sortedKeys(state.lastFiles()); fmt.Sprint(names) != "[index.html v3.html]" {
		t.Fatalf("folder link shared %v", names)
	}
	if _, err := c.Update(context.Background(), "art", &Source{Path: filepath.Join(links, "report.html")}, nil); err != nil {
		t.Fatal(err)
	}
	if files := state.lastFiles(); files["report.html"] != "report" {
		t.Fatalf("file link published %v", files)
	}
}

// An edit to bytes that are sent again after a failed chunk is caught by
// UploadFile, not left for the server's checksum to reject.
func TestUploadFileResumedChunkEdit(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(fmt.Sprint("edit=", edit), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "data.bin")
			write(t, filepath.Dir(file), "data.bin", "0123456789")
			var mu sync.Mutex
			var got []byte
			failed := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case "POST":
					json.NewEncoder(w).Encode(api.Upload{ID: "u", ChunkSize: 1 << 20})
				case "PUT":
					data, _ := io.ReadAll(r.Body)
					if !failed {
						// Keep half of the first attempt, then fail it.
						failed = true
						got = append(got, data[:5]...)
						if edit {
							if err := os.WriteFile(file, []byte("012345678X"), 0600); err != nil {
								t.Error(err)
							}
						}
						http.Error(w, `{"status":500,"detail":"try again"}`, 500)
						return
					}
					got = append(got[:len(got):len(got)], data...)
					json.NewEncoder(w).Encode(api.Upload{ID: "u", Received: int64(len(got))})
				case "GET":
					json.NewEncoder(w).Encode(api.Upload{ID: "u", Received: int64(len(got))})
				}
			}))
			defer server.Close()
			_, sum, err := New(server.URL, "").UploadFile(context.Background(), file, nil)
			if edit {
				if !errors.Is(err, ErrSourceChanged) {
					t.Fatalf("err=%v; want ErrSourceChanged", err)
				}
				return
			}
			if err != nil || sum != sumBytes(got) || string(got) != "0123456789" {
				t.Fatalf("resume: err=%v got=%q", err, got)
			}
		})
	}
}

// Rendered Markdown reports a digest in the hashTree domain: the source
// file's hash for a single file, and the tree digest of the directory (with
// the rendered snapshot standing in for the entry) for a directory.
func TestMarkdownDigestMatchesHashTree(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "docs/notes.md", "# Notes\n\n![chart](chart.png)\n")
	write(t, dir, "docs/chart.png", "png")
	write(t, dir, "docs/.notes.md.swp", "swap")

	single := filepath.Join(dir, "docs", "notes.md")
	p, err := Prepare(&Source{Path: single})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if want, _ := hashTree(single); p.Digest == "" || p.Digest != want {
		t.Fatalf("single-file Markdown digest %q; want %q", p.Digest, want)
	}

	var skipped int
	p, err = Prepare(&Source{Path: dir, Render: RenderMarkdown, Entry: "docs/notes.md", OnSkipped: func(n int) { skipped = n }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if want, _ := hashTree(dir); p.Digest == "" || p.Digest != want {
		t.Fatalf("directory Markdown digest %q; want %q", p.Digest, want)
	}
	files, err := readPack(p.File)
	if err != nil {
		t.Fatal(err)
	}
	if _, swap := files["docs/.notes.md.swp"]; swap || files["docs/chart.png"] != "png" || files["index.html"] == "" || p.skipped != 1 {
		t.Fatalf("pack=%v skipped=%d (callback %d)", sortedKeys(files), p.skipped, skipped)
	}
}

// hashTree uses the packer's walk, so a tree the packer rejects (an
// absolute symlink inside the folder) is rejected by the hash too, with
// the same usage error.
func TestHashTreeSharesPackSymlinkRules(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "page")
	outside := t.TempDir()
	write(t, outside, "secret.txt", "outside")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "secret.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var usage *UsageError
	if _, err := hashTree(dir); !errors.As(err, &usage) {
		t.Fatalf("hashTree err=%v; want a usage error", err)
	}
	if _, err := Prepare(&Source{Path: dir}); !errors.As(err, &usage) {
		t.Fatalf("Prepare err=%v; want a usage error", err)
	}
}

// A symlinked root is resolved once, then walked with the confined rules:
// in-root relative links are followed, relative to the resolved folder.
func TestSymlinkRootIsConfinedToResolvedFolder(t *testing.T) {
	base := t.TempDir()
	site := filepath.Join(base, "site")
	write(t, site, "index.html", "page")
	write(t, site, "assets/app.css", "css")
	if err := os.Symlink(filepath.Join("assets", "app.css"), filepath.Join(site, "app.css")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(site, link); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(&Source{Path: link})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	files, err := readPack(p.File)
	if err != nil || files["app.css"] != "css" || p.Filename != "link" {
		t.Fatalf("files=%v filename=%s err=%v", sortedKeys(files), p.Filename, err)
	}
	if want, _ := hashTree(site); p.Digest != want {
		t.Fatal("symlinked root digest differs from the resolved folder's")
	}
}

// An Emacs lock is a dangling symlink. The skip rule runs before the walk
// follows it, so it is skipped and counted, without a broken-symlink
// warning, and hashTree agrees with the pack.
func TestSkipRuleSkipsEmacsLockSymlink(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "page")
	if err := os.Symlink("user@host.1234:1700000000", filepath.Join(dir, ".#index.html")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	assertSkippedPack(t, dir, 1)
}

// A node_modules symlink out of the root is skipped without being followed,
// so the upload does not fail on the escaping link.
func TestSkipRuleSkipsEscapingSkipNamedSymlink(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "page")
	shared := t.TempDir()
	write(t, shared, "node_modules/pkg/index.js", "module")
	if err := os.Symlink(filepath.Join(shared, "node_modules"), filepath.Join(dir, "node_modules")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	assertSkippedPack(t, dir, 0)
}

// A .git file (a worktree or submodule link naming a local path) is skipped
// and counted.
func TestSkipRuleSkipsGitFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "page")
	write(t, dir, ".git", "gitdir: /home/user/repo/.git/worktrees/site\n")
	assertSkippedPack(t, dir, 1)
}

// A symlinked directory whose name matches the file rule ("drafts~") is
// skipped without being followed, and counted.
func TestSkipRuleSkipsSkipNamedDirectorySymlink(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "page")
	write(t, dir, "real/page.html", "draft")
	if err := os.Symlink("real", filepath.Join(dir, "drafts~")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := assertSkippedPackFiles(t, dir, 1, "[index.html real/page.html]")
	p.Close()
}

// assertSkippedPack prepares dir and requires a pack of only index.html,
// the given skipped count, no warnings, and a digest equal to hashTree.
func assertSkippedPack(t *testing.T, dir string, skipped int) {
	t.Helper()
	assertSkippedPackFiles(t, dir, skipped, "[index.html]").Close()
}

func assertSkippedPackFiles(t *testing.T, dir string, skipped int, want string) *Prepared {
	t.Helper()
	var warnings []string
	p, err := Prepare(&Source{Path: dir, Warn: func(w string) { warnings = append(warnings, w) }})
	if err != nil {
		t.Fatal(err)
	}
	files, err := readPack(p.File)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sortedKeys(files)) != want || len(warnings) != 0 || p.skipped != skipped {
		t.Fatalf("files=%v warnings=%v skipped=%d; want %s, no warnings, %d skipped", sortedKeys(files), warnings, p.skipped, want, skipped)
	}
	if digest, err := hashTree(dir); err != nil || p.Digest != digest {
		t.Fatalf("hashTree=%s err=%v; pack digest %s", digest, err, p.Digest)
	}
	return p
}

// A skipped temp file that vanishes after its directory was listed is not a
// source change: the walk never stats it. The delete runs from ".a.html",
// which sorts before every temp name, so each one is gone when reached.
func TestSkippedFileVanishingMidWalk(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".a.html", "page")
	temps := []string{".goutputstream-ABC123", "z~", "z.html___jb_tmp___"}
	for _, name := range temps {
		write(t, dir, name, "temp")
	}
	p, err := Prepare(&Source{Path: dir, beforeRead: func(rel string) {
		if rel == ".a.html" {
			for _, name := range temps {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Error(err)
				}
			}
		}
	}})
	if err != nil {
		t.Fatalf("vanished skipped files failed the pack: %v", err)
	}
	p.Close()
}

// A Markdown --entry the walk would skip is rejected, since the watch
// digest and event filter could never see it.
func TestMarkdownEntryMustNotBeSkipped(t *testing.T) {
	dir := t.TempDir()
	for _, entry := range []string{"notes~", ".#notes.md", "node_modules/readme.md", "docs/.git"} {
		write(t, dir, entry, "# Notes\n")
		_, err := Prepare(&Source{Path: dir, Render: RenderMarkdown, Entry: entry})
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "skip rule") {
			t.Errorf("entry %s: err=%v; want a skip-rule usage error", entry, err)
		}
	}
}

// A single source that Prepare stat'ed and that is gone when packSingle or
// prepareMarkdown opens it was deleted in between: a source change that
// share retries, not a usage error (exit 2).
func TestStatedSourceVanishedIsSourceChange(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"packSingle", "page.txt"},
		{"prepareMarkdown", "notes.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.file, "# content\n")
			src := &Source{Path: filepath.Join(dir, tc.file), beforeRead: func(string) {
				if err := os.Remove(filepath.Join(dir, tc.file)); err != nil {
					t.Error(err)
				}
			}}
			if tc.name == "packSingle" {
				src.Kind = api.KindBundle
			}
			_, err := Prepare(src)
			var usage *UsageError
			if !errors.Is(err, ErrSourceChanged) || errors.As(err, &usage) {
				t.Fatalf("err=%v; want ErrSourceChanged without a usage error", err)
			}
		})
	}
}

// --entry is checked with the walk's own decision for each component: a
// symlinked directory named by the file rule ("lnk~") is skipped by the
// walk, so an entry through it is rejected.
func TestMarkdownEntryThroughSkippedSymlink(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "real/index.md", "# Notes\n")
	write(t, dir, "other.txt", "x")
	if err := os.Symlink("real", filepath.Join(dir, "lnk~")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := Prepare(&Source{Path: dir, Render: RenderMarkdown, Entry: "lnk~/index.md"})
	var usage *UsageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "skip rule") {
		t.Fatalf("err=%v; want a skip-rule usage error", err)
	}
	p, err := Prepare(&Source{Path: dir, Render: RenderMarkdown, Entry: "real/index.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if digest, _ := hashTree(dir); p.Digest != digest {
		t.Fatal("digest through the real directory differs from hashTree")
	}
}

// A walk that starts below the root (watch registration of a new
// directory) applies the skip rule to its start, by the start's own type.
func TestWalkStartBelowRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "sub/deeper/page.html", "x")
	write(t, dir, "sub/node_modules/pkg/x.js", "x")
	write(t, dir, "drafts~/page.html", "x")
	write(t, dir, "node_modules/pkg/x.js", "x")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for start, want := range map[string]string{
		"sub":          "[sub sub/deeper]",
		"drafts~":      "[drafts~]",
		"node_modules": "[]",
	} {
		var dirs []string
		walk := &bundleWalk{fsys: root.FS(), dir: func(name string) error { dirs = append(dirs, name); return nil }}
		if err := walk.run(start); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(dirs) != want {
			t.Errorf("start %s: dirs=%v; want %s", start, dirs, want)
		}
	}
}
