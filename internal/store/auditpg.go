package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// chainWriter appends audit events with a hash chain, inside the caller's
// transaction.
//
// Two properties matter:
//
//  1. The chain is computed here, not by the caller. A caller that could supply
//     prev_hash could fork the chain and hide history; by reading the previous
//     hash inside the same transaction, the chain is a consequence of commit
//     order rather than of cooperation.
//  2. The row is written in the same transaction as the business change. If the
//     change rolls back, so does the record, so the log never claims something
//     that did not happen.
//
// The lock ordering is safe because this is the only writer of audit.events and
// it always takes the advisory lock before touching the table.
type chainWriter struct{}

// Append implements AuditWriter.
func (chainWriter) Append(ctx context.Context, tx Tx, e AuditEntry) error {
	ptx, ok := tx.(*pgTx)
	if !ok {
		return fmt.Errorf("store: chainWriter requires a PostgreSQL transaction, got %T", tx)
	}

	// One writer at a time across all transactions: without this, two concurrent
	// appends would both read the same predecessor and produce a forked chain
	// that verifies as broken.
	if _, err := ptx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, auditChainLock); err != nil {
		return fmt.Errorf("store: audit chain lock: %w", err)
	}

	// The chain head is the newest row, or the genesis value when the log is
	// empty. Reading it inside the same transaction (under the advisory lock
	// above) is what makes the chain a consequence of commit order rather than
	// of caller cooperation.
	prev := audit.GenesisHash
	var head []byte
	err := ptx.QueryRow(ctx, `SELECT hash FROM audit.events ORDER BY id DESC LIMIT 1`).Scan(&head)
	switch {
	case err == nil:
		prev = head
	case errors.Is(err, pgx.ErrNoRows):
		// Empty table. That is NOT necessarily the beginning of time: a pruned log has an
		// archived head, and chaining the next event from genesis would start a second chain
		// that no longer links to the archive — the mark would say the history ended at a hash
		// the new event does not build on.
		var markHead []byte
		if merr := ptx.QueryRow(ctx,
			`SELECT head_hash FROM audit.prune_marks ORDER BY through_id DESC LIMIT 1`).
			Scan(&markHead); merr == nil {
			prev = markHead
		}
	default:
		return fmt.Errorf("store: read audit chain head: %w", translate(err))
	}

	ev := audit.Event{
		Time:       ptx.now,
		ActorKind:  audit.ActorKind(orDefault(e.ActorKind, "system")),
		ActorEmail: e.ActorEmail,
		ActorIP:    e.ActorIP,
		UserAgent:  e.UserAgent,
		Action:     audit.Action(e.Action),
		TargetKind: audit.TargetKind(orDefault(e.TargetKind, "none")),
		TargetID:   e.TargetID,
		ShareID:    e.ShareID,
		Result:     audit.Result(orDefault(e.Result, "ok")),
		Bytes:      e.Bytes,
		Metadata:   e.Metadata,
		PrevHash:   prev,
	}
	ev.ActorUserID = e.ActorUserID
	if err := ev.Seal(); err != nil {
		return err
	}

	const q = `INSERT INTO audit.events
		(ts, actor_kind, actor_user_id, actor_email, actor_ip, user_agent,
		 action, target_kind, target_id, share_id, result, bytes, metadata, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`
	_, err = ptx.Exec(ctx, q,
		ev.Time, string(ev.ActorKind), ev.ActorUserID, nullifyEmpty(ev.ActorEmail),
		nullifyEmpty(ev.ActorIP), ev.UserAgent, string(ev.Action), string(ev.TargetKind),
		ev.TargetID, ev.ShareID, string(ev.Result), ev.Bytes,
		jsonOrEmpty(ev.Metadata), ev.PrevHash, ev.Hash)
	if err != nil {
		return fmt.Errorf("store: append audit: %w", translate(err))
	}
	return nil
}

// auditChainLock is an arbitrary but stable advisory lock id. It serialises the
// chain head read so concurrent commits cannot fork the chain.
const auditChainLock int64 = 0x7765626361 // "webca"

// EnableAudit wires the hash-chaining audit writer.
//
// It is opt-in rather than always-on so that a store constructed for a read-only
// tool does not silently start writing audit rows; but a serving process must call
// it, and Audit() fails loudly when it has not.
func (s *PG) EnableAudit() { s.auditWriter = chainWriter{} }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nullifyEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonOrEmpty(m map[string]any) any {
	if len(m) == 0 {
		return map[string]any{}
	}
	return m
}
