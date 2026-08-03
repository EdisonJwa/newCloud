package store_test

// T-12 name-key repair.
//
// Migration 0007 backfilled `name_key` with SQL `lower(name)`, which cannot produce Unicode NFC
// or full case folding. Every ASCII name agrees, which is exactly why the drift is easy to miss:
// it only appears once a real client stores a decomposed accent (macOS sends NFD) or a foldable
// pair (ß/ss). A stale key makes the affected file unreachable by the spelling the user types,
// because lookup filters on `name_key`.
//
// These run against PostgreSQL because the defect is in a column the memory backend does not
// have. They are skipped without TEST_DATABASE_URL like the other PG-only tests.

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func repairTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedStaleKey inserts a node whose stored key is what SQL lower() would produce, so the repair
// has something to correct. It bypasses the store API on purpose: going through CreateFile would
// write the correct key and the test would prove nothing.
func seedStaleKey(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, parent uuid.UUID,
	name, staleKey string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO nodes (id, parent_id, owner_id, name, name_key, kind, size, mime, etag)
		VALUES ($1, $2, $3, $4, $5, 'dir', 0, '', '')`,
		id, parent, owner, name, staleKey); err != nil {
		t.Fatalf("seed stale key for %q: %v", name, err)
	}
	return id
}

func nameKeyRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var key string
	if err := pool.QueryRow(context.Background(),
		`SELECT name_key FROM nodes WHERE id = $1`, id).Scan(&key); err != nil {
		t.Fatalf("read key: %v", err)
	}
	return key
}

// TestRepairNameKeysRewritesDecomposedAccent: macOS stores "café" as NFD, so the backfilled key
// keeps the combining mark and never matches the precomposed spelling.
func TestRepairNameKeysRewritesDecomposedAccent(t *testing.T) {
	pool := repairTestPool(t)
	owner, parent := seedOwnerAndRoot(t, pool)

	decomposed := "cafe\u0301.txt"
	id := seedStaleKey(t, pool, owner, parent, decomposed, decomposed)

	rep, err := store.RepairNameKeys(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Corrected == 0 {
		t.Fatal("a decomposed name must be corrected")
	}

	// The stored key is now NFC: the combining mark is gone, replaced by the precomposed rune.
	got := nameKeyRow(t, pool, id)
	precomposed := store.NameKeyForTest("caf\u00e9.txt")
	if got != precomposed {
		t.Fatalf("key after repair = %q, want %q", got, precomposed)
	}
	// And the two spellings now resolve to one key, which is the property that matters.
	if store.NameKeyForTest(decomposed) != precomposed {
		t.Fatal("the key function must treat both spellings as equal")
	}

	_ = parent
}

// TestRepairNameKeysFoldsSharpS: ß and SS are the same filename to a user.
func TestRepairNameKeysFoldsSharpS(t *testing.T) {
	pool := repairTestPool(t)
	owner, parent := seedOwnerAndRoot(t, pool)

	name := "stra\u00dfe.txt"
	id := seedStaleKey(t, pool, owner, parent, name, name) // lower() leaves ß as ß

	if _, err := store.RepairNameKeys(context.Background(), pool); err != nil {
		t.Fatal(err)
	}

	got := nameKeyRow(t, pool, id)
	if got != store.NameKeyForTest("STRASSE.txt") {
		t.Fatalf("key after repair = %q, want the ss-folded key", got)
	}
}

// TestRepairNameKeysIsIdempotent: a second pass must report no work, or `newcloud migrate` would
// rewrite rows on every run.
func TestRepairNameKeysIsIdempotent(t *testing.T) {
	pool := repairTestPool(t)
	seedOwnerAndRoot(t, pool)

	if _, err := store.RepairNameKeys(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	second, err := store.RepairNameKeys(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if second.Corrected != 0 {
		t.Fatalf("a second pass must correct nothing, corrected %d", second.Corrected)
	}
}

// TestRepairNameKeysSkipsCollidingSiblings is the guard against the repair breaking the unique
// index. Two nodes can fold to one key (that is the bug the index prevents); when they do, the
// repair must report the ambiguity rather than crash or pick a winner silently.
func TestRepairNameKeysSkipsCollidingSiblings(t *testing.T) {
	pool := repairTestPool(t)
	owner, parent := seedOwnerAndRoot(t, pool)

	// Two siblings whose names fold to the same key, each with a stale (lower) key:
	// "straße.txt" folds to "strasse.txt" and "STRASSE.txt" lowercases to "strasse.txt" — so the
	// second already holds the key the first would be corrected to.
	first := seedStaleKey(t, pool, owner, parent, "stra\u00dfe.txt", "stra\u00dfe.txt")
	seedStaleKey(t, pool, owner, parent, "STRASSE.txt", "strasse.txt")

	rep, err := store.RepairNameKeys(context.Background(), pool)
	if err != nil {
		t.Fatalf("a collision must be reported, not returned as an error: %v", err)
	}
	if len(rep.Skipped) == 0 {
		t.Fatal("the ambiguous row must be reported as skipped")
	}

	// The skipped row keeps its old key rather than being force-written into a violation.
	if got := nameKeyRow(t, pool, first); got != "stra\u00dfe.txt" {
		t.Fatalf("the skipped row must be left untouched, key = %q", got)
	}
}

// seedOwnerAndRoot creates an account and its namespace root, returning both.
//
// The root has to be a real row: nodes.parent_id is a foreign key, so a made-up parent id fails
// the insert before the test reaches the repair.
func seedOwnerAndRoot(t *testing.T, pool *pgxpool.Pool) (owner, root uuid.UUID) {
	t.Helper()
	owner = uuid.New()
	name := "rk" + owner.String()[:8]
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		owner, name, name+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	root = uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO nodes (id, parent_id, owner_id, name, name_key, kind, size, mime, etag)
		VALUES ($1, NULL, $2, '', '', 'dir', 0, '', '')`, root, owner); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	return owner, root
}

// TestRepairNameKeysCrossesPageBoundary is the batching property.
//
// The repair pages at 1000 rows. Every other test here seeds a handful, so they exercise only the
// first page — an implementation that repaired one page and stopped would pass all of them. This
// seeds more rows than a page and asserts the tail was reached.
//
// It also covers the cursor advance: rows are ordered by id and the loop must move forward on
// every row, including the already-correct ones, or it would never terminate.
func TestRepairNameKeysCrossesPageBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds more than a page of rows")
	}
	pool := repairTestPool(t)
	owner, root := seedOwnerAndRoot(t, pool)

	// Registered before the first assertion: a failing test must still clean up, or the leftovers
	// change the result of the next run and the failure looks like flakiness.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM nodes WHERE owner_id = $1::uuid`, owner)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1::uuid`, owner)
	})

	// 1100 > the 1000-row page size, all with a stale key (ß left unfolded by lower()).
	const n = 1100
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO nodes (id, parent_id, owner_id, name, name_key, kind, size, mime, etag)
		SELECT gen_random_uuid(), $1::uuid, $2::uuid,
		       'pgfile' || g || '-stra' || chr(223) || 'e.txt',
		       'pgfile' || g || '-stra' || chr(223) || 'e.txt',
		       'dir', 0, '', ''
		FROM generate_series(1, $3) g`, root, owner, n); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rep, err := store.RepairNameKeys(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Corrected != n {
		t.Fatalf("want %d corrected across the page boundary, got %d (scanned %d)",
			n, rep.Corrected, rep.Scanned)
	}

	// Every row's key is now the ss-folded form.
	var stale int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM nodes
		WHERE owner_id = $1::uuid AND name LIKE 'pgfile%'
		  AND name_key <> replace(name, chr(223), 'ss')`, owner).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d rows were left stale past the first page", stale)
	}

}
