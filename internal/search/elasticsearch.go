package search

// Elasticsearch engine (core-scope §7.2).
//
// The implementation is deliberately small and speaks only what this product needs: one index per
// deployment (prefix-configurable so two deployments can share a cluster), a document keyed by node
// id, and a filtered query. It does NOT use the official Go client, for the same reason the metrics
// package does not use client_golang: the surface here is four operations, and a dependency that
// brings its own transport, retry and compatibility matrix is larger than the feature.
//
// Compatibility note that matters for an operator: this uses the `_search`, `_doc` and `_bulk`-free
// single-document REST surface, which has been stable from Elasticsearch 5 through 8. It
// deliberately does not use a request-shape that changed between majors, so an operator's existing
// cluster version is not a compatibility puzzle.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ESConfig is the deployment's engine configuration (search.elasticsearch.*).
type ESConfig struct {
	// URLs is the cluster's HTTP endpoints, tried in order.
	URLs []string
	// Username and Password authenticate. Empty means anonymous, which is normal behind a proxy
	// that adds credentials and unusual otherwise.
	Username string
	Password string
	// IndexPrefix namespaces this deployment's index. Two deployments sharing a cluster would
	// otherwise overwrite each other's documents, which is a data-integrity problem that looks like
	// a search bug.
	IndexPrefix string
	// InsecureSkipVerify disables TLS verification. It exists for the same reason the mail
	// transport has the key: a self-signed certificate on a private network. It removes the only
	// protection against an active attacker on that path, so startup logs it.
	InsecureSkipVerify bool
	// Timeout bounds every request. A search that hangs is a page that hangs, and the degraded
	// path exists precisely so this cannot happen.
	Timeout time.Duration
}

// ES is the Elasticsearch-backed Engine.
type ES struct {
	cfg    ESConfig
	client *http.Client
	// available caches reachability for a short window.
	//
	// Not a live probe per call: the HTTP layer asks on every content search, and a network round
	// trip per keystroke-driven search would make the degraded path cost more than the working one.
	// A short window means an engine that comes back is noticed within seconds, which is the
	// responsiveness a user expects after a restart.
	mu          sync.Mutex
	lastCheck   time.Time
	lastHealthy bool
	logger      func(format string, args ...any)
}

// NewES builds an engine. It performs no I/O: a cluster that is down at startup must not prevent
// the server from starting, because the product works without it.
func NewES(cfg ESConfig, logf func(string, ...any)) *ES {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.IndexPrefix == "" {
		cfg.IndexPrefix = "newcloud"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &ES{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout, Transport: transport},
		logger: logf,
	}
}

// IndexName is the concrete index this engine reads and writes.
//
// Exported so an operator's runbook and the mapping bootstrap can name it without duplicating the
// derivation.
func (e *ES) IndexName() string { return e.cfg.IndexPrefix + "-files" }

// Available probes the cluster, caching the answer briefly.
func (e *ES) Available() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.lastCheck) < 3*time.Second {
		return e.lastHealthy
	}
	e.lastCheck = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
	defer cancel()
	status, _, err := e.do(ctx, http.MethodGet, "/", nil)
	e.lastHealthy = err == nil && status >= 200 && status < 300
	return e.lastHealthy
}

// Search runs a filtered content query scoped to the caller's subjects.
func (e *ES) Search(ctx context.Context, scopes Scope, q Query) ([]Hit, error) {
	if scopes.Empty() {
		// Fail closed. An empty scope means "this caller may read nothing", and the alternative
		// reading — no filter at all — is the enumeration oracle §7.2 forbids outright.
		return nil, nil
	}

	// The scope filter is the FIRST thing in the query, so a caller's visibility is decided before
	// scoring and cannot be widened by a filter the client sends.
	filters := []any{
		map[string]any{
			"bool": map[string]any{
				"should": []any{
					map[string]any{"terms": map[string]any{"owner_id": uuidStrings(scopes.Owners)}},
					map[string]any{"terms": map[string]any{
						"space_group_id": uuidStrings(scopes.Groups),
					}},
				},
				"minimum_should_match": 1,
			},
		},
	}

	if q.Filter.Kind != "" {
		filters = append(filters, map[string]any{
			"term": map[string]any{"kind": q.Filter.Kind},
		})
	}
	if q.Filter.MimePrefix != "" {
		filters = append(filters, map[string]any{
			"prefix": map[string]any{"mime": strings.ToLower(q.Filter.MimePrefix)},
		})
	}
	if q.Filter.SizeMin > 0 || q.Filter.SizeMax > 0 {
		rng := map[string]any{}
		if q.Filter.SizeMin > 0 {
			rng["gte"] = q.Filter.SizeMin
		}
		if q.Filter.SizeMax > 0 {
			rng["lte"] = q.Filter.SizeMax
		}
		filters = append(filters, map[string]any{"range": map[string]any{"size": rng}})
	}
	if !q.Filter.ModifiedAfter.IsZero() || !q.Filter.ModifiedBefore.IsZero() {
		rng := map[string]any{}
		if !q.Filter.ModifiedAfter.IsZero() {
			rng["gte"] = q.Filter.ModifiedAfter.UTC().Format(time.RFC3339)
		}
		if !q.Filter.ModifiedBefore.IsZero() {
			rng["lte"] = q.Filter.ModifiedBefore.UTC().Format(time.RFC3339)
		}
		filters = append(filters, map[string]any{"range": map[string]any{"modified_at": rng}})
	}
	if len(q.Filter.TagIDs) > 0 {
		filters = append(filters, map[string]any{
			"terms": map[string]any{"tags": uuidStrings(q.Filter.TagIDs)},
		})
	}
	if q.Filter.FavoriteOf != nil {
		// favorite_count is per DOCUMENT, so a per-user filter cannot be answered from it. The
		// per-user star set is a metadata lookup the caller applies after this query (§7.2's
		// "per-node re-check on the results page"). What the engine can honestly do is require that
		// SOMEBODY starred it, which narrows the candidate set without claiming to know who.
		filters = append(filters, map[string]any{
			"range": map[string]any{"favorite_count": map[string]any{"gt": 0}},
		})
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}

	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"must":   []any{map[string]any{"match": map[string]any{"content": q.Text}}},
				"filter": filters,
			},
		},
		// Highlighting is OFF in v1 (§7.2): a snippet leaks neighbourhood context into a ranking
		// surface, and "earn it later" is the stated decision.
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	path := "/" + e.IndexName() + "/_search"
	status, respBody, err := e.do(ctx, http.MethodPost, path, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if status == http.StatusNotFound {
		// No index yet: a fresh cluster before the backfill has created anything. That is "no
		// documents", not a failure.
		return nil, nil
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%w: the engine answered %d", ErrUnavailable, status)
	}

	var parsed struct {
		Hits struct {
			Hits []struct {
				ID    string  `json:"_id"`
				Score float64 `json:"_score"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("%w: unreadable response: %v", ErrUnavailable, err)
	}

	out := make([]Hit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		id, perr := uuid.Parse(h.ID)
		if perr != nil {
			// A document whose id is not a UUID was not written by this product. Skipping it is
			// the safe reading: the alternative is handing a fabricated id to the metadata lookup.
			continue
		}
		out = append(out, Hit{NodeID: id, Score: h.Score})
	}
	return out, nil
}

// Index makes one document current.
func (e *ES) Index(ctx context.Context, doc Doc) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIndexRejected, err)
	}
	path := "/" + e.IndexName() + "/_doc/" + doc.NodeID.String() + "?refresh=false"
	status, respBody, err := e.do(ctx, http.MethodPut, path, body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound:
		// The index does not exist yet. Creating a mapping from the write path would mean the
		// FIRST document ever indexed decides the analyzer for every later one, so this is reported
		// as retryable and the bootstrap creates the mapping.
		return fmt.Errorf("%w: index %q does not exist yet", ErrUnavailable, e.IndexName())
	case status == http.StatusBadRequest:
		// A 400 is a mapping conflict or a malformed document: retrying it produces the same
		// answer, so the job must stop rather than burn its attempt budget.
		return fmt.Errorf("%w: the engine answered 400: %s", ErrIndexRejected,
			truncate(string(respBody), 400))
	default:
		return fmt.Errorf("%w: the engine answered %d", ErrUnavailable, status)
	}
}

// Delete removes one document. A missing document is a success.
func (e *ES) Delete(ctx context.Context, nodeID uuid.UUID) error {
	path := "/" + e.IndexName() + "/_doc/" + nodeID.String()
	status, _, err := e.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound:
		// Either the index or the document is absent; both mean "not in the index", which is what
		// the caller asked for.
		return nil
	default:
		return fmt.Errorf("%w: the engine answered %d", ErrUnavailable, status)
	}
}

// EnsureMapping creates the index with this deployment's mapping if it does not exist.
//
// Called from the backfill duty rather than at startup: a cluster that is down at boot must not
// affect the process, and an operator who turns the engine on later must not have to restart.
func (e *ES) EnsureMapping(ctx context.Context) error {
	exists, _, err := e.do(ctx, http.MethodHead, "/"+e.IndexName(), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if exists == http.StatusOK {
		return nil
	}

	// The mapping, and the reason each choice is made:
	//
	//   content      `text` with the default analyzer. §7.2 names "language analyzer" as a goal;
	//                per-language analyzers require knowing the language, which v1 does not detect,
	//                and the standard analyzer handles the common case without mis-tokenising CJK
	//                into one giant term.
	//   name / path  `text` AND a `keyword` sub-field: the text field for matching, the keyword for
	//                sorting and for the exact-value filters the listing needs.
	//   tags         `keyword`: a tag is an identifier, and analysing it would split "Q1-review".
	//   ids          `keyword`: a UUID must never be analysed.
	//   modified_at  `date`: the range filters depend on it being a real date.
	mapping := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"owner_id":       map[string]any{"type": "keyword"},
				"space_group_id": map[string]any{"type": "keyword"},
				"node_id":        map[string]any{"type": "keyword"},
				"name": map[string]any{"type": "text", "fields": map[string]any{
					"raw": map[string]any{"type": "keyword"},
				}},
				"path":           map[string]any{"type": "text"},
				"mime":           map[string]any{"type": "keyword"},
				"kind":           map[string]any{"type": "keyword"},
				"size":           map[string]any{"type": "long"},
				"modified_at":    map[string]any{"type": "date"},
				"tags":           map[string]any{"type": "keyword"},
				"favorite_count": map[string]any{"type": "integer"},
				"content":        map[string]any{"type": "text"},
			},
		},
	}
	body, err := json.Marshal(mapping)
	if err != nil {
		return err
	}
	status, respBody, err := e.do(ctx, http.MethodPut, "/"+e.IndexName(), body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return fmt.Errorf("%w: creating the index answered %d: %s", ErrUnavailable, status,
		truncate(string(respBody), 400))
}

// do performs one request against the first reachable URL.
func (e *ES) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var lastErr error
	for _, base := range e.cfg.URLs {
		u, err := url.Parse(strings.TrimSuffix(base, "/") + path)
		if err != nil {
			lastErr = err
			continue
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
		if err != nil {
			lastErr = err
			continue
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if e.cfg.Username != "" {
			req.SetBasicAuth(e.cfg.Username, e.cfg.Password)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		return resp.StatusCode, respBody, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no cluster URL is configured")
	}
	return 0, nil, lastErr
}

// uuidStrings renders ids for a terms query.
func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// truncate bounds an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
