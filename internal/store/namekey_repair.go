package store

// RepairNameKeys reconciles stored name_key values with the current key function (T-12).
//
// Migration 0007 backfilled name_key with SQL `lower(name)`, because the real key is a Go
// function (Unicode NFC + full case folding + stripping format characters) that SQL cannot
// express. For ASCII names the two agree, which is why this is easy to overlook: the drift only
// appears on decomposed accents (macOS hands you NFD), on ß/ss, on ligatures, or on names
// carrying invisible formatting characters.
//
// A stale key is not cosmetic. Lookup filters on name_key, so a node whose key disagrees with
// the function cannot be found under names the function considers equal — the file is listed but
// unreachable by the spelling the user types. It also means the uniqueness rule can miss a
// collision that will bite on the next write.
//
// This runs on `newcloud migrate` rather than as a bare migration step so the repair is explicit
// operator action with a report, and so it re-runs harmlessly after future key-function changes.
// It converges: applying it twice reports zero the second time.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NameKeyRepair reports what a reconciliation pass changed.
type NameKeyRepair struct {
	// Scanned is how many named nodes were examined.
	Scanned int
	// Corrected is how many had a stale key.
	Corrected int
	// Skipped is how many could not be corrected because the corrected key would collide with a
	// sibling. Those are left alone and reported: two nodes folding to one key is a real
	// ambiguity, and silently renaming one is a decision for an operator, not a repair loop.
	Skipped []string
}

// nameRepairBatch bounds how many rows are read per round trip.
//
// The first implementation read the whole table into Go and issued two statements per stale row.
// That is fine against a test database and unusable on a real one: a million nodes meant a
// million rows resident plus two million round trips. Keyset pagination bounds memory and
// collapses the round trips.
const nameRepairBatch = 1000

// nameNode is one row as the repair sees it.
type nameNode struct {
	id       string
	parentID *string
	name     string
	key      string
}

// RepairNameKeys rewrites stale name_key values.
//
// It walks the table by keyset pagination on id. Keyset rather than OFFSET because a concurrent
// insert would shift OFFSET pages and make the walk skip or revisit rows; `id > cursor` is stable
// against that.
func RepairNameKeys(ctx context.Context, pool *pgxpool.Pool) (NameKeyRepair, error) {
	var rep NameKeyRepair

	var (
		cursor   *string
		lastPage int
	)

	for {
		rows, err := pool.Query(ctx, `
			SELECT id::text, parent_id::text, name, name_key
			FROM nodes
			WHERE name <> '' AND ($1::text IS NULL OR id > $1::uuid)
			ORDER BY id
			LIMIT $2`, cursor, nameRepairBatch)
		if err != nil {
			return rep, fmt.Errorf("store: read nodes for key repair: %w", err)
		}

		var stale []nameNode
		lastPage = 0
		for rows.Next() {
			var n nameNode
			if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.key); err != nil {
				rows.Close()
				return rep, fmt.Errorf("store: scan node for key repair: %w", err)
			}
			rep.Scanned++
			lastPage++
			if want := nameKey(n.name); want != n.key {
				stale = append(stale, n)
			}
			// Advance the cursor on every row, not only stale ones: a page of already-correct
			// rows must still move forward or the loop never terminates.
			id := n.id
			cursor = &id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}

		if len(stale) > 0 {
			if err := repairNameBatch(ctx, pool, stale, &rep); err != nil {
				return rep, err
			}
		}

		// A short page is the end of the table. Checking page size rather than a total avoids a
		// second COUNT over every row.
		if lastPage < nameRepairBatch {
			return rep, nil
		}
	}
}

// repairNameBatch applies one page of corrections inside a single transaction.
//
// One transaction per batch rather than one per row: a thousand round trips become one, and the
// batch either lands or does not.
func repairNameBatch(ctx context.Context, pool *pgxpool.Pool, batch []nameNode, rep *NameKeyRepair) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin key repair: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	for _, n := range batch {
		want := nameKey(n.name)

		// Guard the unique index. Two live siblings folding to one key is exactly what the index
		// prevents, so the update would fail and abort the batch. Skipping keeps the rest of the
		// batch progressing, and the row is reported because resolving it is a human decision.
		var clash bool
		if n.parentID != nil {
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM nodes
					WHERE parent_id = $1::uuid AND name_key = $2 AND id <> $3::uuid
					  AND trashed_at IS NULL
				)`, *n.parentID, want, n.id).Scan(&clash); err != nil {
				return fmt.Errorf("store: check key collision: %w", err)
			}
		}
		if clash {
			rep.Skipped = append(rep.Skipped,
				fmt.Sprintf("%s (%q): corrected key already held by a sibling", n.id, n.name))
			continue
		}

		if _, err := tx.Exec(ctx,
			`UPDATE nodes SET name_key = $2 WHERE id = $1::uuid`, n.id, want); err != nil {
			return fmt.Errorf("store: repair name_key for %s: %w", n.id, err)
		}
		rep.Corrected++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit key repair: %w", err)
	}
	return nil
}
