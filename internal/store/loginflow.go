package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// LoginFlow is a pending Nextcloud login-flow-v2 authorisation (ADR-0015).
//
// The client holds two independent secrets: a POLL token (which it sends to the poll
// endpoint) and a LOGIN token (which it opens in the user's browser). Only their
// SHA-256 hashes are stored, so a metadata dump yields neither. The row starts with no
// user; the browser step attaches the account that approved it, and the first poll after
// that deletes the row and mints the credential it authorised.
//
// The token hashes themselves are not part of this struct: they are the lookup keys and
// have no use once the row is found. That keeps them out of logs, audit metadata and
// anything that serialises a LoginFlow.
type LoginFlow struct {
	ID         uuid.UUID
	UserID     *uuid.UUID
	ClientName string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ApprovedAt *time.Time
}

// Approved reports whether the browser step has completed.
func (f LoginFlow) Approved() bool { return f.ApprovedAt != nil && f.UserID != nil }

// LoginFlowTx is the login-flow surface. It lives on Tx for the same reason the App
// Password methods do: the approval and the credential it authorises must not be able
// to disagree. The approval is a conditional UPDATE, and the poll is a DELETE, so two
// browser tabs cannot both claim a flow and two pollers cannot both receive a credential.
type LoginFlowTx interface {
	// CreateLoginFlow records a freshly started flow. pollHash and loginHash are the
	// SHA-256 digests of the two bearer secrets; the plaintext never reaches the store.
	CreateLoginFlow(ctx context.Context, f LoginFlow, pollHash, loginHash [32]byte) error
	// LoginFlowByLoginHash resolves the browser step's token. Expired, consumed and
	// unknown flows all answer ErrNotFound: the distinction is not the browser's to know.
	LoginFlowByLoginHash(ctx context.Context, loginHash [32]byte) (LoginFlow, error)
	// ApproveLoginFlow attaches userID to the flow. It refuses a flow that is expired,
	// consumed, or already approved by a DIFFERENT account (ErrLoginFlowConflict);
	// re-approving by the same account is idempotent. Returns the updated row.
	ApproveLoginFlow(ctx context.Context, loginHash [32]byte, userID uuid.UUID, at time.Time) (LoginFlow, error)
	// ConsumeLoginFlow DELETEs an approved, unexpired flow and returns it, atomically.
	// A flow that is not approved, has expired, or was already consumed answers
	// ErrNotFound, which is what keeps the poll endpoint single-use.
	ConsumeLoginFlow(ctx context.Context, pollHash [32]byte, at time.Time) (LoginFlow, error)
	// DeleteExpiredLoginFlows removes rows past their window. It is housekeeping, not
	// correctness: every read already ignores an expired row.
	DeleteExpiredLoginFlows(ctx context.Context, now time.Time) (int, error)
}

// ErrLoginFlowConflict means the flow was approved by another account first.
var ErrLoginFlowConflict = errors.New("store: login flow already approved by another account")

var _ LoginFlowTx = (*pgTx)(nil)
var _ LoginFlowTx = (*memTx)(nil)
