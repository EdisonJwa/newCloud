package server

// Favorites, tags and comments over the JSON API (core-scope §4, §5, §6).
//
// Three surfaces in one file because they answer the same question — "what does this account, or
// this deployment, say ABOUT a file" — and because the authorisation rule is identical for all
// three: an annotation is only ever reachable through a node the caller may already read, checked
// with the same Authorize call every other node route uses.
//
// The rules these handlers implement, and the reason each is where it is:
//
//   - Node-addressed, never path-addressed (for the by-id routes). A path is a rendering of where a
//     file sits; starring or discussing it must survive a move. The path-addressed forms that do
//     exist resolve to an id FIRST and then go through the same code, so the two cannot drift.
//   - "Not seen" and "not allowed" are the same 404. An id is guessable; whether it exists is not
//     the caller's business (the project's standing rule, and the IDOR table covers these routes).
//   - Auditing follows core-scope §8: favorites and tag assignment are NOT hash-chained; moderating
//     somebody else's comment and admin changes to the system-tag vocabulary ARE.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// tagJSON is the wire shape for a label.
type tagJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	// System marks the deployment's shared vocabulary, which only an administrator may edit.
	System bool `json:"system,omitempty"`
	// Count is how many nodes carry it, filled only by the tag listing — a filter that shows a tag
	// nobody used is a filter with a dead entry.
	Count int `json:"count,omitempty"`
}

// toTagJSON renders one tag.
func toTagJSON(t store.Tag) tagJSON {
	return tagJSON{
		ID: t.ID.String(), Name: t.Name, Color: t.Color, System: t.IsSystem(),
	}
}

// toTagJSONs renders a tag list, returning nil for an empty list so `tags` is omitted.
func toTagJSONs(tags []store.Tag) []tagJSON {
	if len(tags) == 0 {
		return nil
	}
	out := make([]tagJSON, 0, len(tags))
	for _, t := range tags {
		out = append(out, toTagJSON(t))
	}
	return out
}

// commentJSON is the wire shape for one message in a discussion.
type commentJSON struct {
	ID        string `json:"id"`
	NodeID    string `json:"node_id"`
	Author    string `json:"author"`
	AuthorID  string `json:"author_id"`
	ParentID  string `json:"parent_id,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	EditedAt  string `json:"edited_at,omitempty"`
	// Deleted is the tombstone marker: the thread keeps the row so the replies keep their place,
	// and the client renders this one as "deleted" rather than as its body (§6).
	Deleted bool `json:"deleted,omitempty"`
}

// toCommentJSON renders one comment for a viewer.
//
// A deleted comment's BODY IS NOT SENT. The row keeps it because an operator investigating abuse
// needs it (§8 keeps it out of the audit chain precisely so it stays in one place), but a client
// rendering a tombstone must not receive the text it is supposed to be hiding — otherwise the
// "deleted" flag is decoration on a body the browser already holds.
func toCommentJSON(c store.Comment, authors map[uuid.UUID]string) commentJSON {
	out := commentJSON{
		ID: c.ID.String(), NodeID: c.NodeID.String(), AuthorID: c.AuthorID.String(),
		Body: c.Body, CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
	}
	if c.ParentID != nil {
		out.ParentID = c.ParentID.String()
	}
	if c.EditedAt != nil {
		out.EditedAt = c.EditedAt.UTC().Format(time.RFC3339)
	}
	if c.Deleted() {
		out.Deleted = true
		out.Body = ""
	}
	if name, ok := authors[c.AuthorID]; ok {
		out.Author = name
	}
	return out
}

// ------------------------------------------------------------------ favorites --

// handleFavoriteStar stars a node, or un-stars it on DELETE.
//
// Both verbs are idempotent, which is the whole reason this endpoint is a PUT/DELETE pair with no
// body rather than a POST to a "favorites" collection: a client that retries after a dropped
// response must not have to distinguish "already starred" from "starred now", and a UI toggle
// pressed twice must not create two rows.
func (s *Server) handleFavoriteStar(w http.ResponseWriter, r *http.Request) {
	s.handleFavorite(w, r, true)
}

// handleFavoriteUnstar clears a node's star.
func (s *Server) handleFavoriteUnstar(w http.ResponseWriter, r *http.Request) {
	s.handleFavorite(w, r, false)
}

func (s *Server) handleFavorite(w http.ResponseWriter, r *http.Request, star bool) {
	actor, _ := actorFrom(r.Context())

	node, err := s.nodeForAnnotation(r, actor)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Starring requires READ on the node (§4): it is a statement about a file you can see. Write is
	// deliberately not required — a grantee with read-only access should be able to keep their own
	// bookmarks, because the row belongs to them, not to the file.
	cap, cerr := s.deps.Authz.Authorize(actor, node.ID)
	if cerr != nil || !cap.Can(authz.PermRead) {
		// Same answer for forbidden and absent, as everywhere else.
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		if star {
			return tx.SetFavorite(ctx, actor.ID(), node.ID)
		}
		return tx.UnsetFavorite(ctx, actor.ID(), node.ID)
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Not audited (core-scope §8): a favorite is not a security-relevant action, and putting it in
	// the hash chain would dilute the entries that decide accountability.
	writeJSON(w, http.StatusOK, map[string]any{
		"id": node.ID.String(), "starred": star,
	})
}

// handleFavorites lists the caller's starred nodes, newest first.
//
// Each row's grant is re-checked as the authorizer resolves it: "the favorites listing re-checks
// each node's grant (revocation removes it from the list, never leaks)" (§4). The store already
// excludes trashed nodes and other owners' rows; what is NOT delegated is the ACL decision, which
// is made here per node rather than trusted from the row.
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	limit := parseLimit(r.URL.Query().Get("limit"), searchMax)
	cursor := r.URL.Query().Get("cursor")

	// One extra row answers "is there more" without a count.
	var items []store.FavoriteNode
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.FavoritesAfter(ctx, actor.ID(), cursor, limit+1)
		items = got
		return lerr
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid_cursor",
				"the cursor is not one this server issued; drop it to start from the newest entry")
			return
		}
		s.writeStoreError(w, err)
		return
	}

	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = store.FormatSocialCursor(last.FavoritedAt, last.Node.ID)
	}

	rows := make([]fileJSON, 0, len(items))
	nodes := make([]store.Node, 0, len(items))
	for _, item := range items {
		// A revocation between the star and this request removes the row from the listing. Answering
		// with it would leak that a node the caller can no longer read still exists under the old
		// grant.
		if _, cerr := s.deps.Authz.Authorize(actor, item.Node.ID); cerr != nil {
			continue
		}
		rows = append(rows, toFileJSON(item.Node, item.Path))
		nodes = append(nodes, item.Node)
	}
	s.annotate(r.Context(), actor.ID(), rows, nodes, nil)
	// A star is a star whether or not the annotation batch answered; the listing IS the favorites
	// view, so every row in it is true by construction.
	for i := range rows {
		rows[i].Starred = true
		rows[i].FavoriteAt = favoriteAt(items, rows[i].ID)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":     rows,
		"truncated": next != "",
		"cursor":    next,
	})
}

// favoriteAt finds the star timestamp for a rendered row.
func favoriteAt(items []store.FavoriteNode, id string) string {
	for _, item := range items {
		if item.Node.ID.String() == id {
			return item.FavoritedAt.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// ----------------------------------------------------------------------- tags --

// handleTagList lists the tags visible to the caller: the system vocabulary plus their own.
func (s *Server) handleTagList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var tags []store.Tag
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.TagsFor(ctx, actor.ID())
		tags = got
		return lerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// The usage count comes from the same transaction as the list, so a tag added a moment ago
	// cannot be shown with a stale count.
	counts := s.tagUsageCounts(r.Context(), actor.ID(), tags)
	out := make([]tagJSON, 0, len(tags))
	for _, t := range tags {
		row := toTagJSON(t)
		row.Count = counts[t.ID]
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// tagUsageCounts counts visible assignments per tag.
//
// It is a separate optional read rather than part of TagsFor because the count is a DECORATION:
// a backend that cannot answer it cheaply should show a tag list without numbers, not fail.
func (s *Server) tagUsageCounts(ctx context.Context, viewer uuid.UUID, tags []store.Tag) map[uuid.UUID]int {
	counter, ok := s.deps.Meta.(store.TagCounter)
	if !ok || len(tags) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(tags))
	for _, t := range tags {
		ids = append(ids, t.ID)
	}
	counts, err := counter.TagUsageCounts(ctx, viewer, ids)
	if err != nil {
		s.deps.Log.Warn("tag usage counts unavailable", "error", err)
		return nil
	}
	return counts
}

// handleTagCreate defines a personal tag, or a system tag when an administrator asks for one.
func (s *Server) handleTagCreate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Name   string `json:"name"`
		Color  string `json:"color"`
		System bool   `json:"system"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// A SYSTEM tag is the deployment's shared vocabulary, so creating one is an admin action (§5).
	// A non-admin asking for one is refused rather than silently given a personal tag: the two are
	// different objects and a silent downgrade would make "everyone sees this label" false.
	if body.System && !actor.IsAdmin() {
		writeError(w, http.StatusForbidden, "admin_required",
			"a system tag is managed by an administrator; create a personal tag instead")
		return
	}

	tag := store.Tag{Name: body.Name, Color: body.Color}
	if !body.System {
		id := actor.ID()
		tag.OwnerID = &id
		tag.CreatedBy = &id
	} else {
		tag.CreatedBy = actor.Ptr()
	}

	var created store.Tag
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var cerr error
		created, cerr = tx.CreateTag(ctx, tag)
		if cerr != nil {
			return cerr
		}
		if !body.System {
			// Not audited: defining a personal label is not a security-relevant action (§8).
			return nil
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: string(audit.ActionTagSystemChange), TargetKind: "tag",
			TargetID: created.ID.String(), Result: "ok",
			Metadata: map[string]any{"change": "create", "name": created.Name, "color": created.Color},
		})
	})
	if err != nil {
		s.writeTagRefusal(w, err, "no such tag")
		return
	}
	writeJSON(w, http.StatusCreated, toTagJSON(created))
}

// handleTagUpdate renames or recolours a tag.
func (s *Server) handleTagUpdate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id, ok := tagIDFromPath(w, r)
	if !ok {
		return
	}

	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var existing store.Tag
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var terr error
		existing, terr = tx.TagByID(ctx, id)
		return terr
	}); err != nil {
		s.writeTagRefusal(w, err, "no such tag")
		return
	}
	if !s.mayEditTag(actor, existing) {
		writeError(w, http.StatusNotFound, "not_found", "no such tag")
		return
	}

	var updated store.Tag
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var uerr error
		updated, uerr = tx.UpdateTag(ctx, id, body.Name, body.Color)
		if uerr != nil {
			return uerr
		}
		if !existing.IsSystem() {
			return nil
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: string(audit.ActionTagSystemChange), TargetKind: "tag",
			TargetID: id.String(), Result: "ok",
			Metadata: map[string]any{
				"change": "update", "old_name": existing.Name, "name": updated.Name,
				"old_color": existing.Color, "color": updated.Color,
			},
		})
	})
	if err != nil {
		s.writeTagRefusal(w, err, "no such tag")
		return
	}
	writeJSON(w, http.StatusOK, toTagJSON(updated))
}

// handleTagDelete removes a tag and its assignments.
func (s *Server) handleTagDelete(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id, ok := tagIDFromPath(w, r)
	if !ok {
		return
	}

	var existing store.Tag
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var terr error
		existing, terr = tx.TagByID(ctx, id)
		return terr
	}); err != nil {
		s.writeTagRefusal(w, err, "no such tag")
		return
	}
	if !s.mayEditTag(actor, existing) {
		writeError(w, http.StatusNotFound, "not_found", "no such tag")
		return
	}

	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		if derr := tx.DeleteTag(ctx, id); derr != nil {
			return derr
		}
		if !existing.IsSystem() {
			return nil
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: string(audit.ActionTagSystemChange), TargetKind: "tag",
			TargetID: id.String(), Result: "ok",
			Metadata: map[string]any{"change": "delete", "name": existing.Name},
		})
	})
	if err != nil {
		s.writeTagRefusal(w, err, "no such tag")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mayEditTag reports whether this actor may change this tag's definition.
//
// A personal tag belongs to its owner. A system tag belongs to the deployment, so only an
// administrator may change it — and the store refuses to delete one outright, because deleting it
// empties the label off every node in the deployment.
func (s *Server) mayEditTag(actor authz.Actor, tag store.Tag) bool {
	if tag.IsSystem() {
		return actor.IsAdmin()
	}
	return tag.OwnerID != nil && *tag.OwnerID == actor.ID()
}

// handleNodeTags assigns a tag to a node, or removes it on DELETE.
func (s *Server) handleNodeTagsSet(w http.ResponseWriter, r *http.Request) {
	s.handleNodeTagChange(w, r, true)
}

// handleNodeTagsDelete removes one assignment.
func (s *Server) handleNodeTagsDelete(w http.ResponseWriter, r *http.Request) {
	s.handleNodeTagChange(w, r, false)
}

func (s *Server) handleNodeTagChange(w http.ResponseWriter, r *http.Request, assign bool) {
	actor, _ := actorFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	// Assignment is a METADATA WRITE on the node, so it requires `write` (§5). Reading tags only
	// requires `read`; the listing path enforces that by only annotating rows the caller can see.
	cap, cerr := s.deps.Authz.Authorize(actor, nodeID)
	if cerr != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	var body struct {
		TagID string `json:"tag_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tagID, perr := uuid.Parse(body.TagID)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_tag", "not a valid tag id")
		return
	}

	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		tag, terr := tx.TagByID(ctx, tagID)
		if terr != nil {
			return terr
		}
		// A personal tag only makes sense on a node its owner can also write, which the check above
		// establishes. Assigning somebody else's personal tag is refused: it would show up in their
		// listing as a filter entry that matches a file they may never see.
		if tag.OwnerID != nil && *tag.OwnerID != actor.ID() {
			return store.ErrNotFound
		}
		if assign {
			if aerr := tx.AssignTag(ctx, nodeID, tagID, actor.ID()); aerr != nil {
				return aerr
			}
		} else {
			if uerr := tx.UnassignTag(ctx, nodeID, tagID); uerr != nil {
				return uerr
			}
		}
		// The tag set is an indexed field (core-scope §7.2), so a change to it re-indexes the
		// document — in the same transaction, or a tag filter would keep matching a node that no
		// longer carries the tag.
		return enqueueSearchIndex(ctx, tx, nodeID)
	})
	if err != nil {
		s.writeTagRefusal(w, err, "no such tag")
		return
	}

	// Not audited: assignment is not a security-relevant action (§8).
	var tags []store.Tag
	if terr := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.NodeTags(ctx, actor.ID(), nodeID)
		tags = got
		return lerr
	}); terr != nil {
		s.writeStoreError(w, terr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": nodeID.String(), "tags": toTagJSONs(tags),
	})
}

// tagIDFromPath parses the {id} segment, answering 404 for garbage.
func tagIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such tag")
		return uuid.Nil, false
	}
	return id, true
}

// writeTagRefusal maps the tag surface's errors, which are more specific than the file ones.
func (s *Server) writeTagRefusal(w http.ResponseWriter, err error, notFoundMsg string) {
	switch {
	case errors.Is(err, store.ErrTagLimit):
		// 409 rather than 4xx-generic: the request is well formed and the caller has to remove
		// something before it can succeed. The message says so, because "conflict" alone would make
		// them retry.
		writeError(w, http.StatusConflict, "tag_limit",
			"there is no room for another tag; remove one first")
	case errors.Is(err, store.ErrInvalidSocialValue):
		writeError(w, http.StatusUnprocessableEntity, "invalid_value", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "tag_exists",
			"a tag with that name already exists, ignoring case")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", notFoundMsg)
	default:
		s.writeStoreError(w, err)
	}
}

// ------------------------------------------------------------------- comments --

// handleCommentsList returns one node's discussion, in posting order.
func (s *Server) handleCommentsList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	// Permission to comment (and to read a thread) is `read` on the node: you can discuss what you
	// can see (§6). The same Authorize call as /api/nodes/{id}, so a node the caller cannot read is
	// indistinguishable from one that does not exist.
	cap, cerr := s.deps.Authz.Authorize(actor, nodeID)
	if cerr != nil || !cap.Can(authz.PermRead) {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"), searchMax)
	var afterSeq int64
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		seq, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || seq < 0 {
			writeError(w, http.StatusBadRequest, "invalid_cursor",
				"the cursor is not one this server issued; drop it to start from the oldest comment")
			return
		}
		afterSeq = seq
	}

	var comments []store.Comment
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.CommentsAfter(ctx, nodeID, afterSeq, limit+1)
		comments = got
		return lerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	next := ""
	if len(comments) > limit {
		comments = comments[:limit]
		next = strconv.FormatInt(comments[len(comments)-1].Seq, 10)
	}

	authors := s.commentAuthors(r.Context(), comments)
	out := make([]commentJSON, 0, len(comments))
	for _, c := range comments {
		out = append(out, toCommentJSON(c, authors))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     out,
		"truncated": next != "",
		"cursor":    next,
		// Whether this caller may moderate, so the UI can show the action on somebody else's
		// comment instead of offering a button that answers 404.
		"can_moderate": cap.Can(authz.PermManagePermissions),
	})
}

// handleCommentCreate posts a comment.
func (s *Server) handleCommentCreate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	cap, cerr := s.deps.Authz.Authorize(actor, nodeID)
	if cerr != nil || !cap.Can(authz.PermRead) {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	var body struct {
		Body     string `json:"body"`
		ParentID string `json:"parent_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	comment := store.Comment{NodeID: nodeID, AuthorID: actor.ID(), Body: body.Body}
	if body.ParentID != "" {
		parent, perr := uuid.Parse(body.ParentID)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "invalid_parent", "not a valid comment id")
			return
		}
		comment.ParentID = &parent
	}

	limit := store.CommentLimit{
		Limit:  int64(s.deps.Config.Limits.CommentRatePerHour),
		Window: time.Hour,
	}

	var created store.Comment
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var cerr error
		created, cerr = tx.CreateComment(ctx, comment, limit)
		return cerr
	})
	if err != nil {
		s.writeCommentRefusal(w, err)
		return
	}

	// Not audited: comment creation is not a hash-chained action (§8).
	authors := s.commentAuthors(r.Context(), []store.Comment{created})
	writeJSON(w, http.StatusCreated, toCommentJSON(created, authors))
}

// handleCommentUpdate edits the caller's own comment.
func (s *Server) handleCommentUpdate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	comment, cap, ok := s.commentForWrite(w, r, actor)
	if !ok {
		return
	}
	// Editing is the AUTHOR's act alone. Moderation removes; it does not rewrite somebody's words
	// under their name.
	if comment.AuthorID != actor.ID() {
		writeError(w, http.StatusNotFound, "not_found", "no such comment")
		return
	}
	_ = cap

	var body struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var updated store.Comment
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var uerr error
		updated, uerr = tx.UpdateComment(ctx, comment.ID, body.Body, time.Now().UTC())
		return uerr
	})
	if err != nil {
		s.writeCommentRefusal(w, err)
		return
	}
	authors := s.commentAuthors(r.Context(), []store.Comment{updated})
	writeJSON(w, http.StatusOK, toCommentJSON(updated, authors))
}

// handleCommentDelete soft-deletes a comment.
//
// Three parties may do it (§6): the author, an administrator, or a holder of `manage_permissions`
// on the node. Deleting somebody ELSE's comment is a moderation action and is hash-chained (§8);
// deleting your own is not, because it is not a privileged act and the volume of "I removed my own
// typo" would bury the entries that matter.
func (s *Server) handleCommentDelete(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	comment, cap, ok := s.commentForWrite(w, r, actor)
	if !ok {
		return
	}

	isAuthor := comment.AuthorID == actor.ID()
	mayModerate := cap.Can(authz.PermManagePermissions) || actor.IsAdmin()
	if !isAuthor && !mayModerate {
		writeError(w, http.StatusNotFound, "not_found", "no such comment")
		return
	}

	var deleted store.Comment
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var derr error
		deleted, derr = tx.DeleteComment(ctx, comment.ID, actor.ID(), time.Now().UTC())
		if derr != nil {
			return derr
		}
		if isAuthor {
			// The author removing their own text is not a privileged act (§8).
			return nil
		}
		// Moderation: who silenced whom, on which node, and which comment. NO BODY — a comment is
		// user content, not a security fact, and §8 forbids writing one into audit metadata.
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: string(audit.ActionCommentModerate), TargetKind: "comment",
			TargetID: comment.ID.String(), Result: "ok",
			Metadata: map[string]any{
				"node_id":   comment.NodeID.String(),
				"author_id": comment.AuthorID.String(),
			},
		})
	})
	if err != nil {
		s.writeCommentRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": deleted.ID.String(), "deleted": deleted.Deleted(),
	})
}

// commentForWrite resolves the comment in the path and the caller's grant on its node.
func (s *Server) commentForWrite(w http.ResponseWriter, r *http.Request,
	actor authz.Actor) (store.Comment, authz.Cap, bool) {

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such comment")
		return store.Comment{}, authz.Cap{}, false
	}

	var comment store.Comment
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		var cerr error
		comment, cerr = tx.CommentByID(ctx, id)
		return cerr
	}); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such comment")
		return store.Comment{}, authz.Cap{}, false
	}

	// The grant is on the NODE, re-checked now rather than trusted from the comment row: a revoked
	// grant must take the whole thread away, not just the listing.
	cap, cerr := s.deps.Authz.Authorize(actor, comment.NodeID)
	if cerr != nil || !cap.Can(authz.PermRead) {
		writeError(w, http.StatusNotFound, "not_found", "no such comment")
		return store.Comment{}, authz.Cap{}, false
	}
	return comment, cap, true
}

// commentAuthors resolves display names for a page of comments, in one call.
func (s *Server) commentAuthors(ctx context.Context, comments []store.Comment) map[uuid.UUID]string {
	reader, ok := s.deps.Meta.(store.UserLookup)
	if !ok || len(comments) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(comments))
	seen := map[uuid.UUID]bool{}
	for _, c := range comments {
		if !seen[c.AuthorID] {
			seen[c.AuthorID] = true
			ids = append(ids, c.AuthorID)
		}
	}
	out, err := reader.UsernamesFor(ctx, ids)
	if err != nil {
		// A missing display name is cosmetic; the author id is still in every row.
		return nil
	}
	return out
}

// writeCommentRefusal maps the comment surface's errors.
func (s *Server) writeCommentRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrCommentRateLimited):
		// 429 with the product's own code: the client shows "slow down" rather than a generic
		// failure, and the caller knows waiting is the remedy.
		writeError(w, http.StatusTooManyRequests, "comment_rate_limited",
			"you have posted a lot in the last hour; wait a little before posting again")
	case errors.Is(err, store.ErrInvalidSocialValue):
		writeError(w, http.StatusUnprocessableEntity, "invalid_value", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such comment")
	default:
		s.writeStoreError(w, err)
	}
}

// nodeForAnnotation resolves the node an annotation request names.
//
// Two addressing forms, and the id wins when both are present because it is the identity:
//
//	?path=/docs/report.txt   the caller's own namespace
//	?id=<uuid>               a node reached through a grant (a shared folder, a space)
//
// Without the id form, starring a file somebody shared with you is impossible: its path is in THEIR
// namespace, so Resolve can only ever find the caller's own files — where they already have every
// right. That is the same gap the version-history routes had, and it is fixed the same way.
func (s *Server) nodeForAnnotation(r *http.Request, actor authz.Actor) (store.Node, error) {
	q := r.URL.Query()
	if raw := q.Get("id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return store.Node{}, store.ErrNotFound
		}
		var node store.Node
		if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			n, nerr := tx.Node(ctx, id)
			node = n
			return nerr
		}); err != nil {
			return store.Node{}, err
		}
		return node, nil
	}

	rel := q.Get("path")
	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), strings.TrimSpace(rel))
	if err != nil {
		return store.Node{}, err
	}
	return nodes[len(nodes)-1], nil
}
