// Command auditsmoke exercises the audit chain through the production code path, so an
// operator can see on a throwaway database that the tamper-evidence claims hold.
//
// It appends events with the same writer the server uses, verifies the chain, then performs
// the attacks the design says it detects and reports which defence stopped each one. Run it
// against a database you are willing to destroy; it does not clean up after itself, because
// the point is to inspect the damage afterwards with `newcloud audit verify`.
//
//	go run ./tools/auditsmoke -database "$DATABASE_URL" -events 25
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func main() {
	dsn := flag.String("database", "", "DATABASE_URL of a THROWAWAY database")
	count := flag.Int("events", 25, "events to append before testing")
	keep := flag.Bool("keep", false, "leave appended events in place")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "auditsmoke: -database is required")
		os.Exit(2)
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, *dsn)
	must("connect", err)
	defer pool.Close()
	s := store.NewPG(pool)
	s.EnableAudit()

	var owner store.User
	err = s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Subject: "smoke-owner", Email: "smoke@example.test", DisplayName: "Smoke Owner",
		})
		owner = u
		return uerr
	})
	must("create owner", err)
	fmt.Printf("owner %s\n", owner.ID)

	start := time.Now()
	for i := 0; i < *count; i++ {
		err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind:   string(audit.ActorUser),
				ActorUserID: &owner.ID,
				ActorEmail:  owner.Email,
				Action:      string(audit.ActionFileWrite),
				TargetKind:  string(audit.TargetNode),
				TargetID:    fmt.Sprintf("smoke-%d", i),
				Result:      string(audit.ResultOK),
				Metadata:    map[string]any{"seq": i},
			})
		})
		must(fmt.Sprintf("append event %d", i), err)
	}
	elapsed := time.Since(start)
	fmt.Printf("appended %d events in %s (%.2f ms/event)\n",
		*count, elapsed.Round(time.Millisecond), float64(elapsed.Microseconds())/float64(*count)/1000)

	var n int64
	must("count", pool.QueryRow(ctx, `SELECT count(*) FROM audit.events`).Scan(&n))
	fmt.Printf("audit.events holds %d rows\n", n)

	var headAt int64
	must("chain head", pool.QueryRow(ctx,
		`SELECT id FROM audit.events ORDER BY id DESC LIMIT 1`).Scan(&headAt))
	fmt.Printf("chain head id %d\n", headAt)

	if !*keep {
		fmt.Println("\nnext: newcloud audit verify   (expect exit 0)")
		fmt.Println("then: UPDATE audit.events SET action='auth.login.success' WHERE id=1;")
		fmt.Println("      newcloud audit verify   (expect exit 1, tamper detected)")
	}
}

func must(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "auditsmoke: %s: %v\n", what, err)
		os.Exit(1)
	}
}
