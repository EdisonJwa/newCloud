//go:build unix

package lease

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var errLocked = errors.New("locked")

func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errLocked
	}
	return fmt.Errorf("flock: %w", err)
}

func unlockFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("flock unlock: %w", err)
	}
	return nil
}
