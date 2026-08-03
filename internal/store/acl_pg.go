package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementation of ACLTx.

// aclColumns is the shared select list, so the single-row and multi-row paths cannot
// drift apart.
const aclColumns = `id, node_id, user_id, group_id, perms, inherit, created_at`

func scanACL(row pgx.Row) (ACLEntry, error) {
	var e ACLEntry
	// perms is a comma-separated text column, exactly as the schema's own comment
	// describes ('read,write'). Storing it as an array was the obvious first move
	// and pgx rejected it at encode time — the conformance suite caught that before
	// it could reach a deployment.
	var perms string
	err := row.Scan(&e.ID, &e.NodeID, &e.UserID, &e.GroupID, &perms, &e.Inherit, &e.CreatedAt)
	if err != nil {
		return ACLEntry{}, translate(err)
	}
	e.Perms = DecodePerms(splitPermList(perms))
	return e, nil
}

// splitPermList splits the stored comma-separated list, tolerating spaces and empty
// entries so a hand-edited row does not break authorization.
func splitPermList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Grant adds or replaces a grant for one subject on one node.
//
// "Replace" rather than "duplicate" is deliberate: two rows for the same
// (node, subject) would make the effective permission depend on which row a query
// happened to read, and the only sane answer is their union — which is the same as
// storing the intended set once.
func (t *pgTx) Grant(ctx context.Context, e ACLEntry) (ACLEntry, error) {
	if (e.UserID == nil) == (e.GroupID == nil) {
		return ACLEntry{}, fmt.Errorf("%w: a grant names exactly one of user or group", ErrInvalidName)
	}
	names := strings.Join(EncodePerms(e.Perms), ",")
	if len(names) == 0 {
		return ACLEntry{}, fmt.Errorf("%w: a grant with no permissions", ErrInvalidName)
	}

	// The two partial unique indexes this relies on are created in migration 0004.
	// Separate statements rather than one clever CASE: PostgreSQL allows only one
	// conflict target per INSERT, and the two partial indexes cannot be combined.
	if e.UserID != nil {
		const q = `INSERT INTO acl_entries (node_id, user_id, group_id, perms, inherit, created_by)
			VALUES ($1, $2, NULL, $3, $4, $5)
			ON CONFLICT (node_id, user_id) WHERE user_id IS NOT NULL DO UPDATE
				SET perms = EXCLUDED.perms, inherit = EXCLUDED.inherit
			RETURNING ` + aclColumns
		return scanACL(t.tx.QueryRow(ctx, q, e.NodeID, *e.UserID, names, e.Inherit, e.CreatedBy))
	}
	const gq = `INSERT INTO acl_entries (node_id, user_id, group_id, perms, inherit, created_by)
		VALUES ($1, NULL, $2, $3, $4, $5)
		ON CONFLICT (node_id, group_id) WHERE group_id IS NOT NULL DO UPDATE
			SET perms = EXCLUDED.perms, inherit = EXCLUDED.inherit
		RETURNING ` + aclColumns
	return scanACL(t.tx.QueryRow(ctx, gq, e.NodeID, *e.GroupID, names, e.Inherit, e.CreatedBy))
}

func (t *pgTx) Revoke(ctx context.Context, id uuid.UUID) error {
	const q = `DELETE FROM acl_entries WHERE id = $1`
	tag, err := t.tx.Exec(ctx, q, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (t *pgTx) ACLForNode(ctx context.Context, nodeID uuid.UUID) ([]ACLEntry, error) {
	const q = `SELECT ` + aclColumns + ` FROM acl_entries
		WHERE node_id = $1 ORDER BY created_at`
	rows, err := t.tx.Query(ctx, q, nodeID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []ACLEntry
	for rows.Next() {
		e, serr := scanACL(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, e)
	}
	return out, translate(rows.Err())
}

// EffectivePermsFor resolves the actor's permissions on a node by walking upward.
//
// The walk is a recursive CTE that numbers each level by distance from the target, so
// one predicate decides inheritance for the whole chain: a grant applies at its own
// node (distance 0) and at any descendant, but below it only when inherit is true.
//
// Doing this in one statement rather than a round trip per level matters for more
// than latency: a concurrent move between two reads would let the walk see a tree
// that never existed.
func (t *pgTx) EffectivePermsFor(ctx context.Context, actorID, nodeID uuid.UUID) (Perm, bool, error) {
	const q = `
		WITH RECURSIVE chain AS (
		    SELECT id, parent_id, 0 AS distance FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id, n.parent_id, c.distance + 1
		    FROM nodes n JOIN chain c ON n.id = c.parent_id
		)
		SELECT a.perms
		FROM acl_entries a
		JOIN chain c ON c.id = a.node_id
		WHERE (c.distance = 0 OR a.inherit)
		  AND (
		        a.user_id = $2
		     OR a.group_id IN (SELECT group_id FROM group_members WHERE user_id = $2)
		  )`

	rows, err := t.tx.Query(ctx, q, nodeID, actorID)
	if err != nil {
		return 0, false, translate(err)
	}
	defer rows.Close()

	var (
		out Perm
		any bool
	)
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return 0, false, translate(err)
		}
		out |= DecodePerms(splitPermList(stored))
		any = true
	}
	if err := rows.Err(); err != nil {
		return 0, false, translate(err)
	}
	return out, any, nil
}

var _ ACLTx = (*pgTx)(nil)
var _ = errors.Is

// UserByUsername resolves a grant subject named in a manage request.
//
// Case-insensitive because the column is citext: an owner typing "Alice" must find
// the account stored as "alice", and a mismatch here reads as "no such user".
func (t *pgTx) UserByUsername(ctx context.Context, username string) (User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE username = $1`
	return scanUser(t.tx.QueryRow(ctx, q, username))
}

// UserByEmail resolves an account by address.
func (t *pgTx) UserByEmail(ctx context.Context, email string) (User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE email = $1`
	return scanUser(t.tx.QueryRow(ctx, q, email))
}
