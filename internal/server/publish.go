package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

func init() {
	for ext, typ := range map[string]string{
		".md": "text/markdown; charset=utf-8", ".markdown": "text/markdown; charset=utf-8",
		".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
		".csv": "text/csv; charset=utf-8", ".tsv": "text/tab-separated-values; charset=utf-8",
		".yaml": "text/yaml; charset=utf-8", ".yml": "text/yaml; charset=utf-8", ".toml": "text/plain; charset=utf-8",
		".mjs": "text/javascript; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".cjs": "text/javascript; charset=utf-8",
		".map": "application/json", ".json": "application/json", ".webmanifest": "application/manifest+json",
		".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".m4v": "video/mp4",
		".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/mp4", ".flac": "audio/flac",
		".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
		".ico": "image/x-icon", ".avif": "image/avif", ".webp": "image/webp", ".svg": "image/svg+xml",
		".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar", ".7z": "application/x-7z-compressed",
		".pdf": "application/pdf", ".wasm": "application/wasm", ".xml": "application/xml",
		".py": "text/plain; charset=utf-8", ".go": "text/plain; charset=utf-8", ".ts": "text/plain; charset=utf-8",
		".tsx": "text/plain; charset=utf-8", ".sh": "text/plain; charset=utf-8", ".rs": "text/plain; charset=utf-8",
	} {
		mime.AddExtensionType(ext, typ)
	}
}

// contentTypeFor picks a type by extension, falling back to sniffing.
func contentTypeFor(name string, head []byte) string {
	t := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if t == "" {
		t = http.DetectContentType(head)
	}
	if (strings.HasPrefix(t, "text/") || t == "application/json" || t == "image/svg+xml") && !strings.Contains(t, "charset") {
		t += "; charset=utf-8"
	}
	return t
}

func (s *Server) stagedPath(id string) string { return filepath.Join(s.uploadDir, id) }

var errBadUpload = errors.New("bad upload")

type uploadErr struct{ msg string }

func (e uploadErr) Error() string { return e.msg }

func badUpload(format string, a ...any) error { return errBadRequest(fmt.Sprintf(format, a...)) }

// materialize turns a completed upload into stored files for a new version.
func (s *Server) materialize(ctx context.Context, userID string, req *api.PublishRequest) (*store.NewVersion, string, error) {
	up, err := s.db.UploadByID(ctx, userID, req.UploadID)
	if err != nil {
		return nil, "", errNotFound("upload not found or expired")
	}
	if up.Received != up.Size {
		return nil, "", errConflict(fmt.Sprintf("upload incomplete: %d of %d bytes received", up.Received, up.Size))
	}
	f, err := os.Open(s.stagedPath(up.ID))
	if err != nil {
		return nil, "", fail(ctx, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, "", fail(ctx, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, req.SHA256) {
		return nil, "", errBadRequest("sha256 mismatch: upload corrupted; retry")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, "", fail(ctx, err)
	}

	v := &store.NewVersion{Kind: req.Kind, SPA: req.SPA, Note: truncate(req.Note, 500)}
	var title string
	switch req.Kind {
	case api.KindHTML:
		head := make([]byte, 64<<10)
		n, _ := io.ReadFull(f, head)
		head = head[:n]
		f.Seek(0, io.SeekStart)
		sum, size, err := s.blobs.Put(f, s.cfg.MaxVersionBytes)
		if err != nil {
			return nil, "", s.blobErr(ctx, err)
		}
		v.Entry = "index.html"
		v.Files = []store.File{{Path: "index.html", Blob: sum, Size: size, ContentType: "text/html; charset=utf-8"}}
		title = htmlTitle(head)
	case api.KindFile:
		name := sanitizeFilename(req.Filename)
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		f.Seek(0, io.SeekStart)
		sum, size, err := s.blobs.Put(f, s.cfg.MaxVersionBytes)
		if err != nil {
			return nil, "", s.blobErr(ctx, err)
		}
		v.Entry = name
		v.Files = []store.File{{Path: name, Blob: sum, Size: size, ContentType: contentTypeFor(name, head[:n])}}
		title = name
	case api.KindBundle:
		files, err := s.extractBundle(f)
		if err != nil {
			return nil, "", err
		}
		v.Files = files
		entry := strings.TrimPrefix(path.Clean("/"+req.Entry), "/")
		if req.Entry == "" {
			entry = "index.html"
		}
		found := false
		for _, fl := range files {
			if fl.Path == entry {
				found = true
				break
			}
		}
		if found {
			v.Entry = entry
			if strings.HasSuffix(entry, ".html") || strings.HasSuffix(entry, ".htm") {
				for _, fl := range files {
					if fl.Path == entry {
						title = s.blobTitle(fl.Blob)
					}
				}
			}
		} else if req.Entry != "" {
			return nil, "", errBadRequest("entry file " + entry + " is not in the bundle")
		}
		if title == "" {
			title = strings.TrimSuffix(sanitizeFilename(req.Filename), ".tar.gz")
		}
	default:
		return nil, "", errBadRequest("kind must be html, file, or bundle")
	}
	f.Close()
	os.Remove(s.stagedPath(up.ID))
	s.db.DeleteUpload(ctx, up.ID)
	return v, title, nil
}

func (s *Server) blobErr(ctx context.Context, err error) error {
	if errors.Is(err, blob.ErrTooLarge) {
		return errStatus(http.StatusRequestEntityTooLarge, fmt.Sprintf("content exceeds the %s per-version limit", humanBytes(s.cfg.MaxVersionBytes)))
	}
	return fail(ctx, err)
}

func (s *Server) blobTitle(sum string) string {
	f, err := s.blobs.Open(sum)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	return htmlTitle(head[:n])
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func htmlTitle(b []byte) string {
	m := titleRE.FindSubmatch(b)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	return truncate(t, 200)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" || name == ".." || strings.HasPrefix(name, "__redrop") {
		name = "file"
	}
	return truncate(name, 200)
}

// cleanBundlePath validates a path from a bundle archive.
func cleanBundlePath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return "", fmt.Errorf("absolute or invalid path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path escapes bundle: %q", p)
		}
	}
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" || c == "." {
		return "", nil
	}
	if c == "__redrop" || strings.HasPrefix(c, "__redrop/") {
		return "", fmt.Errorf("path %q is reserved", p)
	}
	if len(c) > 1024 || !utf8.ValidString(c) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return c, nil
}

func (s *Server) extractBundle(r io.Reader) ([]store.File, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, errBadRequest("bundle must be a .tar.gz archive")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files []store.File
	seen := map[string]bool{}
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errBadRequest("corrupt bundle archive")
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return nil, badUpload("bundle may only contain regular files (found link or special file %q)", hdr.Name)
		}
		p, err := cleanBundlePath(hdr.Name)
		if err != nil {
			return nil, errBadRequest(err.Error())
		}
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if len(files) >= s.cfg.MaxFiles {
			return nil, badUpload("bundle has more than %d files", s.cfg.MaxFiles)
		}
		remaining := s.cfg.MaxVersionBytes - total
		var head bytes.Buffer
		sum, size, err := s.blobs.Put(io.TeeReader(io.LimitReader(tr, 1<<62), &limitedBuffer{buf: &head, max: 512}), remaining)
		if err != nil {
			if errors.Is(err, blob.ErrTooLarge) {
				return nil, errStatus(http.StatusRequestEntityTooLarge, fmt.Sprintf("bundle exceeds the %s per-version limit when unpacked", humanBytes(s.cfg.MaxVersionBytes)))
			}
			return nil, err
		}
		total += size
		files = append(files, store.File{Path: p, Blob: sum, Size: size, ContentType: contentTypeFor(p, head.Bytes())})
	}
	if len(files) == 0 {
		return nil, errBadRequest("bundle is empty")
	}
	return files, nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.buf.Write(p[:room])
	}
	return len(p), nil
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
