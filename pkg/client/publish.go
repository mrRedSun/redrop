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
	// OnSkipped is called once, after a directory upload that excluded n > 0
	// file entries. Excluded directory subtrees are not traversed or
	// counted as files.
	OnSkipped func(n int)
}

// Prepared is a source ready to upload: the original file itself, or a
// temporary spool or pack that Close removes.
type Prepared struct {
	File     string // path to upload
	Kind     string
	Filename string
	// Digest is in the same domain as hashTree: sorted path/file hashes
	// for a directory, or the source bytes' hash for an explicit file,
	// computed as those bytes were read into the pack. It is empty when
	// File is uploaded as is; the upload checksum, which UploadFile checks
	// against the bytes sent, is then the digest.
	Digest  string
	skipped int // file entries the folder walk excluded
	cleanup func()
}

func (p *Prepared) Close() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

// ErrEmptyDirectory reports a folder with no publishable files.
var ErrEmptyDirectory = errors.New("folder has no files to publish")

// sourceChanged marks errors that mean the tree changed under a read that
// had already listed it: an entry that vanished, or a file that ended
// before its listed size. Other errors pass through unchanged.
func sourceChanged(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %w", ErrSourceChanged, err)
	}
	return err
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

// Prepare makes a Source upload-ready. A single file that is uploaded as
// is (html, file, or a .tar.gz bundle) is not copied: UploadFile detects a
// mid-upload edit. Directories and other bundles are packed into a
// temporary tar.gz, and stdin is spooled to a temporary file. Errors from
// a tree that changes while it is read wrap ErrSourceChanged.
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
	if src.Reader != nil {
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

	if src.Reader != nil {
		// A stream cannot be re-read for a resumed chunk: spool it.
		spool, err := newTemp()
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(spool, src.Reader)
		if cerr := spool.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(err)
		}
		srcPath = spool.Name()
	}
	lower := strings.ToLower(name)
	if !isDir && (kind != api.KindBundle || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")) {
		p.File = srcPath
		return p, nil
	}

	packed, err := newTemp()
	if err != nil {
		return fail(err)
	}
	switch {
	case isDir:
		p.Digest, p.skipped, err = packDir(srcPath, packed)
	case strings.HasSuffix(lower, ".zip"):
		p.Digest, err = zipToTarGz(srcPath, packed)
	default:
		entry := name
		if strings.EqualFold(path.Ext(name), ".html") || strings.EqualFold(path.Ext(name), ".htm") {
			entry = "index.html"
		}
		p.Digest, err = packSingle(srcPath, entry, packed)
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
// (vim swaps .swp down to .swa plus .swx on dot-prefixed names,
// JetBrains ___jb_tmp___/___jb_old___, gedit .goutputstream-*, *~
// backups, emacs .# lock files and #…# autosave), which never belong in a
// published artifact. Applied only to entries inside a directory: an
// explicitly named single file is always published.
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

// vimSwapName matches vim's swap-file names for dotfiles: .file.swp,
// .file.swo, … down to .file.swa (vim counts the last letter backwards),
// plus .swx. Only dot-prefixed names are matched, and only the .sw range,
// so "site.swp" or hidden ".logo.svg" and ".schema.sql" are never dropped.
func vimSwapName(lower string) bool {
	if !strings.HasPrefix(lower, ".") || len(lower) < len(".x.swp") {
		return false
	}
	ext := lower[len(lower)-4:]
	return strings.HasPrefix(ext, ".sw") && (ext[3] >= 'a' && ext[3] <= 'p' || ext[3] == 'x')
}

// walkTestHook, when set by a test, runs between a file's stat and its
// read, to reproduce a tree that changes mid-walk. Always nil otherwise.
var walkTestHook func(rel string)

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
		if walkTestHook != nil {
			walkTestHook(rel)
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
		return "", 0, sourceChanged(err)
	}
	if len(db.entries) == 0 {
		return "", 0, fmt.Errorf("%s: %w", dir, ErrEmptyDirectory)
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

// packSingle packs one file as a bundle and returns the SHA-256 of the
// source bytes packed.
func packSingle(src, name string, w io.Writer) (string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	sum, err := addFile(tw, src, name, st.Size())
	if err != nil {
		return "", sourceChanged(err)
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return sum, gz.Close()
}

// zipToTarGz converts a zip to a tar.gz bundle and returns the zip's
// SHA-256. Zip reads are random access, so the file is hashed before and
// after the conversion; a difference means it changed in between.
func zipToTarGz(src string, w io.Writer) (string, error) {
	before, err := hashFile(src)
	if err != nil {
		return "", err
	}
	if err := convertZip(src, w); err != nil {
		return "", err
	}
	after, err := hashFile(src)
	if err != nil {
		return "", sourceChanged(err)
	}
	if after != before {
		return "", fmt.Errorf("%w: %s was modified while converting it", ErrSourceChanged, src)
	}
	return before, nil
}

func convertZip(src string, w io.Writer) error {
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

// Share prepares, uploads, and publishes a new artifact. If the source
// changes while it is read, it is prepared and uploaded once more.
func (c *Client) Share(ctx context.Context, src *Source, opts PublishOptions, progress Progress) (*api.Artifact, error) {
	u, err := retrySourceChange(func() (*upload, error) { return c.prepareAndUpload(ctx, src, progress) })
	if err != nil {
		return nil, err
	}
	return c.CreateArtifact(ctx, &api.CreateArtifactRequest{
		PublishRequest: u.request(src),
		Title:          opts.Title, TTL: opts.TTL, Password: opts.Password, MaxViews: opts.MaxViews,
	})
}

// Update uploads src as a new version of an existing artifact (same link).
// If the source changes while it is read, it is prepared and uploaded once
// more.
func (c *Client) Update(ctx context.Context, artifactID string, src *Source, progress Progress) (*api.Artifact, error) {
	u, err := retrySourceChange(func() (*upload, error) { return c.prepareAndUpload(ctx, src, progress) })
	if err != nil {
		return nil, err
	}
	req := u.request(src)
	return c.CreateVersion(ctx, artifactID, &req)
}

// UpdateDigest is a single Update attempt that also returns a content
// digest in the same domain as hashTree: for folder bundles, the
// `path sha` digest of what was actually packed; for an explicit file,
// the SHA-256 of its bytes as read (including --as bundle and archives).
// It does not retry: an error wrapping ErrSourceChanged means the source
// changed while it was read, and the caller decides when to try again.
func (c *Client) UpdateDigest(ctx context.Context, artifactID string, src *Source, progress Progress) (string, *api.Artifact, error) {
	u, err := c.prepareAndUpload(ctx, src, progress)
	if err != nil {
		return "", nil, err
	}
	req := u.request(src)
	a, err := c.CreateVersion(ctx, artifactID, &req)
	return u.digest, a, err
}

// upload is a prepared source after a successful UploadFile.
type upload struct {
	id, blobSum, digest, kind, filename string
}

func (u *upload) request(src *Source) api.PublishRequest {
	return api.PublishRequest{UploadID: u.id, SHA256: u.blobSum, Kind: u.kind, Filename: u.filename, Entry: src.Entry, SPA: src.SPA, Note: src.Note}
}

// prepareAndUpload is the shared Prepare + UploadFile step. It reports
// excluded folder entries through src.OnSkipped, once per successful
// upload.
func (c *Client) prepareAndUpload(ctx context.Context, src *Source, progress Progress) (*upload, error) {
	p, err := Prepare(src)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	id, blobSum, err := c.UploadFile(ctx, p.File, progress)
	if err != nil {
		return nil, err
	}
	if p.skipped > 0 && src.OnSkipped != nil {
		src.OnSkipped(p.skipped)
	}
	digest := p.Digest
	if digest == "" {
		digest = blobSum // File was the source itself, checked as sent
	}
	return &upload{id: id, blobSum: blobSum, digest: digest, kind: p.Kind, filename: p.Filename}, nil
}

// retrySourceChange runs step again once if the source changed while it
// was read, then gives up with an error that still wraps ErrSourceChanged.
func retrySourceChange(step func() (*upload, error)) (*upload, error) {
	u, err := step()
	if errors.Is(err, ErrSourceChanged) {
		u, err = step()
		if errors.Is(err, ErrSourceChanged) {
			err = fmt.Errorf("%w (tried twice; publish again once it stops changing)", err)
		}
	}
	return u, err
}
