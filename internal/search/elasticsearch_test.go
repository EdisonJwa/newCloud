package search_test

// The Elasticsearch engine, against a fake cluster (core-scope §7.2).
//
// The interesting assertions here are not "does it speak Elasticsearch". They are the three
// properties the package doc claims, because each of them is a security or availability decision
// that a refactor could quietly invert:
//
//  1. THE SCOPE FILTER CANNOT BE WIDENED. It is the first element of `filter`, it is built from the
//     caller's live subjects rather than from the query, and an empty scope makes no request at all.
//     A test that only checked "a filter is present" would pass against a query that also accepted
//     a client-supplied owner list.
//  2. A MISSING INDEX IS NOT A FAILURE. A fresh cluster answers 404 to `_search` before the backfill
//     creates anything, and reporting that as `ErrUnavailable` would make every content search on a
//     new deployment say "the engine is down".
//  3. RETRYABLE AND REJECTED ARE DIFFERENT. A mapping conflict (400) must stop a job; a restart
//     (503, or an index that does not exist yet) must not. Confusing the two either burns the
//     attempt budget on a permanent failure or permanently drops a document after one blip.
//
// A fake cluster rather than a real one, because these are contract tests: they must run in CI with
// no container, and the contract is this client's behaviour, not Elasticsearch's.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/search"
)

// cluster is a scriptable fake Elasticsearch.
type cluster struct {
	mu sync.Mutex
	// requests records what the client sent, in order.
	requests []recorded
	// status is the code every request gets.
	status int
	// body is the response body; for `_search` the default is an empty hit list.
	body string
	// routes overrides the answer for "METHOD /path-prefix". Method matters: `EnsureMapping` asks
	// HEAD whether the index exists and then PUTs it, and a fake keyed only by path cannot tell
	// those two apart — which is how a test ends up asserting the wrong branch.
	routes map[string]responder
	// seen counts every request, for the failover check.
	seen int
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   []byte
}

type responder struct {
	status int
	body   string
}

func newCluster(t *testing.T) (*cluster, *httptest.Server) {
	t.Helper()
	c := &cluster{status: http.StatusOK, body: `{"hits":{"hits":[]}}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		user, pass, _ := r.BasicAuth()
		auth := ""
		if user != "" {
			auth = user + ":" + pass
		}
		c.mu.Lock()
		c.requests = append(c.requests, recorded{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: auth, Body: raw,
		})
		c.seen++
		status, body := c.status, c.body
		for key, override := range c.routes {
			method, prefix, ok := strings.Cut(key, " ")
			if !ok || method != r.Method {
				continue
			}
			if strings.HasPrefix(r.URL.Path, prefix) {
				status, body = override.status, override.body
				break
			}
		}
		c.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

// lastSearch decodes the most recent `_search` body.
func (c *cluster) lastSearch(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.requests) - 1; i >= 0; i-- {
		if strings.HasSuffix(c.requests[i].Path, "/_search") {
			var out map[string]any
			if err := json.Unmarshal(c.requests[i].Body, &out); err != nil {
				t.Fatalf("the search body is not JSON: %v", err)
			}
			return out
		}
	}
	t.Fatal("no _search request was made")
	return nil
}

func engineFor(srv *httptest.Server, mutate func(*search.ESConfig)) *search.ES {
	cfg := search.ESConfig{URLs: []string{srv.URL}, Timeout: 2 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	return search.NewES(cfg, func(string, ...any) {})
}

// TestAnEmptyScopeFailsClosedAndSendsNothing is §7.2's central rule, in its cheapest form.
//
// "Empty scope means everything" is the single most dangerous possible reading, and the only way to
// be sure it is not implemented is to assert both halves: no hits, AND no request. A test that only
// checked the hits would pass against an implementation that queried and then filtered locally.
func TestAnEmptyScopeFailsClosedAndSendsNothing(t *testing.T) {
	c, srv := newCluster(t)
	engine := engineFor(srv, nil)

	hits, err := engine.Search(context.Background(), search.Scope{}, search.Query{Text: "secret"})
	if err != nil {
		t.Fatalf("an empty scope returned an error (%v); it must return nothing, because a caller "+
			"with no subjects is a caller with no results, not a broken request", err)
	}
	if len(hits) != 0 {
		t.Fatalf("an empty scope returned %d hits", len(hits))
	}

	c.mu.Lock()
	requests := len(c.requests)
	c.mu.Unlock()
	if requests != 0 {
		t.Fatalf("an empty scope made %d request(s); it must make none, because the only query that "+
			"could be built from it is an unscoped one", requests)
	}
}

// TestTheScopeFilterIsFirstAndCannotBeWidened is the other half: the filter that decides visibility
// is built from the SCOPE, is the first element, and no field of the query can remove it.
func TestTheScopeFilterIsFirstAndCannotBeWidened(t *testing.T) {
	c, srv := newCluster(t)
	engine := engineFor(srv, nil)

	owner := uuid.New()
	group := uuid.New()
	// The query carries every filter the HTTP layer can set, which is also every field a crafted
	// request could try to use to reach another account's documents.
	_, err := engine.Search(context.Background(),
		search.Scope{Owners: []uuid.UUID{owner}, Groups: []uuid.UUID{group}},
		search.Query{
			Text:  "quarterly",
			Limit: 10,
			Filter: search.Filter{
				Kind:       "file",
				MimePrefix: "TEXT/",
				SizeMin:    1,
				SizeMax:    100,
				TagIDs:     []uuid.UUID{uuid.New()},
			},
		})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	body := c.lastSearch(t)
	query := firstObject(t, body, "query")
	boolQuery := firstObject(t, query, "bool")
	filters := firstList(t, boolQuery, "filter")

	if len(filters) == 0 {
		t.Fatal("the query has no filter list, so nothing constrains which documents can be scored")
	}

	// The first filter is the scope, and it is a bool with minimum_should_match: 1 over the two
	// subject dimensions. Checking its POSITION matters: a scope appended after the client's
	// filters would still be present, and would still be the thing an "or" could escape.
	scope := object(t, filters[0], "bool")
	if got := scope["minimum_should_match"]; got != float64(1) {
		t.Fatalf("the scope filter has minimum_should_match %v, want 1: without it an empty "+
			"subject list matches every document", got)
	}
	should := firstList(t, scope, "should")
	if len(should) != 2 {
		t.Fatalf("the scope filter has %d clause(s), want 2 (owners and groups)", len(should))
	}

	termsOf := func(clause any, field string) []string {
		wrapped := firstObjectFrom(clause)
		terms := wrapped["terms"]
		tm, ok := terms.(map[string]any)
		if !ok {
			return nil
		}
		raw, ok := tm[field].([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if got := termsOf(should[0], "owner_id"); len(got) != 1 || got[0] != owner.String() {
		t.Fatalf("the owner clause names %v, want exactly the caller %s: a scope built from "+
			"anything but the live subject set is an enumeration oracle", got, owner)
	}
	if got := termsOf(should[1], "space_group_id"); len(got) != 1 || got[0] != group.String() {
		t.Fatalf("the group clause names %v, want exactly %s", got, group)
	}

	// The mime prefix is case-folded, because a client-declared type may not be and "TEXT/" that
	// fails to match "text/plain" is a filter that silently drops results.
	if !strings.Contains(string(mustJSON(t, filters)), `"mime":"text/"`) {
		t.Fatalf("the mime prefix was not lower-cased in the filter list: %s",
			mustJSON(t, filters))
	}
}

// TestAMissingIndexIsNoResultsRatherThanAFailure pins property 2.
func TestAMissingIndexIsNoResultsRatherThanAFailure(t *testing.T) {
	// A cluster whose every path answers 404 is what a fresh deployment looks like before the
	// backfill has created the index.
	c, srv := newCluster(t)
	c.status = http.StatusNotFound
	c.body = `{"error":"no such index","status":404}`
	engine := engineFor(srv, nil)

	hits, err := engine.Search(context.Background(),
		search.Scope{Owners: []uuid.UUID{uuid.New()}}, search.Query{Text: "anything"})
	if err != nil {
		t.Fatalf("a 404 from _search returned %v; on a fresh cluster that is \"no documents yet\", "+
			"not \"the engine is down\", and the difference reaches the user as \"no matches\" "+
			"versus \"search is unavailable\"", err)
	}
	if len(hits) != 0 {
		t.Fatalf("a 404 returned %d hits, want none", len(hits))
	}
}

// TestUnavailableAndRejectedAreDistinct pins property 3, which is the one that decides whether a
// document is retried forever or dropped after one bad answer.
func TestUnavailableAndRejectedAreDistinct(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
		why    string
	}{
		{
			name:   "a mapping conflict is final",
			status: http.StatusBadRequest,
			want:   search.ErrIndexRejected,
			why:    "a 400 answers the same way on every retry, so the job must stop",
		},
		{
			name:   "a missing index is retryable",
			status: http.StatusNotFound,
			want:   search.ErrUnavailable,
			why:    "the mapping bootstrap has not run yet, and it will",
		},
		{
			name:   "a cluster that is restarting is retryable",
			status: http.StatusServiceUnavailable,
			want:   search.ErrUnavailable,
			why:    "a blip must not permanently drop a document from the index",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := newCluster(t)
			c.status = tc.status
			c.body = `{"error":"scripted","status":` + itoa(tc.status) + `}`
			engine := engineFor(srv, nil)

			err := engine.Index(context.Background(), search.Doc{
				NodeID: uuid.New(), OwnerID: uuid.New(), Name: "x.txt", Kind: "file",
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Index with status %d returned %v, want %v (%s)",
					tc.status, err, tc.want, tc.why)
			}
			// And the two must not both match: errors.Is on the WRONG sentinel has to be false,
			// or a caller's `if errors.Is(err, ErrIndexRejected) { stop() }` would fire on a blip.
			other := search.ErrUnavailable
			if tc.want == search.ErrUnavailable {
				other = search.ErrIndexRejected
			}
			if errors.Is(err, other) {
				t.Fatalf("Index with status %d ALSO matches %v; the two outcomes are not "+
					"distinguishable, so a caller cannot decide between retrying and stopping",
					tc.status, other)
			}
		})
	}
}

// TestDeletingAMissingDocumentIsSuccess pins the idempotence the queue requires.
func TestDeletingAMissingDocumentIsSuccess(t *testing.T) {
	c, srv := newCluster(t)
	c.status = http.StatusNotFound
	engine := engineFor(srv, nil)

	if err := engine.Delete(context.Background(), uuid.New()); err != nil {
		t.Fatalf("deleting a document that is not indexed returned %v; the queue is at-least-once, "+
			"so a repeat delivery of the same delete must succeed", err)
	}
}

// TestIndexingIsIdempotentByDocumentID checks the write is keyed by node id, so the same document
// arriving twice overwrites rather than accumulating.
func TestIndexingIsIdempotentByDocumentID(t *testing.T) {
	c, srv := newCluster(t)
	engine := engineFor(srv, nil)
	node := uuid.New()

	for i := 0; i < 2; i++ {
		if err := engine.Index(context.Background(), search.Doc{
			NodeID: node, OwnerID: uuid.New(), Name: "same.txt", Kind: "file",
		}); err != nil {
			t.Fatalf("index %d: %v", i, err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) != 2 {
		t.Fatalf("%d requests for two writes", len(c.requests))
	}
	for _, r := range c.requests {
		if r.Method != http.MethodPut {
			t.Fatalf("Index used %s; a document addressed by id is a PUT, and a POST would create "+
				"a second document per delivery", r.Method)
		}
		if !strings.HasSuffix(r.Path, "/_doc/"+node.String()) {
			t.Fatalf("Index wrote to %q, which is not keyed by the node id", r.Path)
		}
	}
}

// TestOnlyWellFormedHitsAreReturned: a document whose id is not a UUID was not written by this
// product, and handing it to the metadata lookup would be handing over a value nobody validated.
func TestOnlyWellFormedHitsAreReturned(t *testing.T) {
	c, srv := newCluster(t)
	good := uuid.New()
	c.body = `{"hits":{"hits":[
		{"_id":"` + good.String() + `","_score":2.5},
		{"_id":"not-a-uuid","_score":9.9},
		{"_id":"","_score":1.0}
	]}}`
	engine := engineFor(srv, nil)

	hits, err := engine.Search(context.Background(),
		search.Scope{Owners: []uuid.UUID{uuid.New()}}, search.Query{Text: "x"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != good {
		t.Fatalf("the hits are %v, want only %s: a document whose id is not a UUID was not "+
			"written by this product", hits, good)
	}
	if hits[0].Score != 2.5 {
		t.Fatalf("the score was dropped (%v); a merged result set cannot be ordered without it",
			hits[0].Score)
	}
}

// TestAvailableIsCached: the HTTP layer probes on every content search, so an uncached probe would
// make the degraded path cost a network round trip per keystroke.
func TestAvailableIsCached(t *testing.T) {
	c, srv := newCluster(t)
	engine := engineFor(srv, nil)

	for i := 0; i < 5; i++ {
		if !engine.Available() {
			t.Fatalf("probe %d reported unavailable against a healthy cluster", i)
		}
	}

	c.mu.Lock()
	probes := len(c.requests)
	c.mu.Unlock()
	if probes > 1 {
		t.Fatalf("five probes made %d request(s); the answer must be cached for a short window, "+
			"or every search pays for a health check", probes)
	}
}

// TestAvailableReportsAFailedCluster: the cache must not paper over an outage that outlives it.
func TestAvailableReportsAFailedCluster(t *testing.T) {
	c, srv := newCluster(t)
	c.status = http.StatusServiceUnavailable
	engine := engineFor(srv, nil)

	if engine.Available() {
		t.Fatal("Available reported true against a cluster answering 503; the HTTP layer would then " +
			"claim mode=content and return an empty result that looks like \"no matches\"")
	}
}

// TestTheFirstReachableURLWins covers the failover the config documents ("tried in order").
func TestTheFirstReachableURLWins(t *testing.T) {
	_, bad := newCluster(t)
	bad.Close() // a cluster that is gone, not one that answers an error

	c, good := newCluster(t)
	engine := search.NewES(search.ESConfig{
		URLs:    []string{bad.URL, good.URL},
		Timeout: 2 * time.Second,
	}, func(string, ...any) {})

	hits, err := engine.Search(context.Background(),
		search.Scope{Owners: []uuid.UUID{uuid.New()}}, search.Query{Text: "x"})
	if err != nil {
		t.Fatalf("search with a dead first URL returned %v; the configuration says the URLs are "+
			"tried in order, so one unreachable node must not fail the query", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits: %v", hits)
	}
	if c.seen == 0 {
		t.Fatal("the second URL was never reached")
	}
}

// TestCredentialsAreSentWhenConfigured: an operator behind basic authentication has no other way to
// say who they are, and a silently unauthenticated request reads as a permissions problem.
func TestCredentialsAreSentWhenConfigured(t *testing.T) {
	c, srv := newCluster(t)
	engine := engineFor(srv, func(cfg *search.ESConfig) {
		cfg.Username = "elastic"
		cfg.Password = "s3cret"
	})

	_ = engine.Index(context.Background(), search.Doc{NodeID: uuid.New(), OwnerID: uuid.New()})

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		t.Fatal("no request was made")
	}
	if got := c.requests[0].Auth; got != "elastic:s3cret" {
		t.Fatalf("the request carried %q as credentials", got)
	}
}

// TestNoCredentialsMeansAnonymous: an empty username must not produce an empty Authorization header,
// which some proxies reject and others treat as a malformed credential.
func TestNoCredentialsMeansAnonymous(t *testing.T) {
	c, srv := newCluster(t)
	engine := engineFor(srv, nil)
	_, _ = engine.Search(context.Background(),
		search.Scope{Owners: []uuid.UUID{uuid.New()}}, search.Query{Text: "x"})

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.requests[0].Auth != "" {
		t.Fatalf("an unconfigured engine sent credentials %q", c.requests[0].Auth)
	}
}

// TestEnsureMappingCreatesOnlyWhenAbsent: creating it unconditionally would answer 400 on every
// backfill pass, and the index's own existence is the signal.
func TestEnsureMappingCreatesOnlyWhenAbsent(t *testing.T) {
	t.Run("already present", func(t *testing.T) {
		c, srv := newCluster(t)
		c.routes = map[string]responder{
			"HEAD /newcloud-files": {status: http.StatusOK},
		}
		if err := engineFor(srv, nil).EnsureMapping(context.Background()); err != nil {
			t.Fatalf("EnsureMapping against an existing index: %v", err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, r := range c.requests {
			if r.Method == http.MethodPut {
				t.Fatal("EnsureMapping issued a PUT against an index that already exists")
			}
		}
	})

	t.Run("absent", func(t *testing.T) {
		c, srv := newCluster(t)
		c.routes = map[string]responder{
			"HEAD /newcloud-files": {status: http.StatusNotFound},
			"PUT /newcloud-files":  {status: http.StatusOK},
		}
		if err := engineFor(srv, nil).EnsureMapping(context.Background()); err != nil {
			t.Fatalf("EnsureMapping against a missing index: %v", err)
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		var put *recorded
		for i := range c.requests {
			if c.requests[i].Method == http.MethodPut {
				put = &c.requests[i]
			}
		}
		if put == nil {
			t.Fatal("EnsureMapping did not create the index")
		}
		// The mapping has to type the fields the filters rely on. A `keyword`-typed uuid that
		// became `text` would be analysed into fragments and the scope filter would stop matching.
		for _, field := range []string{"owner_id", "space_group_id", "tags", "modified_at",
			"content", "mime", "kind", "size", "favorite_count"} {
			if !strings.Contains(string(put.Body), `"`+field+`"`) {
				t.Fatalf("the mapping does not declare %q, so a filter on it cannot be answered",
					field)
			}
		}
		// And the id fields must be `keyword`: an analysed uuid is fragments, not an id.
		for _, id := range []string{"owner_id", "space_group_id", "node_id", "tags"} {
			if !strings.Contains(string(put.Body), `"`+id+`":{"type":"keyword"}`) {
				t.Fatalf("%q is not mapped as a keyword, so a terms filter on it would match "+
					"tokenised fragments instead of identifiers:\n%s", id, put.Body)
			}
		}
	})
}

// TestTheIndexNameIsPrefixed: two deployments on one cluster must not overwrite each other, and that
// is a data-integrity failure that presents as a search bug.
func TestTheIndexNameIsPrefixed(t *testing.T) {
	_, srv := newCluster(t)

	if got := engineFor(srv, nil).IndexName(); got != "newcloud-files" {
		t.Fatalf("the default index name is %q, want newcloud-files", got)
	}
	if got := engineFor(srv, func(cfg *search.ESConfig) { cfg.IndexPrefix = "acme" }).
		IndexName(); got != "acme-files" {
		t.Fatalf("a prefixed index is named %q, want acme-files", got)
	}
}

// TestTheUnavailableEngineAnswersHonestly is the `search.engine: none` shape.
//
// It exists so that a deployment without an engine has an OBJECT rather than a nil check at every
// call site; one of those checks is eventually forgotten. What it must never do is claim to have
// searched.
func TestTheUnavailableEngineAnswersHonestly(t *testing.T) {
	var engine search.Engine = search.UnavailableEngine{}

	if engine.Available() {
		t.Fatal("UnavailableEngine claims to be available; the HTTP layer would then report " +
			"mode=content for a deployment with no index")
	}
	hits, err := engine.Search(context.Background(), search.Scope{Owners: []uuid.UUID{uuid.New()}},
		search.Query{Text: "x"})
	if err != nil {
		t.Fatalf("UnavailableEngine.Search returned an error (%v); a caller that did not check "+
			"Available must degrade rather than fail the request", err)
	}
	if len(hits) != 0 {
		t.Fatalf("UnavailableEngine returned %d hits", len(hits))
	}
	// Indexing reports success because there is nothing to be behind. An error here would make the
	// worker retry a job that can never succeed on this deployment.
	if err := engine.Index(context.Background(), search.Doc{NodeID: uuid.New()}); err != nil {
		t.Fatalf("UnavailableEngine.Index returned %v", err)
	}
	if err := engine.Delete(context.Background(), uuid.New()); err != nil {
		t.Fatalf("UnavailableEngine.Delete returned %v", err)
	}
}

/* ------------------------------------------------------------------ helpers --- */

func object(t *testing.T, raw any, key string) map[string]any {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", raw)
	}
	inner, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is absent or not an object in %s", key, mustJSON(t, m))
	}
	return inner
}

func firstObject(t *testing.T, raw any, key string) map[string]any {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", raw)
	}
	return object(t, m, key)
}

// firstObjectFrom unwraps one element of a filter list, which is `{"term": {...}}` or `{"bool": {...}}`.
func firstObjectFrom(clause any) map[string]any {
	m, _ := clause.(map[string]any)
	return m
}

func firstList(t *testing.T, raw any, key string) []any {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", raw)
	}
	list, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%q is absent or not a list in %s", key, mustJSON(t, m))
	}
	return list
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
