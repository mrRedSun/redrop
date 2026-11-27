//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"syscall"
)

// lockOpenFlags never opens a lock file through a symlink.
const lockOpenFlags = os.O_CREATE | os.O_RDWR | syscall.O_NOFOLLOW

// tryLock takes an exclusive advisory lock on f without waiting; errLocked
// means another process (or another open of the file) holds it. The kernel
// releases it when the holder closes the file or exits.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}

func unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
