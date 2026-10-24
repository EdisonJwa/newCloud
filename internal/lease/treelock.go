package lease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrTreeLocked means another process holds the Tree root's OS lock.
var ErrTreeLocked = errors.New("lease: Tree root is locked by another process")

// TreeLock is the process-lifetime OS lock on a Tree content root. It is a
// second line of defence behind the PostgreSQL lease: Tree is local-disk-only, so
// a process that loses the database lease cannot be replaced on the same host
// while this lock is still held.
type TreeLock struct {
	f *os.File
}

// LockTree takes the exclusive OS lock for root. It never waits: startup must
// fail closed rather than block behind an old process.
func LockTree(root string) (*TreeLock, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("lease: create tree root: %w", err)
	}
	p := filepath.Join(root, ".newcloud-instance.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lease: open tree lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, errLocked) {
			return nil, fmt.Errorf("%w: %s", ErrTreeLocked, p)
		}
		return nil, fmt.Errorf("lease: lock tree root: %w", err)
	}
	return &TreeLock{f: f}, nil
}

// Close releases the OS lock. It is safe to call more than once.
func (l *TreeLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
