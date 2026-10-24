package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/consistency"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestPGConsistencyPrimitives is the real-PostgreSQL gate for the Phase 1
// journal, revision CAS and G2 visibility. It is skipped when TEST_DATABASE_URL
// is absent; a mocked pass is not evidence for this gate.
func TestPGConsistencyPrimitives(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	root, err := s.EnsureRoot(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	var child store.Node
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: root.ID, Name: "docs"})
		child = n
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if got, err := s.Current(ctx, child.ID); err != nil || got != 1 {
		t.Fatalf("new node revision = %d, %v; want 1", got, err)
	}
	if err := s.CAS(ctx, child.ID, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.CAS(ctx, child.ID, 1, 2); !errors.Is(err, consistency.ErrRevisionConflict) {
		t.Fatalf("stale CAS = %v, want revision conflict", err)
	}

	opID := uuid.New()
	if _, err := s.Create(ctx, consistency.Operation{ID: opID, OwnerID: owner, Kind: "copy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, consistency.Operation{ID: opID, OwnerID: owner, Kind: "copy"}); !errors.Is(err, consistency.ErrOperationExists) {
		t.Fatalf("duplicate operation = %v", err)
	}
	if _, err := s.Transition(ctx, opID, consistency.StateIntent, consistency.StateStaging); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, opID, consistency.StateIntent, consistency.StateStaging); !errors.Is(err, consistency.ErrStateConflict) {
		t.Fatalf("stale transition = %v", err)
	}

	ok, err := consistency.EffectiveExternalVisibility(ctx, s, child.ID)
	if err != nil || !ok {
		t.Fatalf("live child visible = %v, %v", ok, err)
	}
	if err := s.SetStagedBy(ctx, root.ID, &opID); err != nil {
		t.Fatal(err)
	}
	ok, err = consistency.EffectiveExternalVisibility(ctx, s, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a child of a staged root was externally visible in PostgreSQL (G2)")
	}
	if err := s.SetStagedBy(ctx, root.ID, nil); err != nil {
		t.Fatal(err)
	}

	// Epoch fencing is checked under a row lock in the publishing transaction.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var epoch uint64
	if err := tx.QueryRow(ctx, `SELECT epoch FROM consistency_lease WHERE id = 1`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckEpochInTx(ctx, tx, epoch); err != nil {
		t.Fatalf("current epoch rejected: %v", err)
	}
	if err := s.CheckEpochInTx(ctx, tx, epoch+1); !errors.Is(err, consistency.ErrLeaseStale) {
		t.Fatalf("stale epoch = %v, want ErrLeaseStale", err)
	}
	_ = tx.Rollback(ctx)

	// Blob publication is a conditional metadata transaction, not a path rename.
	var current uint64
	if err := pool.QueryRow(ctx, `SELECT epoch FROM consistency_lease WHERE id = 1`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte("blob-publish"))
	opID = beginPublishOp(t, s, owner, "upload")
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: owner.String(), Path: "published.txt",
		Hash: h, Size: 12, EpochSet: true, ExpectedEpoch: current,
	}); err != nil {
		t.Fatalf("blob publish: %v", err)
	}
	opID = beginPublishOp(t, s, owner, "upload")
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: owner.String(), Path: "published.txt",
		Hash: h, Size: 12, EpochSet: true, ExpectedEpoch: current,
	}); !errors.Is(err, consistency.ErrPublishExists) {
		t.Fatalf("second blob no-replace = %v, want ErrPublishExists", err)
	}
	if _, err := s.Resolve(ctx, owner, "published.txt"); err != nil {
		t.Fatalf("published blob node missing: %v", err)
	}

	opID = beginPublishOp(t, s, owner, "upload")
	h2 := sha256.Sum256([]byte("atomic-publish"))
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: owner.String(), Path: "atomic.txt",
		Hash: h2, Size: 14, EpochSet: true, ExpectedEpoch: current,
	}); err != nil {
		t.Fatal(err)
	}
	op, err := s.Get(ctx, opID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != consistency.StateCommitted {
		t.Fatalf("journal state = %s, want COMMITTED in the same transaction", op.State)
	}

	if err := pool.QueryRow(ctx, `UPDATE consistency_lease SET epoch = epoch + 1 WHERE id = 1 RETURNING epoch`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	opID = beginPublishOp(t, s, owner, "upload")
	h3 := sha256.Sum256([]byte("epoch-publish"))
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: owner.String(), Path: "epoch.txt", Hash: h3, Size: 12,
		EpochSet: true, ExpectedEpoch: current + 1,
	}); !errors.Is(err, consistency.ErrLeaseStale) {
		t.Fatalf("stale-epoch publish = %v, want ErrLeaseStale", err)
	}
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: owner.String(), Path: "epoch.txt", Hash: h3, Size: 12,
		EpochSet: true, ExpectedEpoch: current,
	}); err != nil {
		t.Fatalf("current-epoch publish: %v", err)
	}

	// A publish cannot complete a COPY/DELETE operation id by guessing.
	opID = beginPublishOp(t, s, owner, "copy")
	h4 := sha256.Sum256([]byte("wrong-kind"))
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: owner.String(), Path: "wrong-kind.txt", Hash: h4, Size: 10, EpochSet: true,
		ExpectedEpoch: current,
	}); err == nil {
		t.Fatal("publish completed a COPY operation as an upload")
	}
	if _, err := s.Resolve(ctx, owner, "wrong-kind.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed wrong-kind publish left a visible node: %v", err)
	}

	opID = beginPublishOp(t, s, owner, "upload")
	h5 := sha256.Sum256([]byte("missing-epoch"))
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: owner.String(), Path: "missing-epoch.txt", Hash: h5, Size: 13,
	}); !errors.Is(err, consistency.ErrPublishEpoch) {
		t.Fatalf("missing-epoch publish = %v, want ErrPublishEpoch", err)
	}
	if err := s.PublishBlob(ctx, consistency.BlobPublish{
		OperationKind: "upload", Owner: owner.String(), Path: "missing-op.txt", Hash: h5, Size: 13,
		EpochSet: true, ExpectedEpoch: current,
	}); !errors.Is(err, consistency.ErrPublishOperation) {
		t.Fatalf("missing-operation publish = %v, want ErrPublishOperation", err)
	}
}

func beginPublishOp(t *testing.T, s *store.PG, owner uuid.UUID, kind string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := s.Create(ctx, consistency.Operation{ID: id, OwnerID: owner, Kind: kind}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, id, consistency.StateIntent, consistency.StateStaging); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, id, consistency.StateStaging, consistency.StatePublishing); err != nil {
		t.Fatal(err)
	}
	return id
}
