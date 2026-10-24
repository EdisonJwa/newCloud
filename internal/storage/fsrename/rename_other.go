//go:build !windows && !linux && !darwin

package fsrename

import "runtime"

func renameNoReplace(from, to string) error {
	return &unsupportedError{goos: runtime.GOOS}
}

type unsupportedError struct{ goos string }

func (e *unsupportedError) Error() string {
	return "fsrename: no-replace rename is not implemented on " + e.goos
}

func (e *unsupportedError) Is(target error) bool { return target == ErrUnsupported }
