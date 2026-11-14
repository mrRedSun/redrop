package client

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mrRedSun/redrop/pkg/api"
)

// Source describes what to publish.
type Source struct {
	// Path is a file or directory. Use Reader for in-memory/stdin content.
	Path string
	// Reader supplies content when Path is empty; Name is then required.
	Reader io.Reader
	// Name overrides the file name (required with Reader).
	Name string
	// Kind forces html, file, or bundle; empty auto-detects.
	Kind string
	// Entry is the start page inside a bundle (default index.html).
	Entry string
	SPA   bool
	Note  string
	// OnSkipped reports excluded file entries when preparing a directory.
	// Excluded directory subtrees are not traversed or counted as files.
	OnSkipped func(int)
}

// Prepared is a source packed into a single upload-ready file.
type Prepared struct {
	File     string // path to upload
	Kind     string
	Filename string
	// Digest is in the same domain as hashTree: sorted path/file hashes
	// for a directory, or the source bytes' hash for an explicit file.
	// It is computed as those bytes are read into the snapshot/pack.
	Digest string
	// Skipped is the number of file entries the folder walk excluded
	// (editor/OS temp files); 0 for single-file sources.
	Skipped int
	cleanup func()
}

func (p *Prepared) Close() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

// DetectKind chooses html for .html/.htm files, bundle for directories and
// .zip/.tar.gz archives only when forced, and file otherwise.
func DetectKind(name string, isDir bool) string {
	if isDir {
		return api.KindBundle
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".htm":
		return api.KindHTML
	}
	return api.KindFile
}

// Prepare snapshots a Source into upload-ready temporary files. File
// content is immutable throughout hashing and upload, including when the
// original path is overwritten by an editor during an upload.
func Prepare(src *Source) (*Prepared, error) {
	p := &Prepared{}
	var tmpFiles []string
	p.cleanup = func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
	}
	fail := func(err error) (*Prepared, error) { p.Close(); return nil, err }
	newTemp := func() (*os.File, error) {
		f, err := os.CreateTemp("", "redrop-source-*")
		if err == nil {
			tmpFiles = append(tmpFiles, f.Name())
		}
		return f, err
	}

	srcPath, name := src.Path, src.Name
	isDir := false
	var input io.Reader = src.Reader
	if input != nil {
		if name == "" {
			return nil, errors.New("a file name is required when reading from stdin (use --name)")
		}
	} else {
		st, err := os.Stat(srcPath)
		if err != nil {
			return nil, err
		}
		isDir = st.IsDir()
		if !isDir && !st.Mode().IsRegular() {
			return nil, errors.New("source must be a regular file or directory")
		}
		if name == "" {
			abs, err := filepath.Abs(srcPath)
			if err != nil {
				return nil, err
			}
			name = filepath.Base(abs)
		}
	}
	kind := src.Kind
	if kind == "" {
		kind = DetectKind(name, isDir)
	}
	if kind != api.KindHTML && kind != api.KindFile && kind != api.KindBundle {
		return nil, fmt.Errorf("unknown kind %q (use html, file, or bundle)", kind)
	}
	if isDir && kind != api.KindBundle {
		return nil, fmt.Errorf("%s is a directory; share it as a bundle", srcPath)
	}
	p.Kind, p.Filename = kind, name

	if !isDir {
		if input == nil {
			f, err := os.Open(srcPath)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			input = f
		}
		snapshot, err := newTemp()
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		_, err = io.Copy(snapshot, io.TeeReader(input, h))
		if cerr := snapshot.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(err)
		}
		srcPath = snapshot.Name()
		p.File, p.Digest = srcPath, hex.EncodeToString(h.Sum(nil))
		lower := strings.ToLower(name)
		if kind != api.KindBundle || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
			return p, nil
		}
	}

	packed, err := newTemp()
	if err != nil {
		return fail(err)
	}
	switch {
	case isDir:
		p.Digest, p.Skipped, err = packDir(srcPath, packed)
	case strings.HasSuffix(strings.ToLower(name), ".zip"):
		err = zipToTarGz(srcPath, packed)
	default:
		entry := name
		if strings.EqualFold(path.Ext(name), ".html") || strings.EqualFold(path.Ext(name), ".htm") {
			entry = "index.html"
		}
		err = packSingle(srcPath, entry, packed)
	}
	if cerr := packed.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	p.File = packed.Name()
	return p, nil
}

func skipEntry(rel string, d fs.DirEntry) bool {
	return skipEntryName(d.Name(), d.IsDir())
}

// skipEntryName is the single skip rule for publishable content: the
// packer, the watch content digest, and the watch event filter all consult
// it, so they can never disagree about what counts. Directories: VCS and
// dependency caches. Files: OS junk and editor temp/swap/backup files
// (vim's swap range .swp through .saa plus .swx on dot-prefixed names,
// JetBrains ___jb_tmp___/___jb_old___, gedit .goutputstream-*, *~
// backups, emacs .# lock files and #…# autosave), which never belong in a
// published artifact. Applied only to entries inside a directory: an
// explicitly named single file is always published (m5).
func skipEntryName(base string, isDir bool) bool {
	if isDir {
		switch base {
		case ".git", ".svn", ".hg", "node_modules", "__pycache__", ".DS_Store":
			return true
		}
		return false
	}
	lower := strings.ToLower(base)
	switch {
	case lower == ".ds_store" || lower == "thumbs.db":
		return true
	case strings.HasPrefix(lower, ".goutputstream-"): // gedit temp
		return true
	case strings.HasSuffix(lower, "___jb_tmp___"), // JetBrains safe-write
		strings.HasSuffix(lower, "___jb_old___"):
		return true
	case strings.HasPrefix(base, ".#"): // emacs lock files
		return true
	case strings.HasPrefix(base, "#") && strings.HasSuffix(base, "#") && len(base) > 2: // emacs autosave
		return true
	case strings.HasSuffix(lower, "~"): // editor backups
		return true
	case vimSwapName(lower): // vim swap files on dot files
		return true
	}
	return false
}

// vimSwapName matches vim's hidden swap-file names for dotfiles:
// .file.swp, .file.swo, … down to .file.saa (vim counts the last letter
// backwards), plus the rarer .swx. Only dot-prefixed names are matched so
// a legitimate "site.map" style name is never dropped.
func vimSwapName(lower string) bool {
	if !strings.HasPrefix(lower, ".") {
		return false
	}
	dot := strings.LastIndex(lower, ".")
	if dot <= 0 || dot == len(lower)-1 {
		return false
	}
	ext := lower[dot+1:]
	// ext must be exactly s[a-w][a-z] (swp…saa) or swx.
	if len(ext) != 3 || ext[0] != 's' {
		return false
	}
	return (ext[1] >= 'a' && ext[1] <= 'w') && (ext[2] >= 'a' && ext[2] <= 'z') || ext == "swx"
}

// walkPackable walks the regular files under dir that the packer packs —
// applying the shared skip rules and following symlinks to their content,
// exactly like packDir — and calls fn for each with the slash-separated
// path relative to dir, the walked path, and the followed file info; a
// non-nil return from fn aborts the walk. It is the one walk behind both
// packing and the watch content digest.
func walkPackable(dir string, fn func(rel, abs string, info fs.FileInfo) error) (skipped int, err error) {
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skipEntry(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			skipped++
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := os.Stat(p) // follow symlinks to their content
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		return fn(rel, p, info)
	})
	return skipped, err
}

// treeDigestBuilder assembles the `path sha` digest that hashTree builds,
// from hashes collected while packing, so a digest of what was packed is
// available without re-reading files.
type treeDigestBuilder struct {
	entries []digestEntry
}

type digestEntry struct{ rel, sum string }

func (b *treeDigestBuilder) add(rel, sum string) {
	b.entries = append(b.entries, digestEntry{rel, sum})
}

func (b *treeDigestBuilder) digest() string {
	sort.Slice(b.entries, func(i, j int) bool { return b.entries[i].rel < b.entries[j].rel })
	h := sha256.New()
	for _, e := range b.entries {
		fmt.Fprintf(h, "%s %s\n", e.rel, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func packDir(dir string, w io.Writer) (string, int, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	db := &treeDigestBuilder{}
	skipped, err := walkPackable(dir, func(rel, abs string, info fs.FileInfo) error {
		sum, err := addFile(tw, abs, rel, info.Size())
		if err != nil {
			return err
		}
		db.add(rel, sum)
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	return db.digest(), skipped, nil
}

// addFile streams one file into the tar and returns the SHA-256 of the
// bytes actually read, so callers can digest content while packing.
func addFile(tw *tar.Writer, src, name string, size int64) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.CopyN(tw, io.TeeReader(io.LimitReader(f, size), h), size); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func packSingle(src, name string, w io.Writer) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if _, err := addFile(tw, src, name, st.Size()); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func zipToTarGz(src string, w io.Writer) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	// Strip a single top-level folder (common for zipped sites).
	prefix := ""
	for i, f := range zr.File {
		top, _, _ := strings.Cut(strings.TrimPrefix(f.Name, "/"), "/")
		if i == 0 {
			prefix = top + "/"
		}
		if !strings.HasPrefix(strings.TrimPrefix(f.Name, "/"), prefix) {
			prefix = ""
			break
		}
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !f.Mode().IsRegular() {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(f.Name, "/"), prefix)
		if name == "" || strings.HasPrefix(path.Base(name), "._") || strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(f.UncompressedSize64), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.CopyN(tw, rc, int64(f.UncompressedSize64))
		}
		rc.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// PublishOptions are artifact settings for a new share.
type PublishOptions struct {
	Title    string
	TTL      string
	Password string
	MaxViews int
}

// Share prepares, uploads, and publishes a new artifact.
func (c *Client) Share(ctx context.Context, src *Source, opts PublishOptions, progress Progress) (*api.Artifact, error) {
	p, err := Prepare(src)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	if p.Skipped > 0 && src.OnSkipped != nil {
		src.OnSkipped(p.Skipped)
	}
	id, sum, err := c.UploadFile(ctx, p.File, progress)
	if err != nil {
		return nil, err
	}
	return c.CreateArtifact(ctx, &api.CreateArtifactRequest{
		PublishRequest: api.PublishRequest{UploadID: id, SHA256: sum, Kind: p.Kind, Filename: p.Filename, Entry: src.Entry, SPA: src.SPA, Note: src.Note},
		Title:          opts.Title, TTL: opts.TTL, Password: opts.Password, MaxViews: opts.MaxViews,
	})
}

// Update uploads src as a new version of an existing artifact (same link).
func (c *Client) Update(ctx context.Context, artifactID string, src *Source, progress Progress) (*api.Artifact, error) {
	_, a, err := c.UpdateDigest(ctx, artifactID, src, progress)
	return a, err
}

// UpdateDigest is Update and also returns a content digest in the same
// domain as hashTree: for folder bundles, the `path sha` digest of what
// was actually packed; for an explicit file, the SHA-256 of its source
// snapshot (including --as bundle and archives). The API separately gets
// UploadFile's checksum of the transport blob.
func (c *Client) UpdateDigest(ctx context.Context, artifactID string, src *Source, progress Progress) (string, *api.Artifact, error) {
	p, err := Prepare(src)
	if err != nil {
		return "", nil, err
	}
	defer p.Close()
	if p.Skipped > 0 && src.OnSkipped != nil {
		src.OnSkipped(p.Skipped)
	}
	id, blobSum, err := c.UploadFile(ctx, p.File, progress)
	if err != nil {
		return "", nil, err
	}
	a, err := c.CreateVersion(ctx, artifactID, &api.PublishRequest{UploadID: id, SHA256: blobSum, Kind: p.Kind, Filename: p.Filename, Entry: src.Entry, SPA: src.SPA, Note: src.Note})
	return p.Digest, a, err
}
