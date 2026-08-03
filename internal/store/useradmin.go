package store

// User administration (ID-04 approval, ID-08 disable/enable).
//
// This exists because the provisioning policy shipped with a dead end: `pending` mode
// creates an account disabled on purpose so an administrator can approve it, and there was
// no operation anywhere — store, API, or CLI — that could move an account out of `pending`.
// A user stuck there could never be admitted, and the only way out was editing the database
// by hand. A policy whose stated remedy does not exist is worse than no policy.
//
// The operations here are deliberately narrow:
//
//   - SetUserStatus is the only way to change status, and it enumerates the legal values
//     rather than accepting a free-form string. "disable this account" and "typo the status
//     column" must not be the same operation.
//   - Status changes that remove access also revoke credentials, in the same transaction.
//     A disabled account whose sessions still work is not disabled; the project learned
//     this the hard way with role downgrades.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Account status values. They are strings in the column, but the set is closed.
const (
	// StatusActive is a normal, usable account.
	StatusActive = "active"
	// StatusPending is a JIT-provisioned account awaiting approval (ID-04).
	StatusPending = "pending"
	// StatusDisabled is an account an administrator has turned off.
	StatusDisabled = "disabled"
)

// validStatus reports whether s is a status this package will store.
func validStatus(s string) bool {
	switch s {
	case StatusActive, StatusPending, StatusDisabled:
		return true
	}
	return false
}

// UserStatusChange describes what a status change did.
type UserStatusChange struct {
	From    string
	To      string
	Changed bool
	// SessionsRevoked and AppPasswordsRevoked are the credentials cut off by a change
	// that removes access. They are reported so the response can say what happened rather
	// than leaving an administrator to assume.
	SessionsRevoked     int
	AppPasswordsRevoked int
}

// revokesAccess reports whether moving to this status must cut off existing credentials.
//
// Anything other than "active" means the account may no longer act, and an account that
// may not act must not keep a working session: the session is the access.
func revokesAccess(status string) bool { return status != StatusActive }

// UserAdminTx is the administrative surface for accounts.
type UserAdminTx interface {
	// ListUsers returns accounts, oldest first, optionally filtered by status.
	//
	// limit bounds the page; an empty status means "any". The filter exists because the
	// only reason to call this is usually "show me the pending ones".
	ListUsers(ctx context.Context, status string, limit int) ([]User, error)
	// UserByID returns one account.
	UserByID(ctx context.Context, id uuid.UUID) (User, error)
	// SetUserStatus moves an account to a new status, revoking credentials when the new
	// status removes access.
	SetUserStatus(ctx context.Context, id uuid.UUID, status string) (UserStatusChange, error)
}

// validateStatusArg normalises and checks a status argument.
//
// An empty status means "no filter", which is why it is not an error here; the caller that
// *sets* a status calls this with a required value and gets the error.
func validateStatusArg(status string, required bool) (string, error) {
	if status == "" {
		if required {
			return "", fmt.Errorf("store: a status is required")
		}
		return "", nil
	}
	if !validStatus(status) {
		return "", fmt.Errorf("store: unknown account status %q (want %q, %q or %q)",
			status, StatusActive, StatusPending, StatusDisabled)
	}
	return status, nil
}

var _ = context.Background
