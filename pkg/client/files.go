package client

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func bundlePathError(name string, err error) error {
	cause := err
	for errors.Unwrap(cause) != nil {
		cause = errors.Unwrap(cause)
	}
	// Go 1.26 does not export os.Root's path-escape sentinel. Match only that
	// leaf error; permission and other I/O failures remain runtime errors.
	if errors.Is(err, fs.ErrNotExist) || cause.Error() == "path escapes from parent" {
		return &UsageError{
			Message: fmt.Sprintf("cannot access %q: symlinks must be relative and stay inside the directory: %v", name, err),
			Cause:   err,
		}
	}
	return fmt.Errorf("access bundle path %q: %w", name, err)
}

// openRegularFile checks before opening so an ordinary FIFO cannot block a
// publish, and checks the opened descriptor again before reading any bytes.
func openRegularFile(root *os.Root, name string) (*os.File, error) {
	local := filepath.FromSlash(name)
	info, err := root.Stat(local)
	if err != nil {
		return nil, bundlePathError(name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, usageErrorf("%s is not a regular file", name)
	}
	file, err := root.Open(local)
	if err != nil {
		return nil, bundlePathError(name, err)
	}
	info, err = file.Stat()
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

func openSingleSource(src string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return nil, bundlePathError(src, err)
	}
	defer root.Close()
	return openRegularFile(root, filepath.Base(src))
}
