package main

// `newcloud drill` — the restore drill from the requirements (INV-08).
//
// The requirement's wording is that recovery capability is not proven by having backup
// files: "恢复验证不等于『有备份文件』". A backup that has never been restored is a rumour.
// Until this command existed, the project had a drill *procedure* in deploy/backup/ that
// had never been run, so INV-08 was an untested claim.
//
// Why this is a Go subcommand rather than a shell script: the interesting assertions are not
// shell-shaped. Comparing row counts per table, verifying the audit hash chain over a
// restored copy, and reporting a verdict all need the schema knowledge that lives in this
// binary. A script that shelled out to psql would also be untestable here — it could only be
// run on a machine with the deployment's tooling.
//
// The drill never touches the live database beyond reading it:
//
//  1. `pg_dump` the live database, exactly as the nightly job does.
//  2. Verify the dump's table of contents (a truncated file looks fine in `ls`).
//  3. Create a scratch database and restore into it.
//  4. Compare row counts per table; a restore that loses rows is the failure this catches.
//  5. Verify the audit chain *in the restored copy* — the chain is what makes tampering
//     detectable, and a restore that silently breaks it turns the log into inert rows.
//  6. Measure elapsed time against the documented RTO and print a verdict.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// drillTables is the set of tables whose row counts must survive a restore.
//
// It is explicit rather than discovered, because "every table matched" is only meaningful
// if the list was decided in advance: a table added later and forgotten here would be
// silently exempt. The test that checks this list against the schema is
// TestDrillCoversEveryTable.
var drillTables = []string{
	"users", "sessions", "app_passwords", "blobs", "nodes", "versions",
	"dead_props", "lock_tokens", "shares", "share_recipients", "jobs",
	"settings", "acl_entries", "groups", "group_members", "group_role_map",
	"download_sessions", "share_otp_attempts", "share_upload_events",
	"share_password_attempts", "scim_pending_changes",
	// Added after the list was first written, and found by TestDrillCoversEveryTable rather
	// than by anyone noticing: a group-to-role mapping and the record of names the name_key
	// migration had to rename. Both must survive a restore, and neither was being checked.
	"name_conflict_renames",
}

// drillAuditTables are the partitioned audit tables, counted via their parent.
var drillAuditTables = []string{"audit.events", "audit.anchors", "audit.prune_marks"}

// drillResult is the outcome of one drill.
type drillResult struct {
	Elapsed     time.Duration
	DumpBytes   int64
	Tables      int
	RowsCompare int64
	ChainLength int64
	ChainOK     bool

	// Blob content verification. Skipped is distinct from "verified zero files": one means
	// the check could not run, the other means it ran and found nothing to check.
	BlobFiles      int
	BlobVerified   int
	BlobBytes      int64
	BlobSkipped    bool
	BlobSkipReason string

	Problems []string
}

// OK reports whether every check passed.
func (r drillResult) OK() bool { return len(r.Problems) == 0 }

// drillCommand runs the restore drill.
//
// Arguments: `drill [-keep]`. Database endpoints come from the environment, mirroring the
// backup scripts, so the same documentation applies to both.
func drillCommand(args []string) error {
	keep := false
	for _, a := range args {
		if a == "-keep" {
			keep = true
		}
	}

	liveDSN := os.Getenv("LIVE_DSN")
	if liveDSN == "" {
		// DATABASE_URL is what the server uses; accepting it means an operator does not have
		// to learn a second variable name for the same thing.
		liveDSN = os.Getenv("DATABASE_URL")
	}
	if liveDSN == "" {
		return fmt.Errorf("drill: set LIVE_DSN (or DATABASE_URL) to the live database")
	}
	scratchDSN := os.Getenv("SCRATCH_DSN")
	if scratchDSN == "" {
		return fmt.Errorf("drill: set SCRATCH_DSN to a scratch database; " +
			"the drill never restores over the live one")
	}
	if liveDSN == scratchDSN {
		// Refusing to run is the whole safety property. A drill that can destroy the thing it
		// is validating is worse than no drill.
		return fmt.Errorf("drill: SCRATCH_DSN must differ from LIVE_DSN")
	}

	// Where the dump lives depends on how pg_dump is invoked. With DRILL_PG_CONTAINER set the
	// tools run inside the container, so a host path would not be visible to them.
	var dir string
	if container := os.Getenv("DRILL_PG_CONTAINER"); container != "" {
		dir = "/tmp/newcloud-drill-" + fmt.Sprint(time.Now().UnixNano())
		// mkdir as a direct command rather than `sh -c`: the container runs as an
		// unprivileged user in some deployments, and quoting a path through a shell only adds
		// a way for that to fail confusingly.
		if err := runTool(context.Background(), "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("create scratch directory in container: %w", err)
		}
		if !keep {
			defer func() {
				_ = runTool(context.Background(), "rm", "-rf", dir)
			}()
		} else {
			fmt.Printf("drill: keeping artefacts in container:%s\n", dir)
		}
	} else {
		var mkErr error
		dir, mkErr = os.MkdirTemp("", "newcloud-drill-*")
		if mkErr != nil {
			return mkErr
		}
		if !keep {
			defer os.RemoveAll(dir)
		} else {
			fmt.Printf("drill: keeping artefacts in %s\n", dir)
		}
	}

	cfg, cerr := config.Load(os.Getenv("NEWCLOUD_CONFIG"))
	if cerr != nil {
		// The drill needs the storage configuration to know where the blobs are. Refusing is
		// better than silently skipping blob verification: a drill that quietly checks less
		// than it claims is worse than one that fails to start.
		return fmt.Errorf("drill: load configuration (NEWCLOUD_CONFIG): %w", cerr)
	}

	res, err := runDrill(context.Background(), cfg, liveDSN, scratchDSN, dir)
	if err != nil {
		return err
	}
	printDrillResult(res)
	if !res.OK() {
		return fmt.Errorf("drill: FAILED (%d problem(s))", len(res.Problems))
	}
	return nil
}

// toolDSN returns the DSN to hand to pg_dump/pg_restore.
//
// When the tools run inside a container, the host's address for PostgreSQL is wrong there:
// a deployment publishes the database on a host port that the container itself reaches under
// a different one. DRILL_DUMP_DSN and DRILL_RESTORE_DSN override the address for the tools
// while the Go process keeps using the host's. Getting this wrong produces "Connection
// refused" from inside the container, which reads as a broken database rather than a
// misconfigured drill.
func toolDSN(explicit, fallback string) string {
	if explicit != "" {
		return explicit
	}
	return fallback
}

// runDrill performs the drill and returns what it measured.
func runDrill(ctx context.Context, cfg config.Config, liveDSN, scratchDSN, dir string) (drillResult, error) {
	dumpDSN := toolDSN(os.Getenv("DRILL_DUMP_DSN"), liveDSN)
	restoreDSN := toolDSN(os.Getenv("DRILL_RESTORE_DSN"), scratchDSN)
	var res drillResult
	start := time.Now()

	// ---- 1. Dump ----
	dumpPath := filepath.Join(dir, "newcloud.dump")
	if err := runTool(ctx, "pg_dump",
		"--format=custom", "--compress=9",
		"--dbname="+dumpDSN, "--file="+dumpPath); err != nil {
		return res, fmt.Errorf("pg_dump: %w", err)
	}
	res.DumpBytes = fileSize(ctx, dumpPath)
	if res.DumpBytes == 0 {
		return res, fmt.Errorf("pg_dump produced an empty file")
	}

	// ---- 2. The dump must be readable, not merely present ----
	if err := runTool(ctx, "pg_restore", "--list", dumpPath); err != nil {
		return res, fmt.Errorf("the dump is unreadable (pg_restore --list): %w", err)
	}

	// ---- 3. Restore into the scratch database ----
	live, err := pgx.Connect(ctx, liveDSN)
	if err != nil {
		return res, fmt.Errorf("connect live: %w", err)
	}
	defer live.Close(ctx)

	counts, err := tableCounts(ctx, live)
	if err != nil {
		return res, fmt.Errorf("count live rows: %w", err)
	}

	scratch, err := pgx.Connect(ctx, scratchDSN)
	if err != nil {
		return res, fmt.Errorf("connect scratch: %w", err)
	}
	defer scratch.Close(ctx)

	// The schema is dropped rather than the database: creating a database requires
	// CREATEDB, which the drill has no business demanding.
	if _, err := scratch.Exec(ctx, `DROP SCHEMA IF EXISTS audit CASCADE;
		DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`); err != nil {
		return res, fmt.Errorf("reset scratch schema: %w", err)
	}

	if err := runTool(ctx, "pg_restore",
		"--no-owner", "--no-privileges", "--no-comments",
		"--dbname="+restoreDSN, dumpPath); err != nil {
		return res, fmt.Errorf("pg_restore: %w", err)
	}

	// ---- 4. Row counts must match, table by table ----
	restored, err := tableCounts(ctx, scratch)
	if err != nil {
		return res, fmt.Errorf("count restored rows: %w", err)
	}
	res.Tables = len(restored)

	var names []string
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := counts[name]
		got := restored[name]
		res.RowsCompare += want
		if want != got {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s: live has %d rows, restored has %d", name, want, got))
		}
	}
	// A table that exists only in the restored copy is also a discrepancy: it means the
	// dump carried something the live database does not have, which a clean restore cannot
	// produce.
	for name, got := range restored {
		if _, ok := counts[name]; !ok && got > 0 {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s: exists only in the restored copy (%d rows)", name, got))
		}
	}

	// ---- 5. The audit chain must verify in the restored copy ----
	//
	// This is the assertion that makes the drill worth running. A restore that loses or
	// reorders audit rows produces a database that *looks* fine — the tables are there, the
	// counts match — but whose tamper-evidence is gone.
	chainOK, chainLen, chainErrs := verifyRestoredChain(ctx, scratchDSN)
	res.ChainOK = chainOK
	res.ChainLength = chainLen
	res.Problems = append(res.Problems, chainErrs...)

	// ---- 6. Blob content ----
	//
	// The metadata checks above prove the tree survives. They say nothing about the bytes,
	// and a restore that brings back a consistent node tree pointing at blobs that were never
	// copied produces a deployment that lists every file and opens none of them.
	if blobs, berr := func() (blobCheck, error) {
		restoredMeta, merr := openRestoredMetadata(ctx, scratchDSN)
		if merr != nil {
			return blobCheck{}, merr
		}
		liveContent, cerr := openContentStore(cfg, resolverFor(restoredMeta))
		if cerr != nil {
			return blobCheck{}, cerr
		}
		return verifyBlobs(ctx, cfg, liveContent, restoredMeta, filepath.Join(dir, "blobs")), nil
	}(); berr != nil {
		res.Problems = append(res.Problems, "blob verification: "+berr.Error())
	} else {
		res.BlobFiles = blobs.Files
		res.BlobVerified = blobs.Verified
		res.BlobBytes = blobs.Bytes
		res.BlobSkipped = blobs.Skipped
		res.BlobSkipReason = blobs.SkipReason
		res.Problems = append(res.Problems, blobs.Problems...)
	}

	res.Elapsed = time.Since(start)
	res.Problems = append(res.Problems, emptyDrillProblems(res)...)
	return res, nil
}

// emptyDrillProblems refuses to call an empty exercise a successful one.
//
// A drill against a database with no rows, no audit events and no blobs compared nothing and
// verified nothing, and used to print "VERDICT: PASS — the backup restores and the audit chain
// survives" all the same. That is the most likely way to get a green drill without evidence: a
// mistyped LIVE_DSN, a fresh database, a restored-but-wrong instance. The verdict has to be earned
// by comparing data, so an empty run is a problem, not a pass.
func emptyDrillProblems(res drillResult) []string {
	if res.RowsCompare > 0 || res.ChainLength > 0 || res.BlobFiles > 0 {
		return nil
	}
	return []string{"the live database is empty (0 rows, 0 audit events, 0 blobs): this drill " +
		"compared nothing, so PASS would be a claim it did not test. Check that LIVE_DSN points " +
		"at the database you meant to drill."}
}

// openRestoredMetadata connects to the restored database through the normal store.
func openRestoredMetadata(ctx context.Context, dsn string) (store.Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return store.NewPG(pool), nil
}

// openContentStore builds the live content store for comparison.
//
// Only the local blob layout is supported, matching verifyBlobs: other backends would need a
// bucket-level copy that this command does not perform.
func openContentStore(cfg config.Config, resolve blobstore.NodeResolver) (layout.ContentStore, error) {
	if cfg.Storage.Backend != "local" || cfg.Storage.Layout != config.LayoutBlob {
		return nil, nil
	}
	ls, err := local.New(cfg.Storage.Local.Root)
	if err != nil {
		return nil, err
	}
	// The resolver comes from the *restored* metadata on purpose. Both stores are asked for
	// the same path, so using one resolver makes the comparison about the bytes and not about
	// whether two metadata stores happen to agree.
	return blobstore.New(ls, resolve)
}

// tableCounts returns row counts for every table the drill checks.
func tableCounts(ctx context.Context, conn *pgx.Conn) (map[string]int64, error) {
	out := map[string]int64{}

	for _, t := range drillTables {
		var n int64
		// Identifiers cannot be parameterised; the list is a compile-time constant, and the
		// quoting below refuses anything that is not a bare identifier.
		if !isBareIdent(t) {
			return nil, fmt.Errorf("refusing to count %q: not a bare identifier", t)
		}
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			// A table missing from the live database is a schema-level problem worth
			// reporting rather than skipping.
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	for _, t := range drillAuditTables {
		var n int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// verifyRestoredChain re-verifies the hash chain in the restored copy.
//
// It reuses the store's own reader rather than reconstructing events here. The chain hash
// covers every column, so a second reconstruction in this file would drift from the real one
// the moment a field is added — and it would drift silently, because the symptom is "the
// restored chain does not verify", which looks like a failed restore rather than a stale
// verifier.
func verifyRestoredChain(ctx context.Context, scratchDSN string) (bool, int64, []string) {
	pool, err := pgxpool.New(ctx, scratchDSN)
	if err != nil {
		return false, 0, []string{"connect to the restored database: " + err.Error()}
	}
	defer pool.Close()

	restored := store.NewPG(pool)

	// The rows are read in pages so a large log does not have to fit in memory, and the
	// chain is verified across the whole run: a chain that verifies per page but not across
	// page boundaries would be a false pass.
	var (
		all    []audit.Event
		after  int64
		pageSz = 1000
	)
	for {
		rows, rerr := restored.AuditRows(ctx, after, pageSz)
		if rerr != nil {
			return false, 0, []string{"read restored audit rows: " + rerr.Error()}
		}
		if len(rows) == 0 {
			break
		}
		all = append(all, rows...)
		after = rows[len(rows)-1].ID
		if len(rows) < pageSz {
			break
		}
	}

	if len(all) == 0 {
		// An empty chain is trivially valid, and saying so is better than implying the drill
		// proved something about data it did not have.
		return true, 0, nil
	}

	badIndex, verr := audit.VerifyChain(all)
	if verr != nil {
		return false, int64(len(all)), []string{fmt.Sprintf(
			"the restored audit chain does not verify at row %d of %d: %v",
			badIndex, len(all), verr)}
	}
	return true, int64(len(all)), nil
}

// runTool runs an external command and returns its combined output on failure.
//
// The container indirection exists because the deployment this drill targets runs PostgreSQL
// in Docker, where pg_dump and pg_restore live inside the container rather than on the host.
// Without it the drill could only be run on a machine with the client tools installed, which
// is exactly the situation that made the original procedure "documented but never executed".
//
// DRILL_PG_CONTAINER names the container; when empty the tools are invoked directly, which
// is what a packaged installation does.
func runTool(ctx context.Context, name string, args ...string) error {
	var cmd *exec.Cmd
	if c := os.Getenv("DRILL_PG_CONTAINER"); c != "" {
		// docker exec <c> pg_dump ... — the arguments are passed through verbatim.
		dockerArgs := append([]string{"exec", c, name}, args...)
		cmd = exec.CommandContext(ctx, "docker", dockerArgs...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}

	// Output is captured rather than streamed: a successful pg_restore is silent, and the
	// interesting case is the failure message.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// isBareIdent reports whether s is a plain lowercase identifier, optionally schema-qualified.
func isBareIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			ok := (c >= 'a' && c <= 'z') || c == '_' || (i > 0 && c >= '0' && c <= '9')
			if !ok {
				return false
			}
		}
	}
	return true
}

// printDrillResult reports what the drill measured.
//
// The RTO comparison is against the documented target so an operator sees "this took 40
// seconds against a 1-hour target" rather than a bare duration they must interpret.
func printDrillResult(res drillResult) {
	fmt.Printf("\nrestore drill\n")
	fmt.Printf("  dump size        %s\n", humanBytes(res.DumpBytes))
	fmt.Printf("  tables compared  %d\n", res.Tables)
	fmt.Printf("  rows compared    %d\n", res.RowsCompare)
	fmt.Printf("  audit chain      %d events, verified=%v\n", res.ChainLength, res.ChainOK)
	if res.BlobSkipped {
		fmt.Printf("  blobs            SKIPPED (%s)\n", res.BlobSkipReason)
	} else {
		fmt.Printf("  blobs            %d files, %d verified, %s\n",
			res.BlobFiles, res.BlobVerified, humanBytes(res.BlobBytes))
	}
	fmt.Printf("  elapsed          %s\n", res.Elapsed.Round(time.Millisecond))

	const rto = time.Hour
	if res.Elapsed < rto {
		fmt.Printf("  RTO              %s (target %s) — within target\n",
			res.Elapsed.Round(time.Second), rto)
	} else {
		fmt.Printf("  RTO              %s (target %s) — EXCEEDED\n",
			res.Elapsed.Round(time.Second), rto)
	}

	if len(res.Problems) == 0 {
		fmt.Printf("\n  VERDICT: PASS — the backup restores and the audit chain survives\n\n")
		return
	}
	fmt.Printf("\n  VERDICT: FAIL\n")
	for _, p := range res.Problems {
		fmt.Printf("    - %s\n", p)
	}
	fmt.Println()
}

// humanBytes formats a byte count.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// fileSize reports a file's size, reaching into the container when the tools run there.
//
// os.Stat would report 0 for a container-local path, and a zero-size check is the one that
// distinguishes "the dump was written" from "the dump was truncated" — so getting it wrong
// would silently disable that assertion.
func fileSize(ctx context.Context, path string) int64 {
	if container := os.Getenv("DRILL_PG_CONTAINER"); container != "" {
		out, err := exec.CommandContext(ctx, "docker", "exec", container, "stat", "-c", "%s", path).Output()
		if err != nil {
			return 0
		}
		var n int64
		_, _ = fmt.Sscan(string(out), &n)
		return n
	}
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}
