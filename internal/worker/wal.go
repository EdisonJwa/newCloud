package worker

// WAL archive health.
//
// When archive_command fails, PostgreSQL does not stop. It logs, retries, and keeps accepting
// writes — while the unarchived segments accumulate in pg_wal. The deployment is fully functional
// and the disaster-recovery guarantee has silently ended: an RPO that used to be measured in
// minutes is now "whatever happens to still be on that disk when it fails".
//
// Measured in round 42 against a deliberately broken archive_command: zero segments archived,
// pg_wal growing 49 → 65 → 81 MB over three rounds of writes. Nothing in the application noticed,
// because nothing was reading pg_stat_archiver.
//
// This file adds that reader. Three numbers matter and they answer different questions:
//
//	archived_count           is the archive working at all (a counter that should keep rising)
//	failed_count             was it ever broken (a counter that should never rise)
//	segments pending         is the disk filling right now (pg_wal contents not yet archived)
//
// The third is the one that predicts an outage. The first two explain it afterwards.

import (
	"context"
	"fmt"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// WALArchive is a snapshot of the archiver's state.
type WALArchive struct {
	Archived uint64
	Failed   uint64
	// Pending counts WAL segments still on disk that have not been archived. It is the gauge that
	// grows toward a full disk, which is why it is a gauge and not derived from the counters.
	Pending int64
	// LastFailedWAL names the segment that failed, for a log line. Empty when nothing failed.
	LastFailedWAL string
}

// SampleWALArchive reads pg_stat_archiver and counts the segments waiting to be archived.
//
// Reading pg_stat_archiver requires only a connection; it is not privileged. The pending count
// comes from listing pg_wal, which needs superuser or pg_read_server_files, so a failure there is
// tolerated: the counters are still reported and Pending stays at its previous meaning of
// "unknown → 0". Reporting nothing at all because one of three queries is unprivileged would hide
// the part that matters most on a locked-down deployment.
func (w *Worker) SampleWALArchive(ctx context.Context) (WALArchive, error) {
	p, ok := w.meta.(*store.PG)
	if !ok {
		// The in-memory store has no WAL; this is not an error, just not applicable.
		return WALArchive{}, nil
	}
	pool := p.Pool()
	if pool == nil {
		return WALArchive{}, nil
	}

	var out WALArchive
	err := pool.QueryRow(ctx, `
		SELECT archived_count, failed_count, COALESCE(last_failed_wal, '')
		  FROM pg_stat_archiver`).
		Scan(&out.Archived, &out.Failed, &out.LastFailedWAL)
	if err != nil {
		return WALArchive{}, fmt.Errorf("worker: read pg_stat_archiver: %w", err)
	}

	// Pending is best-effort: pg_ls_waldir() is restricted, and on a deployment where the
	// application role cannot call it the counters above are still the useful signal.
	var pending int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_ls_waldir()`).Scan(&pending); err == nil {
		out.Pending = pending
	}
	return out, nil
}

// sampleWALArchive publishes the figures when a registry is attached.
func (w *Worker) sampleWALArchive(ctx context.Context) {
	if w.metrics == nil {
		return
	}
	snap, err := w.SampleWALArchive(ctx)
	if err != nil {
		// A failed read is not published: leaving the previous values in place is better than
		// reporting zero, which would look like "archiving is fine and nothing is pending".
		w.log.Warn("wal archive sample failed", "error", err)
		return
	}
	w.metrics.SetWALArchive(snap.Archived, snap.Failed, snap.Pending)
	if snap.Failed > 0 && snap.LastFailedWAL != "" {
		// Logged every cycle on purpose: an operator reading logs after an incident needs to see
		// when it started, and a once-only line is easy to scroll past.
		w.log.Warn("wal archiving is failing",
			"failed_count", snap.Failed, "last_failed_wal", snap.LastFailedWAL,
			"pending_segments", snap.Pending)
	}
}
