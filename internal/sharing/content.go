package sharing

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/blob"
	"github.com/mrRedSun/redrop/internal/contenttype"
	"github.com/mrRedSun/redrop/internal/e2e"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

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
	if name == "" || name == "." || name == "/" || name == ".." || strings.HasPrefix(name, api.ReservedPrefix) {
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
	if c == api.ReservedPrefix || strings.HasPrefix(c, api.ReservedPrefix+"/") {
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
// decompressed-size limits and rejecting links and special files. With
// encrypted set, every file must be an end-to-end object (by its magic), is
// stored as ciphertext, and the manifest doesn't count against MaxFiles.
func (s *Service) extractBundle(r io.Reader, attempt *publication, encrypted bool) ([]store.File, error) {
	maxFiles := s.cfg.MaxFiles
	if encrypted {
		maxFiles++
	}
	ctx := attempt.ctx
	gz, err := gzip.NewReader(contextReader{ctx: ctx, r: r})
	if err != nil {
		if ctx.Err() != nil {
			return nil, apperr.Internal(ctx.Err())
		}
		return nil, apperr.Invalid("bundle must be a .tar.gz archive")
	}
	defer gz.Close()
	work := &archiveWorkReader{r: contextReader{ctx: ctx, r: gz}, remaining: s.archiveReadLimit(), maxEntries: s.archiveEntryLimit()}
	tr := tar.NewReader(work)
	var files []store.File
	seen := map[string]bool{} // regular-file paths only, bounded by MaxFiles
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, apperr.Internal(ctx.Err())
			}
			var app *apperr.Error
			if errors.As(err, &app) {
				return nil, err
			}
			return nil, apperr.Invalid("corrupt bundle archive")
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeXGlobalHeader:
		default:
			return nil, apperr.Invalid("bundle may only contain regular files (found a link or special file: %q)", hdr.Name)
		}
		if !work.inSync(hdr) {
			// The entry cap counts headers only while the counter agrees
			// with archive/tar on where each one starts.
			return nil, apperr.Invalid("malformed bundle archive: inconsistent entry framing")
		}
		if hdr.Typeflag != tar.TypeReg {
			// As on main, directory and global metadata entries carry no
			// content, and their names are neither validated nor retained.
			// The work reader has counted them.
			continue
		}
		p, err := CleanBundlePath(hdr.Name)
		if err != nil {
			return nil, apperr.Invalid("%s", err.Error())
		}
		if p == "" {
			return nil, apperr.Invalid("regular bundle file has an empty path")
		}
		if seen[p] {
			return nil, apperr.Invalid("duplicate normalized bundle path %q", p)
		}
		if len(files) >= maxFiles {
			return nil, apperr.Invalid("bundle has more than %d files", s.cfg.MaxFiles)
		}
		seen[p] = true
		if hdr.Size < 0 || hdr.Size > s.cfg.MaxVersionBytes-total {
			return nil, apperr.TooLarge("bundle exceeds the %s per-version limit when unpacked", humanBytes(s.cfg.MaxVersionBytes))
		}
		head := &headCapture{max: 512}
		sum, size, err := attempt.put(io.TeeReader(archiveBody{tr}, head), hdr.Size)
		if err != nil {
			if errors.Is(err, blob.ErrTooLarge) {
				return nil, apperr.TooLarge("bundle exceeds the %s per-version limit when unpacked", humanBytes(s.cfg.MaxVersionBytes))
			}
			return nil, s.blobErr(err)
		}
		total += size
		ct := contenttype.For(p, head.buf.Bytes())
		if encrypted {
			if !e2e.HasMagic(head.buf.Bytes()) {
				return nil, apperr.Invalid("%q is not end-to-end encrypted content", p)
			}
			ct = e2eContentType
		}
		files = append(files, store.File{Path: p, Blob: sum, Size: size, ContentType: ct})
	}
	if p := fileDirectoryCollision(files); p != "" {
		return nil, apperr.Invalid("bundle path %q is both a file and a directory", p)
	}
	if len(files) == 0 {
		return nil, apperr.Invalid("bundle is empty")
	}
	return files, nil
}

// archiveBody reads one entry's body from the tar reader. A read error there
// is the archive's fault: a truncated or corrupt compressed stream, or a body
// shorter than its header says. It becomes the same client error as a corrupt
// header, so the publication releases its upload. Application errors (the
// work budget) and cancellation pass through unchanged. Write-side blob errors
// never pass through here and stay internal.
type archiveBody struct{ r io.Reader }

func (b archiveBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == nil || err == io.EOF {
		return n, err
	}
	var app *apperr.Error
	if errors.As(err, &app) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return n, err
	}
	return n, apperr.Invalid("corrupt bundle archive")
}

// fileDirectoryCollision returns a file path that is also a parent directory
// of another file, or "". Directory entries are not retained, so the files'
// own paths are the only evidence. Sorting keeps this O(n log n).
func fileDirectoryCollision(files []store.File) string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	sort.Strings(paths)
	for _, p := range paths {
		// Every path below p+"/" sorts at or after it; the first one decides.
		if i := sort.SearchStrings(paths, p+"/"); i < len(paths) && strings.HasPrefix(paths[i], p+"/") {
			return p
		}
	}
	return ""
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
