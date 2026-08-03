package store_test

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/storetest"
)

// TestPGConformance runs the same metadata suite against PostgreSQL.
//
// Skipped unless TEST_DATABASE_URL is set, so `go test ./...` works on a machine
// without a database; CI sets it. Running the identical suite against both
// backends is the only way to be sure that a green local test means something
// about production.
func TestPGConformance(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	storetest.Run(t, func(t *testing.T) (store.Store, uuid.UUID) {
		// The queue is global, not per-user, so it has to be cleared for each subtest.
		// The in-memory store is fresh per factory call; without this the two backends
		// would disagree about how much work is outstanding, and the disagreement would
		// only ever show up as a confusing failure in the dedupe case.
		//
		// DELETE rather than completing the rows: a row left with a live lease would be
		// invisible to the next subtest's claim *and* invisible to a completion-based
		// cleanup, which is exactly how a queue test becomes flaky.
		if _, err := pool.Exec(context.Background(), `DELETE FROM jobs`); err != nil {
			t.Fatalf("clear jobs: %v", err)
		}
		owner := newUser(t, pool)
		s := store.NewPG(pool)
		s.EnableAudit()
		return s, owner
	}, storetest.Options{
		Versions:   true,
		NewBlob:    func(t *testing.T) uuid.UUID { return newBlob(t, pool) },
		AuditCount: func(t *testing.T, s store.Store) int { return countAudit(t, pool) },
		BlobIDFor: func(t *testing.T, s store.Store, hash [32]byte) uuid.UUID {
			return s.BlobIDFor(context.Background(), hash, 0)
		},
		SetQuota: func(t *testing.T, s store.Store, owner uuid.UUID, limit int64) {
			if _, err := pool.Exec(context.Background(),
				`UPDATE users SET quota_bytes = $2 WHERE id = $1`, owner, limit); err != nil {
				t.Fatalf("set quota: %v", err)
			}
		},
	})
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func newUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	// Short, unique, and legal for the citext unique index.
	id := uuid.New()
	username := "u" + id.String()[:8]
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, username, username+"@example.test")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// newBlob inserts a blob row, because nodes.blob_id has a foreign key to it.
//
// Fabricating an id would violate that constraint ??and the constraint is right:
// a node referencing content that was never stored is exactly the corruption the
// foreign key exists to prevent. The suite takes this as a hook so the
// in-memory store, which has no such table, can run the identical tests.
func newBlob(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var sum [32]byte
	if _, err := rand.Read(sum[:]); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO blobs (id, sha256, size, backend) VALUES ($1, $2, 0, 'local')`,
		id, sum[:])
	if err != nil {
		t.Fatalf("create blob: %v", err)
	}
	return id
}

func countAudit(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit.events`).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

// resetSchema drops and reapplies every embedded migration so each run starts
// clean.
//
// It applies *all* of them, through the same applier the binary uses, rather than
// reading 0001 by filename. That mattered: the version of this helper that read
// only 0001 meant a later migration's table was never created in the test
// database, so a test exercising it would fail for a reason that had nothing to do
// with the code under test ??or, worse, would silently skip the feature.
func resetSchema(t *testing.T, pool *pgxpool.Pool) error {
	t.Helper()
	ctx := context.Background()

	_, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS audit CASCADE;
		DROP SCHEMA public CASCADE;
		CREATE SCHEMA public;
		GRANT ALL ON SCHEMA public TO public;`)
	if err != nil {
		return err
	}

	return store.ApplySchema(ctx, pool)
}

// TestPGAuditChainIsLinked verifies the property the chain exists for: each row
// commits to its predecessor, so editing or removing a row breaks verification
// from that point on.
func TestPGAuditChainIsLinked(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}

	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	root, err := s.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind:  "user",
				ActorEmail: "a@example.test",
				Action:     "file.read",
				TargetKind: "node",
				TargetID:   root.ID.String(),
				Result:     "ok",
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := pool.Query(context.Background(),
		`SELECT prev_hash, hash FROM audit.events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var prev []byte
	n := 0
	for rows.Next() {
		var p, h []byte
		if err := rows.Scan(&p, &h); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			// The first row chains from the genesis value.
			if len(p) != 32 {
				t.Fatalf("first prev_hash should be 32 bytes, got %d", len(p))
			}
		} else if string(p) != string(prev) {
			t.Fatalf("row %d prev_hash does not match previous hash", n)
		}
		prev = h
		n++
	}
	if n != 5 {
		t.Fatalf("want 5 audit rows, got %d", n)
	}
}

// TestPGAuditRequiresEnablement: an operation that cannot be recorded must not
// commit silently.
func TestPGAuditRequiresEnablement(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	_ = newUser(t, pool)

	s := store.NewPG(pool) // no EnableAudit()
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{Action: "file.read", Result: "ok"})
	})
	if err == nil {
		t.Fatal("Audit without a configured writer must fail, not silently drop the record")
	}
}
