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
- **No data export bundle.** Files and metadata can be copied out through WebDAV; there is no
  one-command account export.

## Current gaps

These are capabilities the requirements call for that are not implemented today.

| Area | Current state | Missing |
| --- | --- | --- |
| **Group spaces** | Resource ACLs grant users and groups access to a folder. Groups can be granted `read`, `create`, `overwrite`, `rename`, `delete`, `manage_permissions`, and `download` independently. | Team-owned spaces: a folder whose owner is a group, with quota and lifecycle independent of any one account. |
| **WebDAV sync** | `REPORT` is answered with an explicit `501`. Locking is class 2. | `REPORT` / `sync-collection`, shared locks, and the incremental-sync protocol desktop clients use for large trees. |
| **Additional share modes** | Link, password, expiry, download cap, email recipient, drop box. | The remaining P1/P2 sharing modes in the requirements document. |
| **SCIM** | A `scim_token` configuration key exists. | No SCIM endpoint. Provisioning is OIDC JIT or administrator action. |
| **IdP events** | Role changes apply at the account's next login. | No event source or polling. "Removed from the group" and "permission gone" are separated by one login. |
| **IdP metadata cache** | Startup does not depend on the IdP. Existing sessions, App Passwords, and WebDAV continue when it is offline. | A cached discovery document. Each retry is a network round trip. |
| **Preview isolation** | Pixel, concurrency, and decode-deadline limits bound thumbnail work. | Subprocess sandboxing. Decoding still happens in the server process. |
| **Resumable uploads across restarts** | Sessions are held in memory; an interrupted upload must restart. Content integrity is unaffected. | Durable upload sessions that survive a process restart. |
| **COPY atomicity** | A failed directory COPY leaves the source intact and a partial destination. | Snapshot or rollback of the destination. Each node commits independently. |
| **Cursor pagination** | A row is never returned twice. Renaming an unseen row to before the cursor can hide it until the next listing. | Snapshot isolation across pages. Sorting is byte order, not client locale. |
| **Audit slowness** | Audit failure is fail-closed. | A timeout policy for "audit is slow but not failing". Disk-full behavior was injected, not exercised on a real full disk. |
| **Audit replay** | Refused operations are never accepted, so there is nothing to replay. | No outbox or replay queue, by design. |
| **Audit anchors** | Daily checkpoints detect truncation and mid-chain rewrites. | The code cannot enforce that `anchor_dir` uses different credentials from the database. That is an operator obligation. |
| **Application role bootstrap** | `newcloud_app` can `SELECT`/`INSERT` the audit table and cannot `UPDATE`/`DELETE`/`TRUNCATE` it. | The role password is set manually after migration. A migration cannot contain it safely. |
| **Account deletion** | Deletion moves files to trash, revokes shares and credentials, releases locks, keeps audit events, and leaves an irreversible tombstone. | Restoring a deleted account: an administrator must recover files and create a new account. |
| **`tree` layout** | Filesystem mirror, inspectable with `ls`, `rsync`-friendly. | No S3 backend, no deduplication, full copy per version, Windows reserved names apply, and `blobs.size` cannot be backfilled because the layout is not content-addressed. |
| **Break-glass** | Login endpoint, CIDR restriction, auditing, alerts, and hash generator exist. | No web UI (deliberate) and no terminal no-echo prompt; the password is read from stdin. |
| **Grafana** | Prometheus endpoint, 18 alert rules, runbook. | No bundled dashboard JSON. Dashboards depend on the Grafana version and go stale. |
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

## Where the evidence lives

- [Implementation record](../reference/implementation-record.md) — contract by contract, with the
  tests that pin each one and the remaining gaps.
- [Functional requirements](../reference/functional-requirements.md) — the input document and its
  priority model.
- [Restore drill runbook](../../deploy/backup/restore-drill.md) — measurements, PITR, and the
  drill record.
- [Alert runbook](../../deploy/monitoring/runbook.md) — what each alert means.
