package consistency

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrPublishExists means a no-replace publish found the destination occupied.
var ErrPublishExists = errors.New("consistency: publish destination exists")

// ErrPublishOperation identifies a publish that is not bound to a durable
// operation. A metadata-only publish without a journal row is unrecoverable.
var ErrPublishOperation = errors.New("consistency: publish requires a journaled operation")

// ErrPublishEpoch identifies a PostgreSQL publish that did not carry the lease
// epoch. Production publish must be fenced against a stale owner.
var ErrPublishEpoch = errors.New("consistency: publish requires the current lease epoch")

// BlobPublish is the metadata half of a Blob-layout publish. The bytes are
// already in the content-addressed store under Hash; this request creates or
// replaces the live node with one conditional metadata transaction.
type BlobPublish struct {
	OperationID uuid.UUID
	// OperationKind must match the journal row. This prevents completing a COPY
	// or DELETE operation with an unrelated upload handler's operation id.
	OperationKind string
	Owner         string
	Path          string
	Hash          [32]byte
	Size          int64
	IsDir         bool
	// EpochSet distinguishes "no epoch supplied" from the valid initial epoch 0.
	EpochSet bool
	// ExpectedEpoch must be the current singleton lease epoch for a PostgreSQL
	// publish. The check and the visibility change share one transaction.
	ExpectedEpoch uint64
	// Replace requires the current destination to match ExpectedETag. A
	// no-replace publish sets Replace=false and must fail when the name exists.
	Replace      bool
	ExpectedETag string
}

// BlobPublisher is implemented by the metadata store. It is deliberately
// separate from ContentStore: Blob publication is a metadata decision, not a
// path rename.
type BlobPublisher interface {
	PublishBlob(ctx context.Context, req BlobPublish) error
}
