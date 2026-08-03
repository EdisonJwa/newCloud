package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL share implementation.

const shareColumns = `id, node_id, owner_id, kind, label, token_hash, password_hash,
	expires_at, max_downloads, download_count, max_uploads, upload_count,
	max_bytes_per_file, allowed_mime, permissions, hide_contents, created_at, revoked_at`

func scanShare(row pgx.Row) (Share, error) {
	var s Share
	var kind string
	err := row.Scan(&s.ID, &s.NodeID, &s.OwnerID, &kind, &s.Label, &s.TokenHash,
		&s.PasswordHash, &s.ExpiresAt, &s.MaxDownloads, &s.DownloadCount,
		&s.MaxUploads, &s.UploadCount, &s.MaxBytesPerFile, &s.AllowedMIME,
		&s.Permissions, &s.HideContents, &s.CreatedAt, &s.RevokedAt)
	if err != nil {
		return Share{}, translate(err)
	}
	s.Kind = ShareKind(kind)
	return s, nil
}

func (t *pgTx) CreateShare(ctx context.Context, s Share) (Share, error) {
	const q = `INSERT INTO shares (node_id, owner_id, kind, label, token_hash, password_hash,
			expires_at, max_downloads, max_uploads, max_bytes_per_file, allowed_mime,
			permissions, hide_contents)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING ` + shareColumns
	return scanShare(t.tx.QueryRow(ctx, q, s.NodeID, s.OwnerID, string(s.Kind), s.Label,
		s.TokenHash, s.PasswordHash, s.ExpiresAt, s.MaxDownloads, s.MaxUploads,
		s.MaxBytesPerFile, s.AllowedMIME, s.Permissions, s.HideContents))
}

func (t *pgTx) ShareByTokenHash(ctx context.Context, hash []byte) (Share, error) {
	const q = `SELECT ` + shareColumns + ` FROM shares WHERE token_hash = $1`
	return scanShare(t.tx.QueryRow(ctx, q, hash))
}

func (t *pgTx) ShareByID(ctx context.Context, id uuid.UUID) (Share, error) {
	const q = `SELECT ` + shareColumns + ` FROM shares WHERE id = $1`
	return scanShare(t.tx.QueryRow(ctx, q, id))
}

func (t *pgTx) UpdateShare(ctx context.Context, s Share) (Share, error) {
	const q = `UPDATE shares SET label=$2, password_hash=$3, expires_at=$4,
			max_downloads=$5, max_uploads=$6, max_bytes_per_file=$7,
			allowed_mime=$8, permissions=$9, hide_contents=$10
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING ` + shareColumns
	return scanShare(t.tx.QueryRow(ctx, q, s.ID, s.Label, s.PasswordHash, s.ExpiresAt,
		s.MaxDownloads, s.MaxUploads, s.MaxBytesPerFile, s.AllowedMIME, s.Permissions,
		s.HideContents))
}

func (t *pgTx) RevokeShare(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE shares SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	tag, err := t.tx.Exec(ctx, q, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IncrementDownload counts a download, failing atomically at the cap.
//
// The WHERE clause is the whole point: "count it only if there is room left" as a
// single statement. Reading the counter and then updating lets two concurrent
// requests both observe room for one more copy, which is exactly how "download
// at most once" turns into "downloaded twice".
func (t *pgTx) IncrementDownload(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE shares SET download_count = download_count + 1
		WHERE id = $1
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		  AND (max_downloads IS NULL OR download_count < max_downloads)
		RETURNING download_count`
	var n int
	if err := t.tx.QueryRow(ctx, q, id).Scan(&n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShareExhausted
		}
		return translate(err)
	}
	return nil
}

func (t *pgTx) IncrementUpload(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE shares SET upload_count = upload_count + 1
		WHERE id = $1
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		  AND (max_uploads IS NULL OR upload_count < max_uploads)
		RETURNING upload_count`
	var n int
	if err := t.tx.QueryRow(ctx, q, id).Scan(&n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShareExhausted
		}
		return translate(err)
	}
	return nil
}

func (t *pgTx) SharesForNode(ctx context.Context, nodeID uuid.UUID) ([]Share, error) {
	const q = `SELECT ` + shareColumns + ` FROM shares
		WHERE node_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC`
	rows, err := t.tx.Query(ctx, q, nodeID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	return scanShares(rows)
}

func (t *pgTx) SharesByOwner(ctx context.Context, ownerID uuid.UUID) ([]Share, error) {
	const q = `SELECT ` + shareColumns + ` FROM shares
		WHERE owner_id = $1 ORDER BY created_at DESC`
	rows, err := t.tx.Query(ctx, q, ownerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	return scanShares(rows)
}

func scanShares(rows pgx.Rows) ([]Share, error) {
	var out []Share
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) AddRecipient(ctx context.Context, r ShareRecipient) error {
	const q = `INSERT INTO share_recipients (share_id, email, user_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (share_id, email) DO NOTHING`
	_, err := t.tx.Exec(ctx, q, r.ShareID, r.Email, r.UserID)
	return translate(err)
}

func (t *pgTx) RecipientsFor(ctx context.Context, shareID uuid.UUID) ([]ShareRecipient, error) {
	const q = `SELECT id, share_id, email, user_id, invited_at, verified_at,
			first_access_at, last_access_at, revoked_at, consumed_at
		FROM share_recipients WHERE share_id = $1 ORDER BY invited_at`
	rows, err := t.tx.Query(ctx, q, shareID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []ShareRecipient
	for rows.Next() {
		r, err := scanRecipient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) RecipientFor(ctx context.Context, shareID uuid.UUID, email string) (ShareRecipient, error) {
	const q = `SELECT id, share_id, email, user_id, invited_at, verified_at,
			first_access_at, last_access_at, revoked_at, consumed_at
		FROM share_recipients WHERE share_id = $1 AND email = $2`
	return scanRecipient(t.tx.QueryRow(ctx, q, shareID, email))
}

func (t *pgTx) MarkRecipientVerified(ctx context.Context, shareID uuid.UUID, email string) error {
	const q = `UPDATE share_recipients SET verified_at = COALESCE(verified_at, now())
		WHERE share_id = $1 AND email = $2`
	_, err := t.tx.Exec(ctx, q, shareID, email)
	return translate(err)
}

func (t *pgTx) MarkRecipientAccess(ctx context.Context, shareID uuid.UUID, email string) error {
	const q = `UPDATE share_recipients
		SET first_access_at = COALESCE(first_access_at, now()), last_access_at = now()
		WHERE share_id = $1 AND email = $2`
	_, err := t.tx.Exec(ctx, q, shareID, email)
	return translate(err)
}

func scanRecipient(row pgx.Row) (ShareRecipient, error) {
	var r ShareRecipient
	err := row.Scan(&r.ID, &r.ShareID, &r.Email, &r.UserID, &r.InvitedAt,
		&r.VerifiedAt, &r.FirstAccessAt, &r.LastAccessAt, &r.RevokedAt, &r.ConsumedAt)
	if err != nil {
		return ShareRecipient{}, translate(err)
	}
	return r, nil
}

var _ ShareTx = (*pgTx)(nil)
var _ = time.Now
