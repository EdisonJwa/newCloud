package worker

// Content-search indexing (core-scope §7.2).
//
// Three pieces, and the ordering between them is the design:
//
//  1. `search.index` JOBS are how a change reaches the index. A producer enqueues one IN THE SAME
//     TRANSACTION as the metadata change, so the queue cannot claim an index write for a change that
//     rolled back, nor lose the record of a change that committed.
//
//  2. A BACKFILL duty walks nodes by cursor, so a fresh engine — or one that was down for a week —
//     converges without a migration flag and without anybody remembering to run a command. The
//     cursor is the scrub duty's pattern: bounded and resumable.
//
//  3. A LAG GAUGE, computed from the oldest still-pending index job. This is the signal §7.2 asks
//     for, and it is measured from the QUEUE rather than from the engine: the engine can only say
//     it answered, while the queue says how far behind the answers are.
//
// What none of this does is fail a write. Every producer enqueues; none waits, and none checks the
// engine. That is the "best-effort telemetry, not evidence" rule in code.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/search"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// SearchIndexJobKind is the job kind that keeps one document current.
//
// Exported so a producer names it without a string literal, and so the job catalogue in the
// documentation can be checked against the registry rather than against prose.
const SearchIndexJobKind = "search.index"

// searchBackfillBatch bounds one backfill pass.
//
// Small enough that a pass finishes well inside the duty's timeout even when every document has to
// be read, and large enough that a store of a hundred thousand files converges in hours rather than
// weeks.
const searchBackfillBatch = 200

// registerSearchJobs wires the indexing job.
//
// A nil engine is the `search.engine: none` deployment, and the handler is STILL registered so a job
// queued by a producer is completed rather than dead-lettered. The alternative — no handler — makes
// every write queue a job that fails five times and then sits in the failed queue forever, turning
// "the feature is off" into "the queue is full of errors".
func (w *Worker) registerSearchJobs() {
	w.handlers[SearchIndexJobKind] = func(ctx context.Context, payload map[string]any) error {
		if w.search == nil {
			return nil
		}
		raw, ok := payload["node_id"].(string)
		if !ok || raw == "" {
			return Permanent(fmt.Errorf("%s: missing node_id", SearchIndexJobKind))
		}
		nodeID, err := uuid.Parse(raw)
		if err != nil {
			return Permanent(fmt.Errorf("%s: bad node_id %q: %w", SearchIndexJobKind, raw, err))
		}
		return w.indexOne(ctx, nodeID)
	}
}

// indexOne reconciles one node with the index.
//
// The decision is made from the NODE'S CURRENT STATE, not from the job's payload: the queue is
// at-least-once and a job can be delivered long after the change it describes, so two renames in a
// row must not have the first job resurrect the old name. Reading the row is what makes the handler
// idempotent, which the queue requires.
func (w *Worker) indexOne(ctx context.Context, nodeID uuid.UUID) error {
	var node store.Node
	gone := false
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.NodeIncludingTrashed(ctx, nodeID)
		if errors.Is(nerr, store.ErrNotFound) {
			// Purged for good. The index must forget it: a search result for a deleted file is a
			// dead link that tells the user nothing.
			gone = true
			return nil
		}
		if nerr != nil {
			return nerr
		}
		node = n
		gone = n.TrashedAt != nil
		return nil
	})
	if err != nil {
		return err
	}

	if gone {
		return w.searchCall(func() error { return w.search.Delete(ctx, nodeID) })
	}

	doc, ok := w.searchDoc(ctx, node)
	if !ok {
		// Nothing indexable — no owner, or a path that cannot be reconstructed. Not an error: the
		// metadata is the source of truth and the index is a convenience.
		return nil
	}
	return w.searchCall(func() error { return w.search.Index(ctx, doc) })
}

// searchCall runs one engine call and classifies its failure for the queue.
//
// The metric is recorded HERE rather than at each caller, so no future caller can forget it — the
// same reasoning as writeUploadError. A rejected document is PERMANENT: a mapping conflict produces
// the same answer five times, and retrying it hides the real problem behind a queue of doomed work.
func (w *Worker) searchCall(fn func() error) error {
	if err := fn(); err != nil {
		if w.metrics != nil {
			w.metrics.ObserveSearchIndexError()
		}
		if errors.Is(err, search.ErrIndexRejected) {
			return Permanent(err)
		}
		return err
	}
	return nil
}

// searchDoc builds the document for one node.
//
// Content is extracted only for the configured types and only below the byte cap (§7.2). A file
// whose body is not indexed still gets its METADATA indexed, so it stays findable by name, kind, tag
// and date — the metadata is the part that is always right.
func (w *Worker) searchDoc(ctx context.Context, node store.Node) (search.Doc, bool) {
	if node.OwnerID == uuid.Nil {
		return search.Doc{}, false
	}

	doc := search.Doc{
		OwnerID:      node.OwnerID,
		SpaceGroupID: node.OwnerGroupID,
		NodeID:       node.ID,
		Name:         node.Name,
		Mime:         node.Mime,
		Kind:         string(node.Kind),
		Size:         node.Size,
		ModifiedAt:   node.ModifiedAt.UTC(),
	}
	// Tags and the favorite count are read here rather than carried on the node row: they change
	// without the node changing, which is exactly why a tag change enqueues an index job.
	if tags, err := w.nodeTagNames(ctx, node); err == nil {
		doc.Tags = tags
	}
	if counter, ok := w.meta.(store.FavoriteCounter); ok {
		if n, err := counter.FavoriteCount(ctx, node.ID); err == nil {
			doc.FavoriteCount = n
		}
	}
	if body, ok := w.extract(ctx, node); ok {
		doc.Content = body
	}
	return doc, true
}

// extract reads a document's text, or reports that it should not be indexed.
func (w *Worker) extract(ctx context.Context, node store.Node) (string, bool) {
	if w.reader == nil || node.Kind != store.KindFile || node.Size <= 0 {
		return "", false
	}
	if w.indexBytes > 0 && node.Size > w.indexBytes {
		// Above the cap: metadata only. Reading a two-gigabyte PDF during a sweep is not worth a
		// hit that a filename would have found anyway.
		return "", false
	}
	if !w.contentTypeIndexable(node.Mime, node.Name) {
		return "", false
	}
	body, err := w.reader.ReadContent(ctx, node, w.indexBytes)
	if err != nil {
		// A read failure is not fatal: the document keeps its metadata in the index, and the next
		// change re-tries. Failing the job would refetch the same bytes five times.
		return "", false
	}
	return body, true
}

// contentTypeIndexable reports whether this node's text is worth extracting.
//
// Prefix match on MIME with a filename fallback, because a client-declared type is often empty or
// generic: a ".md" file uploaded as application/octet-stream IS a markdown document, and refusing to
// index it would make the feature look broken to the person who wrote it.
func (w *Worker) contentTypeIndexable(mime, name string) bool {
	for _, want := range w.contentTypes {
		switch want {
		case "text":
			if hasPrefixFold(mime, "text/") {
				return true
			}
		case "markdown":
			if hasPrefixFold(mime, "text/markdown") || hasSuffixFold(name, ".md") ||
				hasSuffixFold(name, ".markdown") {
				return true
			}
		case "pdf":
			if hasPrefixFold(mime, "application/pdf") || hasSuffixFold(name, ".pdf") {
				return true
			}
		}
	}
	return false
}

// nodeTagNames returns the tag names on a node, for the indexed document.
func (w *Worker) nodeTagNames(ctx context.Context, node store.Node) ([]string, error) {
	var out []string
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		social, ok := tx.(store.SocialTx)
		if !ok {
			return nil
		}
		// The OWNER's view, because a tag name is a property of the document rather than of who is
		// asking: the owner sees every tag it carries, so indexing that view indexes all of them.
		// The grant filter belongs on the query path, not in the document.
		tags, terr := social.NodeTags(ctx, node.OwnerID, node.ID)
		if terr != nil {
			return terr
		}
		for _, t := range tags {
			out = append(out, t.Name)
		}
		return nil
	})
	return out, err
}

// BackfillSearchIndex walks nodes by cursor and indexes them, so a fresh engine converges.
//
// The cursor lives in the worker rather than in the database: it only decides WHERE this process
// resumes, and a restart that begins again from the top costs a redundant pass over rows the engine
// already has (every write is idempotent) rather than a correctness problem. §7.2 asks for a
// "bounded, resumable cursor"; both are true of one process's walk, and a per-deployment cursor
// would be a new table for a value whose loss is harmless.
func (w *Worker) BackfillSearchIndex(ctx context.Context) (indexed int, err error) {
	if w.search == nil {
		return 0, nil
	}
	// The mapping must exist before the first document is written, or every write in the first pass
	// fails with a 404 that looks like an outage.
	if es, ok := w.search.(interface{ EnsureMapping(context.Context) error }); ok {
		if merr := es.EnsureMapping(ctx); merr != nil {
			if w.metrics != nil {
				w.metrics.ObserveSearchIndexError()
			}
			return 0, fmt.Errorf("ensure the search mapping: %w", merr)
		}
	}

	var nodes []store.Node
	err = w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.NodesAfter(ctx, w.searchCursor, searchBackfillBatch)
		nodes = list
		return lerr
	})
	if err != nil {
		return 0, err
	}
	if len(nodes) == 0 {
		// The pass reached the end, so the next one starts over from the top. That wrap is how a
		// deployment that was down for a week catches up.
		w.searchCursor = uuid.Nil
		return 0, nil
	}

	for _, node := range nodes {
		if ctx.Err() != nil {
			return indexed, nil
		}
		if node.TrashedAt != nil {
			// Trashed rows are skipped rather than deleted from the index here: the purge path's
			// own job does that, and a backfill deleting on a stale read could drop live documents.
			continue
		}
		doc, ok := w.searchDoc(ctx, node)
		if !ok {
			continue
		}
		if ierr := w.searchCall(func() error { return w.search.Index(ctx, doc) }); ierr != nil {
			// One refused document must not stop the pass: the rest of the store still needs to
			// converge, and the failure is counted rather than reported per row.
			continue
		}
		indexed++
	}
	w.searchCursor = nodes[len(nodes)-1].ID
	return indexed, nil
}

// SearchIndexLag reports how far behind the index is, measured from the queue.
//
// Seconds since the OLDEST pending index job was due. Zero when the queue is empty, which is the
// caught-up value. Published as newcloud_search_index_lag_seconds (§7.2).
func (w *Worker) SearchIndexLag(ctx context.Context) (time.Duration, error) {
	if w.search == nil {
		return 0, nil
	}
	var lag time.Duration
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		oldest, ok, gerr := tx.OldestPendingJob(ctx, SearchIndexJobKind)
		if gerr != nil || !ok {
			return gerr
		}
		if d := time.Since(oldest); d > 0 {
			lag = d
		}
		return nil
	})
	return lag, err
}

// hasPrefixFold and hasSuffixFold are case-insensitive comparisons, for the MIME classifier.
//
// They exist as named helpers rather than inline strings.ToLower calls so the classifier reads as
// the rule it is, and so a future type is added in one place.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && equalFoldASCII(s[:len(prefix)], prefix)
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && equalFoldASCII(s[len(s)-len(suffix):], suffix)
}

// equalFoldASCII folds ASCII case. MIME types and file extensions are ASCII by construction, and a
// Unicode-aware fold here would be a slower way to reach the same answer.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
