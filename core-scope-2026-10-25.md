# Core scope — functions newCloud commits to support

**Status:** Owner decision, 2026-10-25 · **Amends:** ADR-0008 (upload protocol — reversal, see §3);
`competitiveness-analysis-2026-10-25.md` §6 M6.2 (PG content search → Elasticsearch, see §5);
`feature-parity-2026-10.md` rows for tus/checksums/trash/favorites/tags/comments/search.

These seven functions are **core scope**: every deployment has them; they are not optional, not
plugins, and each ships with tests that pin its behavior. They are the minimum bar for competing
with Nextcloud/oCIS/Seafile on the *core file-service* axis (the collaboration suite remains a
non-goal per ADR-0001).

| # | Function | Current state | Effort | Wave |
| --- | --- | --- | --- | --- |
| 1 | Resumable chunked upload via **tus**, durable across restart | own protocol (ADR-0008): durable, parallel, out-of-order | M–L | 3 |
| 2 | **Checksums exposed to client** | per-version sha256 in JSON only | S | 1 |
| 3 | User-initiated **empty trash** | worker purge only | S | 1 |
| 4 | **Favorites / starred** | absent | S–M | 1 |
| 5 | **Tags / labels** | absent | M | 2 |
| 6 | **Comments / discussions** | absent | M | 2 |
| 7 | Search: **name + full-text (Elasticsearch) + filters** | filename substring, no filters/pagination | S (filters) / L (ES) | 1 / 3 |

Waves: (1) additive, no protocol reversals — checksums, empty trash, favorites, name-search
filters; (2) tags, comments (new social-metadata substrate); (3) tus swap and the ES engine —
the two decisions that reverse or extend existing ADRs, sequenced last so waves 1–2 land value
while their designs settle.

---

## 1. Resumable chunked upload via tus (supersedes ADR-0008's wire protocol)

### Decision

Adopt the **tus Resumable Upload Protocol v1.0.0** as the chunked-upload wire protocol, with the
extensions the standard client libraries expect:

- **Core**: `POST` creation semantics via the creation extension, `PATCH
  application/offset+octet-stream` append-at-offset, `HEAD` offset query, server-owned
  `Upload-Offset`, `409 Conflict` on offset mismatch.
- **creation** and **creation-with-upload** (one round trip for small files).
- **creation-defer-length** (unknown size at start; bounded later by `limits.max_file_bytes`).
- **expiration** (reuses `limits.upload_session_ttl_hours`; `Upload-Expires` returned; sweep
  already exists as the `upload-cleanup` duty).
- **termination** (`DELETE` = the current abort).
- **checksum** (`Upload-Checksum: sha256 <base64>`, `Checksum-Trailer` support; mismatch → `460`;
  sha256 only — it is the only hash the server already computes and stores).
- **concatenation** (partial uploads + `Upload-Concat: final;…`): this is how tus-js-client
  implements `parallelUploads`, and it replaces the out-of-order parallelism the current custom
  protocol offers clients.

### What is kept, what changes

The **durability machinery is protocol-independent and stays**: staging file + fsynced JSON
record, restart recovery with the five consistency rules, graceful-shutdown preservation, the
server-owns-the-acknowledged-set principle (`internal/upload/upload.go`). A tus session maps to
the existing session manager: creation → `Create`, PATCH → `Append` (contiguous append is the
tus invariant; the range set remains the internal truth), final upload → the existing
`complete` guards then `publishFile`.

**Kept as product principles, now expressed in tus terms:**

- *Refusals decided before any byte is written*: creation runs the target-directory
  authorization, fold-name conflict check, lock consultation and quota reservation; the
  finalization re-runs every guard inside the statement that changes the row (the chunked
  `complete` guard set, `internal/server/uploads.go:195-297`, moves to the PATCH that reaches
  `Upload-Length` and to the concat-final handler).
- *One publish path*: tus finalization calls the same `publishFile`; the existing invariant that
  both upload types produce the same ETag for the same bytes extends to the tus path.

**Accepted losses, stated honestly:** the bespoke protocol let a client push chunks out of order
into one session; tus core is strictly append-at-offset, and parallelism comes from **multiple
partial uploads joined by concatenation** instead. Clients that implemented our slicing loop must
move to tus (tus-js-client in the web UI; Uppy/tus clients elsewhere). The custom endpoints stay
for exactly one release, marked deprecated, then are removed.

### Endpoints

```text
OPTIONS /api/files/tus          Tus-Resumable, Tus-Version: 1.0.0, Tus-Extension, Tus-Max-Size
POST   /api/files/tus           create (Upload-Length, Upload-Metadata: filename, destination, …)
PATCH  /api/files/tus/{id}      append (Upload-Offset check; body)
HEAD   /api/files/tus/{id}      Upload-Offset, Upload-Length, Upload-Metadata, Upload-Expires
DELETE /api/files/tus/{id}      terminate
```

`Upload-Metadata` carries what the query string carries today: destination path, declared
filename, MIME hint. Values are base64 per spec; the existing name validator runs at creation.

### Acceptance criteria

- tus-js-client round trip (single, parallel-via-concat, defer-length) against a live server;
  browser check replaces the drag-in slicing check's client half.
- Restart mid-tus-upload resumes at the server-reported offset (existing durability tests re-run
  against the tus layer).
- A PATCH with a stale offset is `409` and changes nothing; a checksum mismatch is `460` and the
  acknowledged set does not include the chunk.
- Finalization honors locks, overwrite grants, fold-name conflicts, quota — refused before bytes
  when knowable at creation, re-checked in-statement at finalization.
- Deprecation: old endpoints answer with a `Deprecation`/`Sunset` header pair during the
  transition release.

### ADR action

File **ADR-0015 (Proposed → Accepted at merge)**: "Resumable uploads move to tus", superseding
ADR-0008 §"Why neither tus nor S3 multipart". ADR-0008's argument was client cost ("40 lines of
slicing loop") — valid then, reversed now by three facts: the durability machinery is
protocol-independent (proven by restart tests), the ecosystem argument flipped (tus-js-client,
Uppy, and oCIS interop are off-the-shelf, and our own browser client is the one bespoke state
machine left), and the checksum extension gives transfer verification the custom protocol never
had. ADR-0008's *durable sessions* section survives unchanged.

---

## 2. Checksums exposed to client

The sha256 already exists — the blob address *is* the content hash (`etagFor` truncates it) — so
this is surface plumbing, not new hashing.

- **WebDAV**: `PROPFIND` returns `{http://owncloud.org/2012/ns}checksums` as `SHA256:<hex>` for
  files (ownCloud/Nextcloud property, so third-party clients find it); `PUT` responses carry
  `OC-Checksum`; `GET`/`HEAD` responses carry `OC-Checksum`.
- **JSON**: `fileJSON` gains a top-level `sha256` (today only versions list it,
  `internal/server/files.go:1279-1281`).
- **Verify-on-upload**: a client-supplied `X-OC-Checksum: SHA256:<hex>` (PUT, single-request
  upload, tus creation metadata) is compared against the streamed hash **at finalization, before
  publish** — mismatch refuses the write (`422 checksum_mismatch`), never a trashed wrong file.
  tus PATCHs use the protocol's own checksum extension instead.
- **Version integrity**: the version listing already reports per-version sha256; with exposed
  checksums the UI can display them (Properties pane) — parity with NC's checksum app.

Acceptance: property/header round trip on both layouts; tree layout (hash computed on write)
returns the same value as blob layout for identical bytes; verify-on-upload refusal leaves the
previous file untouched; OpenAPI updated.

---

## 3. User-initiated "empty trash"

- `POST /api/files/trash/empty` purges **the caller's own trash roots** through the exact same
  routine the worker duty runs (per-owner `trash_bytes` accounting, blob refcount release,
  single recursive delete — `internal/store/lifecycle_pg.go:28-185`). No second purge
  implementation.
- Response: `{purged, bytes_freed}`. The handler runs the purge in bounded batches and may be
  called again; it is idempotent, so a timeout means "call me again", never "unknown state".
- Audited as `trash.purge` with actor attribution — the event vocabulary already exists; today
  only the worker emits it. The counts-style single-event pattern (§8.1 of the audit vocabulary)
  is reused, with `via=user`.
- UI: Trash view gains "Empty trash", confirmation names the byte count and the retention
  implication (purged is permanent; restore is impossible afterwards).
- Quota effect is immediate (`used_bytes`/`trash_bytes` move in the same transaction).

Acceptance: concurrent purge + delete cannot double-release refcounts (the guarded-delete
semantics already enforce this — pin it with a user-path test); non-owner sees `404`;
`content_missing`-style accounting unchanged.

---

## 4. Favorites / starred

- Schema: `favorites(user_id, node_id, created_at, PK(user_id, node_id))` — a per-user row, not a
  node flag, so members of a shared folder star independently. Index `(user_id, created_at DESC)`
  drives the listing.
- API: `PUT`/`DELETE /api/files/favorite?path|id` (id-addressed, so a favorite survives rename
  and move — same node-identity philosophy as share links); `GET /api/favorites?cursor` follows
  the ADR-0012 cursor contract, newest first.
- WebDAV: `oc:favorite` (0/1) as a live property — `PROPPATCH` sets/unsets, `PROPFIND` returns it.
  Windows/macOS DAV clients ignore it harmlessly; NC-ecosystem clients use it natively.
- Authorization: starring requires `read` on the node; the favorites listing re-checks each
  node's grant (revocation removes it from the list, never leaks).
- Trash interaction: favorites on trashed nodes are hidden from the listing (join against live
  nodes); the purge path removes their rows (add to the purge SQL, both backends + conformance
  case).
- Not audited: a favorite is not a security-relevant action (see §8 audit policy).

Acceptance: two users star the same shared node independently; move/rename keeps the favorite;
revoked access drops it from listing; conformance suite covers both backends.

---

## 5. Tags / labels

- Schema:
  `tags(id, owner_id NULL, name, color, created_at)` — `owner_id IS NULL` means a **system tag**
  (admin-managed); user tags are per-owner. `UNIQUE (COALESCE(owner_id, …), lower(name))` —
  case-fold uniqueness, the lesson the name-key collision fixes already taught this codebase.
  `node_tags(node_id, tag_id, PK(node_id, tag_id), tagged_by, tagged_at)`.
- API: `GET/POST /api/tags`, `PATCH/DELETE /api/tags/{id}` (own tags; system tags require
  admin); `POST/DELETE /api/nodes/{id}/tags` assigns/removes; assignment requires `write` on the
  node (tags are metadata writes), reading tags requires `read`.
- WebDAV: read-only `oc:tags` property on PROPFIND (assignment stays JSON — PROPPATCH on
  oc:tags is `403` with a propstat note, matching how DAV reserved props are refused today).
- Search integration: `tag` becomes a filter (§7), which makes tags the substrate for any future
  retention/workflow automation — that is *why* tags are core, not decoration.
- System-tag management is an admin action and is audited (existing admin vocabulary);
  assignment is not (§8).
- Caps: name ≤ 64 runes, color from a fixed palette (validated), ≤ 50 tags per node, ≤ 200 user
  tags per account.

Acceptance: case-fold collision refused; assignment on a node the caller cannot write is `404`;
tag filter in search returns exactly the tagged nodes the caller can read; deleting a tag
removes assignments in one transaction.

---

## 6. Comments / discussions

- Schema:
  `comments(id, node_id, author_id, parent_id NULL, body, created_at, edited_at, deleted_at,
  deleted_by)` — soft delete keeps the thread readable ("deleted" tombstone) and keeps the audit
  of *that a deletion happened* out of the hash chain (§8) while preserving moderation facts.
- API (all under the ADR-0012 cursor contract):
  `GET /api/nodes/{id}/comments`, `POST` (`{body, parent_id?}`), `PATCH /api/comments/{id}` (own,
  sets `edited_at`), `DELETE /api/comments/{id}` (own comment, admin, or a holder of
  `manage_permissions` on the node).
- Permission to comment = `read` on the node (you can discuss what you can see); threads are
  single-level replies (`parent_id` must reference a top-level comment) — v1 keeps the data
  model forward-compatible without building a forum.
- **Content policy**: plain text only in v1 — no markdown, no HTML; rendered client-side as
  text. A comment is user content, so it is stored like content (never rendered server-side,
  never inline on the application origin). Body cap 2 000 runes; the same segment-validator
  philosophy applies (no control characters).
- **Rate limit**: N comments/hour/user via the existing durable rolling-window store
  (`internal/store/share_rate.go` pattern) — the anonymous-abuse lessons from the share OTP
  endpoint apply to any user-writable append surface.
- **Actors**: authenticated users only in v1. Anonymous share-visitor comments are deliberately
  out (an append-only surface keyed to nothing but a link token is a spam and abuse liability;
  revisit with guest accounts, which are separately scoped).
- Notifications hook: comment-on-your-file / reply-to-your-comment is the second consumer of the
  notification+mail work (competitiveness M4) — the schema lands before the notifier.

Acceptance: rate-limited posting; tombstone rendering; `manage_permissions` can moderate;
comments never appear for nodes the caller cannot read (id-addressed route, same ACL check as
`/api/nodes/{id}`); body cap enforced server-side.

---

## 7. Search: name + full-text (Elasticsearch) + filters

Two layers, one API — and one architecture rule: **search is best-effort telemetry, not
evidence**. An unavailable engine degrades to name search; a write never fails because the index
is down. This is the deliberate opposite of the audit chain's fail-closed posture, and it is
stated here so nobody "fixes" it later.

### 7.1 Filters + name search (engine-independent, Wave 1)

- `GET /api/files/search` gains: `kind` (file|dir), `mime` prefix, `size_min/size_max`,
  `modified_after/modified_before`, `tag`, `favorite`, plus `limit`/`cursor` pagination under the
  ADR-0012 contract (today: single unbounded page).
- Name matching moves from `strpos` seq-scan to a `pg_trgm` GIN index (the extension the
  limitations page already names as the future step); the in-memory backend keeps the linear
  scan and the conformance suite holds both to the same answers.
- Sort options: `modified|name|size`; results carry the same markers as listings
  (`share_count`, `has_versions`, `starred`, `tags`).

### 7.2 Elasticsearch content search (Wave 3)

- **Engine choice**: Elasticsearch per the core-scope decision (2026-10-25). The earlier
  competitiveness analysis argued for PostgreSQL tsvector to preserve the one-binary story; the
  owner decision takes ES for its analyzers, faceting/scoring, scale path, and ops familiarity
  (it is what Nextcloud deployments already run). The single-binary principle is preserved by
  making the engine **optional**: `search.engine: none|elasticsearch` (default `none` — a
  deployment without ES still has working name+filter search; content search reports
  `mode: name` honestly in the response rather than pretending).

- Config: `search.elasticsearch.{urls, username, password_file, index_prefix, insecure_skip_verify}`,
  `search.content_types` (v1: `text, markdown, pdf` — office extraction is a separate decision
  with an extractor service, not silently worse-than-promised), `limits.search_index_bytes`
  (skip bodies larger than N, still index metadata), all validated at startup like `mail`.

- **Index pipeline**: 
  - Mapping per node: `owner_id`, `node_id`, `space_group_id`, `name`, `path`, `mime`, `kind`,
    `size`, `modified_at`, `tags[]`, `favorite_count`, `content` (text, language analyzer).
  - Incremental: enqueue `search.index` jobs (the job registry already exists — a new job kind,
    leases and backoff included) from the publish/trash/purge/move/rename/tag paths. The enqueue
    is the same transaction as the metadata change; the indexing itself is async.
  - Backfill: a worker duty walks nodes by cursor (the scrub duty's cursor pattern), so a fresh
    ES cluster converges without a migration flag.
  - Deletions: trash/purge emit index deletes; a `search_index_lag_seconds` gauge and
    `search_index_errors_total` counter feed two new alert rules (lag, engine down), joining the
    existing 21.

- **Authorization — the non-negotiable**: the index is a cache of content the server can already
  read; the query path must never become a enumeration oracle. Query-time filter:
  `owner_id IN (self + space groups + ACL-grant subjects)` computed from live grants, then
  per-node re-check on the results page (same check as every read route). A revoked grant stops
  returning hits immediately, even if the index still holds the document. Trashed nodes are
  filtered by join against live metadata (the index never authorizes, it only retrieves).

- **API**: `mode: name|content|all` on the search endpoint; content mode requires the engine and
  otherwise falls back with `mode: name` + a `degraded: true` field. Highlighting off in v1
  (snippets leak neighborhood context into ranking surfaces; earn it later).

Acceptance: phrase-in-body finds the document (text + PDF); file renamed/moved re-indexes; tag
and favorite filters compose with content mode; authorization test — a revoked grant's documents
never appear in results even when directly indexed; engine-down degradation is observable
(`degraded: true`, metric, alert); backfill converges with a bounded, resumable cursor.

---

## 8. Audit policy for social metadata (favorites, tags, comments)

The hash-chained audit log records *who did what to whose files* — the fields that decide
accountability. Favorites, tag assignment, and comment creation are not in that class, and
putting them in the chain would dilute it (volume without evidentiary value, and a comment body
is user content, not a security fact). Policy, stated once:

- **Not hash-chained**: favorite set/unset, tag assignment, comment create/edit.
- **Hash-chained**: comment **deletion by someone other than the author** (moderation is a
  privileged action — `comment.moderate` with actor, node, comment id; no body), system-tag
  management (admin action, existing vocabulary).
- Comment bodies live only in the `comments` table with soft-delete tombstones; they are never
  written into audit metadata.

---

## 9. Sequencing and dependencies

```text
Wave 1 (additive, no ADR reversals)
  checksums ──────────┐ (checksum surface + verify-on-upload)
  empty trash         │
  favorites ──────────┼─→ search filters need favorite/tag columns to filter on
  search filters/name ┘
Wave 2 (social substrate)
  tags ──→ comments (thread UI shares the detail-pane slot)
Wave 3 (protocol + engine — ADR reversals land here)
  tus (needs #2: checksum extension) ──→ web client swap, old protocol deprecation release
  Elasticsearch (needs tags/favorites for filters; needs Wave-1 filter API)
```

Standing rule, unchanged: every wave keeps `go test ./...`, both conformance backends, the
doc-claim tests, and `newcloud drill` green. New ADRs: ADR-0015 (tus, §1) and ADR-0016 (search
engine posture: optional ES, best-effort indexing, query-time authorization) are filed as
Proposed with the first wave-3 commits; §8's audit policy is appended to ADR-0010's audit
vocabulary documentation.
