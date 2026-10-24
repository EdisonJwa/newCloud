package server_test

// The delete path records its intent and then closes it.
//
// The journal is only useful if the row is written before the content moves and closed after the
// namespace commits: a row left open by a successful delete sends the recovery pass chasing work that
// is already done, and a delete with no row at all leaves a crash in the middle unexplained.

import (
	"context"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestADeleteRecordsAndClosesItsJournalEntry(t *testing.T) {
	h := newHarness(t)
	node := h.uploadFile(t, "journal-gone.txt", "bytes")

	resp := h.do(http.MethodDelete, "/api/files?path=journal-gone.txt", nil, nil)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}

	// The row exists, names the node, and is committed rather than left open.
	var ops []store.Op
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.OpsForNode(ctx, node.ID)
		ops = list
		return lerr
	}); err != nil {
		t.Fatalf("OpsForNode: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("the delete recorded %d operation(s) for the node, want 1: without a row, a crash "+
			"in the middle of a delete leaves nothing to repair from", len(ops))
	}
	if ops[0].Kind != "delete" {
		t.Fatalf("the operation is a %q, want a delete", ops[0].Kind)
	}
	if ops[0].State != store.OpCommitted {
		t.Fatalf("the operation ended in %q, want COMMITTED: the recovery pass would try to repair "+
			"a delete that already succeeded", ops[0].State)
	}

	// And nothing is left for recovery to chase.
	var open []store.Op
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.OpenOps(ctx, 100)
		open = list
		return lerr
	}); err != nil {
		t.Fatalf("OpenOps: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("a completed delete left %d operation(s) open", len(open))
	}
}
