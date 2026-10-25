# ADR-0017: search-engine posture and trust boundary

**Status:** Proposed · **Date:** 2026-10-25 · **Amends:** `competitiveness-analysis-2026-10-25.md`
§6 M6.2 · **Implements:** `core-scope-2026-10-25.md` §7.2

> **Numbering note.** `core-scope-2026-10-25.md` asks for this decision to be filed as ADR-0016. That
> number was taken between the spec being written and this change landing: `0016` is the tus ADR.
> The decision is unchanged; only the number moved, and it is recorded here rather than silently
> renumbered, because a document citing ADR-0016 for search would point at the wrong page.

## Context

Name search works and needs no infrastructure: it is a `strpos` over the node table, and migration
0024 adds a `pg_trgm` GIN index so the scan is not a scan. Searching *inside* documents is a different
class of feature — it needs a maintained index, an extractor per document type, and a service to hold
the result.

The competitiveness analysis put PostgreSQL full-text (`tsvector`) forward as the cheaper option. It
is cheaper, and it is the wrong tool for the job the product needs: a `tsvector` column indexes one
document at a time in the same table, so a two-gigabyte PDF's extracted text would live in the
metadata database, the extraction cost would land on the write path, and every language would need
its own configuration row. Elasticsearch exists for exactly this and the analysis already said so.

The interesting decision is therefore not "which engine" but **what the engine is allowed to mean**.

## Decision

### 1. The engine is optional, and the product is complete without it

`search.engine: none` is the default. A deployment without an engine has working name-and-filter
search: kind, MIME prefix, size and date bounds, tag, favorite, three sort orders and cursor
pagination.

A request for a content mode (`mode=content` or `all`) on an engine-less deployment is answered,
never failed: the response carries `mode: name`, `degraded: true` and a reason. This is the whole
posture in one field. A search that silently looked in fewer places than it was asked to would let a
user conclude their document does not exist, which is worse than being told the search did not look.

### 2. Search is best-effort telemetry, not evidence

- **No write ever fails because the index is down.** Every producer enqueues a `search.index` job in
  the same transaction as the metadata change and then returns. Publishing, trashing, restoring,
  moving, renaming and tagging all succeed whether or not the engine answers.
- **The index is a cache.** It holds nothing the metadata store does not, so losing it entirely is
  recoverable by deleting it and letting the backfill rebuild.
- **The queue, not the engine, is the health signal.** `newcloud_search_index_lag_seconds` measures
  how long the oldest pending index job has waited. An engine can only report that it answered; the
  queue reports how far behind the answers are.

This is deliberately the opposite of the audit chain's posture. The two are not in tension: an audit
record that cannot be written makes an operation unauthorised, because the operation would be
unaccountable, while an index that cannot be written makes a search incomplete, because the data is
right where it was.

### 3. The index never authorises

Three separate steps, none trusting the previous one:

1. **Scope from live grants.** The caller's subject set — their own account plus the groups they
   belong to — is read from the metadata store on every query and is what the engine's query is
   filtered by. A document the caller has no business seeing is never in the candidate set.
2. **Retrieve** candidate ids from the engine.
3. **Re-check every candidate** with the same `Authorize` call every other node route uses, and drop
   the ones it refuses.

Step 3 is why the engine is asked for more hits than the page needs. It is also why a revoked grant
takes effect immediately even when the index still holds the document: the index can be minutes
behind, the authorizer cannot.

An empty scope returns nothing rather than everything. "No filter" is the enumeration oracle this
ADR exists to rule out.

### 4. What is extracted, and what is not

v1 extracts **text, markdown and PDF**. PDF extraction is a minimal parser: it lifts the strings out
of text-showing operators in uncompressed and Flate-compressed content streams.

Not extracted, and stated rather than implied:

- **Office formats.** They need an extractor service. A deployment that believes it can search
  `.docx` and cannot is worse than one that knows the limit, so `search.content_types` **refuses an
  unsupported type at startup** instead of accepting and ignoring it.
- **Scanned PDFs.** There is no text to lift. OCR is a different product.
- **Documents above `search.index_bytes`.** They are indexed by their METADATA, so they stay findable
  by name, kind, tag and date. Their bodies are not searched.

The limitation is named on `docs/guides/limitations.md`, which is the page an operator is told to
trust over the README.

### 5. Relevance and highlighting are behind us

Highlighting is off and deep paging is out of v1. A snippet leaks neighbourhood context into a
ranking surface, and relevance tuning is only meaningful once there is a corpus to tune against.
Both are "earn it later" rather than "never".

## Consequences

Easier:

- The single binary stays the default and stays complete.
- A deployment can turn content search on or off without a migration, a re-index command or a data
  change: the backfill converges on its own.
- A stale index is a degraded *convenience*, never a disclosure: the authorisation decision is not in
  the index.

Harder:

- **Three sources of truth for "what can I see"** — the authorizer, the scope query and the per-hit
  re-check — must agree. The first two derive from the same tables, and the third is the authorizer
  itself, so the design is "one decision, applied three times" rather than three decisions. It is
  still three places to change together, and the conformance suite is what keeps them honest.
- **Extraction cost lands on the worker.** A large text file is read during a sweep, which is why the
  byte cap exists and why a read failure keeps the metadata indexed rather than failing the job.
- **The engine is a second thing to run.** An operator who wants content search runs Elasticsearch;
  the alternative — an embedded index in the database — was rejected above on cost and language
  grounds, and pretending otherwise would be a worse answer than an honest dependency.

## What is not decided here

- **Ranking.** Results are the engine's order, merged with name matches. No boosting, no recency
  weighting.
- **Multi-language analysis.** The default analyzer is used for every document, because detecting
  language reliably is a larger feature than the one it would improve.
- **Index-level tenancy.** Documents from every account share one index, separated by a `keyword`
  filter on `owner_id` and `space_group_id`. A per-tenant index would isolate better and would make
  "which index does this caller search" a routing decision the server has to get right; the filter is
  one predicate applied in one place.
