// Package search is the OPTIONAL content-search engine (core-scope §7.2).
//
// The architecture rule this package exists to enforce is stated in §7 and repeated here because it
// is the one thing a future maintainer is most likely to "fix": SEARCH IS BEST-EFFORT TELEMETRY,
// NOT EVIDENCE.
//
//   - A deployment with no engine still has working name-and-filter search. `search.engine: none`
//     is the default and it is not a degraded mode of the product; it is a smaller version of it.
//   - A write NEVER fails because the index is down. Publishing a file, trashing one, renaming it
//     — all of that succeeds and the index catches up later, or never. The index is a cache of
//     content the server can already read, so losing it loses convenience and nothing else.
//   - An unavailable engine degrades the RESPONSE, not the request: `mode: name` plus
//     `degraded: true`, so a user who searched for a phrase inside a document is told the search
//     did not look there rather than concluding the document does not exist.
//
// This is the deliberate opposite of the audit chain's fail-closed posture, and the two are not in
// tension: an audit record that cannot be written makes the operation unauthorised (the operation
// would be unaccountable), while an index that cannot be written makes a search incomplete (the
// data is right where it was).
//
// The second rule, which is the security one: THE INDEX NEVER AUTHORISES. Every query is filtered
// by live grants computed from the metadata store, and every result is re-checked before it is
// returned. A revoked grant stops returning hits immediately even if the index still holds the
// document, because the check is not in the index.
package search

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrUnavailable reports that the engine could not answer.
//
// Callers must treat it as "degrade", never as "no results": a search box that silently returns
// nothing because Elasticsearch was restarting is indistinguishable from one that found nothing,
// and the difference matters to the person who cannot find their file.
var ErrUnavailable = errors.New("search: engine unavailable")

// ErrIndexRejected reports that the engine refused a document. Distinct from unavailable because
// a rejected document will be rejected again: the job that carries it should stop retrying and let
// an operator look, rather than burning five attempts over an hour.
var ErrIndexRejected = errors.New("search: document rejected by the engine")

// Doc is one indexed node.
//
// It is a CACHE of metadata the server already holds; nothing here is the source of truth for what
// exists. The field set is deliberately the one §7.2 names, because a field that is not here cannot
// be filtered on, and adding one later means re-indexing everything.
type Doc struct {
	// OwnerID and SpaceGroupID are the pre-filter fields: a query is scoped to the subjects the
	// caller may read before any scoring happens, so the engine never sees a query that could
	// return another tenant's document at all.
	OwnerID       uuid.UUID
	SpaceGroupID  *uuid.UUID
	NodeID        uuid.UUID
	Name          string
	Path          string
	Mime          string
	Kind          string
	Size          int64
	ModifiedAt    time.Time
	Tags          []string
	FavoriteCount int
	// Content is the extracted text, already bounded by limits.search_index_bytes. Empty for a
	// document whose bytes were not extracted (an unsupported type, or one above the cap), which
	// still indexes its metadata — so a file too large to read is still findable by name.
	Content string
}

// Hit is one search result.
//
// It carries the node id and nothing else the caller must trust: the HTTP layer re-checks the
// grant and re-reads the node from the metadata store, so a stale or poisoned index can at worst
// return an id that is then refused.
type Hit struct {
	NodeID uuid.UUID
	// Score is the engine's relevance, surfaced so a caller can order a merged result set. It is
	// not comparable across queries and must not be presented as a quality measure.
	Score float64
}

// Query is one content search.
type Query struct {
	// Text is the phrase to look for inside document bodies.
	Text string
	// Limit bounds the hits returned by the engine. The HTTP layer asks for one more than the page
	// size so it can tell "there is more" after the per-node permission re-check has dropped some.
	Limit int
	// Filter narrows the query INSIDE the engine, for the same fields the name search supports
	// (§7.2: "tag and favorite filters compose with content mode").
	Filter Filter
}

// Filter is the engine-side narrowing.
//
// Ownership is NOT part of it: the caller's subject set is passed separately (see Engine.Search) so
// that a filter cannot accidentally widen it. Keeping the two apart is what makes "the index never
// authorises" a property of the shape rather than a discipline.
type Filter struct {
	Kind       string
	MimePrefix string
	SizeMin    int64
	SizeMax    int64
	// ModifiedAfter/ModifiedBefore are zero when unbounded.
	ModifiedAfter  time.Time
	ModifiedBefore time.Time
	// TagIDs matches a document carrying ANY of these tags. Any rather than all: a filter bar that
	// ANDs tags cannot be used to broaden a search, which is the opposite of what clicking a second
	// label means to a person.
	TagIDs []uuid.UUID
	// FavoriteOf, when set, restricts to documents that account has starred.
	FavoriteOf *uuid.UUID
}

// Engine is the content-search backend.
//
// Four methods, because that is the whole contract: answer a query, make one document current,
// remove one, and report whether it is reachable. Anything larger would be an engine-specific API
// leaking upward, and anything smaller could not express deletion.
type Engine interface {
	// Available reports whether the engine can answer right now.
	//
	// It must be cheap: the HTTP layer calls it on every content search to decide between
	// `mode: content` and `mode: name, degraded: true`, and a health check that takes a second
	// would make the degraded path slower than the working one.
	Available() bool

	// Search runs a query scoped to the given owners and space groups.
	//
	// scopes is the caller's live subject set (their own id plus every group they belong to).
	// An EMPTY scopes slice must return nothing rather than everything: that is the fail-closed
	// reading, and the alternative — treating empty as "no filter" — is the enumeration oracle
	// §7.2 forbids.
	Search(ctx context.Context, scopes Scope, q Query) ([]Hit, error)

	// Index makes one document current. It must be idempotent: the queue is at-least-once, so the
	// same document will arrive twice after a crashed worker.
	Index(ctx context.Context, doc Doc) error

	// Delete removes one document from the index. Idempotent for the same reason: a delete for a
	// document that was never indexed is a success, not an error.
	Delete(ctx context.Context, nodeID uuid.UUID) error
}

// Scope is the caller's subject set for a search, computed from LIVE grants.
//
// It is a distinct type rather than a bare slice so it cannot be built by accident from the wrong
// thing: a caller that passes "everyone" has to name a value with that meaning, and the meaning is
// written down here.
type Scope struct {
	// Owners is the caller's own account id plus the ids of spaces they belong to.
	Owners []uuid.UUID
	// Groups is every group the caller is a member of. A document shared to a group is visible to
	// its members, and the index stores the owning space's group id rather than a member list
	// (a member list would have to be re-indexed every time somebody joined).
	Groups []uuid.UUID
}

// Empty reports whether the scope can match nothing.
func (s Scope) Empty() bool { return len(s.Owners) == 0 && len(s.Groups) == 0 }

// UnavailableEngine is the engine of a deployment with `search.engine: none`.
//
// It exists rather than a nil interface with an "if m != nil" at every call site: a nil pointer
// embeds a nil check in every branch, and one of them eventually forgets. This type answers
// honestly — not available, no hits, and indexing is a no-op that reports success — which is
// exactly what "optional" means.
type UnavailableEngine struct{}

// Available reports false.
func (UnavailableEngine) Available() bool { return false }

// Search returns nothing rather than an error, so a caller that did not check Available first
// degrades instead of failing a request.
func (UnavailableEngine) Search(context.Context, Scope, Query) ([]Hit, error) { return nil, nil }

// Index is a no-op: with no engine there is nothing to be behind.
func (UnavailableEngine) Index(context.Context, Doc) error { return nil }

// Delete is a no-op.
func (UnavailableEngine) Delete(context.Context, uuid.UUID) error { return nil }
