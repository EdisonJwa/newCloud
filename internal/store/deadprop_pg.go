package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL dead-property implementation.

func (t *pgTx) SetDeadProp(ctx context.Context, nodeID uuid.UUID, space, name, value string) error {
	const q = `INSERT INTO dead_props (node_id, namespace, name, value)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (node_id, namespace, name) DO UPDATE SET value = EXCLUDED.value`
	_, err := t.tx.Exec(ctx, q, nodeID, space, name, value)
	return translate(err)
}

func (t *pgTx) DeadProps(ctx context.Context, nodeID uuid.UUID) ([]DeadProp, error) {
	const q = `SELECT node_id, namespace, name, value FROM dead_props
		WHERE node_id = $1 ORDER BY namespace, name`
	rows, err := t.tx.Query(ctx, q, nodeID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	return scanDeadProps(rows)
}

func (t *pgTx) DeadPropsFor(ctx context.Context, nodeIDs []uuid.UUID) (map[uuid.UUID][]DeadProp, error) {
	out := make(map[uuid.UUID][]DeadProp, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	// The ids travel as text and are cast, so the array encoding cannot depend on how the
	// driver maps a slice of the google/uuid type.
	ids := make([]string, len(nodeIDs))
	for i, id := range nodeIDs {
		ids[i] = id.String()
	}
	const q = `SELECT node_id, namespace, name, value FROM dead_props
		WHERE node_id = ANY($1::uuid[]) ORDER BY node_id, namespace, name`
	rows, err := t.tx.Query(ctx, q, ids)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p DeadProp
		if err := rows.Scan(&p.NodeID, &p.Space, &p.Name, &p.Value); err != nil {
			return nil, translate(err)
		}
		out[p.NodeID] = append(out[p.NodeID], p)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) DeleteDeadProp(ctx context.Context, nodeID uuid.UUID, space, name string) error {
	const q = `DELETE FROM dead_props WHERE node_id = $1 AND namespace = $2 AND name = $3`
	_, err := t.tx.Exec(ctx, q, nodeID, space, name)
	return translate(err)
}

func scanDeadProps(rows pgx.Rows) ([]DeadProp, error) {
	var out []DeadProp
	for rows.Next() {
		var p DeadProp
		if err := rows.Scan(&p.NodeID, &p.Space, &p.Name, &p.Value); err != nil {
			return nil, translate(err)
		}
		out = append(out, p)
	}
	return out, translate(rows.Err())
}
