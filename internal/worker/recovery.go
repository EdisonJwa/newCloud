package worker

// Recovering open operations (ADR-0013 4.4).
//
// The journal records intent; this is the pass that reads it. It is deliberately conservative: it
// decides only what the node's own state proves, and refuses to guess about content.
//
// Why not repair the content here. A crash during a path-backed delete can leave the bytes at the
// reserved sibling name with the row still live. Putting them back requires the node's path, and the
// journal does not store one: the table's only node reference is staged_root. Reconstructing the path
// would mean walking ancestors, and getting that wrong moves somebody else's bytes to a name they do
// not expect - which is exactly the mistake that was measured and reverted during the delete-ordering
// work. So the pass marks the operation NEEDS_OPERATOR and says which node and which operation, and
// the compensation inside Retire remains the repair for the case it can handle.
//
// What it does settle, it settles by proof:
//   - the node is gone, or its row is trashed: the delete's namespace step committed, so COMMITTED;
//   - the node is live: the operation did not finish, so NEEDS_OPERATOR rather than a guess.
//
// The grace period keeps the pass from stealing work from a request that is still running.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// recoveryGrace is how long an operation may stay open before the pass looks at it. A delete takes
// milliseconds; a minute is long enough that a slow request is not misdiagnosed, and short enough
// that a crash shows up while an operator is still on the same shift.
const recoveryGrace = time.Minute

// RecoverOperations resolves or escalates every open operation older than the grace period.
//
// Returns how many were resolved and how many need an operator, so the duty can log the second number
// — a recovery pass that silently escalates is a pass nobody notices.
func (w *Worker) RecoverOperations(ctx context.Context) (resolved, escalated int, err error) {
	var open []store.Op
	if err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.OpenOps(ctx, 100)
		open = list
		return lerr
	}); err != nil {
		return 0, 0, fmt.Errorf("list open operations: %w", err)
	}

	// Set in New; a test that wants every row eligible sets it to zero.
	grace := w.recoveryGrace
	now := time.Now()
	for _, op := range open {
		if now.Sub(op.CreatedAt) < grace {
			// Still inside the window a live request could be using. Next pass.
			continue
		}

		terminal := ""
		if op.StagedRoot == nil {
			// Nothing to resolve against; the row predates the node reference or was written by a
			// caller that had none. Asking an operator is the only honest answer.
			terminal = store.OpNeedsOperator
		} else {
			var node store.Node
			nerr := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				n, gerr := tx.Node(ctx, *op.StagedRoot)
				node = n
				return gerr
			})
			switch {
			case errors.Is(nerr, store.ErrNotFound):
				// No live row: the delete finished, or the purge already took it. Either way the
				// operation's intent is satisfied.
				terminal = store.OpCommitted
			case nerr != nil:
				return resolved, escalated, fmt.Errorf("load node for operation %s: %w",
					op.ID.String(), nerr)
			case node.TrashedAt != nil:
				// The namespace step committed; whatever the content did after that is the trash
				// sweep's business, not this pass's.
				terminal = store.OpCommitted
			default:
				// The node is live, so the operation did not finish. Its content may or may not have
				// moved, and this pass cannot tell without a path it must not guess at.
				terminal = store.OpNeedsOperator
			}
		}

		if aerr := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.AdvanceOp(ctx, op.ID, op.State, terminal)
		}); aerr != nil {
			if errors.Is(aerr, store.ErrPrecondition) {
				// Somebody else moved it between the read and the write. Not an error: the pass ran
				// twice, or a request finished late. Leave it to whoever owns it now.
				continue
			}
			return resolved, escalated, fmt.Errorf("advance operation %s: %w", op.ID.String(), aerr)
		}

		if terminal == store.OpNeedsOperator {
			escalated++
			w.log.Warn("an operation did not finish and recovery will not guess why",
				"operation", op.ID.String(), "kind", op.Kind,
				"node", op.StagedRoot.String(), "opened", op.CreatedAt.UTC().Format(time.RFC3339))
			continue
		}
		resolved++
		w.log.Info("recovered an unfinished operation",
			"operation", op.ID.String(), "kind", op.Kind, "state", terminal)
	}
	return resolved, escalated, nil
}
