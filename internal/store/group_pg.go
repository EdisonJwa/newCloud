package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementation of GroupTx.

const groupColumns = `id, name, display_name, quota_bytes`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.DisplayName, &g.QuotaBytes)
	if err != nil {
		return Group{}, translate(err)
	}
	return g, nil
}

// CreateGroup creates the group, or returns the existing one with that name.
//
// Idempotent because group membership arrives from the IdP on every login: a
// non-idempotent create would fail the second time a user from a known group signs
// in, which is every sign-in.
func (t *pgTx) CreateGroup(ctx context.Context, g Group) (Group, error) {
	const q = `INSERT INTO groups (name, display_name, quota_bytes)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), groups.display_name)
		RETURNING ` + groupColumns
	return scanGroup(t.tx.QueryRow(ctx, q, g.Name, g.DisplayName, g.QuotaBytes))
}

func (t *pgTx) GroupByName(ctx context.Context, name string) (Group, error) {
	const q = `SELECT ` + groupColumns + ` FROM groups WHERE name = $1`
	return scanGroup(t.tx.QueryRow(ctx, q, name))
}

func (t *pgTx) AddGroupMember(ctx context.Context, groupID, userID uuid.UUID, role string) error {
	if role == "" {
		role = "viewer"
	}
	const q = `INSERT INTO group_members (group_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (group_id, user_id) DO UPDATE SET role = EXCLUDED.role`
	_, err := t.tx.Exec(ctx, q, groupID, userID, role)
	return translate(err)
}

func (t *pgTx) RemoveGroupMember(ctx context.Context, groupID, userID uuid.UUID) error {
	const q = `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`
	_, err := t.tx.Exec(ctx, q, groupID, userID)
	return translate(err)
}

func (t *pgTx) GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error) {
	const q = `SELECT g.id, g.name, g.display_name, g.quota_bytes
		FROM groups g
		JOIN group_members m ON m.group_id = g.id
		WHERE m.user_id = $1
		ORDER BY g.name`
	rows, err := t.tx.Query(ctx, q, userID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Group
	for rows.Next() {
		g, serr := scanGroup(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, g)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) GroupMembers(ctx context.Context, groupID uuid.UUID) ([]GroupMember, error) {
	const q = `SELECT group_id, user_id, role FROM group_members
		WHERE group_id = $1 ORDER BY user_id`
	rows, err := t.tx.Query(ctx, q, groupID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []GroupMember
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.GroupID, &m.UserID, &m.Role); err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// Groups lists every group, ordered by name.
func (t *pgTx) Groups(ctx context.Context) ([]Group, error) {
	const q = `SELECT ` + groupColumns + ` FROM groups ORDER BY name`
	rows, err := t.tx.Query(ctx, q)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Group
	for rows.Next() {
		g, serr := scanGroup(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, g)
	}
	return out, translate(rows.Err())
}

var _ GroupTx = (*pgTx)(nil)

// GroupByID resolves a group by id.
func (t *pgTx) GroupByID(ctx context.Context, id uuid.UUID) (Group, error) {
	const q = `SELECT ` + groupColumns + ` FROM groups WHERE id = $1`
	return scanGroup(t.tx.QueryRow(ctx, q, id))
}
