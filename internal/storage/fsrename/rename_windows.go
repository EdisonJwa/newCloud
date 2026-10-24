//go:build windows

package fsrename

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func renameNoReplace(from, to string) error {
	fromp, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	top, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// Flags 0 deliberately omit MOVEFILE_REPLACE_EXISTING: the call fails when the
	// destination exists. MoveFileEx is not os.Rename, which replaces on Windows.
	err = windows.MoveFileEx(fromp, top, 0)
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS), errors.Is(err, windows.ERROR_FILE_EXISTS):
		return fmt.Errorf("%w: %v", ErrExist, err)
	case errors.Is(err, windows.ERROR_NOT_SAME_DEVICE):
		return fmt.Errorf("%w: %v", ErrCrossDevice, err)
	case errors.Is(err, windows.ERROR_INVALID_FUNCTION), errors.Is(err, windows.ERROR_NOT_SUPPORTED):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	default:
		return fmt.Errorf("fsrename: MoveFileEx: %w", err)
	}
}
