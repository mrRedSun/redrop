//go:build unix

package client

import (
	"os"
	"syscall"
)

const regularFileOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK
