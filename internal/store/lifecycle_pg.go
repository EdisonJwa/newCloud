package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementation of LifecycleTx.

// PurgeTrash permanently deletes trashed subtrees older than retention.
//
// The order is what makes this safe to crash mid-way:
//
//  1. select the eligible roots (trashed, and their own trash timestamp is old
//     enough — a child inherits its root's age, which is why the roots are what
//     get selected);
//  2. release quota for the bytes under each root;
//  3. decrement blob references for the nodes about to disappear;
//  4. delete the subtree in one recursive statement.
//
// Steps 2-4 run in the caller's transaction, so a failure leaves the data intact
// rather than half-accounted. The blob bytes themselves are collected later by the
// existing sweep once nothing references them.
func (t *pgTx) PurgeTrash(ctx context.Context, retention time.Duration) (PurgeResult, error) {
	var res PurgeResult
	if retention <= 0 {
		// Explicitly "never". Not a silent default: retention is a data-loss
		// setting and an omitted value must not mean "delete everything".
		return res, nil
	}

	// Eligible roots: trashed, old enough, and not merely a descendant of another
	// trashed node (purging a root takes its subtree with it, so counting children
	// as separate roots would double-count).
	const rootsQ = `SELECT n.id, n.owner_id, n.name, n.trashed_at
		FROM nodes n
		WHERE n.trashed_at IS NOT NULL
		  AND n.trashed_at <= now() - $1::interval
		  AND (n.parent_id IS NULL OR NOT EXISTS (
		        SELECT 1 FROM nodes p WHERE p.id = n.parent_id AND p.trashed_at IS NOT NULL))
		LIMIT 500`

	rows, err := t.tx.Query(ctx, rootsQ, intervalOf(retention))
	if err != nil {
		return res, translate(err)
	}
	var roots []PurgeableTrashRoot
	for rows.Next() {
		var r PurgeableTrashRoot
		if err := rows.Scan(&r.NodeID, &r.OwnerID, &r.Name, &r.TrashedAt); err != nil {
			rows.Close()
			return res, translate(err)
		}
		roots = append(roots, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, translate(err)
	}
	if len(roots) == 0 {
		return res, nil
	}

	for _, root := range roots {
		subtree, bytes, perr := t.purgeSubtree(ctx, root)
		if perr != nil {
			return res, perr
		}
		res.Nodes += subtree
		res.Bytes += bytes
		res.Roots++
	}
	return res, nil
}

// purgeSubtree accounts for and deletes one trashed subtree.
func (t *pgTx) purgeSubtree(ctx context.Context, root PurgeableTrashRoot) (nodes int, bytes int64, err error) {
	// The subtree's live rows and their content. Sizes come back so quota and
	// refcounts can be adjusted before the rows vanish — after the DELETE there is
	// nothing left to compute them from.
	const collect = `
		WITH RECURSIVE subtree AS (
		    SELECT id, size, blob_id, owner_id FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id, n.size, n.blob_id, n.owner_id
		    FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT id, size, blob_id, owner_id FROM subtree`

	rows, err := t.tx.Query(ctx, collect, root.NodeID)
	if err != nil {
		return 0, 0, translate(err)
	}

	type row struct {
		id      uuid.UUID
		size    int64
		blobID  *uuid.UUID
		ownerID uuid.UUID
	}
	var collected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.size, &r.blobID, &r.ownerID); err != nil {
			rows.Close()
			return 0, 0, translate(err)
		}
		collected = append(collected, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, translate(err)
	}

	for _, r := range collected {
		bytes += r.size
		if r.size > 0 {
			// A purged node is always trashed, and trashing already released its bytes from
			// used_bytes. Releasing from the quota here would release them TWICE and hand the account
			// free storage; the bytes leave the retained counter instead.
			if _, qerr := t.tx.Exec(ctx, `
				UPDATE users SET trash_bytes = GREATEST(trash_bytes - $2, 0), updated_at = now()
				WHERE id = $1`, r.ownerID, r.size); qerr != nil {
				return 0, 0, translate(qerr)
			}
		}
		if r.blobID != nil {
			if berr := t.adjustBlobRef(ctx, r.blobID, -1); berr != nil {
				return 0, 0, berr
			}
		}
	}

	// Versions belong to the node, not to the trash: deleting the node cascades,
	// but the cascade would leave the blobs' refcounts too high forever, so they
	// are released explicitly first.
	const dropVersions = `
		WITH RECURSIVE subtree AS (
		    SELECT id FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT v.blob_id FROM versions v WHERE v.node_id IN (SELECT id FROM subtree)`
	vrows, err := t.tx.Query(ctx, dropVersions, root.NodeID)
	if err != nil {
		return 0, 0, translate(err)
	}
	var versionBlobs []uuid.UUID
	for vrows.Next() {
		var id uuid.UUID
		if err := vrows.Scan(&id); err != nil {
			vrows.Close()
			return 0, 0, translate(err)
		}
		versionBlobs = append(versionBlobs, id)
	}
	vrows.Close()
	if err := vrows.Err(); err != nil {
		return 0, 0, translate(err)
	}
	for i := range versionBlobs {
		if berr := t.adjustBlobRef(ctx, &versionBlobs[i], -1); berr != nil {
			return 0, 0, berr
		}
	}

	// One statement for the whole subtree: a per-level delete could stop half-way
	// and leave a directory whose parent is gone.
	const del = `
		WITH RECURSIVE subtree AS (
		    SELECT id FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		DELETE FROM nodes WHERE id IN (SELECT id FROM subtree)`
	tag, err := t.tx.Exec(ctx, del, root.NodeID)
	if err != nil {
		return 0, 0, translate(err)
	}
	return int(tag.RowsAffected()), bytes, nil
}

// ExpireVersions deletes version rows past the retention window.
func (t *pgTx) ExpireVersions(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}

	const find = `SELECT id, blob_id FROM versions
		WHERE created_at <= now() - $1::interval LIMIT 2000`
	rows, err := t.tx.Query(ctx, find, intervalOf(retention))
	if err != nil {
		return 0, translate(err)
	}
	type v struct {
		id     uuid.UUID
		blobID uuid.UUID
	}
	var victims []v
	for rows.Next() {
		var got v
		if err := rows.Scan(&got.id, &got.blobID); err != nil {
			rows.Close()
			return 0, translate(err)
		}
		victims = append(victims, got)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, translate(err)
	}

	for _, victim := range victims {
		// The rows go first, then the reference is dropped, so a crash in between
		// leaves a refcount that is too high (bytes retained, harmless) rather than
		// too low (bytes collected while a row still points at them, fatal).
		if _, derr := t.tx.Exec(ctx, `DELETE FROM versions WHERE id = $1`, victim.id); derr != nil {
			return 0, translate(derr)
		}
		blobID := victim.blobID
		if berr := t.adjustBlobRef(ctx, &blobID, -1); berr != nil {
			return 0, berr
		}
	}
	return len(victims), nil
}

var _ LifecycleTx = (*pgTx)(nil)
var _ = errors.Is
var _ = pgx.ErrNoRows

// ExpireRetained moves nodes whose retention has lapsed into the trash.
//
// Deliberately into the trash rather than out of the database: a share's retention is a policy
// ("do not keep deliveries past 30 days"), not an instruction to destroy data irrecoverably. The
// trash window still applies, so an owner who finds a delivery gone can recover it, and the normal
// purge removes it afterwards. Deleting directly would make a misconfigured retention_seconds
// unrecoverable.
//
// A node is eligible when retain_until is in the past and it is not already trashed. The whole
// subtree is not taken: retention is stamped per delivered file, and a delivered directory is not
// a case this schema creates.
func (t *pgTx) ExpireRetained(ctx context.Context) (int, error) {
	const q = `UPDATE nodes
		SET trashed_at = now(), modified_at = now()
		WHERE retain_until IS NOT NULL
		  AND retain_until <= now()
		  AND trashed_at IS NULL
		RETURNING id`
	rows, err := t.tx.Query(ctx, q)
	if err != nil {
		return 0, translate(err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, translate(err)
	}
	return n, nil
}
