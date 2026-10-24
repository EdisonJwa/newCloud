package store

// PostgreSQL implementation of account deletion.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// DeleteUser applies the ID-08 policy.
//
// Everything happens in one transaction: a partial deletion would leave an account that is
// neither usable nor removable, and the operator would have no way to find out which step
// failed without reading the tables by hand.
func (t *pgTx) DeleteUser(ctx context.Context, id uuid.UUID) (DeleteUserResult, error) {
	var res DeleteUserResult

	var status string
	if err := t.tx.QueryRow(ctx,
		`SELECT status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
		return res, translate(err)
	}
	if err := validateDeletable(status); err != nil {
		return res, err
	}

	// 1. Files enter the trash rather than vanishing.
	//
	// Trashing (instead of deleting rows) keeps the retention window meaningful and keeps the
	// blob references intact, so the collector — not this method — decides when the bytes go.
	// It also means a mistaken deletion is recoverable by an administrator during that window.
	//
	// The WHOLE subtree under each of the account's nodes goes, whatever its owner_id: a
	// group space or an ACL-shared folder holds other users' files, and trashing only the
	// account's own rows used to strand those under a trashed ancestor — unreachable, absent
	// from every trash listing, still charged to their owner, and never reached by the purge
	// (which follows trashed roots). The account's own top-level entries are what seed the
	// walk: a node whose live parent is also the account's is covered by that parent's
	// subtree. Foreign files nested directly under foreign folders are reached when their
	// containing folder belongs to the account being deleted, which is the only way this
	// step can be responsible for them.
	const ownedTops = `
		SELECT id FROM nodes
		WHERE owner_id = $1 AND trashed_at IS NULL AND parent_id IS NOT NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM nodes p
		    WHERE p.id = parent_id AND p.owner_id = $1 AND p.trashed_at IS NULL)`
	rows, err := t.tx.Query(ctx, ownedTops, id)
	if err != nil {
		return res, translate(err)
	}
	var tops []uuid.UUID
	for rows.Next() {
		var top uuid.UUID
		if err := rows.Scan(&top); err != nil {
			rows.Close()
			return res, translate(err)
		}
		tops = append(tops, top)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, translate(err)
	}

	for _, top := range tops {
		// The same accounting Trash does, because this IS a trash: each owner's bytes in the
		// subtree — the account's and anyone else's — move from used_bytes to trash_bytes,
		// or the owner of a stranded file keeps paying for bytes they can no longer reach.
		usage, uerr := t.subtreeUsage(ctx, top, false)
		if uerr != nil {
			// An earlier iteration's subtree walk already took this node; its bytes were
			// accounted there. Any other error is real and must abort the deletion — a
			// partially accounted deletion is exactly what this transaction exists to
			// prevent.
			if !errors.Is(uerr, ErrNotFound) {
				return res, uerr
			}
			continue
		}
		marked, merr := t.markTrashed(ctx, top, nil)
		if merr != nil {
			// Same reason as above: a node listed a moment ago can already be trashed by an
			// earlier iteration that contained it.
			if errors.Is(merr, ErrNotFound) {
				continue
			}
			return res, merr
		}
		res.NodesTrashed += marked
		if err := t.chargeBytesToTrash(ctx, usage); err != nil {
			return res, err
		}
	}

	// 2. Shares the account issued are revoked: a capability must not outlive its issuer.
	stag, err := t.tx.Exec(ctx, `
		UPDATE shares SET revoked_at = now()
		WHERE owner_id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return res, translate(err)
	}
	res.SharesRevoked = int(stag.RowsAffected())

	// 3. Grants naming the account as subject.
	atag, err := t.tx.Exec(ctx, `DELETE FROM acl_entries WHERE user_id = $1`, id)
	if err != nil {
		return res, translate(err)
	}
	res.ACLGrantsRemoved = int(atag.RowsAffected())

	// 4. Grants the account authored on other people's nodes.
	//
	// These are removed because the grantor is gone: a grant whose creator no longer exists
	// cannot be reviewed or revoked by anyone who understands why it was made. Note this is
	// `created_by`, not `user_id` — deleting by subject above already handled the other half.
	btag, err := t.tx.Exec(ctx, `DELETE FROM acl_entries WHERE created_by = $1`, id)
	if err != nil {
		return res, translate(err)
	}
	res.ACLGrantsAuthored = int(btag.RowsAffected())

	// 5. Credentials.
	stok, err := t.tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return res, translate(err)
	}
	res.SessionsRevoked = int(stok.RowsAffected())

	ptok, err := t.tx.Exec(ctx,
		`UPDATE app_passwords SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return res, translate(err)
	}
	res.AppPasswordsRevoked = int(ptok.RowsAffected())

	// 6. WebDAV locks the account held.
	//
	// A lock outliving its holder makes the resource permanently unwritable: nobody else can
	// present the token, and the timeout may be hours away. Releasing them is part of removing
	// the account, not an optional cleanup.
	ltag, err := t.tx.Exec(ctx, `DELETE FROM lock_tokens WHERE user_id = $1`, id)
	if err != nil {
		return res, translate(err)
	}
	res.LocksReleased = int(ltag.RowsAffected())

	// 7. Count the audit history that is deliberately kept.
	//
	// The audit tables have no foreign key to users, so this is a plain count of what remains —
	// reported so the operator can see the policy was honoured rather than assume it.
	if err := t.tx.QueryRow(ctx,
		`SELECT count(*) FROM audit.events WHERE actor_user_id = $1`, id).Scan(&res.AuditRowsKept); err != nil {
		return res, translate(err)
	}

	// 8. Tombstone the account.
	if _, err := t.tx.Exec(ctx, `
		UPDATE users
		SET status = $2, username = $3, email = $4,
		    display_name = 'deleted account', updated_at = now()
		WHERE id = $1`,
		id, StatusDeleted, deletedUsername(id), deletedEmail(id)); err != nil {
		return res, translate(err)
	}

	return res, nil
}

var _ UserDeleteTx = (*pgTx)(nil)
var _ = fmt.Sprintf
