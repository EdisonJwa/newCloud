package store

// The hash-version read surface for audit exports (2026-10-25 review, fix 4).
//
// ReadAudit deliberately returns AuditRow, which mirrors what an exporter needs and does not
// carry the preimage version: the row's hash_version decides WHICH canonical form the event
// was sealed under, and an export that omits it cannot be re-verified — the verifier would
// recompute a version-1 preimage over a version-2 event and report an honest export as
// modified. So the version travels separately, behind an optional interface, and the exporter
// joins it to the rows it already has by id.
//
// Optional rather than part of AuditReader because every existing implementation should not
// have to grow a second query to keep compiling: a backend that does not implement it yields
// exports without the column, which the loader reads as version 1 — the honest answer for a
// backend that never wrote versioned rows anyway.

import (
	"context"
)

// AuditVersionReader reports the preimage version of audit events, keyed by event id.
//
// The filter is the same shape ReadAudit takes, so an exporter that read N rows with a filter
// asks for exactly those rows' versions and the two queries cannot disagree about the set.
type AuditVersionReader interface {
	AuditHashVersions(ctx context.Context, f AuditFilter) (map[int64]int, error)
}

// AuditHashVersions implements the reader for PostgreSQL.
func (s *PG) AuditHashVersions(ctx context.Context, f AuditFilter) (map[int64]int, error) {
	where, args := auditWhere(f)
	q := `SELECT id, hash_version FROM audit.events ` + where
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make(map[int64]int, 64)
	for rows.Next() {
		var id int
		var v int
		if err := rows.Scan(&id, &v); err != nil {
			return nil, translate(err)
		}
		out[int64(id)] = v
	}
	return out, translate(rows.Err())
}

// AuditHashVersions implements the reader for the in-memory store.
//
// The memory backend keeps no hash chain at all (AuditEntry carries neither digest nor
// version), so every row reports version 1 — the version whose preimage covers none of the
// request fields, which is what a row without a version column means everywhere else.
func (m *Memory) AuditHashVersions(ctx context.Context, f AuditFilter) (map[int64]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[int64]int, len(m.audit))
	for i := range m.audit {
		out[int64(i+1)] = 1
	}
	return out, nil
}

var _ AuditVersionReader = (*PG)(nil)
var _ AuditVersionReader = (*Memory)(nil)
