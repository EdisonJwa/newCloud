package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SharedWithMe answers "which of somebody else's nodes can I reach".
//
// This exists because of a gap the ACL work exposed. The permission model resolves
// correctly — a grant on another account's folder produces the right capabilities —
// but every existing entry point resolves a *path* inside the caller's own namespace:
// WebDAV discards the username segment and the JSON API resolves against the caller's
// root. So a grant was necessary and useless: nothing could route a request to the
// node it described.
//
// Rather than weaken the namespace isolation those paths rely on (it is what stops one
// account addressing another's tree by guessing a name), the model gains an explicit
// second way in: a list of nodes the caller has been granted access to, addressed by
// id. Ids are unguessable UUIDs, and reaching one still goes through the authorizer.
//
// The result is one row per shared *root* — the highest node in each granted subtree
// that the caller can see. Listing every descendant would show a shared folder as
// hundreds of independent entries.
type SharedNode struct {
	NodeID     uuid.UUID
	OwnerID    uuid.UUID
	Name       string
	Kind       Kind
	Size       int64
	Mime       string
	ModifiedAt time.Time
	// Perms is what the actor may do here, already resolved.
	Perms Perm
	// ViaGroup is set when the grant came from a group rather than directly.
	ViaGroup bool
}

// SharedTx is the "shared with me" surface.
type SharedTx interface {
	// SharedWithMe lists nodes another account has granted the actor access to.
	//
	// Only roots are returned: a node whose parent the actor can also reach is part
	// of a larger shared subtree and is discovered by descending from that root.
	SharedWithMe(ctx context.Context, actorID uuid.UUID, limit int) ([]SharedNode, error)
}

// sharedWithMeQuery is the SQL and the shape the memory implementation mirrors.
//
// The DISTINCT ON picks one row per node even when several grants reach it (a direct
// grant and two group grants, say); the permission union is applied afterwards by the
// caller so this stays a plain "which nodes" question.
const sharedWithMeQuery = `
	WITH RECURSIVE chain AS (
	    -- every grant that names this actor, directly or through a group
	    SELECT a.node_id, (a.group_id IS NOT NULL) AS via_group
	    FROM acl_entries a
	    WHERE a.user_id = $1
	       OR a.group_id IN (SELECT group_id FROM group_members WHERE user_id = $1)
	),
	inherited AS (
	    -- a grant covers its node and, while inherit is set, everything below it
	    SELECT c.node_id AS root_id, n.id AS covered_id
	    FROM chain c
	    JOIN nodes n ON n.id = c.node_id
	    UNION ALL
	    SELECT i.root_id, child.id
	    FROM inherited i
	    JOIN nodes child ON child.parent_id = i.covered_id
	    JOIN acl_entries g ON g.node_id = i.root_id
	    WHERE g.inherit
	)
	SELECT DISTINCT ON (n.id)
	       n.id, n.owner_id, n.name, n.kind, n.size, n.mime, n.modified_at
	FROM inherited i
	JOIN nodes n ON n.id = i.covered_id
	WHERE n.trashed_at IS NULL
	  AND n.owner_id <> $1
	  -- only roots: if the parent is also covered by this actor, this is not a root
	  AND NOT EXISTS (
	        SELECT 1 FROM inherited p WHERE p.covered_id = n.parent_id
	  )
	ORDER BY n.id, n.name
	LIMIT $2`

// SharedWithMe implements SharedTx for PostgreSQL.
func (t *pgTx) SharedWithMe(ctx context.Context, actorID uuid.UUID, limit int) ([]SharedNode, error) {
	rows, err := t.tx.Query(ctx, sharedWithMeQuery, actorID, limitArg(limit))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []SharedNode
	for rows.Next() {
		var (
			n    SharedNode
			kind string
		)
		if err := rows.Scan(&n.NodeID, &n.OwnerID, &n.Name, &kind, &n.Size,
			&n.Mime, &n.ModifiedAt); err != nil {
			return nil, translate(err)
		}
		n.Kind = Kind(kind)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}

	// Resolve the effective permissions per node, so the caller does not have to
	// ask again for every row.
	for i := range out {
		perms, ok, perr := t.EffectivePermsFor(ctx, actorID, out[i].NodeID)
		if perr != nil {
			return nil, perr
		}
		if !ok {
			// A grant was revoked between the two queries. Dropping the row is the
			// correct answer: the caller must not see something it can no longer use.
			continue
		}
		out[i].Perms = perms
	}
	filtered := out[:0]
	for _, n := range out {
		if n.Perms != 0 {
			filtered = append(filtered, n)
		}
	}
	return filtered, nil
}

var _ SharedTx = (*pgTx)(nil)
