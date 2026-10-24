# Runbook

Each section is what an alert links to. The rule is that a page should arrive with enough context
that the person receiving it does not have to reconstruct the design at 3 a.m. — so every section says
what the signal means, what is *not* the problem, and the first three things to check.

The metrics named here are the ones the server actually publishes. `internal/metrics/metrics.go`
is the source, `internal/server/metricswiring_test.go` enforces that everything there is wired, and
`deploy/monitoring/alerts.yml` is validated by `promtool check rules`.

## How to look at a deployment

```sh
# Is it up and is its database reachable?
curl -fsS http://newcloud.internal:8080/healthz   # the process answered
curl -fsS http://newcloud.internal:8080/readyz    # it can also reach the database

# What does it say about itself?
curl -fsS http://newcloud.internal:8080/metrics | grep ^newcloud_

# What has it been doing? (the audit log is the record, not the metrics)
newcloud audit --limit 50        # or: GET /api/audit/events
```

A useful first distinction: **`/healthz` 200 and `/readyz` not 200** means the process is fine and
the database is not. Almost every alarming metric resolves to that pair.

## NewCloudDown

`up{job="newcloud"} == 0` for 2 minutes.

1. Is the process running? `systemctl status newcloud` or `docker compose ps web`.
2. If it is running but not answering scrapes, check what port it bound and whether the scrape
   target is right. A container bound to `127.0.0.1` inside its namespace is not reachable from a
   scraper on another host — the commonest false alarm.
3. If it is not running, read the log. Startup fails loudly on a bad config or an unreachable
   database, and the message names which. `newcloud migrate` is the usual companion if the schema
   is behind.

Restarting is safe: sessions live in the database, resumable uploads survive in their staging
directory, and no state is only in memory.

## error-rate

More than 5% of responses are 5xx for 10 minutes.

5xx means the server broke, not that the caller did. Distinguish the two cases before doing anything else:

1. **All routes failing** → almost certainly the database. Check `/readyz`. See
   [database-degraded](#database-degraded).
2. **One route failing** → read the log lines around it; `newcloud_http_requests_total` does not
   carry a route label by design (the route set is large and the cardinality would be unbounded),
   so the log is where the route comes from.
3. **Only WebDAV failing** → `newcloud_dav_conflicts_total` alongside it shows whether these are
   lock conflicts being miscounted as errors. Conflicts are 409/412 and are normal in a
   multi-client deployment.

## database-degraded

The process answers scrapes while producing frequent 5xx. This combination is the signature of a
database problem: a healthy process with a dead database stays alive and fails queries.

1. `curl http://newcloud.internal:8080/readyz` — it exists to answer this.
2. Check the connection count. Every request path takes a connection, and a leak shows up here
   before it shows up anywhere else.
3. Check the database's own log for `too many connections`, `deadlock detected`, or disk full. A
   full disk manifests as 5xx on every write path, and reads keep working — which looks like a
   code bug until the disk is checked.

## audit-stalled

`newcloud_audit_chain_head_age_seconds > 3600` **and** recent write traffic, for 15 minutes.

The audit chain is the integrity claim. Its head age growing means no new audit event has been
written — but that alone does not distinguish a quiet deployment from a broken one, so the rule also
requires POST/PUT/PATCH/DELETE traffic in the last hour. A quiet instance therefore does not page.

Until 2026-10-24 the gauge was written with a hard-coded `0` (once a day, and only when anchoring was
configured), so this rule could not fire at all; see `docs/reference/review-2026-10-24.md`, H6. If you
run an instance with no write traffic by design, raise the threshold rather than silencing the rule —
the gauge is now real.

1. **Is there write traffic?** The rule already requires it, so assume yes. Compare
   `sum(rate(newcloud_http_requests_total{method=~"POST|PUT|PATCH|DELETE"}[1h]))` against the event
   count.
2. If there is traffic and no events, audit writes are failing. That is the fail-closed path in
   `deploy/audit-failure-policy.md`: the mutation should have been refused rather than silently
   unrecorded. Look for refusals in the log.
3. Check the `audit.events` table is writable and that the `audit` schema has not been dropped or
   renamed by a botched restore.

## backup-stale

`time() - newcloud_backup_last_success_timestamp_seconds > 36h`.

The series comes from `deploy/backup/backup-all.sh` through node_exporter's textfile collector
(`BACKUP_METRICS_FILE`), not from the application: the backup runs from cron and the app never sees
it. If the series is absent the alert is silent — that is why the collection must be configured
before the backup is relied on.

1. Run the backup by hand and read the exit code. `pg-backup.sh` exits `3` when the dump is valid but
   does not contain this application's tables, which is what a dump of the wrong database looks like.
2. Check the manifest in `$BACKUP_DIR/snapshot-*.manifest`: it records the dump file and the **restic
   snapshot id**, not the word `latest`. A missing `blob_snapshot` line means the blob copy never ran.
3. Do not delete old backups to "make room" before the new one succeeds.

## backup-restore-unproven

`time() - newcloud_backup_last_drill_timestamp_seconds > 100d`.

`newcloud drill` publishes this on a PASS when `BACKUP_METRICS_FILE` is set. A quarter without a drill
means the restore path is a rumour (INV-08). Run it: `deploy/backup/restore-drill.md`.

## audit-verify-failed

`increase(newcloud_audit_anchor_verify_total{result="failed"}[15m]) > 0`.

This is the signal the whole audit design exists to produce. **Do not restart anything and do not
run another migration** — capture the state first, because a restart can roll the chain forward and
destroy the evidence.

1. `newcloud drill` reports the chain verdict; run it against a copy, not the live database.
2. Record the current head hash and the newest anchor:
   ```sql
   SELECT day, encode(head_hash,'hex') FROM audit.anchors ORDER BY day DESC LIMIT 1;
   ```
3. Compare against a backup taken before the first failure timestamp.
4. Treat the deployment as untrusted for writes until the divergence is explained. The chain
   verifying is the property that makes the log worth keeping.

## blob-gc

`increase(newcloud_blob_gc_errors_total[1h]) > 0`.

1. Blobs were meant to be deleted and were not, so disk usage will exceed what the accounting says.
   Check free space first — a full disk is a much more urgent problem than the GC error that
   revealed it.
2. If the backend is S3: credentials, bucket policy, and whether the endpoint is reachable. A
   credential rotation that only updated one of the two roles shows up here.
3. GC is safe to re-run. It is idempotent; a blob whose references are gone is deleted, and one
   that is still referenced is not. Re-running after fixing the cause is the whole remedy.

## jobs

`newcloud_jobs{state="failed"} > 0` or `{state="pending"} > 1000` for 30 minutes.

1. **Failed jobs never retry.** They are terminal by design, so a failed job is work that is simply
   not happening. The queue table is inspectable and the drill covers it.
2. **A large pending count is only a problem if it is not shrinking.** The alert is a count, not a
   rate, precisely because it waits 30 minutes: a backlog draining from 5000 to 100 is healthy.
3. If the backlog is growing and nothing is running, the worker is not scheduled. Check
   `newcloud_uptime_seconds` — a small value means the process restarted, which is consistent with
   a crash loop that also stops the worker.

## uploads

More than 20% of uploads failing for 15 minutes.

Upload failure is about storage far more than about clients.

1. Check the disk. `newcloud_blob_gc_errors_total` alongside this is a strong hint.
2. If the backend is S3, the 5xx rate on the upload routes and the GC error count usually move
   together — both are the same credential or endpoint problem.
3. Quota exhaustion is not an upload failure: it is a 403 with its own error code, and it does not
   count here. If the numbers disagree, read one failing request's response body.

## dav-conflicts

Over 10% of WebDAV requests are 409/412 for 30 minutes.

Not an outage. This is information.

1. The usual cause is one badly-behaved client polling or writing without honoring locks. The
   per-caller pattern is in the audit log, not in the metrics.
2. Two clients editing the same tree is the other common cause, and the fix is organizational.
3. Nothing here needs action at the server. The alert is at `severity: info` for that reason.

## breakglass

`newcloud_auth_events_total{kind="breakglass_ok"}` increased, or
`{kind="breakglass_denied"}` increased more than 5 times in 15 minutes.

Break-glass is the local administrator that works when the IdP does not (§3.3). It exists so a
deployment can be recovered during an outage, which also makes it the credential an attacker most
wants: it bypasses the IdP entirely and cannot be revoked there.

**Use is expected to be rare and planned.** Investigate immediately — this is the one alert here
that is worth waking someone for.

1. Identify who and from where. The audit log has both:
   ```sql
   SELECT created_at, actor_email, actor_ip, user_agent, metadata
     FROM audit.events
    WHERE action = 'auth.breakglass.used'
    ORDER BY created_at DESC LIMIT 20;
   ```
2. Confirm it against the change record. If nobody claims it, treat the credential as compromised:
   rotate the password file, restart the service, and audit everything the account did afterwards
   (`actor_email = 'breakglass'`).
3. **Check what it did not do.** Break-glass is deliberately *not* a superuser (§3.3): it has
   recovery and configuration rights but no implicit read access to other users' private files. If
   the audit shows it reading user content, that is a privilege bug, not a credential incident, and
   is worth its own report.

For a burst of `breakglass_denied`:

1. The `metadata.reason` field separates the cases — `network_denied` means the caller was outside
   the allowed range (which is the control working), `bad_password` means someone knew the account
   name.
2. Repeated `bad_password` from one address is a guessing attempt. Confirm the source network
   restriction is as tight as intended before rotating the password, because rotating it will not
   stop the attempts.

## What is deliberately not alerted

- **A single `breakglass_denied`.** Wrong passwords happen and the account is rate-limited by the
  network restriction. Only a burst is worth waking someone.
- **Per-user login failure rate.** It would be high-cardinality and the signal is mostly expired
  sessions. The audit log answers the question when it is actually asked.

## wal

WAL archiving is failing, backing up, or has stalled.

This is the quietest way for a deployment to lose its disaster-recovery guarantee. When
`archive_command` fails, PostgreSQL **keeps serving**. It logs, it retries, and it keeps accepting
writes — so the service looks perfectly healthy while the recovery point slides backward and
`pg_wal` grows.

1. Look at the numbers first:
   ```sql
   SELECT archived_count, failed_count, last_failed_wal, last_failed_time
     FROM pg_stat_archiver;
   ```
   `failed_count` rising with `archived_count` flat is the signature. `last_failed_wal` names the
   segment that could not be written.

2. Check the destination, not the database. The archive path is usually a separate mount
   (`/mnt/wal-archive` in the compose file) and the usual causes are, in order: **the mount is
   gone**, **the disk is full**, **permissions changed**, **the host is unreachable**.

3. Watch `pg_wal` while fixing it:
   ```sql
   SELECT count(*) FROM pg_ls_waldir();     -- each segment is 16 MB
   ```
   These segments cannot be recycled until they are archived. A backlog that keeps growing will
   fill the data disk, and *then* PostgreSQL stops accepting writes — an outage whose cause is an
   archive command that failed hours earlier.

4. After fixing the cause, do nothing further: the archiver retries on its own and the backlog
   drains. Confirm by watching `archived_count` rise.

**What is lost while this alert is firing:** every write since the last successfully
archived segment is not recoverable by point-in-time recovery. It is still in the database — this
is not data loss yet — but if the disk dies now, the restore point is wherever the archive stops.
Take a fresh `pg_basebackup` once archiving is healthy again, so the base stops depending on a
long replay.

## What is deliberately not alerted

- **A single failed archive attempt.** PostgreSQL retries by design; one failure followed by a
  success is a flaky NFS mount, not an incident. The alert needs a sustained increase.
- **`pending > 0`.** A handful of segments waiting is normal — a segment is archived when it fills
  or when `archive_timeout` fires. Only a backlog that is not draining is worth waking someone.

## blob storage operations

Two things only an operator can do, because they are outside what the process sees.

### Stranded multipart uploads

A multipart upload claims storage for its parts as they arrive, and that storage returns only when
the upload completes or is explicitly aborted. A client that gives up — dropped connection, crash, a
5xx from the endpoint — leaves the parts behind. On S3 they are billed and reclaimed only by a
lifecycle rule; on a self-hosted backend they are simply leaked disk, invisible in any object
listing.

**Measured (round 46):** with every part upload failing, `Put` returned an error and left **zero**
stranded multipart uploads — minio-go aborts its own upload before returning. So the common case is handled,
and this section exists for the cases it is not: a process killed mid-upload, or a client that
disappears between parts.

Check for them at the backend, since the application cannot:

```sh
# S3 / MinIO: list in-progress uploads
aws s3api list-multipart-uploads --bucket <bucket>
# MinIO client
mc admin trace --verbose <alias>
```

Remedy: abort them. They are unreferenced by definition — nothing has a key for them yet — so
aborting is always safe and never loses a committed file.

```sh
aws s3api abort-multipart-upload --bucket <bucket> --key <key> --upload-id <id>
```

A lifecycle rule that aborts incomplete uploads after a day is the standing fix and costs nothing.

### A known upstream race in minio-go's multipart retry

While adding these tests, a genuine data race was found in **minio-go v7.4.1**, not in newCloud.

`hookReader` wraps the caller's reader so retries can rewind it. In the multipart path
(`api-put-object-streaming.go`), when a part upload is retried, the retry goroutine writes the
hook buffer while the previous attempt is still reading it:

```
WARNING: DATA RACE
Write at ... by goroutine N:
  minio-go/v7/hook-reader.go:50
  minio-go/v7/api.go:747
  minio-go/v7/api-put-object-streaming.go:727
```

**Reachable in production**, not just in tests: it needs an object larger than the 16 MiB part size
and one retried part — that is, a normal large upload crossing a transient S3 error. `NumThreads: 1`
does not avoid it; the race is in the retry path, not in concurrency.

**What it means in practice.** Two goroutines race on the same buffer, so the bytes sent on a retried
part may not be the bytes that were read from the file.

**A correction to what this section said first.** The earlier version claimed the local hash check
contained the damage — that a corrupt transfer would be rejected with `ErrHashMismatch` and never
stored. That is not what the check does. `Put` hashes the **input reader** and compares it to the
expected address *before* handing anything to minio; it never looks at what minio actually sent, and
there is no read-back afterwards. So the hash check protects against a corrupt *source*, not against
a transport that garbles bytes mid-flight. If the race ever did corrupt a part, nothing at write time
would notice: the object would land under a valid address with the wrong bytes.

**Measured (round 47), and the honest conclusion.** Under `-race`, 48 MiB uploads with four injected
part failures reproduce the race reliably, and in twelve attempts the stored bytes hashed correctly
every time. So the window is narrow enough that it has not produced corruption in this test — which
is not the same as proving it cannot. The residual risk is real and unquantified.

**What actually limits it.** Content addressing bounds the blast radius rather than preventing it: a
corrupted blob sits under an address it does not match, so `newcloud drill` — which compares restored
bytes against the hash recorded in `blobs.sha256` — reports it as a failure rather than serving it
silently. That is detection after the fact, not prevention.

**What to do about it.** Track upstream; v7.4.1 is the current release and this is not fixable from
the call site (hiding `io.Seeker` to suppress retries does not help — minio buffers and retries
anyway, measured). A deployment that sees repeated `ErrHashMismatch`, or a drill failure naming a
blob, on large uploads against an unstable endpoint should treat it as this race and re-upload from
the client rather than investigate the file.

### Orphan blobs after a restore

Restoring blobs before the database (see `deploy/backup/restore-drill.md` §2.6) is the safe order,
and its cost is blobs with no row: the window's writes land as bytes that no metadata references.

These are reclaimed by the normal collector, because it is metadata-driven — it selects rows from
`blobs` and deletes those that are unreferenced and past their grace period. **A blob file with no
row at all is invisible to it**, which is exactly why the restore order is safe, and also why an
orphan from *outside* the database's knowledge is never collected automatically.

If a restore leaves the store larger than the metadata accounts for, reconcile it by comparing the
blob root against `SELECT encode(sha256,'hex') FROM blobs` and removing the difference **after
confirming the restore is complete and the audit chain verifies**.

## blob-corruption

The integrity scrub found blobs whose bytes do not match their address, or blobs the metadata
references that storage does not have.

Content addressing makes this check exact: a blob's file name **is** the SHA-256 of its contents, so
a mismatch needs no interpretation. The scrub logs both hashes.

**Corrupted (`newcloud_blob_scrub_corrupt`).**

Storage damaged the data — a bad sector, a failed disk, or a transfer that wrote the wrong bytes.
(A known upstream minio-go race can garble a retried multipart part; see
[above](#a-known-upstream-race-in-minio-gos-multipart-retry).)

1. **Do not delete the blob.** The metadata references it, so deleting turns corruption into data
   loss and removes the evidence.
2. Restore that blob from a backup — `restic restore` for a specific path, or the dump for the
   database side.
3. Verify after restoring: its hash must equal its name, which is a one-line check and needs no
   tooling.
4. If several blobs are corrupt, check the disk (`smartctl`, kernel log) before restoring. Restoring
   onto failing hardware repeats the exercise.

**Missing (`newcloud_blob_scrub_missing`).**

Something removed content the database still points at. The usual causes, in order: an interrupted
restore (blobs are restored before the database — see
`deploy/backup/restore-drill.md` §2.6 — so a restore that stopped halfway leaves rows without
bytes); a collector run against the wrong database; or manual deletion.

Restoring the blob is always safe. Deleting the row is not: it discards the file.

**Why the scrub samples rather than sweeps.** Reading every byte of a 40 TiB store on a timer is a
second full-time workload. The scrub verifies a bounded number of blobs per pass and logs the total
it read, so its cost is visible. A pass that finds nothing is not proof of health — it is the absence
of evidence in that sample, which is why the interval matters more than the limit.