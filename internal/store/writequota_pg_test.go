package store_test

// Deterministic regressions for the read-after-lock rule in UpdateFile and RestoreVersion.
//
// The window these tests pin — "a concurrent writer commits between the unlocked read and the
// row lock" — is a race in production but a straight line here: SetPreLockStallForTest runs a
// callback inside exactly that window, and the callback commits the interfering write before
// returning. Without the fix, the transaction computes its precondition and its quota delta
// from the pre-interference snapshot, and the observable result is either a 412 for a client
// whose precondition was correct, or a quota moved by the difference between two sizes that
// were never the file's at the same moment.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestPGUpdateFileReReadsUnderTheLock(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()
	t.Cleanup(func() { s.SetPreLockStallForTest(nil) })

	owner := newUser(t, pool)
	var fileID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		root, err := tx.EnsureRoot(ctx, owner)
		if err != nil {
			return err
		}
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "raced.bin", Size: 100,
			Mime: "application/octet-stream", ETag: "e1",
			BlobID: newBlob(t, pool), BlobHash: [32]byte{5},
		})
		fileID = f.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The interference lands inside the window between the transaction's first read and its
	// lock: 100 -> 300 bytes, quota following (+200), ETag now e2.
	s.SetPreLockStallForTest(func(context.Context) {
		tx2, err := pool.Begin(context.Background())
		if err != nil {
			t.Errorf("interference tx: %v", err)
			return
		}
		defer tx2.Rollback(context.Background())
		if _, err := tx2.Exec(context.Background(),
			`UPDATE nodes SET size = 300, etag = 'e2', modified_at = now() WHERE id = $1`, fileID); err != nil {
			t.Errorf("interference update: %v", err)
			return
		}
		if _, err := tx2.Exec(context.Background(),
			`UPDATE users SET used_bytes = used_bytes + 200 WHERE id = $1`, owner); err != nil {
			t.Errorf("interference quota: %v", err)
			return
		}
		if err := tx2.Commit(context.Background()); err != nil {
			t.Errorf("interference commit: %v", err)
		}
	})

	// The client resolved the file AFTER the interference committed — its If-Match is e2, the
	// ETag that is actually current — and writes 400 bytes. The transaction's pre-lock
	// snapshot still says e1/100.
	var updated store.Node
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.UpdateFile(ctx, store.UpdateFile{
			NodeID: fileID, Size: 400, Mime: "application/octet-stream", ETag: "e3",
			BlobID: newBlob(t, pool), BlobHash: [32]byte{6},
			Expect: &store.Precondition{IfMatch: "e2"},
		})
		updated = n
		return err
	}); err != nil {
		t.Fatalf("a write preconditioned on the CURRENT etag failed: %v. The precondition was "+
			"evaluated against the pre-lock snapshot, so a correct client was refused — the "+
			"stale read leaked past the lock", err)
	}
	if updated.Size != 400 {
		t.Fatalf("written size %d, want 400", updated.Size)
	}

	var used int64
	if err := pool.QueryRow(ctx, `SELECT used_bytes FROM users WHERE id = $1`, owner).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 400 {
		t.Fatalf("used_bytes %d, want 400 (100 created + 200 by the interfering write + 100 by "+
			"the delta from the LOCKED row's 300): billing anything else charged bytes that were "+
			"never written or refunded bytes that were", used)
	}
}

func TestPGRestoreVersionMovesQuotaByTheLockedRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()
	t.Cleanup(func() { s.SetPreLockStallForTest(nil) })

	owner := newUser(t, pool)
	var fileID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		root, err := tx.EnsureRoot(ctx, owner)
		if err != nil {
			return err
		}
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "restored.bin", Size: 100,
			Mime: "application/octet-stream", ETag: "e1",
			BlobID: newBlob(t, pool), BlobHash: [32]byte{7},
		})
		fileID = f.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Grow to 1000, keeping the 100-byte content as version 1. used_bytes is now 1000.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.UpdateFile(ctx, store.UpdateFile{
			NodeID: fileID, Size: 1000, Mime: "application/octet-stream", ETag: "e2",
			BlobID: newBlob(t, pool), BlobHash: [32]byte{8},
			KeepVersion: true, MaxVersions: 10,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The interference lands where RestoreVersion reads: a concurrent save shrinks the file to
	// 500 and moves the quota accordingly. The restore must diff against THAT row, not the
	// 1000-byte row it may have read before the window.
	s.SetPreLockStallForTest(func(context.Context) {
		tx2, err := pool.Begin(context.Background())
		if err != nil {
			t.Errorf("interference tx: %v", err)
			return
		}
		defer tx2.Rollback(context.Background())
		if _, err := tx2.Exec(context.Background(),
			`UPDATE nodes SET size = 500, etag = 'e-mid', modified_at = now() WHERE id = $1`, fileID); err != nil {
			t.Errorf("interference update: %v", err)
			return
		}
		if _, err := tx2.Exec(context.Background(),
			`UPDATE users SET used_bytes = used_bytes - 500 WHERE id = $1`, owner); err != nil {
			t.Errorf("interference quota: %v", err)
			return
		}
		if err := tx2.Commit(context.Background()); err != nil {
			t.Errorf("interference commit: %v", err)
		}
	})

	var restored store.Node
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.RestoreVersion(ctx, fileID, 1, nil)
		restored = n
		return err
	}); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	if restored.Size != 100 {
		t.Fatalf("restored size %d, want 100", restored.Size)
	}

	var used int64
	if err := pool.QueryRow(ctx, `SELECT used_bytes FROM users WHERE id = $1`, owner).Scan(&used); err != nil {
		t.Fatal(err)
	}
	// 1000 - 500 (the interfering save) - 400 (restore 500 -> 100) = 100. Differencing against
	// the stale 1000-byte row releases 900 instead and lands at 0 — free storage conjured out
	// of a read that was stale the moment the write ran.
	if used != 100 {
		t.Fatalf("used_bytes %d after the restore, want 100: the restore moved quota by the "+
			"difference from a row it had not locked, so the delta described a file that no "+
			"longer existed", used)
	}
}
