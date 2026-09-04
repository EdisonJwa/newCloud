# ADR-0004: Metadata store contract

**Status:** Accepted · **Date:** 2026-10-21

The metadata layer identifies nodes by ID only: it never resolves paths and never makes authorization decisions. `storage` maps an owner string and a relative path to bytes, `store` owns the node tree, quota, versions, and audit enqueueing, and `authz` maps an actor and a node ID to a capability. Business rows, audit records, and queued jobs commit in one transaction, and no exported `store` method accepts an `Actor`, so authorization cannot migrate out of `authz`.

Related: [0002-backend-skeleton.md](0002-backend-skeleton.md) §5, [0003-storage-layout.md](0003-storage-layout.md).

## 1. Why three layers: `storage`, `store`, and `authz`

| Layer | Knows | Does not know |
| --- | --- | --- |
| `storage` (layout / blobstore / tree) | owner string + relative path → bytes | who the user is, whether they may read |
| `store` | node tree, quota, versions, audit enqueueing | who is operating, whether they have permission |
| `authz` | Actor + nodeID → Cap | what nodes are called, where bytes live |

**Key constraint: no exported method on `store` takes an `Actor` in its signature.**

This is not fastidiousness. If `store.GetNode(actor, id)` could receive an actor, code would appear that says "the actor is already here, so the permission check can live in `store`" — authorization logic would immediately spread across two packages, and IDOR enters through that gap. `store` therefore accepts only `uuid.UUID`, and a caller must obtain an `authz.Cap` before it can obtain an ID.

```go
// authz layer
cap, err := authorizer.Authorize(actor, nodeID)
if err != nil { return 404 }          // note: not 403

// only with a cap in hand can its nodeID be used to query metadata
node, err := tx.Node(ctx, cap.NodeID())
```

**Why 404 uniformly rather than 403**: a 403 tells an attacker that the ID exists but the attacker lacks permission, which is a free enumeration interface.

## 2. Transaction model: audit, jobs, and business rows commit together

`store.Tx` carries `CreateFile`, `Audit`, and `Enqueue`. They must share one transaction because:

- **business without audit** → an operation with no record, and the audit loses its meaning;
- **audit without business** → the log claims a download that never happened, which misleads forensics;
- **business committed but the job lost** → the file is visible but will never be scanned, so virus scanning amounts to nothing.

The `Memory` implementation simulates this with snapshot rollback, and a test verifies it:

```text
TestTransactionRollbackLeavesNoTrace
```

That test creates a file and writes an audit row in one transaction, then deliberately fails a second file on quota, and asserts that the file is invisible, the quota is not consumed, and no audit row remains.

## 3. The in-memory implementation is part of the contract, not a toy

`store.Memory` is not a placeholder to delete once PostgreSQL arrives. It implements the same **invariants** as PostgreSQL, so a defect found here exists in PostgreSQL as well:

| Invariant | How Memory guarantees it | How PostgreSQL guarantees it |
| --- | --- | --- |
| Sibling file names are unique (trash excluded) | `checkName` skips trashed rows | partial unique index `WHERE trashed_at IS NULL` |
| File names are safe | `layout.ValidateSegment` | `CHECK (name !~ '[/\\]')` + application-level validation |
| A directory cannot be moved into its own subtree | walk up the parent chain | application-level walk (SQL recursive CTE) |
| Quota is charged on the **delta** | `checkQuota(delta)` | the same SQL |
| Trash does not release quota | asserted explicitly in tests | same (only deletion releases) |

**`TestQuotaUsesDeltaOnOverwrite`** deserves separate mention: it covers the bug where re-uploading the same file charges quota twice. If quota were computed from `new_size` rather than `new_size - old_size`, a user repeatedly saving a large file would hit the limit early.

**`TestConcurrentSameNameOnlyOneWins`** covers a race: 8 goroutines create `same.txt` concurrently, and exactly 1 must succeed while 7 receive `ErrConflict`. This test is the reason the partial unique index exists.

## 4. Path resolution is case-exact

```go
func (m *Memory) Resolve(ctx, ownerID, path) ([]Node, error)
```

`Resolve` returns the **ancestor chain of the node**, including the leaf, not a path string. The reason: callers need IDs for authorization, and paths change.

**Case handling is a deliberate choice:**

- The index uses `strings.ToLower(name)`, because table lookups must be fast.
- On a hit, the original case is **compared again**, and a mismatch is treated as absent.

Why: `Readme.md` and `README.md` are the same file on Windows and macOS and two files on Linux. If the server silently treated them as one, a user uploading both from a Linux client would see one of them vanish. A failed second upload with an explicit `ErrConflict` is preferable to a silent merge.

## 5. Trash semantics

`Trash` is recursive: deleting a directory moves the whole subtree to trash. There is no room for compromise here. If only the parent were trashed while its children stayed live, `Resolve` walking the parent chain would enter a state in which the parent is deleted but the child is not, which is a classic consistency problem.

`Restore` is recursive in the same way. Restoring only the directory would bring it back empty while its children stayed unreachable: they do not appear in the parent's trash entry, and their parent is no longer trashed. A descendant that was deleted separately before its ancestor stays in the trash, so restoring an ancestor does not resurrect something the user deleted on purpose. Both behaviors are covered by the shared contract tests.

## 6. Versions and quota

- `UpdateFile` saves the old version only when `KeepVersion=true`.
- `MaxVersions` is an **upper bound, not a trigger**: the oldest version is discarded once the bound is exceeded.
- The `tree` layout should lower `MaxVersions`, because every version is a complete copy. Callers decide from `Caps().CheapVersions`:

```go
maxVersions := 10
if !store.Capabilities().CheapVersions {
    maxVersions = 3  // tree: each version occupies full space
}
```

This is why capabilities are exposed explicitly rather than left to the caller to guess.

## 7. How the blob layout obtains metadata

The blob layout keeps the directory tree only in the database; on disk it has flat hash files. The storage layer must therefore ask the metadata layer: `(owner, path) → hash`.

The interface is deliberately narrow:

```go
type Resolver interface {
    HashFor(ctx, owner, path) (hash [32]byte, ok bool, err error)
    Record(ctx, owner, path, hash [32]byte, isDir bool) error
    Forget(ctx, owner, path) error
    IsDir(ctx, owner, path) (bool, bool, error)
    Children(ctx, owner, dirPath, limit) ([]ResolverNode, error)
}
```

**Why `owner` is a UUID in string form rather than a typed ID**: a path that fails to parse must produce an error, not a panic. `parseOwner` returns `ErrInvalidName` on failure, and the caller treats that as "does not exist".

**Why `store` does not import `storage`**: the dependency direction must be storage → store, otherwise the two layers import each other. The cost is that `store` implements its own validation equivalent to `layout.ValidateSegment`, which the shared contract tests keep from drifting.

**`Walk` returns `ErrWalkUnsupported` rather than an empty result.** An empty result would make jobs such as capacity scanning and orphan reclamation skip all data silently. An explicit error tells the caller that this layout does not support walking and that a metadata query is required.

## 8. M1 status

| Item | Status | Notes |
| --- | --- | --- |
| `store.Memory` | Complete | Full semantics, not a stub; shares the contract test suite with PostgreSQL |
| `store` PostgreSQL implementation | Complete | `WithTx` + pgx, including recursive-CTE trash, restore, and move |
| `store.Restore` recursion | Complete | Does not revive descendants that were deleted individually |
| `RestoreVersion` | Complete | Implemented by both; an earlier Memory returned `errNotImplemented` |
| WebDAV `PROPFIND` | Complete | Streaming output, with a child-count limit |
| WebDAV `PUT` | Complete | Writes content first, then metadata and audit in a transaction |
| WebDAV `LOCK` | Complete | Persistent lock table, with ownership checks and timeout cleanup |
| App Password | Complete | Issue, verify, revoke, list |
| Trash listing | Complete | Lists only the roots of deleted subtrees, consistent with restore semantics |

## 9. Defects found by end-to-end verification

These are recorded because they show that "all tests green" is not the same as "it runs".

### 9.1 First login for a pre-provisioned account returns 500

`UpsertIdPUser` used a single `INSERT ... ON CONFLICT (idp_issuer, idp_subject)`. If the account had already been pre-provisioned by SCIM or by an administrator — a username, no IdP identity — the insert collided with the `users_username_key` unique index, and the user's first login returned 500.

Worse: **the in-memory implementation has no such constraint, so the unit tests were all green.**

The fix is lookup-then-write rather than reliance on `ON CONFLICT`: after a statement fails, PostgreSQL puts the whole transaction in the aborted state (25P02), and every later query errors, so "collide and then try the other path" is unavailable here. The lookup order is: find by IdP identity first, then by username; when claiming a row by username, reject if the row already belongs to another IdP subject, otherwise a username collision becomes account takeover.

Both implementations changed, and the scenario joined the shared contract tests (`UpsertIdPUser/ClaimsPreprovisionedAccount`) and the in-memory identity test `TestUpsertUserRejectsUsernameHijack`.

### 9.2 WebDAV PUT always returns 404

`Handler.blobIDFor` was a stub that returned a zero UUID. The `nodes.blob_id` foreign key in PostgreSQL rejects a zero UUID, so every WebDAV upload failed, and the client saw only a 404.

**The in-memory implementation accepts a zero UUID, so the DAV test suite was all green.**

The fix connects `Store.BlobIDFor`. The regression test `TestPutRecordsBlobID` asserts that the node actually carries a blob reference after the write; an HTTP status code alone does not catch this defect. The test has been verified in reverse against the old implementation: restoring the stub makes it fail.

### 9.3 The session `jti` is not a UUID

The session identifier was originally a random base64 string, while `sessions.id` is a `uuid` column. The in-memory implementation accepts any string; PostgreSQL reports `22P02`. It is now `uuid.NewString()`: the jti is only an identifier for revocation, the credential is the HMAC-signed ticket, and there is no reason not to use a UUID.

### 9.4 The upload buffer was written to the system temporary directory

`spoolToTemp` used `os.CreateTemp("")`, which is the system temporary directory. In containers, `/tmp` is often not writable or lives on a different filesystem from the data disk; the latter degrades publishing a blob from `rename(2)` to a full-file copy. It now creates `tmp/` under the storage root.

### 9.5 Three list endpoints returned three different field names

`{shares: []}`, `{items: []}`, and `{app_passwords: []}`. The front end fell back to `data.items || data`, received an object rather than an array, and `for...of` threw.

All three were unified to `{items: []}`. The same pass found that the trash endpoint was a hardcoded empty-array stub and supplied a real implementation (`Tx.Trashed`, returning only the roots of deleted subtrees).

This defect was found by **screenshot**, not by a test: the UI check at the time asserted only that the content changed after switching to Shares, and after the view threw, a toast reported the error and the content did change, so the check passed. The check now asserts that the view's own content exists and that no error toast appears.

### 9.6 Email shares did not verify the recipient (security gap)

Shares with `shares.kind = 'email'` record `share_recipients` rows, but `Resolve` never checked them. The result: **"share with one email address only" was effectively a public link**, with a note of the intended recipient attached.

This was the most important fix of the round. Today:

- Download, upload, and the page for an email share all pass the same `gate()`, in parallel with the password gate rather than merged into it.
- The verification credential is a one-time code sent to that address (`ticket.PurposeEmailVerify`), bound to both the share and the address. Bound to the share alone, any invitee could forward the code and admit anyone; bound to the address alone, one person's code would open every share.
- A request for a code from a non-invited address returns a 202 **identical** to the one returned for an invited address; otherwise the endpoint becomes a probe for whether someone was invited to a share.
- Successful verification and rejection are both audited. Rejections matter especially: a series of rejections against one share is what a forwarded link looks like.

### 9.7 Audit export filtered out the events that matter most

The first export filtered regular users by `actor_user_id`, meaning the operations that user performed. A share owner's real question, however, is what happened to their files — and **anonymous downloads and rejected recipient attempts have no actor account at all**, so exactly those events were filtered out.

The filter became `ScopeToOwner`: `actor_user_id = <this user> OR share_id IN (<shares this user owns>)`. The two sets are disjoint, and that is the point.

### 9.8 `actor_email` can be NULL

The audit table allows `actor_email` to be null for anonymous events, but the read path scanned it into a `string`, so **the export returned 500 on exactly the anonymous events**. The read path now uses `COALESCE`, and the same hazard in `VerifyAuditChain` was fixed at the same time.

This defect appears only against real PostgreSQL: the in-memory implementation's field is a Go string and is never NULL. Another demonstration that the contract tests must run against both backends.

## 10. Verification

- Unit and integration: `go test ./...`, including the shared contract tests for both the in-memory and the PostgreSQL implementations.
- The real path: `tools/e2e` drives the full OIDC login, upload and download, share hit counting, and WebDAV flows against a running service.
- UI: `tools/uicheck` loads real pages in headless Chrome and verifies rendering, tab switching, and that user data reaches the DOM as text.
- Deployment: `docker build` runs `go vet` and the full test suite in the build stage; `migrate`, `serve`, and `healthcheck` were all verified inside the container.
- Backup: logical backup, restore into a new database, audit hash-chain verification, then start the service against the restored data and read files back; the whole flow passes.
