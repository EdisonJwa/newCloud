package store

// Notifications in the in-memory store (M4).
//
// The rules mirror the SQL deliberately, including the failures: the dedupe is a scan of unread
// rows rather than an index (the workspace is private to the transaction, so there is no race),
// and the listing keyset is the same (created_at DESC, id DESC) contract. The conformance suite
// runs both backends over the same cases.

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

var _ NotificationTx = (*memTx)(nil)

// Notify appends one notification, collapsing a duplicate unread dedupe key.
func (t *memTx) Notify(ctx context.Context, n NewNotification) error {
	if err := ValidateNotification(n); err != nil {
		return err
	}
	if n.DedupeKey != "" {
		for _, existing := range t.m.notifications {
			if existing.UserID == n.UserID && existing.Kind == n.Kind &&
				existing.DedupeKey == n.DedupeKey && existing.ReadAt == nil {
				return nil // already warned while unread: the no-op ON CONFLICT stands for
			}
		}
	}
	meta := map[string]any{}
	for k, v := range n.Meta {
		switch tv := v.(type) {
		case int:
			meta[k] = float64(tv)
		case int64:
			meta[k] = float64(tv)
		default:
			meta[k] = v
		}
	}
	// The id is assigned here, not by a caller: PostgreSQL's column default does this for the
	// other backend, and a nil id here would make every row match every other row's id — the
	// owner scoping and the dedupe would both collapse onto whichever row the scan met first.
	row := Notification{
		ID: uuid.New(), UserID: n.UserID, Kind: n.Kind, DedupeKey: n.DedupeKey,
		Meta: meta, CreatedAt: t.m.now(),
	}
	t.m.notifications = append(t.m.notifications, &row)
	return nil
}

// Notifications pages the inbox newest first.
func (t *memTx) Notifications(ctx context.Context, userID uuid.UUID, cursor string, limit int) ([]Notification, string, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	var after time.Time
	var afterID uuid.UUID
	if cursor != "" {
		at, id, err := decodeNotificationCursor(cursor)
		if err != nil {
			return nil, "", false, ErrInvalidCursor
		}
		after, afterID = at, id
	}

	// Candidates are collected and sorted, not walked in append order: the in-memory clock can
	// return the same value twice in a row (measured on this platform), and for two rows sharing
	// created_at the (created_at, id) order is the id's, which append order does not give. The
	// SQL gets this ordering from the btree; this is its equal.
	candidates := make([]*Notification, 0, len(t.m.notifications))
	for _, n := range t.m.notifications {
		if n.UserID == userID {
			candidates = append(candidates, n)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return uuidLess(b.ID, a.ID) // DESC by id on the tie
	})

	var out []Notification
	for _, n := range candidates {
		if cursor != "" {
			older := n.CreatedAt.Before(after) ||
				(n.CreatedAt.Equal(after) && uuidLess(n.ID, afterID))
			if !older {
				continue
			}
		}
		if len(out) == limit {
			last := out[len(out)-1]
			return out, encodeNotificationCursor(last.CreatedAt, last.ID), true, nil
		}
		out = append(out, *n)
	}
	return out, "", false, nil
}

// UnreadNotificationCount feeds the bell badge.
func (t *memTx) UnreadNotificationCount(ctx context.Context, userID uuid.UUID) (int64, error) {
	var n int64
	for _, row := range t.m.notifications {
		if row.UserID == userID && row.ReadAt == nil {
			n++
		}
	}
	return n, nil
}

// MarkNotificationRead dismisses one row, only its owner's.
func (t *memTx) MarkNotificationRead(ctx context.Context, userID, id uuid.UUID) error {
	for _, row := range t.m.notifications {
		if row.ID != id || row.UserID != userID {
			continue
		}
		if row.ReadAt == nil {
			now := t.m.now()
			row.ReadAt = &now // the workspace clone: published only if the transaction commits
		}
		return nil
	}
	return ErrNotFound
}

// MarkAllNotificationsRead dismisses the whole inbox.
func (t *memTx) MarkAllNotificationsRead(ctx context.Context, userID uuid.UUID) (int64, error) {
	var n int64
	for _, row := range t.m.notifications {
		if row.UserID == userID && row.ReadAt == nil {
			now := t.m.now()
			row.ReadAt = &now
			n++
		}
	}
	return n, nil
}

// PruneNotifications removes rows past the retention window, read or not.
func (t *memTx) PruneNotifications(ctx context.Context, before time.Time) (int64, error) {
	kept := t.m.notifications[:0]
	var n int64
	for _, row := range t.m.notifications {
		if row.CreatedAt.Before(before) {
			n++
			continue
		}
		kept = append(kept, row)
	}
	t.m.notifications = kept
	return n, nil
}

// ExpiringShares lists live shares expiring inside the window, most urgent first.
func (t *memTx) ExpiringShares(ctx context.Context, now time.Time, window time.Duration, limit int) ([]ExpiringShare, error) {
	if limit <= 0 {
		limit = 200
	}
	deadline := now.Add(window)
	var out []ExpiringShare
	for _, s := range t.m.shares {
		if s.RevokedAt != nil || s.ExpiresAt == nil {
			continue
		}
		if !s.ExpiresAt.After(now) || s.ExpiresAt.After(deadline) {
			continue
		}
		e := ExpiringShare{ID: s.ID, OwnerID: s.OwnerID, Label: s.Label,
			Kind: string(s.Kind), ExpiresAt: *s.ExpiresAt}
		if u, err := t.UserByID(ctx, s.OwnerID); err == nil {
			e.OwnerMail = u.Email
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// uuidLess orders ids by their byte representation, the tiebreaker the SQL gets for free from
// the uuid type's default btree ordering.
func uuidLess(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
