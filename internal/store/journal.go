package store

// The operation journal (ADR-0013 4.1): durable intent for operations that touch content.
//
// The table is not new - migration 0016 created `operations`, with the state machine below, a
// non-terminal index, and `nodes.staged_by` for the staged subtree. What was missing is the store API
// over it, which is what this file and its two implementations provide. Nothing writes a row yet;
// wiring the delete path and the recovery pass is the next step, and a smaller one now that the
// schema and the transitions exist.
//
// Why a table at all: a process that dies between moving bytes and committing metadata leaves the
// tree in a state nobody planned, and repairing it needs to know what the operation intended. That
// knowledge has to survive the crash, so it cannot be a log line.

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The operation states, mirroring the CHECK in migration 0016. TestOpStatesMatchTheSchemaCheck keeps
// the two from drifting: a state in one and not the other is either dead code or a write the database
// rejects at 3am.
const (
	// OpIntent: the operation has decided what it will do and has not touched content yet.
	OpIntent = "INTENT"
	// OpStaging: bytes are being written into the operation's private namespace.
	OpStaging = "STAGING"
	// OpPublishing: the staged result is being installed at the live path.
	OpPublishing = "PUBLISHING"
	// OpCommitted: the live tree is correct; the operation is done.
	OpCommitted = "COMMITTED"
	// OpAborted: nothing live was changed; the operation is done.
	OpAborted = "ABORTED"
	// OpDiscarded: the operation was abandoned before it touched anything.
	OpDiscarded = "DISCARDED"
	// OpNeedsOperator: recovery could not decide, and guessing would make it worse.
	OpNeedsOperator = "NEEDS_OPERATOR"
)

// OpTerminal reports whether a state is one the recovery pass should leave alone.
//
// NEEDS_OPERATOR is deliberately terminal for this purpose: recovery produces it when it refuses to
// guess, and a pass that picked those rows up again would spin on them forever.
func OpTerminal(state string) bool {
	switch state {
	case OpCommitted, OpAborted, OpDiscarded, OpNeedsOperator:
		return true
	}
	return false
}

// OpStates lists every state the table accepts, in lifecycle order.
func OpStates() []string {
	return []string{OpIntent, OpStaging, OpPublishing, OpCommitted, OpAborted, OpDiscarded, OpNeedsOperator}
}

// Op is one row of the journal.
type Op struct {
	ID      uuid.UUID
	OwnerID uuid.UUID
	Kind    string
	State   string
	// StagedRoot is the subtree root belonging to this operation until it publishes. A staged node
	// is visible only to the operation that owns it; publishing clears the marker.
	StagedRoot *uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// OpJournal is the journal half of the store contract.
//
// AdvanceOp takes the state it expects to be leaving and refuses if the row is not there. Two
// recovery passes may look at the same operation; only one may move it, and "the row was not in the
// state you thought" is the signal the second one needs to stop rather than retry.
type OpJournal interface {
	StartOp(ctx context.Context, op Op) (Op, error)
	AdvanceOp(ctx context.Context, id uuid.UUID, from, to string) error
	OpByID(ctx context.Context, id uuid.UUID) (Op, error)
	OpenOps(ctx context.Context, limit int) ([]Op, error)
}
