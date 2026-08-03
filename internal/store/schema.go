package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/migrations"
)

// ApplySchema runs the embedded migrations in order.
//
// They are idempotent at the file level only (each migration is written to be
// re-runnable where practical), so the applied set is tracked in a table rather
// than relying on that: a half-applied migration that "succeeds again" silently is
// worse than a loud failure.
func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	const tracking = `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`
	if _, err := pool.Exec(ctx, tracking); err != nil {
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
		err := pool.QueryRow(ctx,
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

		// One transaction per migration: a failure leaves the schema at the
		// previous version instead of half-way through.
		tx, err := pool.Begin(ctx)
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
