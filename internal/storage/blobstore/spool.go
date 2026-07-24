package blobstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/EdisonJwa/newCloud/internal/blob"
)

// spoolToTemp buffers an unknown-length stream on disk.
//
// The blob store needs the content address before it can store an object, so the
// payload must be read once to completion somewhere. Buffering in memory would make
// a 100 GiB upload consume 100 GiB of RAM; spilling to a temp file keeps memory flat
// and lets the same bytes be replayed for the verified write.
//
// dir is the caller's staging directory, not the system temp dir. That matters for
// two reasons: a container may have a read-only /tmp, and the spool file should live
// on the same filesystem as the final blob so the eventual publish is a rename
// rather than a copy. Empty dir falls back to the system temp directory, which is
// only appropriate for small development setups.
func spoolToTemp(dir string, r io.Reader, hasher *blob.Hasher) (*os.File, blob.Hash, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, blob.Hash{}, fmt.Errorf("blobstore: prepare spool dir: %w", err)
		}
	}

	f, err := os.CreateTemp(dir, "newcloud-spool-*")
	if err != nil {
		return nil, blob.Hash{}, fmt.Errorf("blobstore: create spool: %w", err)
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}

	// Unlink immediately where the platform allows it, so an abandoned upload cannot
	// fill the staging area: the file disappears as soon as the descriptor closes.
	// On Windows this fails harmlessly and the explicit Remove in cleanup wins.
	_ = os.Remove(f.Name())

	if _, err := io.Copy(io.MultiWriter(f, hasher), r); err != nil {
		cleanup()
		return nil, blob.Hash{}, fmt.Errorf("blobstore: spool: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, blob.Hash{}, fmt.Errorf("blobstore: rewind spool: %w", err)
	}

	h, _ := hasher.Sum()
	return f, h, nil
}

// spoolDirFor derives the staging directory from the blob root.
//
// Keeping it inside the root is what makes the publish step a rename(2); the same
// rule the local backend enforces at startup.
func spoolDirFor(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, "tmp")
}
