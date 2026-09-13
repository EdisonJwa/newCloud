package store_test

// T-16: behaviour when audit storage is unavailable or the disk is full.
//
// The requirement asks that audited operations "遵循事先约定的 fail-open/fail-closed 策略" — follow a
// previously agreed fail-open/fail-closed policy. The policy this server implements is
// **fail-closed**: an operation that cannot be recorded does not commit.
//
// That is the right choice here and worth stating explicitly, because the alternative is
// tempting: fail-open keeps the file service available during an audit outage, which sounds
// kinder to users. But the audit log is a security control (INV-09), and a platform whose
// tamper-evident log silently stops recording under load or disk pressure is worse than one that
// refuses the write — an attacker's cheapest move would be to fill the disk. The cost is real
// (an audit outage means no uploads, no deletes, no permission changes) and is documented for
// operators.
//
// What must hold, and what these tests pin:
//
//  1. The mutation rolls back. No partially applied change survives.
//  2. The client gets an error it can act on, not a silent success.
//  3. Reads keep working. An audit outage is not a full outage: existing data stays accessible,
//     so an operator can still investigate why writes are failing.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// failingAuditWriter simulates an audit backend that rejects every append, which is what a full
// disk or an unavailable audit table looks like from the store's side.
type failingAuditWriter struct {
	err error
	// calls counts how many appends were attempted, so a test can prove the writer was reached
	// rather than the operation failing for an unrelated reason.
	calls int
}

func (f *failingAuditWriter) Append(ctx context.Context, tx store.Tx, e store.AuditEntry) error {
	f.calls++
	return f.err
}

var errAuditUnavailable = errors.New("audit: no space left on device")

// pgWithFailingAudit wires a store whose audit writes always fail.
func pgWithFailingAudit(t *testing.T, pool *pgxpool.Pool) (*store.PG, *failingAuditWriter) {
	t.Helper()
	w := &failingAuditWriter{err: errAuditUnavailable}
	s := store.NewPG(pool)
	s.SetAuditWriter(w)
	return s, w
}

// TestAuditFailureRollsBackTheMutation is the fail-closed guarantee.
//
// The file must not exist afterwards. If it did, the platform would have accepted a change it
// cannot account for, and the audit log would be a partial fiction — which is precisely the state
// INV-09 exists to prevent.
func TestAuditFailureRollsBackTheMutation(t *testing.T) {
	pool := testPool(t)
	owner, root := seedOwnerAndRoot(t, pool)

	s, w := pgWithFailingAudit(t, pool)
	ctx := context.Background()

	err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, cerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: owner, ParentID: root, Name: "unaudited",
		}); cerr != nil {
			return cerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &owner, ActorEmail: "a@example.test",
			ActorIP: "127.0.0.1", UserAgent: "test",
			Action: "file.write", TargetKind: "node", TargetID: root.String(), Result: "ok",
		})
	})
	if err == nil {
		t.Fatal("an operation that cannot be audited must fail, not commit silently")
	}
	if w.calls == 0 {
		t.Fatal("the audit writer must have been attempted; otherwise this proves nothing")
	}
	if !errors.Is(err, errAuditUnavailable) {
		t.Fatalf("the underlying cause must be preserved for the operator, got %v", err)
	}

	// The directory is gone: the transaction rolled back.
	var exists bool
	if qerr := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM nodes WHERE parent_id = $1::uuid AND name = 'unaudited')`,
		root).Scan(&exists); qerr != nil {
		t.Fatal(qerr)
	}
	if exists {
		t.Fatal("the mutation must be rolled back when the audit write fails")
	}
}

// TestReadsSurviveAnAuditOutage: fail-closed applies to writes.
//
// An operator investigating "writes are failing" needs to read the data and the audit log. Making
// reads fail too would turn a write outage into a total one.
func TestReadsSurviveAnAuditOutage(t *testing.T) {
	pool := testPool(t)
	owner, root := seedOwnerAndRoot(t, pool)

	s, _ := pgWithFailingAudit(t, pool)
	ctx := context.Background()

	// A read inside a transaction, which is how the handlers do it.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, rerr := tx.Node(ctx, root)
		return rerr
	}); err != nil {
		t.Fatalf("reads must keep working while audit is down: %v", err)
	}

	// And resolution through the ordinary path.
	if _, err := s.Resolve(ctx, owner, ""); err != nil {
		t.Fatalf("resolving a path must keep working: %v", err)
	}
}

// TestMissingAuditWriterIsAlsoFailClosed covers the other half: a store that was never wired with
// a writer must refuse audited operations rather than quietly proceeding. This is the
// misconfiguration case — the operator forgot the wiring, and the failure has to be loud.
func TestMissingAuditWriterIsAlsoFailClosed(t *testing.T) {
	pool := testPool(t)
	owner, root := seedOwnerAndRoot(t, pool)

	s := store.NewPG(pool) // no SetAuditWriter
	ctx := context.Background()

	err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &owner, ActorEmail: "a@example.test",
			ActorIP: "127.0.0.1", UserAgent: "test",
			Action: "file.write", TargetKind: "node", TargetID: root.String(), Result: "ok",
		})
	})
	if err == nil {
		t.Fatal("an unwired audit writer must fail loudly, not drop the record")
	}
}

// hangingAuditWriter blocks until its context is cancelled, which is what a wedged audit store
// looks like from the caller's side: not an error, just nothing happening.
type hangingAuditWriter struct{ calls int }

func (h *hangingAuditWriter) Append(ctx context.Context, _ store.Tx, _ store.AuditEntry) error {
	h.calls++
	<-ctx.Done()
	return ctx.Err()
}

// TestSlowAuditStoreIsBoundedNotUnbounded is the gap the policy document used to list as uncovered:
// "the audit subsystem becoming slow rather than unavailable ... there is no timeout-and-fail
// behaviour today".
//
// The append runs inside the caller's transaction, so a hung insert holds that transaction open
// for as long as the database cares to. Every write on the platform would queue behind the
// recorder — the component whose purpose is to observe writes becomes the thing that stops them.
//
// With a bound, the outcome is the already-agreed one: the append fails, the mutation rolls back,
// the client is told 503. This test proves the bound is applied to the writer's context at all,
// which is the only thing that makes it enforceable against a real database.
func TestSlowAuditStoreIsBoundedNotUnbounded(t *testing.T) {
	pool := testPool(t)
	owner, root := seedOwnerAndRoot(t, pool)

	s := store.NewPG(pool)
	w := &hangingAuditWriter{}
	s.SetAuditWriter(w)
	// Short so the test does not take five seconds; the mechanism is identical.
	s.SetAuditAppendTimeoutForTest(50 * time.Millisecond)

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.CreateDir(ctx, store.NewNode{
				OwnerID: owner, ParentID: root, Name: "blocked-by-a-hung-audit",
			}); err != nil {
				return err
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: &owner,
				Action: "file.create", TargetKind: "node", Result: "ok",
			})
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an append that never returned was treated as success")
		}
		if !errors.Is(err, store.ErrAuditUnavailable) {
			t.Fatalf("want ErrAuditUnavailable so the client gets 503 and the operator gets a "+
				"diagnosis, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the audit append was not bounded: the write is still waiting on the recorder. " +
			"One wedged insert would then stall every write on the platform, indefinitely.")
	}
	if w.calls != 1 {
		t.Fatalf("the writer was reached %d times; want exactly 1", w.calls)
	}
}

// deadlineProbe records the context the writer was handed, and returns immediately.
type deadlineProbe struct {
	called   bool
	deadline bool
	budget   time.Duration
}

func (p *deadlineProbe) Append(ctx context.Context, _ store.Tx, _ store.AuditEntry) error {
	p.called = true
	if d, ok := ctx.Deadline(); ok {
		p.deadline = true
		p.budget = time.Until(d)
	}
	return nil
}

// TestAuditAppendIsGivenABoundedContext pins the mechanism rather than its consequence.
//
// The behavioural test above proves a hung writer resolves into a refusal — but when the bound is
// missing that test HANGS instead of failing, because the blocked goroutine pins a pool connection
// until the harness times out. A regression that presents as a sixty-second timeout with no message
// is nearly useless during a bisect.
//
// This one asks the direct question: was the writer handed a context with a deadline? If not, the
// bound is not applied to anything and no amount of downstream behaviour can enforce it.
func TestAuditAppendIsGivenABoundedContext(t *testing.T) {
	pool := testPool(t)
	owner, root := seedOwnerAndRoot(t, pool)

	s := store.NewPG(pool)
	p := &deadlineProbe{}
	s.SetAuditWriter(p)

	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if _, cerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: owner, ParentID: root, Name: "deadline-probe",
		}); cerr != nil {
			return cerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &owner,
			Action: "file.create", TargetKind: "node", Result: "ok",
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	if !p.called {
		t.Fatal("the audit writer was never reached; this test proves nothing")
	}
	if !p.deadline {
		t.Fatal("the audit append was given a context with no deadline, so a wedged insert would " +
			"hold the caller's transaction open indefinitely — every write on the platform would " +
			"queue behind the recorder.")
	}
	if p.budget <= 0 || p.budget > store.DefaultAuditAppendTimeout {
		t.Fatalf("the audit append's budget is %v, outside (0, %v]", p.budget,
			store.DefaultAuditAppendTimeout)
	}
}
