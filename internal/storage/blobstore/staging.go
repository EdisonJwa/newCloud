package blobstore

import (
	"context"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/consistency"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

type stageKey struct {
	op  uuid.UUID
	rel string
}

type stagedContent struct {
	hash  blob.Hash
	size  int64
	isDir bool
}

// StagePut hashes and stores bytes without binding them to a live path. The
// content-addressed store is already addressable only by hash, so this stage is
// invisible to path resolution until PublishNoReplace/PublishReplace commits the
// metadata node.
func (s *Store) StagePut(ctx context.Context, opID uuid.UUID, rel string, r io.Reader, size int64) (layout.PutResult, error) {
	if err := validateStage(opID, rel); err != nil {
		return layout.PutResult{}, err
	}
	hasher := blob.NewHasher()
	spool, h, err := spoolToTemp(s.spoolDir, r, hasher)
	if err != nil {
		return layout.PutResult{}, err
	}
	defer spool.Close()
	if err := s.blobs.Put(ctx, h, spool, size); err != nil {
		return layout.PutResult{}, err
	}
	info, err := s.blobs.Stat(ctx, h)
	if err != nil {
		return layout.PutResult{}, err
	}
	var sum [32]byte
	copy(sum[:], h[:])
	s.stageMu.Lock()
	s.staged[stageKey{op: opID, rel: rel}] = stagedContent{hash: h, size: info.Size}
	s.stageMu.Unlock()
	return layout.PutResult{Hash: sum, Size: info.Size}, nil
}

// StageMkdir records a private directory marker. Blob directories exist only in
// metadata, so there are no physical bytes to create.
func (s *Store) StageMkdir(ctx context.Context, opID uuid.UUID, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateStage(opID, rel); err != nil {
		return err
	}
	s.stageMu.Lock()
	s.staged[stageKey{op: opID, rel: rel}] = stagedContent{isDir: true}
	s.stageMu.Unlock()
	return nil
}

// PublishNoReplace commits the staged metadata only if the destination name is
// free. The conditional decision lives in the metadata transaction; this method
// never does a Stat-then-Record in the storage adapter.
func (s *Store) PublishNoReplace(ctx context.Context, opID uuid.UUID, stagedRel string, final layout.Node) error {
	return s.publish(ctx, opID, stagedRel, final, false, "")
}

// PublishReplace commits the staged content over an existing node whose ETag must
// match the operation's expectation.
func (s *Store) PublishReplace(ctx context.Context, opID uuid.UUID, stagedRel string, final, backup layout.Node) error {
	// The generic StagingStore signature has no ETag expectation. A replace
	// without one must fail rather than overwrite an unverified destination.
	_ = backup
	return s.PublishReplaceExpected(ctx, opID, stagedRel, final, "")
}

// PublishReplaceExpected is the Blob-layout safe replacement: the caller supplies
// the live ETag it observed, and the metadata publisher checks it in the same
// transaction as the write.
func (s *Store) PublishReplaceExpected(ctx context.Context, opID uuid.UUID, stagedRel string, final layout.Node, expectedETag string) error {
	return s.publish(ctx, opID, stagedRel, final, true, expectedETag)
}

func (s *Store) publish(ctx context.Context, opID uuid.UUID, stagedRel string, final layout.Node, replace bool, expectedETag string) error {
	if s.publisher == nil {
		return fmt.Errorf("blobstore: no metadata publisher configured")
	}
	s.stageMu.Lock()
	st, ok := s.staged[stageKey{op: opID, rel: stagedRel}]
	s.stageMu.Unlock()
	if !ok {
		return layout.ErrNotFound
	}
	req := consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: final.Owner, Path: final.Path,
		Hash: [32]byte(st.hash), Size: st.size, IsDir: st.isDir, Replace: replace,
	}
	if replace {
		req.ExpectedETag = expectedETag
	}
	if err := s.publisher.PublishBlob(ctx, req); err != nil {
		return err
	}
	s.stageMu.Lock()
	delete(s.staged, stageKey{op: opID, rel: stagedRel})
	s.stageMu.Unlock()
	return nil
}

// Discard removes the operation's staged descriptors. Immutable blob content is
// left to refcount/GC; no live path was ever created.
func (s *Store) Discard(ctx context.Context, opID uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.stageMu.Lock()
	for k := range s.staged {
		if k.op == opID {
			delete(s.staged, k)
		}
	}
	s.stageMu.Unlock()
	return nil
}

func validateStage(opID uuid.UUID, rel string) error {
	if opID == uuid.Nil {
		return fmt.Errorf("%w: empty operation id", layout.ErrInvalidNode)
	}
	if _, err := layout.ValidatePath(rel); err != nil {
		return err
	}
	return nil
}

// WithPublisher enables metadata publication. Without it the adapter can stage
// content but refuses to publish, rather than pretending the operation worked.
func (s *Store) WithPublisher(p consistency.BlobPublisher) *Store {
	s.publisher = p
	return s
}

// Ensure the blob adapter satisfies the operation-scoped contract.
var _ consistency.StagingStore = (*Store)(nil)
