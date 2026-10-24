package storetest

// Account deletion versus the files OTHER users keep inside the deleted account's folders.
//
// A group space is one directory with many owners: the folder belongs to whoever created it,
// and every member's files stay their own. Deleting the folder's owner therefore cannot be an
// operation over `owner_id = the account` — the first version of that statement stranded every
// member's file under a trashed ancestor: unreachable through any listing, absent from every
// trash view, still charged to its owner, and invisible to the purge (which follows trashed
// roots) until the account's own rows were gone. Deletion has to trash the whole live subtree
// and move each owner's bytes, exactly like Trash does.
//
// This suite is invoked from the PostgreSQL-gated runner only for now: the in-memory
// DeleteUser still trashes just the account's own rows (userdelete_memory.go), so running it
// in the shared runner would fail there until that implementation catches up. When it does,
// move the call into Run's AccountDeletion block so both backends prove it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// MixedOwnerDeletionSuite pins what DeleteUser owes a third party whose file sat in the
// deleted account's folder.
//
// newBlob creates a blob row for the file the case writes. It is a parameter, not the shared
// hook, because this suite runs outside storetest.Run and must not depend on Run having set
// the global hook first — test registration order would decide whether that held.
func MixedOwnerDeletionSuite(t *testing.T, factory Factory, newBlob func(t *testing.T) uuid.UUID) {
	t.Helper()

	t.Run("DeleteUserTrashesForeignChildrenOfTheirFolders", func(t *testing.T) {
		s, ownerA := factory(t)
		rootA := rootOf(t, s, ownerA)
		b := addUser(t, s, "mixown-del-"+strings.ToLower(uuid.NewString()[:8]))
		dir := mkdir(t, s, ownerA, rootA.ID, "shared")

		var fileB store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, cerr := tx.CreateFile(ctx, store.NewFile{
				OwnerID: b.ID, ParentID: dir.ID, Name: "guest.bin",
				Size: 2048, Mime: "application/octet-stream", ETag: "etag-b",
				BlobID: newBlob(t), BlobHash: [32]byte{3},
			})
			fileB = n
			return cerr
		}); err != nil {
			t.Fatalf("create B's file inside A's folder: %v", err)
		}

		usedB := usageOf(t, s, b.ID)
		trashB := trashBytesOf(t, s, b.ID)

		// The documented two-step flow: disable, then delete.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.SetUserStatus(ctx, ownerA, store.StatusDisabled)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var res store.DeleteUserResult
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			r, derr := tx.DeleteUser(ctx, ownerA)
			res = r
			return derr
		}); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		if res.NodesTrashed < 2 {
			t.Fatalf("DeleteUser reported %d nodes trashed, want at least the folder AND B's file "+
				"inside it — a count that only covers the account's own rows is how the foreign "+
				"file went unnoticed", res.NodesTrashed)
		}

		// B's file is no longer live — and it is IN THE TRASH, not destroyed: the trash listing
		// is the only place its owner can find it again.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Node(ctx, fileB.ID)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("B's file is still live after A's deletion: %v. A trashed folder must not "+
				"leave someone else's file hanging under it", err)
		}
		var listed []store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.Trashed(ctx, b.ID, 100)
			listed = got
			return err
		}); err != nil {
			t.Fatal(err)
		}
		inTrash := false
		for _, n := range listed {
			if n.ID == fileB.ID {
				inTrash = true
			}
		}
		if !inTrash {
			t.Fatalf("B's file is not in B's trash listing (%s): a file that is neither live nor "+
				"listed is unreachable, invisible, and still charged", namesOf(listed))
		}

		// B's quota moved to retained usage — the same transition an ordinary delete does. It
		// must not simply vanish (that silently gifts B storage) and must not stay charged as
		// logical usage (that bills B for bytes they cannot reach).
		if got := usageOf(t, s, b.ID); got != usedB-2048 {
			t.Fatalf("B's logical usage %d after the deletion, want %d", got, usedB-2048)
		}
		if got := trashBytesOf(t, s, b.ID); got != trashB+2048 {
			t.Fatalf("B's retained usage %d after the deletion, want %d: the bytes moved to the "+
				"retained counter, or they were lost from the accounting entirely", got, trashB+2048)
		}

		// And the recovery window is real: B can restore their own file out of A's deleted
		// folder, and the quota comes back with it.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Restore(ctx, fileB.ID)
			return err
		}); err != nil {
			t.Fatalf("restore B's file: %v", err)
		}
		if got := usageOf(t, s, b.ID); got != usedB {
			t.Fatalf("B's logical usage %d after restoring their file, want %d", got, usedB)
		}
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Node(ctx, fileB.ID)
			return err
		}); err != nil {
			t.Fatalf("B's file is not live after the restore: %v", err)
		}
	})
}
