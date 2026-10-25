package worker

// Notification duties (competitiveness-analysis M4): the expiry reminder scan and the inbox
// retention prune. The mail.send JOB handler is registered in jobs.go with the others; what
// lives here is the periodic pass that decides who has something to be told.
//
// Both passes are idempotent, which the duty contract requires: the reminder's dedupe key is
// the share id (one reminder per link until read), and the prune is a DELETE with a WHERE.

import (
	"context"
	"fmt"
	"time"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/mail"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// WithMailer attaches the outbound transport for queued messages (M4).
//
// Nil is legal and means the deployment runs without mail: producers are expected not to
// enqueue in that case, but the handler still answers jobs it finds in the queue (from a
// deployment that turned mail off after queuing) by completing them with a warning rather
// than retrying forever — a message nobody can send is not going to become sendable.
func (w *Worker) WithMailer(m mail.Sender) *Worker {
	w.mailer = m
	return w
}

// WithNotify attaches the notification policy (M4): the expiry-reminder window and the inbox
// retention bound. Zero values disable the matching pass, which is what notify.retention_days:
// 0 and notify.share_expiry_days: 0 are for.
func (w *Worker) WithNotify(cfg config.NotifyConfig) *Worker {
	w.notify = cfg
	return w
}

// NotifyPass runs one expiry-reminder scan and one retention prune.
//
// One pass, two statements' worth of intent: the reminder is bounded by the scan's LIMIT and
// the dedupe keys, and the prune is bounded by the window. Neither can grow with the size of
// the deployment beyond those bounds, which is what makes a six-hour cadence enough.
func (w *Worker) NotifyPass(ctx context.Context) error {
	if w.notify.ShareExpiryDays > 0 {
		if err := w.remindExpiringShares(ctx); err != nil {
			return fmt.Errorf("notify: expiry reminders: %w", err)
		}
	}
	if w.notify.RetentionDays > 0 {
		before := time.Now().Add(-time.Duration(w.notify.RetentionDays) * 24 * time.Hour)
		var pruned int64
		err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			n, err := tx.PruneNotifications(ctx, before)
			pruned = n
			return err
		})
		if err != nil {
			return fmt.Errorf("notify: prune inbox: %w", err)
		}
		if pruned > 0 {
			w.log.Info("notification inbox pruned", "rows", pruned)
		}
	}
	return nil
}

// remindExpiringShares tells every owner whose link expires inside the window, once per link,
// in one transaction per pass.
func (w *Worker) remindExpiringShares(ctx context.Context) error {
	window := time.Duration(w.notify.ShareExpiryDays) * 24 * time.Hour
	return w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		expiring, err := tx.ExpiringShares(ctx, time.Now(), window, 500)
		if err != nil {
			return err
		}
		for _, e := range expiring {
			expires := e.ExpiresAt.UTC().Format("2006-01-02 15:04 MST")
			// One reminder per link until the owner reads it: the dedupe key is the share id,
			// and the six-hour cadence would otherwise say the same thing four times a day.
			if err := tx.Notify(ctx, store.NewNotification{
				UserID: e.OwnerID, Kind: store.NotifyLinkExpiring,
				DedupeKey: "share:" + e.ID.String(),
				Meta:      map[string]any{"label": e.Label, "expires_at": expires},
			}); err != nil {
				return err
			}
			// The mail rides the same transaction as the notification and the same dedupe
			// key: a pass before the previous mail was delivered must not say it twice,
			// and the key re-arms with the notification when the owner reads the reminder.
			if w.mailer != nil && e.OwnerMail != "" {
				if err := tx.Enqueue(ctx, store.Job{Kind: "mail.send", Payload: map[string]any{
					"to": e.OwnerMail, "template": "expiry_reminder",
					"label": e.Label, "expires": expires,
				}, DedupeKey: "expiry:" + e.ID.String()}); err != nil {
					return err
				}
			}
		}
		if len(expiring) > 0 {
			w.log.Info("share expiry reminders queued", "shares", len(expiring))
		}
		return nil
	})
}
