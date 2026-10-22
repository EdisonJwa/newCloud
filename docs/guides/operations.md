# Operations

This guide is the operator's map: what to scrape, what to alert on, how to verify the audit chain,
how to back up and restore, and what to do first when a page arrives.

The alert runbook with per-alert triage steps is
[deploy/monitoring/runbook.md](../../deploy/monitoring/runbook.md). This page explains the system
those alerts observe.

## Health endpoints

| Endpoint | Answers | Use |
| --- | --- | --- |
| `/healthz` | The process is alive | Container health check, load-balancer liveness |
| `/readyz` | The process can reach the metadata store and the content store | Load-balancer readiness, deploy verification |
| `/metrics` | Prometheus text format | Scraping only; no authentication |

A useful first distinction: `/healthz` returns `200` while `/readyz` does not, which means the
process is fine and its database or content store is not. Most alarming metrics resolve to that
pair.

```sh
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS http://127.0.0.1:8080/metrics | grep '^newcloud_'
```

## Logs

Logs are JSON on stdout, one line per request plus lifecycle events. `NEWCLOUD_LOG_LEVEL` accepts
`debug`, `info`, `warn`, and `error`.

Startup is intentionally loud about unsafe states:

- no audit anchor configured;
- no integrity scrub configured;
- retention configured as "keep forever";
- a self-signed SMTP relay with verification disabled;
- an unreachable IdP (the process still starts; only new logins are affected).

A startup warning is not noise. It names a property that will otherwise be discovered during an
incident.

## Metrics

All metric names are prefixed `newcloud_`. The registry is dependency-free and serves the
Prometheus text format directly.

| Metric | Type | Meaning |
| --- | --- | --- |
| `newcloud_http_requests_total{method,status_class}` | counter | Responses by method and status class |
| `newcloud_dav_requests_total` | counter | WebDAV requests |
| `newcloud_dav_conflicts_total` | counter | WebDAV `409`/`412` responses: lock contention or a stale ETag |
| `newcloud_uploads_total{result}` | counter | Upload outcomes |
| `newcloud_thumbnails_total{result}` | counter | Thumbnail decode outcomes |
| `newcloud_jobs{state}` | gauge | Queue depth by `pending`, `running`, `failed` |
| `newcloud_blob_gc_deleted_total` | counter | Blobs removed by the collector |
| `newcloud_blob_gc_errors_total` | counter | Failed collection runs |
| `newcloud_blob_scrub_checked` | gauge | Blobs verified in the last integrity pass |
| `newcloud_blob_scrub_corrupt` | gauge | Blobs whose bytes do not match their address |
| `newcloud_blob_scrub_missing` | gauge | Blobs referenced by metadata but absent from storage |
| `newcloud_wal_archived_total` | counter | WAL segments archived successfully |
| `newcloud_wal_archive_failed_total` | counter | WAL archive attempts that failed |
| `newcloud_wal_segments_pending` | gauge | WAL segments waiting to be archived |
| `newcloud_auth_events_total{kind}` | counter | Authentication events, including break-glass use |
| `newcloud_audit_events` | gauge | Events in the audit chain |
| `newcloud_audit_chain_head_age_seconds` | gauge | Age of the newest audit event |
| `newcloud_audit_anchor_verify_total{result}` | counter | Anchor verifications by outcome |
| `newcloud_uptime_seconds` | gauge | Process uptime |

The shipped rules in [deploy/monitoring/alerts.yml](../../deploy/monitoring/alerts.yml) cover
process down, error rate, database degradation, audit stall and verification failure, blob GC
errors, job backlog and failures, upload failures, DAV conflicts, break-glass use and failed
attempts, WAL archive failure/backlog/stall, blob corruption, blob loss, and a scrub that has
stopped running.

Thresholds are starting values, not measured conclusions. Revisit them after two weeks of real
traffic.

There is no bundled Grafana dashboard. Dashboards depend on the Grafana version and go stale;
the metric names and the runbook are the stable interface.

## Audit verification

```sh
newcloud audit verify
```

The command walks the chain, recomputes every hash, and compares the recorded daily anchors. Exit
code `0` means intact; `1` means broken. That contract makes it usable from cron or CI without
parsing output.

The chain alone detects edits and mid-chain rewrites. It cannot detect deletion of the newest
events, because the remaining chain still verifies. Anchors close that gap: a checkpoint of the
chain head and event count is written to `audit.anchor_dir` once per completed day.

The anchor directory must be writable by the application but not rewritable with the database
credentials. A WORM bucket, an append-only volume, or a mounted share the application user cannot
modify is appropriate. A directory on the metadata volume makes the anchor decorative.

Export events for investigation:

```sh
GET /api/audit?format=csv
GET /api/audit?format=json
```

A normal user's export covers that user's own actions plus anonymized events about their data
(anonymous downloads, rejected recipient attempts). Only an administrator can export the full log.

### Retention: pruning the log

The audit table is append-only and grows monotonically, so retention is an explicit operator action
rather than a background job. Pruning a hash chain is not `DELETE WHERE ts < cutoff`: removing the
oldest events leaves a first survivor whose `prev_hash` points at nothing, and every later
verification would report the log as broken — indistinguishable from tampering.

```sh
# See what would go, without removing anything.
newcloud audit prune --days 365 --dry-run

# Archive, then prune. The archive path is recorded inside the log itself.
newcloud audit prune --days 365
```

`--days` defaults to `audit.retention_days`. With neither set, the command refuses rather than
guessing, because the guess deletes history.

What it does, in order:

1. verifies the whole chain and refuses to prune one that does not verify;
2. writes the events about to be removed to `audit.archive_dir` (default `audit-archive/`), fsynced,
   before anything is deleted;
3. deletes them and records an `audit.prune_marks` row: the last removed event id, the chain head at
   that point, the event count, and the archive path.

Verification then starts from the newest prune mark instead of from genesis, so the surviving chain
is still checked end to end and an unrecorded gap is still detectable. The mark is append-only, like
the audit schema itself.

Nothing schedules this. Put it in cron or a systemd timer; the interval is your retention policy
review, not a default.

Schedule the export used for investigations as well, if your policy requires offline copies:

## Backups and drills

The backup order is **`pg_dump` first, then the blobs**. The restore order is **blobs first, then
`pg_restore`**. The reasons are in
[the restore drill runbook](../../deploy/backup/restore-drill.md); the short version is that
content is written before its row, so the backup order that can only leave an orphan blob is the
safe one.

```sh
deploy/backup/backup-all.sh
```

The script dumps the database, runs restic, restores the blobs, and cross-checks the result. It
writes a manifest; a backup that completes without a database dump is a failure, not a partial
success.

Run the drill after schema changes and on a schedule:

```sh
newcloud drill
```

The drill restores into a scratch database and verifies:

- every table in the live schema is present in the restore (the list is self-checked against the
  schema);
- metadata row counts;
- the audit hash chain;
- the bytes of every restored blob against its content address.

The drill is also tested in reverse: corrupt one event or flip one blob byte and it must fail.

Scale measurement is limited to a 5.5 MiB metadata dump. Restore time is linear within that range,
but there is no validated RTO for a 40 TiB deployment. Do not quote one.

## OIDC key rotation

Key rotation does not require a restart. A client that encounters an unknown `kid` re-fetches the
JWKS. Rotation steps, the nine verification-failure outcomes, and the deliberate omissions are in
[deploy/oidc-key-rotation.md](../../deploy/oidc-key-rotation.md).

Existing sessions are unaffected by rotation: a session is its own credential and never asks the
IdP again.

If the IdP is offline, the process keeps serving. Existing sessions, App Passwords, and WebDAV
continue; only new logins return `503`. Recovery does not require a restart.

## Break-glass

Break-glass is a local administrator for the case where the IdP is unavailable and access must be
restored. It is disabled by default.

```sh
newcloud breakglass hash < /run/secrets/breakglass-password
```

Requirements:

- a username and an Argon2id hash file;
- `allowed_cidrs`; an unrestricted break-glass account is an internet-reachable root login;
- auditing and alerting, both of which are built in.

There is deliberately no web UI for break-glass. The login endpoint is
`POST /api/auth/breakglass`, and the recovery surface stays as small as possible. Use of the
account emits `newcloud_auth_events_total{kind="breakglass_ok"}`, which raises a critical alert;
failed attempts use `breakglass_denied` and raise a warning only after five failures in fifteen
minutes.

## WAL archiving and PITR

Point-in-time recovery depends on three things:

- `archive_mode=on`;
- a working `archive_command` that is idempotent;
- `archive_timeout` short enough to meet the RPO on a quiet system.

The reference Compose file sets `archive_timeout=5min` for a stated RPO of fifteen minutes. Without
it, "RPO <= 15 minutes" is true only on a busy system.

WAL archive failure is silent from the database's point of view: the database keeps serving while
`pg_wal` grows. Three metrics and three alerts make it visible. `failed` answers "did it ever
fail"; `pending` answers "is the disk filling now".

`newcloud drill` does not perform PITR. The procedure is in the restore drill runbook, and it has
been exercised end to end: base backup, delete a row, replay to before the delete, verify the row
returned and the instance promoted.

## Routine checks

| Cadence | Check |
| --- | --- |
| Continuous | `up`, error rate, audit chain age, WAL archive failure, blob GC errors |
| Daily | Backup completed; manifest written; no failed jobs; disk headroom |
| Weekly | `newcloud audit verify`; sample a share download; review break-glass events |
| Monthly | Restore drill; cert expiry; IdP group mapping; retention actually deleting |
| After schema change | `newcloud drill`; re-check the gap table in the implementation record |

## First response to a page

1. Is `/healthz` up and `/readyz` up? The pair distinguishes process from dependency.
2. Is the database reachable and is its disk full?
3. Is the audit chain advancing? `newcloud_audit_chain_head_age_seconds` answers this directly.
4. Is WAL archiving failing? Check `newcloud_wal_archive_failed_total` and the archive disk.
5. Is the IdP reachable? If not, new logins fail while existing sessions continue.

Do not delete a corrupt blob to "clean up". Metadata still references it; deleting it turns
corruption into data loss. Follow the `blob-corruption` section of the runbook.
