package store

// Pruning the audit log without breaking its chain.
//
// The chain proves that no event was altered or removed, so a retention policy cannot simply delete
// the oldest rows: the first survivor's prev_hash points at an event that no longer exists, and every
// later verification reports the log as broken — indistinguishable from tampering.
//
// A prune mark is the boundary. It records the last removed event's id and hash, and verification
// starts from there instead of from genesis. A gap that nobody recorded still fails: the mark names
// the event sequence it covers, so a second deletion cannot hide behind the first.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// PruneMark is one recorded archive boundary.
type PruneMark struct {
	ID        int64
	ThroughID int64
	HeadHash  []byte
	Events    int64
	FirstTS   time.Time
	LastTS    time.Time
	// ArchivePath is where the removed events were written, so an auditor can find them.
	ArchivePath string
	PrunedAt    time.Time
}

// PruneResult reports what one prune did.
type PruneResult struct {
	Mark PruneMark
	// Vacuumed is true when the caller asked for the space to be reclaimed.
	Vacuumed bool
}

// ErrNoAuditToPrune means nothing was old enough to remove.
var ErrNoAuditToPrune = errors.New("store: no audit events older than the cutoff")

// LatestPruneMark returns the newest recorded boundary, or a zero mark when the log has never been
// pruned.
func (s *PG) LatestPruneMark(ctx context.Context) (PruneMark, error) {
	const q = `
		SELECT id, through_id, head_hash, events, first_ts, last_ts, archive_path, pruned_at
		FROM audit.prune_marks ORDER BY through_id DESC LIMIT 1`
	var m PruneMark
	err := s.pool.QueryRow(ctx, q).Scan(&m.ID, &m.ThroughID, &m.HeadHash, &m.Events,
		&m.FirstTS, &m.LastTS, &m.ArchivePath, &m.PrunedAt)
	if err != nil {
		if isNoRows(err) {
			return PruneMark{}, nil
		}
		return PruneMark{}, translate(err)
	}
	return m, nil
}

// PruneAuditBefore removes every event older than cutoff and records the boundary.
//
// The rows are returned BEFORE they are deleted so the caller can archive them: this method does not
// write files, because a store that decides where an archive lives would be making an operational
// decision on the operator's behalf. The caller passes the path it wrote, which is stored in the mark.
//
// Everything happens in one transaction: the boundary and the deletion commit together, or neither
// does. A mark without a deletion would make verification skip events that are still present; a
// deletion without a mark would make the log look tampered with.
func (s *PG) PruneAuditBefore(ctx context.Context, cutoff time.Time, archivePath string) (PruneResult, error) {
	var res PruneResult
	err := s.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		ptx, ok := tx.(*pgTx)
		if !ok {
			return fmt.Errorf("store: prune requires the PostgreSQL backend")
		}

		// Find the last event that will be removed, and the boundary hash it carries.
		var (
			throughID int64
			headHash  []byte
			events    int64
			firstTS   time.Time
			lastTS    time.Time
		)
		err := ptx.tx.QueryRow(ctx, `
			SELECT count(*), coalesce(max(id), 0), min(ts), max(ts)
			FROM audit.events WHERE ts < $1`, cutoff).
			Scan(&events, &throughID, &firstTS, &lastTS)
		if err != nil {
			return translate(err)
		}
		if events == 0 {
			return ErrNoAuditToPrune
		}
		if err := ptx.tx.QueryRow(ctx,
			`SELECT hash FROM audit.events WHERE id = $1`, throughID).Scan(&headHash); err != nil {
			return translate(err)
		}

		// The chain may already be broken; pruning anyway would record a boundary over a log that
		// was tampered with before the prune, and the mark would then vouch for it. Refusing is the
		// only safe answer, and it is answered by checking the link this event makes with its
		// predecessor.
		var (
			prevHash []byte
			prevID   int64
		)
		if err := ptx.tx.QueryRow(ctx, `
			SELECT prev_hash FROM audit.events WHERE id = $1`, throughID).Scan(&prevHash); err != nil {
			return translate(err)
		}
		if err := ptx.tx.QueryRow(ctx, `
			SELECT coalesce(max(id), 0) FROM audit.events WHERE id < $1`, throughID).Scan(&prevID); err != nil {
			return translate(err)
		}
		if prevID != 0 {
			var prevEventHash []byte
			if err := ptx.tx.QueryRow(ctx, `SELECT hash FROM audit.events WHERE id = $1`, prevID).
				Scan(&prevEventHash); err != nil {
				return translate(err)
			}
			if string(prevEventHash) != string(prevHash) {
				return fmt.Errorf("store: refusing to prune: the chain is already broken at event %d "+
					"(its prev_hash does not match event %d). Verify the log before archiving part "+
					"of it, or the mark would vouch for a log that was tampered with", throughID, prevID)
			}
		}

		m := PruneMark{
			ThroughID: throughID, HeadHash: headHash, Events: events,
			FirstTS: firstTS, LastTS: lastTS, ArchivePath: archivePath,
		}

		if _, err := ptx.tx.Exec(ctx, `
			INSERT INTO audit.prune_marks (through_id, head_hash, events, first_ts, last_ts, archive_path)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (through_id) DO NOTHING`,
			m.ThroughID, m.HeadHash, m.Events, m.FirstTS, m.LastTS, m.ArchivePath); err != nil {
			return translate(err)
		}

		// The delete is bounded by id, not by ts: the mark names an event sequence, and deleting a
		// slightly different set than the mark describes would leave the two disagreeing.
		tag, err := ptx.tx.Exec(ctx, `DELETE FROM audit.events WHERE id <= $1`, throughID)
		if err != nil {
			return translate(err)
		}
		if tag.RowsAffected() != events {
			return fmt.Errorf("store: prune would remove %d events but the mark records %d; "+
				"concurrent writes during the prune are not supported", tag.RowsAffected(), events)
		}
		res.Mark = m
		return nil
	})
	if err != nil {
		return PruneResult{}, err
	}
	return res, nil
}

// AuditBefore reports how much is older than cutoff, for a dry run.
//
// Counting before deleting is not politeness: the first time this runs is on a production log, and an
// operator needs to see the size before committing to it.
func (s *PG) AuditBefore(ctx context.Context, cutoff time.Time) (int64, time.Time, time.Time, error) {
	var (
		n     int64
		first *time.Time
		last  *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(ts), max(ts) FROM audit.events WHERE ts < $1`, cutoff).
		Scan(&n, &first, &last)
	if err != nil {
		return 0, time.Time{}, time.Time{}, translate(err)
	}
	var f, l time.Time
	if first != nil {
		f = *first
	}
	if last != nil {
		l = *last
	}
	return n, f, l, nil
}

// AuditEventsBefore returns events older than cutoff, oldest first, for archiving.
//
// The whole batch is read before anything is deleted: an archive that is written from a cursor while
// rows disappear underneath it would silently miss events, and the mark would then vouch for an
// archive that does not contain what it claims.
func (s *PG) AuditEventsBefore(ctx context.Context, cutoff time.Time) ([]audit.Event, error) {
	const q = `SELECT id, ts, actor_kind, actor_user_id, COALESCE(actor_email::text, ''),
		COALESCE(actor_ip::text,''), user_agent, action, target_kind, target_id,
		share_id, result, bytes, metadata, prev_hash, hash
		FROM audit.events
		WHERE ts < $1
		ORDER BY id`
	rows, err := s.pool.Query(ctx, q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: read audit rows to archive: %w", translate(err))
	}
	defer rows.Close()

	var out []audit.Event
	for rows.Next() {
		var e audit.Event
		var actorKind, action, targetKind, result string
		if err := rows.Scan(&e.ID, &e.Time, &actorKind, &e.ActorUserID, &e.ActorEmail,
			&e.ActorIP, &e.UserAgent, &action, &targetKind, &e.TargetID,
			&e.ShareID, &result, &e.Bytes, &e.Metadata, &e.PrevHash, &e.Hash); err != nil {
			return nil, fmt.Errorf("store: scan audit row: %w", translate(err))
		}
		e.ActorKind = audit.ActorKind(actorKind)
		e.Action = audit.Action(action)
		e.TargetKind = audit.TargetKind(targetKind)
		e.Result = audit.Result(result)
		out = append(out, e)
	}
	return out, translate(rows.Err())
}
