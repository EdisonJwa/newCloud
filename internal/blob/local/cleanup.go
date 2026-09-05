package local

// §9.3: 清理未完成 Multipart Upload, the local-layout half.
//
// This backend has no multipart API, but it has the same failure: Put writes to a temp file and
// renames it into place, so that a reader never observes a half-written object. The rename is
// atomic; the temp file is not. A crash, a cancelled request or a full disk between CreateTemp and
// Rename leaves a put-* file behind, and nothing ever lists or removes it — it is not an object, so
// the object listing cannot see it, and the deferred Remove only runs if the process survives.
//
// Over time that is unbounded disk growth with no reference anywhere, which is exactly the class of
// leak the requirement is about even though the mechanism is different.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EdisonJwa/newCloud/internal/blob"
)

// AbortStaleUploads removes temp files left by interrupted writes.
func (s *Store) AbortStaleUploads(ctx context.Context, olderThan time.Duration) (blob.UploadCleanupResult, error) {
	var res blob.UploadCleanupResult
	if olderThan <= 0 {
		// Refuse rather than default: a zero window would delete the temp file of a write that is
		// in progress right now, turning a cleanup pass into data loss for a live upload.
		return res, fmt.Errorf("local: AbortStaleUploads needs a positive age window, got %v", olderThan)
	}
	cutoff := time.Now().Add(-olderThan)

	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		if os.IsNotExist(err) {
			// No temp directory means no partial writes ever happened here.
			return res, nil
		}
		return res, fmt.Errorf("local: read temp dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "put-") {
			// Anything that is not one of our own temp files is left alone. The temp directory is
			// ours, but a bug that widened this glob into a delete-everything would be
			// unrecoverable, so the prefix is checked rather than assumed.
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			// Vanished between ReadDir and Info: already cleaned up, not an error.
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if rerr := os.Remove(filepath.Join(s.tmp, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			return res, fmt.Errorf("local: remove stale temp %s: %w", e.Name(), rerr)
		}
		res.Aborted++
		res.Bytes += info.Size()
	}
	return res, nil
}
