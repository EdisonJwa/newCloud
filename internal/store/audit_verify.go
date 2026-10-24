package store

import (
	"context"
	"fmt"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// AuditRows returns events after a sequence position, oldest first, for the chain
// verifier.
//
// Keyset pagination on id rather than OFFSET: the table is append-only and the
// verifier walks the whole log, so an offset would re-scan from the start for every
// batch and turn verification of a large log into a quadratic query.
func (s *PG) AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error) {
	const q = `SELECT id, ts, actor_kind, actor_user_id, COALESCE(actor_email::text, ''),
			COALESCE(actor_ip::text,''), user_agent, action, target_kind, target_id,
			share_id, result, bytes, metadata, prev_hash, hash, request_id, auth_method, hash_version
		FROM audit.events
		WHERE id > $1
		ORDER BY id
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, afterID, limitArg(limit))
	if err != nil {
		return nil, fmt.Errorf("store: read audit rows: %w", translate(err))
	}
	defer rows.Close()

	out := make([]audit.Event, 0, limit)
	for rows.Next() {
		var e audit.Event
		var actorKind, action, targetKind, result string
		if err := rows.Scan(&e.ID, &e.Time, &actorKind, &e.ActorUserID, &e.ActorEmail,
			&e.ActorIP, &e.UserAgent, &action, &targetKind, &e.TargetID,
			&e.ShareID, &result, &e.Bytes, &e.Metadata, &e.PrevHash, &e.Hash, &e.RequestID,
			&e.AuthMethod, &e.HashVersion); err != nil {
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

// VerifyAuditChain re-reads the audit log and checks the hash chain.
//
// Kept for callers that want the boolean answer; the CLI uses auditverify.Run so the
// operator gets a report rather than an error string.
func (s *PG) VerifyAuditChain(ctx context.Context, limit int) error {
	events, err := s.AuditRows(ctx, 0, limit)
	if err != nil {
		return err
	}
	bad, err := audit.VerifyChain(events)
	if err != nil {
		return fmt.Errorf("store: audit chain broken at event %d: %w", bad, err)
	}
	return nil
}

// AuditHead returns the current chain head, which the anchor publishes externally.
func (s *PG) AuditHead(ctx context.Context) ([]byte, error) {
	var head []byte
	err := s.pool.QueryRow(ctx,
		`SELECT hash FROM audit.events ORDER BY id DESC LIMIT 1`).Scan(&head)
	if err != nil {
		if isNoRows(err) {
			// An empty table after a prune still has a head: the newest archive boundary. Returning
			// genesis would tell the verifier the log was truncated to nothing, which is exactly
			// what a pruned log is not.
			var markHead []byte
			if merr := s.pool.QueryRow(ctx,
				`SELECT head_hash FROM audit.prune_marks ORDER BY through_id DESC LIMIT 1`).
				Scan(&markHead); merr == nil {
				return markHead, nil
			}
			return audit.GenesisHash, nil
		}
		return nil, translate(err)
	}
	return head, nil
}

// RecordAnchor stores the published head so an auditor can compare.
//
// It upserts rather than inserts because a drill may re-publish a day. Note that the
// application role holds only INSERT on audit.anchors; running this needs the
// migration role, which is intentional: writing an anchor from the serving process
// would let an attacker who owns that process rewrite the checkpoint too.
func (s *PG) RecordAnchor(ctx context.Context, head []byte, destination string) error {
	const q = `INSERT INTO audit.anchors (day, head_hash, destination)
		VALUES (CURRENT_DATE, $1, $2)
		ON CONFLICT (day) DO UPDATE SET head_hash = EXCLUDED.head_hash,
			destination = EXCLUDED.destination, published_at = now()`
	_, err := s.pool.Exec(ctx, q, head, destination)
	return translate(err)
}

// AuditCount returns how many events the chain holds.
//
// A count(*) over an append-only table is the honest number (the biggest id is not:
// pruning removes rows). The worker samples it hourly, which is a cost a gauge can
// afford and a per-request path cannot — which is why this is not called from
// anywhere hot.
func (s *PG) AuditCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit.events`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count audit events: %w", translate(err))
	}
	return n, nil
}

// AuditNewest returns the time of the newest event, and whether one exists at all.
//
// The primary key makes this an index lookup rather than a scan, which is the
// property the head-age gauge depends on: it is published on every anchor tick, and a
// monitoring query that cost a table walk would be a monitoring query somebody
// switched off. A pruned-empty table reports no rows honestly.
func (s *PG) AuditNewest(ctx context.Context) (time.Time, bool, error) {
	var ts time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT ts FROM audit.events ORDER BY id DESC LIMIT 1`).Scan(&ts)
	if err != nil {
		if isNoRows(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("store: read newest audit event: %w", translate(err))
	}
	return ts, true, nil
}

func isNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}
