// Command anchorsmoke publishes a real anchor checkpoint and then truncates the log, so an
// operator can see that `newcloud audit verify` catches what the bare hash chain cannot.
//
// It exists because that is the one claim a deployment cannot check by reading: the chain
// verifies a shortened log perfectly, and only a published checkpoint reveals it. Run it
// against a throwaway database.
//
//	go run ./tools/anchorsmoke -database "$DATABASE_URL" -dir /tmp/anchors
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func main() {
	dsn := flag.String("database", "", "DATABASE_URL of a THROWAWAY database")
	dir := flag.String("dir", "", "anchor directory to publish into (created if missing)")
	sign := flag.Bool("sign", false, "generate a signing key and sign the checkpoints")
	truncate := flag.Int("truncate", 0, "delete this many newest events AFTER publishing")
	flag.Parse()
	if *dsn == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, "anchorsmoke: -database and -dir are required")
		os.Exit(2)
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, *dsn)
	must("connect", err)
	defer pool.Close()
	s := store.NewPG(pool)

	anchor, err := auditverify.NewFileAnchor(*dir)
	must("open anchor", err)

	var pub string
	if *sign {
		signer, serr := auditverify.GenerateSigner()
		must("generate signer", serr)
		anchor = anchor.WithSigner(signer)
		pub = signer.PublicKeyBase64()
		fmt.Printf("public key: %s\n", pub)
	}

	chain := auditverify.NewStoreChain(s)
	head, count, err := chain.Head()
	must("read head", err)
	fmt.Printf("chain head: %s over %d events\n", auditverify.Digest(head), count)

	// Yesterday, because a checkpoint only means something once its day has closed.
	day := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)
	must("publish", anchor.PublishWithCount(day, head, count))
	fmt.Printf("published checkpoint for %s (count %d)\n", day.Format("2006-01-02"), count)

	if *truncate > 0 {
		tag, derr := pool.Exec(ctx,
			`DELETE FROM audit.events WHERE id IN
			   (SELECT id FROM audit.events ORDER BY id DESC LIMIT $1)`, *truncate)
		must("truncate", derr)
		fmt.Printf("DELETED the newest %d events (simulating tampering)\n", tag.RowsAffected())
	}

	fmt.Println("\nnow run:")
	fmt.Printf("  newcloud audit verify -anchors %s", *dir)
	if pub != "" {
		fmt.Printf(" -pubkey %s", pub)
	}
	fmt.Println()
	fmt.Println("expect: exit 1 with \"checkpoints disagree with the chain\"")
}

func must(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "anchorsmoke: %s: %v\n", what, err)
		os.Exit(1)
	}
}
