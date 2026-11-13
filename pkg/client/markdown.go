package client

import (
	"bytes"
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
	if src.SPA {
		return nil, usageErrorf("--spa is not supported with Markdown rendering")
	}
	lower := strings.ToLower(name)
	if !isDir && (strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")) {
		return nil, usageErrorf("archives cannot be rendered as Markdown; unpack into a directory and select --entry")
	}
	sourceName := name
	base := ""
	if isDir {
		if src.Entry == "" {
			return nil, usageErrorf("--render markdown on a directory requires --entry, e.g. --entry README.md")
		}
		sourceName = src.Entry
		base = path.Dir(sourceName)
	} else if src.Entry != "" && src.Entry != "index.html" {
		return nil, usageErrorf("rendered Markdown uses --entry index.html; share a directory to select a Markdown entry")
	}
	if !fs.ValidPath(sourceName) || sourceName == "." || strings.ContainsAny(sourceName, "\\:") || path.Base(sourceName) == "index.html" || sourceName == api.ReservedPrefix || strings.HasPrefix(sourceName, api.ReservedPrefix+"/") {
		return nil, usageErrorf("Markdown source name must be a relative bundle path other than index.html or %s", api.ReservedPrefix)
	}
	if !isDir && path.Base(sourceName) != sourceName {
		return nil, usageErrorf("--name must be a file name, not a path, when rendering a single file")
	}
	if isDir {
		if _, err := os.Lstat(filepath.Join(srcPath, "index.html")); err == nil {
			return nil, usageErrorf("cannot render Markdown: bundle already contains index.html")
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect bundle index.html: %w", err)
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
	if len(original) > markdown.MaxSourceBytes {
		return nil, usageErrorf("Markdown exceeds %d MiB; use --render none to share it raw", markdown.MaxSourceBytes>>20)
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
	var fsys fs.FS
	if root != nil {
		fsys = root.FS()
	}
	err = packFS(fsys, f,
		bundleFile{Name: "index.html", Content: bytes.NewReader(rendered.HTML), Size: int64(len(rendered.HTML))},
		bundleFile{Name: name, Content: bytes.NewReader(original), Size: int64(len(original)), Replace: true},
	)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	if root == nil {
		for _, reference := range rendered.LocalReferences {
			if reference != name && reference != "index.html" {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s is not uploaded; share the directory with --render markdown --entry %s", reference, name))
			}
		}
	}
	return p, nil
}
