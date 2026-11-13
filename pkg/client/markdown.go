package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mrRedSun/redrop/internal/markdown"
	"github.com/mrRedSun/redrop/pkg/api"
)

func prepareMarkdown(src *Source, srcPath, name string, isDir bool) (*Prepared, error) {
	if src.Kind != "" && src.Kind != api.KindBundle {
		return nil, fmt.Errorf("--render markdown conflicts with --as %s; omit --as or use --as bundle", src.Kind)
	}
	if src.SPA {
		return nil, fmt.Errorf("--spa is not supported with Markdown rendering")
	}
	lower := strings.ToLower(name)
	if !isDir && (strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")) {
		return nil, fmt.Errorf("archives cannot be rendered as Markdown; unpack into a directory and select --entry")
	}
	sourceName := name
	base := ""
	if isDir {
		if src.Entry == "" {
			return nil, fmt.Errorf("--render markdown on a directory requires --entry, e.g. --entry README.md")
		}
		sourceName = src.Entry
		base = path.Dir(sourceName)
	} else if src.Entry != "" && src.Entry != "index.html" {
		return nil, fmt.Errorf("rendered Markdown uses --entry index.html; share a directory to select a Markdown entry")
	}
	if !fs.ValidPath(sourceName) || sourceName == "." || strings.ContainsAny(sourceName, "\\:") || path.Base(sourceName) == "index.html" || sourceName == "__redrop" || strings.HasPrefix(sourceName, "__redrop/") {
		return nil, fmt.Errorf("Markdown source name must be a relative bundle path other than index.html or __redrop")
	}
	if !isDir && path.Base(sourceName) != sourceName {
		return nil, fmt.Errorf("--name must be a file name, not a path, when rendering a single file")
	}
	if isDir {
		if _, err := os.Lstat(filepath.Join(srcPath, "index.html")); !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot render Markdown: bundle already contains index.html or cannot be inspected")
		}
		// OpenRoot confines source selection even when a path contains symlinks.
		root, err := os.OpenRoot(srcPath)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		f, err := root.Open(filepath.FromSlash(sourceName))
		if err != nil {
			return nil, fmt.Errorf("open Markdown entry: %w", err)
		}
		defer f.Close()
		return packMarkdown(sourceName, base, f, root)
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return packMarkdown(sourceName, base, f, nil)
}

func packMarkdown(name, base string, source io.Reader, root *os.Root) (*Prepared, error) {
	original, err := io.ReadAll(io.LimitReader(source, markdown.MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	rendered, err := markdown.Render(original, name, base)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "redrop-markdown-*.tar.gz")
	if err != nil {
		return nil, err
	}
	p := &Prepared{File: f.Name(), Kind: api.KindBundle, Filename: "document.tar.gz", Entry: "index.html", cleanup: func() { os.Remove(f.Name()) }}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	write := func(name string, content io.Reader, size int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: size}); err != nil {
			return err
		}
		_, err := io.CopyN(tw, content, size)
		return err
	}
	err = write("index.html", bytes.NewReader(rendered), int64(len(rendered)))
	if err == nil {
		err = write(name, bytes.NewReader(original), int64(len(original)))
	}
	if err == nil && root != nil {
		err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
			if err != nil || rel == "." {
				return err
			}
			if skipEntry(rel, d) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// Unlike ordinary bundle publishing, do not follow asset symlinks.
			if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if rel == "index.html" {
				return fmt.Errorf("bundle acquired an index.html while rendering")
			}
			if rel == name {
				return nil // exact original bytes were already written
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			// Keep reads confined even if a file or ancestor becomes a symlink
			// between directory inspection and opening the asset.
			asset, err := root.Open(filepath.FromSlash(rel))
			if err != nil {
				return err
			}
			defer asset.Close()
			info, err = asset.Stat()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("bundle asset %q is no longer a regular file", rel)
			}
			return write(rel, asset, info.Size())
		})
	}
	// Close every layer even on failure, preserving the first error.
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if cerr := close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
