# ADR-0013: File-operation consistency for the blob and tree layouts

**Status:** Accepted (v2.1.1) — owner approved the v1 support matrix on 2026-10-24; implementation proceeds in gated phases. **Production deployment is not authorized.**
**Date:** 2026-10-24
**Baseline:** Git `6ed3f92b16349447f7443b95bd250b41acd1f0e8` plus the uncommitted working tree described in Appendix A.
**Related:** [ADR-0003](0003-storage-layout.md), [ADR-0004](0004-metadata-store.md), [ADR-0007](0007-s3-storage.md), [tree-storage-consistency.md](../reference/tree-storage-consistency.md), the 2026-10-24 architecture review and all independent reviews.

## Revision history

| Version | Date | Change |
| --- | --- | --- |
| v1 | 2026-10-24 | Initial decision package: B + narrow A, journal, revision, phases. |
| v2 | 2026-10-24 | Adds the read-side publication fence and open-handle contract (A-001), hierarchical ancestor/subtree fences (A-002), Tree DELETE relocation before no-replace publish (A-003), a complete crash/readiness state machine (A-004), enforced single-process ownership and legacy-writer migration (A-005), platform-specific no-replace and durability contracts (A-006), and the non-blocking contracts from the review. Status remains Proposed. |
| v2.1 | 2026-10-24 | Makes the read-route `OpenLive` migration a Phase 2A entry gate (D1); makes minimal real Tree Restore part of Phase 2A or disables every old restore/purge path, with no false 200 (D2); rewrites the legacy-trash backfill to protect live-owner bytes and mark unrecoverable rows (D3); adds runtime lease-loss fail-stop, a Tree OS lock and epoch fencing for in-flight publishes (D4); adds the PURGING tombstone protocol; and incorporates the secondary clarifications (key identity/mode, pinned handle metadata, per-file source semantics, journal wording, unreachable state, no metadata-only support). |
| v2.1.1 | 2026-10-24 | Adds Binding Gate **G1**: every legacy writer is fenced or rejected before the first Phase 2A physical DELETE/backfill, not at Phase 2B. Adds Binding Gate **G2**: external visibility of a staged node is decided by the whole ancestor chain, not the target row's `staged_by`, and applies to every direct-ID route. Adds the corresponding Phase 1/1R/2A/3 tests. Status remains Proposed. |
| Accepted | 2026-10-24 | Owner approved the full v1 support matrix in §11. Status changes to Accepted; implementation proceeds through Phase 0/1 gates. Production deployment remains unauthorized. |

### Response to the independent review

| Review item | Resolution in v2 |
| --- | --- |
| A-001 Tree physical-first vs read visibility | Read-side publication fence at path resolve + open, with stable open handles; GET/HEAD/Range/download/thumbnail/archive covered; crash/readiness gate blocks inconsistent reads (§4.2, §4.4, §4.5). |
| A-002 FenceKey lacks ancestor/subtree conflict | Node-ID hierarchy: shared ancestor intents conflict with exclusive subtree intents; canonical key ordering; deterministic MOVE-dir vs PUT-child tests (§6.3). |
| A-003 DELETE leaves bytes on the live path before NoReplace | Tree DELETE content relocation is a **Phase 2A gate** and must ship before any Phase 2B no-replace publish is enabled; migration backfill for existing trashed rows (§4.3, §8). |
| A-004 Crash/readiness insufficient | Expanded per-transition durability/recovery table; global readiness gate before all public reads and worker consumption; quarantine by operation ID, never path-blind deletion (§4.4). |
| A-005 Single node is not single process; legacy writers | PostgreSQL-backed singleton lease enforced at startup; every mutation entry point joins the fence in Phase 2 (2A/2B), including not-yet-migrated legacy operations (§2, §8). |
| A-006 No-replace is not `os.Rename` | Explicit Linux/Windows/macOS primitives, per-filesystem probe matrix, symlink/casefold/volume tests, fsync contract, fail-closed startup (§4.2, §6.2). |
| Non-blocking contracts | Revision vs ETag, direct-ID staging leakage, source snapshot policy, journal scope, stage quota/GC, backup consistency, idempotency, purge scope (§6.4). |
| New probes | Reader gap, DELETE→same-name PUT, stale If-Match added to Phase 0; prior P0 matrix retained (§8, §9). |

### Response to the v2 re-review

| Re-review item | Resolution in v2.1 |
| --- | --- |
| D1 phase order: Phase 2A can relocate bytes before readers use `OpenLive` | Phase 2A has an explicit **entry gate**: every byte route is migrated to `OpenLive` (or refused for affected Tree content) and DELETE holds the exclusive read/write fence before any physical move. The dependency graph is in §8 Phase 1R/2A and §4.5. |
| D2 real Tree Trash migration vs REST restore/purge | Phase 2A implements **minimal real Tree restore** (conditional physical move + metadata CAS + audit) or disables every Tree restore/purge path; no `Restore 200 / download 404`. Purge is disabled for Tree until the Phase 5 tombstone/GC. |
| D3 legacy trash backfill can move a live owner's bytes | Backfill may never move/quarantine a path occupied by a live metadata node. It verifies ownership/content mapping; ambiguous legacy rows become `CONTENT_UNRECOVERABLE`/`LEGACY_CONFLICT` and are non-restorable. Phase 2B gate checks live-file physical consistency, not that every old trashed byte was recovered. |
| D4 PG singleton lease can vanish at runtime | Runtime lease monitor, immediate fail-stop (`/readyz` false, no new ops, in-flight publish stops at safe points), a Tree local OS exclusive lock, and a PostgreSQL lease epoch checked in every publish metadata transaction. Failure matrix in §2 and Phase 1. |
| Secondary review items | Key identity separated from mode; `OpenLive` pins the metadata version/ETag/size/mtime/authz with the handle; per-file COPY source semantics; I10/journal wording unified; unreachable journal/meta state marked diagnostic; no metadata-only restore as a supported degradation (§6.3, §4.5, §4.6, §3, §7, §4.4). |
| Purge transaction boundary | Added `PURGING` tombstone: durable non-restorable state before physical deletion, idempotent GC, then row/audit completion (§4.3, §7, Phase 5). |

### Response to the v2.1 review — Binding Gates G1/G2

| Gate | Resolution in v2.1.1 |
| --- | --- |
| **G1 Legacy writer fence** | The “all mutation entry points are fenced or rejected” condition moves from Phase 2B to **Phase 2A Entry** (and Phase 1R Exit), before the first physical Tree DELETE or backfill. It explicitly includes DAV, REST, Batch, uploads/share-drop, restore and background workers. Phase 2A cannot start while any legacy writer can bypass the fence; Phase 2B inherits the closed gate. §2, §8. |
| **G2 Effective external visibility** | A staged child may not be reachable by direct ID just because its own row is live. Visibility is computed from the whole ancestor chain (`EffectiveExternalVisibility`); path and direct-ID routes use it. Phase 1 provides the primitive, Phase 3 tests guessed/nested staged child IDs across download/thumbnail/share/ACL/search/admin. §3 I2/I14, §6.1, §6.4, Phase 1/3. |

## Decision in one page

newCloud will adopt **operation-private staging with a short, resource-scoped publish fence** ("B + a narrow A"). Partial results of COPY, upload and overwrite are never addressable by another request. They are built under an internal staging namespace, recorded in a durable operation journal, and made visible only by a conditional publish that runs under a short **hierarchical read/write fence**, verifies the node revision and WebDAV lock rule, performs the conditional physical rename (tree) and commits the metadata publication in one transaction. Readers resolve and open under the shared side of that fence and then read from a stable handle, so the physical move window is never observable. A PostgreSQL singleton lease with runtime fail-stop, a Tree OS lock and an epoch fence make the single-process scope a checked condition.

This replaces the current pattern of publishing intermediate nodes and then trying to roll them back when a later step fails. The rollback-ownership work already landed (`createdNode`, ancestor chain, dead-property guard) is preserved as evidence and as a temporary safety net, but it is not the target architecture: a rollback that must guess whether another operation touched a node is inherently incomplete. PROPPATCH and LOCK were the last two counterexamples; shares, ACLs and future side tables would produce more.

The tree layout additionally gets a **content lifecycle namespace** for trash and version content, or those capabilities are explicitly withdrawn for the tree profile. Phase 2A relocates deleted Tree bytes off the live path before any no-replace publish is enabled. Metadata-only "restore" and "permanent delete" are not allowed to claim success. Two gates are binding: **G1** every legacy writer is fenced or rejected before the first Phase 2A physical DELETE/backfill; **G2** external visibility is an ancestor-chain property, so a live-looking child of a staged root is never reachable by path or guessed ID.

The blob layout keeps its content-addressed properties and needs no physical publish race; it still uses the same journal, staging metadata and revision rules so the two layouts cannot diverge.

Rejected: adding more rollback snapshot fields; a global coordinator alone; a full stable-ID content-reference/projection redesign now.

## 1. Context: what the code does today

newCloud has two authoritative stores:

- PostgreSQL metadata is the source of truth for names, existence, ownership, ACLs, shares, locks, versions and trash.
- The content store holds bytes. `blob` addresses content by hash; `tree` addresses it by `<owner>/<visible path>`.

This is deliberate (ADR-0003/0004) and is not being replaced. The problem is that several operations publish intermediate metadata nodes and write final physical paths before their own metadata decision is final, then compensate on failure. Another request can legally mutate that intermediate/public state in the window, and the compensation cannot distinguish "my unfinished work" from "the winner's committed work".

### 1.1 Mutation inventory (current behaviour)

| Entry point | Partial result visible? | Metadata commit | Content commit | Compensation on failure |
| --- | --- | --- | --- | --- |
| WebDAV `PUT` (`internal/dav/resource.go:319`) | yes, the target is live immediately | after content write | content written to final path first | orphan bytes; tree can overwrite the previous bytes before `If-Match` is checked at line 340 |
| WebDAV `MKCOL` (`resource.go:544`) | yes | after physical Mkdir | physical first (`dircommit`) | metadata failure leaves an unreferenced directory (documented) |
| WebDAV `COPY` (`move.go:619,710`) | yes, every directory/file node is live as created | node by node | final path per file before `CreateFile` | `rollbackCopy` by node IDs + snapshot; incomplete for side tables and locks |
| WebDAV `COPY` overwrite (`swap.Install`) | staged under a visible reserved name | one tx at the end | physical moves before the tx | staged tree discarded/trashed; destination preserved |
| WebDAV `MOVE` / REST move/rename (`internal/swap/relocate.go:82`) | destination becomes live at metadata commit | before content | physical rename after metadata | metadata put back if physical rename fails; concurrent writers are not fenced |
| WebDAV `DELETE` / REST delete (`resource.go:471`, `files.go:799`) | metadata trash; bytes stay at the live path | only | **no content move** | none |
| REST restore / DAV restore | metadata-only | yes | **no content move** | none |
| Upload / resumable complete / share drop (`files.go:486`, `uploads.go:202`, `sharepages.go:477`) | target live at metadata commit, after physical write | after content | final path first | orphan bytes on metadata failure; tree stale precondition can overwrite |
| Batch COPY (`server/transfer.go:300`) | same as DAV COPY | node by node | final path per file | **no rollback at all** for a new destination |
| PROPPATCH (`move.go:107`) | dead property stored in a side table | side table only | none | no audit row is written at all |
| LOCK (`lock.go:112`) | lock row + audit | yes | none | no node row change, so it is invisible to content snapshots |
| ACL / share grant | side tables | yes | none | invisible to content snapshots |
| Worker purge (`worker.go:314`) | metadata rows deleted | yes | **no tree content collection**; blob GC is separate | none |
| Worker version expiry | rows deleted | yes | no tree content collection | none |

### 1.2 The defect class

The reviews found different symptoms of one class:

1. **Visibility without isolation.** A COPY creates real nodes under the destination name before the COPY is complete. Other clients can GET, PUT, PROPPATCH, LOCK, MOVE or share them. If the COPY later fails, its rollback can revoke those successes.
2. **Ownership inferred from creation, not from current custody.** A node ID or content fingerprint cannot prove that a successful LOCK, dead-property write, share, ACL grant or later move has not changed the custody of the resource.
3. **Physical writes before the metadata decision.** The tree layout writes `Put` to the final path before `tx.CreateFile`/`If-Match`/lock checks. A failed metadata decision can leave the winner's bytes replaced.
4. **No durable commit state.** Filesystem and database cannot commit together; there is no journal, so every compensation is best-effort and every crash window is undefined.
5. **Lifecycle features reuse the live path as content storage.** Trash and (tree) versions have no independent content reference; a later name reuse overwrites the bytes the trash/version points at. Purge deletes rows but not tree bytes.
6. **The lock protocol is not a fence.** Locks are checked by handlers, but `tx.Trash`/rollback/worker paths do not consult them; a successful LOCK is not an ownership token for compensation.

### 1.3 Confirmed gaps beyond the latest audit

The independent review reproduced the stale-`If-Match`, reader-gap and DELETE/no-replace probes below; the remaining items are source-confirmed and need their own red tests in Phase 0.

- **Tree PUT with stale `If-Match` overwrites the old bytes before returning 412.** `Content.Put` runs at `resource.go:319`; `checkIfMatch` runs at line 340. The review's real tree run returned 412 and then GET returned the new bytes with the old ETag; blob returned the original bytes. This is a current data-integrity P0.
- **Tree MOVE/rename has a metadata→physical window.** `swap.Relocate` commits the metadata rename first; a concurrent PUT/PROPPATCH on the new path can be overwritten by the later `Content.Move`.
- **Reader gap during replacement.** Applying the proposed physical sequence to the real tree store: after `old→backup`, GET returned 500 while metadata still named the old path; after `staged→final` but before metadata commit, GET returned new uncommitted bytes with the old ETag. This is why v2 requires the read-side fence and open-handle contract.
- **DELETE does not free the live Tree path.** `PUT reuse.txt`, `DELETE` 204, metadata not found, but `Tree.Content.Stat("reuse.txt")` still exists with bytes. A later same-name `PublishNoReplace` would fail `EEXIST` even though metadata says the name is free. Phase 2A must precede no-replace publish.
- **Swap live staging is addressable.** The reserved staging name is a real metadata node under the destination parent; a client that lists the parent during a long overwrite can see and mutate it.
- **Batch COPY and DAV COPY do not share a property.** Batch new-directory COPY has no rollback (`transfer.go:300` returns the partial tree), while DAV has one. The same failure produces different user-visible states.
- **PROPPATCH writes no audit row** despite ADR-0004's "every write is audited in the same transaction" invariant.
- **Lock coverage is handler-specific.** MKCOL and PUT check ancestor locks; `handleCopyMove` checks the source and an existing destination but not a Depth: 0 lock on the destination parent when COPY creates a new child. Batch COPY likewise has no destination-parent lock guard. This is a protocol lock gap independent of the rollback bug, and part of why lock state must move into the fenced mutation layer rather than remain a per-handler convention.
- **Restore does not take the destination parent lock.** A restore into a collection locked against membership changes can still add the name; the same fence must cover it.

## 2. Assumptions and support matrix (owner approval required)

These are not silently assumed requirements. Each is a decision the owner must confirm.

| ID | Assumption | Current evidence | If rejected |
| --- | --- | --- | --- |
| A1 | **Single active application process** is the v1 consistency scope, **enforced at startup** by a PostgreSQL singleton lease (session advisory lock or a lease row). A second process must fail closed rather than run with a process-local fence. D-09 already says single node first; this ADR makes it a runtime guard, not a README convention. | D-09; deployment guide; one binary includes worker. | The publish fence must move to a database advisory lock/lease from day one; staging becomes shared-filesystem sensitive. |
| A2 | **Direct external modification of a tree root is unsupported.** It is a human-readable backup/export view, not a concurrent write interface. | D-07 (forbidden by default); tree docs. | Requires a scanner/quarantine design; automatic adoption of external changes is a different product. |
| A3 | **Tree layout is local disk only**; no S3-tree. | ADR-0003 §9. | Not planned. |
| A4 | **Tree trash restore and physical purge are supported features** only if this ADR's Phase 5 (or a scoped alternative) ships. Until then they must be hidden or explicitly marked metadata-only; no false success. | Reviews R8-R11; current UI offers restore. | Withdraw tree restore/purge from the first release and document it. |
| A5 | **Tree historical version content** is optional and separately scoped. | `409 no_version_content`; UI hides restore. | Keep as a documented gap. |
| A6 | **Locks are a write fence**, not merely advisory metadata. | ADR-0002/0004; `lockBlocks` in every handler. | If locks become advisory, document it and do not claim Office/DAV co-authoring safety. |
| A7 | **A crash may happen at any instruction boundary.** Restart must reconcile from durable state; in-memory coordination is never a correctness source. | Standard requirement. | Cannot claim crash safety. |
| A8 | **Staging is on the same filesystem as the live tree** (rename, not copy) and is excluded from every API view. | Deployment says content root is one filesystem. | Publish becomes copy+delete; much larger space/CPU cost and different crash semantics. |

The recommended design works on A1 (in-process publish fence), but keeps the fence behind an interface so A1 can become "single node first, DB advisory fence later" without changing handlers. A2 means staging may live in a hidden directory on the same tree root; backup tooling must exclude it (A8).

**A1 enforcement.** Phase 1 acquires a PostgreSQL singleton lease before the HTTP listener is marked ready or the worker consumes jobs. If it is a session advisory lock, it must be held on a dedicated connection for the process lifetime (not borrowed from the pool); alternatively use a lease row with an owner ID and heartbeat. If the lease is held, the process exits with a clear error; hot upgrades must drain/stop the old process first. **At runtime**, losing the lease is a fail-stop condition: `/readyz` false, no new operations, in-flight publishes aborted at safe points, epoch check in every metadata publish, and a Tree OS lock preventing a second local writer. See §6.6. This makes "single process" a checked condition rather than an assumption that operators can violate silently.

**Migration-time writer coverage (Binding Gate G1).** Before the first Phase 2A physical Tree rename/DELETE/backfill, every mutation entry point that can create, replace, rename, restore, delete or mutate the same Tree namespace must acquire the compatible hierarchical fence for the entire relevant physical-and-metadata transition, or be rejected by the feature gate. This includes DAV (PUT/MKCOL/COPY/MOVE/DELETE/PROPPATCH/LOCK), REST (upload/resumable complete/share drop/mkdir/rename/move/batch/delete/restore/ACL/share), background workers (trash/version/GC/retention), and any future writer. No Phase 2A physical operation is permitted while a legacy writer can bypass that fence. A coarse owner-wide fence is acceptable temporarily for an unmigrated operation; leaving an old writer outside the fence is not. The Phase 2A Entry checklist includes this proof; Phase 2B inherits the closed gate and does not postpone it.

## 3. Correctness contracts

These are the testable invariants. "Published" means reachable through a normal path by a normal request.

**I1 — Namespace authority.** Only a live metadata node makes a path exist. Content-store entries alone never make a file or directory visible.

**I2 — Operation privacy.** A node created by an in-progress operation is not resolvable, listable, readable, writable, lockable, shareable or ACL-addressable by any other request until its publish commits. Staging nodes are reachable only by internal code holding the operation ID. Privacy is an **ancestor-chain property**: a live-looking child of a staged/hidden root is not externally visible by path or by ID.

**I3 — Publish is the visibility commit, and readers see one complete state.** The metadata publication transaction is the visibility commit. Physical moves are made under a fence that also excludes path-resolve-and-open by readers; a reader either opens the old complete content (and keeps a stable handle) or the new complete content, never a 500 or new bytes under old metadata. A crash leaves the resource unavailable (readiness gate) until recovery completes or quarantines it.

**I4 — Conditional creation/replacement.** Creating a name or replacing a node is decided atomically against the current metadata (name, revision, ETag, lock state), in the same transaction that changes it. Physical publication must be conditional too: a tree publish never replaces an object that this operation does not own.

**I5 — No cross-operation rollback.** Rollback is only "discard my unpublished staging". A published node is changed only by an explicit, fenced operation that passed the current-state checks. No snapshot/fingerprint exception list.

**I6 — Lock fencing.** A node with a live lock may be trashed, renamed, replaced or rolled back only by a request that satisfies RFC 4918 (token or same-holder rules), checked in the same metadata transaction as the mutation.

**I7 — Side-table participation.** Dead properties, shares, ACL grants, locks and versions are resource state. A mutation of any of them changes the resource revision (or is serialized by the same resource fence). Compensation must not need a table-specific exception.

**I8 — Conditional writes happen before physical destruction.** On the tree layout, `If-Match`, `If-None-Match`, lock and revision checks are decided before the old bytes can be replaced. Staging exists so the decision can be made without touching the target.

**I9 — Lifecycle truthfulness.** Trash restore returns the bytes that were deleted; purge deletes the content it claims to purge; a version's content is either retained by a stable reference or the version-content API refuses. "Metadata success" is not "content success".

**I10 — Crash recovery.** Every mutation that crosses the content store or can be interrupted mid-publication has a durable journal state before it creates staging or moves content. Metadata-only single-transaction mutations are atomic in PostgreSQL and do not need a journal row (§6.4). Startup recovery completes, aborts, or quarantines every non-terminal state without violating I1-I6. A retry of a committed operation is idempotent or returns the committed result.

**I11 — No mutual destruction.** Two concurrent operations on the same name may both fail or one may win; neither may report success and later have its committed result removed/overwritten by the other's failure handling.

**I12 — Audit truthfulness.** A committed mutation and its audit row commit in one transaction. A refusal writes no "ok" row. Staging discards are recorded as system events where they destroy data.

**I13 — Read-side publication consistency.** Every route that serves bytes or opens a file (GET/HEAD/Range, download links, share downloads, thumbnails, archive/zip members, version content, preview) resolves the path and opens the content under a shared read fence and then reads from the stable open handle. A replacement/move/delete/recovery takes the exclusive side of that fence for path mutation. No reader may observe the old → backup or staging → final intermediate during a replacement, and no reader may receive new bytes with an old metadata ETag. A handle opened before the replacement continues to serve the old complete bytes where the OS guarantees it; where it does not (platform or FS), the open is retried or refused with a defined status, never partially returned.

**I14 — Staging is invisible by ID too, via effective visibility.** No API that accepts a node ID (share, ACL, version content, thumbnail, download, search, admin, listing, audit) may reveal an unpublished node or its bytes. The check is not "the target row's `staged_by` is null"; it is `EffectiveExternalVisibility(node)` (G2): the node and every ancestor up to the user-visible root are live/reachable, none of them is staged, quarantined, unpublished, trashed or owned by a non-committed operation, and the request is authorized for that stable identity. Path resolution and direct-ID routes use the same helper.

## 4. Target operation lifecycle

### 4.1 States

```
                +-------------+   crash/abort   +-----------+
   request ---> |   INTENT    | ---------------> |  ABORTED  |  (no visible state)
                |  (journal)  |                  +-----------+
                +------+------+
                       | create staging (invisible)
                       v
                +-------------+   abort/discard  +-----------+
                |   STAGING   | ---------------> | DISCARDED |  (staging GC)
                +------+------+                  +-----------+
                       | all bytes/metadata ready
                       v
                +-------------+  fence conflict   +-----------+
                | PUBLISHING  | ---------------> | DISCARDED |
                | (short fence)|                 +-----------+
                +------+------+
                       | conditional physical move + metadata tx
                       v
                +-------------+
                |  COMMITTED  |  (visible; audit + revision committed)
                +-------------+
```

`PUBLISHING` is deliberately short: it holds the resource fence only for the conditional physical rename and the metadata transaction, never for the whole COPY. Long work happens in `STAGING` with no visibility and no global lock.

### 4.2 Publish protocol

A publish is a short critical section. The fence is **both read-side and write-side**; the read side is held only for path resolution and `open`, never for streaming.

1. **Acquire fence.** Take the hierarchical write intents for the destination parent/name and, for replacement, the existing node and its ancestors. Take the read-side conflict automatically: a publish is exclusive against readers resolving that path. The fence backend is an interface; the first implementation is an in-process hierarchical fence because A1 is enforced by a PostgreSQL singleton lease. Correctness still rests on the journal and conditional checks, not on the lock alone.
2. **Revalidate, including authorization.** Inside the fence, re-read the target: name free or expected node ID; expected revision; ETag/precondition; lock rule (token/Depth); ACL create-vs-overwrite; quota. Re-evaluate the authorization decision under the fence (or check its revision) so a grant revoked between the handler's first check and the commit cannot win by racing. These checks use the same transaction/snapshot as the metadata commit, or a CAS token that makes a late change fail.
3. **Physical publish (Tree only; Blob has no path).**
   - **New name:** `PublishNoReplace(staging, final)`.
   - **Replacement:** `old → backup`, then `staged → final`, under the exclusive fence. The old node, backup path, expected revision and operation ID are already in the journal.
   - **No-replace is a syscall contract, not `os.Rename`** (see §6.2). If the target exists, the publish fails without touching it. If the platform/volume cannot prove no-replace, the tree layout refuses to start.
   - Readers are blocked by the fence from resolving/opening this path during these moves. An already-open handle may continue on the old bytes; the platform contract in §6.2 says whether the rename is allowed while that handle is open, and the publish either waits/retries or fails before metadata commit.
4. **Metadata commit (one tx).** Create/rename/unhide nodes, bump revision, update blob/version/trash refs, write audit rows, update the journal to `COMMITTED`. For large trees, staging nodes already exist hidden; publication is a root rename/unhide plus revision bump, not 10,000 inserts.
5. **Release fence, then cleanup.** Cleanup of staging/backup is best-effort; the journal drives recovery if the process dies. No reader is released into the intermediate because the metadata commit is the visibility point.

**Open-handle rule (A-001).** The shared read fence is released only after `open` returns a handle. On Unix, renaming/unlinking a path does not change the inode an open handle refers to, so the reader continues with the old complete bytes. On Windows, the publish may be forced to wait or fail if an existing handle blocks the rename; the platform probe and tests must establish the exact contract. The response is always one of: old complete bytes, new complete bytes, or a defined retry/refusal status — never a half-file or new bytes with old metadata. If a platform cannot provide that, the tree layout is not supported there.

**Why physical move before metadata (tree only).** Metadata-first would make a live node whose bytes are not yet at the path (the NC-R6-001 class). With staging, only an invisible staging name exists before the move; the move is conditional and the metadata transaction is the visibility point. Journal recovery handles the window where the physical move succeeded but metadata did not commit; the readiness gate prevents readers from observing that window.

### 4.3 Operation-specific sequences

**COPY to a new name (DAV or batch).**
Resolve the source tree with `treewalk` (unchanged). Journal `INTENT` → create hidden staging root → materialise stages → `PUBLISHING` → physical no-replace publish of the staged directory → metadata tx unhides/renames the root and bumps revisions → `COMMITTED`. On any pre-publish failure: delete staging, journal `DISCARDED`. There is no rollback of visible nodes and therefore no `createdNode`/fingerprint logic in the target design.

**COPY over an existing destination.** Same, then the existing `swap.Install` sequence becomes a publish under the fence: old → backup, staged → final, metadata tx renames aside/installs/trashes, journal commit. The staged tree is invisible until the publish. A concurrent PUT/LOCK/PROPPATCH on the old destination either wins before the fence (the fence/ETag check sees it) or is refused after the publish; it is never silently revoked.

**PUT / upload / share drop.** The bytes are already staged (upload staging or a new operation staging area). Publish under the fence: check If-Match/If-None-Match/locks/revision, conditional physical publish for tree, metadata `WriteFile`/`UpdateFile` with revision bump and audit. For a new file, the metadata publication + no-replace publish means a stale precondition cannot overwrite the old bytes. For the blob layout the metadata update is the commit; content is addressed and can be GC'd later.

**MOVE / rename.** Fence source node and destination parent/name; journal; move metadata and physical bytes in a defined order with a recovery decision. On tree: conditional rename source → destination (no replace) for a file; for a directory, rename into a free name and restore on failure. Metadata rename is in the publish transaction. A concurrent write to the old or new name is fenced; after the move it sees the new state. Crash recovery uses the journal's from/to IDs and paths.

**DELETE / trash.** Fence the node and hold the exclusive read/write fence through the physical move and metadata trash commit (G1: no legacy writer may mutate the same namespace outside this fence); if tree, move its bytes to the internal trash namespace **before** the metadata trash transaction (journal records source and trash content path), as the Phase 2A gate. Then metadata marks trashed + revision + audit. A later name reuse cannot touch those bytes because they are no longer at the live path. Blob already has stable content refs. **No-replace publish is not enabled on a tree deployment until the Phase 2A backfill has classified every legacy trashed row and guaranteed that no live-owner path is moved; unproven legacy paths that cannot be recovered are surfaced as occupied/needs-operator names, not silently overwritten.** Otherwise a legal `DELETE` followed by the same-name `PUT` could fail with a physical `EEXIST` while metadata says the name is free.
**Restore (Phase 2A minimal, D2).** Fence the old name and parent; conditionally move the content reference `trash → live` and restore metadata/CAS/audit in the same journaled operation; refuse with a clear conflict if the name is taken, and never return success while the bytes remain in the trash namespace. If the minimal restore is not implemented at the Phase 2A gate, every Tree restore entry point is disabled with an explicit unavailable/conflict status.
**Purge (Phase 5 tombstone, D5).** Tree purge is disabled until Phase 5. Then: (1) a durable metadata transaction marks the trash root `PURGING`/non-restorable with its content reference and operation ID; (2) physical deletion runs idempotently against that immutable reference; (3) rows are deleted and the tombstone/audit completes. If physical deletion fails, the row stays non-restorable and retryable; bytes are never deleted before a durable tombstone. Blob refcount GC keeps its grace period.

**LOCK / UNLOCK / PROPPATCH / share / ACL.** Take the node fence for the short metadata transaction, bump the node revision, and write the audit row (PROPPATCH is currently missing one). The fence serializes these with publish and MOVE; because staging is invisible, there is nothing to roll back. This is where the LOCK/PROPPATCH/Share/ACL counterexamples disappear — not by adding fields to the rollback snapshot.

### 4.4 Failure and crash state machine

The durable journal states and required actions are:

| State / point | Durable bytes and metadata | Required recovery | Reader outcome |
| --- | --- | --- | --- |
| `INTENT` committed, no staging | none changed | mark `ABORTED`; nothing to delete | old state |
| `STAGING` partial | journal + some staging | discard staging (operation-owned); `DISCARDED` | old state |
| `PUBLISHING` before any rename | full staging | discard staging; `DISCARDED` | old state |
| after `old → backup`, before `staged → final` | backup old, staging new, final name absent, metadata old | restore `backup → final`, discard staging, metadata unchanged, `ABORTED` | blocked by readiness gate until restored; then old |
| after `staged → final`, before metadata commit | final new, backup old, metadata old (replacement) or no metadata (new name) | replacement: revert `final → staging`, `backup → final`; new name: revert `final → staging`; `ABORTED` | blocked; then old/absent |
| metadata transaction rolled back (incl. audit failure) | physical may be in either intermediate state | same compensation as above; if compensation fails, `NEEDS_OPERATOR` | blocked; 503, no wrong bytes |
| metadata transaction committed, journal still `PUBLISHING` | final new + metadata new, backup old | **Normal protocol cannot reach this** because journal `COMMITTED` is in the same transaction; if observed, the row/transaction is corrupt: verify node/op identity/revision, quarantine, never assume; set `NEEDS_OPERATOR` | unavailable until resolved |
| committed, cleanup pending | final new, metadata new, backup/staging residual | cleanup by journal; idempotent | new |
| `NEEDS_OPERATOR` | mixed state not safely resolvable by code | instance stays not-ready; admin tooling/quarantine; never path-blind delete | 503 for affected space (v1: instance-wide) |
| path exists with no journal/op ID | unknown ownership | move to internal quarantine keyed by timestamp; investigate; never delete by path | not served as a live file |

Durability points that must be implemented and tested:

- the journal `INTENT`/`STAGING`/`PUBLISHING` rows commit in PostgreSQL before the first staging byte;
- stage file data is fsynced; parent directories are fsynced after create/rename where the platform supports it;
- after `old → backup` and `staged → final`, both parent directories are synced before the metadata transaction; on Windows use the available write-through/flush contract and test it;
- `fsync`/flush failures abort the publish before metadata commit and are reported, not ignored;
- the metadata transaction's commit and the journal `COMMITTED` update are in the same transaction;
- recovery is idempotent and can be interrupted and retried;
- a second crash during recovery leaves the same state recoverable.

**Readiness gate.** Before serving any public read or write route, before accepting WebDAV, and before the worker consumes jobs, startup acquires the singleton lease, scans non-terminal journal states, and either recovers them or puts the instance in `NEEDS_OPERATOR`. `/readyz` is false until this completes. In v1, `NEEDS_OPERATOR` makes the whole instance unavailable (503) rather than risking wrong bytes; per-owner/per-resource quarantine is a later refinement.

### 4.5 Read-side publication consistency

Every byte-serving route uses one helper, `OpenLive` (name illustrative):

- It acquires the shared side of the resource path/ancestor fence, performs metadata resolution, applies **`EffectiveExternalVisibility` (G2)** so a live-looking child of a staged/root-hidden ancestor is not resolvable, checks authorization, then opens the content and releases the fence **after** the handle is open. The handler streams from the handle; it never re-resolves the path for each read/Range. ACL/share revocation takes the exclusive side of the node/ancestor, so it either happens before this open (reader denied) or after it (an already-started stream may finish; no new open is allowed).
- Routes that must use it: WebDAV GET/HEAD/Range, REST download, inline preview, share download, thumbnails, archive/zip member opens, version-content download, and any future byte route. The content invariant test that enumerates byte-emitting routes must be extended to require this helper.
- A publish/replace/move/delete takes the exclusive side. A reader either opens before the exclusive acquisition (old handle, old complete bytes) or after it (new complete bytes). It never observes the `old → backup` or `staging → final` window.
- Metadata-only responses (PROPFIND, listings, search, ACL checks) read metadata and do not open content; they may see the old metadata until the commit, which is a complete old state. They must not consult the filesystem for size/type in a way that can observe an intermediate (use metadata).
- **Open handles:** Unix keeps the inode; Windows may block the rename. The platform contract tests two readers: one open before publish, one attempting open during publish. The publish is allowed to wait/retry or fail before metadata commit; the reader gets either old bytes or a defined retry/refusal, never 500-after-partial.
- **Archives and multi-file operations** are per-file observations unless the product later chooses a source snapshot. The ADR documents that an archive may mix source versions across members; each member is individually complete, and the source ACL is revalidated at each open. A future snapshot feature can hold a source-wide shared fence for the whole archive, at a documented cost.

**D1 entry gate: no Phase 2A physical DELETE before the read routes are migrated.** The dependency is:

```
Phase 1: OpenLive + read fence + route inventory test
   -> Phase 1R: every byte route below uses OpenLive; unmigrated routes are disabled for Tree content
      -> Phase 2A: DELETE may take the exclusive fence and move bytes
         -> Phase 2B: no-replace PUT/upload publish
```

The Phase 1R exit test injects a GET/HEAD/Range at both points of a simulated Tree DELETE (`live→trash` before metadata, and after physical move before commit) and requires old complete bytes or a defined retry/refusal, never 500/404-new-state. A route not yet migrated must be a **listed, refused** route for Tree content, not an unguarded reader. The byte-route inventory includes at least:

| Route group | Current surface | Required before Phase 2A |
| --- | --- | --- |
| WebDAV GET/HEAD/Range | `dav.handleGet` | `OpenLive` |
| REST download and inline preview | `server.handleDownload` (preview mode) | `OpenLive` |
| Share landing/content download | share download + content capability | `OpenLive` |
| Share thumbnail and file thumbnail | thumbnail handlers | `OpenLive` |
| Archive/zip streaming | transfer/archive handler | `OpenLive` per member |
| Version content | version-download handler | `OpenLive` with the version's content ref |
| Content-origin split routes | `content.go`/content listener | `OpenLive` (same helper) |
| Any future byte route | route inventory test | must be classified or refused |

`OpenLive` returns the metadata version it pinned (node ID, revision, ETag, size, mtime, authz decision) together with the handle. The handler uses those pinned values for response headers and Range checks; it must not re-query newer metadata after releasing the fence. The read-side fence is held through resolve + authz + open, then released; the open handle is the content authority for the rest of the request.

### 4.6 Source snapshot semantics for long COPY

A long COPY reads source files over time. The v1 contract is **per-file observation**: each file is read completely at its open, and the copy plan is based on the metadata snapshot resolved before staging. The source ACL/permission is revalidated at each file open and at publish; a source that disappeared, moved, or was replaced fails that file's copy before publish. A source replaced with the same name is read through the opened handle (old bytes) or rejected/retried on revalidation; the plan is not a global point-in-time snapshot and must not be described as one. This is documented in the API/limitations. A single-snapshot copy (holding source readers/fences for the whole operation) is a future option with a large availability cost; it is not silently implied.

## 5. Architecture options considered

| Criterion | A. Operation coordination + conditional writes | B. Private staging + conditional publish | C. Stable content refs + human-readable projection |
| --- | --- | --- | --- |
| Core idea | serialize every mutation with resource locks; check node generation/preconditions before changing | build complete result in an invisible operation space; publish conditionally under a short fence | content has stable IDs and references; the filesystem is a projection of live content |
| Eliminates rollback class | only if every read/write and every side table takes the same lock; otherwise no | yes: unpublished state is unreachable, so there is nothing another request can mutate or roll back | yes |
| Tree physical race | needs lock around all writes to the path; long COPY locks hurt availability; multi-instance needs DB locks held across I/O | staging never touches the final path; no-replace publish decides the winner atomically | projection writer still needs a publish rule |
| Lock/PROPPATCH/share/ACL | must all take the same node lock | only the short publish fence; staging cannot be addressed | natural: mutations are on stable refs |
| Crash recovery | locks disappear; partial physical/metadata state remains; journal still needed | journal + staging cleanup; final is untouched until publish | refs make recovery cleaner, but projection still needs a journal |
| Performance | coarse locks and waiting; long COPY is a DoS vector | no global lock during copy; one rename on publish; extra staging disk | potentially cheapest reads, highest write/projection complexity |
| Disk overhead | none | staging copy + backup for overwrite (tree already copies bytes; rename is cheap) | similar to staging for versions/trash; more metadata |
| Complexity | deceptively simple, but every entry point and side table must participate; lock ordering and deadlock handling | new staging API, hidden namespace, no-replace publish, journal/recovery | largest redesign; sync/scanner/external-write semantics |
| Security | locks must not be bypassable; lock scope is large | staging must be unreachable by every auth path; internal paths must be validated | projection exposes new surface |
| Compatibility | no new interface | new `ContentStore`/staging interfaces; blob and tree both | changes the meaning of paths and storage; migration |
| Debuggability | lock waits/deadlocks; hidden serialization | operation IDs, staging names, journal states; clear failure | powerful but many moving parts |
| Verdict | **needed as the short publish fence, insufficient alone** | **chosen primary model** | **rejected for v1; adopt only a localized content-ref layer for trash/versions if needed** |

**Recommended: B + a narrow A.** A lock/fence is still required at publish and for in-place mutations, but correctness does not rely on holding it for long operations or on every reader. C is not required to fix the class; its useful part (stable content references) is applied only to the lifecycle namespaces in Phase 5, not to the whole namespace.

## 6. Components and data-model changes

### 6.1 Metadata

- `nodes.revision bigint NOT NULL DEFAULT 1` (or `generation`). Every mutation of the node or its side tables increments it. The publish/rename/trash transaction compares the expected revision; a mismatch is a normal conflict, not a retryable blind overwrite.
- `operations` table: `id`, `kind`, `owner_id`, `actor_id`, `state`, `target_parent_id`, `target_name`, `target_node_id`, `expected_revision`, `staging_ref`, `backup_path`, `trashed_content_ref`, `error`, timestamps, `attempts`. The journal is updated in the same transaction as the visibility change; staging states are written before the first byte.
- `hidden`/`staging` ownership: recommended `nodes.staged_by uuid NULL REFERENCES operations(id)` with resolver/listing filters, or an equivalent operation-node mapping. Publication clears `staged_by` on the subtree root (descendants inherit reachability through the root; the resolver must not require a live child flag). A simpler alternative — staging metadata only in the journal and creating final nodes at publish — is possible for blob but loses incremental tree building and makes a 10k-node publish one huge transaction; not recommended. The staging namespace must be reserved against user input: either a store-level internal root outside every user namespace (preferred, needs an addressing exception in the storage API), or a reserved path segment plus validation that user names cannot claim it and a migration/rename rule for existing collisions.
- **Effective external visibility (G2):** for any node N,
  `EffectiveExternalVisibility(N) = live(N) AND live(parent(N)) AND ... AND live(user root)` **and** no node on that chain (including N) has `staged_by != NULL`/quarantined/unpublished state, and the actor is authorized against the stable chain. A staged root makes every descendant externally invisible even though the descendant rows are live; a direct-ID route must walk the chain and reject, not inspect only `N.staged_by`. Publication attests the whole chain in one transaction; an ancestor move/rename changes reachability and is fenced by the same node IDs.
- Trash/version content: a `content_refs` table keyed by `(node_id, generation, kind)` with `storage_path`/`blob_id`, `size`, `hash`, `deleted_at`, or equivalent columns. This is what makes tree restore/purge truthful.

### 6.2 Storage interfaces

Extend the storage abstraction with an operation-scoped staging contract (names illustrative):

```go
type OperationStore interface {
    ContentStore
    StagePut(ctx, opID, rel string, r io.Reader, size int64) (PutResult, error)
    StageMkdir(ctx, opID, rel string) error
    PublishNoReplace(ctx, opID, stagedRel, final layout.Node) error // atomically fails if final exists
    PublishReplace(ctx, opID, stagedRel, final, backup layout.Node) error
    Discard(ctx, opID) error
    Recover(ctx, opID) error
}
```

- Blob implements publish as a metadata/resolver operation; the content is already addressed.
- Tree implements no-replace with the platform primitive; it refuses to start if unavailable.
- The existing `ContentStore` remains the read/write interface for live files, so `Open`, `Put` (live replacement during publish only), `Stat`, `List`, `Delete` keep their meaning.
- `DeleteEmpty` (atomic rmdir-if-empty) is added for recovery/cleanup; no code may use recursive `Delete` to clean a directory of uncertain ownership.

**No-replace and durability are platform contracts, not `os.Rename`:**

| Platform | No-replace primitive | Durability/verification required |
| --- | --- | --- |
| Linux | `renameat2(..., RENAME_NOREPLACE)` through a directory FD (`os.Root`-bound); `EXDEV`/`ENOSYS`/`EINVAL` are startup-probe failures for the tree layout. | fsync stage file, fsync both parent directories after rename; test ext4/xfs/btrfs/tmpfs, symlink parents, casefold/Unicode collisions, concurrent target creation. |
| Windows | `SetFileInformationByHandle`/`FILE_RENAME_INFO` with `ReplaceIfExists=FALSE`, or `MoveFileExW` **without** `MOVEFILE_REPLACE_EXISTING`; never `os.Rename`. | Test NTFS open-handle sharing (reader vs rename), same-volume constraints, case-insensitivity, directory rename; use the documented flush/write-through call; fail closed if a volume cannot guarantee it. |
| macOS | `renamex_np(..., RENAME_EXCL)` or equivalent no-replace primitive. | fsync file and parent directories; case-insensitive default volumes; test APFS. |
| All | The probe runs against the **actual configured volume**, not a temp directory on another filesystem. | Probe file and directory publish, target-exists failure without modification, symlink/path escape, volume boundary, restart after a crash. A failed probe refuses the tree layout at startup. |

`Stat`-then-`Rename` is not a no-replace implementation. If the syscall is unavailable or its semantics cannot be proven, the tree layout is disabled; there is no silent fallback to `os.Rename`.

### 6.3 Request-path API: hierarchical read/write intents

The fence is a hierarchical node-ID lock, not a map of bare path strings. Paths change under rename; node identity and parent membership are the stable keys.

```go
type ResourceKind int // Node | NameInParent
type ResourceKey struct {
    Owner  uuid.UUID
    Kind   ResourceKind
    NodeID uuid.UUID // Node
    Parent uuid.UUID // NameInParent
    Name   string    // NameInParent
}
type FenceMode int // Shared | Exclusive
type FenceIntent struct { Key ResourceKey; Mode FenceMode }

Acquire(ctx context.Context, intents ...FenceIntent) (ReleaseFunc, error)
```

**Identity and mode are separate.** `ResourceKey` identifies the resource; `FenceMode` is the request's intent. Read and Write on the same `NodeID` must map to the same lock instance (shared vs exclusive), not to two unrelated map keys. The lock map is keyed by `ResourceKey` and stores a reader/writer lock. A `NameInParent` key and a `Node` key are distinct resources; the compatibility rules below define how they interact.

| Holder | Conflicts with |
| --- | --- |
| `Node(N)` Shared (reader) | `Node(N)` Exclusive; any exclusive ancestor of N holding subtree intent |
| `Node(N)` Exclusive (write node) | `Node(N)` Shared/Exclusive; `Node(A)` Exclusive for each ancestor A; `NameInParent(parent,N.name)` Exclusive |
| `Node(D)` Exclusive (directory move/delete) | any `Node(descendant)` Shared/Exclusive, because descendants take Shared on D as an ancestor; `NameInParent(source/dest)` |
| `NameInParent(P,N)` Exclusive (create/rename/delete) | same name key; `Node(P)` Exclusive; readers of a node at that name (they hold `Node(target)` Shared, and the name holder takes/validates the target node key) |

Rules:

- **Read (shared node intent):** used by `OpenLive` for path resolve+open. It acquires shared intent on the target node **and every ancestor node ID** (`Node` keys). It is held only until the content handle is open (or, for metadata-only operations that need path stability, for the duration of the metadata read).
- **Write (exclusive node intent):** used for content/state mutation of an existing node (PUT, PROPPATCH, LOCK, ACL/share changes, file rename). It acquires exclusive on the target node and shared on every ancestor. It therefore conflicts with a directory move/delete that holds exclusive on an ancestor.
- **NameWrite (exclusive parent/name intent):** used to create, delete, or rename a name. It acquires exclusive on the `NameInParent` key plus shared on the parent and its ancestors. Delete/rename also take `Node(target)` exclusive (the target may be absent only for create), so a reader holding `Node(target)` shared conflicts; if the target appeared after resolution, the revision/name check refuses.
- **Directory MOVE/DELETE:** exclusive on the directory node ID (which blocks every descendant operation that holds shared ancestor intent) + `NameWrite` on the source and destination names + shared ancestors. It does not need to enumerate descendants.
- **COPY/upload publish:** `NameWrite` on the destination name (and exclusive on the existing node for replacement) + shared ancestors. Staged descendants are private and need no live locks.
- **Canonical order:** sort all `(owner, kind, node/parent ID, name)` resource keys before acquisition; mode is not part of the sort key; no lock upgrades; release in reverse order. Acquire the ancestor chain from metadata before locking, then **revalidate the chain** after acquisition; if a parent/name changed, release and retry. This prevents a rename from moving a resource out from under a key. A rename/reparent bumps the moved node's revision and the source/destination membership revision, so a stale route/path observation fails rather than writing through the old key.
- **Deadlock/DoS:** short timeout; a long COPY holds no write fence; the singleton lease (A1) prevents cross-process fence loss. A later PostgreSQL advisory-lock backend uses the same key/ordering; the mode mapping is implemented by the backend, not by duplicating keys.
- **Deterministic test:** B PUT `folder/file.txt` (Write on child + shared on `folder`) vs A MOVE `folder` (exclusive on `folder` + NameWrite) must produce one serialized order: either B commits before A moves (A sees and moves the updated child) or A moves first (B resolves the new path or fails 404); no interleaving where B reports success and its bytes are later overwritten.

The lock covers **all entry points**, including legacy ones during migration (§2, §8), and the WebDAV LOCK rule is rechecked inside the same metadata transaction; the fence is not a replacement for RFC locks.

### 6.4 Additional technical contracts

**Revision vs ETag.** `ETag` continues to describe file content only. `nodes.revision` is the resource state CAS token (name, parent, content generation, lock/side-table state). A publish/replace checks the expected revision; content `If-Match` still checks the ETag. Side-table mutations bump revision but never invent or change a content ETag. The exact mapping is tested: PROPPATCH/LOCK/share/ACL change revision, not ETag; content replacement changes both.

**Direct-ID staging leakage (G2).** Every route that accepts a node ID (shares, ACL, version content, thumbnails, download, search, admin/API, audit, listing, restore) must call `EffectiveExternalVisibility` (whole ancestor chain, not the target row alone) and reject a staged/hidden/quarantined chain for external actors. The function is shared by path resolution and ID handlers; a route inventory test enumerates ID-taking routes and asserts the guard, and a nested-staged-child test guesses a child ID under a staged root to prove it is refused by download/thumbnail/share/ACL/search/admin.

**Journal scope.** The operation journal is required only for multi-store, long-running or interruptible publication: COPY, upload/publish, replace, MOVE, tree DELETE/restore/purge, recovery. Metadata-only single-transaction operations (LOCK/UNLOCK, PROPPATCH, ACL, share creation/update/revocation, revision-only changes) use the normal metadata transaction + revision + audit and do **not** need a journal row. This avoids a second write path for operations that are already atomic in PostgreSQL.

**Stage quota and GC.** Staging and backup counts against a configured server-side budget, separate from user quota (the user is not charged twice for a not-yet-visible copy). GC may delete only staging/backup paths referenced by a terminal journal state or explicitly expired `DISCARDED` ops; it must never delete a path referenced by a non-terminal operation. Limits and retention are operator settings with documented defaults.

**Backup and restore consistency.** Backups must include all committed live content, enabled trash/version content, the metadata dump and the operation journal (or a documented clean checkpoint). Staging/backup paths referenced by non-terminal operations are either included or the backup is taken after a quiesce/checkpoint that makes the journal empty; a backup that silently omits them cannot be called a consistent restore point. The restore drill must verify the journal/staging relationship, not only file bytes.

**Idempotency.** Long operations carry an operation ID in the journal. HTTP idempotency keys are optional; DAV has no standard one. The contract is **effect idempotency**: retrying after a timeout either returns the committed result (when the operation ID can be recovered) or fails without duplicating/removing committed state. The exact HTTP response for a replay is not promised unless the caller supplied an idempotency key.

**Purge scope.** "Permanent delete" means deletion from active storage and, after the retention/GC step, from the trash namespace. It does not mean secure erasure of backups, snapshots, filesystem journals or media. UI/API wording must state this scope; the backup retention policy is a separate operator document.

### 6.5 Packages

| Package | Change |
| --- | --- |
| `internal/treewalk` | unchanged; remains the only source of a complete COPY plan |
| `internal/dircommit` | keep for MKCOL/REST mkdir; move to the publish helper where a directory is created as part of a larger operation |
| `internal/swap` | keep the overwrite sequence as the publish engine; add journal, hidden staging and revision checks; retire path-based cleanup on disposable stages |
| `internal/dav` | PUT/COPY/MOVE/DELETE/PROPPATCH/LOCK take the fence and use staging/publish; delete `rollbackCopy` and the dead-property exception after Phase 4; add tests for LOCK/PROPPATCH and tree byte preservation |
| `internal/server` | upload, resumable complete, share drop, batch copy/move, mkdir, delete/restore use the same primitives; remove the batch/DAV divergence |
| `internal/store` | revision, CAS, operations journal, trash/version content refs; memory and PG conformance |
| `internal/worker` | operation recovery, staging GC, tree trash/version GC, purge that deletes content and rows; readiness gate |
| `internal/storage/layouttest` | staging/publish/no-replace conformance for both layouts |

### 6.6 Runtime singleton lease and fail-stop (D4)

A startup-only advisory lock is not enough: PostgreSQL releases a session advisory lock when the connection dies, so instance B can acquire the lease while A is still alive with an in-flight Tree publish. The v1 contract is therefore:

- **Lease:** a dedicated PostgreSQL connection holds a session advisory lock, or a `lease` row holds `(owner_id, epoch, expires_at)` with a heartbeat. Acquisition is refused if held.
- **Epoch:** the lease has a monotonically increasing `epoch` in PostgreSQL. The process stores the epoch it acquired. Every publish/metadata transaction checks `current epoch == my epoch` **under a row lock in that same transaction** (or by including the epoch in the conditional UPDATE), so a takeover that increments the epoch cannot interleave between a plain SELECT and the commit. A stale epoch aborts before metadata commit. A new owner increments the epoch only after acquiring the lease and Tree OS lock.
- **Tree local OS lock:** the Tree content root is also held with an OS exclusive lock (flock/LockFileEx) for the process lifetime. Tree is local-disk-only (A3), so this prevents a second process on the same host from publishing to the same physical tree even during a DB failover/lease-loss window. If the OS lock cannot be acquired, the Tree process refuses to start.
- **Runtime monitor:** a goroutine watches the lease connection/heartbeat. On loss or unconfirmed ownership: immediately set `/readyz` false, stop accepting new operations, cancel the server root context, and abort in-flight publishes at their next safe point (before a physical move or before metadata commit). If the process cannot abort/exit promptly, it exits hard; it must not keep accepting work.
- **In-flight publish:** the process checks the lease/epoch before each physical publish and again in the metadata transaction. A physical move already performed after a lease loss is recovered by the journal on the next owner (or by the same process if it still holds the OS lock); readers are gated by readiness.
- **Blob vs Tree:** the epoch protects Blob metadata commits across hosts; Blob physical writes are content-addressed and harmless orphans. Tree adds the OS lock because its physical final names are shared mutable state. A shared/NFS Tree root is outside A3 and out of scope.
- **Second owner after takeover:** it runs recovery for non-terminal operations, increments the epoch only after acquiring the lease and OS lock, and refuses to publish if any journal/OS-lock state conflicts.

Failure matrix to test: lease connection killed; DB failover; heartbeat timeout; stale connection still open; old process paused/stopped but not exited; two starters; graceful shutdown; crash during publish before/after epoch check; Tree OS lock contention; Tree lock released only after the process can no longer publish. Every case must end in "one writer or no writer", never two writers to the Tree final namespace.

## 7. Trash and version lifecycle

**Tree trash without content separation is not a restorable trash.** `tx.Trash` only changes rows; the bytes stay at the live path, so a later PUT at the same name replaces them. The existing metadata-only `Restore` then returns a node with someone else's bytes.

Recommended target:
- On DELETE, move the file/directory bytes to an internal trash namespace keyed by node ID and a generation (`<internal>/trash/<nodeID>/<generation>/...`), and record the reference.
- On RESTORE, conditionally move them back to the original name; if the name is taken, refuse with a clear conflict (already the metadata rule).
- On PURGE, delete the content reference and the rows in one journaled operation; a failure reports failure.
- For tree versions, the same with `<internal>/versions/<nodeID>/<version>/...`; if the product does not support version content in the first tree release, keep the current honest `409` and hide restore.
- Blob trash/versions already use stable refs; the journal and revision still apply.

If the owner does not want to fund content lifecycle for the tree layout, the tree profile must **not expose** restore/purge/version-restore as if they work. The UI/API must say the capability is unavailable; that is a product scope reduction, not an implementation detail.

**Minimum regardless of the product choice:** Phase 2A's content relocation is required before no-replace publish. Even if Tree Trash is withdrawn, DELETE must still move/remove the live bytes under a journal so the path is physically free and the old content is either retained by policy or deleted deliberately. "Metadata-only delete" is not compatible with a no-replace live namespace.

### 7.1 Legacy trash backfill (D3)

Existing tree trash rows predate content references. Their old bytes may be at the original path, overwritten by a newer live node, or gone. The backfill is a **classifier and protector**, not a blind mover:

```
for each trashed tree root:
  if any live metadata node resolves to the old path:
      mark LEGACY_CONFLICT / CONTENT_UNRECOVERABLE
      DO NOT touch, move, delete, or quarantine the physical path
  else if the physical path exists and node identity + size/hash/content mapping can be proven:
      move to the internal trash namespace under a journal; mark CONTENT_OK
  else:
      mark CONTENT_UNRECOVERABLE (or leave in an operator-visible quarantine;
      never overwrite another path, never delete)
```

| Classification | Restore | Purge | Physical action |
| --- | --- | --- | --- |
| `CONTENT_OK` | allowed; moves content back | Phase 5 | owned trash namespace |
| `LEGACY_CONFLICT` | refused with a specific reason | Phase 5 only after operator review | none; live bytes untouched |
| `CONTENT_UNRECOVERABLE` | refused | rows may be retired by policy | none; no fabricated bytes |
| `QUARANTINED` | refused | operator decision | isolated copy/path, never a live path |

Backfill is idempotent and crash-safe: a completed row is never re-moved; a partially moved row is recovered from the journal; a live node is re-checked under the **same writer fence** immediately before any move (G1), so a competing PUT/MOVE cannot create a live node between the check and the move. **The Phase 2B gate must not require that every historical trash byte was recovered.** It requires that every trashed row is classified, that no backfill action touched a live-owner path, and that any remaining unproven physical name is surfaced as an occupied/needs-operator conflict rather than treated as recovered. An operator tool can later inspect/quarantine it; the server never path-blind deletes it.

## 8. Phased implementation plan

No phase starts before this ADR is accepted. Each phase preserves the current green suite and adds tests that fail before it.

**Phase 0 — Freeze and red tests (no behaviour change).**
- Record baseline and back up the worktree (Appendix A).
- Add deterministic negative tests for: LOCK-success-then-failed-COPY, PROPPATCH (already), Tree COPY vs concurrent PUT, Tree PUT stale If-Match, Tree MOVE vs concurrent PUT, live staging addressability, trash restore after name reuse, purge bytes, batch/DAV parity.
- Add the review probes as first-class red tests: (1) reader gap — GET/HEAD/Range after `old→backup` and after `staged→final` but before metadata commit; (2) `DELETE → same-name PUT` with Tree no-replace; (3) Tree stale `If-Match` PUT returning 412 while the old bytes must remain readable; (4) Phase 2A simulation where DELETE moves bytes and REST Restore still returns 200 while download 404; (5) legacy trash row + new live row sharing a path, backfill must not move the live bytes; (6) G1: new DELETE/backfill vs an unfenced legacy PUT/MOVE, where the legacy writer must be prevented from reporting success before being undone; (7) G2: a live-looking child under a simulated staged root must be refused by path and by guessed direct ID.
- Add a test-only interleaving harness (existing `interleavingMkdir`/`interleavingOpen` pattern) shared by DAV/REST/batch.
- Decide A1-A8 with the owner; write the support matrix.
- Exit: red tests demonstrate the class; no production code changed.

**Phase 1 — Primitives and journal.**
- Migration: node revision, operations, content refs (tables may start empty), staging visibility, lease epoch.
- Store methods: `BeginOperation`, `UpdateOperation`, `PublishTX` (node visibility + revision + audit + journal in one tx), `TrashIfRevision`, `RestoreIfRevision`, CAS; `EffectiveExternalVisibility` walked from the node to the user root (G2).
- Storage: `OperationStore`, no-replace publish for tree (platform files), startup capability probe; `DeleteEmpty`; `OpenLive` and the shared read fence.
- Acquire and enforce the **PostgreSQL singleton lease** before readiness; add the startup fail-closed test for a second process. Add the **runtime lease monitor**, Tree OS exclusive lock, and lease-epoch check in every publish transaction (D4, §6.6).
- Journal durability: fsync/flush and directory-sync helpers; write the crash-state recovery table into executable tests.
- Conformance tests: memory/PG revision/CAS/journal; blob/tree staging; no-replace always fails cleanly if the target exists; read fence blocks the intermediate; lease loss fails closed and a stale epoch cannot publish; nested staged child rejected by `EffectiveExternalVisibility` for both path and ID.
- Exit: primitives pass, unused by handlers; a second process cannot start; losing the lease stops a running process before another can publish.

**Phase 1R — Migrate every byte route to `OpenLive` and fence/reject every legacy writer (D1 + G1 gate).**
- Inventory and convert all routes in §4.5: DAV GET/HEAD/Range, REST download/preview, share download/thumbnail, file thumbnail, archive members, version content, content-origin routes, and any new byte route.
- Extend the byte-route inventory test: every byte route is classified as `OpenLive` or explicitly refused for Tree content; an unclassified route fails the build/test.
- **G1 writer inventory:** list every mutation entry point (DAV, REST, batch, upload/share-drop, restore, workers) and either put it behind the hierarchical fence for its whole physical+metadata transition or reject it by the migration feature gate. The checklist is part of the Phase 2A Entry evidence; no Phase 2A physical operation is allowed before it is green.
- Add GET/HEAD/Range interleaving tests around a simulated Tree DELETE physical move and a replacement publish; add cross-entry negative tests: new DELETE/backfill vs a legacy PUT/MOVE/restore, and a legacy writer that bypasses the fence must be prevented/rejected.
- Exit: no byte route remains on the old direct-open path; no legacy writer can mutate the same Tree namespace without the fence; unmigrated/unsupported routes return a defined refusal, not a stale-path 500/404.

**Phase 2A — Tree DELETE content relocation + minimal real Restore (gate before any no-replace publish).**
- **Entry gate (D1 + G1):** Phase 1R must have passed for every byte route, and the G1 writer inventory must show that **every** legacy mutation entry point is either fenced for its whole physical+metadata transition or rejected by the feature gate. There is no Phase 2A physical DELETE/backfill while a legacy DAV/REST/batch/upload/share/restore/worker writer can bypass the fence.
- DELETE takes the exclusive read/write fence before moving bytes, so no reader can resolve/open the old path and no fenced writer can interleave. The physical move and the metadata trash commit are one journaled transition.
- On DELETE, move the file/directory bytes to the internal trash namespace and record the content reference, journaled. The live path is physically free afterwards.
- **Restore in this phase (D2, Option 1):** implement the minimal real physical restore: conditional `trash → live` move + metadata restore/CAS/audit under the fence. If the name is taken, refuse; never return 200 while bytes remain in trash. Alternatively, if this cannot ship, **disable every Tree restore/purge entry point (REST, WebDAV if any, UI, worker) with a clear unsupported/conflict response**; there is no third "metadata-only success".
- **Purge:** disable Tree purge in this phase (leave rows/content in the trash namespace, non-destructive) until the Phase 5 tombstone/GC. A metadata-only purge is not allowed.
- **Legacy trash backfill (D3 + G1):**
  - The backfill's "no live metadata owner" check, the conditional name/ownership check, and the physical move must run under the **same writer fence** as a competing PUT/MOVE/restore. A check followed by an unfenced physical move is a TOCTOU and is not allowed; if the name becomes live between check and move, the backfill aborts and reclassifies.
  - If any live metadata node occupies the old path, **do not move, delete, or quarantine the physical bytes**; classify the old trash row `LEGACY_CONFLICT`/`CONTENT_UNRECOVERABLE` and make restore refuse.
  - If no live node occupies the path, verify node identity, size/hash or a recorded content mapping before moving; if it cannot be proven, leave the path for manual inspection (quarantine only into an operator-visible area, never by overwriting another path).
  - Backfill is idempotent across crash/restart; a row already classified unrecoverable is never re-examined destructively.
  - The Phase 2B gate checks live-file physical consistency, **not** that every historical trash byte was recovered.
- Test: `PUT reuse.txt → DELETE → Metadata.Resolve not found → Tree.Content.Stat must be not found → PUT same name succeeds`; `DELETE → Restore → GET original bytes`; `legacy trash + new live same name → backfill leaves live bytes untouched and marks the old row non-restorable`; `new DELETE/backfill vs a legacy unfenced PUT/MOVE → the legacy writer is fenced or rejected, never a post-success 404`.
- Exit: DELETE releases the live path, restore returns the original bytes or is explicitly refused, purge is disabled for Tree, backfill has not touched any live-owner path, and the G1 inventory is green.

**Phase 2B — PUT / upload / share-drop publish.**
- Route all file creation/replacement through staging + publish, **after** the Phase 2A gate passes and the backfill is complete.
- G1 is already closed at Phase 2A Entry. Any newly introduced mutation entry point must join the fence before it can be enabled; Phase 2B does not postpone or relax the legacy-writer gate.
- Evaluate preconditions/locks/revision before the physical publish; tree never writes the final path before that.
- Add recovery for `PUBLISHING` states and staging GC.
- Exit: stale If-Match no longer overwrites; concurrent PUT vs PUT is one winner; DELETE→same-name works; a legacy MOVE and a new PUT contending on the same path are serialized; all previous tests green.

**Phase 3 — COPY (DAV and batch).**
- Replace the recursive live copy with hidden staging and a directory publish.
- Delete `createdNode`/`rollbackCopy` and the dead-property guard; they become unused. Keep the regression tests as end-to-end scenarios (they must pass through staging).
- Unify DAV and batch on the same copy engine; no partial live destination.
- **G2 tests:** while a directory COPY is staged, guess/obtain a staged child ID and exercise every direct-ID route (download, thumbnail, share, ACL, version content, search, admin/listing, audit). Each must call `EffectiveExternalVisibility` and refuse; after publish the same IDs become visible. Also verify a path under the staged root is not listable/resolvable.
- Exit: LOCK/PROPPATCH/PUT/MOVE during a COPY cannot be revoked; no staging artifacts after success/failure; completeness/413 behaviour unchanged; no staged child is reachable by ID.

**Phase 4 — MOVE/rename, DELETE/restore, LOCK/PROPPATCH/share/ACL fencing.**
- Short fences, revision CAS, lock checks inside the same metadata transaction.
- PROPPATCH gets its missing audit row; share/ACL/lock mutations bump revision.
- Retire all snapshot exceptions; the resource fence is the rule.
- Exit: every prior rollback counterexample is structurally impossible; ordinary MOVE/DELETE/restore behavior unchanged.

**Phase 5 — Tree content lifecycle.**
- Trash/version internal namespaces, content refs, restore/purge/version operations, worker GC/recovery.
- **Purge gate (D5):** implement the `PURGING` tombstone state: a metadata transaction marks the trash root non-restorable with its content reference and operation ID; physical deletion is idempotent against that reference; rows/audit complete only after physical deletion. A crash at any point is a defined non-restorable retry state, never a "restorable row with deleted bytes".
- If A4/A5 say no: remove/hide the features with explicit unavailable responses and update the support matrix.
- Exit: restore returns the original bytes after name reuse; purge removes the bytes it reports; a purge failure leaves a non-restorable tombstone, not a false success; no false success.

**Phase 6 — Multi-instance and production evidence.**
- Decide whether the fence backend becomes PostgreSQL advisory/lease (if A1 is relaxed).
- Real PostgreSQL 17 concurrent tests, real S3 for blob, native DAV clients, crash/restart drills, staging/backup/restore drills, performance and disk-overhead measurements.
- Exit: a fixed commit, real-environment evidence, and a release decision.

Ordering rationale: primitives first; **Phase 1R migrates every reader before Phase 2A moves any bytes**, because a physical DELETE with old readers would reproduce the read gap; Tree DELETE relocation and minimal restore (2A) come before any no-replace publish because a trashed name still occupies the physical path; PUT before COPY because it exercises publish/precondition/recovery on the simplest path; COPY then removes the rollback class; fencing of side tables after the publish engine exists; lifecycle last because it is independently scoped by product.

## 9. Regression matrix

| ID / prior finding | Layout | Current status | Target test / phase |
| --- | --- | --- | --- |
| P0-DAV-COPY-001/002 completeness, 413 over budget | Blob/Tree | Fixed (`copycomplete_test.go`) | keep; Phase 3 re-run |
| NC-R6-001 ignored Mkdir | Blob/Tree | Fixed (`dircommit_test.go`) | keep; Phase 2/3 publish path |
| R7 destructive cleanup / winner directory | Tree/Blob | Fixed (no cleanup, tests) | keep; staging makes it moot |
| R8-DAV-001 name-based rollback | Blob/Tree | Fixed | Phase 3 replaces rollback; test must still pass |
| Concurrent PUT on a created file | Blob/Tree | Fixed by snapshot; interim | Phase 3: published result cannot be rolled back |
| Concurrent MOVE of a partial copy | Blob/Tree | Fixed by snapshot; interim | Phase 3 |
| PROPPATCH on a created file | Blob/Tree | Fixed by interim dead-prop guard | Phase 4: revision/fence; remove exception |
| LOCK on a created file | Blob/Tree | **OPEN** (R11) | Phase 4 must pass; Phase 3 staging already prevents the scenario |
| Tree COPY overwrites concurrent PUT bytes | Tree | **OPEN** | Phase 2B/3: no final write before publish |
| Tree PUT stale If-Match overwrites old bytes | Tree | **Independently reproduced** (412, old ETag, new bytes) | Phase 0 red, Phase 2B |
| Tree MOVE metadata→physical window | Tree | Source-confirmed, no test yet | Phase 0 red, Phase 4 |
| Reader sees intermediate during replacement | Tree/Blob | **Independently reproduced** (500 after old→backup; new bytes + old ETag after staged→final) | Phase 0 red, Phase 4.2/4.5 |
| DELETE leaves live Tree path, blocking same-name no-replace | Tree | **Independently reproduced** (Meta not found, Content.Stat exists) | Phase 2A gate |
| Directory MOVE vs child PUT (fence conflict) | Tree/Blob | Source-confirmed protocol gap | Phase 0 red, Phase 4/6.3 |
| Live overwrite staging is addressable | Tree/Blob | Source-confirmed | Phase 3/4 |
| Trash restore after name reuse | Tree | **OPEN** (product) | Phase 2A/5 or disable |
| Purge leaves tree bytes | Tree | **OPEN** (product) | Phase 5 or relabel |
| Tree version content | Tree | Product scope; honest 409 | A5 decision |
| Batch new-copy partial vs DAV rollback | Blob/Tree | Uneven | Phase 3 unified engine |
| Lock token checked in every write | Blob/Tree | Handler checks only; not in Trash/rollback/worker | Phase 4 fence |
| COPY/RESTORE destination-parent lock | Blob/Tree | Source-confirmed missing on new-child COPY and restore | Phase 0 red, Phase 4 |
| PROPPATCH audit row | Blob/Tree | Missing | Phase 0/4 (small independent fix) |
| Second process starts under single-process contract | Both | No guard today | Phase 1 singleton lease |
| Platform no-replace/durability | Tree | Direction only; not tested per platform/volume | Phase 1 probe + Phase 6 |
| Crash at each publish transition | Both | No journal today | Phase 1 state-machine tests |
| Direct-ID staging leak | Both | No staging today; routes must be audited | Phase 1/4 inventory test |
| Nested staged child reachable by guessed ID | Both | Design-only G2 counterexample | Phase 1 helper, Phase 3 route tests |
| Legacy writer bypasses DELETE fence | Tree | Simulated: legacy PUT 204, then new DELETE 204, then GET 404 | Phase 1R G1 gate + Phase 2A cross-entry tests |
| Backfill check-to-move TOCTOU vs same-name PUT | Tree | Simulated: backfill moves path after new PUT 201; GET 500 | Phase 2A G1/fence test |
| Phase 2A starts before all writers are fenced | Tree | Plan conflict (gate was in Phase 2B) | Phase 2A Entry checklist |
| Unmigrated reader during Phase 2A DELETE | Tree | Simulated: DAV GET 500 / REST GET 404 before metadata commit | Phase 1R gate + Phase 2A interleaving |
| REST Restore returns 200 while bytes remain in trash | Tree | Simulated: restore 200 then download 404 | Phase 2A minimal restore or disable |
| Legacy trash backfill moves a live owner's bytes | Tree | Simulated: old trashed row and new live row share a name; live bytes served | Phase 2A classifier + red test |
| Runtime lease loss with in-flight publish | Both/Tree | Not implemented; startup-only design was insufficient | Phase 1 monitor/epoch/OS-lock matrix |
| Purge deletes bytes before a durable tombstone | Tree | Not implemented | Phase 5 tombstone gate |
| `OpenLive` pins metadata with handle | Both | Design only | Phase 1R header/Range tests |
| Fence key identity vs mode | Both | Design only | Phase 1 fence conformance |

All rows run on both layouts where the feature exists; "Tree only" tests must still run the Blob counterpart to prove no regression.

## 10. Testing requirements

- **Deterministic interleavings** for every documented race: use test-only hooks in the storage/metadata boundary, as the current `interleavingMkdir`/`interleavingOpen` tests do. No `time.Sleep`-based races. Include the directory-MOVE-vs-child-PUT case and the reader-gap case at both physical transition points.
- **Read-side fence tests**: a reader that resolves before the exclusive fence gets the old complete bytes; one that resolves after gets the new complete bytes; neither gets 500 or new bytes with old metadata. Test an already-open handle across a replacement on Unix and Windows, including Range reads.
- **Fault injection** at each boundary: before staging, after staging, during physical publish, before/after metadata commit, during journal update, during cleanup. Each asserts the invariants and the exact visible state.
- **Crash/restart**: kill the process at each `PUBLISHING` boundary; start recovery; assert no live metadata without bytes, no bytes without a documented orphan, staging eventually collected, retry idempotent, and readiness stays false until recovery/quarantine.
- **Singleton runtime**: lease connection drop, DB failover/restart, heartbeat timeout, stale connection, old process paused, two starters, Tree OS lock contention, in-flight publish at lease loss, epoch mismatch; every case ends in one writer or no writer.
- **Read-route gate (D1)**: every byte route is inventoried; an unmigrated route fails the inventory test; interleaved GET/HEAD/Range at both DELETE/replace physical transitions returns old/new complete bytes or a defined refusal.
- **Legacy-writer gate (G1)**: every mutation entry point is inventoried and either fenced or rejected; a new DELETE/backfill racing a legacy PUT/MOVE/restore must not allow the legacy writer to report success before being undone; the backfill's check-and-move runs under the same writer fence.
- **Effective visibility (G2)**: a live child under a staged root is refused via path and via guessed direct ID on every ID-taking route; after publish the same node becomes visible. The test uses a nested staging tree, not only a single staged row.
- **Trash migration (D2/D3)**: DELETE→Restore returns original bytes; restore name conflict refuses without 200; purge disabled for Tree before Phase 5; legacy same-name trash backfill leaves live-owner bytes untouched and marks the row non-restorable; crash/restart mid-backfill is idempotent.
- **Purge tombstone (D5)**: crash after tombstone before physical delete, during delete, after delete before row removal; each state is non-restorable and retryable, never a restorable row with missing bytes.
- **Store conformance**: memory and PostgreSQL 17 for revision CAS, journal, trash/version content refs, purge; PG tests must run against a real instance, not a fake. Include a second-process singleton-lease test.
- **Storage conformance**: blob (local + S3 fake, then real S3) and tree (real local disk) for staging, no-replace, replace, cleanup, permissions, path safety, symlink escape. Platform matrix: Linux ext4/xfs/btrfs/tmpfs (renameat2 support), Windows NTFS with open handles, casefold/Unicode collisions, same-volume constraints, fsync/flush and power-loss semantics.
- **HTTP/protocol**: WebDAV clients for PUT/COPY/MOVE/LOCK/PROPPATCH; REST/batch/share uploads; the same operation through two entry points must produce one rule.
- **Security**: staging and internal namespaces unreachable through every route (GET/PROPFIND/resolve/share/ACL/thumbnail/download/version/search/admin), including ID-taking routes; path traversal; authz; audit coverage.
- **Performance/space**: 10k-node COPY, 10 GB file, staging/backup overhead, publish latency, read-fence impact on GET/Range, recovery scan cost; document budget.
- **Backup/restore**: the journal and permanent content (including enabled trash/version bytes) are included or a documented clean checkpoint is used; restore drill verifies no journal-referenced staging is missing and no live node lacks bytes.
- **Compatibility**: all existing tests; migration of a tree deployment with existing trashed/version rows (no content refs) must fail safe (restore refused or best-effort with a clear warning, never silent wrong bytes).

## 11. Owner decision: approved v1 support matrix

**Owner decision (2026-10-24): approved the full proposed matrix below.** This is a product-scope approval, not a production GO; each phase still needs its engineering and environment gates. The approved scope is the basis for Phase 0/1.

| Item | Approved v1 decision | Why |
| --- | --- | --- |
| A1 single active process | **Yes**, enforced by the PostgreSQL singleton lease, runtime monitor, epoch fence and Tree OS lock | Matches D-09 and avoids cross-instance lock complexity. |
| A2 external Tree writes | **Not supported**; independent read/backup only | Matches D-07; scanning/import is a later product. |
| A3 Tree storage | **Local disk**, no S3 Tree | Keeps the Blob+S3 / Tree split. |
| A4 Tree Trash restore/purge | **Real bytes**; Phase 2A ships minimal real restore or disables all Tree restore/purge; purge becomes real from Phase 5; no metadata-only success | A cloud drive's delete/restore is a core expectation. |
| A5 Tree version content | **May be unsupported in v1** with an honest `409` | Current UI/API already honest; prioritize correctness. |
| A6 DAV LOCK | **Token/Depth enforced**, including the same-holder rule, in parallel with the server-side operation fence | A lock without token enforcement is worse than none. |
| A7 Crash / singleton | **Recover or safe-unavailable**, never wrong bytes; runtime-enforced single active process | The product already promises data reliability; startup-only locking is insufficient. |
| A8 Staging | **Same filesystem**, probed per real volume | Short publish and rename semantics. |
| Backup | Committed live + enabled trash/version bytes + metadata + journal, with a consistent restore point | A restore must rebuild files and index together. |

The remaining engineering parameters are delegated to Phase 1/5 with the defaults in this ADR: staging GC/quota, `NEEDS_OPERATOR` UX, legacy trash classification as non-restorable when unproven, and the future multi-instance path after v1. A5's version-byte deferral is approved; the API/UI must continue to report it honestly.

### Resolution log

| Item | Resolution |
| --- | --- |
| A1/A2/A3/A6/A7/A8 | Approved as proposed. |
| A4 | Approved: Phase 2A minimal real restore or explicit refusal; Phase 5 real purge/tombstone. |
| A5 | Approved: Tree version bytes may remain deferred with `409` and honest UI. |
| Backup | Approved: consistent recovery point includes metadata, journal, committed live content and enabled trash/version content. |
| Items 4-6, 9 | Engineering parameters; Phase 1/5 documentation must set and test the chosen defaults. |

## 12. Consequences, risks, and compatibility

**Positive.** The rollback class is eliminated structurally. Partial results are invisible. Locks/PROPPATCH/shares can no longer be revoked by a failed COPY. Tree bytes are never overwritten before a conditional decision. Lifecycle operations become truthful.

**Costs/risks.**
- New schema and storage primitives; a migration and a recovery worker.
- Staging space and backup space; tree COPY already copies, but a failed long copy leaves staging until GC.
- Publish/rename platform support must be probed; refuse to start rather than degrade. The read fence adds a short lock acquisition to every content open and must be measured against GET/Range throughput.
- Windows open-handle behavior can force publish retries or refusals; supported-volume probes and client tests are mandatory.
- Existing uncommitted work (`treewalk`, `swap`, `dircommit`, regression tests) must be preserved; the phase plan reuses them.
- Multi-instance remains unsupported unless the fence moves to a shared backend; the singleton lease makes that explicit and fail-closed.
- Existing tree trash/version rows cannot be made restorable retroactively without their bytes; the Phase 2A/5 migration must fail safe.
- A global readiness gate trades availability for correctness during recovery; per-resource quarantine is a later refinement.

**Compatibility.** Blob behaviour is preserved (content-addressed, no path race) and gains the same journal/visibility. Tree becomes more conservative: external writes remain unsupported, staging is excluded from every API listing, and the backup treatment of the staging/trash/version namespaces is decision 4. Restore/purge are either real or unavailable.

## Appendix A — Working-tree baseline

Recorded before this ADR was written; no production source was modified by this task.

| Item | Value |
| --- | --- |
| Git HEAD | `6ed3f92b16349447f7443b95bd250b41acd1f0e8` |
| Tracked diff | 40 files, +4211/-818 |
| `git diff` SHA-256 | `7F52F62A853237F24E4EEA4F8205518CF978D5EA42FB74152A79EA68C8CD6917` |
| `git status --porcelain` SHA-256 | `17E5259C7749483B76B360045936EFC26DCD261E6C32B6176C9F7BA16048098B` |
| Untracked source paths | 20 under `internal/`, 3 under `docs/` (plus unrelated local artifacts: `design-studio/`, `cirrus-ui/`) |
| Backup | `C:\Users\admin\Downloads\newCloud_worktree_baseline_2026-10-24.zip` (64 entries: 61 project-file snapshots + `baseline/tracked.diff`, `status.txt`, `untracked.txt`), SHA-256 `2AD54892DFD3265378875E41C21F8CFA91B04E22401DFE7D5995E26F6EEA9BAF` |
| Baseline tests | `go test ./... -count=1` PASS (dav, server, store, swap, treewalk, dircommit included) |
| Independent baseline check | The review read all 64 ZIP entries and confirmed the embedded diff/status hashes match this appendix. |

The backup deliberately excludes the 1.1 GB `design-studio/` tree and other unrelated local artifacts; those were not part of the uncommitted source changes needed to reproduce the fixes. This ADR, its README/architecture links, and the note at the top of `tree-storage-consistency.md` were added after the baseline; no product source or test was modified by this architecture task.

## Appendix B — Evidence map

- Rollback snapshot: `internal/dav/move.go` (`createdNode`, `rollbackCopy`), `internal/dav/copyownership_test.go`.
- Copy physical ordering: `move.go:619/710` (Put before `CreateFile`), `internal/storage/tree/tree.go` (`Put` rename, `Move` rename, `Delete` RemoveAll).
- PUT precondition ordering: `resource.go:319` vs `:340`; independent stale-If-Match Tree probe reproduced 412 with new bytes.
- Move ordering: `internal/swap/relocate.go:82` (metadata) vs `:122` (content).
- Reader gap: independent probe after `old→backup` (500) and after `staged→final` before metadata (new bytes + old ETag).
- DELETE/no-replace: independent probe `PUT → DELETE 204 → Meta not found → Content.Stat exists`.
- G1 legacy-writer gate: independent simulation of a physical DELETE move with an unfenced legacy PUT returning 204, then DELETE, then GET 404; the gate is required before Phase 2A.
- G2 effective visibility: design test with a live child under a simulated staged root, guessed by direct ID; the ancestor-chain check must refuse it until publish.
- Platform primitives: Linux `renameat2(RENAME_NOREPLACE)`, Windows `SetFileInformationByHandle`/`MoveFileExW` without replace, macOS `renamex_np(RENAME_EXCL)`; none is `os.Rename`.
- Uploads: `files.go:486`, `uploads.go:202`, `sharepages.go:477`.
- Trash/restore: `files.go:798/911`, `dav/resource.go:471`; lifecycle `store/lifecycle_memory.go`, `lifecycle_pg.go`; worker `worker.go:309`.
- Locks: `dav/lock.go:112`; `dav/move.go` rollback has no lock check.
- PROPPATCH: `dav/move.go:107` (no `tx.Audit`).
- Staging in overwrite: `internal/swap/swap.go` (`StagedName`, `Dispose`, physical moves).
- Completeness: `internal/treewalk`; directory creation: `internal/dircommit`.
- Deployment assumptions: `functional-requirements.md` D-07/D-09; `architecture.md`; `deployment.md`.
