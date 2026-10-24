# Tree layout storage consistency: current contract and open decisions

> **Architecture status:** the target architecture is [ADR-0013](../adr/0013-file-operation-consistency.md) (Accepted v2.1.1; Phase 0/1 in progress, not implemented in production). This file remains the evidence and defect list until the gated phases land; it is not a substitute for the decision record.

This is a decision record, not a claim that the tree layout is production ready. It exists because
the 2026-10-24 architecture review asked for the correctness boundary to be written down instead of
being inferred from whichever handler was last patched. Nextcloud is a research reference only: no
part of this document requires matching its internal architecture.

## What is guaranteed today

| Area | Guarantee | Evidence |
| --- | --- | --- |
| COPY/archive completeness | A transfer either covers the whole resolved subtree or fails before creating a destination. The traversal is independent of `limits.max_profind_children`. | `internal/treewalk`, `internal/dav/copycomplete_test.go` |
| Directory creation | The content store is written before the metadata node and its success audit row. A physical failure produces no live node; a metadata failure retains the path and never deletes it. | `internal/dircommit`, `internal/dav/dircommit_test.go`, `internal/server/dircommit_test.go` |
| COPY rollback ownership | A failed directory COPY removes only nodes it committed, and only while their parent, name, size, ETag and mtime still match the creation snapshot and their ancestor chain is unchanged. A concurrent PUT, rename or MOVE is left alone. A node with dead properties (a successful PROPPATCH) is also left alone by an interim guard. A LOCK is **not** covered; see open item 1. | `internal/dav/copyownership_test.go` |
| Overwrite staging | A COPY over an existing destination is staged under a reserved sibling name and switched in one metadata transaction. A failed overwrite preserves the original's bytes. The staging name is live/addressable during the operation; ADR-0013 replaces it with private staging. | `internal/swap`, `internal/dav/treelayout_test.go` |
| Tree version honesty | The tree layout records version rows but retains no old bytes; version content and restore-version answer `409 no_version_content`, and the UI does not offer a restore that cannot work. | `internal/server/versioncontent_test.go` (`TestATreeLayoutRefusesToRestoreAVersionItCannotRestore`), `docs/guides/limitations.md` |

## Open P0 / release blockers (as of 2026-10-24)

1. **A failed COPY can still undo a metadata-only success, including a WebDAV LOCK.**
   The reproduced cases are PROPPATCH (dead property) and LOCK. `rollbackCopy` now skips a node with dead properties, which closes the PROPPATCH case, but a successful LOCK changes only the lock table and is not covered. ACL grants, shares and any future side table have the same gap. The architectural answer is operation visibility (partial copies are not addressable) or a store-wide revision/fence; adding one field per table keeps the same class.
2. **Tree COPY can overwrite a concurrent winner's bytes before its metadata conflict is detected.**
   `copyResolved` writes file bytes to the final path before `tx.CreateFile`. A concurrent PUT that
   already committed a node at that path can lose its bytes when the COPY's `Content.Put` runs and
   the COPY's metadata insert then fails. This is not fixed by the rollback ownership rule, and it
   is the reason no tree release candidate exists yet.
3. **Tree PUT with stale `If-Match` overwrites the old bytes while returning 412.** The independent ADR review reproduced this on the real tree store: new bytes, old ETag. Blob is unaffected.
4. **A reader can observe the replacement window.** The ADR review reproduced GET returning 500 after `old→backup`, and new uncommitted bytes with the old ETag after `staged→final` before metadata commit.
5. **Tree DELETE leaves the live path occupied.** The ADR review reproduced metadata-not-found but `Content.Stat` still present; a same-name no-replace publish would fail. ADR-0013 Phase 2A gates this before no-replace publish.
6. **Tree trash restore loses the original bytes after the name is reused.** Trashing is a metadata
   operation; the bytes stay at the live path. A later PUT at the same name replaces them. A restore
   of the old node then succeeds in metadata but cannot read the old content.
7. **Tree purge removes metadata but not physical content.** `Tx.PurgeTrash` deletes rows; there is
   no tree-content collection step, so "permanently deleted" bytes may remain on disk.

Items 6 and 7 are release blockers **if** the tree profile keeps offering restore and permanent
deletion with those meanings. Reducing the tree profile (for example, disabling restore/purge for
tree until a content-retention design exists) is a product decision, not an implementation detail.

### Acceptance tests required before any of the above can be called fixed

- A COPY that has created `copied/a.txt`, then B's successful LOCK or PROPPATCH, then A's failure:
  the node must stay live, its bytes and dead property readable, its lock still valid, and no
  trash/audit row may undo B.
- A Tree COPY racing B's successful PUT at the same final path: B's bytes must remain the bytes the
  path returns after A's metadata conflict.
- A Tree PUT with a stale `If-Match`: 412 and the old bytes/ETag must remain.
- A GET/HEAD/Range during each physical replacement window: old complete bytes or new complete bytes,
  never 500 and never new bytes with old metadata.
- `DELETE → same-name PUT` on Tree: the second create must succeed (Phase 2A content relocation).
- All run on Blob and Tree where applicable, with the competing request injected deterministically.

### Phase gates added by the v2.1 review (design requirements, not current behaviour)

- **Phase 1R before Phase 2A:** every byte route (DAV GET/HEAD/Range, REST download/preview, share download/thumbnail, file thumbnail, archive, version content, content origin) must be on `OpenLive` or explicitly refused for Tree content before DELETE moves bytes. Otherwise a physical DELETE produces a stale-path 500/404.
- **Phase 2A restore truthfulness:** if DELETE moves bytes, REST Restore must either move them back or be disabled; the simulated `Restore 200 → download 404` is a failure. Tree purge stays disabled until the Phase 5 tombstone/GC.
- **Backfill:** a legacy trashed row whose old path is now owned by a live node is `LEGACY_CONFLICT`; backfill must not move the live bytes. Unproven rows are non-restorable.
- **Singleton runtime:** a PostgreSQL lease that is lost at runtime must fail-stop; a Tree OS lock and epoch fence prevent a second writer during the failover window.
- **G1 legacy writers:** before the first Phase 2A physical DELETE/backfill, every DAV/REST/batch/upload/share/restore/worker mutation entry point must be fenced for its whole transition or rejected. The Phase 2A backfill's check-and-move must share that writer fence; Phase 2B no longer defers this gate.
- **G2 effective visibility:** a live row under a staged root is not externally visible. Every direct-ID route must walk the ancestor chain via `EffectiveExternalVisibility`, not just check the target's `staged_by`.


## Product decisions proposed by ADR-0013 (owner confirmation required)

- Does the tree profile support external writes to the same root (an administrator editing files
  directly), and are they reconciled or merely tolerated?
- Is a tree root shared by more than one newCloud instance? This decides whether an in-process
  keyed lock is acceptable or a database/advisory lock is required.
- Does the first release include tree trash restore, purge, and historical version content? The
  support matrix must say so before those surfaces can be truthful.
- Which failure is acceptable when a request dies between two stores: an unreferenced file/directory
  (kept, reconcilable) or a metadata row without bytes (not acceptable for reads)?

## Candidate protocols (evaluation; ADR-0013 v2 proposes B + narrow A)

| Candidate | Sketch | Strength | Cost / open question |
| --- | --- | --- | --- |
| A. Operation locks + conditional writes | Serialize writes to a path across all entry points; check metadata ownership before the physical write. | Smallest conceptual change; fits single-instance deployment. | Must cover every write entry point and ancestor/subtree conflict; does not work across instances without a database lock; the two-store commit window and reader visibility remain. |
| B. Private staging + no-replace publish | Write bytes to an operation-private path, publish atomically only if the target is free, then commit metadata. | A failed COPY cannot touch a winner's bytes; the audit's private-staging candidate. | Needs a no-replace publish primitive, crash reconciliation, **and a read-side fence/open-handle contract** so GET does not observe the physical replacement window; extra space for large trees. |
| C. Stable content references + readable projection | Keep content by stable ID and project it to the human-readable tree. | Naturally separates live, version and trash content. | Projection/synchronisation, external modifications, space and scanning complexity; likely a larger redesign than the first release needs. |

The review's recommendation is to compare these with measured complexity and deployment evidence
before choosing. This record does not pick one, and does not claim cross-filesystem/database
atomicity for any of them.

## Verification boundary

All statements above are backed by tests over the in-memory metadata store plus the real local
blob/tree content stores, with failures injected through the storage interfaces. They are not
evidence for PostgreSQL 17 concurrency, real object storage, native WebDAV clients, or a
representative restore drill. The working tree is also still uncommitted; any release candidate
must first become a fixed revision.
