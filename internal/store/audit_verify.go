package store

import (
	"context"
	"fmt"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// VerifyAuditChain re-reads the audit log and checks the hash chain.
//
// It exists because a tamper-evident log that is never verified is just a log: the
// chain only has value if something reads it back and complains. This is what the
// daily anchor job calls before publishing the head.
//
// The whole log is loaded in one pass. For a year of events that is large but
// bounded, and it runs off the request path.
func (s *PG) VerifyAuditChain(ctx context.Context, limit int) error {
	const q = `SELECT ts, actor_kind, actor_user_id, COALESCE(actor_email::text, ''),
			COALESCE(actor_ip::text,''), user_agent, action, target_kind, target_id,
			share_id, result, bytes, metadata, prev_hash, hash
		FROM audit.events
		ORDER BY id DESC
		LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limitArg(limit))
	if err != nil {
		return fmt.Errorf("store: read audit log: %w", err)
	}
	defer rows.Close()

	var events []audit.Event
	for rows.Next() {
		var e audit.Event
		var actorKind, action, targetKind, result string
		if err := rows.Scan(&e.Time, &actorKind, &e.ActorUserID, &e.ActorEmail,
			&e.ActorIP, &e.UserAgent, &action, &targetKind, &e.TargetID,
			&e.ShareID, &result, &e.Bytes, &e.Metadata, &e.PrevHash, &e.Hash); err != nil {
			return fmt.Errorf("store: scan audit row: %w", err)
		}
		e.ActorKind = audit.ActorKind(actorKind)
		e.Action = audit.Action(action)
		e.TargetKind = audit.TargetKind(targetKind)
		e.Result = audit.Result(result)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// The query returns newest-first for the LIMIT to keep the most recent window;
	// the chain verifies oldest-first.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
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
			return audit.GenesisHash, nil
		}
		return nil, translate(err)
	}
	return head, nil
}

// RecordAnchor stores the published head so an auditor can compare.
func (s *PG) RecordAnchor(ctx context.Context, head []byte, destination string) error {
	const q = `INSERT INTO audit.anchors (day, head_hash, destination)
		VALUES (CURRENT_DATE, $1, $2)
		ON CONFLICT (day) DO UPDATE SET head_hash = EXCLUDED.head_hash,
			destination = EXCLUDED.destination, published_at = now()`
	_, err := s.pool.Exec(ctx, q, head, destination)
	return translate(err)
}

func isNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}
