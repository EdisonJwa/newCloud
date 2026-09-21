package main

// Drill helper tests.
//
// The drill itself needs a live PostgreSQL and the client tools, so its end-to-end path is
// exercised by running it (see docs/reference/implementation-record.md §3). What can be tested without a database is the
// part where a mistake would silently weaken the drill: the identifier guard that stands
// between the table list and string-built SQL.

import (
	"context"
	"time"

	"bytes"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsBareIdent(t *testing.T) {
	valid := []string{"users", "audit.events", "acl_entries", "group_members", "a1"}
	for _, s := range valid {
		if !isBareIdent(s) {
			t.Fatalf("%q should be accepted", s)
		}
	}

	// The list is a compile-time constant, but the guard is what makes interpolating it into
	// `SELECT count(*) FROM <t>` defensible: anything that could carry SQL is refused.
	invalid := []string{
		"", "Users", "user s", "users; DROP TABLE users", "users--",
		"audit..events", ".users", "users.", "users\"", "1users",
	}
	for _, s := range invalid {
		if isBareIdent(s) {
			t.Fatalf("%q must be refused", s)
		}
	}
}

// TestDrillTablesAreBareIdentifiers ties the guard to the actual list, so adding a table with
// a name the guard rejects fails here rather than at drill time.
func TestDrillTablesAreBareIdentifiers(t *testing.T) {
	for _, tbl := range drillTables {
		if !isBareIdent(tbl) {
			t.Fatalf("drillTables contains %q, which the identifier guard rejects", tbl)
		}
	}
	for _, tbl := range drillAuditTables {
		if !isBareIdent(tbl) {
			t.Fatalf("drillAuditTables contains %q, which the identifier guard rejects", tbl)
		}
	}
}

// TestHumanBytes is cosmetic but a wrong unit makes the RTO report unreadable.
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		512:         "512 B",
		2048:        "2.0 KiB",
		1024 * 1024: "1.0 MiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestCopyTreeSkipsSpool pins the exclusion that keeps partial uploads out of the copy.
//
// <root>/tmp holds in-progress uploads. Copying it would put bytes in the restored tree that
// no metadata references, so a restore would carry content nothing can address.
func TestCopyTreeSkipsSpool(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWrite := func(rel string) {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("ab/cd/blob1")
	mustWrite("tmp/in-progress-upload")

	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dst, "ab", "cd", "blob1")); err != nil {
		t.Fatalf("a real blob must be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "tmp")); err == nil {
		t.Fatal("the spool directory must not be copied")
	}
}

// TestDigestOfDetectsOneBitFlip is the property the blob check rests on.
func TestDigestOfDetectsOneBitFlip(t *testing.T) {
	orig := []byte("the quick brown fox")
	sum, n, err := digestOf(bytes.NewReader(orig))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(orig)) {
		t.Fatalf("want %d bytes, got %d", len(orig), n)
	}

	flipped := append([]byte(nil), orig...)
	flipped[0] ^= 0xFF
	sum2, _, err := digestOf(bytes.NewReader(flipped))
	if err != nil {
		t.Fatal(err)
	}
	if sum == sum2 {
		t.Fatal("a single bit flip must change the digest, or the blob check proves nothing")
	}
}

// TestDrillCoversEveryTable keeps the drill's table list honest.
//
// The list is deliberately explicit rather than discovered: "every table matched" is only
// meaningful if the set was decided in advance. But an explicit list rots — a migration adds a
// table, nobody updates the drill, and the new table is silently exempt from the restore check
// forever. That is the failure mode this test exists to prevent, and it had already happened:
// name_conflict_renames and group_role_map were both missing when this test was written.
//
// It reads the live schema, so it needs a database. Without one it skips rather than passing
// vacuously — a completeness check that always passes is worse than none.
func TestDrillCoversEveryTable(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; cannot compare the drill list against a schema")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Every table, WITH its parent when it is a partition.
	//
	// Asking for the parent rather than matching on a name is the whole point. The previous version
	// skipped anything ending in "_default", which happened to catch audit.events_default and
	// happened to also catch sessions_default — a partition of a table the drill DOES cover. Because
	// the suffix check ran before the coverage check, a future partitioned table whose parent was
	// NOT drilled would have been skipped silently: the exemption pattern would have hidden exactly
	// the gap this test exists to find.
	rows, err := pool.Query(ctx, `
		SELECT cns.nspname || '.' || child.relname,
		       COALESCE(pns.nspname || '.' || parent.relname, '')
		FROM pg_class child
		JOIN pg_namespace cns ON cns.oid = child.relnamespace
		LEFT JOIN pg_inherits i ON i.inhrelid = child.oid
		LEFT JOIN pg_class parent ON parent.oid = i.inhparent
		LEFT JOIN pg_namespace pns ON pns.oid = parent.relnamespace
		WHERE cns.nspname IN ('public', 'audit') AND child.relkind = 'r'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer rows.Close()

	// Tables the drill deliberately does not count, with the reason each is exempt.
	//
	// Used, not just declared. The map existed with this comment above it and the loop below
	// filtered by PATTERN instead — so the comment described a policy the code did not implement,
	// and the map was dead. Same class as the two guards corrected in the preceding rounds: the
	// stated mechanism and the actual one had diverged, and the stated one read better.
	exempt := map[string]string{
		"public.schema_migrations": "migration bookkeeping, recreated by the restore itself",
	}

	covered := map[string]bool{}
	for _, t := range drillTables {
		covered["public."+t] = true
	}
	for _, t := range drillAuditTables {
		covered[t] = true
	}

	var missing []string
	for rows.Next() {
		var name, parent string
		if err := rows.Scan(&name, &parent); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ok := exempt[name]; ok {
			continue
		}
		if parent != "" {
			// A partition is covered when its PARENT is: the parent's rows live in the partition, so
			// a dump of the parent carries them and counting the partition separately double-counts.
			// The check is therefore on the parent, and a partition of an undrilled table is
			// reported as its parent rather than hidden by a naming convention.
			if !covered[parent] {
				missing = append(missing, parent+" (via partition "+name+")")
			}
			continue
		}
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(missing) > 0 {
		t.Fatalf("the drill does not verify %d table(s): %v\n"+
			"Add them to drillTables (or drillAuditTables for partitioned audit tables) so a "+
			"restore is proven to carry them.", len(missing), missing)
	}
}

// TestAnEmptyDrillIsNotAPass is the empty-collection trap, applied to the operator tool.
//
// The drill reported PASS with "rows compared 0, audit chain 0 events" against a database that had
// nothing in it. A green restore drill is evidence an operator signs off on; one that compared
// nothing is not evidence of anything, and the most likely cause — a mistyped LIVE_DSN — is exactly
// the mistake that would otherwise go unnoticed.
func TestAnEmptyDrillIsNotAPass(t *testing.T) {
	empty := drillResult{Tables: 24}
	problems := emptyDrillProblems(empty)
	if len(problems) == 0 {
		t.Fatal("a drill that compared no rows, no audit events and no blobs must not pass")
	}
	if !strings.Contains(problems[0], "empty") {
		t.Fatalf("the problem should say the database was empty, got %q", problems[0])
	}
	empty.Problems = append(empty.Problems, problems...)
	if empty.OK() {
		t.Fatal("an empty drill must not be OK()")
	}

	// Any one of the three kinds of evidence is enough: a deployment may legitimately have no
	// blobs yet, or no audit events, but not all three and still claim a restore was verified.
	for _, res := range []drillResult{
		{RowsCompare: 1},
		{ChainLength: 1},
		{BlobFiles: 1},
	} {
		if got := emptyDrillProblems(res); len(got) != 0 {
			t.Fatalf("a drill that compared something must not be called empty: %+v -> %v", res, got)
		}
	}
}
