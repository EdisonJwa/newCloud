package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/consistency"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

var errBlobReplaceNeedsETag = errors.New("store: blob replacement requires an expected ETag")

// PublishBlob is the metadata half of a Blob publish. The content reference is
// already durable; this transaction is the visibility point.
func (m *Memory) PublishBlob(ctx context.Context, req consistency.BlobPublish) error {
	if req.EpochSet {
		return fmt.Errorf("store: epoch fencing is only available on a leased PostgreSQL store")
	}
	if err := requirePublishOperation(req); err != nil {
		return err
	}
	return m.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		return publishBlobTx(ctx, tx, req)
	})
}

func (p *PG) PublishBlob(ctx context.Context, req consistency.BlobPublish) error {
	if err := requirePublishOperation(req); err != nil {
		return err
	}
	if !req.EpochSet {
		return consistency.ErrPublishEpoch
	}
	return p.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		pt, ok := tx.(*pgTx)
		if !ok {
			return fmt.Errorf("store: epoch check requires a PostgreSQL transaction")
		}
		if err := p.CheckEpochInTx(ctx, pt.tx, req.ExpectedEpoch); err != nil {
			return err
		}
		return publishBlobTx(ctx, tx, req)
	})
}

func requirePublishOperation(req consistency.BlobPublish) error {
	if req.OperationID == uuid.Nil || req.OperationKind == "" {
		return consistency.ErrPublishOperation
	}
	return nil
}

var _ consistency.BlobPublisher = (*Memory)(nil)
var _ consistency.BlobPublisher = (*PG)(nil)

// PublishEpoch reports whether this backend requires epoch fencing and, if so,
// the current lease epoch. The shared conformance suite uses it to set
// BlobPublish.EpochSet only on the PostgreSQL backend.
func (m *Memory) PublishEpoch(ctx context.Context) (uint64, bool, error) {
	return 0, false, nil
}

func (p *PG) PublishEpoch(ctx context.Context) (uint64, bool, error) {
	var epoch uint64
	if err := p.pool.QueryRow(ctx, `SELECT epoch FROM consistency_lease WHERE id = 1`).Scan(&epoch); err != nil {
		return 0, false, fmt.Errorf("store: read lease epoch: %w", err)
	}
	return epoch, true, nil
}

func publishBlobTx(ctx context.Context, tx Tx, req consistency.BlobPublish) error {
	ownerID, err := uuid.Parse(req.Owner)
	if err != nil {
		return fmt.Errorf("%w: owner id", layout.ErrInvalidNode)
	}
	if _, err := layout.ValidatePath(req.Path); err != nil {
		return err
	}
	segs := []string{}
	if req.Path != "" {
		segs = strings.Split(req.Path, "/")
	}
	if len(segs) == 0 {
		return fmt.Errorf("%w: a file needs a name", layout.ErrInvalidNode)
	}
	root, err := tx.EnsureRoot(ctx, ownerID)
	if err != nil {
		return err
	}
	parent := root
	for _, seg := range segs[:len(segs)-1] {
		next, err := tx.ChildByName(ctx, parent.ID, seg)
		if err != nil {
			return err
		}
		if !next.IsDir() {
			return fmt.Errorf("%w: parent is not a directory", layout.ErrInvalidNode)
		}
		parent = next
	}
	name := segs[len(segs)-1]

	if req.IsDir {
		if req.Replace {
			return fmt.Errorf("%w: directory replacement is not a blob publish", layout.ErrInvalidNode)
		}
		node, err := tx.CreateDir(ctx, NewNode{OwnerID: ownerID, ParentID: parent.ID, Name: name})
		if errors.Is(err, ErrConflict) {
			return consistency.ErrPublishExists
		}
		if err != nil {
			return err
		}
		if err := tx.Audit(ctx, blobPublishAudit(req, node.ID.String(), "dir")); err != nil {
			return err
		}
		return tx.CompleteOperation(ctx, req.OperationID, ownerID, req.OperationKind)
	}

	if req.Replace {
		if req.ExpectedETag == "" {
			return errBlobReplaceNeedsETag
		}
	}
	hash := req.Hash
	blobID, err := blobIDForTx(ctx, tx, hash, req.Size)
	if err != nil {
		return err
	}
	etag := ETagForHash(hash)
	expect := &Precondition{}
	if req.Replace {
		expect.IfMatch = req.ExpectedETag
	} else {
		expect.IfNoneMatch = "*"
	}
	node, _, err := tx.WriteFile(ctx, WriteFile{
		NewFile: NewFile{
			OwnerID: ownerID, ParentID: parent.ID, Name: name,
			Size: req.Size, ETag: etag, BlobID: blobID, BlobHash: hash,
		},
		Expect: expect,
	})
	if errors.Is(err, ErrPrecondition) {
		if !req.Replace {
			return consistency.ErrPublishExists
		}
		return err
	}
	if err != nil {
		return err
	}
	if err := tx.Audit(ctx, blobPublishAudit(req, node.ID.String(), "file")); err != nil {
		return err
	}
	return tx.CompleteOperation(ctx, req.OperationID, ownerID, req.OperationKind)
}

func blobPublishAudit(req consistency.BlobPublish, targetID, kind string) AuditEntry {
	return AuditEntry{
		ActorKind:  "system",
		Action:     "file.write",
		TargetKind: kind, TargetID: targetID,
		Result: "ok", Bytes: &req.Size,
		Metadata: map[string]any{
			"operation": req.OperationID.String(),
			"publish":   "replaced", "name": req.Path,
		},
	}
}

func blobIDForTx(ctx context.Context, tx Tx, hash [32]byte, size int64) (uuid.UUID, error) {
	switch t := tx.(type) {
	case *memTx:
		return t.blobID(hash), nil
	case *pgTx:
		id, err := t.BlobID(ctx, hash, size)
		if err != nil {
			return uuid.Nil, err
		}
		return id, nil
	default:
		return uuid.Nil, fmt.Errorf("store: unsupported tx type %T for blob publish", tx)
	}
}
