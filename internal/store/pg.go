package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	// auditTimeout bounds a single audit append. Zero disables the bound.
	//
	// §10.3 covers "the log database is full" and "events are backing up"; it does not spell out
	// "the log database is slow". Without a bound, a hung audit insert holds the caller's
	// transaction open for as long as the database cares to, so every write on the platform stalls
	// behind the recorder — the one component whose purpose is to observe writes becomes the thing
	// that stops them. A timeout turns that into the already-agreed outcome: the append fails, the
	// mutation rolls back, the client gets 503 audit_unavailable.
	//
	// The policy document listed this as NOT covered. Bounding it is how that gap closes.
	auditTimeout time.Duration
	now          func() time.Time
	resolver     Resolver
	// stallBeforeLockForTest, when set, runs inside UpdateFile and RestoreVersion between the
	// unlocked read and the row lock. Test-only: "a concurrent writer commits in that window" is
	// a race no deterministic test can otherwise hit, and it is exactly the window where a stale
	// snapshot turns into a wrong quota delta or a wrong refusal. Nil in every non-test process.
	stallBeforeLockForTest func(ctx context.Context)
}

// AuditWriter appends an audit entry within the caller's transaction, computing
// the hash chain.
type AuditWriter interface {
	Append(ctx context.Context, tx Tx, e AuditEntry) error
}

var _ Store = (*PG)(nil)

// NewPG wraps a connection pool.
func NewPG(pool *pgxpool.Pool) *PG {
	pg := &PG{pool: pool, now: time.Now, auditTimeout: DefaultAuditAppendTimeout}
	pg.resolver = &pgResolver{s: pg}
	return pg
}

// SetAuditWriter wires the hash-chaining writer. It is separate from NewPG so a
// misconfiguration is visible: an unwired writer fails loudly on first use.
func (s *PG) SetAuditWriter(w AuditWriter) { s.auditWriter = w }

// DefaultAuditAppendTimeout bounds one audit append.
//
// Generous on purpose: the append is a single indexed insert plus a hash computation, so anything
// approaching this is already pathological. The point is not to police normal latency, it is to
// guarantee that "the audit store is wedged" resolves into a refusal instead of an unbounded hang.
const DefaultAuditAppendTimeout = 5 * time.Second

// SetAuditAppendTimeoutForTest overrides the bound so a test does not have to wait for it.
func (s *PG) SetAuditAppendTimeoutForTest(d time.Duration) { s.auditTimeout = d }

// SetPreLockStallForTest installs the callback UpdateFile and RestoreVersion run between their
// unlocked read and their row lock, so a test can commit a concurrent write into exactly that
// window and pin the read-after-lock behaviour. Passing nil removes it.
func (s *PG) SetPreLockStallForTest(fn func(ctx context.Context)) { s.stallBeforeLockForTest = fn }

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
			// A missing PARENT is a 404: the caller named a collection that is gone, and
			// "there is no such directory" is the honest answer.
			//
			// Every other foreign key is a fault, not an absence. A node whose blob_id
			// points at no blobs row means the metadata references content that does not
			// exist — the opposite of "not found" — and answering 404 would send an
			// operator looking at the client while the storage is damaged. The project's
			// own failure-behaviour table promises this: "Blob missing but row present →
			// error, never 404" (2026-10-24 review, M18).
			if strings.Contains(pgErr.ConstraintName, "parent") {
				return fmt.Errorf("%w: %s", ErrNotFound, pgErr.ConstraintName)
			}
			return fmt.Errorf("store: referential integrity violated (%s): %w",
				pgErr.ConstraintName, err)
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
	err := row.Scan(&n.ID, &n.ParentID, &n.OwnerID, &n.OwnerGroupID, &n.Name, &kind,
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
	// The containing directory changed (M1): same transaction as the insert, so a
	// client comparing ETags never sees the child without the parent's new token.
	if err := t.bumpDirAndUp(ctx, n.ParentID); err != nil {
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

	// The containing directory changed (M1), exactly as CreateDir and UpdateFile do for
	// their own mutations. Without it a backend stamps on create-dir but not create-file, and
	// a sync client sees a collection whose children appeared without the collection's ETag
	// moving — the walk-everything case M1 exists to remove. The in-memory backend always
	// stamped here; the divergence was found by TestDirETagPropagation on PostgreSQL
	// (2026-10-27 CI).
	if err := t.bumpDirAndUp(ctx, n.ParentID); err != nil {
		return Node{}, err
	}

	// Reference the blob row for referential integrity and refcount.
	const q = `INSERT INTO nodes (parent_id, owner_id, owner_group_id, name, name_key, kind, size, mime, etag, blob_id, retain_until, modified_at)
		VALUES ($1, $2, $3, $4, $5, 'file', $6, $7, $8, $9, $10, COALESCE($11, now()))
		RETURNING ` + nodeColumns
	node, err := scanNode(t.tx.QueryRow(ctx, q,
		n.ParentID, n.OwnerID, n.OwnerGroupID, n.Name, nameKey(n.Name),
		n.Size, n.Mime, n.ETag, n.BlobID, n.RetainUntil, n.ModifiedAt))
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
	// This first read only decides "exists and is a file". Everything the write
	// depends on is re-read below, under the lock.
	current, err := t.Node(ctx, u.NodeID)
	if err != nil {
		return Node{}, err
	}
	if current.Kind != KindFile {
		return Node{}, fmt.Errorf("%w: not a file", ErrInvalidName)
	}
	// The overwrite changes the containing directory (M1): stamped before the write,
	// in this transaction, so the token and the content land together or not at all.
	if current.ParentID != nil {
		if err := t.bumpDirAndUp(ctx, *current.ParentID); err != nil {
			return Node{}, err
		}
	}
	if t.s.stallBeforeLockForTest != nil {
		t.s.stallBeforeLockForTest(ctx)
	}
	// The precondition is evaluated against the row as locked by this
	// transaction. Combined with the row lock taken below, that makes the check
	// and the write one step: a competing writer cannot slip between them.
	if err := t.lockNode(ctx, u.NodeID); err != nil {
		return Node{}, err
	}
	// Re-read under the lock, and compute everything from THAT row. Between the
	// read above and lockNode a concurrent writer can commit a new size and ETag;
	// the lock makes this transaction wait for that writer, but only a fresh read
	// sees the row it left behind. Expect or the quota delta computed from the
	// stale snapshot would refuse a correctly-preconditioned write (the stale
	// ETag no longer describes the row) or bill the difference between the wrong
	// pair of sizes — while the comment claiming "one step" made it look safe.
	current, err = t.Node(ctx, u.NodeID)
	if err != nil {
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
		if err := t.pushVersion(ctx, current, u.By); err != nil {
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
	const q = `UPDATE nodes SET size = $2, mime = $3, etag = $4, blob_id = $5, modified_at = COALESCE($7, now())
		WHERE id = $1 AND trashed_at IS NULL AND etag = $6
		RETURNING ` + nodeColumns
	node, err := scanNode(t.tx.QueryRow(ctx, q, u.NodeID, u.Size, u.Mime, u.ETag, u.BlobID, current.ETag, u.ModifiedAt))
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
	// A create or an overwrite both change the containing directory (M1). Inside the
	// same transaction as the write, so a failed write rolls the token back with it.
	if err := t.bumpDirAndUp(ctx, w.ParentID); err != nil {
		return Node{}, false, err
	}

	existing, err := t.ChildByName(ctx, w.ParentID, w.Name)
	switch {
	case err == nil:
		// A name that differs from the requested one only by case or Unicode normalisation
		// is a CONFLICT, not the file the caller meant to write.
		//
		// ChildByName compares name_key so a lookup for "README.md" finds a node created as
		// "Readme.md" — that is what makes name lookup case-robust. Reusing that answer for a
		// content write was a silent overwrite of a differently-named file, followed by a 404
		// on the name the client had actually asked for, because Resolve (the read path) is
		// case-exact. ADR-0004 §4 says a second upload under a conflicting name must fail
		// with ErrConflict, and the shared suite only exercised CreateFile, where the unique
		// index already refused it (2026-10-24 review, C6).
		if existing.Name != w.Name {
			return Node{}, false, ErrConflict
		}
		if !w.Expect.Matches(existing.ETag, true) {
			return Node{}, false, ErrPrecondition
		}
		n, uerr := t.UpdateFile(ctx, UpdateFile{
			NodeID: existing.ID, Size: w.Size, Mime: w.Mime, ETag: w.ETag,
			BlobID: w.BlobID, BlobHash: w.BlobHash,
			KeepVersion: w.KeepVersion, MaxVersions: w.MaxVersions,
			By: w.By, ModifiedAt: w.ModifiedAt,
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

func (t *pgTx) pushVersion(ctx context.Context, cur Node, by *uuid.UUID) error {
	// version_no is allocated from the current maximum inside the transaction so
	// two concurrent saves cannot both claim the same number (the unique
	// constraint would reject the second one anyway, but failing fast here gives
	// a clearer error).
	const q = `INSERT INTO versions (node_id, version_no, blob_id, size, created_by)
		SELECT $1,
		       COALESCE((SELECT max(version_no) FROM versions WHERE node_id = $1), 0) + 1,
		       $2, $3, $4`
	if _, err := t.tx.Exec(ctx, q, cur.ID, cur.BlobID, cur.Size, by); err != nil {
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
	node, err := scanNode(t.tx.QueryRow(ctx, q, id, name, nameKey(name)))
	if err != nil {
		return Node{}, err
	}
	// The directory holding the renamed entry changed (M1).
	if node.ParentID != nil {
		if err := t.bumpDirAndUp(ctx, *node.ParentID); err != nil {
			return Node{}, err
		}
	}
	return node, nil
}

func (t *pgTx) Move(ctx context.Context, id, newParent uuid.UUID) (Node, error) {
	// The old parent is captured BEFORE the update (M1): after it, the row only knows
	// the new chain, and a move changes two directories — the one that lost the entry
	// and the one that gained it. A read raced by a concurrent move can under-stamp by
	// one directory; that costs a client one extra subtree walk, never a wrong answer.
	var oldParent *uuid.UUID
	_ = t.tx.QueryRow(ctx,
		`SELECT parent_id FROM nodes WHERE id = $1 AND trashed_at IS NULL`, id).Scan(&oldParent)

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
	// Both chains, stamped now that the move itself committed inside this transaction.
	if oldParent != nil {
		if err := t.bumpDirAndUp(ctx, *oldParent); err != nil {
			return Node{}, err
		}
	}
	if err := t.bumpDirAndUp(ctx, newParent); err != nil {
		return Node{}, err
	}
	return node, nil
}

func (t *pgTx) Trash(ctx context.Context, id uuid.UUID) error {
	usage, err := t.subtreeUsage(ctx, id, false)
	if err != nil {
		return err
	}
	if _, err := t.markTrashed(ctx, id, nil); err != nil {
		return err
	}
	return t.chargeBytesToTrash(ctx, usage)
}

// ownerBytes pairs one account with the bytes an operation moves for it.
//
// A subtree is mixed-owner wherever writes are: group spaces and ACL-shared folders hold files
// whose owner_id is the writer's, not the folder's. Quota lives on the user row, so every byte
// moved between used_bytes and trash_bytes has to be attributed to the owner of the node it came
// from — a single (sum, owner) answer can only ever be right for one of them.
type ownerBytes struct {
	owner uuid.UUID
	bytes int64
}

// subtreeUsage sums the bytes under a node, per owner.
//
// Summed BEFORE the rows are marked trashed: the mark changes what a second query would
// see, and the number has to describe the subtree as it was when the user asked to delete
// it. trashed selects which state the subtree is read in (false = live, true = in the bin),
// which is what makes the same walk serve Trash and Restore.
//
// One row PER OWNER, scanned with a row loop. The original shape aggregated with GROUP BY
// owner_id and read the answer with QueryRow, which consumes exactly one row: in a
// mixed-owner subtree only the first owner's bytes moved between used_bytes and
// trash_bytes, and every other owner's files were neither released on trash nor credited
// back on restore — silently, and in the direction that under-counts usage.
func (t *pgTx) subtreeUsage(ctx context.Context, id uuid.UUID, trashed bool) ([]ownerBytes, error) {
	mark := "IS NULL"
	if trashed {
		mark = "IS NOT NULL"
	}
	q := `
		WITH RECURSIVE subtree AS (
		    SELECT id, owner_id, size FROM nodes WHERE id = $1 AND trashed_at ` + mark + `
		    UNION ALL
		    SELECT n.id, n.owner_id, n.size FROM nodes n JOIN subtree s ON n.parent_id = s.id
		    WHERE n.trashed_at ` + mark + `
		)
		SELECT owner_id, COALESCE(sum(size),0) FROM subtree GROUP BY owner_id`
	rows, err := t.tx.Query(ctx, q, id)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []ownerBytes
	for rows.Next() {
		var u ownerBytes
		if err := rows.Scan(&u.owner, &u.bytes); err != nil {
			return nil, translate(err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	if len(out) == 0 {
		// The root matched nothing: it is gone, or already in the requested state.
		return nil, ErrNotFound
	}
	return out, nil
}

// chargeBytesToTrash moves each owner's bytes from the logical quota to retained usage.
//
// §4.3: trashing moves the bytes between the two counters, and both are clamped so an
// accounting bug cannot produce a negative quota that silently grants storage.
func (t *pgTx) chargeBytesToTrash(ctx context.Context, usage []ownerBytes) error {
	for _, u := range usage {
		if u.bytes <= 0 {
			continue
		}
		if _, err := t.tx.Exec(ctx, `
			UPDATE users
			SET used_bytes = GREATEST(used_bytes - $2, 0),
			    trash_bytes = trash_bytes + $2,
			    updated_at = now()
			WHERE id = $1`, u.owner, u.bytes); err != nil {
			return translate(err)
		}
	}
	return nil
}

// creditBytesFromTrash is the restore-side inverse of chargeBytesToTrash: each owner's
// bytes leave retained usage and come back into the logical quota.
func (t *pgTx) creditBytesFromTrash(ctx context.Context, usage []ownerBytes) error {
	for _, u := range usage {
		if u.bytes <= 0 {
			continue
		}
		if _, err := t.tx.Exec(ctx, `
			UPDATE users
			SET used_bytes = used_bytes + $2,
			    trash_bytes = GREATEST(trash_bytes - $2, 0),
			    updated_at = now()
			WHERE id = $1`, u.owner, u.bytes); err != nil {
			return translate(err)
		}
	}
	return nil
}

// markTrashed sets trashed_at on the whole subtree in one statement.
//
// Doing it client-side would take one round trip per level and could leave a partially
// trashed tree if the connection dropped mid-way.
//
// expect (nil = unconditional) is evaluated INSIDE the statement that trashes. Under READ
// COMMITTED a separate read would let a concurrent writer commit between the read and the
// write, and the writer's row would then be trashed by a request that never saw it.
//
// It returns the number of rows it marked, which is what a caller reporting "how much
// entered the trash" counts instead of guessing from its own bookkeeping.
func (t *pgTx) markTrashed(ctx context.Context, id uuid.UUID, expect *Precondition) (int, error) {
	// The parent chain is captured BEFORE the subtree is marked (M1): after it, the
	// walk would still find the parent (trashing does not reparent), but capturing
	// first states the intent plainly — the chain changed because this entry left it.
	var trashedParent *uuid.UUID
	_ = t.tx.QueryRow(ctx,
		`SELECT parent_id FROM nodes WHERE id = $1 AND trashed_at IS NULL`, id).Scan(&trashedParent)
	const q = `
		WITH RECURSIVE subtree AS (
		    SELECT id FROM nodes WHERE id = $1 AND trashed_at IS NULL
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		    WHERE n.trashed_at IS NULL
		)
		UPDATE nodes
		SET trashed_at = now(), trashed_from = parent_id
		WHERE id IN (SELECT id FROM subtree)
		  -- The live check is repeated in the OUTER statement, and that repetition is the
		  -- difference between "trash once" and "trash once per concurrent caller".
		  --
		  -- Under READ COMMITTED this UPDATE can block on a row lock and then re-evaluate, and
		  -- PostgreSQL's re-check only re-reads predicates in the outer WHERE — the CTE was
		  -- already materialised. With the guard only inside the CTE, a second transaction that
		  -- waited on the first then updated the row anyway, reported RowsAffected()==1, and
		  -- Trash charged the retained counter again for a node that had already been charged.
		  -- The counter then sits permanently above the bytes actually retained, telling the
		  -- account it is full while its bin is empty. Found by
		  -- TestPGConformance/OwnerTrashPurge/APurgeRacingADeleteNeverDoubleReleases.
		  AND trashed_at IS NULL
		  AND ($2 = '' OR (SELECT etag FROM nodes WHERE id = $1) = $2)`
	etag := ""
	if expect != nil {
		etag = expect.IfMatch
	}
	tag, err := t.tx.Exec(ctx, q, id, etag)
	if err != nil {
		return 0, translate(err)
	}
	if tag.RowsAffected() == 0 {
		// Two causes, and they are different answers: the node changed under us (412, the
		// caller's If-Match no longer describes it), or it is gone (404).
		if expect != nil && etag != "" {
			if _, gerr := t.Node(ctx, id); gerr == nil {
				return 0, ErrPrecondition
			} else if errors.Is(gerr, ErrNotFound) {
				return 0, ErrNotFound
			} else {
				return 0, gerr
			}
		}
		return 0, ErrNotFound
	}
	if trashedParent != nil {
		if err := t.bumpDirAndUp(ctx, *trashedParent); err != nil {
			return 0, err
		}
	}
	return int(tag.RowsAffected()), nil
}

// Trashed lists trashed nodes that are the roots of their deleted subtree, as far as THIS
// owner is concerned.
//
// "Root of a trashed subtree" means the node is trashed and its parent is either not trashed
// or trashed under a DIFFERENT owner's account. That is what distinguishes an item the user
// deleted from a child that went along with it — and the ownership clause is what keeps the
// definition from stranding other people's files: when an account is deleted (or a shared
// folder is binned), another user's file inside it is trashed by someone else's action, its
// parent offers no path to it, and the person who could restore the whole subtree may be gone
// forever. For that file the owner's own trash listing is the only door left, so the listing
// treats the owner boundary as a root even though a trashed ancestor technically exists.
// Listing every trashed row instead would present a deleted folder and all its
// contents as independent, separately-restorable items.
func (t *pgTx) Trashed(ctx context.Context, ownerID uuid.UUID, limit int) ([]Node, error) {
	return t.TrashedAfter(ctx, ownerID, "", limit)
}

// TrashedAfter pages the trash, newest first, strictly after cursor.
//
// The cursor is a (trashed_at, id) pair rather than a bare timestamp because two items trashed in
// the same instant are otherwise indistinguishable, and a keyset that cannot break ties either
// repeats rows or skips them at the page boundary. id is in the ordering for the same reason: it
// is the only column guaranteed unique within one owner.
func (t *pgTx) TrashedAfter(ctx context.Context, ownerID uuid.UUID, cursor string, limit int) ([]Node, error) {
	var (
		at *time.Time
		id *uuid.UUID
	)
	if cursor != "" {
		parsedAt, parsedID, err := ParseTrashCursor(cursor)
		if err != nil {
			return nil, err
		}
		at, id = &parsedAt, &parsedID
	}

	const q = `SELECT ` + nodeColumns + ` FROM nodes n
		WHERE n.owner_id = $1
		  AND n.trashed_at IS NOT NULL
		  AND (
		        n.parent_id IS NULL
		     OR NOT EXISTS (
		          SELECT 1 FROM nodes p
		          WHERE p.id = n.parent_id
		            AND p.trashed_at IS NOT NULL
		            AND p.owner_id = n.owner_id
		        )
		  )
		AND ($2::timestamptz IS NULL OR (n.trashed_at, n.id) < ($2::timestamptz, $3::uuid))
		ORDER BY n.trashed_at DESC, n.id DESC
		LIMIT $4`
	rows, err := t.tx.Query(ctx, q, ownerID, at, id, limitArg(limit))
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

// ParseTrashCursor decodes a trash page cursor.
//
// Exported so the HTTP layer can build the next one from the last row it emitted — the store owns
// the format, because the ordering it must match lives here.
func ParseTrashCursor(cursor string) (time.Time, uuid.UUID, error) {
	atStr, idStr, ok := strings.Cut(cursor, ":")
	if !ok {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: malformed trash cursor", ErrInvalidCursor)
	}
	nanos, err := strconv.ParseInt(atStr, 10, 64)
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: trash cursor timestamp", ErrInvalidCursor)
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.UUID{}, fmt.Errorf("%w: trash cursor id", ErrInvalidCursor)
	}
	return time.Unix(0, nanos).UTC(), id, nil
}

// FormatTrashCursor encodes the position of a trashed node for the next page.
func FormatTrashCursor(n Node) string {
	if n.TrashedAt == nil {
		return ""
	}
	return fmt.Sprintf("%d:%s", n.TrashedAt.UTC().UnixNano(), n.ID)
}
func (t *pgTx) Restore(ctx context.Context, id uuid.UUID) (Node, error) {
	// The bytes come back into the logical quota, so sum them before the update clears the
	// marks — per owner, for the same reason Trash charges per owner: a restored subtree can
	// be mixed-owner, and crediting one account for another's bytes is the same bug in
	// reverse.
	usage, err := t.subtreeUsage(ctx, id, true)
	if err != nil {
		return Node{}, err
	}

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
	if err := t.creditBytesFromTrash(ctx, usage); err != nil {
		return Node{}, err
	}
	node, err := t.Node(ctx, id)
	if err != nil {
		return Node{}, err
	}
	// The entry came back into its parent directory (M1).
	if node.ParentID != nil {
		if err := t.bumpDirAndUp(ctx, *node.ParentID); err != nil {
			return Node{}, err
		}
	}
	return node, nil
}

func (t *pgTx) Versions(ctx context.Context, nodeID uuid.UUID, limit int) ([]Version, error) {
	// The checksum comes from blobs by joining on blob_id, which is the content address. A LEFT
	// JOIN because a version whose blob row was already collected still has an id and a date —
	// hiding it from the history would be worse than showing it without a checksum.
	const q = `SELECT v.id, v.node_id, v.version_no, v.blob_id, v.size, b.sha256, v.created_at,
		    v.created_by
		FROM versions v
		LEFT JOIN blobs b ON b.id = v.blob_id
		WHERE v.node_id = $1
		ORDER BY v.version_no DESC
		LIMIT $2`
	rows, err := t.tx.Query(ctx, q, nodeID, limitArg(limit))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.NodeID, &v.VersionNo, &v.BlobID, &v.Size, &v.SHA256, &v.CreatedAt, &v.CreatedBy); err != nil {
			return nil, translate(err)
		}
		out = append(out, v)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) RestoreVersion(ctx context.Context, nodeID uuid.UUID, versionNo int, by *uuid.UUID) (Node, error) {
	// The content hash comes back with the version, because the ETag must be derived from the
	// CONTENT and not from the row's identifiers.
	const q = `SELECT v.blob_id, v.size, b.sha256
		FROM versions v JOIN blobs b ON b.id = v.blob_id
		WHERE v.node_id = $1 AND v.version_no = $2`
	var blobID uuid.UUID
	var size int64
	var sum []byte
	if err := t.tx.QueryRow(ctx, q, nodeID, versionNo).Scan(&blobID, &size, &sum); err != nil {
		return Node{}, translate(err)
	}

	if t.s.stallBeforeLockForTest != nil {
		t.s.stallBeforeLockForTest(ctx)
	}
	// Lock the row BEFORE reading it. This method rewrites size, ETag and blob_id and moves
	// quota by the difference between the restored size and the current one, so the row it
	// diffs against must be the row it locks. Without the lock, a concurrent update that
	// commits between the read and the write is silently overwritten with the restored
	// content, and the quota moves by a delta computed from a snapshot that no longer
	// describes anything — the same stale-read window UpdateFile closed, on a path that
	// did not even have the lock to narrow it.
	if err := t.lockNode(ctx, nodeID); err != nil {
		return Node{}, err
	}
	current, err := t.Node(ctx, nodeID)
	if err != nil {
		return Node{}, err
	}
	// The restored content changes the file AND its containing directory (M1).
	if current.ParentID != nil {
		if err := t.bumpDirAndUp(ctx, *current.ParentID); err != nil {
			return Node{}, err
		}
	}

	// Restoring an old version keeps the current one: otherwise "undo" is
	// destructive and the user cannot change their mind.
	if current.BlobID != nil {
		if err := t.pushVersion(ctx, current, by); err != nil {
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

	// The ETag is derived from the content address, exactly like every other write path.
	//
	// It used to hash the blob's UUID string. That produced a stable, opaque, correctly
	// quoted tag - and a WRONG one: the same bytes had one ETag when uploaded and a different
	// one after being restored, so a client revalidating with the tag it already held got 412
	// for content that had not changed. It also contradicted ETagForHash's stated purpose:
	// one definition, so a browser upload and a WebDAV PUT cannot disagree about what the
	// same content is.
	var h [32]byte
	copy(h[:], sum)
	etag := ETagForHash(h)

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
	appendCtx := ctx
	if t.s.auditTimeout > 0 {
		var cancel context.CancelFunc
		appendCtx, cancel = context.WithTimeout(ctx, t.s.auditTimeout)
		defer cancel()
	}
	if err := t.s.auditWriter.Append(appendCtx, t, e); err != nil {
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

	// One round trip for the whole path. The per-segment loop this replaced paid the
	// connection latency once per component, and resolution runs before every file
	// operation: a six-deep path was seven queries. The recursive part joins only the
	// child whose name matches the segment at its depth, so the walk visits at most one
	// node per level; a missing or trashed segment simply stops the recursion, and the
	// count check turns that into the same ErrNotFound the loop returned. The depth
	// bound stops the iteration at the last segment, so a pathological name cycle
	// cannot spin it. Empty segs still returns the root: the anchor row alone.
	q := `
		WITH RECURSIVE walk AS (
		    SELECT n.*, 0 AS depth
		    FROM nodes n
		    WHERE n.owner_id = $1 AND n.parent_id IS NULL AND n.trashed_at IS NULL
		    UNION ALL
		    SELECT c.*, w.depth + 1
		    FROM nodes c
		    JOIN walk w ON c.parent_id = w.id
		    WHERE c.trashed_at IS NULL
		      AND w.depth < array_length($2::text[], 1)
		      AND c.name = ($2::text[])[w.depth + 1]
		)
		SELECT ` + nodeColumnsQualified("walk") + ` FROM walk ORDER BY depth`

	rows, err := s.pool.Query(ctx, q, ownerID, segs)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make([]Node, 0, len(segs)+1)
	for rows.Next() {
		// pgx.Rows satisfies pgx.Row, so the single-row scanner serves both shapes and
		// the column list cannot drift between them.
		n, serr := scanNode(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	if len(out) != len(segs)+1 {
		return nil, ErrNotFound
	}
	return out, nil
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

// SetUserQuota writes the account's byte limit.
//
// usage is NOT recomputed: used_bytes is the recorded consumption, and lowering the limit below it
// leaves the account over quota, which is a state reserveQuota already refuses. Rewriting usage to
// match the new limit would forget bytes that are still on disk.
func (t *pgTx) SetUserQuota(ctx context.Context, userID uuid.UUID, bytes int64) error {
	tag, err := t.tx.Exec(ctx,
		`UPDATE users SET quota_bytes = $2, updated_at = now() WHERE id = $1`, userID, bytes)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListOwnerNodes returns every live node the account owns.
func (t *pgTx) ListOwnerNodes(ctx context.Context, ownerID uuid.UUID) ([]Node, error) {
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE owner_id = $1 AND trashed_at IS NULL
		ORDER BY parent_id NULLS FIRST, name, id`
	rows, err := t.tx.Query(ctx, q, ownerID)
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

// ReadSettings returns every runtime override.
//
// Read one row at a time rather than through a generic map scan: the settings are few, and a typed
// decode keeps a malformed value from becoming a runtime surprise in whichever handler reads it.
func (t *pgTx) ReadSettings(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := t.tx.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}
	for rows.Next() {
		var (
			key   string
			value []byte
		)
		if err := rows.Scan(&key, &value); err != nil {
			return nil, translate(err)
		}
		out[key] = json.RawMessage(value)
	}
	return out, translate(rows.Err())
}

// WriteSetting stores one override, replacing any previous value.
func (t *pgTx) WriteSetting(ctx context.Context, key string, value json.RawMessage, by uuid.UUID) error {
	var uuidArg *uuid.UUID
	if by != uuid.Nil {
		uuidArg = &by
	}
	_, err := t.tx.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES ($1, $2, now(), $3)
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		key, []byte(value), uuidArg)
	return translate(err)
}

// DeleteSetting removes an override.
func (t *pgTx) DeleteSetting(ctx context.Context, key string) error {
	_, err := t.tx.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key)
	return translate(err)
}

// TrashCapped moves a node to the trash, refusing when retained usage would exceed the cap.
//
// Both the sum and the counter are read inside the caller's transaction, so the check sees a
// consistent snapshot and two concurrent deletes cannot both fit into one slot.
func (t *pgTx) TrashCapped(ctx context.Context, id uuid.UUID, maxRetainedBytes int64) error {
	return t.TrashCappedExpect(ctx, id, maxRetainedBytes, nil)
}

// TrashCappedExpect is TrashCapped with a precondition on the node's current state.
//
// The precondition is evaluated in the UPDATE that trashes (see markTrashed), which is what
// lets a DELETE refuse because a concurrent PUT changed the file after the handler resolved
// it — instead of trashing that writer's successful update along with the row it belonged to.
func (t *pgTx) TrashCappedExpect(ctx context.Context, id uuid.UUID, maxRetainedBytes int64,
	expect *Precondition) error {
	usage, err := t.subtreeUsage(ctx, id, false)
	if err != nil {
		return err
	}
	if _, err := t.markTrashed(ctx, id, expect); err != nil {
		return err
	}
	if maxRetainedBytes <= 0 {
		return t.chargeBytesToTrash(ctx, usage)
	}

	// The cap is enforced by the WHERE clause of the counter update, not by a read
	// followed by a write.
	//
	// The previous shape read `trash_bytes`, compared it to the cap, and then let Trash
	// add to it in a second statement. Under READ COMMITTED two concurrent deletes both
	// read the same pre-cap total and both passed: eight racing deletes against a
	// 1000-byte cap left trash_bytes at 4800 (2026-10-24 review, H15). Here the same
	// UPDATE both decides and charges, so the row lock serialises the two.
	//
	// Per owner, because the cap bounds each account's retained usage and the subtree can
	// be mixed-owner: charging the whole subtree to the directory's owner would refuse a
	// delete that fits and move bytes between accounts that are not each other's.
	//
	// A zero row count means the cap would have been exceeded — or, when there is nothing
	// to charge, that the owner row is missing, which is a different failure from a full
	// bin. Returning the error rolls the whole transaction back — including the mark
	// above — so a refused delete leaves the tree exactly as it was.
	for _, u := range usage {
		tag, err := t.tx.Exec(ctx, `
			UPDATE users
			SET used_bytes = GREATEST(used_bytes - $2, 0),
			    trash_bytes = trash_bytes + $2,
			    updated_at = now()
			WHERE id = $1 AND trash_bytes + $2 <= $3`, u.owner, u.bytes, maxRetainedBytes)
		if err != nil {
			return translate(err)
		}
		if tag.RowsAffected() == 0 {
			if u.bytes == 0 {
				return ErrNotFound
			}
			return ErrTrashFull
		}
	}
	return nil
}

// TrimRetainedVersions drops the oldest version rows until each account's history fits capBytes.
//
// The running total is computed with a window function ordered oldest-first, and every row whose
// cumulative size exceeds the cap is dropped: that keeps the NEWEST history, which is the order a
// user expects ("undo the last change"). Blob references are released for exactly the rows removed,
// or their refcount stays high and the bytes are never collected.
func (t *pgTx) TrimRetainedVersions(ctx context.Context, capBytes int64) (int, error) {
	if capBytes <= 0 {
		return 0, nil
	}

	const doomed = `
		WITH ranked AS (
			SELECT v.id, v.node_id, v.version_no,
			       sum(v.size) OVER (PARTITION BY n.owner_id ORDER BY v.created_at DESC, v.id DESC
			                         ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS running
			FROM versions v JOIN nodes n ON n.id = v.node_id
		)
		SELECT v.id, v.blob_id FROM versions v
		JOIN ranked r ON r.id = v.id
		WHERE r.running > $1`
	rows, err := t.tx.Query(ctx, doomed, capBytes)
	if err != nil {
		return 0, translate(err)
	}
	type victim struct {
		id     uuid.UUID
		blobID uuid.UUID
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.blobID); err != nil {
			rows.Close()
			return 0, translate(err)
		}
		victims = append(victims, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, translate(err)
	}
	for _, v := range victims {
		if _, err := t.tx.Exec(ctx, `DELETE FROM versions WHERE id = $1`, v.id); err != nil {
			return 0, translate(err)
		}
		blobID := v.blobID
		if err := t.adjustBlobRef(ctx, &blobID, -1); err != nil {
			return 0, err
		}
	}
	return len(victims), nil
}
