package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BlobIDFor returns the blobs row id for a content address, creating the row on
// first use.
//
// The deduplication index lives in the metadata layer, not in the storage
// backend: the backend only knows "these bytes have this hash", while the
// database knows which nodes reference it and how to bill for it.
//
// It is deliberately a Store-level call rather than a transaction method: the
// bytes have already been written by the time a handler asks for the id, and
// making it transactional would invite holding a database transaction open across
// a multi-gigabyte upload.
// size must be the real content length. It used to be hardcoded to 0, which made every blobs
// row claim it held nothing: the GC's accounting (BlobRef.Size, used to report what was
// reclaimed) was computed from a zero, and an operator reading `blobs.size` to understand disk
// usage got a table full of zeros. The node rows carried the true size, so nothing user-visible
// broke — which is exactly why it survived.
//
// The first writer for a given content address wins the row, and identical content always has
// the same length, so DO UPDATE leaving size alone is correct: whatever the first caller
// supplied is the truth.
func (s *PG) BlobIDFor(ctx context.Context, hash [32]byte, size int64) uuid.UUID {
	const q = `INSERT INTO blobs (sha256, size, backend)
		VALUES ($1, $2, $3)
		ON CONFLICT (sha256) DO UPDATE SET sha256 = blobs.sha256
		RETURNING id`
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, q, hash[:], size, "local").Scan(&id)
	if err == nil {
		return id
	}
	// Fall back to a read: the upsert can fail on a permission-restricted role.
	if rerr := s.pool.QueryRow(ctx, `SELECT id FROM blobs WHERE sha256 = $1`, hash[:]).Scan(&id); rerr == nil {
		return id
	}
	return uuid.Nil
}

// pgTxBlobID is the transactional variant used when the caller already holds a
// transaction.
func (t *pgTx) BlobID(ctx context.Context, hash [32]byte, size int64) (uuid.UUID, error) {
	const q = `INSERT INTO blobs (sha256, size, backend)
		VALUES ($1, $2, $3)
		ON CONFLICT (sha256) DO UPDATE SET sha256 = blobs.sha256
		RETURNING id`
	var id uuid.UUID
	if err := t.tx.QueryRow(ctx, q, hash[:], size, "local").Scan(&id); err != nil {
		return uuid.Nil, translate(err)
	}
	return id, nil
}

var _ = errors.Is
var _ = sql.ErrNoRows
var _ = pgx.ErrNoRows

// BlobHash returns the content address recorded for a blob.
//
// It is the reverse of BlobID and exists for the restore drill, which must compare restored
// bytes against what the metadata *recorded* rather than against the live copy: a corruption
// present on both sides would otherwise pass.
func (t *pgTx) BlobHash(ctx context.Context, id uuid.UUID) ([]byte, error) {
	var sum []byte
	if err := t.tx.QueryRow(ctx, `SELECT sha256 FROM blobs WHERE id = $1`, id).Scan(&sum); err != nil {
		return nil, translate(err)
	}
	return sum, nil
}

// SampleBlobHashes returns up to limit content addresses from the blobs table.
func (t *pgTx) SampleBlobHashes(ctx context.Context, after [32]byte, limit int) ([][32]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Keyset pagination, not OFFSET: the unique index on sha256 makes `sha256 > $1` a range scan,
	// so each pass costs what it returns rather than scanning everything before the cursor.
	const q = `SELECT sha256 FROM blobs WHERE $1::bytea IS NULL OR sha256 > $1::bytea
	           ORDER BY sha256 LIMIT $2`
	var cursor any
	if after != ([32]byte{}) {
		cursor = after[:]
	}
	rows, err := t.tx.Query(ctx, q, cursor, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out [][32]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var h [32]byte
		copy(h[:], raw)
		out = append(out, h)
	}
	return out, translate(rows.Err())
}
