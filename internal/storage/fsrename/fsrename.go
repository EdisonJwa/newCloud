// Package fsrename provides the conditional rename primitive ADR-0013 requires.
//
// A no-replace publish is not Stat-then-Rename: two processes can both pass the
// Stat. Each platform function below asks the kernel to fail if the destination
// exists. If the running platform or filesystem cannot provide that, the caller
// must fail closed; there is no fallback to os.Rename.
package fsrename

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	// ErrUnsupported means the platform/filesystem cannot prove no-replace.
	ErrUnsupported = errors.New("fsrename: no-replace rename unsupported")
	// ErrExist means the destination already exists; the source was not moved.
	ErrExist = errors.New("fsrename: destination exists")
	// ErrCrossDevice means the source and destination are not on one filesystem.
	ErrCrossDevice = errors.New("fsrename: cross-device rename")
)

// RenameNoReplace atomically renames from to to only when to is absent. On
// success the source is gone and the destination holds the source's inode.
func RenameNoReplace(from, to string) error { return renameNoReplace(from, to) }

// Probe verifies the primitive on the actual directory that will hold live
// content. A probe in a temp directory on another volume proves nothing.
func Probe(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("fsrename: probe mkdir: %w", err)
	}
	base := filepath.Join(dir, ".fsrename-probe-"+randomSuffix())
	src := base + "-src"
	dst := base + "-dst"
	defer os.Remove(src)
	defer os.Remove(dst)

	if err := os.WriteFile(src, []byte("probe"), 0o600); err != nil {
		return fmt.Errorf("fsrename: probe write: %w", err)
	}
	if err := os.WriteFile(dst, []byte("occupied"), 0o600); err != nil {
		return fmt.Errorf("fsrename: probe destination: %w", err)
	}
	if err := RenameNoReplace(src, dst); !errors.Is(err, ErrExist) {
		return fmt.Errorf("fsrename: probe target-exists check = %v, want ErrExist", err)
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("fsrename: probe moved the source despite the target existing: %w", err)
	}
	if err := os.Remove(dst); err != nil {
		return fmt.Errorf("fsrename: probe cleanup target: %w", err)
	}
	if err := RenameNoReplace(src, dst); err != nil {
		return fmt.Errorf("fsrename: probe free-target rename: %w", err)
	}
	return nil
}
