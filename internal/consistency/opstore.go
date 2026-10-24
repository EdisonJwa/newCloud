package consistency

import (
	"context"
	"io"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// StagingStore is the operation-scoped storage contract from ADR-0013 §6.2.
// Blob implements publish as metadata/resolver work; Tree uses the platform
// no-replace primitive. Implementations must make staging unreachable through
// every live address.
type StagingStore interface {
	// StagePut writes bytes into the operation's private namespace.
	StagePut(ctx context.Context, opID uuid.UUID, rel string, r io.Reader, size int64) (layout.PutResult, error)
	// StageMkdir creates a private directory (and parents).
	StageMkdir(ctx context.Context, opID uuid.UUID, rel string) error
	// PublishNoReplace atomically installs a staged path only when the final path
	// is free. It fails closed if the platform cannot prove no-replace.
	PublishNoReplace(ctx context.Context, opID uuid.UUID, stagedRel string, final layout.Node) error
	// PublishReplace moves the old final path to operation-private backup and
	// installs the staged path. The backup is retained for recovery.
	PublishReplace(ctx context.Context, opID uuid.UUID, stagedRel string, final, backup layout.Node) error
	// Discard removes the operation's private stage and never a live path.
	Discard(ctx context.Context, opID uuid.UUID) error
}
