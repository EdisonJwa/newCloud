package store

// PostgreSQL implementation of the mount surface (ADR-0014).
//
// The queries are small on purpose: a mount row is a pointer, not a tree, and every
// operation here is a primary-key or unique-key lookup. The one statement with any
// subtlety is DeleteMount, which owns the pairing between the mount row and its
// mount-point node — see the comment there.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

const mountColumns = `id, node_id, owner_id, name, scheme, endpoint, root_path, username,
	secret_enc, read_only, created_by, health_status, health_detail, health_at, created_at`

func scanMount(row interface {
	Scan(dest ...any) error
}) (Mount, error) {
	var m Mount
	err := row.Scan(&m.ID, &m.NodeID, &m.OwnerID, &m.Name, &m.Scheme, &m.Endpoint,
		&m.RootPath, &m.Username, &m.SecretEnc, &m.ReadOnly, &m.CreatedBy,
		&m.HealthStatus, &m.HealthDetail, &m.HealthAt, &m.CreatedAt)
	if err != nil {
		return Mount{}, translate(err)
	}
	return m, nil
}

func (t *pgTx) CreateMount(ctx context.Context, m NewMount) (Mount, error) {
	const q = `INSERT INTO mounts (id, node_id, owner_id, name, scheme, endpoint, root_path,
		username, secret_enc, read_only, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING ` + mountColumns
	return scanMount(t.tx.QueryRow(ctx, q, m.ID, m.NodeID, m.OwnerID, m.Name, m.Scheme,
		m.Endpoint, m.RootPath, m.Username, m.SecretEnc, m.ReadOnly, m.CreatedBy))
}

func (t *pgTx) MountByNodeID(ctx context.Context, nodeID uuid.UUID) (Mount, error) {
	const q = `SELECT ` + mountColumns + ` FROM mounts WHERE node_id = $1`
	return scanMount(t.tx.QueryRow(ctx, q, nodeID))
}

func (t *pgTx) MountByID(ctx context.Context, id uuid.UUID) (Mount, error) {
	const q = `SELECT ` + mountColumns + ` FROM mounts WHERE id = $1`
	return scanMount(t.tx.QueryRow(ctx, q, id))
}

func (t *pgTx) MountsForOwner(ctx context.Context, ownerID uuid.UUID) ([]Mount, error) {
	rows, err := t.tx.Query(ctx,
		`SELECT `+mountColumns+` FROM mounts WHERE owner_id = $1 ORDER BY name`, ownerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	var out []Mount
	for rows.Next() {
		m, err := scanMount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) AllMounts(ctx context.Context) ([]Mount, error) {
	rows, err := t.tx.Query(ctx,
		`SELECT `+mountColumns+` FROM mounts ORDER BY owner_id, name`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	var out []Mount
	for rows.Next() {
		m, err := scanMount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// DeleteMount removes the mount row and its mount-point node, hard, in the one transaction.
//
// The child guard is a CHECK-style refusal rather than a cascade: a mount point with live
// children cannot exist by construction (nothing below a mount point is stored), so hitting
// the guard means some other path created rows under a mount — exactly the drift this
// refuses to paper over. The pairing (both the mount id and the node id) is in the WHERE of
// both deletes, so a caller cannot name one mount and delete another's node.
func (t *pgTx) DeleteMount(ctx context.Context, id uuid.UUID) error {
	var nodeID uuid.UUID
	err := t.tx.QueryRow(ctx,
		`SELECT node_id FROM mounts WHERE id = $1 FOR UPDATE`, id).Scan(&nodeID)
	if err != nil {
		return translate(err)
	}
	tag, err := t.tx.Exec(ctx, `DELETE FROM nodes WHERE id = $1
		AND NOT EXISTS (SELECT 1 FROM nodes c WHERE c.parent_id = nodes.id AND c.trashed_at IS NULL)`,
		nodeID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		// The node grew children, or the pairing changed underneath the read. Both are
		// "stop and look", not "delete something else".
		return errors.New("mount node has live children; refusing to delete")
	}
	if _, err := t.tx.Exec(ctx, `DELETE FROM mounts WHERE id = $1 AND node_id = $2`, id, nodeID); err != nil {
		return translate(err)
	}
	return nil
}

func (t *pgTx) SetMountHealth(ctx context.Context, id uuid.UUID, status, detail string, at time.Time) error {
	_, err := t.tx.Exec(ctx,
		`UPDATE mounts SET health_status = $2, health_detail = $3, health_at = $4 WHERE id = $1`,
		id, status, detail, at)
	return translate(err)
}
