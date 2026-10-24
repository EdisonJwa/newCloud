# ADR 0014: External storage mounts

**Status:** Accepted · **Date:** 2026-10-25 · **Blocks:** the "mount external storage as folders" requirement

## Context

The requirements list mounting external storage (SMB, SFTP, FTP, WebDAV) as folders a user can
browse and edit in place — no copy, no synchronisation job, no second copy of the bytes. The
alternative designs are:

1. **Metadata sync** — mirror the remote tree into `nodes` on a schedule. Rejected: it is a
   synchroniser wearing a mount's clothes (drift, conflict windows, a local copy the user was
   promised would not exist).
2. **Proxy-only namespaces** — mounts live outside the tree under `/api/mounts/...`. Rejected:
   permission management was an explicit requirement, and the ACL system authorises *nodes*. A
   mount that is not a node cannot be granted, shared or space'd.
3. **A mount root is a node; everything below it is virtual** — chosen.

## Decision

### The mount root is an ordinary directory node with a `mount_id`

`nodes.mount_id` (nullable FK) marks a directory row as a mount point. The row is created by the
mount service under the owner's root as a **top-level folder**, so:

- resolution, listings, ACL grants, group spaces and the audit trail treat it exactly like a
  folder, because it is one;
- the owner can grant it to users and groups with the existing per-permission bits
  (`read`, `download`, `create`, …), which is the "permission management" requirement satisfied
  by the machinery that already exists rather than a parallel one.

Everything **below** the mount root has no node row and never will. A path is split by walking
the store segment-by-segment and stopping at the first node with a `mount_id`; the remaining
segments are the remote path, served live by a provider. Listing a mount directory asks the
provider; there is no cache to invalidate because there is no cache.

### Providers are one interface, dialled per request

`internal/external.Storage` — `List`, `Stat`, `Open`, `Create`, `Mkdir`, `Remove`, `Rename`,
`Ping`. Implementations:

- **WebDAV** — a stdlib HTTP client (`PROPFIND` Depth 1, `GET`, `PUT`, `MKCOL`, `DELETE`,
  `MOVE`). Range requests pass through.
- **FTP** — a stdlib client (`MLSD` with a `LIST` fallback, `RETR`/`STOR`/`MKD`/`DELE`/`RMD`/
  `RNFR`+`RNTO`, passive mode, explicit FTPS). Whole-entity reads only.
- **SFTP** — the SFTP version-3 wire protocol spoken over `golang.org/x/crypto/ssh`, which this
  repository already depends on. No new module.
- **SMB** — **not implemented.** The Go SMB client (`hirochachacha/go-smb2`) cannot be fetched
  in this build environment, and shipping an untested integration would be worse than a named
  gap (the same call the LDAP decision in the acceptance record made). The scheme is refused
  with a clear error naming the reason; when the dependency becomes fetchable it slots into the
  same interface with no product changes.

### Semantics that follow from "no copy"

- **No trash, no versions, no quota for external content.** A delete below a mount root is a
  remote delete — immediate, no undo, and the API says so rather than pretending. External
  bytes never touch `used_bytes`. The mount root node itself cannot be deleted through the file
  API; it is removed by `DELETE /api/mounts/{id}` (unmount), which deletes the node row and
  nothing else.
- **Cross-storage moves are refused**, with `409 cross_storage`: moving between a mount and
  local storage (or between two mounts) is a copy+delete wearing a move's name, and the copy is
  exactly what this feature promises not to do silently. Rename *within* a mount is a remote
  rename.
- **Locks stop at the mount root.** A `LOCK` below a mount is refused: the lock register fences
  local writers, and remote storage has concurrent writers of its own that no local token can
  fence. Pretending otherwise would make `LOCK` a lie — the same reasoning as the class-2
  scope note in the README.
- **Chunked (resumable) uploads target local storage only.** Their staging and recovery design
  is local-disk-shaped; a single-request upload streams into a mount fine.

### Security posture

- **Credentials** are stored AES-256-GCM encrypted; the key is a hex file
  (`mounts.secret_file`), the same custody shape as the break-glass password. A mount cannot
  be created without it.
- **Mount creation is an operator action by default.** `mounts.allow_user_mounts` opts a
  deployment into user-created mounts, and when it does, `deny_private_addresses` (default on)
  refuses endpoints that resolve to loopback, link-local or RFC1918 space for non-admin
  creators: a user who can point the server at `http://169.254.169.254/` can read whatever that
  address serves, and that is an SSRF primitive, not a file share. Administrator-created mounts
  are exempt (an administrator already controls the process).
- **A read-only mount is enforced at the service boundary**, not by hoping the credentials are
  read-only on the server side.
- **Every external file operation writes an audit row** (`via: "mount"`, the mount id), and
  mount create/delete are audited with credentials never appearing in the row.

## Consequences

- `Resolve` never sees below a mount root; both HTTP surfaces consult `mount.ResolveTarget`
  first. The store stays unaware of providers, and providers stay unaware of nodes.
- Search (name search over `nodes`) cannot see mount contents; it can see the mount root. This
  is documented, not a bug to fix later by syncing.
- Thumbnails and previews are node-id addressed and therefore absent for external files in this
  revision.
- ZIP download refuses external subtrees (`external_not_supported`): the archive builder
  budget-proves completeness over a metadata walk, and a remote walk cannot make that promise
  cheaply. It can be added later as a provider-walking variant.
- A provider outage surfaces as `502 mount_unavailable`, not as empty folders: an empty
  directory and a dead server must not look alike.
