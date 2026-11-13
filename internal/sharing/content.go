package sharing

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/store"
)

// ReservedPrefix is the URL path space used by gate pages on artifact hosts.
const ReservedPrefix = "__redrop"

func init() {
	// Distroless images ship no /etc/mime.types; register what artifacts use.
	for ext, typ := range map[string]string{
		".md": "text/markdown; charset=utf-8", ".markdown": "text/markdown; charset=utf-8",
		".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
		".csv": "text/csv; charset=utf-8", ".tsv": "text/tab-separated-values; charset=utf-8",
		".yaml": "text/yaml; charset=utf-8", ".yml": "text/yaml; charset=utf-8", ".toml": "text/plain; charset=utf-8",
		".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8", ".cjs": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
		".json": "application/json", ".map": "application/json", ".webmanifest": "application/manifest+json",
		".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
		".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/mp4", ".flac": "audio/flac",
		".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
		".ico": "image/x-icon", ".avif": "image/avif", ".webp": "image/webp", ".svg": "image/svg+xml",
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
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

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// htmlTitle extracts the <title> of an HTML document head.
func htmlTitle(b []byte) string {
	m := titleRE.FindSubmatch(b)
	if m == nil {
		return ""
	}
	return truncate(strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " "), 200)
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

// sanitizeFilename reduces a client-supplied name to a safe single path element.
func sanitizeFilename(name string) string {
	name = path.Base(strings.TrimSpace(strings.ReplaceAll(name, "\\", "/")))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" || name == ".." || strings.HasPrefix(name, ReservedPrefix) {
		return "file"
	}
	return truncate(name, 200)
}

// CleanBundlePath validates and normalizes a path from a bundle archive.
// It returns "" for entries that should be skipped (the archive root).
func CleanBundlePath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("absolute or invalid path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path escapes the bundle: %q", p)
		}
	}
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" || c == "." {
		return "", nil
	}
	if c == ReservedPrefix || strings.HasPrefix(c, ReservedPrefix+"/") {
		return "", fmt.Errorf("path %q is reserved", p)
	}
	if len(c) > 1024 || !utf8.ValidString(c) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return c, nil
}

// headCapture keeps the first max bytes written (for type sniffing).
type headCapture struct {
	buf bytes.Buffer
	max int
}

func (h *headCapture) Write(p []byte) (int, error) {
	if room := h.max - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// extractBundle unpacks a tar.gz into blobs, enforcing file-count and
// decompressed-size limits and rejecting links and special files.
func (s *Service) extractBundle(r io.Reader) ([]store.File, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, apperr.Invalid("bundle must be a .tar.gz archive")
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
			return nil, apperr.Invalid("corrupt bundle archive")
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeReg:
		default:
			return nil, apperr.Invalid("bundle may only contain regular files (found a link or special file: %q)", hdr.Name)
		}
		p, err := CleanBundlePath(hdr.Name)
		if err != nil {
			return nil, apperr.Invalid("%s", err.Error())
		}
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if len(files) >= s.cfg.MaxFiles {
			return nil, apperr.Invalid("bundle has more than %d files", s.cfg.MaxFiles)
		}
		head := &headCapture{max: 512}
		sum, size, err := s.blobs.Put(io.TeeReader(tr, head), s.cfg.MaxVersionBytes-total)
		if err != nil {
			if errors.Is(err, blob.ErrTooLarge) {
				return nil, apperr.TooLarge("bundle exceeds the %s per-version limit when unpacked", humanBytes(s.cfg.MaxVersionBytes))
			}
			return nil, apperr.Internal(err)
		}
		total += size
		files = append(files, store.File{Path: p, Blob: sum, Size: size, ContentType: contentTypeFor(p, head.buf.Bytes())})
	}
	if len(files) == 0 {
		return nil, apperr.Invalid("bundle is empty")
	}
	return files, nil
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
