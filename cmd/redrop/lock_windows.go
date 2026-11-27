//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const lockOpenFlags = os.O_CREATE | os.O_RDWR

// tryLock takes an exclusive lock on f's first byte without waiting;
// errLocked means another handle holds it. Windows releases it when the
// handle closes or the process exits.
func tryLock(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLocked
	}
	return err
}

func unlock(f *os.File) { windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{}) }
