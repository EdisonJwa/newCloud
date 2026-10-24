package store_test

// Benchmarks for the metadata operations a sync client actually feels.
//
// The competitor comparison's §8 says the number that matters is "files/second on PROPFIND over
// a large directory", because slow listing is the single most-cited complaint about the systems
// this is measured against. These are the store-level halves of that number: the listing query,
// the path resolution a client does before every request, and the write path.
//
// They are benchmarks, not tests: they do not run in CI, and the fixture size comes from
// BENCH_CHILDREN so the same harness can answer "10k" and "100k" without editing code.
//
//	go test ./internal/store -bench . -run '^$' -benchtime 1x
//	BENCH_CHILDREN=100000 go test ./internal/store -bench ListChildren -run '^$' -benchtime 1x

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/google/uuid"

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

// benchEnv builds a store with one directory holding n files, and returns that directory.
func benchEnv(b *testing.B, n int) (store.Store, store.Node, uuid.UUID) {
	b.Helper()
	st := store.NewMemory()
	ctx := context.Background()
	owner := uuid.New()

	root, err := st.EnsureRoot(ctx, owner)
	if err != nil {
		b.Fatalf("root: %v", err)
	}
	var dir store.Node
	if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		d, derr := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: root.ID, Name: "listing"})
		dir = d
		return derr
	}); err != nil {
		b.Fatalf("mkdir: %v", err)
	}

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

func uuidPtr() uuid.UUID { return uuid.New() }

// BenchmarkListChildren measures one page of a directory listing.
func BenchmarkListChildren(b *testing.B) {
	n := benchChildren()
	st, dir, _ := benchEnv(b, n)
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

// BenchmarkResolvePath measures the resolution a client does before every request, on a path
// six segments deep.
func BenchmarkResolvePath(b *testing.B) {
	st, dir, owner := benchEnv(b, 100)
	ctx := context.Background()

	parent := dir.ID
	for i := 0; i < 4; i++ {
		var next store.Node
		if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			d, derr := tx.CreateDir(ctx, store.NewNode{
				OwnerID: owner, ParentID: parent, Name: fmt.Sprintf("level-%d", i),
			})
			next = d
			return derr
		}); err != nil {
			b.Fatal(err)
		}
		parent = next.ID
	}
	if err := st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: parent, Name: "leaf.bin",
			Size: 8, Mime: "text/plain", ETag: `"e"`,
			BlobID: uuidPtr(), BlobHash: [32]byte{9},
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

// BenchmarkCreateFile measures the write path a sync client hits hardest: one new file in one
// transaction, the way the upload route does it. The fixture is rebuilt each iteration because
// a create needs a free name.
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
