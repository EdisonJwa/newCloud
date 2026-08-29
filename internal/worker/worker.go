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

	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// Worker runs the periodic jobs.
type Worker struct {
	meta store.Store
	log  *slog.Logger
	// blobs is the content store, needed by the integrity scrub. Optional: a worker without it
	// simply does not scrub, and every other duty is unaffected.
	blobs BlobReader
	// scrubLimit is how many blobs one pass verifies. Zero disables the scrub, which is the safe
	// reading of "unset" — defaulting to "everything" would make enabling it a surprise full read
	// of the store.
	scrubLimit int
	// scrubInterval is how often a pass runs. Separate from the sweep because the two have
	// different costs and different urgency: the sweep reclaims unreferenced bytes and can wait
	// six hours, while a corrupted blob is a file a user cannot open, and finding it within the
	// hour is the difference between "we caught it" and "the backups were never valid".
	scrubInterval time.Duration
	// intervals are configurable so tests can run them fast.
	lockInterval  time.Duration
	sweepInterval time.Duration
	blobGrace     time.Duration
	// purgeInterval is when expired trash and versions are deleted. Separate from
	// the blob sweep because the two answer different questions: the sweep reclaims
	// bytes nothing points at, the purge removes rows the retention policy says are
	// no longer wanted.
	purgeInterval time.Duration
	// retention comes from configuration. Zero means "never", so a deployment that
	// has not decided a retention policy deletes nothing.
	versionRetention time.Duration
	trashRetention   time.Duration

	// jobInterval is how often the queue is polled. Short enough that work queued by
	// a request is picked up in the same second a user is still watching, long enough
	// that an idle deployment is not querying constantly.
	jobInterval time.Duration
	// jobBatch bounds how many jobs one round takes, so a burst cannot starve the
	// maintenance loops.
	jobBatch int
	// jobLease is how long a claimed job stays owned by this worker. A handler that
	// runs longer than this may be run twice, which is why handlers must be
	// idempotent.
	jobLease time.Duration
	// workerID identifies this process in the jobs table.
	workerID string
	// handlers maps a job kind to its implementation.
	handlers map[string]JobHandler

	// anchor publishes the audit chain head, and chain reads the chain it verifies.
	// Both are nil when anchoring is not configured, which is why every use is guarded.
	anchor *auditverify.FileAnchor
	chain  auditverify.Chain
	// anchorInterval is how often the anchor step runs. Daily would be enough; a short
	// interval means a long-running process does not miss the day boundary, and the
	// step is idempotent so extra runs cost one directory listing.
	anchorInterval time.Duration

	// metrics is optional; nil means nothing is recorded.
	metrics *metrics.Registry
}

// WithBlobScrub enables the integrity scrub over blobs, verifying up to limit blobs per pass.
//
// Kept separate from WithMetrics because it needs the content store, which the worker otherwise
// does not touch — and because a deployment that has not thought about the read cost should not get
// a scrub by accident.
func (w *Worker) WithBlobScrub(blobs BlobReader, limit int) *Worker {
	w.blobs = blobs
	w.scrubLimit = limit
	return w
}

// WithScrubInterval sets how often the integrity scrub runs. Zero keeps the sweep cadence.
func (w *Worker) WithScrubInterval(d time.Duration) *Worker {
	w.scrubInterval = d
	return w
}

// WithMetrics attaches a metrics registry.
func (w *Worker) WithMetrics(r *metrics.Registry) *Worker {
	w.metrics = r
	return w
}

// New builds a worker with production defaults.
func New(meta store.Store, log *slog.Logger) *Worker {
	w := &Worker{
		meta:          meta,
		log:           log,
		lockInterval:  time.Minute,
		sweepInterval: 6 * time.Hour,
		blobGrace:     7 * 24 * time.Hour,
		purgeInterval: 6 * time.Hour,
		jobInterval:   5 * time.Second,
		jobBatch:      10,
		jobLease:      5 * time.Minute,
		workerID:      jobWorkerID(),
	}
	w.registerJobs()
	w.anchorInterval = time.Hour
	return w
}

// WithRetention sets the data-retention policy.
//
// It is a setter rather than a constructor argument so the zero value stays safe:
// a worker built without it retains everything, which is the only defensible
// default for a setting that deletes a user's data.
func (w *Worker) WithRetention(versions, trash time.Duration) *Worker {
	w.versionRetention = versions
	w.trashRetention = trash
	return w
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
	purges := time.NewTicker(w.purgeInterval)
	defer purges.Stop()
	retention := time.NewTicker(w.purgeInterval)
	defer retention.Stop()
	ratePrune := time.NewTicker(w.purgeInterval)
	defer ratePrune.Stop()
	jobs := time.NewTicker(w.jobInterval)
	defer jobs.Stop()
	anchors := time.NewTicker(w.anchorInterval)
	defer anchors.Stop()
	// Archiving health changes slowly but its consequences are severe: a broken archive_command
	// leaves pg_wal growing until the disk fills. Sampled on the sweep cadence.
	walSample := time.NewTicker(w.sweepInterval)
	defer walSample.Stop()
	// The scrub is the only check that reads live content back. It runs on the sweep cadence
	// because a corrupted blob is a storage problem, not a latency one.
	scrubEvery := w.scrubInterval
	if scrubEvery <= 0 {
		scrubEvery = w.sweepInterval
	}
	scrubs := time.NewTicker(scrubEvery)
	defer scrubs.Stop()

	w.log.Info("worker started",
		"lock_interval", w.lockInterval, "sweep_interval", w.sweepInterval,
		"purge_interval", w.purgeInterval,
		"version_retention", w.versionRetention, "trash_retention", w.trashRetention)

	if w.trashRetention <= 0 && w.versionRetention <= 0 {
		// Say so loudly. "Retention is configured" and "retention does nothing"
		// look identical from outside, and an operator who believes the second is
		// the first discovers it when the disk fills.
		w.log.Warn("no data retention is configured: trash and versions are kept forever",
			"hint", "set limits.trash_retention_days and limits.version_retention_days")
	}

	// One pass at startup: a process that restarts more often than the interval would
	// otherwise never anchor at all.
	if err := w.anchorOnce(ctx); err != nil {
		w.log.Error("audit anchoring failed", "error", err)
	}

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
			if n, err := w.ExpireDownloadSessions(ctx); err != nil {
				w.log.Error("expire download sessions failed", "error", err)
			} else if n > 0 {
				w.log.Info("expired download sessions", "count", n)
			}
		case <-scrubs.C:
			// Integrity: read a sample of live blobs and compare to their addresses. Distinct
			// from the sweep, which reclaims UNREFERENCED content — this looks for content that
			// is referenced and WRONG, which the sweep cannot see by construction.
			w.runScrub(ctx)
		case <-sweeps.C:
			n, err := w.SweepBlobs(ctx)
			// The collector is the only thing that reclaims disk, so both its successes and its
			// failures are worth publishing: a run that deletes nothing is normal, a run that
			// errors means space is being held that should have been freed. Recording only
			// successes would make the error counter permanently zero and the alert on it dead.
			if w.metrics != nil {
				w.metrics.ObserveBlobGC(n, err != nil)
			}
			if err != nil {
				w.log.Error("blob sweep failed", "error", err)
			} else if n > 0 {
				w.log.Info("collected orphaned blobs", "count", n)
			}
		case <-walSample.C:
			// A broken archiver does not surface anywhere else: the database keeps serving, the
			// application keeps writing, and the only visible symptom is pg_wal growing until the
			// disk fills. Sampling on its own timer is what turns that into an alert.
			w.sampleWALArchive(ctx)
		case <-anchors.C:
			if err := w.anchorOnce(ctx); err != nil {
				w.log.Error("audit anchoring failed", "error", err)
			}
		case <-jobs.C:
			// A failure here is logged and the loop continues: the queue is durable, so
			// a transient database error means the work waits rather than being lost.
			if n, err := w.ProcessJobs(ctx); err != nil {
				w.log.Error("job processing failed", "error", err)
			} else if n > 0 {
				w.log.Debug("processed jobs", "count", n)
			}
		case <-ratePrune.C:
			w.pruneRateLimitRows(ctx)
		case <-retention.C:
			// Share retention (SHARE-05 保留期限). Separate from PurgeTrash: this moves
			// expired deliveries to the trash, and the trash window still applies before
			// anything is destroyed.
			w.expireRetained(ctx)
		case <-purges.C:
			if res, err := w.PurgeTrash(ctx); err != nil {
				w.log.Error("trash purge failed", "error", err)
			} else if res.Roots > 0 {
				w.log.Info("purged expired trash",
					"roots", res.Roots, "nodes", res.Nodes, "bytes", res.Bytes)
			}
			if n, err := w.ExpireVersions(ctx); err != nil {
				w.log.Error("version expiry failed", "error", err)
			} else if n > 0 {
				w.log.Info("expired old versions", "count", n)
			}
		}
	}
}

// PurgeTrash deletes trash older than the configured retention.
func (w *Worker) PurgeTrash(ctx context.Context) (store.PurgeResult, error) {
	var res store.PurgeResult
	if w.trashRetention <= 0 {
		return res, nil
	}
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, perr := tx.PurgeTrash(ctx, w.trashRetention)
		res = got
		return perr
	})
	return res, err
}

// ExpireVersions deletes version rows older than the configured retention.
func (w *Worker) ExpireVersions(ctx context.Context) (int, error) {
	if w.versionRetention <= 0 {
		return 0, nil
	}
	var n int
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, eerr := tx.ExpireVersions(ctx, w.versionRetention)
		n = got
		return eerr
	})
	return n, err
}

// ExpireDownloadSessions clears download sessions past their TTL so the table does
// not grow without bound.
func (w *Worker) ExpireDownloadSessions(ctx context.Context) (int, error) {
	var n int
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, eerr := tx.ExpireDownloadSessions(ctx)
		n = got
		return eerr
	})
	return n, err
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

// runScrub performs one integrity pass and reports it.
//
// Corruption is logged at error level and never swallowed: the whole value of the scrub is that
// somebody finds out. A missing blob is logged separately because it means something deleted
// content the metadata still references, which is a different investigation.
func (w *Worker) runScrub(ctx context.Context) {
	if w.blobs == nil || w.scrubLimit <= 0 {
		return
	}
	res, err := w.ScrubBlobs(ctx, w.blobs, w.scrubLimit)
	if err != nil {
		w.log.Error("blob integrity scrub failed", "error", err)
		return
	}
	if w.metrics != nil {
		w.metrics.SetBlobScrub(res.Checked, len(res.Corrupt), len(res.Missing))
	}
	for _, c := range res.Corrupt {
		w.log.Error("blob content does not match its address",
			"blob", c.Hash.String(), "actual_sha256", fmt.Sprintf("%x", c.Actual[:]),
			"size", c.Size)
	}
	for _, m := range res.Missing {
		w.log.Error("blob is referenced by metadata but absent from storage", "blob", m)
	}
	if res.Checked > 0 {
		w.log.Debug("blob integrity scrub", "checked", res.Checked, "bytes", res.Bytes,
			"corrupt", len(res.Corrupt), "missing", len(res.Missing))
	}
}

// expireRetained enforces share retention (SHARE-05 保留期限).
func (w *Worker) expireRetained(ctx context.Context) {
	var n int
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, eerr := tx.ExpireRetained(ctx)
		n = got
		return eerr
	})
	if err != nil {
		w.log.Error("share retention sweep failed", "error", err)
		return
	}
	if n > 0 {
		// Logged rather than silent: data moving to the trash because a policy expired is
		// something an operator may need to explain to a user, and "it vanished" is not an
		// answer.
		w.log.Info("share retention expired deliveries", "moved_to_trash", n)
	}
}

// pruneRateLimitRows bounds the two append-only tables that rate limiting depends on.
//
// Both were defined and neither was ever called, which makes them a slow disk leak: a row per
// OTP attempt and a row per accepted upload, forever. Pruning is what turns "we record events to
// enforce a window" into something that does not grow without limit.
//
// The retention is deliberately generous — an hour past the longest window any share can set,
// which is far longer than any window in use — because deleting a row that is still inside a
// window would weaken the limit it exists to enforce. Being slightly late costs disk; being early
// costs correctness.
func (w *Worker) PruneRateLimitRows(ctx context.Context) { w.pruneRateLimitRows(ctx) }

func (w *Worker) pruneRateLimitRows(ctx context.Context) {
	const keep = 24 * time.Hour

	var otp, uploads, passwords int
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var e1, e2, e3 error
		otp, e1 = tx.PruneOTPAttempts(ctx, keep)
		uploads, e2 = tx.PruneShareUploadEvents(ctx, keep)
		passwords, e3 = tx.PrunePasswordAttempts(ctx, keep)
		return errors.Join(e1, e2, e3)
	})
	if err != nil {
		w.log.Error("prune rate-limit rows failed", "error", err)
		return
	}
	if otp > 0 || uploads > 0 || passwords > 0 {
		w.log.Debug("pruned rate-limit rows",
			"otp_attempts", otp, "upload_events", uploads, "password_attempts", passwords)
	}
}
