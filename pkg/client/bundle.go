package client

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// bundleFile adds generated content or a previously read source snapshot.
// Replace is reserved for the source snapshot; generated files must not replace
// files in the input filesystem. A Replace file is source content, so it is
// part of the pack's content digest; generated files are not.
type bundleFile struct {
	Name    string
	Content io.Reader
	Size    int64
	Replace bool
}

// walkHooks are the optional callbacks of a bundle walk.
type walkHooks struct {
	// warn receives non-fatal warnings, such as a skipped broken symlink.
	warn func(string)
	// beforeRead, set only by tests, runs between a file's stat and its
	// read, to reproduce a tree that changes mid-read.
	beforeRead func(name string)
}

// bundleWalk is the one traversal of a directory bundle. Packing, the watch
// content digest (hashTree) and the watch directory registration all use it,
// so they agree on which files count: the same os.Root confinement, symlink
// rules and skip rule (skipEntryName, which the watch event filter also
// uses). Errors from entries that were listed and then vanished, and from
// files that ended before their listed size, wrap ErrSourceChanged.
type bundleWalk struct {
	fsys  fs.FS
	hooks walkHooks
	// claim, if set, runs before each path is visited. A claimed path is
	// not walked; an error aborts the walk.
	claim func(name string) (claimed bool, err error)
	// dir, if set, runs for every directory walked, including the start.
	dir func(name string) error
	// file, if set, receives every regular file, opened. Without it, files
	// are not opened.
	file func(name string, content io.Reader, size int64) error
	// skipped counts the file entries the skip rule excluded. Excluded
	// directory subtrees are not traversed or counted.
	skipped int
}

// run walks start ("." for the whole tree) and everything below it.
func (b *bundleWalk) run(start string) error {
	return b.walk(start, nil, nil)
}

// skipEntry is the walk's skip decision for one path component with base
// name base and type mode, as listed (not followed): a directory uses the
// directory rule; anything else is skipped by the file rule, and a symlink
// also by the directory rule, so a skip-named link is never followed.
// counted reports a skip that the walk counts as a skipped file. The watch
// event filter and the Markdown --entry check use it too, so they decide
// exactly as the walk does.
func skipEntry(base string, mode fs.FileMode) (skip, counted bool) {
	switch {
	case mode.IsDir():
		return skipEntryName(base, true), false
	case skipEntryName(base, false):
		return true, true
	case mode&fs.ModeSymlink != 0:
		return skipEntryName(base, true), false
	}
	return false, false
}

// skip applies skipEntry to name before anything follows it, so a skipped
// entry is never stat'ed through a symlink, warned about, or reported as
// changed. Skipped file entries are counted.
func (b *bundleWalk) skip(name string, entry fs.DirEntry) bool {
	if name == "." {
		return false
	}
	var mode fs.FileMode
	if entry != nil {
		mode = entry.Type()
	} else { // a start below the root: its type is not listed yet
		info, err := fs.Lstat(b.fsys, name)
		if err != nil {
			return false // the stat in walk reports it
		}
		mode = info.Mode()
	}
	skip, counted := skipEntry(path.Base(name), mode)
	if counted {
		b.skipped++
	}
	return skip
}

// walk visits name. entry is how its directory listed it (nil for the
// start), used to apply the skip rule before the entry is followed.
func (b *bundleWalk) walk(name string, entry fs.DirEntry, ancestors []fs.FileInfo) error {
	if b.claim != nil {
		if claimed, err := b.claim(name); claimed || err != nil {
			return err
		}
	}
	if b.skip(name, entry) {
		return nil
	}
	info, err := fs.Stat(b.fsys, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			link, linkErr := fs.Lstat(b.fsys, name)
			if linkErr == nil && link.Mode()&fs.ModeSymlink != 0 {
				b.warn(fmt.Sprintf("bundle path %q skipped: broken symlink", name))
				return nil
			}
			if name != "." && errors.Is(linkErr, fs.ErrNotExist) {
				return sourceChanged(err) // listed by its directory, then gone
			}
		}
		return bundlePathError(b.fsys, name, err)
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
		if b.dir != nil {
			if err := b.dir(name); err != nil {
				return err
			}
		}
		entries, err := fs.ReadDir(b.fsys, name)
		if err != nil {
			if name != "." && errors.Is(err, fs.ErrNotExist) {
				return sourceChanged(err)
			}
			return bundlePathError(b.fsys, name, err)
		}
		for _, child := range entries {
			if err := b.walk(path.Join(name, child.Name()), child, append(ancestors, info)); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		b.warn(fmt.Sprintf("bundle path %q skipped: not a regular file or directory", name))
		return nil
	}
	if b.file == nil {
		return nil
	}
	if b.hooks.beforeRead != nil {
		b.hooks.beforeRead(name)
	}
	file, err := b.fsys.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return sourceChanged(err)
		}
		return bundlePathError(b.fsys, name, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return usageErrorf("bundle file %q is no longer a regular file", name)
	}
	// Read the size the walk listed: a file that shrank since then is a
	// short read (ErrSourceChanged); one that grew is packed and hashed up
	// to that size, and the next tree hash sees the difference.
	return b.file(name, &shortReadDetector{r: file}, info.Size())
}

func (b *bundleWalk) warn(message string) {
	if b.hooks.warn != nil {
		b.hooks.warn(message)
	}
}

// shortReadDetector reports a file that ends before the size it was opened
// with as ErrSourceChanged, since the bytes read cannot be one version.
type shortReadDetector struct{ r io.Reader }

func (s *shortReadDetector) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF {
		err = fmt.Errorf("%w: %w", ErrSourceChanged, io.ErrUnexpectedEOF)
	}
	return n, err
}

// packed describes a pack: its content digest entries and the file entries
// the skip rule excluded.
type packed struct {
	digest  treeDigestBuilder
	skipped int
}

// packFS uses the same traversal and symlink policy for every directory bundle.
// Callers supply an os.Root filesystem so every stat, directory read, and open
// remains within the selected root, including when following symlinks. Each
// walked file and each Replace extra is hashed as it streams into the tar, so
// the returned digest covers the bytes actually packed.
func packFS(fsys fs.FS, w io.Writer, hooks walkHooks, extras ...bundleFile) (*packed, error) {
	added := make(map[string]bundleFile, len(extras))
	for _, extra := range extras {
		if !fs.ValidPath(extra.Name) || extra.Name == "." || strings.Contains(extra.Name, "\\") || extra.Size < 0 {
			return nil, usageErrorf("invalid bundle file %q", extra.Name)
		}
		if _, exists := added[extra.Name]; exists {
			return nil, usageErrorf("duplicate bundle file %q", extra.Name)
		}
		added[extra.Name] = extra
	}
	result := &packed{}
	err := writeTarGzip(w, func(tw *tar.Writer) error {
		for _, extra := range extras {
			sum, err := addBundleFile(tw, extra.Name, extra.Content, extra.Size)
			if err != nil {
				return err
			}
			if extra.Replace {
				result.digest.add(extra.Name, sum)
			}
		}
		if fsys == nil {
			return nil
		}
		walk := &bundleWalk{
			fsys:  fsys,
			hooks: hooks,
			claim: func(name string) (bool, error) {
				extra, exists := added[name]
				if !exists {
					return false, nil
				}
				if extra.Replace {
					return true, nil // retain the exact source snapshot, not a second read
				}
				return true, usageErrorf("bundle already contains %s; generated content cannot replace it", name)
			},
			file: func(name string, content io.Reader, size int64) error {
				sum, err := addBundleFile(tw, name, content, size)
				if err == nil {
					result.digest.add(name, sum)
				}
				return err
			},
		}
		err := walk.run(".")
		result.skipped = walk.skipped
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// skipEntryName is the single skip rule for publishable content: the
// bundle walk (packing, the watch content digest, watch registration) and
// the watch event filter all consult it, so they can never disagree about
// what counts; skipEntry adds the type-dependent part. Directories: VCS and
// dependency caches. Files: a .git file (worktrees and submodules), OS junk
// and editor temp/swap/backup files (vim swaps .swp down to .swa plus .swx
// on dot-prefixed names, JetBrains ___jb_tmp___/___jb_old___, gedit
// .goutputstream-*, *~ backups, emacs .# lock files and #…# autosave),
// which never belong in a published artifact. Applied only to entries
// inside a directory: an explicitly named single file is always published.
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
	case base == ".git": // a worktree or submodule link; it names a local path
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

// single returns the hash of the only digested file, in the hashFile domain.
// It is the digest of a pack of one explicitly named source: packSingle, or
// single-file Markdown, where the Replace snapshot is the only digested
// entry and generated files are not digested.
func (b *treeDigestBuilder) single() string {
	if len(b.entries) != 1 {
		panic(fmt.Sprintf("single-source digest over %d entries", len(b.entries)))
	}
	return b.entries[0].sum
}

func (b *treeDigestBuilder) digest() string {
	sort.Slice(b.entries, func(i, j int) bool { return b.entries[i].rel < b.entries[j].rel })
	h := sha256.New()
	for _, e := range b.entries {
		fmt.Fprintf(h, "%s %s\n", e.rel, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// packDir packs a directory and returns the tree digest of what was packed
// (the hashTree domain) and the number of file entries the skip rule
// excluded. A directory with no packable files is ErrEmptyDirectory.
func packDir(dir string, w io.Writer, hooks walkHooks) (string, int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", 0, filePathError(dir, err, os.Lstat)
	}
	defer root.Close()
	result, err := packFS(root.FS(), w, hooks)
	if err != nil {
		return "", 0, err
	}
	if len(result.digest.entries) == 0 {
		return "", 0, fmt.Errorf("%s: %w", dir, ErrEmptyDirectory)
	}
	return result.digest.digest(), result.skipped, nil
}

// packSingle packs one explicitly named file as a bundle and returns the
// SHA-256 of the source bytes packed.
func packSingle(src, name string, w io.Writer, beforeRead func(string)) (string, error) {
	if beforeRead != nil {
		beforeRead(name)
	}
	file, err := openStatedSource(src)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	content := &shortReadDetector{r: file}
	result, err := packFS(nil, w, walkHooks{}, bundleFile{Name: name, Content: content, Size: info.Size(), Replace: true})
	if err != nil {
		return "", err
	}
	return result.digest.single(), nil
}

// addBundleFile writes one file into the tar and returns the SHA-256 of
// the bytes written.
func addBundleFile(tw *tar.Writer, name string, content io.Reader, size int64) (string, error) {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: size}); err != nil {
		return "", err
	}
	return copyHashed(tw, content, size)
}

// copyHashed copies n bytes of r to w and returns the hex SHA-256 of the
// bytes copied.
func copyHashed(w io.Writer, r io.Reader, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(w, h), r, n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
