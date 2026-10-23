package store

// PostgreSQL implementation of account administration.

import (
	"context"

	"github.com/google/uuid"
)

// ListUsers returns accounts, oldest first.
func (t *pgTx) ListUsers(ctx context.Context, status string, limit int) ([]User, error) {
	st, err := validateStatusArg(status, false)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	// Two statements rather than a dynamic WHERE: the plans differ (the filtered one uses
	// the status index) and a parameterised `($1 = '' OR status = $1)` prevents the planner
	// from using it.
	var (
		rows interface {
			Next() bool
			Scan(...any) error
			Close()
			Err() error
		}
		qerr error
	)
	if st == "" {
		rows, qerr = t.tx.Query(ctx,
			`SELECT `+userColumns+` FROM users ORDER BY created_at, id LIMIT $1`, limit)
	} else {
		rows, qerr = t.tx.Query(ctx,
			`SELECT `+userColumns+` FROM users WHERE status = $1 ORDER BY created_at, id LIMIT $2`,
			st, limit)
	}
	if qerr != nil {
		return nil, translate(qerr)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if serr := rows.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.Role, &u.Status,
			&u.QuotaBytes, &u.UsedBytes, &u.IdPIssuer, &u.IdPSubject,
			&u.IsBreakglass, &u.IdPConfirmedAt, &u.CreatedAt, &u.UpdatedAt); serr != nil {
			return nil, translate(serr)
		}
		out = append(out, u)
	}
	return out, translate(rows.Err())
}

// SetUserStatus changes an account's status and cuts off its credentials when the new
// status removes access.
//
// The row is locked first so two concurrent approvals cannot both see "from pending" and
// each report a change that only one of them made.
func (t *pgTx) SetUserStatus(ctx context.Context, id uuid.UUID, status string) (UserStatusChange, error) {
	st, err := validateStatusArg(status, true)
	if err != nil {
		return UserStatusChange{}, err
	}

	var from string
	if err := t.tx.QueryRow(ctx,
		`SELECT status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&from); err != nil {
		return UserStatusChange{}, translate(err)
	}

	change := UserStatusChange{From: from, To: st}
	if from == st {
		// Idempotent: approving twice is not an error, and reporting "changed" twice
		// would put a misleading row in the audit log.
		return change, nil
	}

	if _, err := t.tx.Exec(ctx, `UPDATE users SET status = $2, updated_at = now() WHERE id = $1`,
		id, st); err != nil {
		return UserStatusChange{}, translate(err)
	}
	change.Changed = true

	if revokesAccess(st) {
		// Same reasoning as role downgrade: the credential is the access. A disabled
		// account that still has a live session has not been disabled.
		tag, serr := t.tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now()
			 WHERE user_id = $1 AND revoked_at IS NULL`, id)
		if serr != nil {
			return change, translate(serr)
		}
		change.SessionsRevoked = int(tag.RowsAffected())

		atag, aerr := t.tx.Exec(ctx,
			`UPDATE app_passwords SET revoked_at = now()
			 WHERE user_id = $1 AND revoked_at IS NULL`, id)
		if aerr != nil {
			return change, translate(aerr)
		}
		change.AppPasswordsRevoked = int(atag.RowsAffected())
	}

	return change, nil
}

var _ UserAdminTx = (*pgTx)(nil)
