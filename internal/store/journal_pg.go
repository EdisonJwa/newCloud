package store

// The PostgreSQL journal, against the table migration 0016 already created.

import (
	"context"

	"github.com/google/uuid"
)

// opColumns is the read order for scanOp; kept next to the scan so they cannot drift apart.
const opColumns = `id, owner_id, kind, state, staged_root, created_at, updated_at`

func scanOp(row interface{ Scan(...any) error }) (Op, error) {
	var op Op
	if err := row.Scan(&op.ID, &op.OwnerID, &op.Kind, &op.State, &op.StagedRoot,
		&op.CreatedAt, &op.UpdatedAt); err != nil {
		return Op{}, translate(err)
	}
	return op, nil
}

// StartOp writes the intent.
//
// The row goes in before any content is touched, which is what the whole design rests on: an
// operation that crashed before its row existed changed nothing, and one that crashed after it
// existed can be found by the recovery pass.
func (t *pgTx) StartOp(ctx context.Context, op Op) (Op, error) {
	if op.ID == uuid.Nil {
		op.ID = uuid.New()
	}
	if op.State == "" {
		op.State = OpIntent
	}

	got, err := scanOp(t.tx.QueryRow(ctx, `
		INSERT INTO operations (id, owner_id, kind, state, staged_root)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+opColumns,
		op.ID, op.OwnerID, op.Kind, op.State, op.StagedRoot))
	if err != nil {
		return Op{}, err
	}
	return got, nil
}

// AdvanceOp moves an operation, refusing if it is not in the expected state.
func (t *pgTx) AdvanceOp(ctx context.Context, id uuid.UUID, from, to string) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE operations SET state = $3, updated_at = now()
		WHERE id = $1 AND state = $2`, id, from, to)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row is gone or somebody already moved it. Both mean this caller's view is
		// stale, which is exactly what ErrPrecondition reports.
		return ErrPrecondition
	}
	return nil
}

// OpByID reads one operation.
func (t *pgTx) OpByID(ctx context.Context, id uuid.UUID) (Op, error) {
	return scanOp(t.tx.QueryRow(ctx, `SELECT `+opColumns+` FROM operations WHERE id = $1`, id))
}

// OpenOps lists operations recovery should look at, oldest first.
//
// Oldest first is deliberate: a bounded pass should spend its work on the operations waiting
// longest, not on whichever rows the planner returns first. NEEDS_OPERATOR is excluded because
// recovery itself produces it by refusing to guess.
func (t *pgTx) OpenOps(ctx context.Context, limit int) ([]Op, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := t.tx.Query(ctx, `SELECT `+opColumns+` FROM operations
		WHERE state NOT IN ('COMMITTED', 'ABORTED', 'DISCARDED', 'NEEDS_OPERATOR')
		ORDER BY created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Op
	for rows.Next() {
		op, serr := scanOp(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, op)
	}
	return out, translate(rows.Err())
}
