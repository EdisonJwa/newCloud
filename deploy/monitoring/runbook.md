# Runbook

Each section is what an alert links to. The rule is that a page should arrive with enough context
that the person receiving it does not have to reconstruct the design at 3am — so every section says
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

5xx means we broke, not that the caller did. Distinguish the two cases before doing anything else:

1. **All routes failing** → almost certainly the database. Check `/readyz`. See
   [database-degraded](#database-degraded).
2. **One route failing** → read the log lines around it; `newcloud_http_requests_total` does not
   carry a route label by design (the route set is large and the cardinality would be unbounded),
   so the log is where the route comes from.
3. **Only WebDAV failing** → `newcloud_dav_conflicts_total` alongside it tells you whether these
   are lock conflicts being miscounted as errors. Conflicts are 409/412 and are normal in a
   multi-client deployment.

## database-degraded

The process answers scrapes while producing frequent 5xx. This combination is the signature of a
database problem: a healthy process with a dead database stays alive and fails queries.

1. `curl http://newcloud.internal:8080/readyz` — it exists to answer this.
2. Check the connection count. Every request path takes a connection, and a leak shows up here
   before it shows up anywhere else.
3. Check the database's own log for `too many connections`, `deadlock detected`, or disk full. A
   full disk manifests as 5xx on every write path, and reads keep working — which looks like a
   code bug until you check.

## audit-stalled

`newcloud_audit_chain_head_age_seconds > 3600` for 15 minutes.

The audit chain is the integrity claim. Its head age growing means no new audit event has been
written.

1. **Is the deployment simply idle?** A quiet instance writes no events, and this alert fires. Check
   the last events before treating it as a fault — this is why the threshold is an hour and not
   five minutes.
2. If there is traffic and no events, audit writes are failing. That is the fail-closed path in
   `deploy/audit-failure-policy.md`: the mutation should have been refused rather than silently
   unrecorded. Look for refusals in the log.
3. Check the `audit.events` table is writable and that the `audit` schema has not been dropped or
   renamed by a botched restore.

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

1. The usual cause is one badly-behaved client polling or writing without honouring locks. The
   per-caller pattern is in the audit log, not in the metrics.
2. Two clients editing the same tree is the other common cause, and the fix is organisational.
3. Nothing here needs action at the server. The alert is at `severity: info` for that reason.
