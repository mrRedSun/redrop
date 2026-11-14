package client

import (
	"archive/tar"
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
	// Render is auto (the default), markdown, or none. Auto renders .md files
	// unless Kind is explicitly set. Directories require markdown and Entry.
	Render RenderMode
	// Warn receives non-fatal publishing warnings, such as missing local assets.
	// Nil leaves warnings available only on Prepared.Warnings.
	Warn func(string)
	// Entry is the start page inside a bundle (default index.html).
	// With Render markdown on a directory, it selects the Markdown source.
	Entry string
	SPA   bool
	Note  string
	// OnSkipped is called once, after a directory upload that excluded n > 0
	// file entries. Excluded directory subtrees are not traversed or
	// counted as files.
	OnSkipped func(n int)

	// beforeRead, set only by tests, runs before a file is read for
	// packing (between a folder walk's stat and read, and between a zip's
	// first hash and its conversion), and before a single file is uploaded
	// as is (between Prepare and UploadFile), to reproduce a tree that
	// changes mid-read.
	beforeRead func(rel string)
}

// Prepared is a source ready to upload: the original file itself, or a
// temporary spool or pack that Close removes.
type Prepared struct {
	File     string // path to upload
	Kind     string
	Filename string
	Entry    string // effective entry, including generated index.html
	Warnings []string
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

func (p *Prepared) warn(message string) {
	p.Warnings = append(p.Warnings, message)
}

func (p *Prepared) reportWarnings(report func(string)) {
	if report != nil {
		for _, warning := range p.Warnings {
			report(warning)
		}
	}
}

// ErrEmptyDirectory reports a folder with no publishable files.
var ErrEmptyDirectory = errors.New("folder has no files to publish")

// sourceChanged marks errors that mean the tree changed under a read that
// had already listed it: an entry that vanished, or a file that ended
// before its listed size. Other errors pass through unchanged.
func sourceChanged(err error) error {
	if errors.Is(err, ErrSourceChanged) {
		return err
	}
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
// mid-upload edit. Directories, rendered Markdown and other bundles are
// packed into a temporary tar.gz, and stdin is spooled to a temporary
// file. Errors from a tree that changes while it is read wrap
// ErrSourceChanged.
func Prepare(src *Source) (*Prepared, error) {
	mode, err := ParseRenderMode(string(src.Render))
	if err != nil {
		return nil, err
	}
	if mode == RenderMarkdown && src.Kind != "" && src.Kind != api.KindBundle {
		return nil, usageErrorf("--render markdown conflicts with --as %s; omit --as or use --as bundle", src.Kind)
	}
	p := &Prepared{Entry: src.Entry}
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
			return nil, usageErrorf("a file name is required when reading from stdin (use --name)")
		}
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
	} else {
		if name == "" {
			abs, err := filepath.Abs(srcPath)
			if err != nil {
				return nil, err
			}
			name = filepath.Base(abs) // the name given, not a link target's
		}
		st, err := os.Stat(srcPath)
		if err != nil {
			return nil, filePathError(srcPath, err, os.Lstat)
		}
		// Resolve a symlinked root once, so a link to a folder is walked
		// (and confined) as that folder.
		resolved, err := filepath.EvalSymlinks(srcPath)
		if err != nil {
			return nil, err
		}
		srcPath = resolved
		isDir = st.IsDir()
		if !isDir && !st.Mode().IsRegular() {
			return nil, usageErrorf("%s is not a regular file or directory", src.Path)
		}
	}
	shouldRender := mode == RenderMarkdown || (mode == RenderAuto && src.Kind == "" && !isDir && strings.EqualFold(path.Ext(name), ".md"))
	if shouldRender {
		rendered, err := prepareMarkdown(src, srcPath, name, isDir)
		if err != nil {
			return fail(err)
		}
		rendered.reportWarnings(src.Warn)
		cleanup := rendered.cleanup
		rendered.cleanup = func() { cleanup(); p.Close() }
		return rendered, nil
	}
	kind := src.Kind
	if kind == "" {
		kind = DetectKind(name, isDir)
	}
	if kind != api.KindHTML && kind != api.KindFile && kind != api.KindBundle {
		return fail(usageErrorf("unknown kind %q (use html, file, or bundle)", kind))
	}
	if isDir && kind != api.KindBundle {
		return fail(usageErrorf("%s is a directory; share it as a bundle", src.Path))
	}
	p.Kind, p.Filename = kind, name

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
		p.Digest, p.skipped, err = packDir(srcPath, packed, walkHooks{warn: p.warn, beforeRead: src.beforeRead})
	case strings.HasSuffix(lower, ".zip"):
		p.Digest, err = zipToTarGz(srcPath, src.beforeRead, packed)
	case strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm"):
		p.Digest, err = packSingle(srcPath, "index.html", packed)
	default:
		p.Digest, err = packSingle(srcPath, name, packed)
	}
	if cerr := packed.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	p.File = packed.Name()
	p.reportWarnings(src.Warn)
	return p, nil
}

// zipToTarGz converts a zip to a tar.gz bundle and returns the zip's
// SHA-256. Zip reads are random access, so the file is hashed before and
// after the conversion; a difference means it changed in between. That
// includes a conversion that failed because the zip was being rewritten:
// it fails as ErrSourceChanged when the zip's hash changed.
func zipToTarGz(src string, beforeRead func(string), w io.Writer) (string, error) {
	before, err := hashFile(src)
	if err != nil {
		return "", sourceChanged(err)
	}
	if beforeRead != nil {
		beforeRead(filepath.Base(src))
	}
	convErr := convertZip(src, w)
	after, err := hashFile(src)
	switch {
	case err != nil:
		return "", sourceChanged(err)
	case after != before && convErr != nil:
		return "", fmt.Errorf("%w: %s was modified while converting it: %w", ErrSourceChanged, src, convErr)
	case after != before:
		return "", fmt.Errorf("%w: %s was modified while converting it", ErrSourceChanged, src)
	case convErr != nil:
		return "", convErr
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
	return writeTarGzip(w, func(tw *tar.Writer) error {
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
			_, err = addBundleFile(tw, name, rc, int64(f.UncompressedSize64))
			if closeErr := rc.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// PublishOptions are artifact settings for a new share.
type PublishOptions struct {
	Title    string
	TTL      string
	Password string
	MaxViews int
	// InfoButton shows or hides the info button (and the viewer frame for
	// HTML); nil uses the server default.
	InfoButton *bool
	// OldVersions lets viewers open older versions from the info panel.
	OldVersions bool
}

// Share prepares, uploads, and publishes a new artifact. If the source
// changes while it is read, it is prepared and uploaded once more.
func (c *Client) Share(ctx context.Context, src *Source, opts PublishOptions, progress Progress) (*api.Artifact, error) {
	u, err := retrySourceChange(func() (*upload, error) { return c.prepareAndUpload(ctx, src, progress, nil) })
	if err != nil {
		return nil, err
	}
	return c.CreateArtifact(ctx, &api.CreateArtifactRequest{
		PublishRequest: u.request(src),
		Title:          opts.Title, TTL: opts.TTL, Password: opts.Password, MaxViews: opts.MaxViews,
		InfoButton: opts.InfoButton, OldVersions: opts.OldVersions,
	})
}

// Update uploads src as a new version of an existing artifact (same link).
// If the source changes while it is read, it is prepared and uploaded once
// more.
func (c *Client) Update(ctx context.Context, artifactID string, src *Source, progress Progress) (*api.Artifact, error) {
	return c.UpdateWithOptions(ctx, artifactID, src, VersionOptions{}, progress)
}

// VersionOptions are artifact settings changed together with a new version.
type VersionOptions struct {
	// Settings that are set are saved explicitly before the upload starts,
	// so a failed setting change never leaves a new version published.
	InfoButton  *bool
	OldVersions *bool
}

// ErrSettingsOnly reports that VersionOptions were saved but the new version
// was not published. It wraps the publishing error.
type ErrSettingsOnly struct{ Err error }

func (e *ErrSettingsOnly) Error() string {
	return "settings were saved, but the new version was not published: " + e.Err.Error()
}
func (e *ErrSettingsOnly) Unwrap() error { return e.Err }

// UpdateWithOptions prepares src, applies opts, then uploads src as a new
// version. A failure after opts were applied is an *ErrSettingsOnly. If
// the source changes while it is read, it is prepared and uploaded once
// more; opts are applied only once.
func (c *Client) UpdateWithOptions(ctx context.Context, artifactID string, src *Source, opts VersionOptions, progress Progress) (*api.Artifact, error) {
	applied := false
	apply := func() error {
		if applied || (opts.InfoButton == nil && opts.OldVersions == nil) {
			return nil
		}
		if _, err := c.UpdateArtifact(ctx, artifactID, &api.UpdateArtifactRequest{InfoButton: opts.InfoButton, OldVersions: opts.OldVersions}); err != nil {
			return err
		}
		applied = true
		return nil
	}
	u, err := retrySourceChange(func() (*upload, error) { return c.prepareAndUpload(ctx, src, progress, apply) })
	if err == nil {
		req := u.request(src)
		var a *api.Artifact
		if a, err = c.CreateVersion(ctx, artifactID, &req); err == nil {
			return a, nil
		}
	}
	if applied {
		return nil, &ErrSettingsOnly{Err: err}
	}
	return nil, err
}

// UpdateDigest is a single Update attempt that also returns a content
// digest in the same domain as hashTree: for folder bundles, the
// `path sha` digest of what was actually packed; for an explicit file,
// the SHA-256 of its bytes as read (including --as bundle, archives and
// rendered Markdown). It does not retry: an error wrapping
// ErrSourceChanged means the source changed while it was read, and the
// caller decides when to try again.
func (c *Client) UpdateDigest(ctx context.Context, artifactID string, src *Source, progress Progress) (string, *api.Artifact, error) {
	u, err := c.prepareAndUpload(ctx, src, progress, nil)
	if err != nil {
		return "", nil, err
	}
	req := u.request(src)
	a, err := c.CreateVersion(ctx, artifactID, &req)
	return u.digest, a, err
}

// upload is a prepared source after a successful UploadFile.
type upload struct {
	id, blobSum, digest, kind, filename, entry string
}

func (u *upload) request(src *Source) api.PublishRequest {
	return api.PublishRequest{UploadID: u.id, SHA256: u.blobSum, Kind: u.kind, Filename: u.filename, Entry: u.entry, SPA: src.SPA, Note: src.Note}
}

// prepareAndUpload is the shared Prepare + UploadFile step. beforeUpload,
// if set, runs after a successful Prepare and before the upload; its error
// aborts the step. Excluded folder entries are reported through
// src.OnSkipped, once per successful upload.
func (c *Client) prepareAndUpload(ctx context.Context, src *Source, progress Progress, beforeUpload func() error) (*upload, error) {
	p, err := Prepare(src)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	if beforeUpload != nil {
		if err := beforeUpload(); err != nil {
			return nil, err
		}
	}
	if p.Digest == "" && src.Reader == nil && src.beforeRead != nil {
		src.beforeRead(p.Filename)
	}
	id, blobSum, err := c.UploadFile(ctx, p.File, progress)
	if err != nil {
		if p.Digest == "" && src.Reader == nil && errors.Is(err, fs.ErrNotExist) {
			// File is the source itself, found by Prepare a moment ago:
			// it was deleted or replaced in between.
			err = fmt.Errorf("%w: %w", ErrSourceChanged, err)
		}
		return nil, err
	}
	if p.skipped > 0 && src.OnSkipped != nil {
		src.OnSkipped(p.skipped)
	}
	digest := p.Digest
	if digest == "" {
		digest = blobSum // File was the source itself, checked as sent
	}
	return &upload{id: id, blobSum: blobSum, digest: digest, kind: p.Kind, filename: p.Filename, entry: p.Entry}, nil
}

// retrySourceChange runs step again once if the source changed while it
// was read, then gives up with an error that still wraps ErrSourceChanged.
// A retried upload abandons the first upload session, possibly after all
// of it was sent; the server deletes unfinished sessions when they expire
// (24h), so until then an edited 500 MB share holds up to 1 GB of staging.
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
