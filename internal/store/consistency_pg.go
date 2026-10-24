package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EdisonJwa/newCloud/internal/consistency"
)

// PostgreSQL Phase 1 foundations. These methods are intentionally not part of
// Store/Tx yet: Phase 1 proves them, Phase 1R wires them into handlers. The
// migration is 0016_consistency.sql.
var _ consistency.Journal = (*PG)(nil)
var _ consistency.RevisionStore = (*PG)(nil)
var _ consistency.VisibilityReader = (*PG)(nil)

func (p *PG) Create(ctx context.Context, op consistency.Operation) (consistency.Operation, error) {
	if op.ID == uuid.Nil {
		return consistency.Operation{}, fmt.Errorf("consistency: operation id is required")
	}
	if op.State == "" {
		op.State = consistency.StateIntent
	}
	if op.State != consistency.StateIntent {
		return consistency.Operation{}, fmt.Errorf("%w: a new operation must start in INTENT", consistency.ErrInvalidTransition)
	}
	var out consistency.Operation
	err := p.pool.QueryRow(ctx, `
		INSERT INTO operations (id, owner_id, kind, state)
		VALUES ($1, $2, $3, $4)
		RETURNING id, owner_id, kind, state, staged_root, created_at, updated_at`,
		op.ID, op.OwnerID, op.Kind, string(op.State),
	).Scan(&out.ID, &out.OwnerID, &out.Kind, &out.State, &out.StagedRoot, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return consistency.Operation{}, consistency.ErrOperationExists
		}
		return consistency.Operation{}, fmt.Errorf("store: create operation: %w", err)
	}
	return out, nil
}

func (p *PG) Get(ctx context.Context, id uuid.UUID) (consistency.Operation, error) {
	var out consistency.Operation
	err := p.pool.QueryRow(ctx, `
		SELECT id, owner_id, kind, state, staged_root, created_at, updated_at
		FROM operations WHERE id = $1`, id,
	).Scan(&out.ID, &out.OwnerID, &out.Kind, &out.State, &out.StagedRoot, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return consistency.Operation{}, consistency.ErrOperationNotFound
	}
	if err != nil {
		return consistency.Operation{}, fmt.Errorf("store: get operation: %w", err)
	}
	return out, nil
}

func (p *PG) Transition(ctx context.Context, id uuid.UUID, from, to consistency.State) (consistency.Operation, error) {
	if !consistency.ValidTransition(from, to) {
		return consistency.Operation{}, fmt.Errorf("%w: %s -> %s", consistency.ErrInvalidTransition, from, to)
	}
	var out consistency.Operation
	err := p.pool.QueryRow(ctx, `
		UPDATE operations SET state = $3, updated_at = now()
		WHERE id = $1 AND state = $2
		RETURNING id, owner_id, kind, state, staged_root, created_at, updated_at`,
		id, string(from), string(to),
	).Scan(&out.ID, &out.OwnerID, &out.Kind, &out.State, &out.StagedRoot, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "no such operation" from "the state moved under us".
		var exists bool
		if qerr := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operations WHERE id = $1)`, id).Scan(&exists); qerr != nil {
			return consistency.Operation{}, fmt.Errorf("store: check operation: %w", qerr)
		}
		if !exists {
			return consistency.Operation{}, consistency.ErrOperationNotFound
		}
		return consistency.Operation{}, consistency.ErrStateConflict
	}
	if err != nil {
		return consistency.Operation{}, fmt.Errorf("store: transition operation: %w", err)
	}
	return out, nil
}

// CompleteOperation moves PUBLISHING -> COMMITTED in the caller's transaction.
func (t *pgTx) CompleteOperation(ctx context.Context, opID, ownerID uuid.UUID, kind string) error {
	if opID == uuid.Nil {
		return consistency.ErrPublishOperation
	}
	if kind == "" {
		return consistency.ErrPublishOperation
	}
	tag, err := t.tx.Exec(ctx,
		`UPDATE operations SET state = 'COMMITTED', updated_at = now()
		 WHERE id = $1 AND state = 'PUBLISHING' AND owner_id = $2 AND kind = $3`,
		opID, ownerID, kind)
	if err != nil {
		return fmt.Errorf("store: complete operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return consistency.ErrStateConflict
	}
	return nil
}

func (p *PG) Unresolved(ctx context.Context, limit int) ([]consistency.Operation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, owner_id, kind, state, staged_root, created_at, updated_at
		FROM operations
		WHERE state = 'NEEDS_OPERATOR'
		ORDER BY updated_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list unresolved operations: %w", err)
	}
	defer rows.Close()
	var out []consistency.Operation
	for rows.Next() {
		var op consistency.Operation
		if err := rows.Scan(&op.ID, &op.OwnerID, &op.Kind, &op.State, &op.StagedRoot, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (p *PG) NonTerminal(ctx context.Context, limit int) ([]consistency.Operation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, owner_id, kind, state, staged_root, created_at, updated_at
		FROM operations
		WHERE state IN ('INTENT','STAGING','PUBLISHING')
		ORDER BY updated_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list non-terminal operations: %w", err)
	}
	defer rows.Close()
	var out []consistency.Operation
	for rows.Next() {
		var op consistency.Operation
		if err := rows.Scan(&op.ID, &op.OwnerID, &op.Kind, &op.State, &op.StagedRoot, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (p *PG) Current(ctx context.Context, nodeID uuid.UUID) (consistency.Revision, error) {
	var rev int64
	err := p.pool.QueryRow(ctx, `SELECT revision FROM nodes WHERE id = $1`, nodeID).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, consistency.ErrRevisionConflict
	}
	if err != nil {
		return 0, fmt.Errorf("store: read revision: %w", err)
	}
	return consistency.Revision(rev), nil
}

func (p *PG) CAS(ctx context.Context, nodeID uuid.UUID, expected, next consistency.Revision) error {
	if next != expected+1 {
		return fmt.Errorf("consistency: revision must advance by one: %d -> %d", expected, next)
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE nodes SET revision = $3, modified_at = now() WHERE id = $1 AND revision = $2`,
		nodeID, int64(expected), int64(next))
	if err != nil {
		return fmt.Errorf("store: cas revision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return consistency.ErrRevisionConflict
	}
	return nil
}

func (p *PG) VisibilityNode(ctx context.Context, id uuid.UUID) (consistency.VisibilityNode, bool, error) {
	var (
		out      consistency.VisibilityNode
		parent   *uuid.UUID
		trashed  *time.Time
		stagedBy *uuid.UUID
	)
	err := p.pool.QueryRow(ctx,
		`SELECT id, parent_id, trashed_at, staged_by FROM nodes WHERE id = $1`, id,
	).Scan(&out.ID, &parent, &trashed, &stagedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return consistency.VisibilityNode{}, false, nil
	}
	if err != nil {
		return consistency.VisibilityNode{}, false, fmt.Errorf("store: read visibility: %w", err)
	}
	out.ParentID = parent
	out.Trashed = trashed != nil
	out.StagedBy = stagedBy
	return out, true, nil
}

// SetStagedBy records operation ownership. It is a separate transaction for the
// Phase 1 contract; publishing will clear it in the same transaction that makes
// the subtree visible.
func (p *PG) SetStagedBy(ctx context.Context, nodeID uuid.UUID, opID *uuid.UUID) error {
	tag, err := p.pool.Exec(ctx, `UPDATE nodes SET staged_by = $2 WHERE id = $1`, nodeID, opID)
	if err != nil {
		return fmt.Errorf("store: set staged_by: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CheckEpochInTx verifies the process's lease epoch under a row lock in the
// same transaction that will publish metadata. A stale epoch aborts before the
// visibility change.
func (p *PG) CheckEpochInTx(ctx context.Context, tx pgx.Tx, expected uint64) error {
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT epoch FROM consistency_lease WHERE id = 1 FOR UPDATE`).Scan(&epoch); err != nil {
		return fmt.Errorf("store: read lease epoch: %w", err)
	}
	if uint64(epoch) != expected {
		return fmt.Errorf("%w: current %d, expected %d", consistency.ErrLeaseStale, epoch, expected)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
