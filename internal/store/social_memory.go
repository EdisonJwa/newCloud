package store

// Social metadata in the in-memory store (core-scope §4, §5, §6).
//
// The rules mirror the PostgreSQL statements deliberately, including their failures. The memory
// backend serialises transactions, so "count then insert" is atomic here in a way it would not be
// in PostgreSQL — which is exactly why the SQL versions are single statements and these only have
// to be CORRECT rather than clever. The shared conformance suite runs both, so a difference in what
// they refuse is caught rather than argued about.

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var _ SocialTx = (*memTx)(nil)

// memFavorite is one star.
type memFavorite struct {
	UserID    uuid.UUID
	NodeID    uuid.UUID
	CreatedAt time.Time
}

// memNodeTag is one assignment.
type memNodeTag struct {
	NodeID   uuid.UUID
	TagID    uuid.UUID
	TaggedBy uuid.UUID
	TaggedAt time.Time
}

// ---------------------------------------------------------------- favorites --

// SetFavorite stars a node. Idempotent, and a repeat does not move the star to the top: the
// listing is ordered by when the item was starred.
func (t *memTx) SetFavorite(ctx context.Context, userID, nodeID uuid.UUID) error {
	key := [2]uuid.UUID{userID, nodeID}
	if t.m.favorites == nil {
		t.m.favorites = map[[2]uuid.UUID]time.Time{}
	}
	if _, ok := t.m.favorites[key]; ok {
		return nil
	}
	t.m.favorites[key] = t.m.now()
	return nil
}

// UnsetFavorite removes a star. Idempotent.
func (t *memTx) UnsetFavorite(ctx context.Context, userID, nodeID uuid.UUID) error {
	delete(t.m.favorites, [2]uuid.UUID{userID, nodeID})
	return nil
}

// IsFavorite reports whether this user has starred this node.
func (t *memTx) IsFavorite(ctx context.Context, userID, nodeID uuid.UUID) (bool, error) {
	_, ok := t.m.favorites[[2]uuid.UUID{userID, nodeID}]
	return ok, nil
}

// FavoritesAfter pages a user's starred nodes, newest first.
func (t *memTx) FavoritesAfter(ctx context.Context, userID uuid.UUID, cursor string,
	limit int) ([]FavoriteNode, error) {

	var (
		afterAt time.Time
		afterID uuid.UUID
		have    bool
	)
	if cursor != "" {
		at, id, err := ParseSocialCursor(cursor)
		if err != nil {
			return nil, err
		}
		afterAt, afterID, have = at, id, true
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	type entry struct {
		node Node
		at   time.Time
	}
	found := make([]entry, 0, 8)
	for key, at := range t.m.favorites {
		if key[0] != userID {
			continue
		}
		rec, ok := t.m.nodes[key[1]]
		if !ok || rec.node.TrashedAt != nil {
			// A star on a trashed node is hidden, not deleted: restoring the node brings the star
			// back, which is what a user expects from "I deleted it by accident" (§4).
			continue
		}
		if have {
			if at.After(afterAt) {
				continue
			}
			if at.Equal(afterAt) && key[1].String() >= afterID.String() {
				continue
			}
		}
		found = append(found, entry{node: rec.node, at: at})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].at.Equal(found[j].at) {
			return found[i].node.ID.String() > found[j].node.ID.String()
		}
		return found[i].at.After(found[j].at)
	})
	if len(found) > limit {
		found = found[:limit]
	}

	out := make([]FavoriteNode, 0, len(found))
	for _, e := range found {
		out = append(out, FavoriteNode{
			Node: e.node, Path: t.m.pathLocked(e.node), FavoritedAt: e.at,
		})
	}
	return out, nil
}

// -------------------------------------------------------------------- tags --

// CreateTag defines a tag, enforcing both caps.
func (t *memTx) CreateTag(ctx context.Context, tag Tag) (Tag, error) {
	tag, err := ValidateTag(tag)
	if err != nil {
		return Tag{}, err
	}

	if tag.OwnerID != nil {
		var mine int
		for _, existing := range t.m.tags {
			if existing.OwnerID != nil && *existing.OwnerID == *tag.OwnerID {
				mine++
			}
		}
		if mine >= MaxTagsPerAccount {
			return Tag{}, ErrTagLimit
		}
	}

	// Case-fold uniqueness, enforced here rather than by "the index will catch it" — the two
	// backends must refuse with the same error, and PostgreSQL's index produces ErrConflict.
	for _, existing := range t.m.tags {
		if !sameTagScope(*existing, tag) {
			continue
		}
		if strings.EqualFold(existing.Name, tag.Name) {
			return Tag{}, ErrConflict
		}
	}

	out := Tag{
		ID: newID(), OwnerID: tag.OwnerID, Name: tag.Name, Color: tag.Color,
		CreatedAt: t.m.now(), CreatedBy: tag.CreatedBy,
	}
	t.m.tags = append(t.m.tags, &out)
	return out, nil
}

// sameTagScope reports whether two tags live in the same uniqueness scope: both system, or the
// same owner.
func sameTagScope(a, b Tag) bool {
	if a.OwnerID == nil || b.OwnerID == nil {
		return a.OwnerID == nil && b.OwnerID == nil
	}
	return *a.OwnerID == *b.OwnerID
}

// TagByID fetches one tag.
func (t *memTx) TagByID(ctx context.Context, id uuid.UUID) (Tag, error) {
	for _, tag := range t.m.tags {
		if tag.ID == id {
			return *tag, nil
		}
	}
	return Tag{}, ErrNotFound
}

// TagsFor lists the tags visible to one account: system tags plus that account's own.
func (t *memTx) TagsFor(ctx context.Context, ownerID uuid.UUID) ([]Tag, error) {
	out := make([]Tag, 0, len(t.m.tags))
	for _, tag := range t.m.tags {
		if tag.OwnerID == nil || *tag.OwnerID == ownerID {
			out = append(out, *tag)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// UpdateTag renames and/or recolours a tag.
func (t *memTx) UpdateTag(ctx context.Context, id uuid.UUID, name, color string) (Tag, error) {
	validated, err := ValidateTag(Tag{Name: name, Color: color})
	if err != nil {
		return Tag{}, err
	}
	for _, tag := range t.m.tags {
		if tag.ID == id {
			// Re-check the fold, excluding this tag itself: renaming a tag to its own name with
			// different capitalisation is legal, renaming it onto a sibling is not.
			for _, other := range t.m.tags {
				if other.ID == id || !sameTagScope(*other, *tag) {
					continue
				}
				if strings.EqualFold(other.Name, validated.Name) {
					return Tag{}, ErrConflict
				}
			}
			tag.Name = validated.Name
			tag.Color = validated.Color
			return *tag, nil
		}
	}
	return Tag{}, ErrNotFound
}

// DeleteTag removes a tag and its assignments.
func (t *memTx) DeleteTag(ctx context.Context, id uuid.UUID) error {
	idx := -1
	for i, tag := range t.m.tags {
		if tag.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	if t.m.tags[idx].IsSystem() {
		// Shared vocabulary: emptying it off every node in the deployment is not something a
		// single delete should be able to do, however the caller frames it.
		return ErrConflict
	}
	t.m.tags = append(t.m.tags[:idx], t.m.tags[idx+1:]...)
	for key := range t.m.nodeTags {
		if key[1] == id {
			delete(t.m.nodeTags, key)
		}
	}
	return nil
}

// AssignTag applies a tag to a node, enforcing the per-node cap.
func (t *memTx) AssignTag(ctx context.Context, nodeID, tagID uuid.UUID, by uuid.UUID) error {
	if _, err := t.TagByID(ctx, tagID); err != nil {
		return err
	}
	if t.m.nodeTags == nil {
		t.m.nodeTags = map[[2]uuid.UUID]memNodeTag{}
	}
	key := [2]uuid.UUID{nodeID, tagID}
	if _, ok := t.m.nodeTags[key]; ok {
		// Already assigned. Checked BEFORE the cap so re-assigning something the node already
		// carries is the no-op it is, rather than a "limited" refusal a client would retry forever.
		return nil
	}
	var n int
	for k := range t.m.nodeTags {
		if k[0] == nodeID {
			n++
		}
	}
	if n >= MaxTagsPerNode {
		return ErrTagLimit
	}
	t.m.nodeTags[key] = memNodeTag{NodeID: nodeID, TagID: tagID, TaggedBy: by, TaggedAt: t.m.now()}
	return nil
}

// UnassignTag removes one assignment. Idempotent.
func (t *memTx) UnassignTag(ctx context.Context, nodeID, tagID uuid.UUID) error {
	delete(t.m.nodeTags, [2]uuid.UUID{nodeID, tagID})
	return nil
}

// NodeTags lists the tags on one node that the given viewer may see.
func (t *memTx) NodeTags(ctx context.Context, viewerID, nodeID uuid.UUID) ([]Tag, error) {
	out := make([]Tag, 0, 4)
	for _, tag := range t.m.tags {
		if tag.OwnerID != nil && *tag.OwnerID != viewerID {
			continue
		}
		if _, ok := t.m.nodeTags[[2]uuid.UUID{nodeID, tag.ID}]; ok {
			out = append(out, *tag)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// ---------------------------------------------------------------- comments --

// CreateComment posts a comment, enforcing the cap and the rolling-window rate limit.
func (t *memTx) CreateComment(ctx context.Context, c Comment, limit CommentLimit) (Comment, error) {
	body, err := ValidateCommentBody(c.Body)
	if err != nil {
		return Comment{}, err
	}
	c.Body = body

	if c.ParentID != nil {
		parent, ok := t.commentLocked(*c.ParentID)
		if !ok || parent.NodeID != c.NodeID || parent.ParentID != nil || parent.Deleted() {
			// Single-level replies on the same node; a parent on another node would put a comment
			// in a thread about a file the author may not be able to see.
			return Comment{}, ErrNotFound
		}
	}

	if limit.Limit > 0 && limit.Window > 0 {
		cutoff := t.m.now().Add(-limit.Window)
		var n int64
		for _, existing := range t.m.comments {
			if existing.AuthorID == c.AuthorID && existing.CreatedAt.After(cutoff) {
				n++
			}
		}
		if n >= limit.Limit {
			return Comment{}, ErrCommentRateLimited
		}
	}

	if c.ID == uuid.Nil {
		c.ID = newID()
	}
	c.CreatedAt = t.m.now()
	// The thread order is a monotonic sequence, not the timestamp: the in-memory clock is coarse
	// enough that several posts in a row share one (measured on this platform: no two consecutive
	// time.Now() calls differ), and ordering by (created_at, random id) would then put a reply
	// above the message it answers.
	t.m.nextCommentSeq++
	c.Seq = t.m.nextCommentSeq
	stored := c
	t.m.comments = append(t.m.comments, &stored)
	return stored, nil
}

// commentLocked finds a comment by id.
func (t *memTx) commentLocked(id uuid.UUID) (Comment, bool) {
	for _, c := range t.m.comments {
		if c.ID == id {
			return *c, true
		}
	}
	return Comment{}, false
}

// CommentByID fetches one comment including its tombstone state.
func (t *memTx) CommentByID(ctx context.Context, id uuid.UUID) (Comment, error) {
	c, ok := t.commentLocked(id)
	if !ok {
		return Comment{}, ErrNotFound
	}
	return c, nil
}

// CommentsAfter pages a node's thread in posting order.
func (t *memTx) CommentsAfter(ctx context.Context, nodeID uuid.UUID, afterSeq int64,
	limit int) ([]Comment, error) {

	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	found := make([]Comment, 0, 8)
	for _, c := range t.m.comments {
		if c.NodeID != nodeID || c.Seq <= afterSeq {
			continue
		}
		found = append(found, *c)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Seq < found[j].Seq })
	if len(found) > limit {
		found = found[:limit]
	}
	return found, nil
}

// UpdateComment replaces a body and stamps edited_at.
func (t *memTx) UpdateComment(ctx context.Context, id uuid.UUID, body string,
	at time.Time) (Comment, error) {

	validated, err := ValidateCommentBody(body)
	if err != nil {
		return Comment{}, err
	}
	for _, c := range t.m.comments {
		if c.ID != id {
			continue
		}
		if c.Deleted() {
			// A tombstone cannot be rewritten: that would let the author undo a moderator's
			// decision and leave the row claiming to be both deleted and edited.
			return Comment{}, ErrNotFound
		}
		c.Body = validated
		stamp := at
		c.EditedAt = &stamp
		return *c, nil
	}
	return Comment{}, ErrNotFound
}

// DeleteComment soft-deletes a comment, idempotently.
func (t *memTx) DeleteComment(ctx context.Context, id uuid.UUID, by uuid.UUID,
	at time.Time) (Comment, error) {

	for _, c := range t.m.comments {
		if c.ID != id {
			continue
		}
		if c.DeletedAt == nil {
			stamp := at
			c.DeletedAt = &stamp
		}
		if c.DeletedBy == nil {
			who := by
			c.DeletedBy = &who
		}
		return *c, nil
	}
	return Comment{}, ErrNotFound
}

// CountRecentComments reports how many comments an author posted inside the window.
func (t *memTx) CountRecentComments(ctx context.Context, authorID uuid.UUID,
	window time.Duration) (int64, error) {

	cutoff := t.m.now().Add(-window)
	var n int64
	for _, c := range t.m.comments {
		if c.AuthorID == authorID && c.CreatedAt.After(cutoff) {
			n++
		}
	}
	return n, nil
}

// SocialFor returns per-viewer state for a batch of nodes.
//
// The checksum comes from the node's recorded content address, which both layouts set: the blob
// layout stores content under that address and the tree layout computes the same SHA-256 on write.
// That is what makes §2's "both layouts report the same value for identical bytes" true rather
// than aspiring.
func (m *Memory) SocialFor(ctx context.Context, viewerID uuid.UUID,
	nodeIDs []uuid.UUID) (map[uuid.UUID]NodeSocial, error) {

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[uuid.UUID]NodeSocial, len(nodeIDs))
	get := func(id uuid.UUID) NodeSocial {
		v := out[id]
		if v.Tags == nil {
			v.Tags = []Tag{}
		}
		return v
	}

	for _, id := range nodeIDs {
		rec, ok := m.nodes[id]
		if !ok {
			continue
		}
		v := get(id)
		if _, starred := m.favorites[[2]uuid.UUID{viewerID, id}]; starred {
			v.Starred = true
		}
		if rec.node.Kind == KindFile {
			sum := make([]byte, len(rec.blob))
			copy(sum, rec.blob[:])
			// A zero address means "no content recorded for this node", which is not a checksum:
			// reporting 0x000…0 would let a client verify against a value nobody computed.
			if rec.blob != ([32]byte{}) {
				v.SHA256 = sum
			}
		}
		for _, tag := range m.tags {
			if tag.OwnerID != nil && *tag.OwnerID != viewerID {
				continue
			}
			if _, assigned := m.nodeTags[[2]uuid.UUID{id, tag.ID}]; assigned {
				v.Tags = append(v.Tags, *tag)
			}
		}
		sort.Slice(v.Tags, func(i, j int) bool {
			li, lj := strings.ToLower(v.Tags[i].Name), strings.ToLower(v.Tags[j].Name)
			if li != lj {
				return li < lj
			}
			return v.Tags[i].ID.String() < v.Tags[j].ID.String()
		})
		out[id] = v
	}
	return out, nil
}
