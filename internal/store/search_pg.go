package store

// Search on PostgreSQL.
//
// Substring matching uses strpos(lower(name), lower($2)) rather than LIKE: a user's query is
// data, and feeding "%" or "_" from it into a LIKE pattern would silently change what the
// query means. strpos has no metacharacters, so there is nothing to escape.
//
// There is no index on lower(name), so this is a bounded sequential scan. At the scale this
// is deployed that is fine, and the LIMIT keeps the cost predictable. If it stops being fine,
// the next step is a pg_trgm index on name — a migration, not a rewrite of this query.

import (
	"context"

	"github.com/google/uuid"
)

// Search implements Searcher.
func (s *PG) Search(ctx context.Context, ownerID uuid.UUID, query string, limit int) ([]RecentFile, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	const q = `
		WITH RECURSIVE hit AS (
		    SELECT id
		    FROM nodes
		    WHERE owner_id = $1
		      AND trashed_at IS NULL
		      AND strpos(lower(name), lower($2)) > 0
		    ORDER BY modified_at DESC
		    LIMIT $3
		), up(origin, id, parent_id, name, depth) AS (
		    SELECT id, id, parent_id, name, 0
		    FROM nodes
		    WHERE id IN (SELECT id FROM hit)
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
		WHERE n.id IN (SELECT id FROM hit)
		ORDER BY n.modified_at DESC`

	rows, err := s.pool.Query(ctx, q, ownerID, query, limit)
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
			&path); err != nil {
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
