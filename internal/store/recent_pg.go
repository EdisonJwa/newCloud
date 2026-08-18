package store

// RecentFiles on PostgreSQL.
//
// One query answers the whole thing. The tempting shape — select the recent rows, then walk up
// each one's parents in Go — is N+1 exactly where it hurts, because the home screen is the
// first request after sign-in. The recursive CTE builds every ancestor chain in a single pass
// and the string aggregation assembles the path, with a depth cap so a corrupted parent cycle
// cannot spin forever.

import (
	"context"

	"github.com/google/uuid"
)

// RecentFiles implements RecentReader.
func (s *PG) RecentFiles(ctx context.Context, ownerID uuid.UUID, limit int) ([]RecentFile, error) {
	if limit <= 0 {
		limit = 12
	}
	if limit > 100 {
		limit = 100
	}

	const q = `
		WITH RECURSIVE recent AS (
		    SELECT id
		    FROM nodes
		    WHERE owner_id = $1 AND kind = 'file' AND trashed_at IS NULL
		    ORDER BY modified_at DESC
		    LIMIT $2
		), up(origin, id, parent_id, name, depth) AS (
		    SELECT id, id, parent_id, name, 0
		    FROM nodes
		    WHERE id IN (SELECT id FROM recent)
		    UNION ALL
		    SELECT up.origin, n.id, n.parent_id, n.name, up.depth + 1
		    FROM nodes n
		    JOIN up ON n.id = up.parent_id
		    WHERE up.depth < 64
		), chains AS (
		    SELECT origin,
		           string_agg(name, '/' ORDER BY depth DESC) FILTER (WHERE depth > 0 AND name <> '') AS dirs
		    FROM up
		    GROUP BY origin
		)
		SELECT ` + nodeColumns + `,
		       COALESCE(chains.dirs || '/', '') || n.name AS path
		FROM nodes n
		LEFT JOIN chains ON chains.origin = n.id
		WHERE n.id IN (SELECT id FROM recent)
		ORDER BY n.modified_at DESC`

	rows, err := s.pool.Query(ctx, q, ownerID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make([]RecentFile, 0, limit)
	for rows.Next() {
		var n Node
		var kind string
		var path string
		if err := rows.Scan(&n.ID, &n.ParentID, &n.OwnerID, new(*uuid.UUID), &n.Name, &kind,
			&n.Size, &n.Mime, &n.ETag, &n.BlobID, &n.CreatedAt, &n.ModifiedAt, &n.TrashedAt,
			&n.RetainUntil, &path); err != nil {
			return nil, translate(err)
		}
		n.Kind = Kind(kind)
		out = append(out, RecentFile{Node: n, Path: path})
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	return out, nil
}
