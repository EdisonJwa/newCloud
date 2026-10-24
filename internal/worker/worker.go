// Package worker runs the periodic maintenance tasks.
//
// Everything here is idempotent and safe to run from several processes: the
// cleanup queries are written so that a duplicate run is wasteful but never
// wrong. That property is what lets the deployment start with one process and add
// replicas later without changing this code.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/search"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
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
	// scrubCursor is where the previous pass stopped. It is what makes the scrub cover the whole
	// store over time instead of re-verifying the same first rows forever.
	scrubCursor [32]byte
	// anchorSigner signs published checkpoints when the deployment configured a key.
	anchorSigner *auditverify.Signer
	// versionByteCap bounds retained history in bytes (§4.3). Zero disables the trim.
	versionByteCap int64
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
	// uploads reclaims partial uploads left by interrupted writes. Nil means the backend cannot do
	// it, which is a complete answer rather than a failure — a third backend is not required to
	// implement blob.UploadCleaner.
	uploads     blob.UploadCleaner
	uploadGrace time.Duration
	// byteDeleter removes the stored bytes once their metadata row is gone. Nil means the
	// deployment has no content store to delete from (or wiring was skipped): the sweep then
	// collects rows only, and serve says so at startup.
	byteDeleter ByteDeleter

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

	// passCap overrides passTimeout when non-zero. Set only by tests, which cannot wait
	// thirty minutes to watch a hung pass be cut off.
	passCap time.Duration
	// recoveryGrace overrides the recovery pass's grace period. Set only by tests, which cannot
	// wait a minute for every case.
	recoveryGrace time.Duration

	// metrics is optional; nil means nothing is recorded.
	metrics *metrics.Registry

	// The optional content-search engine (core-scope §7.2). Nil is the `search.engine: none`
	// deployment: every index duty becomes a no-op and no write waits on an index.
	search search.Engine
	// reader supplies document BYTES for text extraction. Nil means metadata-only indexing, which
	// is the honest limit of a worker that has no content store wired.
	reader DocumentReader
	// contentTypes is the set of types whose text is extracted (search.content_types).
	contentTypes []string
	// indexBytes is the largest body the indexer reads (search.index_bytes). Zero disables
	// extraction entirely, so a deployment can have metadata-only content search on purpose.
	indexBytes int64
	// searchCursor is where the last backfill pass stopped. In-process on purpose: it decides where
	// THIS worker resumes, and losing it costs a redundant idempotent pass rather than correctness.
	searchCursor uuid.UUID

	// runMu guards done, which is closed exactly once, when Run finishes. Run must be
	// started once per process: a second call must not start a second set of duty
	// goroutines on the same store. done is what serve joins at shutdown.
	runMu sync.Mutex
	done  chan struct{}
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

// ByteDeleter is the physical half of blob collection: removing the stored bytes once their
// metadata row is gone.
//
// The method matches blob.Store's Delete signature on purpose — both shipped backends are
// content-addressed, so both already implement it and wiring needs no adapter. It is a
// one-method interface rather than blob.Store because the collector must never PUT, OPEN or
// STAT anything: a garbage collector with write access to the live content it exists to
// reclaim is one refcount bug away from deleting referenced data.
type ByteDeleter interface {
	Delete(ctx context.Context, h blob.Hash) error
}

// WithByteDeletion wires physical byte deletion into the blob sweep.
//
// Without it the sweep deletes only metadata rows, and the bytes stay on the storage device
// forever while the metrics report them collected — the collector reclaimed rows and called it
// disk. Nil is legitimate (the tree layout has no content store to address bytes with) and
// serve logs that at startup, so the gap is a visible decision rather than a surprise found
// by a full disk.
func (w *Worker) WithByteDeletion(d ByteDeleter) *Worker {
	w.byteDeleter = d
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

// DocumentReader supplies a node's bytes for text extraction (core-scope §7.2).
//
// One method rather than the whole ContentStore: the indexer must never write, move or delete
// anything. A worker that could PUT would be one bug away from rewriting a user's file with the
// text it was supposed to be indexing.
type DocumentReader interface {
	// ReadContent returns at most maxBytes of a node's text, already converted to searchable
	// plain text. It must not return the raw bytes of a PDF as if they were words — that is the
	// caller's job to avoid by only asking for types it can handle.
	ReadContent(ctx context.Context, node store.Node, maxBytes int64) (string, error)
}

// WithContentSearch wires the content indexer.
//
// Everything is optional and nothing fails a write: the producer enqueues, and this runs later.
// A deployment with a nil engine keeps name-and-filter search and answers `degraded: true` for
// content queries, which is the documented posture (§7.2, ADR-0016).
func (w *Worker) WithContentSearch(engine search.Engine, reader DocumentReader,
	contentTypes []string, indexBytes int64) *Worker {

	w.search = engine
	w.reader = reader
	w.contentTypes = contentTypes
	w.indexBytes = indexBytes
	return w
}

// New builds a worker with production defaults.
func New(meta store.Store, log *slog.Logger) *Worker {
	w := &Worker{
		meta:          meta,
		recoveryGrace: recoveryGrace,
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
// WithUploadCleanup enables the partial-upload sweep (§9.3 清理未完成 Multipart Upload).
//
// grace must exceed the longest upload the deployment accepts, or the sweep would abort transfers
// that are still running. It is a required argument rather than a default for that reason: the
// wrong value destroys live uploads, and there is no value that is obviously safe for every
// deployment.
func (w *Worker) WithUploadCleanup(cleaner blob.UploadCleaner, interval, grace time.Duration) *Worker {
	w.uploads = cleaner
	w.uploadGrace = grace
	if interval > 0 {
		w.purgeInterval = interval
	}
	return w
}

func (w *Worker) WithRetention(versions, trash time.Duration) *Worker {
	w.versionRetention = versions
	w.trashRetention = trash
	return w
}

// duty is one periodic task: its own timer, its own goroutine, its own failure boundary.
//
// The previous shape was a single select loop with one case per timer, which had three
// problems that only show up in production (2026-10-24 review, M12):
//
//   - The duties were serialised despite the comment claiming they were not. A blob sweep
//     over a large store blocks the loop for minutes, and lock expiry — the one duty whose
//     lateness a user feels immediately, as "the file is locked by nobody" — waits behind it.
//   - A panic anywhere took the process down. One corrupt row in a purge ended the whole
//     server, including the HTTP listener, because the worker runs in the same process.
//   - Nothing bounded a pass, so a query blocked on a lock stalled every other duty with no
//     signal beyond "the worker log stopped".
type duty struct {
	name     string
	interval time.Duration
	// run does one pass and must respect ctx cancellation.
	run func(ctx context.Context) error
}

// passTimeout is the ceiling on a single pass, independent of its interval.
//
// A duty that hangs holds whatever it touched until the process is restarted, and no part of
// this package can tell "slow" from "stuck". Cutting the pass off bounds the damage; every
// duty is idempotent, so a pass that was cut short simply runs again on the next tick.
const passTimeout = 30 * time.Minute

// passTimeoutOr is the bound for one pass, with a test override.
func (w *Worker) passTimeoutOr() time.Duration {
	if w.passCap > 0 {
		return w.passCap
	}
	return passTimeout
}

// duties returns every periodic task, in the order they are logged at startup.
func (w *Worker) duties() []duty {
	scrubEvery := w.scrubInterval
	if scrubEvery <= 0 {
		scrubEvery = w.sweepInterval
	}
	return []duty{
		// Lock expiry runs on the shortest interval because it is the only duty whose lateness
		// a user feels directly: a lock nobody holds blocks every write to that file (RFC 4918
		// §6.1), and the client that took it is already gone.
		{name: "locks", interval: w.lockInterval, run: func(ctx context.Context) error {
			n, err := w.ExpireLocks(ctx)
			if err != nil {
				return fmt.Errorf("expire locks: %w", err)
			}
			if n > 0 {
				w.log.Info("expired locks", "count", n)
			}
			n, err = w.ExpireDownloadSessions(ctx)
			if err != nil {
				return fmt.Errorf("expire download sessions: %w", err)
			}
			if n > 0 {
				w.log.Info("expired download sessions", "count", n)
			}
			return nil
		}},
		// The collector is the only thing that reclaims disk, so both its successes and its
		// failures are published: a run that deletes nothing is normal, a run that errors means
		// space is held that should have been freed. Recording only successes would make the
		// error counter permanently zero and the alert on it dead.
		{name: "blob-sweep", interval: w.sweepInterval, run: func(ctx context.Context) error {
			n, err := w.SweepBlobs(ctx)
			if w.metrics != nil {
				w.metrics.ObserveBlobGC(n, err != nil)
			}
			if err != nil {
				return fmt.Errorf("blob sweep: %w", err)
			}
			if n > 0 {
				w.log.Info("collected orphaned blobs", "count", n)
			}
			return nil
		}},
		// A broken archiver does not surface anywhere else: the database keeps serving and the
		// only visible symptom is pg_wal growing until the disk fills.
		{name: "wal-archive", interval: w.sweepInterval, run: func(ctx context.Context) error {
			w.sampleWALArchive(ctx)
			return nil
		}},
		// The anchor tick is hourly, matching the head-age alert's own threshold: sampling more
		// often would not page anyone sooner, and sampling less would leave the alert blind for
		// most of the hour it claims to watch.
		{name: "audit-anchor", interval: w.anchorInterval, run: func(ctx context.Context) error {
			err := w.anchorOnce(ctx)
			// The audit gauges are not part of anchoring: they must exist on a deployment with
			// no anchor directory configured too, or the chain-stalled alert is dead by
			// configuration rather than by threshold.
			w.AuditHealth(ctx)
			if err != nil {
				return fmt.Errorf("audit anchoring: %w", err)
			}
			return nil
		}},
		// Integrity: read a sample of live blobs and compare to their addresses. Distinct from
		// the sweep, which reclaims UNREFERENCED content — this looks for content that is
		// referenced and WRONG, which the sweep cannot see by construction.
		{name: "audit-scrub", interval: scrubEvery, run: func(ctx context.Context) error {
			w.runScrub(ctx)
			return nil
		}},
		{name: "jobs", interval: w.jobInterval, run: func(ctx context.Context) error {
			// Before claiming, release leases whose worker died mid-job. Claiming would recover
			// them anyway — an expired lease is claimable — so this is not what makes recovery
			// work. It is the only place that can REPORT it, which is the difference between
			// "the queue is busy" and "the queue is being lost".
			w.reapExpiredJobLeases(ctx)
			// A failure here is logged and the pass ends: the queue is durable, so a transient
			// database error means the work waits rather than being lost.
			n, err := w.ProcessJobs(ctx)
			if err != nil {
				return fmt.Errorf("job processing: %w", err)
			}
			if n > 0 {
				w.log.Debug("processed jobs", "count", n)
			}
			return nil
		}},
		// Operation recovery reads the journal that the delete path writes. An open row means a
		// process stopped between recording intent and finishing, so the pass either resolves it from
		// the node's own state or escalates it — it never guesses, and it never deletes content.
		{name: "op-recovery", interval: w.purgeInterval, run: func(ctx context.Context) error {
			resolved, escalated, err := w.RecoverOperations(ctx)
			if err != nil {
				return fmt.Errorf("recover operations: %w", err)
			}
			if escalated > 0 {
				w.log.Warn("operations need an operator", "count", escalated, "resolved", resolved)
			} else if resolved > 0 {
				w.log.Info("recovered unfinished operations", "count", resolved)
			}
			return nil
		}},
		{name: "rate-limit-prune", interval: w.purgeInterval, run: func(ctx context.Context) error {
			w.pruneRateLimitRows(ctx)
			return nil
		}},
		// Session rows are one per login, and nothing read a dead one again: expiry and
		// revocation make a row inert, not absent, so without this pass the table grows
		// with every login ever made. Daily is enough — the retention horizon is thirty
		// days, so an hourly tick would delete the same nothing 23 times out of 24.
		{name: "session-prune", interval: sessionPruneInterval, run: func(ctx context.Context) error {
			n, err := w.PruneSessions(ctx)
			if err != nil {
				return fmt.Errorf("session prune: %w", err)
			}
			if n > 0 {
				w.log.Debug("pruned dead session rows", "count", n)
			}
			return nil
		}},
		// Share retention (SHARE-05 保留期限). Separate from PurgeTrash: this moves expired
		// deliveries to the trash, and the trash window still applies before anything is
		// destroyed.
		{name: "share-retention", interval: w.purgeInterval, run: func(ctx context.Context) error {
			w.expireRetained(ctx)
			return nil
		}},
		// Partial uploads are invisible to every listing and cost storage until removed, but
		// they are not urgent, so there is no reason for a faster tick.
		{name: "upload-cleanup", interval: w.purgeInterval, run: func(ctx context.Context) error {
			w.abortStaleUploads(ctx)
			return nil
		}},
		{name: "purge", interval: w.purgeInterval, run: func(ctx context.Context) error {
			var firstErr error
			keep := func(err error) {
				if err != nil && firstErr == nil {
					firstErr = err
				}
			}
			if res, err := w.PurgeTrash(ctx); err != nil {
				keep(fmt.Errorf("trash purge: %w", err))
			} else if res.Roots > 0 {
				w.log.Info("purged expired trash",
					"roots", res.Roots, "nodes", res.Nodes, "bytes", res.Bytes)
			}
			if n, err := w.ExpireVersions(ctx); err != nil {
				keep(fmt.Errorf("version expiry: %w", err))
			} else if n > 0 {
				w.log.Info("expired old versions", "count", n)
			}
			// The byte cap runs after the age expiry: expiring by age first means the trim
			// usually has nothing to do, and the two policies never fight over the same row.
			if _, err := w.TrimVersionHistory(ctx); err != nil {
				keep(fmt.Errorf("version history trim: %w", err))
			}
			return firstErr
		}},
		// Content-search maintenance (core-scope §7.2). Two things in one pass, both no-ops when no
		// engine is wired:
		//
		//   backfill  walks nodes by cursor so a fresh engine converges without a migration flag;
		//   lag       publishes how far behind the queue is, which is the alert signal.
		//
		// The lag is published even when the backfill indexed nothing: the interesting number is
		// often "the queue is not draining", and a gauge that only reported on activity would go
		// quiet exactly when the backlog builds. The same reasoning as the job-depth gauges.
		{name: "search-index", interval: w.jobInterval, run: func(ctx context.Context) error {
			if w.search == nil {
				return nil
			}
			if n, err := w.BackfillSearchIndex(ctx); err != nil {
				return fmt.Errorf("search backfill: %w", err)
			} else if n > 0 {
				w.log.Debug("indexed documents", "count", n)
			}
			if lag, err := w.SearchIndexLag(ctx); err != nil {
				return fmt.Errorf("search index lag: %w", err)
			} else if w.metrics != nil {
				w.metrics.SetSearchIndexLag(lag)
			}
			return nil
		}},
	}
}

// Run blocks until the context is cancelled, then waits for in-flight passes to finish.
//
// Each duty runs in its own goroutine, so a slow blob sweep cannot delay lock expiry. The
// wait at the end is what makes shutdown joined: returning while a purge was still writing
// would kill it half-way through a transaction that the next start has to reason about.
//
// A second call in the same process does not start a second set of duties — it blocks until
// the first Run finishes. Completion is observable with Wait, which is what the serving
// process joins at shutdown; before Wait existed, nothing joined the worker at all.
func (w *Worker) Run(ctx context.Context) {
	w.runMu.Lock()
	if w.done != nil {
		done := w.done
		w.runMu.Unlock()
		<-done
		return
	}
	w.done = make(chan struct{})
	w.runMu.Unlock()

	defer close(w.done)

	w.log.Info("worker started",
		"lock_interval", w.lockInterval, "sweep_interval", w.sweepInterval,
		"purge_interval", w.purgeInterval,
		"version_retention", w.versionRetention, "trash_retention", w.trashRetention)

	if w.trashRetention <= 0 && w.versionRetention <= 0 {
		// Say so loudly. "Retention is configured" and "retention does nothing" look identical
		// from outside, and an operator who believes the second is the first discovers it when
		// the disk fills.
		w.log.Warn("no data retention is configured: trash and versions are kept forever",
			"hint", "set limits.trash_retention_days and limits.version_retention_days")
	}

	// One pass at startup: a process that restarts more often than the interval would otherwise
	// never anchor at all.
	if err := w.anchorOnce(ctx); err != nil {
		w.log.Error("audit anchoring failed", "error", err)
	}
	w.AuditHealth(ctx)

	w.runDuties(ctx, w.duties())
}

// Wait blocks until a previously started Run has finished, and reports whether it did.
//
// False means the timeout elapsed while the worker was still shutting down — a pass stuck on
// a slow query, most likely. The caller decides what abandoning means; the serving process
// logs it, because a shutdown that gave up on its maintenance loop is a fact about that
// shutdown, not a success.
//
// A Run that was never started needs no join, so Wait returns true immediately — there is
// nothing in flight to wait for.
func (w *Worker) Wait(timeout time.Duration) bool {
	w.runMu.Lock()
	done := w.done
	w.runMu.Unlock()
	if done == nil {
		return true
	}
	// An already-finished Run must satisfy even a zero timeout: with both channels
	// ready, select picks randomly, so the timed wait below would report the finished
	// worker as still running about half the time.
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// runDuties starts one goroutine per duty and waits for all of them at shutdown.
//
// Split out from Run so a test can drive the real machinery — goroutines, the join, the
// panic boundary — with a task it controls, instead of needing a store to make the real
// duties do anything.
func (w *Worker) runDuties(ctx context.Context, duties []duty) {
	var wg sync.WaitGroup
	for _, d := range duties {
		if d.interval <= 0 {
			// A zero interval means "not configured", not "as fast as possible".
			continue
		}
		wg.Add(1)
		go func(d duty) {
			defer wg.Done()
			w.loop(ctx, d)
		}(d)
	}

	<-ctx.Done()
	w.log.Info("worker stopping; waiting for in-flight passes")
	wg.Wait()
	w.log.Info("worker stopped")
}

// loop runs one duty until the context is cancelled.
func (w *Worker) loop(ctx context.Context, d duty) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.pass(ctx, d)
		}
	}
}

// pass runs one iteration with a timeout and a panic boundary.
//
// The recover is the reason a single corrupt row can no longer take the process down: the
// worker shares the process with the HTTP listener, so an unrecovered panic in a purge is an
// outage for every user, not just a lost maintenance cycle.
func (w *Worker) pass(ctx context.Context, d duty) {
	if ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("worker pass panicked; the process stays up and the pass is skipped",
				"duty", d.name, "panic", r, "stack", string(debug.Stack()))
		}
	}()

	limit := w.passTimeoutOr()
	passCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	if err := d.run(passCtx); err != nil {
		// A cancelled pass is shutdown, not failure: the duty was interrupted on purpose and
		// its next tick will see the same state.
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			w.log.Error("worker pass exceeded its timeout; it will be retried",
				"duty", d.name, "timeout", limit)
			return
		}
		w.log.Error("worker pass failed", "duty", d.name, "error", err)
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
		if perr != nil {
			return perr
		}
		// Audited in the SAME transaction as the deletion (§8.1).
		//
		// After the rows are gone there is no remaining evidence of what removed them, so a
		// separate audit write could claim a purge that was rolled back, or lose the record of one
		// that committed. Only a row in this transaction is true exactly when the delete is.
		//
		// Nothing is written when nothing was deleted: a sweep every few minutes would otherwise
		// fill the audit log with "deleted 0 items", which is the fastest way to make an audit log
		// unreadable.
		if got.Nodes == 0 {
			return nil
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: string(audit.ActionTrashPurge),
			TargetKind: "trash", TargetID: "retention",
			Result: string(audit.ResultOK),
			Metadata: map[string]any{
				"nodes": got.Nodes, "roots": got.Roots, "bytes": got.Bytes,
				"retention_seconds": int64(w.trashRetention.Seconds()),
			},
		})
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
		if eerr != nil {
			return eerr
		}
		// Same-transaction audit, same reasoning as the trash purge above. A version that
		// disappears has no row left to explain itself, and "I could restore this last week" is
		// answered by this event or by nothing.
		if got == 0 {
			return nil
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: string(audit.ActionVersionExpire),
			TargetKind: "version", TargetID: "retention",
			Result: string(audit.ResultOK),
			Metadata: map[string]any{
				"versions":          got,
				"retention_seconds": int64(w.versionRetention.Seconds()),
			},
		})
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
//
// Two halves, both required for the sweep to actually reclaim disk: the metadata row and
// the physical bytes. When a byte deleter is wired, each successful row delete is followed
// by the physical delete. The honest limitation: that second step is best-effort, because
// the row is already committed by the time it runs. A physical delete that fails is logged
// with the blob id and the sweep CONTINUES — failing here would report a collection that
// happened as failed, and the caller would re-collect nothing. The retry is an orphan sweep
// (storage-side listing against the metadata), not this pass, which by construction can no
// longer see the rows it removed.
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
		var hash []byte
		// Delete the row first: if the process dies midway, the bytes are orphaned
		// rather than the row pointing at nothing. An orphan costs disk; a dangling
		// row costs a broken file for a user.
		//
		// The content address is read in the SAME transaction, immediately before the
		// delete: once the row is gone, nothing in the metadata can name the bytes, so
		// this is the last moment the physical half can learn where to aim.
		err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			h, herr := tx.BlobHash(ctx, ref.ID)
			if herr != nil {
				return herr
			}
			hash = h
			return tx.DeleteBlob(ctx, ref.ID)
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return removed, fmt.Errorf("worker: delete blob %s: %w", ref.ID, err)
		}
		removed++

		// Now the bytes. Not wired means the deployment has no content store to delete
		// from; serve logs that once at startup and the sweep here does the row half only.
		if w.byteDeleter == nil || len(hash) != len(blob.Hash{}) {
			continue
		}
		var h blob.Hash
		copy(h[:], hash)
		if derr := w.byteDeleter.Delete(ctx, h); derr != nil {
			w.log.Error("a collected blob's bytes could not be deleted; the row is already "+
				"gone, so the object must be reclaimed by an orphan sweep",
				"blob", ref.ID.String(), "sha256", h.String(), "error", derr)
		}
	}

	// One audit row per RUN, not per blob.
	//
	// Per-blob would be correct in the transactional sense and wrong in every other: a first sweep
	// of a large deployment collects thousands of blobs, and an audit log with thousands of
	// "collected 1 blob" rows is one nobody reads. What an operator needs is the summary — when the
	// collector ran, how much it took, and under what grace period.
	//
	// The honest limitation: this row is written AFTER the per-blob transactions, so a process that
	// dies mid-loop leaves blobs deleted with no audit row. The deletes themselves are individually
	// safe (DeleteBlob refuses anything still referenced), so the loss is a gap in the record, not a
	// gap in the data. Recording that tradeoff here because the alternative — one row per blob — was
	// rejected deliberately rather than overlooked.
	if removed > 0 {
		var bytes int64
		for _, ref := range candidates {
			bytes += ref.Size
		}
		if err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "system", Action: string(audit.ActionBlobCollect),
				TargetKind: "blob", TargetID: "gc",
				Result: string(audit.ResultOK),
				Metadata: map[string]any{
					"blobs":           removed,
					"candidate_bytes": bytes,
					"grace_seconds":   int64(w.blobGrace.Seconds()),
				},
			})
		}); err != nil {
			// The bytes are already gone; failing the sweep now would only make the caller retry a
			// collection it has already performed.
			w.log.Error("could not audit blob collection", "error", err, "removed", removed)
		}
	}
	return removed, nil
}

// abortStaleUploads discards partial uploads that will never complete.
//
// Nothing lists these: an interrupted multipart upload is not an object, and a crash-left temp file
// is not an object either, so the usage they consume is invisible to every count the server keeps.
// That is why this is a sweep on a timer rather than something triggered by a failed request — the
// failed request is gone by the time anyone could notice.
func (w *Worker) abortStaleUploads(ctx context.Context) {
	if w.uploads == nil || w.uploadGrace <= 0 {
		return
	}
	res, err := w.uploads.AbortStaleUploads(ctx, w.uploadGrace)
	if err != nil {
		w.log.Error("partial-upload cleanup failed", "error", err)
		return
	}
	if res.Aborted > 0 {
		w.log.Info("discarded stale partial uploads",
			"aborted", res.Aborted, "bytes", res.Bytes)
	}
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

// sessionPruneInterval is how often dead session rows are collected. Daily, because the
// retention horizon is measured in weeks and the pass deletes nothing most days.
const sessionPruneInterval = 24 * time.Hour

// PruneSessions removes session rows that died (expired or revoked) further back than the
// store's retention horizon.
//
// A no-op on a small deployment and a bounded delete on a busy one; the retention constant
// lives with the store implementations, which is where the death comparison is defined.
func (w *Worker) PruneSessions(ctx context.Context) (int64, error) {
	var n int64
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, perr := tx.PruneSessions(ctx, store.SessionRetention)
		n = got
		return perr
	})
	return n, err
}

// reapExpiredJobLeases releases leases held past their expiry and reports them.
//
// A worker that dies mid-job leaves its lease behind; the job becomes claimable again when the
// lease expires, which is the recovery path and needs no help. What was missing is the signal: a
// deployment whose processes are being killed or crashing mid-handler looked identical to one that
// is simply idle, because nothing counted the leases being abandoned.
func (w *Worker) reapExpiredJobLeases(ctx context.Context) int {
	var n int
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		count, rerr := tx.ReapExpiredLeases(ctx)
		n = count
		return rerr
	})
	if err != nil {
		w.log.Error("reaping expired job leases failed", "error", err)
		return 0
	}
	if n > 0 {
		w.log.Warn("released job leases from workers that did not finish",
			"count", n,
			"hint", "a worker is dying mid-job, or the lease is shorter than the slowest handler")
	}
	return n
}

// ReapExpiredJobLeasesForTest exposes the reaper to the package's tests.
func (w *Worker) ReapExpiredJobLeasesForTest(ctx context.Context) int {
	return w.reapExpiredJobLeases(ctx)
}

// WithAnchorSigner signs every published checkpoint.
//
// Set from configuration; nil publishes unsigned checkpoints, and the startup log says which.
func (w *Worker) WithAnchorSigner(s *auditverify.Signer) *Worker {
	w.anchorSigner = s
	return w
}

// WithVersionByteCap sets the retained-history byte cap.
func (w *Worker) WithVersionByteCap(bytes int64) *Worker {
	w.versionByteCap = bytes
	return w
}

// TrimVersionHistory drops the oldest versions until each account's history fits the cap (§4.3).
//
// A sweep rather than a check on the write path: refusing a save because the account's HISTORY is full
// would lose the content the user is trying to write, and trimming is the policy every product with
// versioning converges on. The cost is that the cap is enforced with a delay, which is acceptable for
// a retention limit and not for the quota — the quota is checked in the write transaction.
func (w *Worker) TrimVersionHistory(ctx context.Context) (int, error) {
	if w.meta == nil {
		return 0, nil
	}
	// The effective cap: a runtime override wins over the configured value, so an administrator can
	// change how much history is retained without a restart. Read once per sweep rather than cached:
	// the sweep runs on an interval, and a settings read per interval is cheaper than the machinery a
	// cache would need to stay correct across processes.
	cap := w.versionByteCap
	if err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		values, verr := tx.ReadSettings(ctx)
		if verr != nil {
			return verr
		}
		raw, ok := values["limits.max_retained_version_bytes"]
		if !ok {
			return nil
		}
		var v int64
		if jerr := json.Unmarshal(raw, &v); jerr == nil {
			cap = v
		}
		return nil
	}); err != nil {
		// A settings read failure falls back to the configured value: retention trimming must not stop
		// because an override could not be read.
		w.log.Warn("could not read the retained-history cap; using the configured value",
			"error", err)
	}
	if cap <= 0 {
		return 0, nil
	}
	var n int
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, terr := tx.TrimRetainedVersions(ctx, cap)
		n = got
		if terr != nil {
			return terr
		}
		if got == 0 {
			return nil
		}
		// Same-transaction audit, same reasoning as the sweeps above: a trimmed version has no row
		// left to explain itself, and "the version I needed is gone" is answered by this event or by
		// nothing.
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: "version.trim", TargetKind: "version",
			TargetID: "retained", Result: "ok",
			Metadata: map[string]any{"removed": got, "cap_bytes": cap},
		})
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		w.log.Warn("trimmed retained version history to the cap", "removed", n, "cap_bytes", cap)
	}
	return n, nil
}
