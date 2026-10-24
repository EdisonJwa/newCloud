package blobstore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/consistency"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

type noopResolver struct{}

func (noopResolver) HashFor(context.Context, string, string) (blob.Hash, bool, error) {
	return blob.Hash{}, false, nil
}
func (noopResolver) Record(context.Context, string, string, blob.Hash, bool) error { return nil }
func (noopResolver) Forget(context.Context, string, string) error                  { return nil }
func (noopResolver) Children(context.Context, string, string, int) ([]layout.Node, error) {
	return nil, nil
}
func (noopResolver) IsDir(context.Context, string, string) (bool, bool, error) {
	return false, false, nil
}

func TestBlobStagePublishesNoReplaceAndNeverExposesBeforeCommit(t *testing.T) {
	ctx := context.Background()
	blobs, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(blobs, noopResolver{})
	if err != nil {
		t.Fatal(err)
	}
	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	adapter = adapter.WithPublisher(meta)

	op := uuid.New()
	beginPublishing(t, meta, op, user.ID)
	if _, err := adapter.StagePut(ctx, op, "a.txt", bytes.NewReader([]byte("payload")), 7); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := meta.Resolve(ctx, user.ID, "a.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("staged blob is visible before publish: %v", err)
	}
	final := layout.Node{Owner: user.ID.String(), Path: "a.txt"}
	if err := adapter.PublishNoReplace(ctx, op, "a.txt", final); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := meta.Resolve(ctx, user.ID, "a.txt"); err != nil {
		t.Fatalf("published node missing: %v", err)
	}

	// A second operation cannot replace the committed name by no-replace.
	op2 := uuid.New()
	beginPublishing(t, meta, op2, user.ID)
	if _, err := adapter.StagePut(ctx, op2, "a.txt", bytes.NewReader([]byte("other")), 5); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishNoReplace(ctx, op2, "a.txt", final); !errors.Is(err, consistency.ErrPublishExists) {
		t.Fatalf("second no-replace publish = %v, want ErrPublishExists", err)
	}

	live, err := meta.Resolve(ctx, user.ID, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	op3 := uuid.New()
	beginPublishing(t, meta, op3, user.ID)
	if _, err := adapter.StagePut(ctx, op3, "a.txt", bytes.NewReader([]byte("better")), 6); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishReplaceExpected(ctx, op3, "a.txt", final, `"wrong"`); err == nil {
		t.Fatal("replace with a stale ETag succeeded")
	}
	if err := adapter.PublishReplaceExpected(ctx, op3, "a.txt", final, live[len(live)-1].ETag); err != nil {
		t.Fatalf("safe replace: %v", err)
	}
	updated, err := meta.Resolve(ctx, user.ID, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if updated[len(updated)-1].ETag == live[len(live)-1].ETag {
		t.Fatal("replace did not change the ETag")
	}
}

func TestBlobStageDiscardIsOperationScoped(t *testing.T) {
	ctx := context.Background()
	blobs, _ := local.New(t.TempDir())
	adapter, _ := New(blobs, noopResolver{})
	adapter = adapter.WithPublisher(store.NewMemory())
	a, b := uuid.New(), uuid.New()
	if _, err := adapter.StagePut(ctx, a, "a", bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.StagePut(ctx, b, "b", bytes.NewReader([]byte("b")), 1); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Discard(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishNoReplace(ctx, a, "a", layout.Node{}); !errors.Is(err, layout.ErrNotFound) {
		t.Fatalf("discarded operation published: %v", err)
	}
}

func beginPublishing(t *testing.T, meta *store.Memory, opID, owner uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := meta.Create(ctx, consistency.Operation{ID: opID, OwnerID: owner, Kind: "upload"}); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.Transition(ctx, opID, consistency.StateIntent, consistency.StateStaging); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.Transition(ctx, opID, consistency.StateStaging, consistency.StatePublishing); err != nil {
		t.Fatal(err)
	}
}
