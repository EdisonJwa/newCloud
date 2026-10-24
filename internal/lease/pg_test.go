package lease_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/lease"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL lease test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPGLeaseExcludesASecondOwnerAndAdvancesEpoch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	key := int64(0x4e430013) // a stable, deployment-specific advisory-lock key
	firstMgr := lease.NewPG(pool, key)
	first, err := firstMgr.Acquire(ctx, uuid.New())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Close()

	secondMgr := lease.NewPG(pool, key)
	if _, err := secondMgr.Acquire(ctx, uuid.New()); !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("second owner = %v, want ErrHeld", err)
	}

	first.Close()
	thirdMgr := lease.NewPG(pool, key)
	third, err := thirdMgr.Acquire(ctx, uuid.New())
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if third.Epoch <= first.Epoch {
		t.Fatalf("epoch did not advance: first %d, third %d", first.Epoch, third.Epoch)
	}
	third.Close()
}

func TestPGTreeLockIsIndependentOfTheLease(t *testing.T) {
	// The OS lock is process-local, not database state; this test pins that it is
	// still acquired and refuses a second owner even when the database is healthy.
	dir := t.TempDir()
	first, err := lease.LockTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := lease.LockTree(dir); !errors.Is(err, lease.ErrTreeLocked) {
		t.Fatalf("second Tree lock = %v, want ErrTreeLocked", err)
	}
}

func TestPGLeaseLossFiresWhenTheBackendDies(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	mgr := lease.NewPG(pool, int64(0x4e430014)).WithMonitorIntervalForTest(50 * time.Millisecond)
	got, err := mgr.Acquire(ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	pid := mgr.BackendPID()
	if pid == 0 {
		t.Fatal("lease did not record its backend PID")
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got.Lost:
	case <-time.After(3 * time.Second):
		t.Fatal("lease loss did not fire after the backend was terminated")
	}
}
