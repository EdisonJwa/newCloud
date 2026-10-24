package worker_test

// The session-prune duty.
//
// The sessions table had exactly one writer and no reaper: one row per login, kept forever,
// including the address and agent of every login ever made. The duty is only useful if it
// removes the right rows — dead ones past the horizon — and keeps the live ones, so this test
// builds one of each and advances the clock past the retention window.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

func sessionPruneFixture(t *testing.T) (*worker.Worker, *store.Memory, context.Context) {
	t.Helper()
	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "prune", Email: "prune@example.test", Role: "user"})
	ctx := context.Background()
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// Live: expires far enough out that advancing the clock by the retention window
		// cannot reach it.
		if err := tx.CreateSession(ctx, store.Session{
			ID: "live", UserID: user.ID, ExpiresAt: meta.Now().Add(400 * 24 * time.Hour),
		}); err != nil {
			return err
		}
		// Expired: died an hour ago, so the window has long passed once the clock moves.
		if err := tx.CreateSession(ctx, store.Session{
			ID: "expired", UserID: user.ID, ExpiresAt: meta.Now().Add(-time.Hour),
		}); err != nil {
			return err
		}
		// Revoked: still unexpired, but explicitly killed.
		if err := tx.CreateSession(ctx, store.Session{
			ID: "revoked", UserID: user.ID, ExpiresAt: meta.Now().Add(400 * 24 * time.Hour),
		}); err != nil {
			return err
		}
		return tx.RevokeSession(ctx, "revoked")
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return w, meta, ctx
}

func TestSessionPruneRemovesDeadRowsAndKeepsLiveOnes(t *testing.T) {
	w, meta, ctx := sessionPruneFixture(t)

	// Inside the retention window nothing goes: a row that died five minutes ago is kept,
	// because the window exists to answer "who was signed in when this happened".
	n, err := w.PruneSessions(ctx)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 0 {
		t.Fatalf("pruning removed %d row(s) before the retention horizon passed", n)
	}

	// Cross the horizon: everything dead is now retention-stale.
	meta.AdvanceForTest(store.SessionRetention + 2*time.Hour)
	n, err = w.PruneSessions(ctx)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("prune removed %d rows, want 2 (the expired one and the revoked one)", n)
	}

	// And the survivor is still a working credential record, not collateral damage.
	err = meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		live, lerr := tx.SessionLive(ctx, "live")
		if lerr != nil {
			return lerr
		}
		if !live {
			t.Fatal("the live session did not survive the prune")
		}
		if dead, derr := tx.SessionLive(ctx, "expired"); derr != nil || dead {
			t.Fatalf("the expired session is live=%v err=%v after the prune", dead, derr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
