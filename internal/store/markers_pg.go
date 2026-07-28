package store

// MarkersFor on the PostgreSQL backend: two aggregate queries for the whole page.

import (
	"context"

	"github.com/google/uuid"
)

// MarkersFor returns share counts and version presence for the given nodes.
//
// Two queries, never one per node. The share query filters on owner_id as well as node_id:
// a node id is a UUID and cannot be guessed, but scoping by owner means a bug that passed the
// wrong ids would still return nothing rather than another tenant's sharing state.
func (s *PG) MarkersFor(ctx context.Context, ownerID uuid.UUID, nodeIDs []uuid.UUID) (map[uuid.UUID]Marker, error) {
	out := make(map[uuid.UUID]Marker, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}

	const shareQ = `
		SELECT node_id, count(*)
		FROM shares
		WHERE owner_id = $1 AND revoked_at IS NULL AND node_id = ANY($2)
		GROUP BY node_id`

	rows, err := s.pool.Query(ctx, shareQ, ownerID, nodeIDs)
	if err != nil {
		return nil, translate(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, translate(err)
		}
		m := out[id]
		m.ShareCount = n
		out[id] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}

	const versionQ = `
		SELECT DISTINCT node_id
		FROM versions
		WHERE node_id = ANY($1)`

	vrows, err := s.pool.Query(ctx, versionQ, nodeIDs)
	if err != nil {
		return nil, translate(err)
	}
	for vrows.Next() {
		var id uuid.UUID
		if err := vrows.Scan(&id); err != nil {
			vrows.Close()
			return nil, translate(err)
		}
		m := out[id]
		m.HasVersions = true
		out[id] = m
	}
	vrows.Close()
	if err := vrows.Err(); err != nil {
		return nil, translate(err)
	}

	return out, nil
}
