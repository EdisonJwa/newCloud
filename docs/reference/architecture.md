# Architecture

newCloud is one Go process in front of two stateful systems: PostgreSQL and a content store.
Everything else — sessions, locks, jobs, shares, the audit chain — is a table. That is a deliberate
constraint: a deployment can be restarted, moved, or run behind a load balancer without a
coordination service.

## Design goals

- **One binary, one database, one process.** No PHP, no Redis, no message broker, no cron daemon
  required for correctness.
- **The database is the source of truth for metadata.** The content store holds bytes; it never
  decides what exists.
- **Every write is auditable in the same transaction.** A write that cannot be recorded is
  refused.
- **Concurrency rules are shared by every entry point.** The web API and WebDAV cannot disagree
  about ETags, locks, or quotas.
- **Storage layout is an operator decision.** `blob` and `tree` differ in what they optimize, not
  in what they promise.

## Process map

| Package | Responsibility |
| --- | --- |
| `cmd/newcloud` | Process entry point, subcommands (`serve`, `migrate`, `healthcheck`, `audit verify`, `breakglass hash`, `drill`), startup self-checks |
| `internal/config` | YAML configuration, environment overlay, validation of unsafe combinations |
| `internal/auth` | OIDC discovery and callback, session cookies, App Password verification, break-glass login |
| `internal/authz` | `Actor` and `Cap`: the only way a handler asks "may this actor do this?" |
| `internal/store` | Metadata: nodes, versions, quota, trash, shares, locks, ACLs, jobs, audit, sessions, users |
| `internal/storage` | Content-store interface, `blob` and `tree` layouts, shared contract tests |
| `internal/blob` | Local and S3 content-addressed stores, hashing, multipart handling |
| `internal/dav` | WebDAV method handlers, path parsing, lock enforcement, conditional requests |
| `internal/server` | HTTP routing, JSON API, share pages, previews, content origin, metrics middleware |
| `internal/share` | Share creation, password and one-time-code flows, download accounting |
| `internal/audit` | Hash-chained event log, append, verify, export |
| `internal/worker` | Retention sweeps, orphan GC, resumable-upload cleanup, blob scrub, job consumer |
| `internal/metrics` | Prometheus registry and instrumented call sites |
| `internal/webassets` | The built web client, embedded into the binary |

## Request paths

```text
Browser ── OIDC authorization code ──▶ /auth/callback ──▶ session cookie
   │                                                       │
   ├── /api/*  (session or App Password) ──▶ authz ──▶ store ──▶ content store
   ├── /dav/*  (App Password; never a session cookie)
   └── /s/*    (share token; no account required)

Share visitor ── token ──▶ share lookup ──▶ password / one-time code ──▶ content origin
```

There are two origins by design:

- `server.public_url` serves the authenticated application shell. Session cookies are scoped here.
- `server.usercontent_url` serves uploaded bytes, previews, and share downloads. It has no session
  cookie and sends `Content-Security-Policy: sandbox`.

An upload containing stored XSS therefore executes on an origin that cannot read the session
cookie or reach the application DOM. The two URLs must differ in scheme, host, or port; the
configuration validator rejects anything else.

## Data model

The metadata schema is additive across migrations:

| Group | Tables |
| --- | --- |
| Identity | `users`, `groups`, `group_members`, `group_role_map`, `sessions`, `app_passwords`, `scim_pending_changes` |
| Tree | `nodes`, `versions`, `blobs`, `acl_entries`, `name_conflict_renames`, `dead_props` |
| Lifecycle | `shares`, `share_recipients`, `share_otp_attempts`, `download_sessions`, `lock_tokens`, `jobs`, `settings` |
| Audit | `audit.events`, `audit.anchors` |

A **node** is a file or directory in an owner's namespace. A file node points at a **blob** (the
content address) and may have many **versions**. A directory node is a parent, not a container on
disk. Shares, ACL entries, and locks reference nodes, so they survive rename and move.

## The write path

Four invariants make the system predictable:

1. **Content is stored before its row is published.** An interrupted upload leaves an orphan blob
   (harmless, collectable) rather than a row pointing at missing bytes.
2. **The audit event commits in the same transaction as the change.** If the audit insert fails,
   the change rolls back and the client receives `503 audit_unavailable`.
3. **Conditional writes are decided in the SQL statement that changes the row.** `If-Match`,
   `If-None-Match`, quota checks, and lock checks are evaluated under the same row lock, so two
   concurrent uploads cannot both believe they won.
4. **Locks are checked on every write path.** `LOCK` records a lock; `PUT`, `DELETE`, `MOVE`,
   `COPY`, `MKCOL`, `PROPPATCH`, and the JSON API check it. A lock that only exists in the `LOCK`
   handler protects nothing.

The web UI, resumable-upload completion, and WebDAV `PUT` all call the same store method, so they
cannot drift into different concurrency rules.

## Storage abstraction

`internal/storage` defines one `ContentStore` interface with two implementations:

- **`blob`** — content-addressed. The disk path is derived from the SHA-256 of the content. The
  directory tree exists only in PostgreSQL. Deduplicates identical content, and a new version
  stores only its differences. Not inspectable with `ls`.
- **`tree`** — filesystem mirror. The path is `<root>/<owner>/<user-visible path>`. Inspectable,
  `rsync`-friendly, and each version is a full copy. Path segments are validated, and `os.Root`
  provides syscall-level containment against traversal and symlink escape.

`Capabilities()` exposes the difference instead of hiding it: deduplication, cheap versions,
direct filesystem inspection, and efficient rename. Callers branch on capabilities, never on the
layout name.

Uploads always stage on local disk, even when the destination is S3: the content hash must be
known before the object can be named. Staging lives under `storage.upload_dir`, outside the
addressable tree.

## Audit

Every audited operation appends an event whose hash covers the previous event's hash. The chain
proves internal consistency. It cannot, by itself, prove completeness: someone with database
access can delete the newest rows and the remaining chain still verifies.

Daily **anchors** close that gap. A checkpoint of the chain head and event count is written to
`audit.anchor_dir`, which must be a location the application cannot rewrite. A missing or
mismatched anchor is evidence of truncation or a mid-chain rewrite.

`newcloud audit verify` walks the chain, checks every hash, and compares the recorded anchors.
Exit code `0` means intact; `1` means broken. The same logic is exposed to monitoring through
audit metrics and alerts.

The application connects as `newcloud_app`, which has `SELECT` and `INSERT` on the audit table but
no `UPDATE`, `DELETE`, or `TRUNCATE`. The append-only guarantee is enforced by PostgreSQL grants,
not by convention.

## Background worker

Maintenance runs in the same process, in a loop with leases:

- retention sweeps for trash and old versions
- orphan blob collection, driven by metadata rows only
- expired resumable-upload staging cleanup
- expired lock cleanup
- periodic blob integrity scrub (opt-in via `limits.blob_scrub_per_pass`)
- the job queue consumer, with retries and backoff

The worker does not assume it is the only instance. Job claims use `FOR UPDATE SKIP LOCKED` and a
lease, and every sweep is idempotent. A second replica adds a second sweeper without corrupting
anything.

## Failure behavior

| Failure | Behavior |
| --- | --- |
| Audit storage unavailable | Mutating requests fail closed with `503 audit_unavailable`; reads continue |
| IdP unreachable at startup | Process starts; existing sessions, App Passwords, and WebDAV continue; new logins return `503` |
| Blob missing but row present | Error, never `404`: a missing file and a storage fault are different things |
| Object-store error | `Put` fails without leaving an object; `Stat` failure is not `NotFound`; `Delete` failure is reported |
| Database unavailable | `/readyz` fails; `/healthz` still reports the process |
| Split storage filesystem | Startup rename probe fails; the process refuses to start |
| Interrupted upload | Half-written data stays in staging, structurally unreachable by hash |

## Deployment shapes

A single node with local disk and PostgreSQL is the reference deployment. The process is stateless
apart from local upload staging, so running more than one replica is possible when uploads are
routed consistently or staged on shared storage. Sessions, locks, jobs, and audit do not require
sticky routing.

S3 removes the local content volume but not the staging volume. `tree` layout requires local disk;
object storage has no directory hierarchy to mirror.

See [ADR-0002](../adr/0002-backend-skeleton.md) for the credential model,
[ADR-0003](../adr/0003-storage-layout.md) for the storage decision, and
[ADR-0004](../adr/0004-metadata-store.md) for the store contract.
