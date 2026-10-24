//go:build darwin

package fsrename

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func renameNoReplace(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return fmt.Errorf("%w: %v", ErrExist, err)
	case errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %v", ErrCrossDevice, err)
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	default:
		return fmt.Errorf("fsrename: renamex_np: %w", err)
	}
}
