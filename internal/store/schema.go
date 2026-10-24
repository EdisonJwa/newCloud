package store

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/migrations"
)

// MigrationLockID is the advisory-lock key that serialises migration runs.
//
// `pg_advisory_lock` takes a bigint chosen by the application; this one is the ASCII of
// "newcloud-migrate" folded into a number, so two different tools cannot collide by
// accident.
const MigrationLockID int64 = 0x6e65_7763_6c6f_7564 // "newcloud" in ASCII

// ApplySchema runs the embedded migrations in order.
//
// They are idempotent at the file level only (each migration is written to be
// re-runnable where practical), so the applied set is tracked in a table rather
// than relying on that: a half-applied migration that "succeeds again" silently is
// worse than a loud failure.
//
// The whole run holds a PostgreSQL advisory lock on ONE connection, because two
// concurrent `newcloud migrate` runs — a rolling deploy, or a compose restart while cron
// is running it — otherwise both read "not applied" for the same file and both apply it.
// The loser then fails on a duplicate object, and the failure lands on whichever process
// was later, which is not the one an operator is watching (2026-10-24 review, M11). The
// lock is released when the session ends even if the process is killed.
func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire a connection for migrations: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, MigrationLockID); err != nil {
		return fmt.Errorf("store: take the migration lock: %w", err)
	}
	defer func() {
		// Best effort: the session releases the lock on close anyway, and a failure here must
		// not mask the migration's own error. The connection is about to go back to the pool
		// with the lock still held if this fails, so it is unlocked on a fresh background
		// context that a cancelled request cannot abort.
		if _, uerr := conn.Exec(context.Background(),
			`SELECT pg_advisory_unlock($1)`, MigrationLockID); uerr != nil {
			_ = conn.Conn().Close(context.Background())
		}
	}()

	const tracking = `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`
	if _, err := conn.Exec(ctx, tracking); err != nil {
		return fmt.Errorf("store: create migration table: %w", err)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("store: check migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		raw, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		// A byte-order mark is valid UTF-8 and invisible in every editor, and PostgreSQL answers it
		// with `syntax error at or near "\ufeff"` — a message that names nothing a person can find.
		// Editors on Windows add one by habit, so it is stripped here rather than left as a trap that
		// fails a migration at the worst possible moment.
		raw = bytes.TrimPrefix(raw, []byte("\ufeff"))

		// One transaction per migration: a failure leaves the schema at the
		// previous version instead of half-way through.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		// The file may contain its own BEGIN/COMMIT; strip them so they do not
		// conflict with the surrounding transaction.
		body := stripTxControl(string(raw))
		if _, err := tx.Exec(ctx, body); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: commit %s: %w", name, err)
		}
	}
	return nil
}

// CheckSchema reports whether the database has every migration this binary embeds.
//
// It exists because `serve` used to apply no check at all: a binary deployed ahead of
// its schema served traffic, answered /readyz, and then failed on the first request
// that touched a new column — a 500 whose cause was three hops away from its symptom
// (2026-10-24 review, H8). Refusing to serve with a message naming the missing
// migrations turns that into a startup error an operator can act on before any user
// sees it.
//
// The direction matters. A database BEHIND the binary is fatal (the code will query
// columns that do not exist). A database AHEAD of the binary is a rollback in
// progress: it is reported so the mismatch is visible, but it is not the check's job
// to refuse — the older binary may well serve correctly, and refusing would make a
// rollback impossible without a database restore.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) (behind, ahead []string, err error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, nil, fmt.Errorf("store: read migrations: %w", err)
	}
	embedded := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			embedded = append(embedded, e.Name())
		}
	}
	sort.Strings(embedded)

	rows, qerr := pool.Query(ctx, `SELECT name FROM schema_migrations`)
	if qerr != nil {
		// The table itself may be missing, which means nothing has been applied. That
		// is the "behind" case with every migration listed, not an error to hide.
		return embedded, nil, nil
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			return nil, nil, fmt.Errorf("store: read applied migrations: %w", serr)
		}
		applied[name] = true
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, nil, fmt.Errorf("store: read applied migrations: %w", rerr)
	}

	for _, name := range embedded {
		if !applied[name] {
			behind = append(behind, name)
		}
	}
	known := map[string]bool{}
	for _, name := range embedded {
		known[name] = true
	}
	for name := range applied {
		if !known[name] {
			ahead = append(ahead, name)
		}
	}
	sort.Strings(ahead)
	return behind, ahead, nil
}

// stripTxControl removes top-level BEGIN/COMMIT statements.
//
// The migration files are written to be runnable by psql directly, which needs
// them; the Go applier supplies the transaction instead.
// stripTxControl removes the top-level BEGIN/COMMIT that psql needs.
//
// It tracks dollar-quoted blocks ($$ ... $$) because a plpgsql DO block contains
// its own BEGIN ... END, and deleting those lines corrupts the block into a syntax
// error. Line-based filtering alone is not enough, which is exactly the bug this
// function had when the migration first failed to apply.
// stripTxControl removes the wrapper BEGIN/COMMIT from a migration file, because the
// runner executes each file inside its own transaction.
//
// It must ignore anything inside a dollar-quoted body. Two forms occur: anonymous `$$` and
// a named tag such as `$fn$`. Tracking only `$$` is not enough — a `CREATE FUNCTION ...
// $fn$ ... $fn$` block then reads as ordinary SQL, and any line inside it that happens to
// be "BEGIN" (a PL/pgSQL block opener) is dropped, silently corrupting the function body.
// That failure is invisible locally and produces a confusing "is an index" error at apply
// time, so both forms are handled here.
func stripTxControl(sql string) string {
	var (
		out  []string
		open string // the currently open dollar-quote tag, or "" when outside
	)

	for _, line := range strings.Split(sql, "\n") {
		wasOpen := open != ""
		// Scan the line for dollar-quote delimiters, honouring named tags.
		open = scanDollarQuotes(line, open)

		if !wasOpen && open == "" {
			trimmed := strings.TrimSpace(strings.ToUpper(line))
			if trimmed == "BEGIN;" || trimmed == "BEGIN" ||
				trimmed == "COMMIT;" || trimmed == "COMMIT" {
				continue
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// scanDollarQuotes advances the dollar-quote state across one line.
//
// PostgreSQL dollar quoting is `$tag$ ... $tag$` where the tag is an optional identifier.
// The opening and closing delimiters must match, so the state is the tag itself rather than
// a boolean: a `$fn$` body containing `$$` (a nested anonymous quote, which is legal) would
// otherwise close early.
func scanDollarQuotes(line, open string) string {
	for i := 0; i < len(line); {
		if line[i] != '$' {
			i++
			continue
		}
		// Read the candidate tag: $ [identifier] $
		j := i + 1
		for j < len(line) && (isTagChar(line[j])) {
			j++
		}
		if j >= len(line) || line[j] != '$' {
			i++
			continue
		}
		tag := line[i : j+1] // includes both dollars

		if open == "" {
			open = tag
		} else if open == tag {
			open = ""
		}
		// A different tag inside a quoted body is text, not a delimiter.
		i = j + 1
	}
	return open
}

// isTagChar reports whether c may appear in a dollar-quote tag.
//
// PostgreSQL allows [A-Za-z_][A-Za-z0-9_]*; a leading digit is not valid, but accepting it
// here is harmless because the delimiter must still close with a matching `$`.
func isTagChar(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}
