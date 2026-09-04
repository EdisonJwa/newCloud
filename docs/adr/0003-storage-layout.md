# ADR-0003: Optional storage layouts (blob and tree)

**Status:** Accepted · **Date:** 2026-10-21

newCloud offers two storage layouts behind one `ContentStore` interface: `blob`, which stores content-addressed bytes under a flat hash tree on disk and keeps the directory tree only in PostgreSQL, and `tree`, which mirrors the directory tree directly on the filesystem and uses the database as an index. The layout is a deployment-wide setting (`storage.layout`) rather than a per-user switch, because the two options differ in backup, migration, capacity accounting, and failure recovery; one instance runs one layout at a time. Changing the layout after data exists requires a migration tool that is designed but not implemented, so in practice the choice is made before the first upload.

Related: [0001-scope-and-language.md](0001-scope-and-language.md) §8.7, [0002-backend-skeleton.md](0002-backend-skeleton.md) §6.

## 0. Background

An earlier version of the design fixed the content-addressed blob as the only storage method. The requirement is that the operator be able to choose: store files directly according to a directory tree, or store them as blobs.

The layout is deployment-level rather than a per-user switch because the difference is not only in file naming, but in the data structure itself. In the blob layout, the directory tree exists only in PostgreSQL and disks hold a flat hash tree. In the tree layout, the directory tree is the directory structure on disk and the database is only an index. Backup, migration, capacity accounting, and failure recovery therefore all differ. Letting individual users mix layouts within one instance would require the system to maintain two sources of truth at the same time, and that becomes harder to unwind as capacity grows.

```yaml
storage:
  backend: local       # where the bytes live: local | s3
  layout: blob         # how they are organized: blob | tree
```

Switching requires a configuration change plus a migration of the existing content. The migration tool is designed but not implemented, and mixing layouts at runtime is not allowed.

## 1. How the two layouts differ

| Dimension | `blob` (content-addressed) | `tree` (filesystem mirror) |
| --- | --- | --- |
| On-disk path | `<root>/ab/cd/<sha256>` | `<root>/<owner>/<path the user sees>` |
| Directory tree | PostgreSQL only | The filesystem itself |
| Deduplication | ✅ Identical content stored once | ❌ Every copy stored separately |
| Version cost | ✅ A new version adds only a delta blob | ❌ Every version is a complete copy |
| Rename a directory (including one with a million files) | ✅ Change one metadata row | ❌ Rename the entire subtree |
| Administrator inspection with `ls` | ❌ Sees only hashes | ✅ Sees exactly what the user sees |
| Direct rsync / rclone sync | ⚠️ Syncs, but the result is unreadable | ✅ Naturally suited |
| Path traversal risk | ✅ No paths exist on disk at all | ⚠️ Every path segment must be validated |
| Delete a user | Reclaim by refcount | `rm -rf <owner>` |
| Disk usage | Smaller (deduplication and delta versions) | Larger (versions and trash each hold a full copy) |

### Choosing a layout

| Situation | Recommendation |
| --- | --- |
| Large capacity, many users, many duplicate files (templates, images, archives) | **`blob`** (default) |
| Operations staff inspect files directly, back up with rsync, or integrate with an existing NAS workflow | `tree` |
| Disk capacity is tight | `blob` (deduplication saves 20–40%) |
| Few files, each large, rarely rewritten (for example video archives) | `tree` (little deduplication benefit, large readability benefit) |
| A compliance requirement that deleting a user deletes everything | `tree` |

**For large capacities, `blob` remains the recommendation**: versions and trash bring the real occupancy of the tree layout close to 2×, and blob deduplication plus delta versions absorb that overhead. When the team is small and direct inspection of files on the machine after an incident matters more, `tree` is a reasonable trade-off — which is exactly why it exists.

## 2. Interface design

Both layouts implement the same interface (`internal/storage/layout`):

```go
type ContentStore interface {
    Mode() Mode
    Put(ctx, Node, r io.Reader, size int64) (PutResult, error)
    Open(ctx, Node) (io.ReadSeekCloser, error)   // must support Seek (Range)
    Stat(ctx, Node) (Node, error)
    Mkdir(ctx, Node) error
    Move(ctx, src, dst Node) error
    Delete(ctx, Node) error
    List(ctx, dir Node, limit int) ([]Node, error)
    Walk(ctx, dir Node, fn func(Node) error) error
    Capabilities() Caps
}
```

**The semantic difference in `Walk` must be explicit.**

- The tree layout can walk natively, because the filesystem has a hierarchy.
- The blob layout **cannot**: the directory tree lives in the database.

If the blob layout's `Walk` returned an empty list, callers would conclude that the subtree is empty, and jobs such as quota recalculation, virus scanning, and storage migration would silently skip data. It therefore returns `ErrWalkUnsupported`, and callers must use metadata queries instead.

**`Capabilities()` tells the UI about the differences up front**, rather than failing after a click:

```go
type Caps struct {
    Deduplicated    bool  // blob: true, tree: false
    CheapVersions   bool  // blob: true, tree: false
    DirectFSInspect bool  // blob: false, tree: true
    EfficientRename bool  // blob: true, tree: false
}
```

In the UI this appears as:

- In tree mode, the versions page states that each version occupies its own space.
- In blob mode, the storage path field is hidden, because it has no meaning.
- In tree mode, renaming a large directory shows a progress indicator, because the operation really is renaming a million files.

## 3. Security design for the tree layout

The tree layout is the **clearly more dangerous** path: one miscalculated path is an arbitrary file read. It therefore has three layers of defense, not a single string check.

### Layer 1: segment validation

`layout.ValidateSegment` rejects:

- `..`, `.`, and empty segments
- `/`, `\`, NUL, and control characters
- Windows-illegal characters `<>:"|?*` (`C:evil` is a drive-relative path, and `file:stream` is an NTFS data stream)
- Windows reserved device names (`CON`, `NUL`, `COM1`, …) — otherwise a file synced to a Windows client cannot be opened, and the user reports a corrupted file
- a trailing dot or space (Windows strips these silently, which makes `report.` and `report` collide)
- names longer than 255 bytes

### Layer 2: system-call-level isolation (`os.Root`)

Go's `os.Root` (introduced in Go 1.24, with write methods added in 1.25) resolves paths with `openat(2)` and **refuses escapes at the kernel level**, including through symlinks.

This is not redundancy: when layer 1 has a bug, layer 2 still contains the escape. A test covers exactly this case — a symlink in the store that points at an external file must fail on read (`TestSymlinkEscapeBlocked`).

```go
root, _ := os.OpenRoot(dir)
root.Open("../../etc/passwd")  // -> "path escapes from parent"
```

### Layer 3: write atomicity

A write lands in `.newcloud-tmp/<random>`, is fsynced, and is then moved into place with `rename(2)`.

Effect: on an interrupted upload, a process crash, or a full disk, **readers always see either the old content or the new content, never half a file**. The test `TestInterruptedUploadKeepsOldContent` covers this scenario.

### Other constraints

- **The owner is a path segment too**, validated by the same rules; `owner=".."` would place every file of an account outside the store.
- **Deleting the store root is forbidden** (`path=""`); otherwise a user deleting their own root directory deletes the account.
- **Moving a directory into its own subtree is forbidden**. The filesystem returns EINVAL, but the error is reported clearly in advance.
- **Cross-owner moves are rejected outright** and must go through copy-then-delete, because the operation would otherwise be an unauthorized rename.

## 4. Implementation notes for the blob layout

`internal/storage/blobstore` is an adapter that connects the existing `blob.Store` (content-addressed) to the unified interface.

**It needs the metadata layer to answer `(owner, path) -> hash`.** In production this comes from PostgreSQL. Today an in-memory implementation in `internal/storage/resolve` closes the loop and **warns explicitly at startup that it must not be used in production**:

```text
WARN blob layout is using the in-memory node resolver;
     metadata will not survive a restart until the PostgreSQL resolver is wired
```

**Deliberate decisions:**

1. **`Delete` does not delete bytes.** The same content may be referenced by other users, other versions, or the trash. Reference counting lives in the database, not in the storage layer; the adapter deletes only the metadata mapping.
2. **`Move` does not move bytes.** It changes only the mapping. That is why renaming a directory in the blob layout is O(1).
3. **Metadata that points at a missing blob produces a loud error that includes the hash.** Returning a plain 404 would mask data corruption: the user concludes the file is gone when the storage layer is the problem.
4. **Large files are spooled to a temporary file first.** Blob storage must know the content address before it can write, so the content must be read in full; it must not be read into memory, because a 100 GiB upload would consume 100 GiB of RAM. On Unix the temporary file is unlinked immediately and released when the handle is closed.

## 5. Consistency guarantees

`internal/storage/layouttest` is a shared contract test suite; **both layouts run the same cases**.

This guarantees that the WebDAV and API layers need no `switch layout { ... }`. Where behavior genuinely differs between the layouts (deduplication, for example), it belongs to `Caps` and is checked explicitly by the caller rather than scattered through handlers.

Covered: write, read, and overwrite; Seek; size-mismatch rejection; unsafe-path rejection; empty owner; a directory rejects `Put`; Mkdir and List (non-recursive); limit; Move; cross-owner Move rejection; delete semantics; owner isolation; and Caps consistent with the mode.

## 6. Migration path (designed, not implemented)

The planned interface is:

```text
newcloud migrate-storage --to tree
newcloud migrate-storage --to blob
```

No such subcommand exists today. The table records the intended procedure so that an operator does not attempt an in-place switch.

| Direction | Method | Risk |
| --- | --- | --- |
| blob → tree | Walk the node tree via metadata, read from the blob, and write to `<owner>/<path>` | Needs one extra copy of the space (cannot delete while writing); large directories are slow |
| tree → blob | Walk the filesystem, compute SHA-256 for each file, write it to the blob, and update metadata in step | All hashes must be computed first; no writes during that period |

**Common requirements:**

- A full backup and verification before migration.
- Migration in a maintenance window, or a dual-write transition (considered only after M3).
- An `fsck`-style verification after migration: compare hash and size file by file.

## 7. Impact on M1

Because this abstraction came first, the M1 WebDAV implementation **does not need to know which layout is underneath**. Had the original plan been followed — hardcode blob first, add tree later — the node tree, versions, quota, and trash would all have required rework.

Wiring in practice:

```go
// WebDAV PUT
res, err := h.store.Put(ctx, layout.Node{Owner: actor.Owner(), Path: vpath}, body, size)

// The response ETag uses the content address, identical in both layouts
etag := fmt.Sprintf(`"%x"`, res.Hash[:8])

// The version policy reads capabilities, not the concrete type
if h.store.Capabilities().CheapVersions {
    // keep all versions (blob: only the delta occupies space)
} else {
    // tell the user each version consumes full space, or lower the retention count
}
```

## 8. Configuration example

```yaml
storage:
  backend: local
  layout: tree
  local:
    # tree layout: files live here directly, organized as <owner>/<path>
    root: /var/lib/newcloud/files

# or
storage:
  backend: local
  layout: blob
  local:
    # blob layout: <root>/ab/cd/<sha256>
    root: /var/lib/newcloud/blobs
```

**At startup the log states the layout in effect**, and `/readyz` also reports it, so a deployment cannot silently believe it configured A while running B.

## 9. Open questions

1. **The S3 + tree combination is rejected**, because object storage has no directory hierarchy to mirror. Making S3 "look like directories" is really the blob layout with a readable object-key prefix scheme, which is a separate design.
2. **The migration tool's concrete implementation** is scheduled for M2.
3. **Quota accounting**: the tree layout sums `Usage()` by walking, which is slow, but the filesystem is right there; the blob layout reads `used_bytes` from the database directly. The semantics are identical, the performance differs by three orders of magnitude, and the UI may need to show "calculating".
