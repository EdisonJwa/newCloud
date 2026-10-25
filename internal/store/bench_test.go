package store_test

// Benchmarks for the metadata operations a sync client actually feels.
//
// The competitor comparison's §8 says the number that matters is "files/second on a PROPFIND
// over a large directory", because slow listing is the most-cited complaint about the systems
// this is measured against. These are the store-level halves of that number: the listing query,
// the path resolution a client does before every request, and the write path.
//
// They are benchmarks, not tests: they do not run in CI, and the fixture size comes from
// BENCH_CHILDREN so the same harness answers "10k" and "100k" without editing code.
//
//	go test ./internal/store -bench . -run '^$' -benchtime 3x
//	BENCH_CHILDREN=100000 go test ./internal/store -bench ListChildren -run '^$' -benchtime 3x
//
// The PostgreSQL variant needs TEST_DATABASE_URL and is skipped without it.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// benchChildren is how many entries the listing fixture holds.
func benchChildren() int {
	if v := os.Getenv("BENCH_CHILDREN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10_000
}

// seedOwner creates an owner and its root directory, returning both ids.
func seedOwner(b *testing.B, st store.Store) (owner, rootID uuid.UUID) {
	b.Helper()
	ctx := context.Background()
	want := uuid.New()
	// The owner row has to exist: a node with no users row is a referential-integrity fault on
	// PostgreSQL, and the store reports it as one rather than as "not found".
	if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer: "https://idp.test", Subject: want.String(),
			Username: "bench-" + want.String()[:8],
			Email:    "bench-" + want.String()[:8] + "@example.test", Role: "user",
		})
		owner = u.ID
		return uerr
	}); err != nil {
		b.Fatalf("create owner: %v", err)
	}
	root, err := st.EnsureRoot(ctx, owner)
	if err != nil {
		b.Fatalf("root: %v", err)
	}
	return owner, root.ID
}

func mkdirBench(b *testing.B, st store.Store, owner, parent uuid.UUID, name string) store.Node {
	b.Helper()
	var dir store.Node
	if err := st.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		d, derr := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: parent, Name: name})
		dir = d
		return derr
	}); err != nil {
		b.Fatalf("mkdir %s: %v", name, err)
	}
	return dir
}

// benchEnv builds the in-memory fixture: one directory holding n files.
func benchEnv(b *testing.B, n int) (store.Store, store.Node, uuid.UUID) {
	b.Helper()
	st := store.NewMemory()
	ctx := context.Background()
	owner, rootID := seedOwner(b, st)
	dir := mkdirBench(b, st, owner, rootID, "listing")

	// One transaction for the whole fixture: a transaction per file would make the setup
	// dominate the benchmark and hide what is being measured.
	if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for i := 0; i < n; i++ {
			if _, cerr := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: dir.ID,
				Name: fmt.Sprintf("file-%05d.bin", i),
				Size: 1024, Mime: "application/octet-stream", ETag: `"e"`,
				BlobID: uuidPtr(), BlobHash: [32]byte{byte(i)},
			}); cerr != nil {
				return cerr
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed %d files: %v", n, err)
	}
	return st, dir, owner
}

// benchEnvPG builds the same fixture against PostgreSQL, which is the number a deployment pays.
//
// The in-memory backend removes I/O on purpose, so it measures the application's own cost; it
// also sorts every child before applying the page limit, which PostgreSQL does not have to do.
// The two answer different questions and both belong on the page.
//
// The children are seeded in ONE statement. Creating them through the store one at a time takes
// minutes at 100k, and a benchmark whose fixture dominates its measurement is measuring the
// fixture. No truncation: the fixture lives under a fresh owner and every query is scoped to it.
func benchEnvPG(b *testing.B, n int) (store.Store, store.Node, uuid.UUID) {
	b.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("TEST_DATABASE_URL not set; skipping the PostgreSQL fixture")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	b.Cleanup(pool.Close)
	if err := store.ApplySchema(ctx, pool); err != nil {
		b.Fatalf("apply schema: %v", err)
	}
	pg := store.NewPG(pool)

	owner, rootID := seedOwner(b, pg)
	dir := mkdirBench(b, pg, owner, rootID, "listing")

	// A node's blob_id must reference a real row on PostgreSQL; one shared blob satisfies the
	// constraint, and the benchmark measures the listing, not the content.
	var blobID uuid.UUID
	sum := make([]byte, 32)
	sum[0] = 1
	if err := pool.QueryRow(ctx,
		`INSERT INTO blobs (sha256, size, backend) VALUES ($1, $2, 'bench')
		 ON CONFLICT (sha256) DO UPDATE SET size = EXCLUDED.size
		 RETURNING id`, sum, 1024).Scan(&blobID); err != nil {
		b.Fatalf("seed blob: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO nodes (parent_id, owner_id, name, name_key, kind, size, mime, etag, blob_id)
		SELECT $1, $2,
		       'file-' || lpad(g::text, 5, '0') || '.bin',
		       'file-' || lpad(g::text, 5, '0') || '.bin',
		       'file', 1024, 'application/octet-stream', '"e"', $3
		FROM generate_series(0, $4 - 1) AS g`, dir.ID, owner, blobID, n); err != nil {
		b.Fatalf("bulk seed %d rows: %v", n, err)
	}
	// Without statistics the planner has no idea how many children a directory holds and picks a
	// seq scan plus a sort: the first version of this benchmark measured a plan no deployment
	// would ever run, and reported the listing as 30 ms when the indexed query takes 0.1 ms.
	if _, err := pool.Exec(ctx, `ANALYZE nodes`); err != nil {
		b.Fatalf("analyze: %v", err)
	}
	return pg, dir, owner
}

func uuidPtr() uuid.UUID { return uuid.New() }

// benchList measures one page of a directory listing.
func benchList(b *testing.B, env func(*testing.B, int) (store.Store, store.Node, uuid.UUID)) {
	n := benchChildren()
	st, dir, _ := env(b, n)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var got int
		if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			kids, err := tx.Children(ctx, dir.ID, 500)
			got = len(kids)
			return err
		}); err != nil {
			b.Fatal(err)
		}
		if got == 0 {
			b.Fatal("listing returned nothing")
		}
	}
	b.ReportMetric(float64(n), "entries_in_dir")
}

// BenchmarkListChildren is one 500-row page of a directory listing, in memory.
func BenchmarkListChildren(b *testing.B) { benchList(b, benchEnv) }

// BenchmarkListChildrenPostgreSQL is the same page on the backend a deployment runs: the only
// figure that can be held up against another product, because it includes the round trip and the
// query plan.
func BenchmarkListChildrenPostgreSQL(b *testing.B) { benchList(b, benchEnvPG) }

// benchResolve measures the resolution a client does before every request, on a path
// six segments deep.
func benchResolve(b *testing.B, env func(*testing.B, int) (store.Store, store.Node, uuid.UUID)) {
	st, dir, owner := env(b, 100)
	ctx := context.Background()

	parent := dir.ID
	for i := 0; i < 4; i++ {
		next := mkdirBench(b, st, owner, parent, fmt.Sprintf("level-%d", i))
		parent = next.ID
	}
	// The leaf references a real blob row: the memory backend would accept any UUID, but
	// the PostgreSQL fixture would fault on nodes_blob_id_fkey, and the two variants must
	// build the same tree. BlobIDFor is the entry point the upload path itself uses.
	leafHash := [32]byte{9}
	leafBlob := st.BlobIDFor(ctx, leafHash, 8)
	if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: parent, Name: "leaf.bin",
			Size: 8, Mime: "text/plain", ETag: `"e"`,
			BlobID: leafBlob, BlobHash: leafHash,
		})
		return cerr
	}); err != nil {
		b.Fatal(err)
	}
	rel := "listing/level-0/level-1/level-2/level-3/leaf.bin"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Resolve(ctx, owner, rel); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolvePath is the six-segment resolution in memory.
func BenchmarkResolvePath(b *testing.B) { benchResolve(b, benchEnv) }

// BenchmarkResolvePathPostgreSQL is the same resolution on the backend a deployment runs.
// The two differ by the round trip: the in-memory backend walks a map, while PostgreSQL pays
// network latency per query, which is exactly the cost a path-walking resolver multiplies by
// depth.
func BenchmarkResolvePathPostgreSQL(b *testing.B) { benchResolve(b, benchEnvPG) }

// BenchmarkCreateFile measures the write path a sync client hits hardest: one new file in one
// transaction, the way the upload route does it. The fixture is rebuilt each iteration because a
// create needs a free name.
func BenchmarkCreateFile(b *testing.B) {
	for i := 0; i < b.N; i++ {
		st, dir, owner := benchEnv(b, 1)
		ctx := context.Background()
		b.StartTimer()
		if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, cerr := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: dir.ID, Name: "new.bin",
				Size: 8, Mime: "text/plain", ETag: `"e"`,
				BlobID: uuidPtr(), BlobHash: [32]byte{7},
			})
			return cerr
		}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
	}
}
