package lease

import (
	"errors"
	"testing"
)

func TestTreeLockIsExclusiveAndReleasable(t *testing.T) {
	dir := t.TempDir()
	first, err := LockTree(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := LockTree(dir); !errors.Is(err, ErrTreeLocked) {
		t.Fatalf("second lock = %v, want ErrTreeLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	third, err := LockTree(dir)
	if err != nil {
		t.Fatalf("relock after close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}
