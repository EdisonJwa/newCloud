package worker_test

// WAL archive health sampling, against a real PostgreSQL.
//
// The failure this covers is silent by construction: when archive_command fails, PostgreSQL keeps
// serving and keeps accepting writes. The only visible symptom is that pg_wal grows, and nothing in
// the application noticed because nothing read pg_stat_archiver. Measured in round 42 against a
// deliberately broken archive_command: archived_count stayed 0, failed_count climbed, and pg_wal
// went 49 → 65 → 81 MB over three rounds of writes.
//
// These tests use TEST_DATABASE_URL. They skip when it is unset, which is the same contract the
// store conformance suite uses — but a skip is reported rather than silently passing.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

func walTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; the WAL sampler needs a real PostgreSQL")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestSampleWALArchiveReadsTheArchiver.
func TestSampleWALArchiveReadsTheArchiver(t *testing.T) {
	pool := walTestPool(t)
	w := worker.New(store.NewPG(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	snap, err := w.SampleWALArchive(ctx)
	if err != nil {
		t.Fatalf("SampleWALArchive: %v", err)
	}

	// The counters must be readable. Their values depend on the cluster's configuration — a
	// cluster with archive_mode=off reports zeros and that is correct — so the assertion is that
	// the query ran and produced a snapshot, not a particular number.
	var archivedInDB, failedInDB uint64
	if err := pool.QueryRow(ctx,
		`SELECT archived_count, failed_count FROM pg_stat_archiver`).
		Scan(&archivedInDB, &failedInDB); err != nil {
		t.Fatalf("read pg_stat_archiver directly: %v", err)
	}
	if snap.Archived != archivedInDB || snap.Failed != failedInDB {
		t.Fatalf("snapshot %+v does not match the database (%d archived, %d failed)",
			snap, archivedInDB, failedInDB)
	}
}

// TestSampleWALArchiveIsZeroForAnInMemoryStore.
//
// The worker runs with either backend. An in-memory store has no WAL, and the sampler must return
// a zero snapshot rather than an error — otherwise every test using the memory store would log a
// warning on every sweep.
func TestSampleWALArchiveIsZeroForAnInMemoryStore(t *testing.T) {
	w := worker.New(store.NewMemory(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	snap, err := w.SampleWALArchive(context.Background())
	if err != nil {
		t.Fatalf("an in-memory store must not error: %v", err)
	}
	if snap != (worker.WALArchive{}) {
		t.Fatalf("want a zero snapshot for the memory store, got %+v", snap)
	}
}

// TestSampleWALArchiveReportsPendingSegments is the disk-fill signal.
//
// Pending is the gauge that predicts an outage: it is the count of WAL segments sitting on disk
// that have not been archived. It needs pg_ls_waldir(), which is restricted, so the sampler
// tolerates its absence — this asserts that when it IS available the number comes back, because a
// silently-always-zero gauge would look like "nothing pending" on a cluster that is filling up.
func TestSampleWALArchiveReportsPendingSegments(t *testing.T) {
	pool := walTestPool(t)
	w := worker.New(store.NewPG(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Whether the role may call pg_ls_waldir() at all.
	var canList bool
	if err := pool.QueryRow(ctx,
		`SELECT has_function_privilege('pg_ls_waldir()', 'EXECUTE')`).Scan(&canList); err != nil {
		t.Skipf("cannot check pg_ls_waldir privilege: %v", err)
	}
	if !canList {
		t.Skip("this role cannot call pg_ls_waldir(); the pending gauge is unavailable by design")
	}

	snap, err := w.SampleWALArchive(ctx)
	if err != nil {
		t.Fatalf("SampleWALArchive: %v", err)
	}

	var want int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_ls_waldir()`).Scan(&want); err != nil {
		t.Fatalf("count pg_ls_waldir directly: %v", err)
	}
	if snap.Pending != want {
		t.Fatalf("pending = %d, want %d. A gauge that reports zero while the cluster fills is "+
			"worse than no gauge: it reads as healthy.", snap.Pending, want)
	}
	// A live cluster always has at least one segment; if this is zero the query silently failed
	// somewhere and the comparison above would have passed for the wrong reason.
	if want == 0 {
		t.Fatal("pg_ls_waldir returned no segments, which is not possible on a running cluster")
	}
}

// TestEveryTickerHasASelectCase.
//
// The integrity scrub was written, wired into main, logged at startup — and never ran, because the
// `case <-scrubs.C:` arm of the worker's select was missing. The ticker was created and stopped, and
// nothing read from it. It took a live end-to-end run with a deliberately corrupted blob to notice,
// because every unit test called ScrubBlobs directly and every log line looked right.
//
// This is the structural check that would have caught it: a ticker declared in Run must be read in
// Run's select. It is a source-shape test, which is unusual, but the defect it guards is a shape —
// there is no runtime behaviour to observe when the code never executes.
func TestEveryTickerHasASelectCase(t *testing.T) {
	root := repoRootForWorker(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "worker", "worker.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	// Tickers declared with `name := time.NewTicker(...)`.
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(\w+)\s*:=\s*time\.NewTicker\(`).FindAllStringSubmatch(text, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("found no tickers; this check would pass vacuously")
	}

	for name := range declared {
		// The ticker must be received from somewhere, which is what makes it fire.
		if !strings.Contains(text, "<-"+name+".C") {
			t.Fatalf("ticker %q is created but nothing receives from %q.C: the task it drives never "+
				"runs. A background job that is declared, logged and never invoked is worse than a "+
				"missing one, because the startup log says it is enabled.", name, name)
		}
	}
}

// repoRootForWorker walks up to the module root.
func repoRootForWorker(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the module root")
	return ""
}
