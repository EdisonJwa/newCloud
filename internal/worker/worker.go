// Package worker runs the periodic maintenance tasks.
//
// Everything here is idempotent and safe to run from several processes: the
// cleanup queries are written so that a duplicate run is wasteful but never
// wrong. That property is what lets the deployment start with one process and add
// replicas later without changing this code.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// Worker runs the periodic jobs.
type Worker struct {
	meta store.Store
	log  *slog.Logger
	// intervals are configurable so tests can run them fast.
	lockInterval  time.Duration
	sweepInterval time.Duration
	blobGrace     time.Duration
}

// New builds a worker with production defaults.
func New(meta store.Store, log *slog.Logger) *Worker {
	return &Worker{
		meta:          meta,
		log:           log,
		lockInterval:  time.Minute,
		sweepInterval: 6 * time.Hour,
		blobGrace:     7 * 24 * time.Hour,
	}
}

// Run blocks until the context is cancelled.
//
// Each ticker runs independently: a slow blob sweep must not delay lock expiry,
// because a stale lock blocks a user's edits immediately while an unreferenced
// blob costs only disk.
func (w *Worker) Run(ctx context.Context) {
	locks := time.NewTicker(w.lockInterval)
	defer locks.Stop()
	sweeps := time.NewTicker(w.sweepInterval)
	defer sweeps.Stop()

	w.log.Info("worker started",
		"lock_interval", w.lockInterval, "sweep_interval", w.sweepInterval)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopping")
			return
		case <-locks.C:
			if n, err := w.ExpireLocks(ctx); err != nil {
				w.log.Error("expire locks failed", "error", err)
			} else if n > 0 {
				w.log.Info("expired locks", "count", n)
			}
		case <-sweeps.C:
			if n, err := w.SweepBlobs(ctx); err != nil {
				w.log.Error("blob sweep failed", "error", err)
			} else if n > 0 {
				w.log.Info("collected orphaned blobs", "count", n)
			}
		}
	}
}

// ExpireLocks deletes locks past their timeout.
//
// A lock left behind by a crashed client would otherwise block edits forever:
// WebDAV has no mechanism to notice a client is gone, so expiry is the only way a
// stale lock is released.
func (w *Worker) ExpireLocks(ctx context.Context) (int, error) {
	var n int
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		expired, err := tx.ExpireLocks(ctx)
		if err != nil {
			return err
		}
		n = expired
		return nil
	})
	return n, err
}

// SweepBlobs deletes content whose references are gone and whose grace period has
// passed.
//
// The grace period is the whole safety mechanism: without it, deleting a file and
// then hitting "undo" would find the bytes already gone. Seven days is long enough
// that a user notices and restores, short enough that the disk does not fill with
// forgotten content.
func (w *Worker) SweepBlobs(ctx context.Context) (int, error) {
	var candidates []store.BlobRef
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := tx.CollectableBlobs(ctx, w.blobGrace)
		if err != nil {
			return err
		}
		candidates = refs
		return nil
	})
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, ref := range candidates {
		// Delete the row first: if the process dies midway, the bytes are orphaned
		// rather than the row pointing at nothing. An orphan costs disk; a dangling
		// row costs a broken file for a user.
		err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.DeleteBlob(ctx, ref.ID)
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return removed, fmt.Errorf("worker: delete blob %s: %w", ref.ID, err)
		}
		removed++
	}
	return removed, nil
}
