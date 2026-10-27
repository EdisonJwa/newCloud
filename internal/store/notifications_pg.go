package store

// Notifications on PostgreSQL (M4). The statements follow the house rules the social surface set:
// the dedupe is one INSERT with ON CONFLICT (no read-then-insert window), the listing is keyset,
// and the expiry scan reads only columns the share table guarantees.

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
)

var _ NotificationTx = (*pgTx)(nil)

// Notify inserts one notification, collapsing a duplicate unread dedupe key.
//
// The ON CONFLICT names the partial index's predicate (WHERE ...) which is the only way to target
// it; a conflict means an unread row with the same key exists, and "already warned" is the state
// the caller asked for, so it is not an error.
func (t *pgTx) Notify(ctx context.Context, n NewNotification) error {
	if err := ValidateNotification(n); err != nil {
		return err
	}
	meta, err := json.Marshal(marshalableMeta(n.Meta))
	if err != nil {
		return err
	}
	var key *string
	if n.DedupeKey != "" {
		k := n.DedupeKey
		key = &k
	}
	const q = `INSERT INTO notifications (user_id, kind, dedupe_key, meta)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, kind, dedupe_key)
		WHERE dedupe_key IS NOT NULL AND read_at IS NULL
		DO NOTHING`
	_, err = t.tx.Exec(ctx, q, n.UserID, n.Kind, key, meta)
	if err != nil {
		return translate(err)
	}
	return nil
}

// Notifications pages the inbox newest first.
func (t *pgTx) Notifications(ctx context.Context, userID uuid.UUID, cursor string, limit int) ([]Notification, string, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	args := []any{userID}
	q := `SELECT id, user_id, kind, dedupe_key, meta, read_at, created_at
		FROM notifications WHERE user_id = $1`
	if cursor != "" {
		at, id, err := decodeNotificationCursor(cursor)
		if err != nil {
			return nil, "", false, ErrInvalidCursor
		}
		args = append(args, at, id)
		q += ` AND (created_at, id) < ($2, $3)`
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)

	rows, err := t.tx.Query(ctx, q, args...)
	if err != nil {
		return nil, "", false, translate(err)
	}
	defer rows.Close()

	var out []Notification
	for rows.Next() {
		n, rerr := scanNotification(rows)
		if rerr != nil {
			return nil, "", false, rerr
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, translate(err)
	}
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	var next string
	if truncated {
		last := out[len(out)-1]
		next = encodeNotificationCursor(last.CreatedAt, last.ID)
	}
	return out, next, truncated, nil
}

// UnreadNotificationCount feeds the bell badge.
func (t *pgTx) UnreadNotificationCount(ctx context.Context, userID uuid.UUID) (int64, error) {
	var n int64
	err := t.tx.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`,
		userID).Scan(&n)
	if err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// MarkNotificationRead dismisses one row, only its owner's.
func (t *pgTx) MarkNotificationRead(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := t.tx.Exec(ctx,
		`UPDATE notifications SET read_at = now()
		 WHERE id = $1 AND user_id = $2 AND read_at IS NULL`,
		id, userID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		// Already read is success (idempotent); a row that is not the caller's — or does not
		// exist — is ErrNotFound, the uniform answer.
		var exists bool
		if rerr := t.tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM notifications WHERE id = $1 AND user_id = $2 AND read_at IS NOT NULL)`,
			id, userID).Scan(&exists); rerr == nil && exists {
			return nil
		}
		return ErrNotFound
	}
	return nil
}

// MarkAllNotificationsRead dismisses the whole inbox in one statement.
func (t *pgTx) MarkAllNotificationsRead(ctx context.Context, userID uuid.UUID) (int64, error) {
	tag, err := t.tx.Exec(ctx,
		`UPDATE notifications SET read_at = now()
		 WHERE user_id = $1 AND read_at IS NULL`,
		userID)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// PruneNotifications removes rows past the retention window, read or not.
func (t *pgTx) PruneNotifications(ctx context.Context, before time.Time) (int64, error) {
	tag, err := t.tx.Exec(ctx,
		`DELETE FROM notifications WHERE created_at < $1`, before)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// ExpiringShares lists live shares expiring inside the window, most urgent first.
//
// Only shares WITH an expiry are candidates (expires_at IS NOT NULL is implied by the range
// comparison), and revoked shares are excluded by the same predicate every share read uses. The
// owner's address rides along so the duty can queue the reminder mail in the same transaction
// without a second lookup per row.
func (t *pgTx) ExpiringShares(ctx context.Context, now time.Time, window time.Duration, limit int) ([]ExpiringShare, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := t.tx.Query(ctx, `
		SELECT s.id, s.owner_id, s.label, s.kind, s.expires_at, u.email
		FROM shares s
		JOIN users u ON u.id = s.owner_id
		WHERE s.revoked_at IS NULL
		  AND s.expires_at > $1
		  AND s.expires_at <= $1 + make_interval(secs => $2 / 1000000.0)
		ORDER BY s.expires_at ASC
		LIMIT $3`,
		now, window.Microseconds(), limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []ExpiringShare
	for rows.Next() {
		var e ExpiringShare
		var label *string
		if err := rows.Scan(&e.ID, &e.OwnerID, &label, &e.Kind, &e.ExpiresAt, &e.OwnerMail); err != nil {
			return nil, translate(err)
		}
		if label != nil {
			e.Label = *label
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// marshalableMeta normalises int/int64 to the one json number type, so the same meta written by
// either backend round-trips through jsonb identically.
func marshalableMeta(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case int:
			out[k] = float64(t)
		case int64:
			out[k] = float64(t)
		default:
			out[k] = v
		}
	}
	return out
}

// scanNotification reads one row. meta comes back as jsonb → []byte → map.
func scanNotification(rows interface {
	Scan(dest ...any) error
}) (Notification, error) {
	var n Notification
	var meta []byte
	var key *string
	if err := rows.Scan(&n.ID, &n.UserID, &n.Kind, &key, &meta, &n.ReadAt, &n.CreatedAt); err != nil {
		return Notification{}, translate(err)
	}
	if key != nil {
		n.DedupeKey = *key
	}
	if len(meta) > 0 {
		m := map[string]any{}
		if err := json.Unmarshal(meta, &m); err != nil {
			return Notification{}, err
		}
		n.Meta = m
	}
	return n, nil
}
