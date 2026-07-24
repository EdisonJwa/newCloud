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
func (s *PG) BlobIDFor(ctx context.Context, hash [32]byte) uuid.UUID {
	const q = `INSERT INTO blobs (sha256, size, backend)
		VALUES ($1, $2, $3)
		ON CONFLICT (sha256) DO UPDATE SET sha256 = blobs.sha256
		RETURNING id`
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, q, hash[:], 0, "local").Scan(&id)
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
func (t *pgTx) BlobID(ctx context.Context, hash [32]byte) (uuid.UUID, error) {
	const q = `INSERT INTO blobs (sha256, size, backend)
		VALUES ($1, $2, $3)
		ON CONFLICT (sha256) DO UPDATE SET sha256 = blobs.sha256
		RETURNING id`
	var id uuid.UUID
	if err := t.tx.QueryRow(ctx, q, hash[:], 0, "local").Scan(&id); err != nil {
		return uuid.Nil, translate(err)
	}
	return id, nil
}

var _ = errors.Is
var _ = sql.ErrNoRows
var _ = pgx.ErrNoRows
