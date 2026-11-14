package client

import (
	"archive/tar"
	"archive/zip"
	"context"
	"fmt"
	"io"
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
}

// Prepared is a source packed into a single upload-ready file.
type Prepared struct {
	File     string // path to upload
	Kind     string
	Filename string
	Entry    string // effective entry, including generated index.html
	Warnings []string
	cleanup  func()
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

	srcPath := src.Path
	name := src.Name
	isDir := false
	if src.Reader != nil {
		if name == "" {
			return nil, usageErrorf("a file name is required when reading from stdin (use --name)")
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
			return nil, filePathError(srcPath, err, os.Lstat)
		}
		isDir = st.IsDir()
		if name == "" {
			abs, _ := filepath.Abs(srcPath)
			name = filepath.Base(abs)
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
	p.Kind, p.Filename = kind, name

	switch kind {
	case api.KindHTML, api.KindFile:
		if isDir {
			return fail(usageErrorf("%s is a directory; share it as a bundle", srcPath))
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
			err = packDir(srcPath, tmp, p.warn)
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
		return fail(usageErrorf("unknown kind %q (use html, file, or bundle)", kind))
	}
	p.reportWarnings(src.Warn)
	return p, nil
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
			err = addBundleFile(tw, name, rc, int64(f.UncompressedSize64))
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
		PublishRequest: api.PublishRequest{UploadID: id, SHA256: sum, Kind: p.Kind, Filename: p.Filename, Entry: p.Entry, SPA: src.SPA, Note: src.Note},
		Title:          opts.Title, TTL: opts.TTL, Password: opts.Password, MaxViews: opts.MaxViews,
	})
}

// Update uploads src as a new version of an existing artifact (same link).
func (c *Client) Update(ctx context.Context, artifactID string, src *Source, progress Progress) (*api.Artifact, error) {
	p, err := Prepare(src)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	id, sum, err := c.UploadFile(ctx, p.File, progress)
	if err != nil {
		return nil, err
	}
	return c.CreateVersion(ctx, artifactID, &api.PublishRequest{UploadID: id, SHA256: sum, Kind: p.Kind, Filename: p.Filename, Entry: p.Entry, SPA: src.SPA, Note: src.Note})
}
