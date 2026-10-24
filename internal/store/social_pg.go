package store

// Social metadata on PostgreSQL (core-scope §4, §5, §6).
//
// The queries here are written to be the SAME decision as the in-memory implementation, because
// the shared conformance suite runs both backends against one set of cases. Where the two could
// disagree, the SQL does the whole decision in one statement rather than reading and then writing:
//
//   - the tag caps count and insert together, so two concurrent requests cannot both observe "one
//     under the limit";
//   - the comment rate limit is the same guarded-insert shape AllowShareUpload uses;
//   - deleting a tag takes its assignments with it in a single statement, so no window exists in
//     which a filter matches a tag nobody can see.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var _ SocialTx = (*pgTx)(nil)

// ---------------------------------------------------------------- favorites --

// SetFavorite stars a node for a user.
//
// ON CONFLICT DO NOTHING makes it idempotent, which matters for the reason the spec gives: a
// client that retries after a dropped response must not have to distinguish "already starred" from
// "starred now". The created_at is deliberately NOT refreshed on a repeat: the listing is ordered
// by when the item was starred, and re-starring something already starred should not jump it to
// the top of the list.
func (t *pgTx) SetFavorite(ctx context.Context, userID, nodeID uuid.UUID) error {
	const q = `INSERT INTO favorites (user_id, node_id) VALUES ($1, $2)
		ON CONFLICT (user_id, node_id) DO NOTHING`
	if _, err := t.tx.Exec(ctx, q, userID, nodeID); err != nil {
		return translate(err)
	}
	return nil
}

// UnsetFavorite removes a star. Idempotent like SetFavorite.
func (t *pgTx) UnsetFavorite(ctx context.Context, userID, nodeID uuid.UUID) error {
	if _, err := t.tx.Exec(ctx,
		`DELETE FROM favorites WHERE user_id = $1 AND node_id = $2`, userID, nodeID); err != nil {
		return translate(err)
	}
	return nil
}

// IsFavorite reports whether this user has starred this node.
func (t *pgTx) IsFavorite(ctx context.Context, userID, nodeID uuid.UUID) (bool, error) {
	var ok bool
	if err := t.tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM favorites WHERE user_id = $1 AND node_id = $2)`,
		userID, nodeID).Scan(&ok); err != nil {
		return false, translate(err)
	}
	return ok, nil
}

// FavoritesAfter pages a user's starred nodes, newest first.
//
// The join on `trashed_at IS NULL` is the "trash interaction" half of §4: a favorite on a trashed
// node is hidden from the listing without deleting the row, so restoring the node brings the star
// back. The path is assembled with the same recursive walk the search query uses, so a favorite
// deep in the tree carries a path that actually locates it.
func (t *pgTx) FavoritesAfter(ctx context.Context, userID uuid.UUID, cursor string,
	limit int) ([]FavoriteNode, error) {

	var (
		afterAt *time.Time
		afterID *uuid.UUID
	)
	if cursor != "" {
		at, id, err := ParseSocialCursor(cursor)
		if err != nil {
			return nil, err
		}
		afterAt, afterID = &at, &id
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	// A variable, not a const: nodeColumnsQualified is a function, and the column list must be the
	// same one every other node query scans or the Scan below silently reads the wrong fields.
	q := `
		WITH hits AS (
		    SELECT f.node_id, f.created_at
		    FROM favorites f
		    JOIN nodes n ON n.id = f.node_id AND n.trashed_at IS NULL
		    WHERE f.user_id = $1
		      AND ($2::timestamptz IS NULL OR (f.created_at, f.node_id) < ($2::timestamptz, $3::uuid))
		    ORDER BY f.created_at DESC, f.node_id DESC
		    LIMIT $4
		), up(origin, id, parent_id, name, depth) AS (
		    SELECT node_id, n.id, n.parent_id, n.name, 0
		    FROM nodes n JOIN hits h ON h.node_id = n.id
		    UNION ALL
		    SELECT up.origin, p.id, p.parent_id, p.name, up.depth + 1
		    FROM nodes p JOIN up ON p.id = up.parent_id
		    WHERE up.depth < 64
		), chains AS (
		    SELECT origin,
		           string_agg(name, '/' ORDER BY depth DESC)
		             FILTER (WHERE depth > 0 AND name <> '') AS dirs
		    FROM up GROUP BY origin
		)
		SELECT ` + nodeColumnsQualified("n") + `,
		       COALESCE(chains.dirs || '/', '') || n.name AS path,
		       h.created_at
		FROM nodes n
		JOIN hits h ON h.node_id = n.id
		LEFT JOIN chains ON chains.origin = n.id
		ORDER BY h.created_at DESC, h.node_id DESC`

	rows, err := t.tx.Query(ctx, q, userID, afterAt, afterID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make([]FavoriteNode, 0, limit)
	for rows.Next() {
		var n Node
		var kind, path string
		var favoritedAt time.Time
		if err := rows.Scan(&n.ID, &n.ParentID, &n.OwnerID, &n.OwnerGroupID, &n.Name, &kind,
			&n.Size, &n.Mime, &n.ETag, &n.BlobID, &n.CreatedAt, &n.ModifiedAt, &n.TrashedAt,
			&n.RetainUntil, &path, &favoritedAt); err != nil {
			return nil, translate(err)
		}
		n.Kind = Kind(kind)
		out = append(out, FavoriteNode{Node: n, Path: path, FavoritedAt: favoritedAt})
	}
	return out, translate(rows.Err())
}

// -------------------------------------------------------------------- tags --

// CreateTag defines a tag, enforcing both caps.
//
// The per-account cap is checked inside the INSERT, so the count and the write are one decision.
// The system-tag cap does not exist: an administrator manages a handful of labels, and a limit
// there would be a limit on nothing.
func (t *pgTx) CreateTag(ctx context.Context, tag Tag) (Tag, error) {
	tag, err := ValidateTag(tag)
	if err != nil {
		return Tag{}, err
	}
	if tag.OwnerID != nil {
		// Serialise this account's tag creation. Without it two concurrent creates both read a
		// count one below the cap and both insert, which is the failure mode a limit exists to
		// prevent. The lock is per account, so it never serialises across tenants.
		if _, err := t.tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext($1::text))`,
			"tags:"+tag.OwnerID.String()); err != nil {
			return Tag{}, translate(err)
		}
	}

	const q = `INSERT INTO tags (owner_id, name, color, created_by)
		SELECT $1, $2, $3, $4
		WHERE $1::uuid IS NULL
		   OR (SELECT count(*) FROM tags WHERE owner_id = $1) < $5
		RETURNING id, owner_id, name, color, created_at, created_by`
	var out Tag
	if err := t.tx.QueryRow(ctx, q, tag.OwnerID, tag.Name, tag.Color, tag.CreatedBy,
		MaxTagsPerAccount).Scan(&out.ID, &out.OwnerID, &out.Name, &out.Color, &out.CreatedAt,
		&out.CreatedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The WHERE clause rejected every candidate row: the account is at its cap.
			return Tag{}, ErrTagLimit
		}
		return Tag{}, translate(err)
	}
	return out, nil
}

// TagByID fetches one tag.
func (t *pgTx) TagByID(ctx context.Context, id uuid.UUID) (Tag, error) {
	const q = `SELECT id, owner_id, name, color, created_at, created_by FROM tags WHERE id = $1`
	var out Tag
	if err := t.tx.QueryRow(ctx, q, id).Scan(&out.ID, &out.OwnerID, &out.Name, &out.Color,
		&out.CreatedAt, &out.CreatedBy); err != nil {
		return Tag{}, translate(err)
	}
	return out, nil
}

// TagsFor lists the tags visible to one account: the system tags plus that account's own.
func (t *pgTx) TagsFor(ctx context.Context, ownerID uuid.UUID) ([]Tag, error) {
	const q = `SELECT id, owner_id, name, color, created_at, created_by
		FROM tags
		WHERE owner_id IS NULL OR owner_id = $1
		ORDER BY lower(name), id`
	rows, err := t.tx.Query(ctx, q, ownerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	return scanTags(rows)
}

// UpdateTag renames and/or recolours a tag.
//
// The uniqueness index is what actually refuses a case-fold collision; translate() turns that
// into ErrConflict, so both backends answer a duplicate name the same way.
func (t *pgTx) UpdateTag(ctx context.Context, id uuid.UUID, name, color string) (Tag, error) {
	validated, err := ValidateTag(Tag{Name: name, Color: color})
	if err != nil {
		return Tag{}, err
	}
	const q = `UPDATE tags SET name = $2, color = $3 WHERE id = $1
		RETURNING id, owner_id, name, color, created_at, created_by`
	var out Tag
	if err := t.tx.QueryRow(ctx, q, id, validated.Name, validated.Color).Scan(&out.ID,
		&out.OwnerID, &out.Name, &out.Color, &out.CreatedAt, &out.CreatedBy); err != nil {
		return Tag{}, translate(err)
	}
	return out, nil
}

// DeleteTag removes a tag and its assignments in one transaction.
//
// Two statements rather than one recursive delete, because the assignments have to go BEFORE the
// tag row: the foreign key cascades, but relying on the cascade would leave the ordering implicit
// in the schema rather than in the code that answers "did this remove the assignments".
func (t *pgTx) DeleteTag(ctx context.Context, id uuid.UUID) error {
	tag, err := t.TagByID(ctx, id)
	if err != nil {
		return err
	}
	if tag.IsSystem() {
		// A system tag is shared vocabulary; deleting it silently would empty the label off every
		// node in the deployment. The handler refuses this for non-admins, and the store refuses it
		// outright for the system set so no caller can do it by accident.
		return ErrConflict
	}
	if _, err := t.tx.Exec(ctx, `DELETE FROM node_tags WHERE tag_id = $1`, id); err != nil {
		return translate(err)
	}
	tag_rows, err := t.tx.Exec(ctx, `DELETE FROM tags WHERE id = $1`, id)
	if err != nil {
		return translate(err)
	}
	if tag_rows.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AssignTag applies a tag to a node, enforcing the per-node cap.
func (t *pgTx) AssignTag(ctx context.Context, nodeID, tagID uuid.UUID, by uuid.UUID) error {
	// Serialise assignments on this node so the cap cannot be raced past.
	if _, err := t.tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1::text))`, "nodetags:"+nodeID.String()); err != nil {
		return translate(err)
	}
	const q = `INSERT INTO node_tags (node_id, tag_id, tagged_by)
		SELECT $1, $2, $3
		WHERE (SELECT count(*) FROM node_tags WHERE node_id = $1) < $4
		ON CONFLICT (node_id, tag_id) DO NOTHING`
	tag, err := t.tx.Exec(ctx, q, nodeID, tagID, by, MaxTagsPerNode)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Zero rows means either "already assigned" or "at the cap". Distinguishing them is what makes
	// the error honest: a client told "limited" for something it already has will retry forever.
	var exists bool
	if err := t.tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM node_tags WHERE node_id = $1 AND tag_id = $2)`,
		nodeID, tagID).Scan(&exists); err != nil {
		return translate(err)
	}
	if exists {
		return nil
	}
	return ErrTagLimit
}

// UnassignTag removes one assignment. Idempotent.
func (t *pgTx) UnassignTag(ctx context.Context, nodeID, tagID uuid.UUID) error {
	if _, err := t.tx.Exec(ctx,
		`DELETE FROM node_tags WHERE node_id = $1 AND tag_id = $2`, nodeID, tagID); err != nil {
		return translate(err)
	}
	return nil
}

// NodeTags lists the tags on one node that the given viewer may see.
func (t *pgTx) NodeTags(ctx context.Context, viewerID, nodeID uuid.UUID) ([]Tag, error) {
	const q = `SELECT t.id, t.owner_id, t.name, t.color, t.created_at, t.created_by
		FROM node_tags nt
		JOIN tags t ON t.id = nt.tag_id
		WHERE nt.node_id = $1 AND (t.owner_id IS NULL OR t.owner_id = $2)
		ORDER BY lower(t.name), t.id`
	rows, err := t.tx.Query(ctx, q, nodeID, viewerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	return scanTags(rows)
}

// scanTags reads a tag result set.
func scanTags(rows pgx.Rows) ([]Tag, error) {
	out := make([]Tag, 0, 8)
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.ID, &tag.OwnerID, &tag.Name, &tag.Color, &tag.CreatedAt,
			&tag.CreatedBy); err != nil {
			return nil, translate(err)
		}
		out = append(out, tag)
	}
	return out, translate(rows.Err())
}

// ---------------------------------------------------------------- comments --

// CreateComment posts a comment, enforcing the cap and the rolling-window rate limit.
//
// The rate limit is a guarded INSERT: the SELECT produces a row only when the author's count
// inside the window is below the limit, so "count then insert" cannot interleave with a concurrent
// post. The advisory lock serialises just the decision, per author, so a busy thread does not make
// every other author's post wait.
//
// A deleted comment still counts, because deleting your own posts must not reset the budget.
func (t *pgTx) CreateComment(ctx context.Context, c Comment, limit CommentLimit) (Comment, error) {
	body, err := ValidateCommentBody(c.Body)
	if err != nil {
		return Comment{}, err
	}
	c.Body = body

	if limit.Limit > 0 && limit.Window > 0 {
		if _, err := t.tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext($1::text))`,
			"comments:"+c.AuthorID.String()); err != nil {
			return Comment{}, translate(err)
		}
	}

	// One level of replies (§6): the parent must be a top-level comment on the SAME node. A parent
	// on another node would let a comment appear in a thread about a file the author cannot see.
	const q = `INSERT INTO comments (id, node_id, author_id, parent_id, body, created_at)
		SELECT $1, $2, $3, $4, $5, now()
		WHERE ($4::uuid IS NULL OR EXISTS (
		          SELECT 1 FROM comments p
		          WHERE p.id = $4 AND p.parent_id IS NULL AND p.node_id = $2))
		  AND ($6::bigint <= 0 OR
		       (SELECT count(*) FROM comments
		          WHERE author_id = $3 AND created_at > now() - $7::interval) < $6)
		RETURNING seq, id, node_id, author_id, parent_id, body, created_at, edited_at, deleted_at,
		          deleted_by`

	var out Comment
	row := t.tx.QueryRow(ctx, q, c.ID, c.NodeID, c.AuthorID, c.ParentID, c.Body, limit.Limit,
		intervalOf(limit.Window))
	if err := row.Scan(&out.Seq, &out.ID, &out.NodeID, &out.AuthorID, &out.ParentID, &out.Body,
		&out.CreatedAt, &out.EditedAt, &out.DeletedAt, &out.DeletedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The guard refused every candidate row. Two causes, and the caller needs them apart:
			// a bad parent is a 4xx about the request, a rate limit is a 429 about the caller.
			return Comment{}, t.commentInsertRefusal(ctx, c, limit)
		}
		return Comment{}, translate(err)
	}
	return out, nil
}

// commentInsertRefusal says why nobody inserted; only called when the guarded INSERT produced no row.
func (t *pgTx) commentInsertRefusal(ctx context.Context, c Comment, limit CommentLimit) error {
	if c.ParentID != nil {
		var ok bool
		if err := t.tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM comments
			   WHERE id = $1 AND parent_id IS NULL AND node_id = $2 AND deleted_at IS NULL)`,
			*c.ParentID, c.NodeID).Scan(&ok); err == nil && !ok {
			return ErrNotFound
		}
	}
	if limit.Limit > 0 && limit.Window > 0 {
		var n int64
		if err := t.tx.QueryRow(ctx,
			`SELECT count(*) FROM comments
			   WHERE author_id = $1 AND created_at > now() - $2::interval`,
			c.AuthorID, intervalOf(limit.Window)).Scan(&n); err == nil && n >= limit.Limit {
			return ErrCommentRateLimited
		}
	}
	// Neither guard explains it: report the refusal as a conflict rather than inventing a cause.
	return ErrConflict
}

// CommentByID fetches one comment including its tombstone state.
func (t *pgTx) CommentByID(ctx context.Context, id uuid.UUID) (Comment, error) {
	const q = `SELECT seq, id, node_id, author_id, parent_id, body, created_at, edited_at,
		          deleted_at, deleted_by
		FROM comments WHERE id = $1`
	var out Comment
	if err := t.tx.QueryRow(ctx, q, id).Scan(&out.Seq, &out.ID, &out.NodeID, &out.AuthorID,
		&out.ParentID, &out.Body, &out.CreatedAt, &out.EditedAt, &out.DeletedAt,
		&out.DeletedBy); err != nil {
		return Comment{}, translate(err)
	}
	return out, nil
}

// CommentsAfter pages a node's thread in posting order.
func (t *pgTx) CommentsAfter(ctx context.Context, nodeID uuid.UUID, afterSeq int64,
	limit int) ([]Comment, error) {

	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	// A plain `seq > $2` on an indexed (node_id, seq) is the whole pagination contract: the
	// sequence is monotonic and unique, so a row inserted between pages sorts on one side of the
	// cursor and cannot be returned twice or skipped.
	const q = `SELECT seq, id, node_id, author_id, parent_id, body, created_at, edited_at,
		          deleted_at, deleted_by
		FROM comments
		WHERE node_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`
	rows, err := t.tx.Query(ctx, q, nodeID, afterSeq, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make([]Comment, 0, limit)
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.Seq, &c.ID, &c.NodeID, &c.AuthorID, &c.ParentID, &c.Body,
			&c.CreatedAt, &c.EditedAt, &c.DeletedAt, &c.DeletedBy); err != nil {
			return nil, translate(err)
		}
		out = append(out, c)
	}
	return out, translate(rows.Err())
}

// UpdateComment replaces a body and stamps edited_at.
//
// A deleted comment cannot be edited: rewriting a tombstone would let a moderator's decision be
// undone by the author, and the row would then claim to be both deleted and edited.
func (t *pgTx) UpdateComment(ctx context.Context, id uuid.UUID, body string,
	at time.Time) (Comment, error) {

	validated, err := ValidateCommentBody(body)
	if err != nil {
		return Comment{}, err
	}
	const q = `UPDATE comments SET body = $2, edited_at = $3
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING seq, id, node_id, author_id, parent_id, body, created_at, edited_at, deleted_at,
		          deleted_by`
	var out Comment
	if err := t.tx.QueryRow(ctx, q, id, validated, at).Scan(&out.Seq, &out.ID, &out.NodeID,
		&out.AuthorID, &out.ParentID, &out.Body, &out.CreatedAt, &out.EditedAt, &out.DeletedAt,
		&out.DeletedBy); err != nil {
		return Comment{}, translate(err)
	}
	return out, nil
}

// DeleteComment soft-deletes a comment.
//
// Idempotent: deleting an already-deleted comment returns the existing tombstone rather than an
// error, because two moderators pressing the button at once is a normal race, not a conflict.
func (t *pgTx) DeleteComment(ctx context.Context, id uuid.UUID, by uuid.UUID,
	at time.Time) (Comment, error) {

	const q = `UPDATE comments SET deleted_at = COALESCE(deleted_at, $2),
		          deleted_by = COALESCE(deleted_by, $3)
		WHERE id = $1
		RETURNING seq, id, node_id, author_id, parent_id, body, created_at, edited_at, deleted_at,
		          deleted_by`
	var out Comment
	if err := t.tx.QueryRow(ctx, q, id, at, by).Scan(&out.Seq, &out.ID, &out.NodeID,
		&out.AuthorID, &out.ParentID, &out.Body, &out.CreatedAt, &out.EditedAt, &out.DeletedAt,
		&out.DeletedBy); err != nil {
		return Comment{}, translate(err)
	}
	return out, nil
}

// CountRecentComments reports how many comments an author posted inside the window.
func (t *pgTx) CountRecentComments(ctx context.Context, authorID uuid.UUID,
	window time.Duration) (int64, error) {

	var n int64
	if err := t.tx.QueryRow(ctx,
		`SELECT count(*) FROM comments
		   WHERE author_id = $1 AND created_at > now() - $2::interval`,
		authorID, intervalOf(window)).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// SocialFor returns per-viewer state for a batch of nodes (core-scope §2, §4, §5).
//
// Three queries for the whole page, never one per node: the same reasoning as MarkersFor. The
// checksum comes from blobs through nodes.blob_id, which is the content address — the blob layout
// stores content THERE, and the tree layout computes the same hash on write, so both layouts report
// the same value for identical bytes (the acceptance criterion in §2).
func (s *PG) SocialFor(ctx context.Context, viewerID uuid.UUID,
	nodeIDs []uuid.UUID) (map[uuid.UUID]NodeSocial, error) {

	out := make(map[uuid.UUID]NodeSocial, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	get := func(id uuid.UUID) NodeSocial {
		v := out[id]
		if v.Tags == nil {
			v.Tags = []Tag{}
		}
		return v
	}
	put := func(id uuid.UUID, v NodeSocial) {
		out[id] = v
	}

	// 1. Stars, per viewer.
	starRows, err := s.pool.Query(ctx,
		`SELECT node_id FROM favorites WHERE user_id = $1 AND node_id = ANY($2)`, viewerID, nodeIDs)
	if err != nil {
		return nil, translate(err)
	}
	for starRows.Next() {
		var id uuid.UUID
		if err := starRows.Scan(&id); err != nil {
			starRows.Close()
			return nil, translate(err)
		}
		v := get(id)
		v.Starred = true
		put(id, v)
	}
	starRows.Close()
	if err := starRows.Err(); err != nil {
		return nil, translate(err)
	}

	// 2. Tags visible to this viewer: system tags plus their own.
	tagRows, err := s.pool.Query(ctx,
		`SELECT nt.node_id, t.id, t.owner_id, t.name, t.color, t.created_at, t.created_by
		 FROM node_tags nt
		 JOIN tags t ON t.id = nt.tag_id
		 WHERE nt.node_id = ANY($1) AND (t.owner_id IS NULL OR t.owner_id = $2)
		 ORDER BY lower(t.name), t.id`, nodeIDs, viewerID)
	if err != nil {
		return nil, translate(err)
	}
	for tagRows.Next() {
		var nodeID uuid.UUID
		var tag Tag
		if err := tagRows.Scan(&nodeID, &tag.ID, &tag.OwnerID, &tag.Name, &tag.Color,
			&tag.CreatedAt, &tag.CreatedBy); err != nil {
			tagRows.Close()
			return nil, translate(err)
		}
		v := get(nodeID)
		v.Tags = append(v.Tags, tag)
		put(nodeID, v)
	}
	tagRows.Close()
	if err := tagRows.Err(); err != nil {
		return nil, translate(err)
	}

	// 3. Content addresses.
	hashRows, err := s.pool.Query(ctx,
		`SELECT n.id, b.sha256
		 FROM nodes n JOIN blobs b ON b.id = n.blob_id
		 WHERE n.id = ANY($1)`, nodeIDs)
	if err != nil {
		return nil, translate(err)
	}
	for hashRows.Next() {
		var id uuid.UUID
		var sum []byte
		if err := hashRows.Scan(&id, &sum); err != nil {
			hashRows.Close()
			return nil, translate(err)
		}
		v := get(id)
		v.SHA256 = sum
		put(id, v)
	}
	hashRows.Close()
	return out, translate(hashRows.Err())
}

// TagUsageCounts counts, per tag, how many nodes THIS VIEWER can see carry it.
//
// "This viewer can see" is the security-relevant half: a count over every node would let a caller
// infer the existence of files they have no access to — a tag showing "3 uses" while the caller can
// only find one of them tells them two files exist that they must not know about. So the query
// joins the ACL walk: a node counts when the caller owns it, or when a live grant reaches it.
//
// The join is written against acl_entries directly rather than through EffectivePermsFor, because
// that method is per-node and this is one query for the whole tag list. The rules it reproduces are
// the authorizer's: the owner always sees their own tree, a grant inherits downward, and the walk
// stops at the owner's namespace root.
func (s *PG) TagUsageCounts(ctx context.Context, viewerID uuid.UUID,
	tagIDs []uuid.UUID) (map[uuid.UUID]int, error) {

	out := make(map[uuid.UUID]int, len(tagIDs))
	if len(tagIDs) == 0 {
		return out, nil
	}

	const q = `
		WITH RECURSIVE visible AS (
		    -- The caller's own live nodes.
		    SELECT n.id FROM nodes n
		    WHERE n.owner_id = $1 AND n.trashed_at IS NULL
		    UNION
		    -- Nodes a live grant reaches, walking downward from every granted node.
		    SELECT child.id
		    FROM nodes child
		    JOIN visible parent ON child.parent_id = parent.id
		    WHERE child.trashed_at IS NULL
		), granted AS (
		    -- A grant on a node covers that node and, when it inherits, its subtree.
		    SELECT n.id FROM nodes n
		    WHERE n.trashed_at IS NULL
		      AND EXISTS (
		        SELECT 1 FROM acl_entries e
		        WHERE e.node_id = n.id
		          AND (e.user_id = $1 OR (e.group_id IS NOT NULL AND EXISTS (
		                SELECT 1 FROM group_members m
		                WHERE m.group_id = e.group_id AND m.user_id = $1))))
		    UNION
		    SELECT child.id
		    FROM nodes child
		    JOIN granted parent ON child.parent_id = parent.id
		    WHERE child.trashed_at IS NULL
		      AND EXISTS (
		        SELECT 1 FROM acl_entries e
		        WHERE e.node_id = parent.id AND e.inherit
		          AND (e.user_id = $1 OR (e.group_id IS NOT NULL AND EXISTS (
		                SELECT 1 FROM group_members m
		                WHERE m.group_id = e.group_id AND m.user_id = $1))))
		), reachable AS (
		    SELECT id FROM visible UNION SELECT id FROM granted
		)
		SELECT nt.tag_id, count(*)
		FROM node_tags nt
		JOIN nodes n ON n.id = nt.node_id AND n.trashed_at IS NULL
		JOIN reachable r ON r.id = nt.node_id
		JOIN tags t ON t.id = nt.tag_id AND (t.owner_id IS NULL OR t.owner_id = $1)
		WHERE nt.tag_id = ANY($2)
		GROUP BY nt.tag_id`

	rows, err := s.pool.Query(ctx, q, viewerID, tagIDs)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, translate(err)
		}
		out[id] = n
	}
	return out, translate(rows.Err())
}

// UsernamesFor resolves display names for a batch of accounts.
func (s *PG) UsernamesFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, username, display_name FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var username, display string
		if err := rows.Scan(&id, &username, &display); err != nil {
			return nil, translate(err)
		}
		// The display name when set, the username otherwise. Never the email: a comment author
		// column is visible to everybody who can read the node, and an address is not something
		// every reader of a shared folder should be handed.
		if display == "" {
			display = username
		}
		out[id] = display
	}
	return out, translate(rows.Err())
}

// FavoriteCount reports how many accounts have starred a node.
//
// The count of OTHER people's stars, which is what the index's facet field is for. It is not
// filtered by viewer: "how popular is this document" is a property of the document, and the query
// path decides who may see the document at all.
func (s *PG) FavoriteCount(ctx context.Context, nodeID uuid.UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM favorites WHERE node_id = $1`, nodeID).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// NodesAfter pages every node in id order, for the search backfill.
func (t *pgTx) NodesAfter(ctx context.Context, after uuid.UUID, limit int) ([]Node, error) {
	if limit <= 0 {
		limit = 200
	}
	// A zero id is "from the beginning": no node has an all-zero primary key, so a plain `>` with
	// the zero value is already correct and needs no branch.
	const q = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE id > $1
		ORDER BY id
		LIMIT $2`
	rows, err := t.tx.Query(ctx, q, after, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make([]Node, 0, limit)
	for rows.Next() {
		n, serr := scanNode(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, n)
	}
	return out, translate(rows.Err())
}

// OldestPendingJob reports when the oldest still-pending job of this kind was due.
func (t *pgTx) OldestPendingJob(ctx context.Context, kind string) (time.Time, bool, error) {
	var due *time.Time
	err := t.tx.QueryRow(ctx,
		`SELECT min(run_after) FROM jobs WHERE kind = $1 AND completed_at IS NULL`,
		kind).Scan(&due)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, translate(err)
	}
	if due == nil {
		// min() over an empty set is NULL, which is the caught-up answer.
		return time.Time{}, false, nil
	}
	return due.UTC(), true, nil
}

var (
	_ SocialReader    = (*PG)(nil)
	_ TagCounter      = (*PG)(nil)
	_ UserLookup      = (*PG)(nil)
	_ FavoriteCounter = (*PG)(nil)
)
