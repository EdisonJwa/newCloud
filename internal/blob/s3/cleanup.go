package s3

// §9.3: 清理未完成 Multipart Upload. AWS explicitly warns that leftover parts keep costing money,
// and the package comment used to say only that an aborted upload "leaves no visible object" —
// true, and beside the point: the parts are invisible, which is why nothing notices them.
//
// A completion or abort is the normal end of a multipart upload. The abnormal end is a crash, a
// cancelled request, or a client that walked away mid-PUT. minio-go does not abort those for us, so
// they persist until something lists and removes them.

import (
	"context"
	"fmt"
	"time"

	"github.com/EdisonJwa/newCloud/internal/blob"
)

// AbortStaleUploads discards incomplete multipart uploads older than olderThan.
//
// The age test uses the server's own Initiated time rather than anything we recorded, because we
// recorded nothing: an interrupted upload has no row in our metadata at all. That is also why this
// can only be a sweeping pass rather than a targeted repair.
func (s *Store) AbortStaleUploads(ctx context.Context, olderThan time.Duration) (blob.UploadCleanupResult, error) {
	var res blob.UploadCleanupResult
	if olderThan <= 0 {
		// Refuse rather than default. A zero window would abort transfers that are in progress
		// right now, and a caller who forgot to configure one would lose live uploads.
		return res, fmt.Errorf("s3: AbortStaleUploads needs a positive age window, got %v", olderThan)
	}
	cutoff := time.Now().Add(-olderThan)

	// recursive=true: objects are stored under the configured prefix, and a non-recursive listing
	// would walk only its top level.
	for info := range s.client.ListIncompleteUploads(ctx, s.bucket, s.prefix, true) {
		if info.Err != nil {
			return res, fmt.Errorf("s3: list incomplete uploads: %w", info.Err)
		}
		// A fresh upload may be a transfer in progress. This is the whole safety mechanism, so it
		// is checked before anything is removed.
		if !info.Initiated.Before(cutoff) {
			continue
		}
		if err := s.client.RemoveIncompleteUpload(ctx, s.bucket, info.Key); err != nil {
			return res, fmt.Errorf("s3: abort incomplete upload %q: %w", info.Key, err)
		}
		res.Aborted++
	}
	return res, nil
}
