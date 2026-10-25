package store

// Notifications: the in-app inbox (competitiveness-analysis-2026-10-25 M4).
//
// Three decisions, stated once because both backends must keep them:
//
//   - NOTHING HERE IS HASH-CHAINED. Core-scope §8 sets the policy for social metadata and
//     notifications follow it: the audit chain records who did what to whose files, and a quota
//     warning is not in that class. Diluting the chain with telemetry would make it worse at the
//     one job it exists for.
//   - A ROW IS STRUCTURED, NOT RENDERED. The kind and a small meta map are stored, never a
//     pre-localised sentence: the client renders in the user's locale, which the server does not
//     know. That is also why meta values are scalars — a nested object is a rendering decision
//     the server would be making for the client.
//   - DEDUPE IS PER UNREAD NOTIFICATION. The unique index covers (user, kind, dedupe_key) only
//     while read_at IS NULL, so "you are over 90%" fires once until the user reads it, and fires
//     again the next time it becomes true after that. An insert that hits the index is a no-op,
//     the same idempotency SetFavorite has.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Notification kinds. The set is closed: an unknown kind in the database is a programming
// error, and ValidateNotification refuses it rather than letting the client render a blank.
const (
	// NotifyShareReceived tells a grantee a folder was shared with them (an ACL grant whose
	// subject is a user). Group grants do not notify in v1: a group grant can name a hundred
	// accounts and the inbox is not a broadcast channel — the group's space listing is.
	NotifyShareReceived = "share.received"
	// NotifyQuotaWarning tells an owner they crossed the configured percentage of their quota.
	NotifyQuotaWarning = "quota.warning"
	// NotifyLinkExpiring tells a share owner a link expires inside the configured window.
	NotifyLinkExpiring = "link.expiring"
	// NotifyAVQuarantined is reserved for the antivirus verdict (M5): a file was moved to the
	// trash because a scan found it infected. Registered now so the vocabulary is stable by the
	// time the scanner lands; no emitter writes it yet.
	NotifyAVQuarantined = "av.quarantined"
	// NotifyCommentNew is reserved for comment/reply notifications (the §6 hook). Like the AV
	// kind, it is vocabulary without an emitter: the notifier lands with the notifications
	// substrate, the emitters follow their features.
	NotifyCommentNew = "comment.new"
)

// MaxNotificationMetaKeys bounds the meta map. Eight fields is more than any current kind uses;
// the bound exists so a future emitter cannot quietly turn a notification into a document.
const MaxNotificationMetaKeys = 8

// ErrInvalidNotification covers value-level refusals (unknown kind, bad meta shape, oversized
// dedupe key). One sentinel for the same reason ErrInvalidSocialValue is one: the writer is
// always this package and the failure is always a programming error, not user input.
var ErrInvalidNotification = errors.New("store: invalid notification")

// Notification is one inbox row.
type Notification struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Kind      string
	DedupeKey string
	Meta      map[string]any
	ReadAt    *time.Time
	CreatedAt time.Time
}

// Read reports whether the user has dismissed it.
func (n Notification) Read() bool { return n.ReadAt != nil }

// NewNotification is what an emitter supplies. UserID and Kind are required; DedupeKey and Meta
// are optional.
type NewNotification struct {
	UserID    uuid.UUID
	Kind      string
	DedupeKey string
	Meta      map[string]any
}

// ValidateNotification enforces the vocabulary and the meta shape for both backends.
func ValidateNotification(n NewNotification) error {
	switch n.Kind {
	case NotifyShareReceived, NotifyQuotaWarning, NotifyLinkExpiring,
		NotifyAVQuarantined, NotifyCommentNew:
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidNotification, n.Kind)
	}
	if n.UserID == uuid.Nil {
		return fmt.Errorf("%w: no user", ErrInvalidNotification)
	}
	if len(n.DedupeKey) > 128 {
		return fmt.Errorf("%w: dedupe key too long", ErrInvalidNotification)
	}
	if len(n.Meta) > MaxNotificationMetaKeys {
		return fmt.Errorf("%w: meta has more than %d fields", ErrInvalidNotification, MaxNotificationMetaKeys)
	}
	for k, v := range n.Meta {
		if !validNotificationMetaKey(k) {
			return fmt.Errorf("%w: meta key %q is not [a-z0-9_]", ErrInvalidNotification, k)
		}
		switch t := v.(type) {
		case nil, bool:
		case int:
		case int64:
		case float64:
		case string:
			if utf8.RuneCountInString(t) > 200 {
				return fmt.Errorf("%w: meta field %q is too long", ErrInvalidNotification, k)
			}
		default:
			return fmt.Errorf("%w: meta field %q is not a scalar", ErrInvalidNotification, k)
		}
	}
	return nil
}

func validNotificationMetaKey(k string) bool {
	if k == "" || len(k) > 32 {
		return false
	}
	for _, r := range k {
		if r < 'a' || r > 'z' {
			if r < '0' || r > '9' {
				if r != '_' {
					return false
				}
			}
		}
	}
	return true
}

// encodeNotificationCursor renders the keyset position (created_at DESC, id DESC). Opaque to the
// client by construction — it is two joined values, and the handler refuses one the server did
// not issue rather than guessing what it meant.
func encodeNotificationCursor(at time.Time, id uuid.UUID) string {
	return at.UTC().Format(time.RFC3339Nano) + "|" + id.String()
}

// decodeNotificationCursor is the inverse. An unparsable cursor is an error, never a restart:
// that is the ADR-0012 contract every listing keeps.
func decodeNotificationCursor(s string) (time.Time, uuid.UUID, error) {
	at, id, ok := strings.Cut(s, "|")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("notification cursor: no separator")
	}
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("notification cursor: bad time: %w", err)
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("notification cursor: bad id: %w", err)
	}
	return ts, uid, nil
}

// ExpiringShare is one row of the expiry duty's scan: a live share whose expires_at falls inside
// the window, with the owner's address so the reminder mail can be queued in the same pass.
type ExpiringShare struct {
	ID        uuid.UUID
	OwnerID   uuid.UUID
	OwnerMail string
	Label     string
	Kind      string
	ExpiresAt time.Time
}

// NotificationTx is the inbox surface. Both backends implement it; the shared conformance suite
// holds them to the same answers, including the dedupe and cursor behaviour.
type NotificationTx interface {
	// Notify inserts one notification. Idempotent under a dedupe key while the previous row is
	// unread: the conflicting insert is a no-op, not an error, because the caller's statement
	// ("warn once") is satisfied either way.
	Notify(ctx context.Context, n NewNotification) error
	// Notifications pages a user's inbox, newest first, under the keyset cursor contract.
	Notifications(ctx context.Context, userID uuid.UUID, cursor string, limit int) ([]Notification, string, bool, error)
	// UnreadNotificationCount feeds the bell badge.
	UnreadNotificationCount(ctx context.Context, userID uuid.UUID) (int64, error)
	// MarkNotificationRead dismisses one. A row that is not the caller's is ErrNotFound — the
	// uniform answer, so an inbox cannot be used to probe whose notifications exist.
	MarkNotificationRead(ctx context.Context, userID, id uuid.UUID) error
	// MarkAllNotificationsRead dismisses everything and returns how many that was.
	MarkAllNotificationsRead(ctx context.Context, userID uuid.UUID) (int64, error)
	// PruneNotifications removes rows older than the window, read or not. The retention policy is
	// a bound on the table, not a promise to the user: an unread warning a year old is noise.
	PruneNotifications(ctx context.Context, before time.Time) (int64, error)
	// ExpiringShares lists live shares that expire within the window (the link.expiring duty).
	// Rows are ordered by expires_at so a bounded pass warns about the most urgent first.
	ExpiringShares(ctx context.Context, now time.Time, window time.Duration, limit int) ([]ExpiringShare, error)
}
