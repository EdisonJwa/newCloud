package store

// Account deletion (ID-08).
//
// The requirement asks for a *defined policy*, not merely an endpoint: "账号删除/禁用后，文件归属、
// 共享、审计和保留数据有确定策略". This file is that policy, and the code below implements it.
//
// The policy, decided deliberately:
//
//  1. Files are deleted, not transferred. Handing one user's documents to an administrator on
//     account removal is a silent data-move nobody asked for, and it makes "delete my account"
//     a request an admin can satisfy by reading the files instead. The trash is entered first
//     so a mistaken deletion is recoverable for the retention window.
//  2. Shares the account created are revoked, because a share is a capability the account
//     issued and it must not outlive the account.
//  3. Audit rows are kept. They are not owned by the user, they are the record of what
//     happened; deleting them would let an account erase its own history on the way out.
//     They reference the user by id and email as text/columns without a foreign key, so they
//     survive by construction.
//  4. The account row itself becomes a tombstone rather than disappearing: status 'deleted',
//     username and email replaced with a per-account placeholder.
//
// Why a tombstone rather than a DELETE: `nodes.owner_id`, `shares.owner_id` and
// `versions.created_by` reference users with NO ACTION, and more importantly the audit log
// refers to the account by id. Physically deleting the row would either fail on those
// references or require cascading into the audit trail — which is exactly what the policy says
// must not happen. The tombstone also keeps the identity non-reusable: the old (issuer,
// subject) pair can never be re-provisioned by a stale IdP token, and the email is freed for a
// genuinely new account.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// StatusDeleted marks a tombstoned account.
const StatusDeleted = "deleted"

// DeleteUserResult reports what a deletion did.
//
// Every count is reported rather than logged, so an operator answering "what happened to their
// data" does not have to guess or reconstruct it from the audit log.
type DeleteUserResult struct {
	// NodesTrashed is how many files and folders entered the trash.
	NodesTrashed int
	// SharesRevoked is how many share links the account had issued.
	SharesRevoked int
	// ACLGrantsRemoved is how many grants naming the account as subject.
	ACLGrantsRemoved int
	// ACLGrantsAuthored is how many grants the account had created on other people's nodes.
	// They are removed because the grantor no longer exists.
	ACLGrantsAuthored int
	// SessionsRevoked and AppPasswordsRevoked are the credentials cut off.
	SessionsRevoked     int
	AppPasswordsRevoked int
	// LocksReleased is how many WebDAV locks the account held.
	LocksReleased int
	// AuditRowsKept is the account's audit history, which deliberately survives.
	AuditRowsKept int
}

// UserDeleteTx is the account-removal surface.
type UserDeleteTx interface {
	// DeleteUser applies the deletion policy and returns what it did.
	//
	// The account must already be non-active, or the caller must have decided to skip that
	// step: deletion is irreversible, so the two-step flow (disable, then delete) is the
	// documented path and this method refuses to imply otherwise.
	DeleteUser(ctx context.Context, id uuid.UUID) (DeleteUserResult, error)
}

// deletedUsername returns the placeholder a tombstone carries.
//
// It has to be unique because `users.username` is unique; embedding the id keeps it stable and
// makes the row traceable back to its former identity in the audit log.
func deletedUsername(id uuid.UUID) string {
	return "deleted-" + id.String()
}

// deletedEmail returns the placeholder address, or nil when the account had none.
//
// The real address is released: an organisation that re-invites the same person should be able
// to create a fresh account for them, and holding the old address would block that with a
// unique-constraint violation whose cause is invisible.
func deletedEmail(id uuid.UUID) string {
	return "deleted-" + id.String() + "@invalid"
}

// validateDeletable is the guard shared by both backends.
func validateDeletable(status string) error {
	switch status {
	case StatusDeleted:
		return fmt.Errorf("store: account is already deleted")
	case StatusActive:
		// Deletion is irreversible and the requirement distinguishes disable from delete.
		// Refusing an active account forces the operator through the reversible step first,
		// which is where a mistake gets caught.
		return fmt.Errorf("store: disable the account before deleting it")
	}
	return nil
}

var _ = time.Now
