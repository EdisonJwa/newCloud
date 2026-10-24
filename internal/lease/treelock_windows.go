//go:build windows

package lease

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var errLocked = errors.New("locked")

func lockFile(f *os.File) error {
	var ol windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLocked
	}
	return fmt.Errorf("LockFileEx: %w", err)
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol); err != nil {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}
