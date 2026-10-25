package worker

// Notification duty and mail.send handler tests (M4).
//
// The store conformance suite covers the inbox's own semantics. What can only be tested here
// is the loop end of the design: that a queued message is actually delivered through the
// transport, that a delivery failure is retried rather than dropped, and that the expiry pass
// produces both the owner's notification and its mail in one transaction.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/mail"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

// fakeMailer records (or fails) deliveries without a network.
type fakeMailer struct {
	fail       bool
	notices    []string // "to|label"
	reminders  []string // "to|label"
	codesCount int
}

func (f *fakeMailer) SendRecipientCode(to, shareLabel, code, ttl string) {
	f.codesCount++
}

func (f *fakeMailer) SendShareNotice(to, actorName, label string) error {
	if f.fail {
		return errors.New("relay down")
	}
	f.notices = append(f.notices, to+"|"+label)
	return nil
}

func (f *fakeMailer) SendExpiryReminder(to, label, expires string) error {
	if f.fail {
		return errors.New("relay down")
	}
	f.reminders = append(f.reminders, to+"|"+label)
	return nil
}

var _ mail.Sender = (*fakeMailer)(nil)

func TestMailSendDeliversThroughTheTransport(t *testing.T) {
	w, _ := testWorker(t)
	m := &fakeMailer{}
	w.WithMailer(m)

	enqueue(t, w.meta.(*store.Memory), store.Job{Kind: "mail.send", Payload: map[string]any{
		"to": "friend@example.test", "template": "share_notice",
		"actor": "owner", "label": "Reports",
	}})
	n, err := w.ProcessJobs(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("processed %d jobs, err=%v; want 1, nil", n, err)
	}
	if len(m.notices) != 1 || m.notices[0] != "friend@example.test|Reports" {
		t.Fatalf("delivery: %v", m.notices)
	}
}

func TestMailSendRetriesAFailingTransport(t *testing.T) {
	w, mstore := testWorker(t)
	w.WithMailer(&fakeMailer{fail: true})

	enqueue(t, mstore, store.Job{Kind: "mail.send", Payload: map[string]any{
		"to": "friend@example.test", "template": "share_notice",
	}})
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatalf("process: %v", err)
	}
	pending, _, failed, err := jobCounts(t, w)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("a relay outage was recorded as a PERMANENT failure; it is the retryable case")
	}
	if pending != 1 {
		t.Fatalf("the failed job is not pending for retry: pending=%d", pending)
	}
}

func TestMailSendRefusesAnUnknownTemplatePermanently(t *testing.T) {
	w, mstore := testWorker(t)
	w.WithMailer(&fakeMailer{})

	enqueue(t, mstore, store.Job{Kind: "mail.send", Payload: map[string]any{
		"to": "friend@example.test", "template": "carrier-pigeon",
	}})
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatalf("process: %v", err)
	}
	_, _, failed, err := jobCounts(t, w)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("an unknown template must dead-letter, failed=%d", failed)
	}
}

func jobCounts(t *testing.T, w *Worker) (pending, running, failed int, err error) {
	t.Helper()
	err = w.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		p, r, f, cerr := tx.JobCounts(ctx)
		pending, running, failed = p, r, f
		return cerr
	})
	return
}

func TestMailSendDedupeKeyCollapsesADuplicateQueue(t *testing.T) {
	// The double-send guarantee for a message whose producer retried: both enqueues carry the
	// same dedupe key, so the queue holds one job, and one delivery is all the transport sees.
	// This is the mail half of the idempotency the jobs_test table demands of every kind.
	w, mstore := testWorker(t)
	mailer := &fakeMailer{}
	w.WithMailer(mailer)

	job := store.Job{Kind: "mail.send", DedupeKey: "expiry:fixed",
		Payload: map[string]any{
			"to": "once@example.test", "template": "expiry_reminder",
			"label": "Once only", "expires": "soon",
		}}
	enqueue(t, mstore, job)
	enqueue(t, mstore, job) // the retried producer

	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mailer.reminders) != 1 {
		t.Fatalf("a deduped enqueue delivered %d times, want 1: %v", len(mailer.reminders), mailer.reminders)
	}
	pending, _, failed, err := jobCounts(t, w)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 || failed != 0 {
		t.Fatalf("queue not clean after the deduped pair: pending=%d failed=%d", pending, failed)
	}
}

func TestNotifyPassRemindsExpiringShareOwners(t *testing.T) {
	w, mstore := testWorker(t)
	mailer := &fakeMailer{}
	w.WithMailer(mailer).WithNotify(config.NotifyConfig{ShareExpiryDays: 3, RetentionDays: 90})

	owner := mstore.AddUser(store.User{
		Username: "owner", Email: "owner@example.test", Role: "user",
	})
	var shareID uuid.UUID
	err := mstore.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		root, rerr := tx.EnsureRoot(ctx, owner.ID)
		if rerr != nil {
			return rerr
		}
		soon := time.Now().Add(2 * time.Hour)
		sh, cerr := tx.CreateShare(ctx, store.Share{
			NodeID: root.ID, OwnerID: owner.ID, Kind: store.ShareLink,
			Label: "Quarterly", TokenHash: []byte(uuid.NewString()),
			ExpiresAt: &soon,
		})
		shareID = sh.ID
		return cerr
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.NotifyPass(context.Background()); err != nil {
		t.Fatalf("notify pass: %v", err)
	}

	// The owner's inbox has the reminder, exactly once.
	var kinds []string
	err = mstore.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		items, _, _, gerr := tx.Notifications(ctx, owner.ID, "", 50)
		for _, n := range items {
			kinds = append(kinds, n.Kind)
		}
		return gerr
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 || kinds[0] != store.NotifyLinkExpiring {
		t.Fatalf("owner notifications: %v", kinds)
	}

	// A second pass the same day says nothing new: the dedupe key is the share id.
	if err := w.NotifyPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	err = mstore.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		items, _, _, gerr := tx.Notifications(ctx, owner.ID, "", 50)
		if len(items) != 1 {
			t.Fatalf("second pass produced %d notifications, want the same 1", len(items))
		}
		_ = shareID
		return gerr
	})
	if err != nil {
		t.Fatal(err)
	}

	// And the mail was queued, then delivered by the queue the same worker runs.
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mailer.reminders) != 1 || mailer.reminders[0] != "owner@example.test|Quarterly" {
		t.Fatalf("reminder mail: %v", mailer.reminders)
	}
}

func TestNotifyPassWithoutWindowStaysQuiet(t *testing.T) {
	w, mstore := testWorker(t)
	w.WithNotify(config.NotifyConfig{ShareExpiryDays: 0, RetentionDays: 0})

	owner := mstore.AddUser(store.User{Username: "quiet", Email: "q@example.test", Role: "user"})
	err := mstore.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		root, rerr := tx.EnsureRoot(ctx, owner.ID)
		if rerr != nil {
			return rerr
		}
		soon := time.Now().Add(time.Hour)
		_, cerr := tx.CreateShare(ctx, store.Share{
			NodeID: root.ID, OwnerID: owner.ID, Kind: store.ShareLink,
			TokenHash: []byte(uuid.NewString()), ExpiresAt: &soon,
		})
		return cerr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.NotifyPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var unread int64
	_ = mstore.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, gerr := tx.UnreadNotificationCount(ctx, owner.ID)
		unread = n
		return gerr
	})
	if unread != 0 {
		t.Fatalf("a disabled window still notified: %d", unread)
	}
}
