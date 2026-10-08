package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ReadAudit returns the events matching f, oldest first by default.
//
// The filter is assembled as a parameter list rather than string concatenation: every
// value comes from an HTTP query, and building SQL from request input is how injection
// happens. The only literal that varies is the ORDER BY direction, which is chosen from
// a bool.
func (s *PG) ReadAudit(ctx context.Context, f AuditFilter) ([]AuditRow, error) {
	where, args := auditWhere(f)

	order := "ASC"
	if f.Descending {
		order = "DESC"
	}

	q := fmt.Sprintf(`SELECT id, ts, actor_kind, actor_user_id, COALESCE(actor_email::text, ''),
			COALESCE(actor_ip::text, ''), user_agent, action, target_kind, target_id,
			share_id, result, bytes, metadata, prev_hash, hash, request_id
		FROM audit.events
		%s
		ORDER BY id %s
		LIMIT $%d OFFSET $%d`, where, order, len(args)+1, len(args)+2)

	args = append(args, limitArg(f.Limit), offsetArg(f.Offset))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make([]AuditRow, 0, 64)
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.Time, &r.ActorKind, &r.ActorUserID, &r.ActorEmail,
			&r.ActorIP, &r.UserAgent, &r.Action, &r.TargetKind, &r.TargetID,
			&r.ShareID, &r.Result, &r.Bytes, &r.Metadata, &r.PrevHash, &r.Hash, &r.RequestID,
			&r.AuthMethod); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// CountAudit reports the number of matching events.
func (s *PG) CountAudit(ctx context.Context, f AuditFilter) (int, error) {
	where, args := auditWhere(f)
	q := `SELECT count(*) FROM audit.events ` + where

	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// auditWhere builds the WHERE clause and its arguments.
//
// It returns the clause with a leading WHERE, or an empty string when nothing is
// filtered — an unconditional "WHERE" with no predicate is a syntax error, and getting
// that wrong turns "export everything" into a 500.
func auditWhere(f AuditFilter) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if f.Since != nil {
		clauses = append(clauses, "ts >= "+next(*f.Since))
	}
	if f.Until != nil {
		// Exclusive upper bound: a range expressed as two dates covers whole days
		// without either including the other's midnight twice.
		clauses = append(clauses, "ts < "+next(*f.Until))
	}
	if len(f.Actions) > 0 {
		clauses = append(clauses, "action = ANY("+next(f.Actions)+")")
	}
	if f.ActorUserID != nil {
		clauses = append(clauses, "actor_user_id = "+next(*f.ActorUserID))
	}
	if f.ShareID != nil {
		clauses = append(clauses, "share_id = "+next(*f.ShareID))
	}
	if f.Result != "" {
		clauses = append(clauses, "result = "+next(f.Result))
	}
	if f.ScopeToOwner != nil {
		// Two disjoint ways an event belongs to the caller:
		//
		//   1. they caused it (their own uploads, logins, credential changes);
		//   2. it was about a share they own (an anonymous download, a refused recipient
		//      attempt) — these have no actor account at all.
		//
		// Case 2 is why this scope exists. A pure actor filter answered "what did I do"
		// while the question being asked is "what happened to my files".
		owner := next(*f.ScopeToOwner)
		clauses = append(clauses, "(actor_user_id = "+owner+
			" OR share_id IN (SELECT id FROM shares WHERE owner_id = "+owner+"))")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

// Compile-time check that the PG store satisfies the reader.
var _ AuditReader = (*PG)(nil)

// pgx is referenced for the Row alias used by the shared scanners.
var _ pgx.Row
