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
		// OpenRoot confines source selection even when a path contains symlinks.
		root, err := os.OpenRoot(srcPath)
		if err != nil {
			return nil, filePathError(srcPath, err, os.Lstat)
		}
		defer root.Close()
		if err := checkEntryNotSkipped(root, sourceName); err != nil {
			return nil, err
		}
		if _, err := root.Lstat("index.html"); err == nil {
			return nil, usageErrorf("cannot render Markdown: bundle already contains index.html")
		} else if !os.IsNotExist(err) {
			return nil, bundlePathError(root.FS(), "index.html", err)
		}
		f, err := openRegularFile(root, sourceName)
		if err != nil {
			return nil, fmt.Errorf("--entry %s: %w", sourceName, err)
		}
		defer f.Close()
		return packMarkdown(sourceName, base, f, root, src.beforeRead)
	}
	if src.beforeRead != nil {
		src.beforeRead(sourceName)
	}
	f, err := openStatedSource(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return packMarkdown(sourceName, base, f, nil, nil)
}

// checkEntryNotSkipped rejects an --entry that the walk would skip, through
// any component of its path, since the watch digest and event filter would
// then never see it. Each component is decided by skipEntry with its type as
// listed inside root; a component that cannot be listed is left to the open
// that follows.
func checkEntryNotSkipped(root *os.Root, entry string) error {
	parts := strings.Split(entry, "/")
	for i, part := range parts {
		mode := fs.ModeDir
		if i == len(parts)-1 {
			mode = 0
		}
		if info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/"))); err == nil {
			mode = info.Mode()
		}
		if skip, _ := skipEntry(part, mode); skip {
			return usageErrorf("--entry %s is excluded from folder uploads (%s matches the skip rule; see redrop guide)", entry, part)
		}
	}
	return nil
}

// packMarkdown renders source and packs it with the original snapshot and,
// for a directory, the rest of root. Prepared.Digest is in the hashTree
// domain: the tree digest of root with the snapshot standing in for the
// source file, or the snapshot's hash for a single file.
func packMarkdown(name, base string, source io.Reader, root *os.Root, beforeRead func(string)) (*Prepared, error) {
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
	result, err := packFS(fsys, f, walkHooks{warn: p.warn, beforeRead: beforeRead},
		bundleFile{Name: "index.html", Content: bytes.NewReader(rendered.HTML), Size: int64(len(rendered.HTML))},
		bundleFile{Name: name, Content: bytes.NewReader(original), Size: int64(len(original)), Replace: true},
	)
	if err == nil {
		p.skipped = result.skipped
		if root != nil {
			p.Digest = result.digest.digest()
		} else {
			p.Digest = result.digest.single()
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	for _, image := range rendered.RejectedImageSources {
		if image == "" {
			p.warn("empty image source removed: images must be files inside the bundle")
		} else {
			p.warn(fmt.Sprintf("image %q removed: images must be files inside the bundle", image))
		}
	}
	if root == nil {
		for _, reference := range rendered.LocalReferences {
			if reference != name && reference != "index.html" {
				p.warn(fmt.Sprintf("%s is not uploaded; share the directory with --render markdown --entry %s", reference, name))
			}
		}
	}
	return p, nil
}
