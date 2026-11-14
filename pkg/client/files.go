package client

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func bundlePathError(fsys fs.FS, name string, err error) error {
	return filePathError(name, err, func(name string) (fs.FileInfo, error) { return fs.Lstat(fsys, name) })
}

func filePathError(name string, err error, lstat func(string) (fs.FileInfo, error)) error {
	if errors.Is(err, fs.ErrNotExist) {
		message := fmt.Sprintf("%q does not exist: %v", name, err)
		if info, statErr := lstat(name); statErr == nil && info.Mode()&fs.ModeSymlink != 0 {
			message = fmt.Sprintf("symlink %q has a target that does not exist: %v", name, err)
		}
		return &UsageError{Message: message, Cause: err}
	}
	if errors.Is(err, syscall.ELOOP) {
		return &UsageError{Message: fmt.Sprintf("cannot access %q: symlink cycle: %v", name, err), Cause: err}
	}
	cause := err
	for errors.Unwrap(cause) != nil {
		cause = errors.Unwrap(cause)
	}
	// Go 1.26 does not export os.Root's path-escape sentinel. The outside-file
	// and absolute-inside cases in TestBundleRejectsUnsafeSymlinks guard this
	// leaf-error match across toolchain upgrades. Other I/O errors stay runtime.
	if cause.Error() == "path escapes from parent" {
		return &UsageError{
			Message: fmt.Sprintf("cannot access %q: symlinks must be relative and stay inside the directory: %v", name, err),
			Cause:   err,
		}
	}
	return fmt.Errorf("access path %q: %w", name, err)
}

// openRegularFile confines a directory entry and requires a regular file before
// and after opening. Unix opens are nonblocking so a FIFO swapped in after Stat
// cannot block the open; the descriptor check rejects it before any read.
func openRegularFile(root *os.Root, name string) (*os.File, error) {
	local := filepath.FromSlash(name)
	info, err := root.Stat(local)
	if err != nil {
		return nil, bundlePathError(root.FS(), name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, usageErrorf("%s is not a regular file", name)
	}
	file, err := root.OpenFile(local, regularFileOpenFlags, 0)
	if err != nil {
		return nil, bundlePathError(root.FS(), name, err)
	}
	return checkOpenedRegularFile(file, name)
}

// Explicit single-file selections follow normal filesystem symlinks. They are
// not directory walks, and must not be confined to the symlink's parent.
func openSingleSource(src string) (*os.File, error) {
	info, err := os.Stat(src)
	if err != nil {
		return nil, filePathError(src, err, os.Lstat)
	}
	if !info.Mode().IsRegular() {
		return nil, usageErrorf("%s is not a regular file", src)
	}
	file, err := os.OpenFile(src, regularFileOpenFlags, 0)
	if err != nil {
		return nil, filePathError(src, err, os.Lstat)
	}
	return checkOpenedRegularFile(file, src)
}

// openStatedSource opens a single source that Prepare has already stat'ed
// and resolved. If it is gone by now, it was deleted or replaced in between:
// that is a source change, not a usage error.
func openStatedSource(src string) (*os.File, error) {
	file, err := openSingleSource(src)
	var pe *fs.PathError
	if err != nil && errors.Is(err, fs.ErrNotExist) && errors.As(err, &pe) {
		return nil, fmt.Errorf("%w: %w", ErrSourceChanged, pe)
	}
	return file, err
}

func checkOpenedRegularFile(file *os.File, name string) (*os.File, error) {
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, usageErrorf("%s is no longer a regular file", name)
	}
	return file, nil
}
