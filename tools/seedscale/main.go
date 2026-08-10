// seedscale populates a database with a chosen number of accounts, folders, files and audit
// events, so the restore drill can be measured at more than one data volume.
//
// Why this exists: deploy/backup/restore-drill.md records a single measurement — 78 KiB, about
// 1.2 seconds — and then says, correctly, that this cannot be extrapolated to 40 TiB. The
// unanswerable question behind that sentence is not "what is the RTO at 40 TiB" (nobody can
// answer it here) but "does the cost grow with the data volume in a predictable way, or does it
// blow up past some threshold". Two measurements an order of magnitude apart answer the second
// question, which is what makes any extrapolation honest.
//
// It goes through the store rather than inserting rows by hand: the point is to measure the real
// schema, including the audit hash chain, and hand-written INSERTs would produce a database the
// drill then reports as broken for reasons that have nothing to do with restore.
//
//	go run ./tools/seedscale -dsn "$DATABASE_URL" -users 20 -files 400 -events 5000
//
// The schema must already exist (`newcloud migrate` first).
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "database to populate")
	users := flag.Int("users", 20, "number of accounts")
	files := flag.Int("files", 400, "number of files across those accounts")
	events := flag.Int("events", 5000, "number of audit events")
	blobRoot := flag.String("blob-root", "",
		"local blob root to write content into (matches storage.local.root in the config); "+
			"without it the seeded files have no bytes and the drill reports them as unservable")
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "seedscale: -dsn (or DATABASE_URL) is required")
		os.Exit(2)
	}
	if err := run(*dsn, *users, *files, *events, *blobRoot); err != nil {
		fmt.Fprintf(os.Stderr, "seedscale: %v\n", err)
		os.Exit(1)
	}
}

func run(dsn string, users, files, events int, blobRoot string) error {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// The schema must already exist: this seeds a database, it does not create one. Failing loudly
	// is better than half-seeding a schema that is about to change under us.
	var haveMigrations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&haveMigrations); err != nil {
		return fmt.Errorf("the schema is not present (run `newcloud migrate` first): %w", err)
	}

	m := store.NewPG(pool)
	// Audit writes are chained and transactional. Without this the store refuses to commit any
	// operation at all — the fail-closed policy in deploy/audit-failure-policy.md — so seeding
	// stops at the first audited write rather than producing a half-populated database.
	m.EnableAudit()
	start := time.Now()

	// One blob shared by every file. Content addressing stores identical bytes once, so the drill
	// has a single object to verify. What this measures is metadata volume, which is what pg_dump
	// mostly moves.
	content := strings.Repeat("seedscale ", 64)
	hash := sha256.Sum256([]byte(content))

	var blobID uuid.UUID
	if err := m.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		blobID = m.BlobIDFor(ctx, hash, int64(len(content)))
		return nil
	}); err != nil {
		return fmt.Errorf("reserve blob: %w", err)
	}

	// The bytes have to exist, not just the metadata row.
	//
	// The first version of this tool skipped this, and the drill immediately reported
	// "the restored copy cannot serve it: blob: not found" for every file — which is the drill
	// doing its job. A seed that writes rows without content produces a database that looks
	// populated and is not, which is precisely the state the drill exists to detect.
	if blobRoot != "" {
		if err := writeBlob(blobRoot, hash, []byte(content)); err != nil {
			return fmt.Errorf("write blob content: %w", err)
		}
		fmt.Printf("blob %s (%d bytes) written to %s\n", blobID, len(content), blobRoot)
	} else {
		fmt.Printf("blob %s (%d bytes); -blob-root not set, content NOT written "+
			"(the drill will report every file as unservable)\n", blobID, len(content))
	}

	// Accounts, each with a root and a small folder tree. Created through UpsertIdPUser because
	// that is the only path that creates an account in the SQL store — the memory store has a
	// convenience AddUser, and using it here would mean this tool could not seed the backend the
	// drill actually runs against.
	var owners []uuid.UUID
	for i := 0; i < users; i++ {
		name := fmt.Sprintf("seed%03d", i)
		var u store.User
		if err := m.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
				Subject: "seed-" + name, Username: name,
				Email: name + "@seed.example.test", Role: "user",
			})
			u = got
			return uerr
		}); err != nil {
			return fmt.Errorf("account %s: %w", name, err)
		}
		if _, err := m.EnsureRoot(ctx, u.ID); err != nil {
			return fmt.Errorf("root for %s: %w", name, err)
		}
		owners = append(owners, u.ID)
	}
	fmt.Printf("accounts: %d\n", len(owners))

	// Files, spread across the accounts and over ten folders each, so the tree is not flat.
	for i := 0; i < files; i++ {
		owner := owners[i%len(owners)]
		folder := fmt.Sprintf("folder-%02d", (i/20)%10)
		err := m.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			root, err := tx.EnsureRoot(ctx, owner)
			if err != nil {
				return err
			}
			dir, err := ensureDir(ctx, tx, owner, root.ID, folder)
			if err != nil {
				return err
			}
			_, err = tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: dir, Name: fmt.Sprintf("file-%05d.txt", i),
				Size: int64(len(content)), Mime: "text/plain",
				BlobID: blobID, BlobHash: hash,
			})
			return err
		})
		if err != nil {
			return fmt.Errorf("file %d: %w", i, err)
		}
		if (i+1)%200 == 0 {
			fmt.Printf("  files %d/%d\n", i+1, files)
		}
	}
	fmt.Printf("files: %d\n", files)

	// Audit events go through the real chain, so each links to the previous hash and the drill can
	// verify the chain after restore. Batched, because one transaction per event is dominated by
	// commit cost rather than by the data — which would make the seed itself the slow part.
	const batch = 500
	for done := 0; done < events; done += batch {
		n := batch
		if done+n > events {
			n = events - done
		}
		err := m.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			for i := 0; i < n; i++ {
				if err := tx.Audit(ctx, store.AuditEntry{
					ActorKind: "user", ActorEmail: "seed@seed.example.test",
					Action: "seed.event", TargetKind: "none",
					Result: "ok", Metadata: map[string]any{"seq": done + i},
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("audit batch at %d: %w", done, err)
		}
	}
	fmt.Printf("audit events: %d\n", events)

	var dbSize string
	_ = pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_database_size(current_database()))`).Scan(&dbSize)
	fmt.Printf("seeded in %s: %d accounts, %d files, %d audit events, database %s\n",
		time.Since(start).Round(time.Millisecond), len(owners), files, events, dbSize)
	return nil
}

// ensureDir finds or creates the named folder under parent.
//
// It looks the folder up by listing the parent's children rather than by resolving a path: Resolve
// lives on the store, not on the transaction, and calling it inside one would start a second
// transaction against the same pool while this one is open.
func ensureDir(ctx context.Context, tx store.Tx, owner, parent uuid.UUID, name string) (uuid.UUID, error) {
	children, err := tx.Children(ctx, parent, 1000)
	if err != nil {
		return uuid.Nil, err
	}
	for _, ch := range children {
		if ch.Name == name && ch.IsDir() {
			return ch.ID, nil
		}
	}
	n, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: parent, Name: name})
	if err != nil {
		return uuid.Nil, err
	}
	return n.ID, nil
}

// writeBlob places content at the layout the local backend uses: <root>/<hex[0:2]>/<hex[2:4]>/<hex>.
//
// Written directly rather than through the backend so this tool does not have to construct a
// config; the layout is the one documented in internal/blob/local, and getting it wrong here shows
// up immediately as the drill reporting the blobs unservable.
func writeBlob(root string, hash [32]byte, content []byte) error {
	hexHash := fmt.Sprintf("%x", hash)
	dir := filepath.Join(root, hexHash[0:2], hexHash[2:4])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, hexHash), content, 0o644)
}
