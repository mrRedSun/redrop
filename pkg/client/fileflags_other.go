//go:build !unix

package client

import "os"

const regularFileOpenFlags = os.O_RDONLY
