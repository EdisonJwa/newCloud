package worker

// The recovery pass settles what the node's own state proves, and escalates what it does not.
//
// The distinction is the whole point: a pass that guessed would be the delete-ordering mistake again
// with a timer on it, and a pass that did nothing would let a crashed delete sit invisible forever.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func newRecoveryWorker(t *testing.T) (*Worker, *store.Memory) {
	t.Helper()
	meta := store.NewMemory()
	// A worker with no content store: recovery deliberately does not touch content, so a test that
	// needed one would be testing something this pass does not do.
	w := New(meta, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.recoveryGrace = 0 // every open operation is old, by construction
	return w, meta
}

func TestRecoveryResolvesAFinishedDeleteAndEscalatesALiveOne(t *testing.T) {
	w, meta := newRecoveryWorker(t)
	ctx := context.Background()

	var owner uuid.UUID
	var finished, unfinished uuid.UUID
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, _ := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "recovery", Username: "recovery",
			Email: "recovery@example.test", Role: "user",
		})
		owner = u.ID
		root, rerr := tx.EnsureRoot(ctx, owner)
		if rerr != nil {
			return rerr
		}
		gone, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "finished.txt",
			Size: 4, Mime: "text/plain", BlobID: uuid.Nil,
		})
		if cerr != nil {
			return cerr
		}
		finished = gone.ID
		live, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "unfinished.txt",
			Size: 4, Mime: "text/plain", BlobID: uuid.Nil,
		})
		if cerr != nil {
			return cerr
		}
		unfinished = live.ID

		// The delete of "finished.txt" got as far as the namespace step, then the process died.
		if _, oerr := tx.StartOp(ctx, store.Op{Kind: "delete", OwnerID: owner, StagedRoot: &finished}); oerr != nil {
			return oerr
		}
		if terr := tx.Trash(ctx, finished); terr != nil {
			return terr
		}

		// The delete of "unfinished.txt" recorded intent and never touched the namespace.
		_, oerr := tx.StartOp(ctx, store.Op{Kind: "delete", OwnerID: owner, StagedRoot: &unfinished})
		return oerr
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resolved, escalated, err := w.RecoverOperations(ctx)
	if err != nil {
		t.Fatalf("RecoverOperations: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("resolved %d operations, want 1: a delete that reached the trash is finished, and "+
			"leaving it open would have recovery chase completed work on every pass", resolved)
	}
	if escalated != 1 {
		t.Fatalf("escalated %d operations, want 1: a live node mid-delete is the case recovery must "+
			"not decide on its own", escalated)
	}

	var open []store.Op
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.OpenOps(ctx, 10)
		open = list
		return lerr
	}); err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("%d operation(s) still open after recovery: a row that stays open is a row the "+
			"pass will re-examine forever", len(open))
	}

	// And the escalated one is escalated, not quietly closed: an operator has to see it.
	var escalatedCount int
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		ops, oerr := tx.OpsForNode(ctx, unfinished)
		for _, o := range ops {
			if o.State == store.OpNeedsOperator {
				escalatedCount++
			}
		}
		return oerr
	}); err != nil {
		t.Fatal(err)
	}
	if escalatedCount != 1 {
		t.Fatalf("the unresolved operation is in %d NEEDS_OPERATOR row(s), want 1", escalatedCount)
	}
}
