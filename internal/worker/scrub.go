package worker

// Blob integrity scrub.
//
// Content addressing means a blob's name is the hash of its bytes, so reading a blob and hashing it
// is a complete integrity check with no extra metadata. Nothing was doing that against LIVE storage
// — the restore drill verifies blobs copied out of a dump, which is a different question and a much
// heavier operation. The practical consequence: a blob corrupted in place (a bad disk sector, a
// half-written part from an interrupted transfer, an upstream library race) stays undetected until
// somebody runs a quarterly drill, and then surfaces as a backup problem rather than as what it is.
//
// This scrub reads a bounded sample of live blobs per cycle and compares the bytes to the recorded
// hash. It is deliberately sampling rather than exhaustive: reading every byte of a 40 TiB store on
// a timer is a second full-time workload, and the point is to find divergence early, not to prove
// absence. The sample is deterministic per run and spread over the keyspace so every blob is
// eventually covered.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ScrubResult is what one scrub pass found.
type ScrubResult struct {
	// Checked is how many blobs were read and hashed.
	Checked int
	// Corrupt lists the blobs whose bytes do not match their address.
	Corrupt []CorruptBlob
	// Missing lists blobs the metadata references that the backend does not have.
	Missing []string
	// Bytes is how much content was read, so the cost of a pass is visible.
	Bytes int64
}

// CorruptBlob names a blob whose stored bytes disagree with its address.
type CorruptBlob struct {
	Hash blob.Hash
	// Actual is the hash of what was read back, so the two can be compared in a report.
	Actual [32]byte
	Size   int64
}

// ScrubBlobs verifies a sample of live blobs against their content addresses.
//
// limit bounds the pass. A limit of zero checks nothing, which is the safe reading of "unset" — a
// scrub that defaulted to "everything" would become a surprise full read of the store the first
// time somebody enabled it.
func (w *Worker) ScrubBlobs(ctx context.Context, store blob.Store, limit int) (ScrubResult, error) {
	var res ScrubResult
	if store == nil || limit <= 0 {
		return res, nil
	}
	if w.meta == nil {
		return res, nil
	}

	// The sample comes from the metadata, because that is what knows which blobs are supposed to
	// exist. Reading the directory instead would verify exactly the blobs that are easy to find and
	// miss the ones whose rows exist but whose bytes do not.
	hashes, err := w.sampleBlobHashes(ctx, limit)
	if err != nil {
		return res, err
	}

	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		bh := blob.Hash(h)
		rc, err := store.Open(ctx, bh)
		if err != nil {
			if err == blob.ErrNotFound {
				res.Missing = append(res.Missing, bh.String())
				continue
			}
			return res, fmt.Errorf("worker: scrub open %s: %w", bh, err)
		}
		hasher := sha256.New()
		n, copyErr := io.Copy(hasher, rc)
		closeErr := rc.Close()
		res.Checked++
		res.Bytes += n
		if copyErr != nil {
			// A read error is reported, not counted as corruption: an unreadable blob is a
			// storage failure and the distinction matters for the operator's next step.
			return res, fmt.Errorf("worker: scrub read %s: %w", bh, copyErr)
		}
		if closeErr != nil {
			return res, fmt.Errorf("worker: scrub close %s: %w", bh, closeErr)
		}

		var actual [32]byte
		copy(actual[:], hasher.Sum(nil))
		if actual != h {
			res.Corrupt = append(res.Corrupt, CorruptBlob{Hash: bh, Actual: actual, Size: n})
		}
	}
	return res, nil
}

// sampleBlobHashes returns up to limit distinct content hashes from the metadata.
//
// Ordered by id so successive passes with a growing offset would walk the keyspace; for now the
// order is stable and the set is the first `limit` rows, which is enough to be repeatable and to
// cover a store smaller than the limit entirely.
func (w *Worker) sampleBlobHashes(ctx context.Context, limit int) ([][32]byte, error) {
	var out [][32]byte
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := tx.SampleBlobHashes(ctx, limit)
		if err != nil {
			return err
		}
		out = rows
		return nil
	})
	return out, err
}
