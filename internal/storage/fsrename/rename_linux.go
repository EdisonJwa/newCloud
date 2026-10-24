//go:build linux

package fsrename

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return fmt.Errorf("%w: %v", ErrExist, err)
	case errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %v", ErrCrossDevice, err)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.EOPNOTSUPP):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	default:
		return fmt.Errorf("fsrename: renameat2: %w", err)
	}
}
