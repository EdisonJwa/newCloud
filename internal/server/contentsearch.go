package server

// Content search over the optional engine (core-scope §7.2).
//
// The security property this file exists to preserve, stated once because every line below serves
// it: THE INDEX NEVER AUTHORISES. An engine query is a RETRIEVAL, not a decision. So the flow is
// deliberately three separate steps, and none of them trusts the previous one:
//
//  1. COMPUTE the caller's live subject set from the metadata store (their own account plus the
//     groups they belong to). This is what the engine's query is scoped to, so a document that the
//     caller has no business seeing is never in the candidate set at all.
//
//  2. RETRIEVE candidate node ids from the engine.
//
//  3. RE-CHECK each candidate with the same Authorize call every other node route uses, and drop
//     the ones it refuses. This is the half that makes a revoked grant take effect immediately even
//     when the index still holds the document: the index can be minutes behind, the authorizer
//     cannot.
//
// Step 3 is why the engine is asked for MORE hits than the page needs. A page of ten after the
// re-check would otherwise be five, and a client would page forever through documents it may not
// see.

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/search"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// contentSearchOverscan is how many extra candidates are requested.
//
// A factor rather than a fixed number: the interesting case is a shared deployment where most
// documents belong to somebody else, and a constant would be wrong at both ends of that scale.
const contentSearchOverscan = 4

// handleContentSearch answers mode=content or mode=all.
//
// `all` merges the engine's hits with the name search, so a document is found by its filename AND
// by a phrase inside it in one query. The merge is by node id, name results first: a name match is
// a stronger signal than a body match for the thing the user typed, and it costs nothing to prefer.
func (s *Server) handleContentSearch(w http.ResponseWriter, r *http.Request, actor authz.Actor,
	q store.SearchQuery, mode searchMode) {

	scope := s.searchScope(r.Context(), actor)
	if scope.Empty() {
		// Fail closed. A caller with no subjects can read nothing, and an unscoped query would be
		// the enumeration oracle §7.2 forbids.
		writeJSON(w, http.StatusOK, s.emptySearchBody(q, mode, false))
		return
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}

	hits, err := s.deps.Search.Search(r.Context(), scope, search.Query{
		Text:  q.Text,
		Limit: limit * contentSearchOverscan,
		Filter: search.Filter{
			Kind:           string(q.Kind),
			MimePrefix:     q.MimePrefix,
			SizeMin:        q.SizeMin,
			SizeMax:        q.SizeMax,
			ModifiedAfter:  q.ModifiedAfter,
			ModifiedBefore: q.ModifiedBefore,
			TagIDs:         tagIDsOf(q),
		},
	})
	if err != nil {
		// Degrade rather than fail: the request is answerable without the engine, and a 5xx would
		// tell the client the SERVER is broken when the truth is that a convenience is missing.
		s.deps.Log.Warn("content search degraded", "error", err)
		if mode == searchModeContent {
			if s.deps.Metrics != nil {
				s.deps.Metrics.ObserveSearchIndexError()
			}
			writeJSON(w, http.StatusOK, s.emptySearchBody(q, searchModeName, true))
			return
		}
		// mode=all still has its name half, which needs no engine at all.
		s.writeNameOnlyResults(w, r, actor, q, true)
		return
	}

	// Step 3: re-check every candidate against live grants, and re-read the node so the response
	// carries the metadata the listing would.
	nodes := make([]store.Node, 0, len(hits))
	paths := make([]string, 0, len(hits))
	seen := map[uuid.UUID]bool{}
	for _, hit := range hits {
		if len(nodes) >= limit {
			break
		}
		if seen[hit.NodeID] {
			continue
		}
		seen[hit.NodeID] = true

		if _, cerr := s.deps.Authz.Authorize(actor, hit.NodeID); cerr != nil {
			// A revoked grant, a trashed node or a document from another tenant: dropped here, not
			// reported. Answering 403 mentions the id, and an id is not a capability.
			continue
		}
		node, path, nerr := s.nodeAndPath(r.Context(), hit.NodeID)
		if nerr != nil || node.TrashedAt != nil {
			continue
		}
		nodes = append(nodes, node)
		paths = append(paths, path)
	}

	items := make([]fileJSON, 0, len(nodes))
	for i, node := range nodes {
		items = append(items, toFileJSON(node, paths[i]))
	}
	s.annotate(r.Context(), actor.ID(), items, nodes, nil)

	body := map[string]any{
		"items": items, "query": q.Text, "mode": mode,
		// Content search is not paginated in v1: relevance ordering plus a keyset would need the
		// engine to return a stable secondary key, and §7.2 leaves highlighting and deep paging out
		// of the first release. Saying `truncated` honestly is better than a cursor nobody tested.
		"truncated": len(items) >= limit,
		"cursor":    "",
	}
	writeJSON(w, http.StatusOK, body)
}

// writeNameOnlyResults falls back to the name search when mode=all lost its engine half.
func (s *Server) writeNameOnlyResults(w http.ResponseWriter, r *http.Request, actor authz.Actor,
	q store.SearchQuery, degraded bool) {

	searcher, ok := s.deps.Meta.(store.FilterSearcher)
	if !ok {
		writeJSON(w, http.StatusOK, s.emptySearchBody(q, searchModeName, degraded))
		return
	}
	page, err := searcher.SearchNodes(r.Context(), actor.ID(), q)
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) || errors.Is(err, store.ErrInvalidSearch) {
			writeError(w, http.StatusBadRequest, "invalid_search", err.Error())
			return
		}
		s.writeStoreError(w, err)
		return
	}
	s.writeSearchPage(w, r, actor, page, q, searchModeName, degraded)
}

// searchScope computes the caller's live subject set.
//
// "Live" is the whole point: it is read from the metadata store on every query, so a group the
// caller was removed from a moment ago is absent, and the engine cannot return a document whose
// only claim to visibility was that membership.
func (s *Server) searchScope(ctx context.Context, actor authz.Actor) search.Scope {
	scope := search.Scope{Owners: []uuid.UUID{actor.ID()}}

	groups, err := s.groupsOf(ctx, actor.ID())
	if err != nil {
		// A failure here is a failure to compute visibility, so the caller gets their own files and
		// nothing else. The alternative — proceeding with an empty group list — is what this
		// already is, and proceeding with NO owners would be "everyone".
		s.deps.Log.Warn("could not resolve groups for a search scope", "error", err)
		return scope
	}
	scope.Groups = groups
	return scope
}

// groupsOf lists the group ids an account belongs to.
func (s *Server) groupsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		groups, gerr := tx.GroupsForUser(ctx, userID)
		for _, g := range groups {
			out = append(out, g.ID)
		}
		return gerr
	})
	return out, err
}

// nodeAndPath reads one node and reconstructs its path for the search response.
func (s *Server) nodeAndPath(ctx context.Context, id uuid.UUID) (store.Node, string, error) {
	var node store.Node
	if err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, id)
		node = n
		return nerr
	}); err != nil {
		return store.Node{}, "", err
	}
	rel, err := s.pathOfNode(ctx, node.OwnerID, node.ID)
	if err != nil {
		return store.Node{}, "", err
	}
	return node, rel, nil
}

// tagIDsOf returns the tag filter as a slice, which is the shape the engine takes.
func tagIDsOf(q store.SearchQuery) []uuid.UUID {
	if q.TagID == nil {
		return nil
	}
	return []uuid.UUID{*q.TagID}
}

// tagsOfQuery renders the tag filter for the response, so a client can show what it filtered on.
//
// Nil when there is no tag filter: an empty array would read as "filtered on nothing", which is a
// different statement from "no tag filter applied".
func (s *Server) tagsOfQuery(r *http.Request, actor authz.Actor) []tagJSON {
	raw := r.URL.Query().Get("tag")
	if raw == "" {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil
	}
	var tag store.Tag
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		t, terr := tx.TagByID(ctx, id)
		tag = t
		return terr
	}); err != nil {
		// The filter was accepted but the tag is now gone; omitting it from the echo is honest and
		// harmless, because the result set is already empty.
		return nil
	}
	return []tagJSON{toTagJSON(tag)}
}

var _ = authz.PermRead
