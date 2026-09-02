package store_test

// §8.1: 版本、回收站、分享和正在进行的下载可能共同引用同一 Blob，物理 GC 必须确保没有有效引用。
//
// Versions, trash, shares and in-flight downloads may all reference the same blob, and physical GC
// must be sure there are no live references.
//
// The existing collector test covers ONE source: a live node whose blob has a forced-zero refcount.
// The requirement is about the others, which differ in ways that matter:
//
//   - A TRASHED node still holds its blob, because the trash is recoverable until the retention
//     sweep runs. A collector that counted only live nodes would delete the bytes of a file the
//     user can still restore — unrecoverable loss, and exactly what this clause is about.
//   - A VERSION row holds its blob when no node points at it. This is not an edge case: after an
//     edit, the old content is referenced ONLY by the version row, because the node has moved on to
//     the new blob. Collecting that blob breaks restore-version for a file that still exists.
//   - A download session references a NODE, not a blob, so its protection is transitive. It holds
//     only while the node row is there.
//
// Each source is isolated in its own test. A single test that added them all and removed them all
// would not tell you which reference was doing the work — and the first version of this file made
// exactly that mistake: it attached the version to the node whose deletion CASCADES the version
// away (versions.node_id is ON DELETE CASCADE), so the version reference was never once the thing
// under test, and the test would have passed with the version join deleted.

import (
	"context"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

// collectorHarness bundles what each case needs.
type collectorHarness struct {
	s     store.Store
	root  uuid.UUID
	owner uuid.UUID
}

func newCollectorHarness(t *testing.T) collectorHarness {
	t.Helper()
	owner := newUser(t, testPool(t))
	s := store.NewPG(testPool(t))
	s.EnableAudit()
	var root uuid.UUID
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner)
		root = n.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return collectorHarness{s: s, root: root, owner: owner}
}

// collectable reports whether the collector would pick this blob up right now.
func (h collectorHarness) collectable(t *testing.T, blobID uuid.UUID) bool {
	t.Helper()
	var refs []store.BlobRef
	if err := h.s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.CollectableBlobs(ctx, 0)
		refs = got
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.ID == blobID {
			return true
		}
	}
	return false
}

// TestPGCollectorProtectsBlobReferencedOnlyByAVersion is the history case.
//
// After an edit the previous content is referenced by the version row and by nothing else: the node
// has moved on to the new blob. Collecting the old blob breaks restore-version silently — the
// history entry stays and restoring it serves nothing.
func TestPGCollectorProtectsBlobReferencedOnlyByAVersion(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()
	owner := newUser(t, pool)
	h := collectorHarness{s: s, owner: owner}

	var root uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner)
		root = n.ID
		h.root = root
		return err
	}); err != nil {
		t.Fatal(err)
	}

	oldBlob := newBlob(t, pool)
	newBlobID := newBlob(t, pool)

	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root, Name: "edited.bin", Size: 4,
			Mime: "application/octet-stream", ETag: "v1", BlobID: oldBlob,
		})
		if err != nil {
			return err
		}
		// Replace the content, KEEPING the old one as a version — what an ordinary edit does.
		_, uerr := tx.UpdateFile(ctx, store.UpdateFile{
			NodeID: f.ID, Size: 5, BlobID: newBlobID, ETag: "v2", KeepVersion: true,
		})
		return uerr
	}); err != nil {
		t.Fatal(err)
	}

	// Prove the setup: the node no longer points at oldBlob, but a version does. Without this the
	// assertions below could be resting on the node reference without saying so.
	var nodeRefs, versionRefs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM nodes WHERE blob_id = $1`, oldBlob).Scan(&nodeRefs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM versions WHERE blob_id = $1`, oldBlob).Scan(&versionRefs); err != nil {
		t.Fatal(err)
	}
	if nodeRefs != 0 || versionRefs == 0 {
		t.Fatalf("setup: the old blob should be referenced only by a version (nodes=%d, versions=%d)",
			nodeRefs, versionRefs)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE blobs SET refcount = 0, collect_after = now() - interval '1 day'
		WHERE id = $1`, oldBlob); err != nil {
		t.Fatal(err)
	}

	if h.collectable(t, oldBlob) {
		t.Fatal("a blob referenced only by a VERSION row was collectable. After an edit the old " +
			"content is referenced by the version and by nothing else, so the version join in " +
			"CollectableBlobs is the only thing keeping restore-version working for a file that " +
			"still exists.")
	}

	// Control: with the version gone the blob must become collectable, or this test cannot tell a
	// guarded collector from one that never collects anything.
	if _, err := pool.Exec(ctx, `DELETE FROM versions WHERE blob_id = $1`, oldBlob); err != nil {
		t.Fatal(err)
	}
	if !h.collectable(t, oldBlob) {
		t.Fatal("the old blob never became collectable with its version removed")
	}
}

// TestPGCollectorProtectsBlobReferencedOnlyByTrashedNode is the recoverable-trash case.
func TestPGCollectorProtectsBlobReferencedOnlyByTrashedNode(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()
	owner := newUser(t, pool)

	var root uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner)
		root = n.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := collectorHarness{s: s, root: root, owner: owner}

	blob := newBlob(t, pool)
	var fileID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root, Name: "trashed.bin", Size: 4,
			Mime: "application/octet-stream", ETag: "t", BlobID: blob,
		})
		if err != nil {
			return err
		}
		fileID = f.ID
		return tx.Trash(ctx, fileID)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE blobs SET refcount = 0, collect_after = now() - interval '1 day'
		WHERE id = $1`, blob); err != nil {
		t.Fatal(err)
	}

	if h.collectable(t, blob) {
		t.Fatal("a blob referenced only by a TRASHED node was collectable. A trashed file is " +
			"recoverable until the retention sweep runs, so collecting its bytes is unrecoverable " +
			"loss — the failure §8.1 names.")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, fileID); err != nil {
		t.Fatal(err)
	}
	if !h.collectable(t, blob) {
		t.Fatal("the blob never became collectable with its trashed node removed")
	}
}

// TestPGCollectorProtectsBlobBehindAnInFlightDownload is the transfer case.
//
// A download session references a node, not a blob, so the protection is transitive: it holds only
// while the node row is there. Stating that explicitly is the point — a future change that released
// the node reference when a transfer started would silently expose the bytes.
func TestPGCollectorProtectsBlobBehindAnInFlightDownload(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()
	owner := newUser(t, pool)

	var root, fileID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner)
		if err != nil {
			return err
		}
		root = n.ID
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root, Name: "downloading.bin", Size: 16,
			Mime: "application/octet-stream", ETag: "dl", BlobID: newBlob(t, pool),
		})
		if err != nil {
			return err
		}
		fileID = f.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := collectorHarness{s: s, root: root, owner: owner}

	var blobID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT blob_id FROM nodes WHERE id = $1`, fileID).Scan(&blobID); err != nil {
		t.Fatal(err)
	}

	var shareID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		sh, err := tx.CreateShare(ctx, store.Share{
			NodeID: fileID, OwnerID: owner, Kind: store.ShareLink,
			TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
		})
		if err != nil {
			return err
		}
		shareID = sh.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
			ShareID: shareID, NodeID: fileID, GrantHash: []byte("grant"),
			ETag: "dl", ExpiresAt: time.Now().Add(time.Hour),
		}, false)
		return err
	}); err != nil {
		t.Fatalf("authorize download: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE blobs SET refcount = 0, collect_after = now() - interval '1 day'
		WHERE id = $1`, blobID); err != nil {
		t.Fatal(err)
	}

	if h.collectable(t, blobID) {
		t.Fatal("a blob behind an in-flight download was collectable. The download session " +
			"references the node rather than the blob, so the protection is transitive — it holds " +
			"only because the node row is still there.")
	}
}
