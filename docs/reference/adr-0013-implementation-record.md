# ADR-0013 implementation record

**Status:** Phase 0 complete (10 executable red tests); Phase 1 consistency infrastructure
implemented and the independent review's R12-001…R12-004 findings are patched with permanent
regressions. Phase 1R is not started and remains on hold pending independent re-review. Production
remains NO-GO.
**ADR:** [ADR-0013](../adr/0013-file-operation-consistency.md) — Accepted v2.1.1 on 2026-10-24
(SHA-256 `5A30AC0C6E0A21F404A372FCBDF1D31968575EE2FCE0EFF91B49151C1162F0F0`).
**Baseline:** Git `6ed3f92b16349447f7443b95bd250b41acd1f0e8` plus the uncommitted worktree.

## 1. Baseline and recovery checkpoint

| Item | Value |
| --- | --- |
| Git HEAD | `6ed3f92b16349447f7443b95bd250b41acd1f0e8` |
| Original backup | `C:\Users\admin\Downloads\newCloud_worktree_baseline_2026-10-24.zip` |
| Original backup SHA-256 | `2AD54892DFD3265378875E41C21F8CFA91B04E22401DFE7D5995E26F6EEA9BAF` |
| Phase 0 checkpoint | `C:\Users\admin\Downloads\newCloud_phase0_checkpoint_2026-10-24.zip` |
| Phase 0 checkpoint SHA-256 | `24C974CADBEEB2DD3A419E2FFDE40B09C181B36DA99969A51CC930F93F462EEB` |
| Phase 1 checkpoint | `C:\Users\admin\Downloads\newCloud_phase1_checkpoint_2026-10-24.zip` |
| Phase 1 checkpoint SHA-256 | `8C18ECFC93942A3F873DF7BF7FA05846B2EDEE43AB8DA04EA9A15C0B09FC38CF` |
| Phase 1 final checkpoint | `C:\Users\admin\Downloads\newCloud_phase1_final_checkpoint_2026-10-24.zip` |
| Phase 1 final checkpoint SHA-256 | `982BCC59A55BD80E9EE0364E79B27D87EE0A89E02890ED9349C2C01FE454FE22` |
| Latest checkpoint | `C:\Users\admin\Downloads\newCloud_phase1_R12_corrective_checkpoint_2026-10-24.zip` |
| Latest checkpoint SHA-256 | `1752D7BD5B937AD18FEF39D740953AF31FE591A098177FC8A18A8C785F33BA55` |
| Baseline `go test ./... -count=1` | PASS before Phase 0/1 edits |

The checkpoint contains the binary tracked diff, `git status --porcelain`, the untracked
file manifest, and every untracked project source/doc file except the unrelated
`design-studio/` and `cirrus-ui/` trees. The original ZIP is retained unchanged.

## 2. Owner decision

The owner approved the full v1 support matrix in ADR-0013 §11 on 2026-10-24:

- one active application process with runtime lease/epoch/Tree OS-lock enforcement;
- Tree local disk only, no external Tree writers, no S3 Tree;
- real Tree Trash restore/purge or an explicit refusal, never metadata-only success;
- Tree version bytes may be deferred with an honest `409`;
- DAV LOCK token/Depth enforcement;
- crash recovery or fail-closed;
- same-filesystem staging;
- consistent backup of metadata, journal, committed content and enabled trash/version content.

The ADR status and architecture index were updated to Accepted. This is not a production GO.

## 3. Phase 0 regression suite

The intentionally red tests carry the `phase0red` build tag, so the normal suite stays green.
The executable red set is **10 tests: 7 in `internal/dav` and 3 in `internal/server`**. Wider
negative scenarios are listed separately as deferred; they are not counted as current red tests.
Run them explicitly:

```powershell
go test -tags phase0red ./internal/dav -run TestPhase0 -count=1
go test -tags phase0red ./internal/server -run TestPhase0 -count=1
```

### DAV / Tree physical publication

| Test | Invariant | Interleaving / boundary | Current result | Owning phase |
| --- | --- | --- | --- | --- |
| `TestPhase0TreePutStaleIfMatchKeepsOldBytes` | A stale `If-Match` returns 412 and the old bytes remain | PUT writes content at `resource.go:319` before `checkIfMatch` at `:340` | RED: 412 then GET `"NEW"` | Phase 2B |
| `TestPhase0TreeCopyDoesNotOverwriteConcurrentPutBytes` | A COPY that loses the name race must not overwrite a committed PUT | B PUT runs immediately before A's `Content.Put` | RED: GET returns `"SOURCE BYTES"`, not B's bytes | Phase 3 (Phase 2B no-replace first) |
| `TestPhase0DeleteReleasesTheTreeLivePath` | DELETE must free the live physical path before a no-replace publish | after DELETE, `Tree.Content.Stat` | RED: Stat succeeds | Phase 2A |
| `TestPhase0ReaderNeverSeesTheReplacementWindow` | GET sees old complete bytes until metadata commit | simulated `old->backup` and `staged->final` | RED: 500 at the first point; new bytes at the second | Phase 4/read fence (Phase 1R prerequisite) |
| `TestPhase0FailedCopyDoesNotUndoAConcurrentLock` | A failed COPY cannot revoke a successful LOCK | B LOCKs a copied child while A fails on a later source read | RED: GET 404 after the rollback | Phase 4 (R11) |
| `TestPhase0LegacyWriterBypassesDeleteFence` | A legacy writer's successful PUT cannot be removed by the new DELETE | simulated Phase 2A physical move, legacy PUT, metadata DELETE | RED: GET 404 after B returned success | Phase 1R/2A (G1) |
| `TestPhase0BackfillMovesNewLiveBytes` | Backfill never moves a path a newer live node owns | check sees trash-only, new PUT wins, then physical move | RED: GET 500 | Phase 2A (G1/D3) |

### Server / lifecycle and parity

| Test | Invariant | Interleaving / boundary | Current result | Owning phase |
| --- | --- | --- | --- | --- |
| `TestPhase0TreeRestoreAfterReuseReturnsOldBytes` | Restore returns the deleted bytes or refuses | delete old, reuse+delete new, restore old | RED: 200 but GET returns `"NEW BYTES"` | Phase 2A |
| `TestPhase0TreePurgeLeavesUnreferencedBytes` | Purge removes the bytes it claims to purge | metadata purge after DELETE | RED: physical file remains | Phase 5 |
| `TestPhase0BatchCopyLeavesNoPartialDestination` | A failed directory COPY leaves no live partial destination, matching DAV | second child Put fails | RED: `dst/a.txt` remains live | Phase 3 |

### Deferred to their primitive

These cannot be exercised against current APIs without inventing a parallel model. They are
recorded here and will be added with the Phase 1/1R primitives rather than weakened:

- G1 legacy-writer fence / backfill TOCTOU: needs the fence and operation-aware backfill.
- G2 staged-descendant direct-ID access: needs `staged_by`/operation visibility in the store.
- Lease loss and crash transitions: needs the runtime lease and journal.
- Directory MOVE racing a child PUT: needs the hierarchical fence wired into MOVE.

The reproduced browser/DAV failures above are not claims that Phase 1 fixes them. They stay red
until Phase 1R/2A/3/4 pass their tests.

### Phase 0 red run (current evidence)

```text
$ go test -tags phase0red ./internal/dav -run TestPhase0 -count=1
--- FAIL: TestPhase0TreePutStaleIfMatchKeepsOldBytes
    after a 412 stale If-Match, GET = 200 "NEW", want 200 with the old bytes
--- FAIL: TestPhase0TreeCopyDoesNotOverwriteConcurrentPutBytes
    after COPY 405, GET dst.txt = 200 "SOURCE BYTES", want B's committed bytes
--- FAIL: TestPhase0DeleteReleasesTheTreeLivePath
    after DELETE, the live tree path still holds bytes (Stat err = <nil>)
--- FAIL: TestPhase0ReaderNeverSeesTheReplacementWindow
    after old->backup GET = 500; after staged->final GET = "NEW STAGED"
--- FAIL: TestPhase0FailedCopyDoesNotUndoAConcurrentLock
    after a failed COPY, the locked file is 404
--- FAIL: TestPhase0LegacyWriterBypassesDeleteFence
    after a successful legacy PUT, GET = 404
--- FAIL: TestPhase0BackfillMovesNewLiveBytes
    after the backfill race, GET = 500
```

```text
$ go test -tags phase0red ./internal/server -run TestPhase0 -count=1
--- FAIL: TestPhase0TreeRestoreAfterReuseReturnsOldBytes
    restored file = 200 "NEW BYTES", want "OLD BYTES"
--- FAIL: TestPhase0TreePurgeLeavesUnreferencedBytes
    purge removed metadata but left physical bytes
--- FAIL: TestPhase0BatchCopyLeavesNoPartialDestination
    a failed batch COPY left dst/a.txt live
```

These are expected red results. They are not skipped or inverted; a green result later
means the owning phase changed the protocol.

## 4. Phase 1 foundations implemented so far

### Coordination and visibility

- `internal/fence` — hierarchical `Node`/`Name` keys, shared/exclusive intents, canonical key
  ordering, no upgrades, cancellation, idempotent release. `Acquire` copies the caller's variadic
  slice before sorting and the release closure captures only that private copy (R12-001). Tests
  include directory-exclusive vs child-shared, opposite acquisition orders, duplicate intents,
  caller-slice mutation after Acquire, and race runs.
- `internal/consistency` — operation journal with the ADR state machine and CAS transitions;
  `EffectiveExternalVisibility` walking the whole ancestor chain (G2); node revision CAS; the
  `StagingStore` interface; `OpenLive`, which re-checks that the held fence plan covers the
  resolved node and its complete ancestor chain before authorizing/opening, and pins
  revision/ETag/size with the handle (R12-002); and `Recover`, which aborts INTENT, discards
  STAGING and quarantines PUBLISHING as `NEEDS_OPERATOR` instead of guessing a physical rollback.
  `Unresolved`/`HasUnresolved` keep that quarantine visible across restarts (R12-003). Memory and
  PostgreSQL implementations are present; PostgreSQL is exercised when `TEST_DATABASE_URL` is set.
- `internal/lease` — a singleton-lease contract with a deterministic memory implementation and a
  PostgreSQL session-advisory-lock implementation that increments `consistency_lease.epoch` and
  closes `Lost` when the dedicated connection fails. `FailStop` turns loss into a one-shot stop
  callback. Also a Tree OS exclusive lock (`flock`/`LockFileEx`) with a fail-fast second-owner test.
- **Startup wiring** — `cmd/newcloud` calls `acquireRuntimeProtection` before opening the
  listener: Tree processes take the OS root lock, PostgreSQL-backed processes take the singleton
  lease, and lease loss calls `srv.SetNotReady()` and cancels the signal context. Tests cover a
  second Tree process (`ErrTreeLocked`), a second PostgreSQL process (`ErrHeld`), `/readyz`
  returning 503 after `SetNotReady`, and an actual terminated PostgreSQL backend closing `Lost`.
  `openStorage` also runs `ProbeNoReplace` on the Tree volume before it is served. After storage
  opens, `recoverBeforeServing` applies Recover and refuses to start if any operation remains
  `NEEDS_OPERATOR`; that quarantine is persistent because `Unresolved` reads it on every restart.
- **Epoch fencing** — `PG.CheckEpochInTx` reads the lease epoch `FOR UPDATE` in the publishing
  transaction; `PublishBlob` requires `EpochSet` and calls it, returning `ErrLeaseStale` before
  visibility changes. No live handler supplies the epoch yet.
- `internal/storage/fsrename` — platform no-replace primitive: Linux `renameat2(RENAME_NOREPLACE)`,
  Windows `MoveFileEx` without replace, macOS `renamex_np(RENAME_EXCL)`, fail-closed otherwise;
  target-volume probe. Tested on the current Windows volume, including concurrent publishers.
- `internal/storage/tree/operation.go` — operation-private `.newcloud-stage/<opID>` staging,
  `StagePut`, `StageMkdir`, `PublishNoReplace`, `PublishReplace` with retained backup, `Discard`,
  and `ProbeNoReplace`. Stage paths are unreachable through a live address.
- `internal/storage/blobstore` staging — operation-scoped descriptors over immutable
  content-addressed bytes, `StagePut`/`StageMkdir`, conditional `PublishNoReplace`, and
  `PublishReplaceExpected` for safe replacement against a caller-observed ETag. The generic
  `PublishReplace` refuses to replace without that expectation. Memory and PostgreSQL publishers
  are tested; no live handler uses this path yet.
- `internal/store/blobpublish.go` — `PublishBlob` creates/replaces the metadata node, writes
  its audit row and moves the operation journal PUBLISHING→COMMITTED in one transaction. It
  refuses an empty operation id, a missing operation kind, and an operation whose owner/kind does
  not match; PostgreSQL also requires `EpochSet` and the current epoch (R12-004). The
  no-replace/ETag check is inside that transaction, not a storage-adapter Stat-then-Record.
- `internal/store/consistency_memory.go` / `consistency_pg.go` — journal, revision CAS and
  visibility methods on both stores. `Memory.SetStagedByForTest` gives the G2 tests a staged root.
- `migrations/0016_consistency.sql` — `nodes.revision`, `nodes.staged_by`, `operations`,
  `content_refs`, `consistency_lease` (additive; no handler reads them in Phase 1).

### Corrective patch after independent Phase 0/1 review

The independent review reported four API-invariant defects. All four now have permanent tests:

| Finding | Fix | Regression |
| --- | --- | --- |
| R12-001 — Release aliased caller Intent slice | `Acquire` deep-copies intents; Release captures the copy | `TestReleaseDoesNotDependOnCallerSliceMutation`, `TestAcquireDoesNotReorderCallerSlice` |
| R12-002 — OpenLive opened a node outside the planned fence | Plan carries expected node + ancestor chain; `OpenLive` re-checks coverage after Resolve and re-plans/refuses; resolver must set `ExternallyVisible` | `TestOpenLiveRejectsAStalePlanIdentity`, `TestOpenLiveRejectsAnAncestorChainChange`, `TestOpenLiveRejectsEffectiveVisibilityFalse` |
| R12-003 — NEEDS_OPERATOR forgotten across restart | Journal `Unresolved` query; `HasUnresolved`; startup refuses to serve while quarantine exists | `TestRecover...` restart assertion, `TestRecoveryBeforeServingRefusesPersistentQuarantine` |
| R12-004 — Blob publish could complete another operation without epoch | publish requires non-empty op id, matching owner/kind, PUBLISHING state; PostgreSQL requires `EpochSet` and current epoch | shared `CompleteOperation` conformance, PG missing-operation/missing-epoch/wrong-kind/epoch-stale tests |

The Tree no-replace probe now runs in `openStorage` before the Tree layout is served, so a volume
that cannot prove no-replace fails startup instead of enabling a weaker rename.

### Verification

| Check | Result |
| --- | --- |
| `go test ./internal/fence -race` | PASS |
| `go test ./internal/consistency -race` | PASS |
| `go test ./internal/storage/fsrename -race` | PASS |
| `go test ./internal/storage/tree` | PASS |
| `go test ./internal/storage/blobstore -run TestBlobStage` | PASS: staged bytes invisible, no-replace conflict, safe replace with expected ETag |
| `go test ./internal/store -run TestSharedSuiteCoversEveryStoreMethod` | PASS: `PublishBlob` exercised by the shared suite on both backends |
| `go test ./internal/store -run TestConformance/CompleteOperationIsTransactional` | PASS: Tx journal completion on both backends |
| `go test ./internal/store -run TestMemory` | PASS |
| `go test ./internal/lease -race` | PASS |
| `go test ./internal/consistency -run TestOpenLive -race` | PASS |
| `go test ./internal/consistency -run TestRecover` | PASS |
| `TEST_DATABASE_URL=... go test ./internal/store -run TestPG` | PASS on PostgreSQL 17.11 and 18.6 (includes atomic journal and epoch-fenced publish) |
| `TEST_DATABASE_URL=... go test ./internal/lease -run TestPG` | PASS on 17.11/18.6, including actual backend termination → `Lost` |
| `go test ./internal/fence -run 'TestReleaseDoesNot\|TestAcquireDoesNot'` | PASS: R12-001 |
| `go test ./internal/consistency -run TestOpenLive` | PASS: R12-002 stale identity/ancestor/visibility |
| `go test ./cmd/newcloud -run TestRecoveryBeforeServing` | PASS: R12-003 persistent quarantine blocks startup |
| `go test ./cmd/newcloud -run TestRuntimeProtection` | PASS: second Tree/PG process refused |
| `go test ./internal/server -run TestSetNotReady` | PASS: lease-loss readiness transition |
| `go test ./... -count=1` | PASS (normal suite; red suite excluded by build tag) |
| `go build ./...`, `go vet ./...` | PASS |
| Linux (WSL2) `go test ./...` | PASS; real `renameat2(RENAME_NOREPLACE)` on the Linux filesystem |
| `GOOS=darwin GOARCH=arm64 go test -c ./internal/storage/fsrename` | PASS (compile only; no macOS runtime) |
| Windows full suite | PASS |
| `git diff --check` | clean |

### Linux evidence (WSL2 Ubuntu, Go 1.26.0)

The Ubuntu environment had no Go toolchain, so Go 1.26.0 was unpacked into the
user's home (no system install). Against the real Linux filesystem:

| Test | Result |
| --- | --- |
| `go test ./internal/storage/fsrename` | PASS: `renameat2(RENAME_NOREPLACE)` target-exists, free-target and directory cases |
| `go test ./internal/storage/tree` | PASS |
| `go test ./... -count=1` | PASS (all packages) |
| `go test -race ./internal/storage/fsrename ./internal/storage/tree ./internal/fence` | PASS |

The Linux run exposed a real cross-platform bug: `tree.Stat` mapped only `ENOENT`, so a path
below a regular file returned `ENOTDIR` and surfaced as 500 instead of `layout.ErrNotFound`.
`isNotFound` now maps both, and the full Linux suite is green. This is a justified Phase 1 fix.

### PostgreSQL evidence (PostgreSQL 17.11 and 18.6)

A local PostgreSQL 18.6 cluster was used first. PostgreSQL **17.11** was then unpacked
user-locally inside WSL2 (`postgresql-17` .deb, no system install) and run on port 55433 with
`-k /tmp`; the same Go packages ran against it:

| Test | PostgreSQL 17.11 | PostgreSQL 18.6 |
| --- | --- | --- |
| `go test ./internal/store -run TestPG` | **PASS** | PASS |
| `go test ./internal/lease -run TestPG` (takeover, epoch, backend termination → `Lost`) | **PASS** | PASS |
| `go test ./cmd/newcloud -run TestRuntimeProtectionPGLease` | **PASS** | PASS |
| `TestPGConsistencyPrimitives` (journal, revision CAS, G2, atomic publish, epoch) | **PASS** | PASS |

The full existing PG conformance suite had a pre-existing isolation defect: the global `groups`
table accumulated across subtests. The PG factory now clears it; both versions pass.

### Not yet implemented / unverified

- macOS no-replace/open-handle behaviour is unverified; the Darwin code compiles, while Windows and
  Linux were exercised on real filesystems.
- Parent-directory `fsync` after rename and the Windows equivalent of a flush-durable file rename
  are not yet proven against power loss. The no-replace probe proves the primitive, not the
  durability of the directory entry.
- `PublishBlob` now requires `EpochSet`; no live handler supplies the epoch yet. That wiring is
  Phase 1R/2B.
- `Recover` handles INTENT/STAGING safely and quarantines PUBLISHING; physical recovery of a
  PUBLISHING operation is still open and accepted as safe-unavailable.
- No DAV/REST handler uses the new primitives. That is deliberate: handler migration is Phase 1R.
- Phase 1R and Phase 2A entry criteria are therefore **not met**.

## 5. Architecture conformance and justified deviations

- **Tree staging/publish** follows ADR-0013 §6.2: private `.newcloud-stage/<opID>`, no-replace
  publish, retained operation backup, and a target-volume probe. No live handler calls it.
- **Metadata primitives** follow the ADR separation: `nodes.revision` CAS, operation journal,
  `staged_by`/effective visibility, and `content_refs` are in migration 0016 and the store
  implementations. They are not yet used by `Tx`/`Store` interfaces or handlers; that is the next
  integration step, not a claim of implementation.
- **Blob staging** uses immutable content-addressed bytes plus a conditional metadata publish
  (`PublishNoReplace`; `PublishReplaceExpected` for safe replacement). It is not a Stat-then-Record
  path: the no-replace/ETag check is inside the metadata transaction. The generic
  `StagingStore.PublishReplace` requires an expected ETag and refuses without one; the journal
  integration that supplies it is still open.
- **Runtime singleton:** the lease, epoch check, `FailStop` and Tree OS lock are acquired at startup
  and covered by tests. The readiness flag is not driven by lease state yet, and the epoch check is
  not called by a live publish transaction. This is still an open Phase 1 gate.

## 6. Phase 1R prerequisites (do not start until all are true)

1. Phase 0 red suite is recorded and reviewed; normal suite is green.
2. PostgreSQL 17 conformance passes for operation journal, revision CAS, visibility and lease.
3. Tree no-replace probe passes on each supported target volume; unsupported volumes refuse startup.
4. Every byte route is inventoried for `OpenLive`; every legacy writer is inventoried for G1.
5. `git diff --check`, build, vet, normal tests, race tests pass on the Phase 1 commit.
6. Separate implementation review approves the Phase 1R entry.

### 6b. What the four remaining phase-0 reproductions actually require

Measured after the delete relocation landed (`swap.Retire`, 2026-10-25), because that change looked
like it might close more than one of them and did not. Four of the seven now pass. The other three
are grouped here by what would have to exist, so a later round does not repeat the guess that they
are one small fix away.

| Reproduction | What it simulates | What closing it requires |
|---|---|---|
| `TestPhase0ReaderNeverSeesTheReplacementWindow` | A physical move of a node's content, with the metadata untouched, and a reader arriving in between | The read path must not derive a content location from `parent + name`. Either the location is recorded per node (indirection) or reads are fenced against the writer that moves it. A lock alone does not help: the reproduction performs the move outside any operation, so no writer holds anything to fence against. |
| `TestPhase0ADeleteDoesNotMoveAConcurrentWritersBytes` | **Closed** by a repair rather than by an ordering: after the namespace step refuses, the delete asks whether the live path is occupied. Occupied means a writer committed after the move and its bytes are the live ones, so the moved copy is left alone; empty means the moved bytes were the writer's, and they go back. The reproduction passes, and removing the check makes it fail again. | Two orderings were measured and neither closes the window: content-first moves a late writer's bytes, namespace-first moves a writer who created a file at the freed path. The repair therefore lives on the failure path; the journal is still the structural answer for the general case. |
| `TestPhase0LegacyWriterBypassesDeleteFence` | A writer that does not join the fence, landing in the window between a delete's content move and its metadata rename | The operation journal — **which now exists for this path** (rounds 44-46: transitions, INTENT recorded before the content move, and an `op-recovery` duty for what a crash leaves open). The reproduction still fails, correctly: its writer does not take a fence, so recording intent lets recovery explain the state afterwards but cannot stop a writer that never asked. Closing it for such a writer needs the writer path to consult the journal — a fence with participants. The window is real — `swap.Retire` moves content, then renames metadata, then trashes, and a writer arriving between the first two steps still updates the OLD node, which the trash then takes with it. A fence cannot stop a writer that does not take one; what catches it is recording the intent before the move so recovery can tell "this writer's bytes are the live ones" from "these are the ones being retired". |
| `TestPhase0BackfillMovesNewLiveBytes` | A maintenance process moving bytes that a live node now owns | The same indirection as the first row. It is also the least urgent of the three: no code in this tree performs that move. |

Two of the three are therefore the read-side half of the ADR (§ "OpenLive"), and the third is the
journal half. Neither is a defect in a path this deployment serves today: both describe what becomes
possible once content has an identity separate from its path, which is the point of the ADR and not a
bug in the path-addressed layout.

The delete relocation is worth separating from them, because it is the one that was a real defect and
is now fixed: a deleted file used to keep occupying its path until the retention sweep, which blocked
no-replace publish on this layout and left the retained copy addressable at a name that no longer
existed. That is `TestPhase0DeleteReleasesTheTreeLivePath`, and it passes.
### 6c. Read indirection: declined, with the condition that would reopen it

The remaining half of Phase 2A is reading content by a recorded identity rather than by
`parent + name`. It is not implemented, and this is a decision rather than an omission.

The two reproductions that need it (`TestPhase0ReaderNeverSeesTheReplacementWindow`,
`TestPhase0BackfillMovesNewLiveBytes`) both perform a **physical move of content outside the store**,
with the metadata untouched, and then expect a reader to see the original bytes. On a layout whose
content lives at its path, that is not a race to be fixed but a state the layout cannot represent: the
bytes are at no addressable location, and nothing the store did or did not do changes that. Recording
the location per node does not help either — the recorded location is the same path the move emptied.

What the product does instead was built deliberately and measured: the read path distinguishes "no
such row" from "row exists, bytes missing", answers `500 content_missing` for the second (never `404`),
counts it as `newcloud_content_missing_total`, pages on it through `NewCloudContentMissing`, and the
runbook says how to find the affected files and what a burst usually means. That is the honest
response to a failure the layout cannot prevent: make it loud, countable and diagnosable.

**What would reopen this decision.** One of these becoming true:

1. Content must be shared between paths (deduplication across nodes), which requires an identity
   anyway and makes the indirection free.
2. A layout is introduced whose content addressing is objective-independent (the blob layout already
   is; this would be a tree-like view over it).
3. An operator reports a real out-of-band move in production. That is the case the detection above is
   designed to surface, and it would be the evidence that the failure is reachable rather than
   theoretical.

Until one of those holds, building the indirection would add a per-node content table, a second way to
resolve every read, and a migration — to protect against a state the deployment cannot create, while
the detectable consequence is already covered end to end.
### 6a. First slice, in the order it has to happen

Written down after the 2026-10-24 review's M4 (2026-10-25): the artefact-name leak and
restore naming are one fix, and restore naming has to go first.

1. **Restore must not hand out a reserved name.** `swap.Install` displaces the destination to
   `<rand>.newcloud-replaced-<name>` and trashes it under that name, so `tx.Restore` today
   returns a live node whose name is an artefact. The primary key for a node is its id, and the
   name index is partial on live rows, so the name is free to change; only the PATH is not,
   because on the `tree` layout the content path is derived from parent + name.
2. **So the fix belongs in the restore handler, not in the store.** `internal/server/files.go`
   `handleRestore` already resolves the trashed node, authorises the parent, and calls
   `tx.Restore`. Give it: choose a usable name (strip everything up to and including
   `.newcloud-replaced-`; on collision append ` (n)`), then perform the name change and the byte
   move together with `swap.Relocate` — the same primitive the rename path uses, which is why
   the tree layout does not desync. The WebDAV side has no RESTORE method, so there is one
   entry point to change.
3. **Then the addressability guard can land.** `swap.IsReservedSegment` plus a refusal in
   `dav.splitPath` (and the API's path parser) — a one-liner each once step 2 is in, and it must
   NOT land before it: with restore still producing artefact names, the guard turns "restore"
   into "silently produce an unreachable file". The existing expectation that a restored node is
   readable over HTTP is the check that keeps the two in the right order
   (`internal/dav/treelayout_test.go`, the case that caught this).

## 7. Phase 0/1 acceptance checklist

| Criterion | Status |
| --- | --- |
| Baseline verified and recoverable checkpoints created | **DONE** |
| Owner approved the v1 support matrix; ADR marked Accepted | **DONE** |
| Phase 0 red suite reproduces the stated defects; normal baseline green | **DONE** |
| Tree private staging, no-replace publish, replacement backup, discard/probe | **DONE**, tested on Windows and Linux (real `renameat2`); macOS open |
| Hierarchical fence, journal, revision CAS, G2 visibility, OpenLive, recovery | **DONE**, unit/race tested; persistence tested on PostgreSQL 17.11 and 18.6 |
| Metadata publication with atomic audit + journal | **DONE for Blob publish** — metadata, audit and journal complete in one transaction; Tree publish still needs journal wiring |
| Blob `StagingStore` | **DONE** — content stage + conditional metadata publish; safe replace requires caller ETag |
| Runtime lease/OS-lock startup acquisition and fail-stop wiring | **DONE for primitives/wiring** — startup, readiness and stop callback tested; handlers do not pass their epoch yet |
| Physical recovery of a PUBLISHING operation | **ACCEPTED as safe-unavailable** — PUBLISHING is quarantined `NEEDS_OPERATOR`; journaled physical revert is deferred |
| PostgreSQL 17 evidence | **DONE** — 17.11 and 18.6 both pass |
| macOS runtime | **NOT CLAIMED** — Darwin code compiles; no macOS runtime/filesystem evidence |
| R12-001…R12-004 corrective patch | **DONE** — permanent regressions pass on Memory/PostgreSQL and Windows/Linux |
| Tree no-replace probe at startup | **DONE** — `openStorage` refuses an unproven volume |
| Persistent quarantine gate | **DONE** — `Unresolved` survives restart; startup refuses |
| DAV/REST/worker route migration (Phase 1R) | **NOT STARTED**, separate reviewed phase |
| Phase 1R independent implementation review | **NOT STARTED** |
| Production GO | **NO** |

**Phase 1 result:** CONDITIONAL PASS after the corrective patch. The four independently reproduced
API-invariant defects have permanent regressions, the Tree probe runs at startup, and quarantine is
a persistent startup blocker. Remaining items are explicitly scoped: macOS is not claimed without
runtime evidence, power-loss directory durability is unproven, physical PUBLISHING recovery is
accepted as safe-unavailable quarantine, and Phase 1R is a separate reviewed phase. No handler
migration or production deployment is claimed.

## 8. Files added/modified

Tracked changes are listed from `git status` at each checkpoint. Phase 0/1 additions:

- `internal/fence/fence.go`, `fence_test.go`
- `cmd/newcloud/recovery_startup_test.go`, `readiness_lease_test.go`
- `internal/consistency/journal.go`, `visibility.go`, `revision.go`, `opstore.go`, `openlive.go`, `recovery.go`, tests
- `internal/lease/lease.go`, `treelock.go`, platform lock files, tests
- `internal/storage/fsrename/*`
- `internal/storage/tree/operation.go`, `operation_test.go`
- `internal/storage/tree/tree.go` (`ENOTDIR` → not-found cross-platform fix)
- `internal/storage/blobstore/staging.go`, `staging_test.go`
- `internal/store/blobpublish.go`, `store.go` (`PublishBlob` contract)
- `internal/store/consistency_memory.go`, `consistency_pg.go`, tests
- `internal/store/pg_test.go` (clear global groups between conformance subtests)
- `migrations/0016_consistency.sql`
- `cmd/newcloud/drill.go` (backup table list)
- `cmd/newcloud/main.go` (startup lease/Tree-lock wiring), `runtime_protection_test.go`
- `internal/dav/phase0_red_test.go` (`phase0red`)
- `internal/server/phase0_red_test.go` (`phase0red`)
- this record and the ADR/index status updates.
