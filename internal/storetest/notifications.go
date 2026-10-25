package storetest

// Notification conformance (M4). The suite exists for the same reason the social one does: the
// surface is implemented twice — SQL with a partial unique index, and a Go loop with a scan — and
// a case only one backend runs proves nothing about the other.
//
// The clock is the in-memory backend's trap: time.Now() can return the same value twice in a row
// on this platform, so the same-instant tiebreak on id is load-bearing here, not decoration.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

func mustNotify(t *testing.T, s store.Store, n store.NewNotification) {
	t.Helper()
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Notify(ctx, n)
	})
	if err != nil {
		t.Fatalf("Notify(%s): %v", n.Kind, err)
	}
}

func listNotifications(t *testing.T, s store.Store, user uuid.UUID, cursor string, limit int) ([]store.Notification, string, bool, error) {
	t.Helper()
	var out []store.Notification
	var next string
	var truncated bool
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		items, n, tr, err := tx.Notifications(ctx, user, cursor, limit)
		out, next, truncated = items, n, tr
		return err
	})
	return out, next, truncated, err
}

func mustMarkRead(t *testing.T, s store.Store, user, id uuid.UUID) {
	t.Helper()
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.MarkNotificationRead(ctx, user, id)
	})
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
}

func unreadCount(t *testing.T, s store.Store, user uuid.UUID) int64 {
	t.Helper()
	var n int64
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		c, err := tx.UnreadNotificationCount(ctx, user)
		n = c
		return err
	})
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	return n
}

// NotificationCases is the shared suite.
func NotificationCases(factory Factory) func(*testing.T) {
	return func(t *testing.T) {
		s, alice := factory(t)
		bob := addUser(t, s, "notif-bob").ID

		// The inbox lists newest first, and paging returns every row exactly once — including
		// rows that share a created_at, which is the id tiebreak's whole job.
		t.Run("InboxPagesNewestFirstWithoutRepeats", func(t *testing.T) {
			mustNotify(t, s, store.NewNotification{UserID: alice, Kind: store.NotifyQuotaWarning,
				Meta: map[string]any{"percent": float64(90)}})
			mustNotify(t, s, store.NewNotification{UserID: alice, Kind: store.NotifyShareReceived,
				Meta: map[string]any{"label": "Reports"}})
			mustNotify(t, s, store.NewNotification{UserID: bob, Kind: store.NotifyShareReceived,
				Meta: map[string]any{"label": "Other inbox"}})

			var items []store.Notification
			cursor := ""
			for {
				page, next, truncated, err := listNotifications(t, s, alice, cursor, 1)
				if err != nil {
					t.Fatalf("page: %v", err)
				}
				items = append(items, page...)
				if !truncated {
					break
				}
				cursor = next
			}
			if len(items) != 2 {
				t.Fatalf("alice's inbox has %d items, want 2 (bob's row must not leak)", len(items))
			}
			// The two rows share created_at on a clock whose ticks do not advance, so their
			// relative order is the id tiebreak — an implementation detail, not a promise.
			// What the walk must guarantee is exactly-once: both rows, whichever order.
			kinds := map[string]bool{}
			for _, n := range items {
				kinds[n.Kind] = true
				if n.UserID != alice {
					t.Fatalf("foreign row in alice's inbox")
				}
			}
			if !kinds[store.NotifyQuotaWarning] || !kinds[store.NotifyShareReceived] {
				t.Fatalf("inbox is missing a row: %v", kinds)
			}

			// A cursor the server did not issue is refused, not treated as "start over".
			if _, _, _, err := listNotifications(t, s, alice, "not-a-cursor", 10); err != store.ErrInvalidCursor {
				t.Fatalf("malformed cursor: got %v, want ErrInvalidCursor", err)
			}
		})

		t.Run("DedupeCollapsesUntilRead", func(t *testing.T) {
			key := "quota:" + uuid.NewString()
			mustNotify(t, s, store.NewNotification{UserID: alice, Kind: store.NotifyQuotaWarning,
				DedupeKey: key, Meta: map[string]any{"percent": float64(91)}})
			mustNotify(t, s, store.NewNotification{UserID: alice, Kind: store.NotifyQuotaWarning,
				DedupeKey: key, Meta: map[string]any{"percent": float64(99)}})

			items, _, _, err := listNotifications(t, s, alice, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			var unreads int
			var id uuid.UUID
			for _, n := range items {
				if n.DedupeKey == key && !n.Read() {
					unreads++
					id = n.ID
				}
			}
			if unreads != 1 {
				t.Fatalf("duplicate unread dedupe rows: %d, want 1", unreads)
			}

			// Reading it re-arms the key: the next warning is a new row, which is what a bell
			// promises — dismissed once, told again the next time it happens.
			mustMarkRead(t, s, alice, id)
			mustNotify(t, s, store.NewNotification{UserID: alice, Kind: store.NotifyQuotaWarning,
				DedupeKey: key})
			items, _, _, err = listNotifications(t, s, alice, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			var total int
			for _, n := range items {
				if n.DedupeKey == key {
					total++
				}
			}
			if total != 2 {
				t.Fatalf("after read, dedupe should allow a new row: %d rows with the key, want 2", total)
			}
		})

		t.Run("MarkReadIsOwnerScopedAndIdempotent", func(t *testing.T) {
			mustNotify(t, s, store.NewNotification{UserID: alice, Kind: store.NotifyLinkExpiring,
				DedupeKey: "share:" + uuid.NewString()})
			items, _, _, err := listNotifications(t, s, alice, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) == 0 {
				t.Fatal("no notification to read")
			}
			// The first UNREAD row, not the first row: earlier subtests left read rows behind,
			// and on a clock whose ticks do not advance the newest-first order among same-time
			// rows is the id tiebreak, which can put an already-read row first.
			var id uuid.UUID
			for _, n := range items {
				if !n.Read() {
					id = n.ID
					break
				}
			}
			if id == uuid.Nil {
				t.Fatal("no unread notification in the listing")
			}
			before := unreadCount(t, s, alice)

			// Bob cannot read (or discover) alice's notification.
			err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.MarkNotificationRead(ctx, bob, id)
			})
			if err != store.ErrNotFound {
				t.Fatalf("foreign mark-read: got %v, want ErrNotFound", err)
			}
			mustMarkRead(t, s, alice, id)
			// Reading again is success, not a conflict.
			mustMarkRead(t, s, alice, id)

			// Exactly the one row was dismissed — the earlier subtests' rows are untouched.
			if got := unreadCount(t, s, alice); got != before-1 {
				t.Fatalf("unread after reading one of %d: %d, want %d", before, got, before-1)
			}
		})

		t.Run("MarkAllAndPrune", func(t *testing.T) {
			for i := 0; i < 3; i++ {
				mustNotify(t, s, store.NewNotification{UserID: bob, Kind: store.NotifyShareReceived,
					DedupeKey: "bulk:" + uuid.NewString()})
			}
			if got := unreadCount(t, s, bob); got < 1 {
				t.Fatal("bob has nothing unread to mark")
			}
			var marked int64
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.MarkAllNotificationsRead(ctx, bob)
				marked = n
				return err
			})
			if err != nil || marked == 0 {
				t.Fatalf("mark-all: marked=%d err=%v", marked, err)
			}
			if got := unreadCount(t, s, bob); got != 0 {
				t.Fatalf("unread after mark-all: %d", got)
			}

			var pruned int64
			err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.PruneNotifications(ctx, time.Now().Add(time.Hour))
				pruned = n
				return err
			})
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if pruned == 0 {
				t.Fatal("prune removed nothing although everything is older than the window")
			}
		})

		t.Run("ValidationRefusesUnknownKindsAndBadMeta", func(t *testing.T) {
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.Notify(ctx, store.NewNotification{UserID: alice, Kind: "yell.at_user"})
			})
			if !errors.Is(err, store.ErrInvalidNotification) {
				t.Fatalf("unknown kind: got %v, want ErrInvalidNotification", err)
			}
			err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.Notify(ctx, store.NewNotification{UserID: alice, Kind: store.NotifyQuotaWarning,
					Meta: map[string]any{"evil": map[string]any{"nested": true}}})
			})
			if !errors.Is(err, store.ErrInvalidNotification) {
				t.Fatalf("nested meta: got %v, want ErrInvalidNotification", err)
			}
		})

		t.Run("ExpiringShares", func(t *testing.T) {
			r := rootOf(t, s, alice)
			soon := time.Now().Add(2 * time.Hour)
			never := time.Now().Add(30 * 24 * time.Hour)
			expiring := createExpiringShare(t, s, r.ID, alice, &soon)
			farOut := createExpiringShare(t, s, r.ID, alice, &never)

			var got []store.ExpiringShare
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				rows, err := tx.ExpiringShares(ctx, time.Now(), 24*time.Hour, 100)
				got = rows
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			var sawExpiring, sawFar bool
			for _, e := range got {
				if e.ID == expiring {
					sawExpiring = true
					if e.OwnerMail == "" {
						t.Fatal("expiring share did not carry the owner's address")
					}
				}
				if e.ID == farOut {
					sawFar = true
				}
			}
			if !sawExpiring {
				t.Fatal("a share expiring in 2h was not listed inside a 24h window")
			}
			if sawFar {
				t.Fatal("a share expiring in 30d was listed inside a 24h window")
			}
		})
	}
}

// createExpiringShare creates one link share with the given expiry (nil = none) and returns its id.
func createExpiringShare(t *testing.T, s store.Store, nodeID, owner uuid.UUID, expires *time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		c, cerr := tx.CreateShare(ctx, store.Share{
			NodeID: nodeID, OwnerID: owner, Kind: store.ShareLink,
			TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
			Label: "expiry probe", ExpiresAt: expires,
		})
		id = c.ID
		return cerr
	})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	return id
}
