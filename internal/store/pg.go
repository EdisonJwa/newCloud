package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG is the PostgreSQL-backed metadata store.
//
// Every mutation runs in a transaction together with its audit entry and any
// queued job, which is what makes "no operation without a record" true rather
// than aspirational. Two consequences shape the code below:
//
//   - Audit rows are appended through a dedicated writer (see internal/auditdb)
//     that computes the hash chain inside the same transaction. A caller cannot
//     supply prev_hash, so a forged predecessor is not expressible.
//   - Errors are translated into the package sentinels at the boundary, so
//     handlers never have to know about pgx error codes.
type PG struct {
	pool *pgxpool.Pool
	// auditWriter is optional at construction time so the store can be used
	// before the audit package is wired. When nil, Audit() returns an error
	// instead of silently dropping the record.
	auditWriter AuditWriter
	now         func() time.Time
	resolver    Resolver
}

// AuditWriter appends an audit entry within the caller's transaction, computing
// the hash chain.
type AuditWriter interface {
	Append(ctx context.Context, tx Tx, e AuditEntry) error
}

var _ Store = (*PG)(nil)

// NewPG wraps a connection pool.
func NewPG(pool *pgxpool.Pool) *PG {
	pg := &PG{pool: pool, now: time.Now}
	pg.resolver = &pgResolver{s: pg}
	return pg
}

// SetAuditWriter wires the hash-chaining writer. It is separate from NewPG so a
// misconfiguration is visible: an unwired writer fails loudly on first use.
func (s *PG) SetAuditWriter(w AuditWriter) { s.auditWriter = w }

// Pool exposes the pool for maintenance tasks (partitions, sweeps).
func (s *PG) Pool() *pgxpool.Pool { return s.pool }

// WithTx runs fn in a transaction.
//
// Isolation is the PostgreSQL default (read committed). That is sufficient
// because every invariant that matters is protected by a constraint — the
// partial unique index on sibling names, the foreign keys, the CHECK on quota —
// rather than by reading and then writing. Code that needs a stronger guarantee
// asks for it with SELECT ... FOR UPDATE at the specific row.
func (s *PG) WithTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	wrapped := &pgTx{tx: tx, s: s, now: s.now()}
	if err := fn(ctx, wrapped); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", translate(err))
	}
	return nil
}

type pgTx struct {
	tx  pgx.Tx
	s   *PG
	now time.Time
}

var _ Tx = (*pgTx)(nil)

// translate maps PostgreSQL error codes onto the package sentinels.
//
// Keeping this in one place is why handlers can map errors to HTTP status codes
// without importing pgx.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrConflict, pgErr.ConstraintName)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrNotFound, pgErr.ConstraintName)
		case "23514": // check_violation
			if strings.Contains(pgErr.ConstraintName, "name") {
				return fmt.Errorf("%w: %s", ErrInvalidName, pgErr.ConstraintName)
			}
			return fmt.Errorf("%w: %s", ErrInvalidName, pgErr.ConstraintName)
		case "22001": // string_data_right_truncation
			return fmt.Errorf("%w: value too long", ErrInvalidName)
		}
	}
	return err
}

const nodeColumns = `id, parent_id, owner_id, owner_group_id, name, kind, size,
	mime, etag, blob_id, created_at, modified_at, trashed_at, retain_until`

// nodeColumnsQualified is nodeColumns with every name prefixed by alias.
//
// Needed in any query that joins: the recursive subtree walk joins `nodes n` to a CTE that
// also has an `id`, and unqualified columns there fail with "column reference id is ambiguous".
// The alternative — writing the list out per query — is how the two drift apart.
func nodeColumnsQualified(alias string) string {
	parts := strings.Split(nodeColumns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func scanNode(row pgx.Row) (Node, error) {
	var n Node
	var kind string
	err := row.Scan(&n.ID, &n.ParentID, &n.OwnerID, new(*uuid.UUID), &n.Name, &kind,
		&n.Size, &n.Mime, &n.ETag, &n.BlobID, &n.CreatedAt, &n.ModifiedAt, &n.TrashedAt,
		&n.RetainUntil)
	if err != nil {
		return Node{}, translate(err)
	}
	n.Kind = Kind(kind)
	return n, nil
}

func (t *pgTx) Node(ctx context.Context, id uuid.UUID) (Node, error) {
	const q = `SELECT ` + nodeColumns + ` FROM nodes WHERE id = $1 AND trashed_at IS NULL`
	n, err := scanNode(t.tx.QueryRow(ctx, q, id))
	if err != nil {
		return Node{}, err
	}
	return n, nil
}

// NodeIncludingTrashed is needed by restore and by the trash listing. It is
// separate from Node() so the common path cannot accidentally return a deleted
// node.
func (t *pgTx) NodeIncludingTrashed(ctx context.Context, id uuid.UUID) (Node, error) {
	const q = `SELECT ` + nodeColumns + ` FROM nodes WHERE id = $1`
	return scanNode(t.tx.QueryRow(ctx, q, id))
}

func (t *pgTx) Children(ctx context.Context, parentID uuid.UUID, limit int) ([]Node, error) {
	// A limit of 0 means "no limit"; the caller passes the configured cap to
	// protect against a directory with a million entries.
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE parent_id = $1 AND trashed_at IS NULL
		ORDER BY name
		LIMIT $2`
	rows, err := t.tx.Query(ctx, q, parentID, limitArg(limit))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, translate(rows.Err())
}

// ChildrenAfter implements keyset pagination.
//
// The comparison is strictly greater-than on the same key the sort uses, so a
// concurrent insert behind the cursor cannot make the next page start earlier, and
// a concurrent delete ahead of it cannot skip a row. An OFFSET would do both.
func (t *pgTx) ChildrenAfter(ctx context.Context, parentID uuid.UUID, after string, limit int) ([]Node, error) {
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE parent_id = $1 AND trashed_at IS NULL AND name > $2
		ORDER BY name
		LIMIT $3`
	rows, err := t.tx.Query(ctx, q, parentID, after, limitArg(limit))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) ChildByName(ctx context.Context, parentID uuid.UUID, name string) (Node, error) {
	// Lookup compares name_key, not name (T-12): case and Unicode normalisation are handled by
	// the same Go function the memory backend uses, so the two cannot drift. Comparing the raw
	// name here would make "README.md" miss a node created as "Readme.md".
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE parent_id = $1 AND name_key = $2 AND trashed_at IS NULL`
	return scanNode(t.tx.QueryRow(ctx, q, parentID, nameKey(name)))
}

func (t *pgTx) CreateDir(ctx context.Context, n NewNode) (Node, error) {
	// Validate in Go as well as in SQL. The database CHECK is the last line of
	// defence, but it can only express what fits in an SQL expression; the shared
	// validator is the single authority on what a legal name is, and both storage
	// layouts use it too. Without this call the two implementations would accept
	// different names, which is exactly the divergence the conformance suite is
	// meant to prevent.
	if err := validateName(n.Name); err != nil {
		return Node{}, err
	}
	const q = `INSERT INTO nodes (parent_id, owner_id, owner_group_id, name, name_key, kind, size, mime, etag)
		VALUES ($1, $2, $3, $4, $5, 'dir', 0, '', '')
		RETURNING ` + nodeColumns
	return scanNode(t.tx.QueryRow(ctx, q, n.ParentID, n.OwnerID, n.OwnerGroupID, n.Name,
		nameKey(n.Name)))
}

func (t *pgTx) CreateFile(ctx context.Context, n NewFile) (Node, error) {
	if err := validateName(n.Name); err != nil {
		return Node{}, err
	}
	// Quota is enforced in SQL, not by reading then comparing: a read-then-write
	// check races with a concurrent upload and lets two writers each pass at
	// 90% usage.
	//
	// used_bytes is maintained incrementally because COUNT(*)/SUM() over the node
	// table is not viable once it holds millions of rows.
	if err := t.reserveQuota(ctx, n.OwnerID, n.Size); err != nil {
		return Node{}, err
	}

	// Reference the blob row for referential integrity and refcount.
	const q = `INSERT INTO nodes (parent_id, owner_id, owner_group_id, name, name_key, kind, size, mime, etag, blob_id, retain_until)
		VALUES ($1, $2, $3, $4, $5, 'file', $6, $7, $8, $9, $10)
		RETURNING ` + nodeColumns
	node, err := scanNode(t.tx.QueryRow(ctx, q,
		n.ParentID, n.OwnerID, n.OwnerGroupID, n.Name, nameKey(n.Name),
		n.Size, n.Mime, n.ETag, n.BlobID, n.RetainUntil))
	if err != nil {
		return Node{}, err
	}
	blobID := n.BlobID
	if err := t.adjustBlobRef(ctx, &blobID, +1); err != nil {
		return Node{}, err
	}
	return node, nil
}

func (t *pgTx) UpdateFile(ctx context.Context, u UpdateFile) (Node, error) {
	current, err := t.Node(ctx, u.NodeID)
	if err != nil {
		return Node{}, err
	}
	if current.Kind != KindFile {
		return Node{}, fmt.Errorf("%w: not a file", ErrInvalidName)
	}
	// The precondition is evaluated against the row as locked by this
	// transaction. Combined with the row lock taken below, that makes the check
	// and the write one step: a competing writer cannot slip between them.
	if err := t.lockNode(ctx, u.NodeID); err != nil {
		return Node{}, err
	}
	if !u.Expect.Matches(current.ETag, true) {
		return Node{}, ErrPrecondition
	}

	// Quota accounting is on the delta. Re-uploading the same file must not
	// consume quota twice, and that is only true if we subtract the old size.
	delta := u.Size - current.Size
	if delta > 0 {
		if err := t.reserveQuota(ctx, current.OwnerID, delta); err != nil {
			return Node{}, err
		}
	} else if err := t.releaseQuota(ctx, current.OwnerID, -delta); err != nil {
		return Node{}, err
	}

	if u.KeepVersion && current.BlobID != nil && *current.BlobID != u.BlobID {
		if err := t.pushVersion(ctx, current); err != nil {
			return Node{}, err
		}
		if u.MaxVersions > 0 {
			if err := t.trimVersions(ctx, current.ID, u.MaxVersions); err != nil {
				return Node{}, err
			}
		}
	}

	if current.BlobID != nil && *current.BlobID != u.BlobID {
		if err := t.adjustBlobRef(ctx, current.BlobID, -1); err != nil {
			return Node{}, err
		}
	}

	// The ETag predicate is repeated in the UPDATE even though the row is
	// locked. Two lines of redundancy buy the guarantee that survives a future
	// refactor which drops the lock: the write itself refuses to clobber a
	// different version.
	const q = `UPDATE nodes SET size = $2, mime = $3, etag = $4, blob_id = $5, modified_at = now()
		WHERE id = $1 AND trashed_at IS NULL AND etag = $6
		RETURNING ` + nodeColumns
	node, err := scanNode(t.tx.QueryRow(ctx, q, u.NodeID, u.Size, u.Mime, u.ETag, u.BlobID, current.ETag))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The row was there a moment ago and the predicate matched, so a
			// miss here means somebody else committed first.
			return Node{}, ErrPrecondition
		}
		return Node{}, err
	}
	newBlob := u.BlobID
	if err := t.adjustBlobRef(ctx, &newBlob, +1); err != nil {
		return Node{}, err
	}
	return node, nil
}

// lockNode takes a row lock on a node for the rest of the transaction.
//
// This is what removes the time-of-check/time-of-use window in conditional
// writes: without it, two transactions can both read the same ETag before either
// writes, and both conclude they are allowed to proceed.
func (t *pgTx) lockNode(ctx context.Context, id uuid.UUID) error {
	const q = `SELECT 1 FROM nodes WHERE id = $1 AND trashed_at IS NULL FOR UPDATE`
	var one int
	if err := t.tx.QueryRow(ctx, q, id).Scan(&one); err != nil {
		return translate(err)
	}
	return nil
}

// WriteFile implements the shared upsert-with-precondition path.
func (t *pgTx) WriteFile(ctx context.Context, w WriteFile) (Node, bool, error) {
	// The parent row is locked first. Two concurrent creates of the same name in
	// the same directory then serialise here instead of racing into the unique
	// index, which turns a retryable conflict into a hard error for one client.
	if err := t.lockNode(ctx, w.ParentID); err != nil {
		return Node{}, false, err
	}

	existing, err := t.ChildByName(ctx, w.ParentID, w.Name)
	switch {
	case err == nil:
		if !w.Expect.Matches(existing.ETag, true) {
			return Node{}, false, ErrPrecondition
		}
		n, uerr := t.UpdateFile(ctx, UpdateFile{
			NodeID: existing.ID, Size: w.Size, Mime: w.Mime, ETag: w.ETag,
			BlobID: w.BlobID, BlobHash: w.BlobHash,
			KeepVersion: w.KeepVersion, MaxVersions: w.MaxVersions,
		})
		if uerr != nil {
			return Node{}, false, uerr
		}
		return n, false, nil
	case errors.Is(err, ErrNotFound):
		if !w.Expect.Matches("", false) {
			return Node{}, false, ErrPrecondition
		}
		n, cerr := t.CreateFile(ctx, w.NewFile)
		if cerr != nil {
			return Node{}, false, cerr
		}
		return n, true, nil
	default:
		return Node{}, false, err
	}
}

func (t *pgTx) pushVersion(ctx context.Context, cur Node) error {
	// version_no is allocated from the current maximum inside the transaction so
	// two concurrent saves cannot both claim the same number (the unique
	// constraint would reject the second one anyway, but failing fast here gives
	// a clearer error).
	const q = `INSERT INTO versions (node_id, version_no, blob_id, size, created_by)
		SELECT $1,
		       COALESCE((SELECT max(version_no) FROM versions WHERE node_id = $1), 0) + 1,
		       $2, $3, $4`
	if _, err := t.tx.Exec(ctx, q, cur.ID, cur.BlobID, cur.Size, (*uuid.UUID)(nil)); err != nil {
		return translate(err)
	}
	// The new version row is a reference to the bytes. Without this increment the
	// blob's refcount falls to zero as soon as the live file is replaced, and the
	// sweep deletes content that history still needs — restoring an old version
	// then fails with a missing blob, months later, with no obvious cause.
	return t.adjustBlobRef(ctx, cur.BlobID, +1)
}

func (t *pgTx) trimVersions(ctx context.Context, nodeID uuid.UUID, keep int) error {
	// The blobs of the versions being dropped must lose their reference before the
	// rows disappear, or their refcount stays permanently high and the bytes are
	// never collected.
	const refs = `SELECT blob_id FROM versions
		WHERE node_id = $1
		  AND version_no <= (
		    SELECT COALESCE(max(version_no), 0) - $2 FROM versions WHERE node_id = $1
		  )`
	rows, err := t.tx.Query(ctx, refs, nodeID, keep)
	if err != nil {
		return translate(err)
	}
	var dropped []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return translate(err)
		}
		dropped = append(dropped, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return translate(err)
	}

	const q = `DELETE FROM versions
		WHERE node_id = $1
		  AND version_no <= (
		    SELECT COALESCE(max(version_no), 0) - $2 FROM versions WHERE node_id = $1
		  )`
	if _, err := t.tx.Exec(ctx, q, nodeID, keep); err != nil {
		return translate(err)
	}
	for i := range dropped {
		if err := t.adjustBlobRef(ctx, &dropped[i], -1); err != nil {
			return err
		}
	}
	return nil
}

// reserveQuota adds n bytes to the owner's usage, failing if it would exceed the
// quota. The check and the increment are one statement, so concurrent uploads
// cannot both squeeze under the limit.
func (t *pgTx) reserveQuota(ctx context.Context, ownerID uuid.UUID, n int64) error {
	const q = `UPDATE users
		SET used_bytes = used_bytes + $2, updated_at = now()
		WHERE id = $1
		  AND (quota_bytes <= 0 OR used_bytes + $2 <= quota_bytes)
		RETURNING used_bytes`
	var used int64
	if err := t.tx.QueryRow(ctx, q, ownerID, n).Scan(&used); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the user does not exist or the quota would be exceeded;
			// distinguish them so the caller can return 507 rather than 404.
			var exists bool
			if e := t.tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1`, ownerID).Scan(&exists); e != nil {
				return ErrNotFound
			}
			return ErrQuota
		}
		return translate(err)
	}
	return nil
}

func (t *pgTx) releaseQuota(ctx context.Context, ownerID uuid.UUID, n int64) error {
	if n <= 0 {
		return nil
	}
	// GREATEST(...,0) keeps the counter from going negative if an earlier
	// accounting bug over-released; a negative usage would silently hand out
	// free quota forever.
	const q = `UPDATE users SET used_bytes = GREATEST(used_bytes - $2, 0), updated_at = now()
		WHERE id = $1`
	_, err := t.tx.Exec(ctx, q, ownerID, n)
	return translate(err)
}

func (t *pgTx) adjustBlobRef(ctx context.Context, blobID *uuid.UUID, delta int) error {
	if blobID == nil {
		return nil
	}
	// collect_after is set when the count reaches zero and cleared when it comes
	// back: that is the grace period before the bytes are deleted, which makes an
	// accidental delete recoverable.
	const q = `UPDATE blobs
		SET refcount = GREATEST(refcount + $2, 0),
		    collect_after = CASE
		      WHEN refcount + $2 <= 0 THEN COALESCE(collect_after, now() + interval '7 days')
		      ELSE NULL
		    END
		WHERE id = $1`
	_, err := t.tx.Exec(ctx, q, *blobID, delta)
	return translate(err)
}

func (t *pgTx) Rename(ctx context.Context, id uuid.UUID, name string) (Node, error) {
	// name_key is updated with the name: leaving it stale would make the renamed node
	// unlookupable under its new name and would let a later file take its old key.
	const q = `UPDATE nodes SET name = $2, name_key = $3, modified_at = now()
		WHERE id = $1 AND trashed_at IS NULL AND parent_id IS NOT NULL
		RETURNING ` + nodeColumns
	return scanNode(t.tx.QueryRow(ctx, q, id, name, nameKey(name)))
}

func (t *pgTx) Move(ctx context.Context, id, newParent uuid.UUID) (Node, error) {
	// The cycle check is a recursive CTE: walking up from the new parent, the
	// move is invalid if we meet the node being moved. Doing it in SQL keeps it
	// atomic with the UPDATE; doing it in Go would race with a concurrent move.
	const q = `
		WITH RECURSIVE ancestors AS (
		    SELECT id, parent_id FROM nodes WHERE id = $2
		    UNION ALL
		    SELECT n.id, n.parent_id
		    FROM nodes n JOIN ancestors a ON n.id = a.parent_id
		)
		UPDATE nodes SET parent_id = $2, modified_at = now()
		WHERE id = $1
		  AND trashed_at IS NULL
		  AND parent_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM ancestors WHERE id = $1)
		RETURNING ` + nodeColumns

	node, err := scanNode(t.tx.QueryRow(ctx, q, id, newParent))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Either the node is missing or the move would create a cycle.
			var exists bool
			if e := t.tx.QueryRow(ctx, `SELECT true FROM nodes WHERE id = $1`, id).Scan(&exists); e == nil && exists {
				return Node{}, ErrCycle
			}
		}
		return Node{}, err
	}
	return node, nil
}

func (t *pgTx) Trash(ctx context.Context, id uuid.UUID) error {
	// A single recursive UPDATE marks the whole subtree. Doing it client-side
	// would take one round trip per level and could leave a partially trashed
	// tree if the connection dropped mid-way.
	const q = `
		WITH RECURSIVE subtree AS (
		    SELECT id FROM nodes WHERE id = $1 AND trashed_at IS NULL
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		    WHERE n.trashed_at IS NULL
		)
		UPDATE nodes
		SET trashed_at = now(), trashed_from = parent_id
		WHERE id IN (SELECT id FROM subtree)`
	tag, err := t.tx.Exec(ctx, q, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Trashed lists trashed nodes that are the roots of their deleted subtree.
//
// "Root of a trashed subtree" means the node is trashed but its parent is not — that
// is what distinguishes an item the user deleted from a child that went along with
// it. Listing every trashed row instead would present a deleted folder and all its
// contents as independent, separately-restorable items.
func (t *pgTx) Trashed(ctx context.Context, ownerID uuid.UUID, limit int) ([]Node, error) {
	const q = `SELECT ` + nodeColumns + ` FROM nodes n
		WHERE n.owner_id = $1
		  AND n.trashed_at IS NOT NULL
		  AND (
		        n.parent_id IS NULL
		     OR NOT EXISTS (
		          SELECT 1 FROM nodes p
		          WHERE p.id = n.parent_id AND p.trashed_at IS NOT NULL
		        )
		  )
		ORDER BY n.trashed_at DESC
		LIMIT $2`
	rows, err := t.tx.Query(ctx, q, ownerID, limitArg(limit))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		n, serr := scanNode(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, n)
	}
	return out, translate(rows.Err())
}
func (t *pgTx) Restore(ctx context.Context, id uuid.UUID) (Node, error) {
	// Restoring a directory must restore its subtree, otherwise the directory
	// comes back empty and the children are unreachable forever: they are not in
	// the trash listing of the parent, and their parent is no longer trashed.
	const q = `
		WITH RECURSIVE subtree AS (
		    SELECT id FROM nodes WHERE id = $1 AND trashed_at IS NOT NULL
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		    WHERE n.trashed_at IS NOT NULL
		)
		UPDATE nodes SET trashed_at = NULL, trashed_from = NULL
		WHERE id IN (SELECT id FROM subtree)`
	if _, err := t.tx.Exec(ctx, q, id); err != nil {
		return Node{}, translate(err)
	}
	return t.Node(ctx, id)
}

func (t *pgTx) Versions(ctx context.Context, nodeID uuid.UUID, limit int) ([]Version, error) {
	const q = `SELECT id, node_id, version_no, blob_id, size, created_at, created_by
		FROM versions WHERE node_id = $1
		ORDER BY version_no DESC
		LIMIT $2`
	rows, err := t.tx.Query(ctx, q, nodeID, limitArg(limit))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.NodeID, &v.VersionNo, &v.BlobID, &v.Size, &v.CreatedAt, &v.CreatedBy); err != nil {
			return nil, translate(err)
		}
		out = append(out, v)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) RestoreVersion(ctx context.Context, nodeID uuid.UUID, versionNo int) (Node, error) {
	const q = `SELECT blob_id, size FROM versions WHERE node_id = $1 AND version_no = $2`
	var blobID uuid.UUID
	var size int64
	if err := t.tx.QueryRow(ctx, q, nodeID, versionNo).Scan(&blobID, &size); err != nil {
		return Node{}, translate(err)
	}

	current, err := t.Node(ctx, nodeID)
	if err != nil {
		return Node{}, err
	}

	// Restoring an old version keeps the current one: otherwise "undo" is
	// destructive and the user cannot change their mind.
	if current.BlobID != nil {
		if err := t.pushVersion(ctx, current); err != nil {
			return Node{}, err
		}
	}

	delta := size - current.Size
	if delta > 0 {
		if err := t.reserveQuota(ctx, current.OwnerID, delta); err != nil {
			return Node{}, err
		}
	} else if err := t.releaseQuota(ctx, current.OwnerID, -delta); err != nil {
		return Node{}, err
	}

	// The ETag must be derived from the content address, exactly like every
	// other write path. This used to be an unquoted UUID prefix, which meant a
	// conditional write after a restore could never match again: every WebDAV
	// client holding the ETag would get 412 forever.
	etag := etagForBlob(blobID.String())

	const upd = `UPDATE nodes SET blob_id = $2, size = $3, etag = $4, modified_at = now()
		WHERE id = $1 AND trashed_at IS NULL
		RETURNING ` + nodeColumns
	node, err := scanNode(t.tx.QueryRow(ctx, upd, nodeID, blobID, size, etag))
	if err != nil {
		return Node{}, err
	}
	if err := t.adjustBlobRef(ctx, &blobID, +1); err != nil {
		return Node{}, err
	}
	return node, nil
}

func (t *pgTx) Audit(ctx context.Context, e AuditEntry) error {
	// Fail-closed (T-16). An operation that cannot be recorded must not be committed, or the
	// audit log becomes a partial fiction — a platform whose tamper-evident log silently stops
	// recording under disk pressure is worse than one that refuses the write, because the
	// cheapest attack becomes "fill the disk".
	//
	// The policy and its cost (an audit outage means no writes) are documented for operators in
	// deploy/audit-failure-policy.md.
	if t.s.auditWriter == nil {
		return fmt.Errorf("%w: audit writer not configured; refusing to commit an unaudited operation",
			ErrAuditUnavailable)
	}
	if err := t.s.auditWriter.Append(ctx, t, e); err != nil {
		// Wrapped so the HTTP layer can answer with a diagnosable status instead of a generic
		// 500: an operator seeing "audit storage unavailable" knows to check the audit table and
		// the disk, which "something went wrong" does not tell them.
		return fmt.Errorf("%w: %w", ErrAuditUnavailable, err)
	}
	return nil
}

func (t *pgTx) Enqueue(ctx context.Context, j Job) error {
	// A job with no explicit schedule is due *now*, and "now" must be the database's clock.
	//
	// Using the application clock here made the queue intermittently unclaimable: a job
	// stamped with a Go time a few milliseconds ahead of the database's `now()` is not
	// `run_after <= now()` when the worker asks, so it sits unclaimed until the clocks
	// drift past each other. The conformance suite caught it only on PostgreSQL, only
	// sometimes, and only after the machine had been up long enough for the two clocks to
	// diverge - which is the worst kind of flake to debug.
	//
	// A caller-supplied RunAfter is still honoured as-is (it is a deliberate future
	// schedule), but the default comes from the database.
	runAfter := j.RunAfter
	if runAfter.IsZero() {
		runAfter = time.Time{} // sentinel: let the SQL use now()
	}
	// A nil payload would violate the column's NOT NULL constraint. A job with no
	// arguments is legitimate, so it is stored as an empty object rather than
	// rejected — the alternative is every producer remembering to pass one.
	payload := j.Payload
	if payload == nil {
		payload = map[string]any{}
	}

	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	const q = `INSERT INTO jobs (kind, payload, dedupe_key, run_after, max_attempts)
		VALUES ($1, $2, $3, COALESCE($4, now()), $5)
		ON CONFLICT (kind, dedupe_key) WHERE completed_at IS NULL AND dedupe_key IS NOT NULL
		DO NOTHING`

	// A nil timestamp lets COALESCE pick now(); passing time.Time{} would write year 1.
	var runAfterArg any
	if !runAfter.IsZero() {
		runAfterArg = runAfter
	}
	_, err := t.tx.Exec(ctx, q, j.Kind, payload, nullableString(j.DedupeKey), runAfterArg, maxAttempts)
	return translate(err)
}

// Exec runs a statement in this transaction.
//
// It is unexported-by-convention: only the store's own audit writer should use
// it. Exposing raw SQL to handlers would let them bypass the invariants the rest
// of this file maintains.
func (t *pgTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

// QueryRow runs a single-row query in this transaction.
func (t *pgTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

// limitArg turns "no limit" into SQL NULL.
//
// PostgreSQL rejects a negative LIMIT, so passing -1 (the MySQL idiom) fails at
// runtime rather than meaning "unlimited". NULL is the documented way to express
// it in a parameterised query.
func limitArg(limit int) any {
	if limit <= 0 {
		return nil
	}
	return limit
}

// offsetArg clamps a negative offset to zero.
//
// PostgreSQL accepts OFFSET 0 but not a negative value, and callers compute offsets from
// page numbers, so a bad one should read from the start rather than fail the query.
func offsetArg(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---- Store-level operations ----

func (s *PG) Root(ctx context.Context, ownerID uuid.UUID) (Node, error) {
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE owner_id = $1 AND parent_id IS NULL AND trashed_at IS NULL
		LIMIT 1`
	return scanNode(s.pool.QueryRow(ctx, q, ownerID))
}

// EnsureRoot creates the owner's root directory on first use.
//
// The insert races with itself on concurrent first logins; the partial unique
// index on (owner_id) WHERE parent_id IS NULL is what makes ON CONFLICT usable,
// so both callers converge on one row instead of creating two roots.
func (s *PG) EnsureRoot(ctx context.Context, ownerID uuid.UUID) (Node, error) {
	const q = `INSERT INTO nodes (parent_id, owner_id, name, name_key, kind, size, mime, etag)
		VALUES (NULL, $1, '', '', 'dir', 0, '', '')
		ON CONFLICT (owner_id) WHERE parent_id IS NULL AND trashed_at IS NULL DO NOTHING
		RETURNING ` + nodeColumns

	node, err := scanNode(s.pool.QueryRow(ctx, q, ownerID))
	if err == nil {
		return node, nil
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		// Lost the race or the conflict target did not match: read the winner.
		return s.Root(ctx, ownerID)
	}
	return Node{}, err
}

func (s *PG) Resolve(ctx context.Context, ownerID uuid.UUID, p string) ([]Node, error) {
	segs, err := splitPath(p)
	if err != nil {
		return nil, err
	}

	root, err := s.Root(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	out := []Node{root}
	cur := root

	for _, seg := range segs {
		child, err := s.childByName(ctx, cur.ID, seg)
		if err != nil {
			return nil, err
		}
		out = append(out, child)
		cur = child
	}
	return out, nil
}

func (s *PG) childByName(ctx context.Context, parentID uuid.UUID, name string) (Node, error) {
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE parent_id = $1 AND name = $2 AND trashed_at IS NULL`
	return scanNode(s.pool.QueryRow(ctx, q, parentID, name))
}

func (s *PG) Usage(ctx context.Context, ownerID uuid.UUID) (int64, error) {
	var used int64
	err := s.pool.QueryRow(ctx, `SELECT used_bytes FROM users WHERE id = $1`, ownerID).Scan(&used)
	return used, translate(err)
}

func (s *PG) Resolver() Resolver { return s.resolver }

// pgResolver answers the storage layer's questions from the database.
//
// It resolves owner strings to UUIDs at the boundary: the storage interface is
// string-keyed because it must also serve anonymous uploads (file requests),
// which have no user row.
type pgResolver struct{ s *PG }

func (r *pgResolver) HashFor(ctx context.Context, owner, p string) ([32]byte, bool, error) {
	ownerID, err := parseOwner(owner)
	if err != nil {
		return [32]byte{}, false, err
	}
	nodes, err := r.s.Resolve(ctx, ownerID, p)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return [32]byte{}, false, nil
		}
		return [32]byte{}, false, err
	}
	leaf := nodes[len(nodes)-1]
	if leaf.IsDir() || leaf.BlobID == nil {
		return [32]byte{}, false, nil
	}
	const q = `SELECT sha256 FROM blobs WHERE id = $1`
	var hash []byte
	if err := r.s.pool.QueryRow(ctx, q, *leaf.BlobID).Scan(&hash); err != nil {
		return [32]byte{}, false, translate(err)
	}
	var out [32]byte
	copy(out[:], hash)
	return out, true, nil
}

func (r *pgResolver) Record(ctx context.Context, owner, p string, hash [32]byte, isDir bool) error {
	// Node creation is the metadata layer's job; the storage adapter only needs
	// the content mapping, which CreateFile/CreateDir already wrote.
	return nil
}

func (r *pgResolver) Forget(ctx context.Context, owner, p string) error { return nil }

func (r *pgResolver) IsDir(ctx context.Context, owner, p string) (bool, bool, error) {
	ownerID, err := parseOwner(owner)
	if err != nil {
		return false, false, err
	}
	nodes, err := r.s.Resolve(ctx, ownerID, p)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	leaf := nodes[len(nodes)-1]
	return leaf.IsDir(), true, nil
}

func (r *pgResolver) Children(ctx context.Context, owner, dirPath string, limit int) ([]ResolverNode, error) {
	ownerID, err := parseOwner(owner)
	if err != nil {
		return nil, err
	}
	nodes, err := r.s.Resolve(ctx, ownerID, dirPath)
	if err != nil {
		return nil, err
	}
	leaf := nodes[len(nodes)-1]
	if !leaf.IsDir() {
		return nil, ErrNotFound
	}

	const q = `SELECT name, kind, size FROM nodes
		WHERE parent_id = $1 AND trashed_at IS NULL
		ORDER BY name
		LIMIT $2`
	rows, err := r.s.pool.Query(ctx, q, leaf.ID, limitArg(limit))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []ResolverNode
	for rows.Next() {
		var name, kind string
		var size int64
		if err := rows.Scan(&name, &kind, &size); err != nil {
			return nil, translate(err)
		}
		child := ResolverNode{Path: joinPath(dirPath, name), IsDir: kind == "dir"}
		if !child.IsDir {
			child.Size = size
		}
		out = append(out, child)
	}
	return out, translate(rows.Err())
}
