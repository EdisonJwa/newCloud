//go:build !windows && !unix

package lease

import (
	"errors"
	"os"
)

var errLocked = errors.New("locked")

func lockFile(f *os.File) error {
	return errors.New("lease: Tree OS lock is not implemented on this platform")
}

func unlockFile(f *os.File) error { return nil }
