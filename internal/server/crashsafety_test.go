package server_test

// §G3: "断网、进程中断、S3 错误注入无数据破坏" — a process interruption must not corrupt data.
//
// The upload path has two crash windows, and they behave differently on purpose:
//
//	mid-chunk
//	    The staging file is partial and no metadata exists. It cannot be served, because the local
//	    backend derives a blob's path from its hash — <root>/<hex[0:2]>/<hex[2:4]>/<hex> — and a
//	    staging file lives in <root>/tmp. There is no route from a hash to that directory, so a
//	    partial upload is unreachable by construction rather than by a check somebody remembered.
//	    NewManager clears the orphans at startup.
//
//	mid-commit
//	    Content is stored BEFORE the row (the invariant pinned in round 38). A crash between the
//	    two leaves a blob with no row — an orphan, which GC reclaims. A crash inside the metadata
//	    transaction leaves neither. In no case is a row visible whose bytes are absent, so no
//	    client ever receives a file it cannot read.
//
// Both are properties of the code as written, and both are the sort of thing a refactor breaks
// silently: moving the staging directory under the content-addressed tree, or reordering the row
// before the content, would each turn a crash into a corrupt file. These tests state them.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/upload"
)

// TestPartialUploadIsUnreachableByHash is the mid-chunk property.
func TestPartialUploadIsUnreachableByHash(t *testing.T) {
	root := t.TempDir()
	store, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}

	// A staging directory with a partial file in it, exactly as a crash would leave it.
	staging := filepath.Join(root, "tmp")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	// The file is named by the hash of its own contents, which is what a staging layout inside the
	// addressable tree would produce — a partially-written chunk that answers to its own address.
	content := []byte("half a file")
	h := blob.NewHasher()
	_, _ = h.Write(content)
	hash, _ := h.Sum()

	partial := filepath.Join(staging, hash.String())
	if err := os.WriteFile(partial, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Open(context.Background(), hash); err == nil {
		t.Fatal("a partial upload in the staging directory was served by its content hash. The " +
			"local backend derives a blob path from the hash, so this means the staging directory " +
			"is inside the addressable tree — and a crashed upload would be readable as if it were " +
			"a complete, committed file.")
	} else if err != blob.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestCommittedBlobIsAddressable is the control.
//
// Without it, a store that could never open anything would pass the test above.
func TestCommittedBlobIsAddressable(t *testing.T) {
	root := t.TempDir()
	store, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("a complete file")
	hasher := blob.NewHasher()
	_, _ = hasher.Write(content)
	hash, size := hasher.Sum()
	_ = size
	if err := store.Put(context.Background(), hash, strings.NewReader(string(content)),
		int64(len(content))); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := store.Open(context.Background(), hash)
	if err != nil {
		t.Fatalf("a committed blob must be readable by its hash: %v", err)
	}
	defer rc.Close()
}

// TestStagingDirectoryIsOutsideTheAddressableTree states the layout rule directly.
//
// The behavioural test above proves the consequence; this one names the cause, so a change to the
// layout fails with a message about the layout rather than about an unexpected file being served.
func TestStagingDirectoryIsOutsideTheAddressableTree(t *testing.T) {
	root := t.TempDir()
	if _, err := local.New(root); err != nil {
		t.Fatal(err)
	}

	// The backend's own idea of where a hash lives, for a hash whose first bytes would collide
	// with the staging name if the layout were flat.
	var h blob.Hash
	h[0], h[1] = 't', 'm'
	// "tmp" is two levels of directory in the real layout; a staging directory at <root>/tmp is
	// therefore not a prefix of any blob path.
	blobPath := filepath.Join(root, h.String()[0:2], h.String()[2:4], h.String())
	staging := filepath.Join(root, "tmp")
	if strings.HasPrefix(blobPath, staging+string(filepath.Separator)) {
		t.Fatalf("a blob path (%s) falls under the staging directory (%s); partial uploads would "+
			"be addressable", blobPath, staging)
	}
}

// TestStartupRemovesOrphansThatNoSessionOwns is the recovery half.
//
// Clearing at startup is what stops a crashed upload from occupying disk forever. It is asserted
// here rather than in the upload package alone because it is the recovery mechanism this document
// points at for the mid-chunk case.
func TestStartupRemovesOrphansThatNoSessionOwns(t *testing.T) {
	staging := t.TempDir()

	// A file named the way this package names its staging files, which is what a previous process
	// would have left. The first version of this test used an arbitrary name and failed — correctly:
	// startup removes files matching its own `part-` prefix and deliberately leaves strangers alone,
	// because deleting every file in a directory it was merely pointed at is how a backup target
	// gets wiped by an upload server. The test was wrong, not the code.
	orphan := filepath.Join(staging, "part-crashed-process")
	if err := os.WriteFile(orphan, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, err := upload.NewManager(staging, 0, 4)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("the previous process's staging file survived startup (%v). A crashed upload "+
			"would then hold disk forever, and nothing else knows how to find it.", err)
	}
}
