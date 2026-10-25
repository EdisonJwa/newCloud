package server

// The notification surface (competitiveness-analysis M4): the inbox routes, the emitters that
// feed them, and the seam that queues outbound mail.
//
// The split is deliberate: the inbox is READ from here, but notifications are WRITTEN by the
// features that have something to say (a grant, an upload against a quota, a link nearing its
// expiry). Those emitters live beside their features; the helpers they share live here.

import (
	"context"
	"net/http"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// notificationJSON is one inbox row as the client renders it. The meta map is passed through
// untouched — rendering happens in the user's locale, which the server does not know.
type notificationJSON struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Meta      map[string]any `json:"meta,omitempty"`
	Read      bool           `json:"read"`
	CreatedAt string         `json:"created_at"`
}

// handleNotifications lists the caller's inbox, newest first, under the cursor contract.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	limit := parseLimit(r.URL.Query().Get("limit"), 100)

	var items []store.Notification
	var cursor string
	var truncated bool
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		page, next, tr, err := tx.Notifications(ctx, actor.ID(), r.URL.Query().Get("cursor"), limit)
		items, cursor, truncated = page, next, tr
		return err
	})
	if err != nil {
		if err == store.ErrInvalidCursor {
			writeError(w, http.StatusBadRequest, "invalid_cursor",
				"the cursor is not one this server issued; drop it to start over")
			return
		}
		s.writeStoreError(w, err)
		return
	}

	var unread int64
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.UnreadNotificationCount(ctx, actor.ID())
		unread = n
		return err
	})

	out := make([]notificationJSON, 0, len(items))
	for _, n := range items {
		out = append(out, notificationJSON{
			ID: n.ID.String(), Kind: n.Kind, Meta: n.Meta, Read: n.Read(),
			CreatedAt: n.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": out, "unread": unread, "truncated": truncated, "cursor": cursor,
	})
}

// handleNotificationRead dismisses one row. A row that is not the caller's answers 404, the
// uniform answer — an inbox must not be a probe for whose notifications exist.
func (s *Server) handleNotificationRead(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "the notification id is not a uuid")
		return
	}
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		return tx.MarkNotificationRead(ctx, actor.ID(), id)
	}); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "not_found", "no such notification")
			return
		}
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleNotificationsReadAll dismisses the whole inbox and reports how many that was, so the
// client can tell "done" from "there was nothing to do".
func (s *Server) handleNotificationsReadAll(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	var marked int64
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.MarkAllNotificationsRead(ctx, actor.ID())
		marked = n
		return err
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": marked})
}

// enqueueMail queues one message. The worker owns delivery, retry and backoff: a request that
// shared something must not wait on an SMTP relay, and a relay that is down must not fail a
// share — the job failing and retrying is the honest middle.
//
// Called inside the caller's transaction where one exists (the grant path), so a share that
// rolls back never queues a mail about it. The dedupe key is the caller's statement of what
// "the same mail" means; without one, a retried request delivers twice.
func enqueueMail(ctx context.Context, tx store.Tx, template, to string, fields map[string]string, dedupeKey string) error {
	if to == "" {
		return nil
	}
	payload := map[string]any{"to": to, "template": template}
	for k, v := range fields {
		payload[k] = v
	}
	return tx.Enqueue(ctx, store.Job{Kind: "mail.send", DedupeKey: dedupeKey, Payload: payload})
}

// maybeWarnQuota is the quota-warning emitter, called after a successful publish.
//
// Deliberately AFTER the commit and deliberately best-effort: the warning is telemetry (§8 —
// not hash-chained), and a user's file being uploaded matters more than the bell that mentions
// it. A crash between the publish and this check loses a warning, not data.
//
// Two reads like quotaHasRoomFor, for the same reason: exactness is the reservation's job
// inside WriteFile; this is the message about the number, not the number's accountant.
func (s *Server) maybeWarnQuota(ctx context.Context, ownerID uuid.UUID) {
	threshold := s.deps.Notify.QuotaWarningPercent
	if threshold <= 0 {
		return
	}
	used, err := s.deps.Meta.Usage(ctx, ownerID)
	if err != nil {
		return
	}
	var quota int64
	if err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, gerr := tx.UserByID(ctx, ownerID)
		if gerr != nil {
			return gerr
		}
		quota = u.QuotaBytes
		return nil
	}); err != nil || quota <= 0 {
		return
	}
	pct := int(used * 100 / quota)
	if pct < threshold {
		return
	}
	// One dedupe key per band: crossing the threshold warns once, filling the quota warns
	// once more, and each re-arms when read.
	band := "warn"
	if pct >= 100 {
		band = "full"
	}
	err = s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Notify(ctx, store.NewNotification{
			UserID: ownerID, Kind: store.NotifyQuotaWarning,
			DedupeKey: "quota:" + band,
			Meta: map[string]any{
				"percent":    pct,
				"used_bytes": used, "quota_bytes": quota,
			},
		})
	})
	if err != nil {
		s.deps.Log.Warn("quota warning could not be recorded", "user", ownerID, "error", err)
	}
}

// notifyShareReceived is the grant emitter: an ACL grant whose subject is a user tells that
// user, in the same transaction as the grant and its audit row.
//
// Group grants do not notify in v1: a group can name a hundred accounts and an inbox is not a
// broadcast channel. The group's members find the space through the spaces listing, which is
// the surface that scales with membership.
func notifyShareReceived(ctx context.Context, tx store.Tx, grantee *store.User, actor authz.Actor,
	entryID uuid.UUID, label string) error {
	if grantee == nil {
		return nil
	}
	if err := tx.Notify(ctx, store.NewNotification{
		UserID: grantee.ID, Kind: store.NotifyShareReceived,
		DedupeKey: "grant:" + entryID.String(),
		Meta:      map[string]any{"actor": actor.Username(), "label": label},
	}); err != nil {
		return err
	}
	// The dedupe key is the grant entry: a retried grant request cannot mail twice, and a NEW
	// grant to the same person is new news and says so.
	return tx.Enqueue(ctx, store.Job{Kind: "mail.send",
		DedupeKey: "grant-notice:" + entryID.String(),
		Payload: map[string]any{
			"to": grantee.Email, "template": "share_notice",
			"actor": actor.Username(), "label": label,
		}})
}
