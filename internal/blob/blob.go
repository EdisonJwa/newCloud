// Package blob defines the content-addressed storage boundary.
//
// The interface is intentionally tiny. Anything backend-specific (buckets,
// regions, ACLs, multipart APIs, listing) stays out: multipart upload, versions,
// quotas and deduplication all live above this layer, which is why a third
// backend only needs Put/Open/Delete/Stat.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"time"
)

// Hash is a content address: the SHA-256 of the stored bytes.
type Hash [32]byte

// ErrNotFound is returned when a blob is absent.
var ErrNotFound = errors.New("blob: not found")

// Info describes a stored blob.
type Info struct {
	Hash Hash
	Size int64
}

// Store is the storage backend contract.
//
// Open must support seeking: HTTP Range requests are served by seeking into the
// blob, and buffering a 100 GiB file to satisfy a byte range is not an option.
type Store interface {
	// Put stores r under h. It is idempotent: putting the same hash twice is a
	// no-op and must not corrupt an existing object. Implementations verify
	// that the bytes hash to h and return ErrHashMismatch otherwise.
	Put(ctx context.Context, h Hash, r io.Reader, size int64) error

	// Open returns a reader positioned at the start of the blob.
	Open(ctx context.Context, h Hash) (io.ReadSeekCloser, error)

	// Delete removes the blob. Returns ErrNotFound if absent.
	Delete(ctx context.Context, h Hash) error

	// Stat returns metadata without reading the content.
	Stat(ctx context.Context, h Hash) (Info, error)
}

// ErrHashMismatch is returned by Put when the payload does not match its
// claimed content address. Silently trusting the caller would let a
// corrupted or truncated upload poison the dedup index for every user.
var ErrHashMismatch = errors.New("blob: content hash mismatch")

// String renders a hash as lowercase hex.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// Prefix returns the leading n hex characters, used for storage sharding
// (e.g. n=4 -> "ab/cd/...").
func (h Hash) Prefix(n int) string {
	s := h.String()
	if n > len(s) {
		n = len(s)
	}
	return s[:n]
}

// Hasher computes a content address while data streams through it.
type Hasher struct {
	h hash.Hash
	n int64
}

// NewHasher returns a Hasher that also counts bytes.
func NewHasher() *Hasher { return &Hasher{h: sha256.New()} }

func (w *Hasher) Write(p []byte) (int, error) {
	n, err := w.h.Write(p)
	w.n += int64(n)
	return n, err
}

// Sum returns the hash and byte count observed so far.
func (w *Hasher) Sum() (Hash, int64) {
	var out Hash
	copy(out[:], w.h.Sum(nil))
	return out, w.n
}

// TeeReader hashes everything read through it. Uploads use it to compute the
// content address without buffering the payload.
func TeeReader(r io.Reader) (io.Reader, *Hasher) {
	hw := NewHasher()
	return io.TeeReader(r, hw), hw
}

// UploadCleaner is implemented by backends that can leave partial uploads behind, so a maintenance
// pass can reclaim them.
//
// It is NOT part of Store. A third backend must only implement Put/Open/Delete/Stat, and adding a
// fifth method to the core interface would force every backend to write a method that most of them
// would stub out with `return nil`. An optional interface lets a caller ask "can you clean up after
// yourself" and treat "no" as a complete answer.
//
// §9.3: 清理未完成 Multipart Upload；AWS 明确建议对残留分块设置生命周期规则，否则可能持续产生费用.
//
// Both shipped backends need this, for different reasons: an S3 backend stages large objects as
// multipart uploads and an interrupted one leaves billable parts; the local backend writes to a
// temp file before renaming it into place, and a crash mid-write leaves that temp file behind.
// Neither is visible as an object, which is exactly why neither is noticed — nothing lists them.
type UploadCleaner interface {
	// AbortStaleUploads discards partial uploads older than olderThan and reports what it
	// reclaimed.
	//
	// The age bound is not optional. Aborting a fresh upload would destroy a transfer that is
	// still in progress, so a caller must pass a window comfortably longer than the largest
	// upload it accepts.
	AbortStaleUploads(ctx context.Context, olderThan time.Duration) (UploadCleanupResult, error)
}

// UploadCleanupResult reports what a cleanup pass reclaimed.
type UploadCleanupResult struct {
	// Aborted is how many partial uploads were discarded.
	Aborted int
	// Bytes is how much staged content was reclaimed, when the backend knows. Zero means "not
	// measured", which is different from "nothing was reclaimed" and is reported rather than
	// guessed.
	Bytes int64
}
