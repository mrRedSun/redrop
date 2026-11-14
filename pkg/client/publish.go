package client

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
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
	// Entry is the start page inside a bundle (default index.html).
	Entry string
	SPA   bool
	Note  string
}

// Prepared is a source packed into a single upload-ready file.
type Prepared struct {
	File     string // path to upload
	Kind     string
	Filename string
	cleanup  func()
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

// Prepare packs a Source into an upload-ready temporary file when needed.
func Prepare(src *Source) (*Prepared, error) {
	p := &Prepared{}
	var tmpFiles []string
	p.cleanup = func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
	}
	fail := func(err error) (*Prepared, error) { p.Close(); return nil, err }

	srcPath := src.Path
	name := src.Name
	isDir := false
	if src.Reader != nil {
		if name == "" {
			return nil, errors.New("a file name is required when reading from stdin (use --name)")
		}
		tmp, err := os.CreateTemp("", "redrop-stdin-*")
		if err != nil {
			return nil, err
		}
		tmpFiles = append(tmpFiles, tmp.Name())
		if _, err := io.Copy(tmp, src.Reader); err != nil {
			tmp.Close()
			return fail(err)
		}
		tmp.Close()
		srcPath = tmp.Name()
	} else {
		st, err := os.Stat(srcPath)
		if err != nil {
			return nil, err
		}
		isDir = st.IsDir()
		if name == "" {
			abs, _ := filepath.Abs(srcPath)
			name = filepath.Base(abs)
		}
	}
	kind := src.Kind
	if kind == "" {
		kind = DetectKind(name, isDir)
	}
	p.Kind, p.Filename = kind, name

	switch kind {
	case api.KindHTML, api.KindFile:
		if isDir {
			return fail(fmt.Errorf("%s is a directory; share it as a bundle", srcPath))
		}
		p.File = srcPath
	case api.KindBundle:
		tmp, err := os.CreateTemp("", "redrop-bundle-*.tar.gz")
		if err != nil {
			return fail(err)
		}
		tmpFiles = append(tmpFiles, tmp.Name())
		lower := strings.ToLower(name)
		switch {
		case isDir:
			err = packDir(srcPath, tmp)
		case strings.HasSuffix(lower, ".zip"):
			err = zipToTarGz(srcPath, tmp)
		case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
			var in *os.File
			if in, err = os.Open(srcPath); err == nil {
				_, err = io.Copy(tmp, in)
				in.Close()
			}
		case strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm"):
			err = packSingle(srcPath, "index.html", tmp)
		default:
			err = packSingle(srcPath, name, tmp)
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(err)
		}
		p.File = tmp.Name()
	default:
		return fail(fmt.Errorf("unknown kind %q (use html, file, or bundle)", kind))
	}
	return p, nil
}

func skipEntry(rel string, d fs.DirEntry) bool {
	base := d.Name()
	if d.IsDir() {
		return base == ".git" || base == "node_modules" || base == ".svn" || base == ".hg" || base == "__pycache__" || base == ".DS_Store"
	}
	return base == ".DS_Store" || base == "Thumbs.db"
}

func packDir(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := os.Stat(p) // follow symlinks to their content
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		return addFile(tw, p, rel, info.Size())
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addFile(tw *tar.Writer, src, name string, size int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, size)
	return err
}

func packSingle(src, name string, w io.Writer) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := addFile(tw, src, name, st.Size()); err != nil {
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
	// InfoButton shows or hides the info button (and the viewer frame for
	// HTML); nil uses the server default.
	InfoButton *bool
	// OldVersions lets viewers open older versions from the info panel.
	OldVersions bool
}

// Share prepares, uploads, and publishes a new artifact.
func (c *Client) Share(ctx context.Context, src *Source, opts PublishOptions, progress Progress) (*api.Artifact, error) {
	p, err := Prepare(src)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	id, sum, err := c.UploadFile(ctx, p.File, progress)
	if err != nil {
		return nil, err
	}
	return c.CreateArtifact(ctx, &api.CreateArtifactRequest{
		PublishRequest: api.PublishRequest{UploadID: id, SHA256: sum, Kind: p.Kind, Filename: p.Filename, Entry: src.Entry, SPA: src.SPA, Note: src.Note},
		Title:          opts.Title, TTL: opts.TTL, Password: opts.Password, MaxViews: opts.MaxViews,
		InfoButton: opts.InfoButton, OldVersions: opts.OldVersions,
	})
}

// Update uploads src as a new version of an existing artifact (same link).
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
// version. A failure after opts were applied is an *ErrSettingsOnly.
func (c *Client) UpdateWithOptions(ctx context.Context, artifactID string, src *Source, opts VersionOptions, progress Progress) (*api.Artifact, error) {
	p, err := Prepare(src)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	applied := false
	if opts.InfoButton != nil || opts.OldVersions != nil {
		if _, err := c.UpdateArtifact(ctx, artifactID, &api.UpdateArtifactRequest{InfoButton: opts.InfoButton, OldVersions: opts.OldVersions}); err != nil {
			return nil, err
		}
		applied = true
	}
	id, sum, err := c.UploadFile(ctx, p.File, progress)
	if err == nil {
		var a *api.Artifact
		if a, err = c.CreateVersion(ctx, artifactID, &api.PublishRequest{UploadID: id, SHA256: sum, Kind: p.Kind, Filename: p.Filename, Entry: src.Entry, SPA: src.SPA, Note: src.Note}); err == nil {
			return a, nil
		}
	}
	if applied {
		return nil, &ErrSettingsOnly{Err: err}
	}
	return nil, err
}
