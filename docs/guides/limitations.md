# Limitations

This page lists what newCloud does not do. It separates deliberate non-goals from features that are
missing, and verified behavior from behavior that has only been tested in a simulated environment.

If a claim here conflicts with the README, this page wins: it carries the dates and the evidence.

## Deliberate non-goals

These are product decisions, not backlog items.

- **No app ecosystem.** There is no plugin API, app store, calendar, chat, or document editing.
  The scope is file storage, sharing, and audit.
- **No Nextcloud client compatibility mode.** The WebDAV endpoint follows RFC 4918; it does not
  emulate Nextcloud's private sync protocol. Desktop clients that speak standard WebDAV work.
- **No end-to-end encryption.** Server-side previews, deduplication, thumbnail generation, and
  sharing are incompatible with a server that cannot read content. Encryption at rest is delegated
  to the disk, volume, or object store.
- **No application-managed encryption keys.** There is no KMS integration or key-rotation surface
  for content. S3 server-side encryption (SSE) can be enabled by the bucket policy.
- **No online Office, real-time collaboration, or AI file analysis.**
- **No multi-region active-active.** The database is the source of truth, and there is one writer.
- **No comments, activity feed, or notification system.**
- **No full-text content search.** Search matches file names only, across the account's tree. An
  index maintained on write would be a larger feature, and a search that silently misses files is
  worse than an honest name search.
- **No import from Nextcloud or other file services.** Migration tooling between `blob` and `tree`
  layouts is designed, not implemented.
- **Account export covers files and metadata, not the whole account.** `newcloud export --user <name>
  --out <dir>` writes the account's tree with real file names, a standalone JSON manifest, and a
  SHA-256 per file verified before it reports success — so the output is readable without this
  database. It is deliberately *not* a full backup: versions, trash, ACL grants and share links are
  not included, and there is no import path back into the system (see the import and migration row
  under current gaps).

## Current gaps

These are capabilities the requirements call for that are not implemented today.

| Area | Current state | Missing |
| --- | --- | --- |
| **Group spaces** | Resource ACLs grant users and groups access to a folder. Groups can be granted `read`, `create`, `overwrite`, `rename`, `delete`, `manage_permissions`, and `download` independently. | Team-owned spaces: a folder whose owner is a group, with quota and lifecycle independent of any one account. |
| **WebDAV sync** | `REPORT` is answered with an explicit `501`. Locking is class 2, exclusive only; a `LOCK` requesting `<shared>` is refused with `403` and a `supportedlock` body naming the scope that is supported. | `REPORT` / `sync-collection`, shared locks, and the incremental-sync protocol desktop clients use for large trees. |
| **Additional share modes** | Link, password, expiry, download cap, email recipient, drop box. | The remaining P1/P2 sharing modes in the requirements document. |
| **IdP events** | With `auth.revalidate_minutes` set, a session or App Password past the window is refused and must be renewed interactively, so an IdP-side change takes effect within that bound. Also applies to WebDAV App Passwords. | No event source or polling, so there is no revocation *at the moment of* the change; the window is the bound, and `0` disables it. SCIM would close this properly. |
| **SCIM** | A `scim_token` configuration key exists. | No SCIM endpoint. Provisioning is OIDC JIT or administrator action. |
| **IdP metadata cache** | Startup does not depend on the IdP. Existing sessions, App Passwords, and WebDAV continue when it is offline. | A cached discovery document. Each retry is a network round trip. |
| **Preview isolation** | Decoding runs in a child process by default (`limits.thumbnail_isolation: process`): the deadline kills the decoder instead of abandoning a goroutine, the child gets a minimal environment, its input and output are size-capped, and a child that hangs is killed. Pixel, concurrency and cooldown limits still apply. | A real OS sandbox (seccomp, namespace, cgroup memory limit). Isolation is process-level: a decoder bug can still consume memory until the kernel intervenes, and `inline` restores the old behaviour deliberately. |
| **Resumable uploads across restarts** | Durable for both restart paths. Each session writes a JSON record beside its staging file, fsynced after the bytes it describes; startup recovers live sessions and resumes at the last acknowledged offset, truncating any bytes the record does not acknowledge. A **graceful** stop (`Manager.Shutdown`) fsyncs, records and releases the handles but preserves the staging bytes and record, so a deploy or upgrade resumes exactly as a crash does; only an explicit abort or the TTL sweep deletes. Every append seeks to the acknowledged offset first, and every failure path rolls back by truncating **and** seeking, so a chunk retried after a mid-stream connection reset cannot leave a zero-filled hole. Covered by `TestASessionResumesAfterARestart`, `TestShutdownPreservesUnfinishedUploads`, `TestARetriedChunkAfterAMidStreamFailureLeavesNoHole` and `TestAnAppendRepositionsTheHandleToTheAcknowledgedOffset`. | Nothing at the protocol level: the client must still re-request and honour the server's offset. Cross-node resume is not available (sessions are local to one staging directory). A session whose staging file cannot be rolled back is invalidated rather than offered for resume. |
| **COPY atomicity** | A failed COPY always leaves the source intact. A destination that did not exist is rolled back by trashing **only the nodes that COPY itself committed**, and only while they are still at the parent, name and content fingerprint the COPY left them at, in reverse creation order inside one metadata transaction; a directory is trashed only when it has no live children. A concurrent PUT (which changes the file's ETag/mtime) or MOVE (which changes its parent/name, or an ancestor's) is therefore left untouched, as is a concurrent MKCOL/COPY that won the destination name (`R8-DAV-001` and its follow-ups, pinned by `TestAFailedCopyDoesNotTrashAConcurrentMkcol`, `TestAFailedCopyDoesNotUnpublishAConcurrentWritersFile`, `TestAFailedCopyDoesNotUndoAConcurrentPut` and `TestAFailedCopyDoesNotUndoAConcurrentMove` on both layouts). A node carrying dead properties (a successful PROPPATCH) is also skipped by an interim guard (`TestAFailedCopyDoesNotUndoAConcurrentProppatch`); this is not a general revision contract, and ACL/share/lock side tables are not covered. A COPY over an **existing** destination is staged under a reserved sibling name and swapped in only after the whole copy succeeds, so a failed overwrite leaves the original bytes readable at their original path; on success the displaced original goes to the trash with its own node identity, versions and bytes intact. The namespace switch — original aside, staged into place, original to the trash, both audit rows — is **one metadata transaction**, so a completed swap can never be missing from the log. Both layouts are tested: `internal/dav/treelayout_test.go` runs every overwrite scenario against the real `tree.Store` as well as the blob layout. A COPY is never truncated by the listing page size: the source subtree is resolved to completion before any destination exists, and a tree too large to prove complete is refused with `413` rather than partially copied (`internal/dav/copycomplete_test.go`). | **Best-effort recoverable replacement, not atomic across a process kill.** For the `tree` layout the bytes follow paths, so the physical moves (original aside, staged into place) happen before the metadata transaction; a kill between the physical moves and that transaction can leave the destination name resolving to the new bytes while the metadata still names the old node. The original's bytes are always preserved under `<random>.newcloud-replaced-<name>`, so the state is recoverable by hand — see the recovery note below — but it is not automatic and there is no journal. A concurrent reader can also observe the destination name briefly absent during a same-layout swap on the tree backend. For the `blob` layout the swap is metadata-only and has no such window. The non-overwrite rollback also cannot erase a child another request adds under a partially-built copy between the rollback's child check and its trash; the transaction plus "skip a directory with live children" is the ownership rule, not a distributed lock. **A Tree COPY can still overwrite bytes another request committed at the same physical path before the COPY's metadata conflict is detected** (file-copy `Content.Put` targets the final path); choosing a private-staging/no-replace-publish protocol, or a documented single-instance lock, is an open architectural decision, not covered by the rollback rule. **The rollback snapshot does not see ACL, share or lock changes**; those side tables need the same operation-visibility/revision design before the rollback can be called complete. |
| **Directory creation vs metadata** | A directory is created in the content store before its metadata node, so a storage failure cannot produce a live node (or a success audit row) for a directory that does not exist. WebDAV MKCOL and COPY, REST mkdir and the JSON batch COPY share `internal/dircommit`. If the metadata transaction then fails, the physical path is **retained unchanged** rather than deleted. That is deliberate: the seventh review reproduced a file created after an emptiness check and a concurrent winner's directory both being removed by compensating deletes (P0-R7-001/002). After the failure the path may be unreferenced, or a concurrent request may have adopted the same name and made it live; the create path cannot prove which, so it does not assume either. Covered on both layouts by `internal/dircommit/dircommit_test.go`, `internal/dav/dircommit_test.go` and `internal/server/dircommit_test.go`, including the physical failure, the audit-store failure, a concurrent winner, a writer's file under the retained path, and the retry after recovery. | Deleting the retained path would need ownership proof that survives the whole metadata window; no observation taken by the create path can provide that, and the tree store's `Delete` is recursive. A failed metadata transaction therefore can leave a directory that nothing names, until a reconciliation pass or an operator removes it under a lock that also covers name creation. That reconciler does not exist yet, and such a path must not be assumed empty or unreferenced: a concurrent writer may have added content during the window. The alternative — a best-effort recursive delete — can destroy another request's bytes and is not used. |
| **MOVE atomicity** | A failed MOVE over an existing destination leaves **both** files exactly as they were: the source back at its own name and parent with its bytes and its node identity (versions, shares, permissions) intact, and the destination untouched. That is not a convention but the swap API's type: a MOVE's stage is the user's own source node relocated to a reserved sibling name, so it is declared `swap.Borrowed` and a rollback returns it instead of deleting it, and a stage cannot be declared at all without choosing disposable-vs-borrowed. Both entry points — the JSON `POST /api/files/batch` and WebDAV `MOVE` — now run the same `swap` core, including a successful overwrite keeping the displaced destination recoverable through the trash (its reserved sibling name is not itself addressable; the restore route returns it under its own name). Every refusal is decided before anything is modified: a MOVE whose destination **is** its own source is `403` (RFC 4918 §9.9.4), and a collection moved into its own subtree is `409`, in both cases with the source, the destination, the trash and every node id unchanged. Covered end to end on both layouts for the triggers the 2026-10-23 reviews reproduced: a trash cap too small to hold the displaced destination (`507 trash_full`), an audit row that cannot be written (`503 audit_unavailable`), a self-MOVE, and a cycle. | The same best-effort boundary as COPY: on the `tree` layout the physical moves happen before the metadata transaction and there is no journal, so a kill mid-swap can leave the reserved names populated. A compensating move that itself fails leaves the source under `<random>.newcloud-staged-<name>` with its bytes and identity intact — loud, findable, never deleted — and an operator must put it back by hand. The swap is one metadata transaction, not distributed atomicity between the filesystem and PostgreSQL. |
| **Restoring a node whose name was reused** | Restoring returns a node that is reachable: the name index is rebound, so `GET` on the path its metadata names reads its bytes. If a **live** sibling holds the name now, the restore is refused with `409 conflict` instead of succeeding into invisibility — the same outcome PostgreSQL produces through its partial unique index, now pinned for both backends by `RestoreRebindsTheNameAfterTheNameWasTakenAndFreed` and `RestoreOntoALiveSiblingIsRefused` in the shared conformance suite. | A restore does not pick a new name for you. A user whose old name is taken must rename the restored item, which on the `tree` layout is an operation that moves bytes (the API does; a bare metadata rename does not). |
| **Recovering an interrupted overwrite (`tree` layout)** | The reserved names make the state identifiable: `*.newcloud-staged-*` holds the replacement that was being installed, `*.newcloud-replaced-*` holds the displaced original. If a swap was interrupted, list the destination's parent and act on what is there: with both present and the destination name missing, rename `*.newcloud-replaced-*` back to the destination name (and delete the staged tree) to restore the previous state; with a staged tree present and a destination present, the swap did not begin and the staged tree can simply be deleted. | Requires a filesystem and metadata inspection by an operator; there is no automatic reconciliation pass at startup. **After an interrupted MOVE the `*.newcloud-staged-*` entry is the source itself, not a copy**: rename it back to its original name, and never delete it — its bytes are the only copy of that file. **Renaming metadata does not move bytes on this layout**: a node recovered by hand in the metadata store must have its path changed through the API (or with `mv`), or its bytes stay at the old path. |
| **Swap artefact names** | The names a swap gives its staged and displaced siblings are **unaddressable**: `GET`, `PUT` and `PROPFIND` on `/<rand>.newcloud-staged-<name>` answer `404`, not `403`, so the refusal cannot be used to probe whether an overwrite is in progress. A displaced file is still recoverable — through the trash and the restore route, which hands it back under its own name and moves the bytes with it. | The `.newcloud-staged-` and `.newcloud-replaced-` infixes are reserved: a user file whose name contains one cannot be addressed over WebDAV. That is the price of keeping swap by-products out of the namespace, and it is why the infix is documented as reserved. |
| **Performance evidence** | `internal/store/bench_test.go` measures the three metadata operations a sync client pays for on every request — one listing page, path resolution, and create — with a fixture size set by `BENCH_CHILDREN`, and [benchmarks.md](../reference/benchmarks.md) records a baseline and what it means. Listing is the one that scales with directory size rather than page size. | Upload throughput, a concurrent sync walk, idle memory and PostgreSQL figures are not measured; the baseline is the in-memory backend, which removes I/O on purpose and is therefore a lower bound on application cost rather than a production number. |
| **Audit chain coverage** | Every field that decides who did what is hashed, including `request_id` and `auth_method`: the preimage is versioned, new rows are sealed under version 2 (which appends both), and the verifier uses each row's recorded version. Rows written before the change keep verifying under version 1, so an upgrade does not report history as tampered with. Rewriting `auth_method` in the database now breaks verification, which is what makes it evidence rather than decoration. | The version is per row, so a deployment cannot later "upgrade" old hashes in place — it must not want to, since re-hashing history is the operation the chain exists to prevent. |
| **Cursor pagination** | A row is never returned twice. Renaming an unseen row to before the cursor can hide it until the next listing. | Snapshot isolation across pages. Sorting is byte order, not client locale. |
| **Audit slowness** | Audit failure is fail-closed, and each append is bounded by a 5s deadline, so a wedged audit store refuses writes instead of stalling them. Disk-full behavior was injected, not exercised on a real full disk. | An intermittent-but-not-wedged audit store still costs every write the full timeout before refusing it; there is no circuit breaker. |
| **Audit replay** | Refused operations are never accepted, so there is nothing to replay. | No outbox or replay queue, by design. |
| **Audit retention** | `newcloud audit prune` archives then removes events older than the window, recording a signed `prune_marks` boundary so the surviving chain still verifies end to end. `audit.retention_days` is read by it. | Nothing schedules it: an operator must run it (cron, systemd timer). PG-backed prune tests skip without `TEST_DATABASE_URL`. |
| **Chunk size** | `limits.upload_chunk_bytes` is enforced: a larger chunk is refused with `413 chunk_too_large`, and the limit is runtime-overridable. | — |
| **Audit anchors** | Daily checkpoints detect truncation and mid-chain rewrites. | The code cannot enforce that `anchor_dir` uses different credentials from the database. That is an operator obligation. |
| **Application role bootstrap** | `newcloud_app` can `SELECT`/`INSERT` the audit table and cannot `UPDATE`/`DELETE`/`TRUNCATE` it. | The role password is set manually after migration. A migration cannot contain it safely. |
| **Account deletion** | Deletion moves files to trash, revokes shares and credentials, releases locks, keeps audit events, and leaves an irreversible tombstone. | Restoring a deleted account: an administrator must recover files and create a new account. |
| **`tree` layout** | Filesystem mirror, inspectable with `ls`, `rsync`-friendly. | No S3 backend, no deduplication, Windows reserved names apply, and `blobs.size` cannot be backfilled because the layout is not content-addressed. **Version history is recorded but not retained**: the layout overwrites the bytes at the path instead of copying the superseded version, so `GET /api/files/version` and `POST /api/files/restore-version` answer `409 no_version_content` rather than reporting a success they cannot perform, and the version list reports `version_content_available: false` so the UI does not offer a Restore action that cannot work. Treat the rows as a record of changes, not as recoverable content. |
| **Break-glass** | Login endpoint, CIDR restriction, auditing, alerts, and hash generator exist. | No web UI (deliberate) and no terminal no-echo prompt; the password is read from stdin. |
| **Grafana** | Prometheus endpoint, 20 alert rules, runbook. | No bundled dashboard JSON. Dashboards depend on the Grafana version and go stale. |
| **Job types** | The queue has leases, retries, backoff, and three registered job kinds. | More business job kinds. The infrastructure is complete; the catalog is small. |
| **Metrics thresholds** | Alerts cover process, errors, database, audit, GC, jobs, uploads, DAV, break-glass, WAL, and blob integrity. | Thresholds are starting values, not measured conclusions. Revisit after two weeks of traffic. |

## Verification limits

These are implemented, but the evidence has a boundary. Do not describe the boundary as covered.

### Object storage

The S3 backend is tested against an in-process fake that implements the S3 semantics the code uses.
That covers the backend's own logic: conditional writes, Range reads, multipart uploads over 16 MiB,
deletes, deduplication, owner isolation, and injected failures.

It does **not** prove interoperability with AWS S3, MinIO, Ceph, Cloudflare R2, Backblaze B2, or
Wasabi. Set `TEST_S3_ENDPOINT` to run the same suite against a real endpoint.

An upstream race in minio-go v7.4.1 (`hookReader` is read and written concurrently when a part is
retried) is reachable in production for objects over the 16 MiB part threshold when a part is
retried. It cannot be fixed from this side. The local hash is computed before the upload, so it
does not detect transport corruption after that point; a corrupted object can land at an address
that does not match its bytes. The integrity scrub and the restore drill are what detect it. This
is documented in the runbook rather than hidden.

### Restore timing

Restore measurements cover metadata dumps of 130 KiB, 1.2 MiB, and 5.5 MiB. Cost is linear within
that range, at roughly 6.9–9.4 seconds per MiB.

A linear extrapolation to 40 TiB gives about 92,000 hours. That number is the finding: the
extrapolation is unusable, and no RTO for a 40 TiB deployment has been validated. A real figure
requires a drill on equivalent hardware at the target size.

PITR has been exercised end to end: base backup, delete a row, replay to before the delete, verify
the row returned and the instance promoted. It has not been exercised against a real disk failure,
a power loss, or a full WAL archive disk.

### Client interoperability

WebDAV is exercised with protocol-level tests and a headless browser. It has not been validated
against Windows Explorer's mapped-drive client or macOS Finder on real machines. Windows reserved
names and trailing-dot collisions are handled server-side, but a real client round trip is a
separate test.

### Browser behavior

Two production defects only appeared in a real browser: an infinite reload loop on the home page
and a share password form that submitted without effect. Both are now covered by browser-driven
checks. The general lesson is recorded in
[Browser-only failures](../reference/browser-only-failures.md): a handler test does not prove that
a browser can use the page.

### Scale and concurrency

There is no sustained concurrency benchmark. Concurrency tests construct the two backup/restore
orderings deterministically rather than racing a real backup against real writes. The job queue
and the cursor contract are tested for correctness, not throughput.

## Operational constraints

These are not bugs, but deploying without accounting for them is a mistake.

- **One mount point for `/var/lib/newcloud`.** `blobs/` and `blobs/tmp/` must share a filesystem
  or publishing a blob degrades to a full copy. Startup refuses to run if the rename probe fails.
- **Two origins.** `public_url` and `usercontent_url` must differ. The validator rejects a
  same-origin deployment.
- **The origin boundary needs `server.usercontent_addr`.** Setting it gives the content origin its
  own listener and makes the *anonymous* half of the separation structural: the application listener
  refuses `/content/*` outright and answers the two share content routes with a redirect to the
  content origin, while the content listener serves exactly those three routes and answers 404 to
  everything else regardless of the `Host` header. Startup warns when it is unset, because then the
  boundary is entirely the reverse proxy's job. `deploy/Caddyfile` refuses `/content/*` on the
  application vhost, proxies the content vhost to the content port, and strips the upstream `Cookie`
  header with `header_up -Cookie` (`header { -Cookie }` manipulates RESPONSE headers and does
  nothing for the backend — a mistake this file previously made).
- **What the split does not do.** Authenticated routes still serve a signed-in user their own bytes
  from the application origin: `/api/files/download`, `/api/files/version`, `/api/files/thumbnail`,
  `/api/files/zip`, `/api/shared/{id}/content`, and WebDAV `GET`. They cannot be moved without
  giving the content origin the ability to authenticate a session, which is the isolation the split
  exists to provide.
  `/api/files/download?inline=1` is the only route that can render bytes inline on that origin, and
  it is a vetted exception: opt-in per request, a type allowlist with no SVG, magic-byte sniffing
  that forces an attachment when the bytes disagree with the declared type, `text/plain` for text
  previews, plus `nosniff` and a `sandbox` CSP. `internal/server/contentinvariant_test.go`
  classifies every route that can emit bytes and fails if a new one appears unclassified.
  `/api/files/zip` streams many nodes at once; every member is authorized before the archive headers
  are written, because a refusal discovered mid-stream could only produce a truncated archive rather
  than an honest error.
- **Share downloads on a split deployment** are handed to the content origin with a short-lived
  (30 minute) capability minted only after the password/recipient gate passes on the application
  origin, because those grants are host-only cookies the content host never receives. The
  capability names one share, one file and one purpose, is inert on any other share or file, and is
  re-validated against the share's lifecycle on every request — so revocation, expiry, exhaustion
  and owner deactivation take effect immediately rather than when the capability expires. |
- **Explicit storage layout.** `blob` is the default, but the choice changes the shape of the data
  on disk and cannot be mixed per user.
- **Different database credentials for anchors.** The application cannot verify its own anchor if
  the anchor lives where the database credentials can rewrite it.
- **Manual application-role password.** The first boot sequence includes an `ALTER ROLE`; a
  deployment that skips it starts with a role that cannot authenticate.
- **Retention defaults to "keep forever".** `trash_retention_days: 0` and
  `version_retention_days: 0` are safe defaults, not a policy. Startup warns.
- **The integrity scrub is opt-in.** `blob_scrub_per_pass: 0` disables it. Without it, corruption
  is found by a user or by a drill.
- **Email shares need SMTP.** A half-configured relay is rejected at startup; invitations are not
  accepted and silently dropped.

## Closed since the 2026-10-24 review

The [full review](../reference/review-2026-10-24.md) found defects in this tree. These were fixed with
tests that fail without the fix; the row says what changed, not just that something did.

| Was | Now |
| --- | --- |
| A stale `If-Match` on the `tree` layout answered `412` *after* the content store had replaced the bytes, so a refused write destroyed the file. The precondition was evaluated in the handler after `Put`. | The conditional is evaluated before a byte is written, and re-evaluated inside the `UPDATE` (`UpdateFile.Expect`), so a writer that commits in between fails with `412` and the old bytes are untouched. Chunked bodies are capped by `http.MaxBytesReader`, so `Transfer-Encoding: chunked` can no longer exceed `limits.max_file_bytes`. `If-Match` on a missing resource, a matching `If-None-Match` and a weak `If-Match` are all refused. |
| The restore drill compared `LIVE_DSN` and `SCRATCH_DSN` as strings, then ran `DROP SCHEMA … CASCADE`. Two spellings of one database (for example with `?sslmode=disable`) passed the check. | The guard asks the server: it compares `current_database()`, `inet_server_addr()` and `inet_server_port()` for both connections and refuses when they match, whichever DSN was used. The `pg_dump`/`pg_restore` connection string reaches the tools through libpq's environment, never through `argv`, and credentials are redacted from tool error output. |
| `newcloud_audit_chain_head_age_seconds` was written with a hard-coded `0`, once a day, only when anchoring was configured, so the critical "audit chain stalled" rule could never fire — and the scrub alert depended on a counter that stayed zero. | The gauges are published on every anchor tick regardless of anchoring, from the newest event's own timestamp. The stalled rule now also requires recent write traffic, so a quiet deployment is not paged. |
| `THUMBNAIL` served a 320-pixel rendition to a grantee holding `read` without `download`, while every other content route refused that grant. | The preview requires `download`, like the content paths beside it. The pinned test was corrected rather than the comment. |
| PostgreSQL could grant two exclusive locks on one node (`INSERT … NOT EXISTS` cannot see an uncommitted row). | `lock_tokens` has a unique index on `node_id` (migration `0017`) and the insert is `ON CONFLICT DO NOTHING`; an expired row is cleared first so it does not block. The shared conformance suite now races eight locks on one node and requires exactly one grant on **both** backends. |
| `Range: bytes=0-` counted as a continuation, so an exhausted share could still serve the complete file for the session's TTL. | A whole-entity range is a fresh download and is charged; seeks and resumes (non-zero start, bounded window, short suffix) still ride the paid session. |
| A create-only link listed the directory's names, sizes and ETags even though download refused it. | A share that cannot read does not list. |
| Share tokens were written to the INFO access log and into audit metadata for refusals. | Both are redacted (`/s/***/download`); the audit log keeps the share id, which is not a credential. |
| The content-ticket path served bytes with no audit row. | It writes `file.read` with `via=content_ticket`, attributed to the ticket's subject. |
| A name that collided by case or Unicode folding silently overwrote the differently-named file and then 404'd on the name the client asked for. | It is `ErrConflict`, as ADR-0004 §4 always said. |
| Retention expiry set `trashed_at` without moving the bytes, so the quota was never released and the later purge subtracted from a counter that was never credited. | Expiry performs the same accounting as `Trash` on both backends, and the purge no longer double-releases. |
| The trash cap was compared and then charged in a second statement, so concurrent deletes overshot it. | The cap is the `WHERE` clause of the statement that charges; racing deletes cannot exceed it. |
| The reference Caddyfile proxied everything to the application, publishing `/metrics` and `/readyz` (which echoes dependency errors) to the internet. | The app vhost refuses both and answers `404`; the app listener is loopback-only, so a host-local scraper still works. |
| `--version` did not exist, `version` was always `"dev"`, and `serve` never checked the schema. | `newcloud --version` prints a stamped build (Makefile, Dockerfile build arg, CI), and `serve` refuses to start when the database is behind the binary, naming the missing migrations. |
| `limits.max_retained_version_bytes` was documented and had no effect: the worker only read a runtime settings row. | The configured cap is wired into the worker; a runtime setting still overrides it. |
| `docker compose up -d` SIGKILLed in-flight uploads after 10 s while the server drains for 30 s. | `stop_grace_period: 45s`. |
| The backup manifest recorded the literal `restic:latest`, the dump was only checked for a readable TOC, and nothing alerted on a stale backup. | The manifest records the real snapshot id (which survives `forget --prune`), the dump must contain the application's tables, and `newcloud_backup_last_success_timestamp_seconds` drives a stale-backup alert (opt in with `BACKUP_METRICS_FILE`). |
| A lock on a collection did not fence anything: COPY could add a member to a Depth: 0-locked folder, MOVE could remove one, and DELETE of a folder returned 204 with a locked file inside it. A failed COPY's rollback also trashed a file that had been locked and reported as success. | COPY/MOVE consult the destination's collection (Depth: 0 for a new member, Depth: infinity for an existing one), MOVE also consults the source's, and DELETE checks every live lock in the subtree with one recursive query (`LocksInSubtree`). A holder proceeds by submitting its token; everyone else gets `423`. The rollback skips a node that gained a live lock, the same way it already skipped one that gained a dead property. |
| The in-memory backend diverged from PostgreSQL on edges the shared suite did not cover: it numbered versions `len+1`, so after the `MaxVersions` cap trimmed the oldest, the next save reused a number that was still in use; and it answered `Children` and `Versions` for a missing node with `ErrNotFound` where PostgreSQL answers "no rows". | Version numbers are allocated from the current maximum (the same rule PostgreSQL uses), and both methods answer an empty result for a missing node. All three are now conformance cases, so the backends are held to the same answer rather than one of them being silently excused. |
| `newcloud audit prune` failed as the documented application role, and only after writing the archive: the grants give `newcloud_app` `INSERT, SELECT` on `audit.events` (deliberately — an append-only log the application can delete from is not append-only), so the `DELETE` could never succeed. A destructive maintenance command that half-succeeds is worse than one that refuses. | The command checks the connected role's privileges before it verifies the chain or writes anything, and refuses naming the missing grant. `AUDIT_PRUNE_DATABASE_URL` points this one command at the schema owner without changing what the server runs as. `--dry-run` only reads, so it still works as either role. |
| Two concurrent `newcloud migrate` runs raced: a rolling deploy, or a compose restart while cron was running the migration, had both processes read "not applied" for the same file and both apply it, so the loser failed on a duplicate object — and the failure landed on whichever process was later, not the one an operator watches. | The whole run holds a PostgreSQL advisory lock (`MigrationLockID`) on the one connection that executes the migrations, released when the session ends even if the process is killed. |
| The worker ran every duty from one select loop, so a slow blob sweep delayed lock expiry — the one duty whose lateness a user feels directly — despite a comment claiming the duties were independent. A panic anywhere ended the process, taking the HTTP listener with it, and nothing bounded a pass, so a query blocked on a lock stalled every other duty with no signal. | Each duty runs in its own goroutine with its own ticker. A pass is wrapped in a panic boundary that logs the stack and keeps the process up, and in a thirty-minute timeout, after which it is abandoned and retried on the next tick (every duty is idempotent). Shutdown is joined: `Run` waits for in-flight passes instead of returning underneath them. |
| A foreign-key violation was mapped to `404 Not Found` indiscriminately, so a node referencing a `blobs` row that does not exist — storage damage — was reported to the client as "no such file". That is the opposite of what it means and sends an operator looking at the right place for the wrong reason. | Only a missing *parent* is a 404 (the caller named a collection that is gone). Every other referential failure is returned as a fault, which the HTTP layer answers `500`, matching the failure-behaviour table's promise that a present row with missing content is an error and never a 404. |
| PROPFIND never returned `DAV:supportedlock` or `DAV:lockdiscovery`, so a client could not tell "not locked" from "this server has no locks"; dead properties written by PROPPATCH were never returned, making PROPPATCH write-only; and a property the server did not have was omitted instead of answered, which RFC 4918 §9.1 forbids. A `LOCK` response also reported `Depth: infinity` whatever the client asked for. | PROPFIND advertises `supportedlock` (exclusive, write) and reports `lockdiscovery` for the locks actually held — read in one query for the whole listing — and returns stored dead properties. Named properties that are not set are answered in a `404 Not Found` propstat, so a client can distinguish "unset" from "ignored". `LOCK` echoes the depth the client requested (a refresh echoes the depth the lock already has). |
| A COPY to a new name published its bytes at the destination **before** creating the metadata row, so a concurrent `PUT` that won that name had its bytes replaced by a copy that then failed to publish — the winner's success revoked with no error addressed to it. | Where the content address is the path (the `tree` layout) the copy stages beside the destination and installs the bytes only after the row commits; a lost race removes the stage and reports a failure, and a publish that fails after the commit trashes the row rather than leaving a file that answers 500 forever. The `blob` layout writes directly, because there the path is a metadata mapping and a losing copy can only leave an orphan blob. |
| A DELETE resolved the node and trashed it in a later transaction, so a `PUT` that updated that row in between was trashed with it and the writer's success vanished. | The trash is conditional on the row still matching the resolved ETag (`TrashCappedExpect`, evaluated inside the statement that trashes, not by a separate read). A writer that committed first now wins and the DELETE answers `412`. |
| A full `go test ./...` with `TEST_DATABASE_URL` set was racy: the PostgreSQL suites share one database and reset its schemas, so parallel packages dropped each other's tables. | CI and the Makefile run `-p 1`. |

Two things this table does **not** claim: the ADR-0013 staging machinery is still not wired into the
DAV request path (COPY/MOVE/`tree`-layout crash windows remain as described above), and no real desktop
WebDAV client has been run against this build.

The remaining four probes are one design gap, not four bugs: on the `tree` layout the content address
**is** the path, so a physical move cannot be made atomic with the metadata commit without the
operation ledger ADR-0013 designs (a delete that relocates content to a trash area, and reads that do
not depend on the live path). Until that lands, `internal/dav/phase0_red_test.go` keeps all four
reproductions, and `TEST_DATABASE_URL`-free runs of `go test ./...` do not execute them — they carry a
`phase0red` build tag.

DELETE's *resolve-to-trash* window is closed: the trash is conditional on the row still matching what
the handler resolved (`TrashCappedExpect`), the same arbitration `UpdateFile.Expect` gives a write. What
that does not cover is the case the probe models, where a physical move has already happened outside the
metadata's knowledge — that is the ledger's job.

## Where the evidence lives

- [Implementation record](../reference/implementation-record.md) — contract by contract, with the
  tests that pin each one and the remaining gaps.
- [Functional requirements](../reference/functional-requirements.md) — the input document and its
  priority model.
- [Restore drill runbook](../../deploy/backup/restore-drill.md) — measurements, PITR, and the
  drill record.
- [Alert runbook](../../deploy/monitoring/runbook.md) — what each alert means.
