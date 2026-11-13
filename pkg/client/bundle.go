package client

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// bundleFile adds generated content or a previously read source snapshot.
// Replace is reserved for the source snapshot; generated files must not replace
// files in the input filesystem.
type bundleFile struct {
	Name    string
	Content io.Reader
	Size    int64
	Replace bool
}

// packFS uses the same traversal and symlink policy for every directory bundle.
// Callers supply an os.Root filesystem so every stat, directory read, and open
// remains within the selected root, including when following symlinks.
func packFS(fsys fs.FS, w io.Writer, extras ...bundleFile) error {
	added := make(map[string]bundleFile, len(extras))
	for _, extra := range extras {
		if !fs.ValidPath(extra.Name) || extra.Name == "." || strings.Contains(extra.Name, "\\") || extra.Size < 0 {
			return usageErrorf("invalid bundle file %q", extra.Name)
		}
		if _, exists := added[extra.Name]; exists {
			return usageErrorf("duplicate bundle file %q", extra.Name)
		}
		added[extra.Name] = extra
	}
	return writeTarGzip(w, func(tw *tar.Writer) error {
		for _, extra := range extras {
			if err := addBundleFile(tw, extra.Name, extra.Content, extra.Size); err != nil {
				return err
			}
		}
		if fsys == nil {
			return nil
		}
		var walk func(string, []fs.FileInfo) error
		walk = func(name string, ancestors []fs.FileInfo) error {
			if extra, exists := added[name]; exists {
				if extra.Replace {
					return nil // retain the exact source snapshot, not a second read
				}
				return usageErrorf("bundle already contains %s; generated content cannot replace it", name)
			}
			info, err := fs.Stat(fsys, name)
			if err != nil {
				return fmt.Errorf("inspect bundle path %q: %w", name, err)
			}
			if skipEntry(name, info.IsDir()) {
				return nil
			}
			if info.IsDir() {
				// SameFile detects real-directory cycles immediately. The depth
				// bound also protects filesystems without comparable identities.
				if len(ancestors) >= 64 {
					return usageErrorf("bundle directory depth exceeds 64 at %q; check for symlink cycles", name)
				}
				for _, ancestor := range ancestors {
					if os.SameFile(info, ancestor) {
						return usageErrorf("bundle directory symlink cycle at %q", name)
					}
				}
				entries, err := fs.ReadDir(fsys, name)
				if err != nil {
					return fmt.Errorf("read bundle directory %q: %w", name, err)
				}
				for _, entry := range entries {
					if err := walk(path.Join(name, entry.Name()), append(ancestors, info)); err != nil {
						return err
					}
				}
				return nil
			}
			if !info.Mode().IsRegular() {
				return usageErrorf("bundle path %q is not a regular file or directory", name)
			}
			file, err := fsys.Open(name)
			if err != nil {
				return fmt.Errorf("open bundle file %q: %w", name, err)
			}
			defer file.Close()
			info, err = file.Stat()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return usageErrorf("bundle file %q is no longer a regular file", name)
			}
			return addBundleFile(tw, name, file, info.Size())
		}
		return walk(".", nil)
	})
}

func skipEntry(name string, isDir bool) bool {
	base := path.Base(name)
	if isDir {
		return base == ".git" || base == "node_modules" || base == ".svn" || base == ".hg" || base == "__pycache__" || base == ".DS_Store"
	}
	return base == ".DS_Store" || base == "Thumbs.db"
}

func packDir(dir string, w io.Writer) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return packFS(root.FS(), w)
}

func packSingle(src, name string, w io.Writer) error {
	root, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(src))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return usageErrorf("bundle source %q is not a regular file", src)
	}
	return packFS(nil, w, bundleFile{Name: name, Content: file, Size: info.Size()})
}

func addBundleFile(tw *tar.Writer, name string, content io.Reader, size int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: size}); err != nil {
		return err
	}
	_, err := io.CopyN(tw, content, size)
	return err
}

func writeTarGzip(w io.Writer, write func(*tar.Writer) error) (err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer func() {
		for _, finish := range []func() error{tw.Close, gz.Close} {
			if closeErr := finish(); err == nil {
				err = closeErr
			}
		}
	}()
	return write(tw)
}
