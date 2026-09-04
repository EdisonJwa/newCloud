# Audit storage failure policy (T-16)

The requirement asks that audited operations follow a fail-open or fail-closed policy agreed in
advance. This document is that policy. It exists because deciding at 3 a.m. during an incident is
not a policy.

## The decision: fail-closed

**If an operation cannot be written to the audit log, the operation does not happen.**

The alternative — fail-open, continuing to serve writes during an audit outage — is tempting because
it sounds kinder to users. It is the wrong choice for this platform:

- The audit log is a security control (INV-09), not telemetry. It records who downloaded what, which
  permission changed, which session was revoked. A log that silently stops recording under pressure
  is worse than no log, because people make decisions based on it.
- Fail-open makes **filling the disk the cheapest attack on the system**. An attacker who can
  exhaust the disk then operates entirely unlogged, and the log's absence is indistinguishable from
  "nothing happened".
- The chain would still verify. Every recorded event would be correctly chained and every
  verification would pass; the missing events leave no trace at all. That is the worst property a
  tamper-evident log can have.

## What fails and what keeps working

| Operation | During an audit outage |
| --- | --- |
| Upload, delete, move, rename, mkcol | **Refused** — `503 audit_unavailable`, nothing modified |
| Permission and ACL changes | **Refused** |
| Share creation and revocation | **Refused** |
| Login and session creation | **Refused** |
| Session revocation | **Refused** (it is itself audited) |
| **Reads: list, download, metadata, DAV GET/PROPFIND** | **Keep working** |
| **Audit verification and export** | **Keep working** |
| **Health and readiness** | **Keep working** |

Reads deliberately stay up. An operator investigating "writes are failing" needs to read the data
and the audit log; turning an audit outage into a total outage would remove the tools needed to
diagnose it. `TestReadsSurviveAnAuditOutage` asserts this.

## What the client sees

```text
HTTP/1.1 503 Service Unavailable
{"error":"audit_unavailable",
 "message":"the audit log is unavailable, so the change was refused and nothing was modified"}
```

503 rather than 500 because the request is well-formed and the data is intact — the *recorder* is
down, and retrying after it recovers will succeed. The distinction matters operationally: a 500
sends an operator to look at the data, and the data is fine.

The underlying cause is preserved in the error chain, so the server log carries the real reason
(`no space left on device`, for example, or `relation "audit.events_default" does not exist`) rather
than only "audit unavailable".

## Operator runbook

1. `curl -s http://localhost:8080/readyz` — the payload's `checks` reports the metadata and content
   stores. Audit health is separate and appears as write failures, not in readiness.
2. `newcloud audit verify` — a *failing* verification is a different incident (tampering or
   corruption). This policy covers an audit store that cannot accept new rows at all.
3. Check the disk: the most common cause is the volume holding PostgreSQL reaching full. The audit
   table is append-only and never VACUUMs away rows, so it grows monotonically.
4. Check that the table exists and is writable: a dropped or revoked `audit.events_default` presents
   the same symptom.
5. Once writes succeed again, the backlog is *not* replayed. **Refused operations were never
   accepted**, so there is nothing to reconcile; the client saw an error and can retry. Operators
   should expect this consequence of fail-closed: during the outage, some user actions simply did
   not happen.

## What this policy does not cover

- **The audit subsystem becoming slow rather than unavailable.** Every write pays the chain cost in
  its own transaction, so a slow audit store slows every write. There is no timeout-and-fail
  behavior today, distinct from slow-and-wait.
- **Partial writes inside the audit writer.** The append runs inside the caller's transaction, so it
  either commits with the mutation or rolls back with it. A failure *after* commit is a different
  problem that this policy does not address.
- **Backlog replay.** By design there is none (see step 5). If an operator needs the refused
  actions, they came from users who saw errors.

## Where this is enforced

| Layer | File | Test |
| --- | --- | --- |
| The policy itself | `internal/store/pg.go` (`pgTx.Audit`) | `TestAuditFailureRollsBackTheMutation` |
| Reads stay up | — | `TestReadsSurviveAnAuditOutage` |
| Misconfiguration is loud | `internal/store/pg.go` | `TestMissingAuditWriterIsAlsoFailClosed` |
| Client-visible status | `internal/server/files.go` (`writeStoreError`) | `TestAuditOutageReturns503` |
