package store

// Subtree enumeration.
//
// §4.2 is explicit that a recursive operation must not check only the root and ignore an
// inner node that carries a more restrictive grant: "目录递归操作：不能只检查根目录，忽略内部
// 受特殊保护的文件". Honouring that needs a way to see the whole subtree, and none existed —
// every recursive handler (trash, archive, move, copy) had to either trust the root or
// reimplement a walk.
//
// The walk is breadth-first and bounded. Bounded because a subtree can be a million nodes
// and the caller is an HTTP handler with a request timeout: an unbounded walk is a way to
// turn one request into an outage.

import (
	"context"

	"github.com/google/uuid"
)

// Descendants returns every node below rootID, excluding rootID itself.
//
// The limit applies to the whole subtree, not per level: the caller needs to know whether
// it saw everything, and a per-level cap would silently truncate a wide tree.
//
// When the subtree is larger than limit, the returned slice holds the first limit nodes and
// `complete` is false. A caller that must check permissions on everything has to treat an
// incomplete walk as a refusal rather than as approval of the part it saw — the node it did
// not see may be the protected one.
func (t *pgTx) Descendants(ctx context.Context, rootID uuid.UUID, limit int) (nodes []Node, complete bool, err error) {
	if limit <= 0 {
		limit = 10_000
	}

	// One recursive CTE rather than a query per level. `UNION` (not UNION ALL) terminates
	// even if the parent pointers were ever made cyclic by a bug: the CTE cannot revisit a
	// row it already produced.
	// A var rather than a const: the column list is built by a function so the qualified
	// and unqualified forms cannot drift apart.
	q := `
		WITH RECURSIVE sub AS (
		    SELECT id, parent_id, 1 AS depth FROM nodes WHERE parent_id = $1
		    UNION
		    SELECT n.id, n.parent_id, s.depth + 1
		    FROM nodes n JOIN sub s ON n.parent_id = s.id
		     WHERE s.depth < 64
		)
		SELECT ` + nodeColumnsQualified("n") + `
		FROM nodes n JOIN sub s ON n.id = s.id
		ORDER BY s.depth, n.name
		LIMIT $2`

	rows, err := t.tx.Query(ctx, q, rootID, limit+1)
	if err != nil {
		return nil, false, translate(err)
	}
	defer rows.Close()

	for rows.Next() {
		n, serr := scanNode(rows)
		if serr != nil {
			return nil, false, translate(serr)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, false, translate(err)
	}

	// One extra row was requested purely to learn whether the subtree is bigger than the
	// caller asked for.
	if len(nodes) > limit {
		return nodes[:limit], false, nil
	}
	return nodes, true, nil
}

// The depth bound (64) is a safety valve, not a product limit. It exists so a cycle
// introduced by a bug terminates instead of exhausting the connection, and 64 exceeds any
// plausible folder nesting.
var _ = context.Background

// AllRoots returns every user's namespace root.
//
// A restore drill has to check every account's files, not one owner's: a backup that loses
// one user's tree is exactly the failure the drill exists to catch, and it would be invisible
// to a per-owner walk.
func (t *pgTx) AllRoots(ctx context.Context) ([]Node, error) {
	// No join here, so the unqualified nodeColumns and ORDER BY are unambiguous.
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE parent_id IS NULL AND trashed_at IS NULL
		ORDER BY created_at, id`
	rows, err := t.tx.Query(ctx, q)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		n, serr := scanNode(rows)
		if serr != nil {
			return nil, translate(serr)
		}
		out = append(out, n)
	}
	return out, translate(rows.Err())
}

// AllRoots implements Store.AllRoots by running the Tx method in its own transaction.
func (p *PG) AllRoots(ctx context.Context) ([]Node, error) {
	var out []Node
	err := p.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		got, aerr := tx.AllRoots(ctx)
		out = got
		return aerr
	})
	return out, err
}
