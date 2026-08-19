package store

// SHARE-05 速率: the upload rate limit.
//
// The other four limits in SHARE-05 are totals — file count, per-file size, total space, retention
// — and a column plus a guard in the same UPDATE keeps them exact. A rate is different in kind: it
// is a rolling window, and no column can represent "how many arrived in the last N minutes"
// because the answer changes as time passes, without any write happening. The honest representation
// is the events themselves, so this is a table.
//
// Why not an in-process token bucket, which is the obvious implementation: the worker is already
// written to tolerate a second replica, so a deployment may well have several. An in-memory limiter
// would give each replica its own allowance, making the effective limit N × replicas and changing
// it whenever a pod rescheduled. It would also reset on restart, which turns "restart the process"
// into a way to clear the budget. The rows are durable for the same reason the audit log is.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrShareRateLimited is returned when a share's upload rate has been reached.
//
// Distinct from ErrShareExhausted on purpose: exhaust means the allowance is gone for good, rate
// means wait. They lead a visitor to different next steps and an operator to different causes, so
// collapsing them would make both less useful.
var ErrShareRateLimited = errors.New("store: share upload rate exceeded")

// ShareRateTx is the rate-limit surface.
type ShareRateTx interface {
	// AllowShareUpload counts recent uploads and, when there is room, records this one.
	//
	// It must be a single atomic decision. A separate count followed by a separate insert lets two
	// concurrent uploads both observe "one under the limit", which is how a limit of 10 accepts 20
	// — the same failure the download cap had before it was made atomic.
	AllowShareUpload(ctx context.Context, id uuid.UUID, rate int64, window time.Duration,
		ip string, bytes int64) error

	// PruneShareUploadEvents deletes rate-limit rows older than the longest window in use,
	// bounding the table.
	PruneShareUploadEvents(ctx context.Context, olderThan time.Duration) (int, error)
}

// ShareUploadEvent is one accepted upload, as recorded for rate limiting.
type ShareUploadEvent struct {
	ShareID uuid.UUID
	IP      string
	Bytes   int64
}
