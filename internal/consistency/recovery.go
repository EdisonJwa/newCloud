package consistency

import (
	"context"
	"fmt"
)

// RecoveryAction is what startup recovery did to one operation.
type RecoveryAction string

const (
	RecoveryAborted       RecoveryAction = "ABORTED_INTENT"
	RecoveryDiscarded     RecoveryAction = "DISCARDED_STAGING"
	RecoveryNeedsOperator RecoveryAction = "NEEDS_OPERATOR_PUBLISHING"
)

// RecoveryResult records one recovered operation.
type RecoveryResult struct {
	Operation Operation
	Action    RecoveryAction
}

// Recover handles the safe subset of non-terminal operations after a restart:
//
//   - INTENT never created staging, so it is aborted.
//   - STAGING owned only its private stage, so the stage is discarded and the
//     operation is marked DISCARDED.
//   - PUBLISHING may have changed a live path before the metadata commit. Without
//     the operation's final/backup locations in the journal, reverting it would
//     be a guess; it is marked NEEDS_OPERATOR and stays unavailable until a human
//     or a later journal extension resolves it.
//
// The function does not claim the live namespace is consistent; the caller must
// keep readiness false until it has handled every result.
func Recover(ctx context.Context, j Journal, staging StagingStore, limit int) ([]RecoveryResult, error) {
	ops, err := j.NonTerminal(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RecoveryResult, 0, len(ops))
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var action RecoveryAction
		switch op.State {
		case StateIntent:
			action = RecoveryAborted
			if _, err := j.Transition(ctx, op.ID, StateIntent, StateAborted); err != nil {
				return out, fmt.Errorf("recover %s: abort intent: %w", op.ID, err)
			}
		case StateStaging:
			action = RecoveryDiscarded
			if staging != nil {
				if err := staging.Discard(ctx, op.ID); err != nil {
					return out, fmt.Errorf("recover %s: discard stage: %w", op.ID, err)
				}
			}
			if _, err := j.Transition(ctx, op.ID, StateStaging, StateDiscarded); err != nil {
				return out, fmt.Errorf("recover %s: mark discarded: %w", op.ID, err)
			}
		case StatePublishing:
			action = RecoveryNeedsOperator
			if _, err := j.Transition(ctx, op.ID, StatePublishing, StateNeedsOperator); err != nil {
				return out, fmt.Errorf("recover %s: quarantine publishing: %w", op.ID, err)
			}
		default:
			return out, fmt.Errorf("recover %s: non-terminal state %s has no recovery rule", op.ID, op.State)
		}
		updated, err := j.Get(ctx, op.ID)
		if err != nil {
			return out, err
		}
		out = append(out, RecoveryResult{Operation: updated, Action: action})
	}
	return out, nil
}

// HasUnresolved reports whether any operation remains quarantined. Startup must
// keep readiness false while this is true, even after a restart where there are
// no non-terminal operations left to recover.
func HasUnresolved(ctx context.Context, j Journal, limit int) (bool, error) {
	ops, err := j.Unresolved(ctx, limit)
	if err != nil {
		return false, err
	}
	return len(ops) > 0, nil
}
