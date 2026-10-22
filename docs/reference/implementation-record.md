# Implementation record: contracts, evidence, and remaining gaps

**Status:** This file records which cross-module contracts are implemented, which test pins each one, and
what remains undone. It is the evidence file: every implemented claim points at a runnable test or a
command that can be run by hand, and every unverified property is stated next to the results that do pass.

**Input:** [functional requirements and gap analysis, v0.2](./functional-requirements.md).

## 1. Contracts implemented, round by round

Newest first. Each round gives the finding that changed the code, the tests that pin it, and a lesson only
where the lesson keeps the defect from returning. A round that covered two topics has two headings.

### Round 47: A conclusion that did not hold, and the real gap it pointed at

Round 46 claimed that the local hash check reduced the upstream race from "polluted storage" to "failed
upload". Wrong: `Put` hashes the input reader before the bytes reach minio, never checks what minio writes,
and nothing reads the object back afterwards. The check defends against corrupt source data, not against a
corrupted transfer, so a damaged part lands at the correct address holding the wrong bytes.

- `-race` reproduces the race under a 48 MiB upload with four injected failures;
- twelve attempts read back correct bytes, a narrow window that does not exclude corruption;
- hiding `io.Seeker` changes nothing, because minio buffers and retries on its own.

Content addressing bounds only the blast radius: a mismatched blob makes `newcloud drill` fail rather than
serve it, and that bound had applied only during the quarterly drill. `worker.ScrubBlobs` now samples live
blobs each cycle and compares them to their content address. Six tests: corruption is detected (deleting
the comparison turns it red), healthy blobs are not reported, a missing blob is distinguished from a
corrupt one, the limit is honored including 0 for no sampling, cancellation interrupts a run, and an empty
store reports nothing. Three alert rules and a `## blob-corruption` runbook section, including "do not
delete the corrupt blob — metadata still references it, so deleting turns corruption into data loss".

Lesson: the earlier claim named a real mechanism, applied to the wrong cause; a reader who verifies that
the mechanism exists stops there.

### Round 46: Multipart uploads, and a real upstream race

Every part failing: `Put` errors, no object remains, `PendingMultipart()` is 0 — minio-go aborts by itself.
With a payload above the 16 MiB part threshold, `-race` reports:

```
WARNING: DATA RACE
Write at ... by goroutine N:
  minio-go/v7/hook-reader.go:50
  minio-go/v7/api.go:747
  minio-go/v7/api-put-object-streaming.go:727
```

`hookReader` wraps the caller's reader so a retry can rewind it; during a part retry the retrying goroutine
writes the hook buffer while the previous attempt reads it. Reachable in production: any object over
16 MiB with one retried part, a normal large upload crossing a transient S3 error. `NumThreads: 1` does not
avoid it, because the race is on the retry path, and the harm is scrambled part content rather than a
crash. Round 46 attributed the defense to the pre-upload hash check; round 47 showed that the check does
not cover transport corruption, so the race can land a mismatched blob and only the scrub or the drill
detects it. The race cannot be fixed here: the test moved to the single-PUT path, and the runbook records
it as an upstream limitation with a diagnostic — repeated `ErrHashMismatch` on large files against an
unstable endpoint is this race, and the answer is a client retry. Also fixed: a missing blank line after
the `## wal` heading.

### Round 45: G3's third item, injecting S3 errors

S3 error injection had never run, although `s3fake` shipped with `FailNextOn`. Four of the first five tests
failed because of the injection, not the product: minio-go retries 5xx by default, so the first attempt
consumed "next request fails" and the retry succeeded, asserting the success path under the failure path's
name. `FailAlwaysOn` (fail until `ClearFailures`) fixed it; the four tests then passed at 4–6 seconds each,
which is the retries running and proof that the injection arrived.

| Test | Property |
| --- | --- |
| `TestPutFailsCleanlyWhenTheBackendErrors` | a failed upload reports an error and leaves no object, or the next `Put` at that address takes the idempotent shortcut |
| `TestPutRejectsCorruptTransferBeforeCommitting` | `Put` hashes the input before uploading, so a source that does not match its address is refused before anything is written (`ErrHashMismatch`); this does not detect corruption after the hash is taken |
| `TestStatFailureIsNotMistakenForAbsence` | a failed `Stat` is not NotFound, or `Put` overwrites a key it never checked |
| `TestOpenFailureIsReported` | a failed head request is reported by `Open`, not deferred to a reader that fails halfway |
| `TestDeleteFailureIsReported` | a failed `Delete` errors, or GC records space that was not reclaimed |

`TestOpenSucceedsAndFailsOnFirstRead` records a limitation: HEAD succeeds and GET fails, so `Open` returns
and the error appears on the first Read, after 200 and part of the body have gone out. Fixing it would mean
buffering the whole object, the wrong trade for streaming; the defenses are `Content-Length` and the blob
hash.

Lesson: deleting the tested behavior must turn a test red, and an injected fault must actually reach the
code. `FailNextOn` documented "test backend error handling" and injected nothing against a client that
retries.

### Round 44: G3's crash paths, process interruption does not corrupt data

"Upload sessions are held in memory and do not survive a restart" is a capability gap, not an integrity
gap; keeping them apart prevents the wrong fix (persisting sessions) while the property goes unverified.

**a) A crash mid-part.** The staging file is partial and has no metadata. It cannot be read, because the
local backend derives the path from the hash (`<root>/<hex[0:2]>/<hex[2:4]>/<hex>`) and staging lives in
`<root>/tmp`. There is no path from a hash to that directory, so a partial upload is structurally
unreachable rather than guarded by a check that must remember to run.

**b) A crash during commit.** Content lands first and the row second (round 38's invariant). A crash
between them leaves an orphan blob that GC collects; a crash inside the transaction leaves neither. No row
ever exists whose bytes are missing.

Four mutation-verified tests: `TestPartialUploadIsUnreachableByHash` (a `root/tmp` file named after its own
hash must not open), `TestCommittedBlobIsAddressable` (a committed blob must open, or "nothing opens" would
also pass), `TestStagingDirectoryIsOutsideTheAddressableTree`, and `TestStartupRemovesOrphansThatNoSessionOwns`.
The one red light came from a wrong fixture: an orphan named `left-by-a-crashed-process` failed because
cleanup deletes only `part-` files and deliberately leaves strangers alone — an upload service pointed at a
directory must not delete a backup target. `part-crashed-process` passed.

### Round 43: Auditing the gap list, and an ordering defect

Three rows still said PITR had not been rehearsed while round 41 had recorded `recovery stopping before
commit`. The manual search found two; the third appeared when the new guard ran.
`TestGapTableDoesNotClaimMeasuredThingsAreUnmeasured` checks for "the list says it was never done, the
document records that it was", scanning table rows only and looking for evidence only inside round
sections. The pairs: PITR against `recovery stopping before commit`, restic restore against `no errors were
found`, large-database timing against the scale points. Restoring a claim turns it red and names the round.

Also fixed, from T-01: the chunk route parsed `Content-Range` before checking identity, which let a caller
distinguish "the endpoint parsed my request" from "this session is not yours". `Uploads.Get` now runs first,
then the range, then the size.
`TestUploadChunkAuthorisesBeforeParsing` sends no header, a malformed header, and a valid header against a
session owned by someone else; all three must return 404, and the old order returns 400 for the first two.
The rule: "authenticate, then parse" has to be expressible as a test.

### Round 42: WAL archiving failures are silent

With `archive_command` always failing:

```
after round 1 : 5 wal files, 49 MB
after round 2 : 6 wal files, 65 MB
after round 3 : 7 wal files, 81 MB
archived successfully: 0
```

The database worked throughout; `pg_wal` grew monotonically and nothing left the host, with
`LOG: archive command failed with exit code 1` and finally `WARNING: archiving ... failed too many times, will try again later`. The worst failure shape available: healthy service, no disaster recovery. RPO
becomes "whatever is on that disk", and the only symptom is `pg_wal` filling the data disk hours after the
cause. newCloud saw nothing: no metric, no alert rule.

`worker.SampleWALArchive` reads `pg_stat_archiver` and counts `pg_ls_waldir()` backlog. Three metrics —
`newcloud_wal_archived_total`, `newcloud_wal_archive_failed_total`, `newcloud_wal_segments_pending` — are
sampled each cleanup cycle, answering whether archiving works, whether it failed, and whether the disk is
filling; only the third predicts the outage.

```
healthy cluster:  archived=0 failed=0  pending=9 lastFailed=""
failing cluster:  archived=0 failed=19 pending=6 lastFailed="000000010000000000000001"
```

Three rules (`promtool` passes 15): rising failures critical, backlog above 64 segments critical (over a
gigabyte at 16 MB per segment), one hour with zero archives but a backlog a warning. The `## wal` runbook
section has three diagnostic queries, four causes in likelihood order (mount point gone, disk full,
permissions changed, host unreachable), and one sentence that must be written down: every write made while
the alert fires is unrecoverable through PITR. `pg_ls_waldir()` needs privileges; without them the counters
still report and `Pending` keeps its previous value rather than 0, because "cannot read" and "no backlog"
must differ.

### Round 41: PITR, from one sentence to a measured procedure

`docker-compose.yml` had `archive_mode=on` and `restore-drill.md` claimed a 15-minute RPO, and nothing said
how to run a point-in-time recovery. On an `archive_mode=on`, `archive_timeout=5s` instance: insert two
rows and record the time; `pg_basebackup`; delete the second row; unpack into a new data directory with
`restore_command`, `recovery_target_time`, `recovery_target_action=promote`, and `touch recovery.signal`;
start. The log reads `recovery stopping before commit of transaction 742`, `select id from probe` returns
**1, 2**, and `pg_is_in_recovery()` is `false`.

Running it exposed a missing setting: `archive_timeout` was absent, and PostgreSQL archives only when a
16 MB segment fills or the timeout fires, so "RPO of 15 minutes" held only on a busy system.
`archive_timeout=5min` was added — not 15, because 15 minutes is the whole RPO budget and transfer and
restore need part of it. Also confirmed: `test ! -f /archive/%f && cp %p /archive/%f`. The `test ! -f` is
not redundant, because PostgreSQL retries a failed `archive_command` forever, and a command returning
non-zero on an existing target would retry while `pg_wal` grows until the disk fills.

Three configuration tests: `TestComposeEnablesContinuousWalArchiving` (`archive_mode=on` and a timeout
shorter than the RPO), `TestArchiveCommandIsIdempotent`, and `TestPITRProcedureIsDocumented` (the document
must contain `pg_basebackup`, `recovery_target_time`, `recovery_target_action`, `recovery.signal`, and
`restore_command`). One self-inflicted bug: the timeout check compared strings, so `"5" >= "15"` was true
and the test failed against a correct configuration.

### Round 40: Running the backup scripts for the first time

The scripts had never been executed; the tests checked ordering and configuration reading, not whether the
scripts run. With `pg_dump` (scoop `postgresql` 18.6) and `restic` installed, the chain ran on real data.

The documented order was prose: `pg-backup.sh` and `blob-backup.sh` were each correct and unaware of each
other, and which half a cron entry ran depended on someone having read a paragraph.
`deploy/backup/backup-all.sh` now does dump, then blobs, with a manifest between them. The order became
executable instead of remembered.

Running it found a real defect: `pg-backup.sh` called bare `pg_dump`, which reads `PG*` variables and
ignores `DATABASE_URL`, so it connected to `localhost:5432` and reported connection refused while the
database was on 15437; elsewhere it could silently dump the `postgres` database. It now uses
`pg_dump --dbname="$CONNECTION"`, which fails when the connection string is missing.

| Step | Result |
| --- | --- |
| dump | 88 508 bytes, readable by `pg_restore --list`, contains the `audit` schema |
| manifest | dump path, start and end times, blob snapshot |
| restic backup / check | 8 blobs, 4 521 bytes, `no errors were found` |
| restic restore | 8 of 8, each SHA-256 matching its file name |
| `pg_restore` after the blobs | 55 nodes, 200 audit events |
| cross-check | 0 blobs required by the restored metadata are missing from the copy |

A test written wrongly: `TestBackupAllRunsTheDumpBeforeTheBlobs` searched the whole file and matched the
header comment explaining the order, so moving the blob call before the dump still passed. Restricting it
to executable lines turned the mutation red — the fourth instance of "deleting the tested behavior still
passes", all of them assertions on the presence of text rather than on where the behavior sits.

### Round 39: Downgrading "order matters" to an accurate statement

Round 38 also said the reversed restore order loses data. That overstated it, and overstatement in that
direction sends people looking for data loss that is not there.

| Order | During the window | Afterwards |
| --- | --- | --- |
| blobs, then `pg_restore` | no metadata, so nothing can be read | normal |
| `pg_restore`, then blobs | metadata names files whose bytes have not arrived; reads fail with `blob not found` | also recovers |

Both converge and neither corrupts data. Blobs first is recommended because it fails closed, not because
the reverse is irreversible: a wrong backup order is permanent, a wrong restore order is temporary.

"Both are safe" depends on GC being metadata-driven: `CollectableBlobs` walks
`FROM blobs WHERE refcount = 0 AND collect_after <= now()` and never scans directories, so restored bytes
with no row are invisible to it. If collection ever scanned directories, "blobs first" would delete the
restore site.

Six mutation-verified tests: `TestUnreferencedBlobFileSurvivesCollection`,
`TestPGCollectorIgnoresBlobFilesWithNoRow` (the same fact against real PostgreSQL),
`TestPGCollectorHonoursTheGracePeriod`, `TestPGCollectorSkipsReferencedBlobs`, `TestGCIsMetadataDriven`
(source-level: `FROM blobs b`, no `ReadDir` or `WalkDir`), and `TestRestoreOrderAdviceIsNotOverstated`.

`TestPGCollectorSkipsReferencedBlobs` was wrong twice. A plain upload with the node join removed still
passed, because `CreateFile` increments `refcount` and the join was redundant — a test that passes when the
guard is deleted tests something else. The second version forces `refcount` to 0 while the node still
points at the blob, a state only a refcount bug produces; removing the join now fails with "`refcount`
alone is not enough". A control asserts that the blob does become collectable once the node row is deleted.

### Round 38: Concurrent writes inside the backup window (§11.2 item 4)

A backup is a dump and a blob copy taken at two moments and cannot be atomic. Whether a write inside the
window leaves a dangling reference depends on the order, and which order is safe depends on one invariant.

The first conclusion was backwards: blobs first, reasoning that the older artifact is safer. The invariant
is content first, row second (`Content.Put` before `publishFile` in `files.go`), which reverses the orders:

| Order | A write inside the window | After restore |
| --- | --- | --- |
| `pg_dump`, then blobs | bytes in the copy, row not in the dump | an orphan blob, collectable |
| blobs, then `pg_dump` | row in the dump, bytes land after the copy | metadata points at missing content and every read fails; the restore looks successful until someone opens the file |

Orphans are collected; a dangling reference has no repair. The order is therefore dump first, then blobs.
That reversed paragraph was nearly written into `restore-drill.md`, and documentation pointing the wrong
way is worse than none: it reads like advice and produces the failure it claims to prevent.

Four tests: `TestDumpBeforeBlobsLosesNoReference` (every dump record has bytes in the copy, and the window
write leaves only an orphan), `TestBlobsBeforeDumpCanLoseAReference` (constructs the bad state and asserts
it is bad, or the safe-order test would pass even if the orders made no difference),
`TestBlobIsWrittenBeforeItsRow` (source check; inserting `publishFile` before `Content.Put` turns it red),
and `TestBackupDocsStateTheRightOrder`. Both invariants are tested because a change to either flips the
other's correct value.

### Round 37: Three scale points, and an arithmetic error of three orders of magnitude

The gap list said "78 KiB takes about 1.2 seconds, which cannot be extrapolated to 40 TiB". True, but at
the wrong question: what matters is whether cost grows smoothly with data volume or falls over past a
threshold. `tools/seedscale` (through the real store, so the audit chain is real) ran `newcloud drill` at
three volumes:

| Dump | Rows | Elapsed | Seconds per MiB |
| --- | --- | --- | --- |
| 130 KiB | 1 171 | 1.19 s | 9.38 |
| 1.2 MiB | 22 301 | 8.32 s | 6.94 |
| 5.5 MiB | 110 401 | 40.78 s | 7.41 |

All three passed, with audit chains of 1 000, 20 000, and 100 000 events and `verified=true`. Seconds per
MiB stayed flat across a 43-fold range, which excludes the knee a single measurement cannot rule out.

The text then claimed "linear extrapolation gives about 82 hours for 40 TiB"; the arithmetic gives about
92 000 hours, three orders of magnitude off, and the difference inverts the conclusion: 82 hours reads as
close to a 24-hour RTO target, while 92 000 says linear extrapolation is the wrong approach entirely.
`TestDrillScaleMeasurementsAreConsistent` recomputes the extrapolation from the table and fails beyond 20
percent; restoring the wrong figure reports `a factor of 985`. Arithmetic in prose cannot be reviewed by
eye, which is how it had been reviewed until then. The first `seedscale` also wrote metadata without blob
content, and the drill failed at once — `the restored copy cannot serve it: blob: not found`, 100 files,
0 readable.

### Round 36: break-glass alerts on use

The gap row for §3.3 break-glass said "no UI", a diagnosis wrong in two directions. The requirement is
"strong authentication, credentials kept offline, an alert after use, mandatory audit, and periodic
recovery drills": the alert did not exist, because a successful login writes `auth.breakglass.used` and a
refusal `auth.breakglass.denied`, and nothing alerts on the audit table, since the scraper reads
`/metrics`. And "no UI" was the wrong thing to build — break-glass is the recovery entry for a failed IdP,
and a web interface only widens the exposure.

`newcloud_auth_events_total`, labeled by kind, is recorded at both points, with two rules:

| Rule | Expression | Severity |
| --- | --- | --- |
| `NewCloudBreakglassUsed` | `increase(...{kind="breakglass_ok"}[5m]) > 0` | critical — the first use is the one nobody declared |
| `NewCloudBreakglassAttemptsFailing` | `increase(...{kind="breakglass_denied"}[15m]) > 5` | warning — mistyped passwords are common, only a burst matters |

The kinds stay separate: merged, "someone used it" could never be scheduled, because "someone is guessing"
fires often. `TestAlertRulesNameOnlyMetricsThatExist` went red, reporting that the rule used
`kind="breakglass_ok"` while the server had never emitted it — right, since a labeled counter exists only
after the label is observed — and the fixture was extended. `promtool check rules`: 12 rules pass. The
`## breakglass` runbook section carries the "who, from where" query and "check what it did not do":
break-glass is deliberately not a superuser.

### Round 35: A gap table that had outlived its facts

Two rows contradicted each other: one said `§4.1 ACL interface | API complete, no UI | front end not
connected`, another from round 30 described the screens that existed. Both were true when written; the
first outlived the fact. `§11.1` still said no alert rules shipped after round 33 added
`deploy/monitoring/alerts.yml`. A stale "not done" column sends the next person to rebuild what works.

Two structural tests prevent a repeat: `TestGapTableDoesNotClaimClosedGapsAreOpen` — assertions of the form
"the document says this does not exist, but it does", scoped to the named row, so restoring the stale row
fails with `the gap table still says "front end not connected" ... but
web/src/components/FolderAccess.tsx exists` — and `TestGapTableRowsAreWellFormed` (three columns, a status
and an impact on every row, no duplicated requirement name, since duplication is how the two rows came to
contradict each other).

One real gap closed with it: the SHARE-06 cell said links keep working after a move "but this is not
separately tested". Three tests were added — a link survives a rename and reports the new name, survives a
move with the download still working, and is refused after a rename then trashing — and a mutation that
pretends to cache the old name fails with `the link reports cached-old-name, want the new name`.

### Round 34: A red baseline that was not a code problem

Four tests were red and the cause was not code: Docker Desktop was not running, so the PostgreSQL container
was unreachable. After starting it, all 638 tests passed. "Baseline red" and "code broken" are separate
events.

### Round 33: Alert rules, a runbook, and three metrics that were always zero

`deploy/monitoring/` (`alerts.yml`, `prometheus.example.yml`, `runbook.md`) was added and promtool installed
— and writing the rules exposed three metrics production code had never written. `ObserveUpload`,
`ObserveThumbnail`, and `ObserveBlobGC` were called only from `internal/metrics/metrics_test.go`, so those
series had a correct format and were permanently zero: 11 unit tests green, and the rules written against
them could never fire. A metric nobody writes looks exactly like a healthy one.

The wiring went in where the event happens: `publishFile` with `defer` (shared by simple and chunked
uploads, so it records once per file); `writeUploadError` by error type (409 is a resume, not a failure);
thumbnails at every return point ("not an image" is not a failure, or a PDF directory looks like a broken
renderer); GC at the `sweepBlobs` call site for success and failure, since recording only success leaves the
error counter at 0 and its alert dead.

`TestEveryMetricFamilyIsFedByProductionCode` parses the mutator methods from `metrics.go` and looks for call
sites outside `internal/metrics` and `_test.go`; removing the `ObserveThumbnail` wiring reports `1 metric
method(s) are never called outside the metrics package and its tests: ObserveThumbnail`.
`TestAlertRulesNameOnlyMetricsThatExist` takes a real exposition and checks that every metric name, label
selector value, and `runbook.md#anchor` exists; it first went red on `status_class="5xx"` because the
harness had never produced one, so `ObserveRequest(200/404/500)` was added. promtool validates rule syntax
only.

### Round 32: Wiring `/metrics` end to end

The metrics were complete — 11 tests in `internal/metrics` — and nothing had ever fetched `/metrics` over
HTTP, because the harness never passed a registry: route registration, the middleware that feeds counters,
and the no-registry branch had never run.

`newHarnessTuned` plus four HTTP-level tests exposed three wrong assumptions. `/metrics` returns 200
without a registry, not 404, because `r.NotFound(s.ui.ServeHTTP)` hands unregistered paths to the shell and
`index.html` comes back; the correct assertion is "this is not metrics content", since a scraper pointed at
a deployment with metrics off must fail visibly rather than record a zero. `/api/definitely-not-a-route`
also reached the shell, so the 4xx counter stayed 0; only
`/api/files/download?path=/nope/absent.txt` produces a real 404. And the assertion was too weak: containing
`status_class="4xx"` still passed with every request recorded as `method=MUTATED`; requiring a
`method="GET"` series with 2xx or 4xx above zero made it fail with `no non-zero
newcloud_http_requests_total{method="GET",status_class="2xx"}`.

Lesson: an assertion means something only when a mutation would fail it. Two toothless assertions were
written before the mutation was run, which is how a missing wiring passed 11 green unit tests.

### Round 31: The account approval screen

The JIT policy creates `pending` accounts and a `pending` account cannot log in. The approval API
(`GET /api/admin/users?status=pending`, `POST /api/admin/users/{id}/status`) had 8 tests and nothing in the
interface could reach it, so the policy could strand a person where only curl helped.

`views/Admin.tsx` filters by status (pending, active, disabled, all), shows account, role, status, and
usage per row, and offers only the actions valid for the state. The navigation item is appended for
administrators only, because an entry that always returns 404 is worse than none; the server returns 404 for
non-administrators, held by `TestAdminEndpointsAreInvisibleToNonAdmins`.

`adminui_test.go` sends exactly the requests the panel sends, because type checking proves the request
matches `openapi.yaml` but not that the handler reads it the same way: `?status=pending` must return only
the pending account, and `{"status":"active"}` must change the status, since a 200 alone would pass with a
handler that does nothing. Making the handler ignore the parameter reports `got 4`. The `pending` pill was
first hardcoded (`#8a5a00` on `#fdf6e7`), invisible on any dark theme; the `--warn` and `--warn-wash`
tokens pass `pnpm contrast` in light, dark, and high-contrast (5.43 / 8.09 / 9.19). The i18n check listed
the 15 keys the view was missing instead of rendering `admin.title`.

### Round 30: A front-end entry point for the fourth ACL layer, and i18n validation

The API, the authorizer, and `/api/shared` were complete and tested, and nothing in the browser could
create a grant, so sharing a folder required curl. `FolderAccess` on the Access page loads a folder's
direct grants by path, revokes one at a time, and creates new ones (account plus permission checkboxes),
using the storage vocabulary (`read/write/create/overwrite/delete/share/manage_permissions`) with `delete`
separate rather than folded into `write`. Grants are addressed by path on the current node, matching the
server model; an account-wide list would invent a view the API does not have.

Type checking caught a wrong contract: `revokeGrant` was missing the `path` query parameter, and the server
needs the id and the path together. The types come from `openapi.yaml`, and a hand-written side drifts.

`web/scripts/i18n-check.mjs` checks that every English key exists elsewhere, except plural forms the
language can never select — Chinese has only `other` (`Intl.PluralRules("zh-Hans").select(1)` returns
`"other"`, measured), so the five `_one` keys are correctly absent, and the exemption is computed rather
than listed by hand. It also checks that every `t("...")` key exists, because a typo renders the key name.
Deleting the Chinese line for `acl.granted` reports `zh-Hans is missing "acl.granted"`; a call to
`t("acl.grantedTypo")` reports `FolderAccess.tsx calls t("acl.grantedTypo") but en has no such key`. The
first mutation run passed because PowerShell wrote nothing and the mutations had not landed; absolute paths
turned both red.

### Round 29: Completing INV-08, a restic snapshot restore measured

`scoop install restic` (0.19.1) made the item open since round 8 measurable. The chain ran: backup → forget
(7 daily, 4 weekly, 12 monthly) → prune → `restic check --read-data-subset=1/20`, reporting "no errors were
found". The first run crashed:

```
restic: Device IDs are not supported on Windows
panic: ...
```

`--one-file-system` panics on Windows. It is correct on Linux, where it keeps the backup out of mount
points under the blob root, but the script passed it unconditionally, so a Windows operations machine
exited before saving a byte; it is now added from `uname`. The documented restore command also failed:

```bash
restic restore latest --target /var/lib/newcloud --include /var/lib/newcloud/blobs
```

It restores 0 files: snapshot paths are absolute from backup time (`/C/Users/.../blobs`) and `--include`
cannot match them, while `restic restore` recreates absolute paths under the target, so
`--target /var/lib/newcloud` produces `target/var/lib/newcloud/blobs`. `--include` selects what, not where.
Following the command gives an empty directory or an extra level of nesting while appearing to succeed. It
is now two steps — restore to a scratch directory, then `cp -a` — and measured:

```
ab\cd\abcd1234   200 bytes   A48742653620D8AE...
ab\cd\abcd5678    50 bytes   21AB8B865E56EFD6...
```

Both blobs are byte-identical in SHA-256, in the layout `ab/cd/<hash>`. Both problems needed a real run: a
platform difference in a flag and a path semantic that had been assumed.

### Round 28: A local CI-equivalent environment

`go test -race` and S3 against a real endpoint were recorded as unverifiable locally; both were uninstalled
software. After `scoop install mingw`, `go test -race ./...` passes with `CGO_ENABLED=1`: 22 packages, zero
data races. "No race detected" is not evidence unless the detector fires, so a bare `raceProbe++` in
`RecordDownload` produced:

```
WARNING: DATA RACE
--- FAIL: TestConcurrentDownloadsRespectLimit
```

That makes "zero races" a result with content; the code the CI comment names — share download counting and
session revocation, changed in rounds 5 and 7 — had never run under the detector. With `moto[server]`,
`TEST_S3_ENDPOINT=127.0.0.1:19000 ... go test -race ./...` passed layout conformance 16 of 16, the blob
contract 7 of 7, and `TestOpenStorageBuildsS3`, so the round-9 fake had not hidden protocol problems. The
full CI configuration ran locally: 630 tests, zero skips, zero races.

### Round 27: Share links after a file is deleted

`handleSharePage` discarded the node query error:

```go
_ = s.deps.Meta.WithTx(...)   // the error is dropped here
```

With the file in the trash, `tx.Node` returned `not found` and nobody looked at it:

```json
{"kind":"link","label":"","root_name":"","root_kind":"","children":[],"can_read":true}
```

200, an empty list, nothing said — and a blank page cannot be distinguished from "this folder is empty",
which is worse than an error because there is nothing to report. The fix handles the error and maps
`not found` to `store.ErrShareGone`.

A distinct message for "the file was deleted" was the first plan, and it is wrong: `renderShareError`
collapses every reason into "this link is not available", because distinguishing expired, revoked, and
never existed turns the endpoint into a token oracle that lets anyone holding a candidate token test
whether it was ever valid. `ErrShareGone` is therefore server-internal: it stops the silent empty list and
lets the log record the reason, while the recipient sees what every other failure shows. Two tests: a
trashed file means 404 rather than 200 with an empty list, and the bodies for "deleted", "revoked", and
"never existed" are byte-identical. Restoring the discarded error prints
`got 200 with map[can_read:true children:[] root_name:]`.

### Round 27, continued: Version control and CI alignment

Twenty-seven rounds were uncommitted, and round 14 had already destroyed uncommitted work with one
`git checkout`, so the work was committed: 216 files, 25,517 lines added, after excluding `design-studio/`,
adding `.pnpm-store/` to `.gitignore`, and confirming 2.2 MB of staged source with no binaries or logs.

Comparing against CI found a real difference: `ci.yml` runs `gofmt -l .` over the whole tree, while local
checks had used `gofmt -l cmd internal tools`. The tree is clean, but the local check was narrower than CI.

| CI step | Local result |
| --- | --- |
| `gofmt -l .` (whole tree) | passes |
| `go vet ./...` | passes |
| example configuration parses | passes |
| web `typecheck` | passes (exit 0) |
| web `contrast` (WCAG AA) | passes (exit 0) |
| web `gen:api:check` (generated types did not drift) | passes, no diff |
| `go test -race` | not runnable: needs cgo, and the machine has no C compiler (`gcc not found`) |
| S3 conformance against a real endpoint | not run: the round used the in-process fake; a real endpoint needs the CI moto service |

### Round 26: Upload sessions stop being exempt from T-01

Round 25 exempted three upload session routes because sessions live in an in-memory Manager. Half right: a
test can create a session owned by the victim through the Manager. Adding the `uploads *upload.Manager`
field found two things — `handleUploadChunk` parsed `Content-Range` before checking identity, so the first
test was testing the range parser rather than authorization; and all three routes now have cases whose
reverse check is effective, since deleting `s.Owner != owner` in `Manager.Get` turns all three red, with
`write a chunk` returning 200 (an intruder writes into someone else's session) and `complete` returning
409. All 12 routes with ids now have coverage or a written exemption, leaving `DELETE /api/acl/{id}`
(covered by existing tests) and `DELETE /api/admin/users/{id}` (the non-admin cases prove the same gate).
"Inconvenient test infrastructure" is a signal to fix the infrastructure.

### Round 25: T-01, checked route by route

T-01 is "user A guesses user B's file id, version id, thumbnail, or download link". One IDOR test existed
against 12 routes that take ids. Every handler authorized correctly, which is the problem: IDOR is the route
added last without the check, and a test covering the three that come to mind passes while the fourth leaks.
The test is a table over the route table, and each case asserts that the status is 404 rather than 403 (403
confirms the id exists), that the refusal body leaks no metadata (a 404 carrying a file name is still a
leak), and that the resource is unchanged after a mutating route. `TestIDORTableCoversEveryIdRoute` aligns
the hand-maintained list of 12 routes against "covered or explicitly exempt with a reason". Deleting the
ownership check in `handleShareRevoke` fails with `want 404 ..., got 204` — 204 meaning the intruder revoked
someone else's share.

Three attempts to leak metadata through `handleSharedNode` did not turn the test red, because the refusal
happened on the path where `Authorize` returns an error and none of the edits touched that branch. A
`println` showed the handler running; the fourth edit printed `denied: secret-plan.txt`. When an assertion
does not fire, find the branch that ran instead of editing code and hoping.

### Round 24: Documentation integrity, a cited test must exist

Round 23 wrote two test names into a policy document from memory, and neither existed.
`TestDocumentsCiteOnlyRealTests` scans every backticked `Test*` name in `docs/`, `deploy/`, and
`README.md` and diffs it against every `func Test*(` under `cmd/` and `internal/`. It caught three more,
all in the earlier draft of this record, one unnoticed: round 12 wrote `TestCreateWithoutOverwriteCannotReplace`, <!-- docIgnore: narrative naming a test that was renamed -->
renamed to `TestCreateDoesNotImplyOverwrite` at the time, while the narrative kept the old name. All three
were narratives about errors, so such a line is marked `docIgnore:` rather than deleted: an exclusion is an
action, not a pattern. The test also fails when it finds no tests or no documents, so an empty comparison
cannot pass forever. Inserting a `TestThisIsNotReal` into `0012-cursor-contract.md` <!-- docIgnore: narrative deliberately inserting a fake test name -->
reports `TestThisIsNotReal (cited in 0012-cursor-contract.md)`.

### Round 23: ID-03, JWKS key rotation and the validation failure policy

Rotation could never be tested: the fake IdP had one fixed key with `kid` hardcoded to `"test-key"`, so a
client that fetched JWKS once and never refreshed passed every test. `fakeidp.RotateKey()` adds a new key
while still publishing the old one — dropping it at once invalidates tokens just issued — and
`RetireOldKeys()` ends the window; signing reads the current `kid` and key under one lock, so a header
cannot disagree with its signature.

| Assertion | Result |
| --- | --- |
| after rotation, the next login in the same process succeeds | passes (a different account, so a cached session cannot mask it) |
| a logged-in session still works after rotation | passes (the session is its own credential; the ID-07 property) |
| login still succeeds after all old keys are retired and the IdP rotates | passes (requires refetching JWKS) |

Signing with an unpublished key turns the first two red; changing the key but not the `kid` did not fail,
because go-oidc refetches JWKS on any validation failure. `deploy/oidc-key-rotation.md` records the four
steps, nine validation-failure results, and the non-goals: no key pinning (rotation would become a
coordinated redeployment), no on-disk JWKS cache, and no revocation push (revocation applies to new
validations, and a cached key waits for a refetch or restart). The document first named
`TestOIDCNonceMismatchIsRefused` and `TestCallbackRejectsForgedState`, both invented; the real names are <!-- docIgnore: narrative about test names that were wrong -->
`TestMismatchedNonceIsRejected` and `TestForgedStateRejected`, and every name was then checked against the
source. <!-- docIgnore: narrative about test names that were wrong -->

### Round 22: INV-07, backfilling `blobs.size`

Round 21 said sizes could not be backfilled because only the object knows its length — the counterexample to
its own premise, since the objects are content-addressed, on disk, and aware of their length.
`store.RepairBlobSizes` runs in `migrate`: the `stat` function is a parameter, so the metadata layer does
not know the backend and local disk and S3 run the same check; it reads the whole table, since blob rows are
few and this is an operational action; and a missing object is reported without aborting the backfill.

```
{"msg":"blob sizes reconciled","scanned":5,"fixed":4,"recovered_bytes":2936,"missing":1}
second run: {"msg":"blob sizes consistent","scanned":4}
```

The first version recorded every failed `Stat` as `BackendError`, so one dangling row printed "the content
store reported errors". The count now happens after the loop: only if every object fails is the backend
unreachable. Reverting the size write turns two tests red; the old inference turns the classification test
red with `a healthy store must not be reported as failing: blob: not found`.

### Round 21: INV-08 re-run, the drill's table list checks itself

The drill had not run since round 8 while the schema changed twice (0006 account deletion, 0007 `name_key`).
Its table list is hand-maintained, and
the comment in `drill.go` claimed `TestDrillCoversEveryTable` checked it — a test that did not exist. It
exists now and caught two tables:

```
the drill does not verify 2 table(s): [public.group_role_map public.name_conflict_renames]
```

The re-run then failed:

```
blobs  14 files, 12 verified
VERDICT: FAIL
  - mine-report.txt: the restored copy cannot serve it: blob: not found
```

Every `blobs` row had size 0 while the `nodes` rows claimed 512, 609, and 706 bytes:

```sql
-- before the fix
       name      | node_size | blob_size
 mine-report.txt |         3 |         0
 file0.txt       |       512 |         0
```

`BlobIDFor`'s INSERT hardcodes the size as `0` and never had another parameter. Nothing user-visible broke,
because the node row carries its own size; GC's reclaimed-bytes accounting (`BlobRef.Size`) and an operator
reading the table were told every blob is empty. After the fix:

```
 size | count
  512 |     1
  609 |     1
  706 |     1
  803 |     1
```

The drill passes: dump 78.8 KiB, 22 tables (up from 20), 57 rows, 32 audit events `verified=true`, 12 of 12
blobs at 7.7 KiB, 1.002s against a 1h target. `TestBlobRowRecordsContentSize` registered its cleanup after
its assertions, so a deliberately broken run left a size-0 row that broke the restored implementation;
`t.Cleanup` now registers first.

### Round 20: T-20, a failed COPY or MOVE does not damage the source

The requirement's weight is on what must not happen: a truncated original, or a half-written target that
looks complete, is data loss. `TestCopyDoesNotDuplicateBlobOnDisk` expected identical content to share one
blob and found the in-memory `snapshot()`:

```go
// around line 220
blobIDs: make(map[[32]byte]uuid.UUID, len(m.blobIDs)),   // an empty map is declared
...
// then every other map is copied entry by entry -- nodes, children, roots, usage, quota, ...
// the loop for blobIDs was never written
```

Every transaction started with an empty index and overwrote the real one on commit, so identical content got
a new blob id each time, deduplication failed silently, every upload consumed its full size, and every copy
doubled disk usage. `BlobCount()` was permanently 0 (`count=0` before the fix, `count=1` after), and only
the cross-transaction case exposes it. The fix copies `blobIDs` and has `BlobIDFor` write into the
transaction snapshot; the conformance case `BlobDedupAcrossTransactions` runs on both backends and asserts
one id per address across transactions and different ids for different addresses.

| T-20 scenario | Result |
| --- | --- |
| COPY with insufficient quota | refused, source bytes unchanged, target invisible |
| MOVE with insufficient quota | source still in place (a move is not delete-then-write) |
| target parent missing | 404, deliberately not the RFC's 409: a target the caller cannot see must be indistinguishable from one that does not exist, or the endpoint probes other people's directory names |
| directory copy hits an unreadable child | fails at the first one rather than skipping silently |

All pass, and each was reverse-verified.

### Round 19: The cursor consistency contract

An offset over name ordering duplicates and skips rows when siblings change between requests, so
`ChildrenAfter` already used `name > cursor`. The contract was never written down;
`docs/adr/0012-cursor-contract.md` states it, with no row returned twice as the guarantee and one limitation:

| Event | Guarantee |
| --- | --- |
| insert after the cursor | visible later in this pass |
| insert before the cursor | not visible this pass, visible in the next full list; never duplicated |
| delete a row, seen or unseen | nothing is lost |
| rename an unseen row to after the cursor | visible later in this pass |
| rename an unseen row to before the cursor | not visible this pass (it exists and appears in the next list) |
| any case | never the same row twice |

Duplication makes a client act twice — delete twice, upload twice, process one share download twice — while
a delay is only a delay. An offset cannot fix it, trading rare missing rows for frequent duplicates;
snapshot isolation (`WHERE modified_at < $txn_start`) would, but it is a schema and index change and was not
done. `RenameAcrossCursorIsTheDocumentedLimitation` fails on purpose when the behavior changes and requires
the document to change with it. `TestCursorDoesNotOutlivePermission` verifies that a cursor carries no
permissions: after a grant is revoked, a stale cursor must fail. Changing `name > $2` to `name >= $2` turns
four cases red with boundary rows repeated, `[a c c d d e e e e ...]`.

### Round 18: T-16, fail-closed when the audit log is unavailable

The policy is that an operation which cannot be audited does not happen. fail-open is wrong here for three
reasons: audit is a security control (INV-09), not telemetry, and stopping quietly under pressure is worse
than no log because people decide from it; it makes filling the disk the cheapest attack, since "nothing in
the log" and "nothing happened" become indistinguishable; and the hash chain still verifies, so written
events link correctly and missing ones leave no trace, the one property a tamper-evident log must not have.

Reads stay available (listing, download, metadata, DAV GET and PROPFIND, audit verification, health checks),
because an operator diagnosing failed writes needs data and logs. The client sees a new sentinel,
`store.ErrAuditUnavailable`:

```
HTTP/1.1 503 Service Unavailable
{"error":"audit_unavailable",
 "message":"the audit log is unavailable, so the change was refused and nothing was modified"}
```

503 rather than 500: the request and the data are fine, the recorder stopped, and a retry succeeds after
recovery — operationally, "look at the data" against "look at the audit table and the disk". The cause stays
in the error chain through a double `%w` (for example `no space left on device`).
`deploy/audit-failure-policy.md` records the policy, the failure and retention list, the client contract,
the runbook, and what it does not cover.

| Assertion | Test | After switching to fail-open |
| --- | --- | --- |
| the mutation rolls back | `TestAuditFailureRollsBackTheMutation` | red: `must fail, not commit silently` |
| reads unaffected | `TestReadsSurviveAnAuditOutage` | — |
| a missing writer fails loudly | `TestMissingAuditWriterIsAlsoFailClosed` | — |
| the client gets 503 | `TestAuditOutageReturns503` | red: `got 204` |
| uploads are fail-closed too | `TestUploadIsRefusedDuringAuditOutage` | red: `got 201` |

`TestReadsWorkDuringAuditOutage` covers the second read path. The in-memory `snapshot()` did not carry
`auditFault`, so audit writes inside a transaction bypassed the injected fault; fields missing from a
snapshot only show up in injection tests.

### Round 17: T-12, `RepairNameKeys` iterates in pages

Reading the whole table into memory and running two statements per row does not scale, so the repair now
uses keyset pagination with one transaction per batch of 1 000 rows. Keyset rather than `OFFSET`, because
concurrent inserts make `OFFSET` paging skip or repeat rows, and the cursor advances on every row, including
correct ones, or a page of correct rows would loop. Measured across a page boundary (1 200 expired rows,
page size 1 000):

```
{"msg":"name keys reconciled","scanned":1206,"corrected":1200,"skipped":1}
elapsed: 3381ms
expired rows remaining: 0
```

`TestRepairNameKeysCrossesPageBoundary` seeds 1 100 rows, because the other four tests only touch the first
page and an implementation that repairs the first page and returns would pass them. Returning after the
first page fails with `want 1100 corrected across the page boundary, got 990 (scanned 1000)`; 990 is
exactly the number needing folding on the first page. Cleanup now registers before any assertion, because a
failing run left 1 100 rows behind and made the restored implementation fail.

### Round 16: T-12, converging stored `name_key` values

Migration `0007` backfilled with SQL `lower(name)`, while the key is a Go function (NFC, full case folding,
format-character stripping). The two agree on ASCII, which is why this was easy to miss: macOS names arrive
in NFD, so `café` arrives as `cafe` plus U+0301 and `lower()` keeps the combining mark, and `ß` against `ss`
or `ﬁ` against `fi` need full folding. A stale key is not cosmetic: lookups filter on `name_key`, so a node
is visible in a listing and cannot be found by the spelling a user would type, and uniqueness misses a
conflict. `store.RepairNameKeys` runs in `newcloud migrate` and updates only disagreeing rows; it is an
operator action rather than another migration because it must be re-runnable.

```
{"msg":"name keys reconciled","scanned":4,"corrected":2,"skipped":0}

before: 63616665cc81 (cafe + U+0301)   73747261c39f65 (straße)
after:  636166c3a9 (café, NFC)         73747261737365 (strasse)

second run: {"msg":"name keys already consistent","scanned":4}
```

Conflicts are skipped and reported, not forced: two siblings can fold to one key, which is what the unique
index prevents, and which of two files wins is an operator decision. Not verified: real Windows and macOS
clients, and the runtime at a million nodes, which round 17's batching addressed. Mid-round the repository
was renamed to `github.com/EdisonJwa/newCloud`; 588 tests passed, seven migrations applied to an
empty database, and the round's changes survived.

### Round 15: T-12, case and Unicode name conflicts

The in-memory backend keyed siblings by `strings.ToLower(name)` and the PostgreSQL unique index was
`(parent_id, name)` over raw bytes, so `Readme.md` and `README.md` were two siblings in the database while
the in-memory backend refused the second — and on a case-insensitive client the user saw one file, with
another upload silently overwriting it. `store.nameKey` now serves sibling uniqueness, lookup, and both
backends:

| Step | What it prevents |
| --- | --- |
| Unicode NFC | precomposed `é` and `e` plus a combining accent are different bytes with the same name; without normalization a directory lists two identical-looking files |
| full case folding | simple folding misses `ß`→`ss`, the Greek final sigma, and the `ﬁ`→`fi` ligature, exactly the pairs a user cannot distinguish |
| strip format characters | zero-width joiners, soft hyphens, and bidi overrides render as nothing; a name can be entirely RTL overrides and display blank |

`0007_name_key.sql` adds `name_key` and a `(parent_id, name_key)` unique index — a plain column, because
normalization is a Go function SQL cannot express. The migration had to handle existing conflicts: the
database contained pairs such as `{Readme.md, README.md}` and the first `CREATE UNIQUE INDEX` returned
23505. Deleting one is not a migration author's decision, so the earliest is kept and the later renamed,
recorded in `name_conflict_renames`, with bytes, ownership, versions, and shares untouched:

```
old_name  |        new_name
----------+------------------------
 README.md | README (conflict 2).md

          name          |        name_key
------------------------+--------------------------
 README (conflict 2).md | readme (conflict 2).md
 Readme.md              | readme.md
```

Both files still exist, which is the round's most important assertion. A real bug was fixed with it: the
second `EnsureRoot` in `identity_pg.go` did not set `name_key`, so a NOT NULL violation aborted the
transaction and the caller saw `current transaction is aborted`. Not verified: real clients, and whether the
Go normalization and the SQL `lower()` backfill differ on rare combining sequences; the difference
self-heals on the next write, and round 16's `RepairNameKeys` addresses existing rows.

### Round 14: §5.3, the difference between `Depth: 0` and `Depth: infinity`

RFC 4918 §7.4 distinguishes two collection locks: `Depth: 0` protects membership, not the content of
existing children, and `Depth: infinity` covers the subtree including content. `lockAncestors` ignored
`Depth`, so `Depth: 0` over-blocked: after locking a folder a client could not edit files inside it, and the
423 gave no hint that its own lock caused it. The reverse error was subtler — creating a child was blocked
by a `Depth: 0` parent lock only by accident, so splitting the paths needed an explicit branch.

| Operation | Nature | Ancestor lock that applies |
| --- | --- | --- |
| PUT overwriting an existing child | content | `Depth: infinity` only |
| PUT creating a new child | membership | `Depth: 0` is enough |
| MKCOL, delete a child | membership | `Depth: 0` is enough |
| a lock on the target node | content | any depth |

Both are tested and reverse-verified. An operational mistake belongs here: while refactoring
`internal/dav/resource.go`, `git checkout` was used to clean up a failed edit, and the file held uncommitted
work — PUT's 201/204 codes, the `If-Match` and `If-None-Match` preconditions, version retention, and lock
checks. HEAD was the initial commit, so all of it was lost and the DAV tests went from 40 green to 8 red.
`handlePut`, `handleDelete`, and `handleMkcol` were rebuilt from the test contracts and all 40 pass, but
recovery came from tests, not process: `git stash` or a `git status` check for `M` comes first.

### Round 13: §5.3, write arbitration across interfaces

WebDAV checked locks and the JSON API did not, which is the worst half-finished state: a browser could
overwrite a file locked by a WebDAV client and one side of the change was lost silently. A lock honored by
one of two write paths is a false promise to the client that took it.

Every mutating JSON handler now asks the same question through the same store operation, `ActiveLock`, so
the paths cannot drift. Covered: overwriting an existing file by upload, delete, rename, and move. The
holder may write but must submit the token, or a stale tab overwrites a file the client believes is
protected. With the guard off, all three fail by causing real damage:

```
--- FAIL: TestHTTPOverwriteHonoursWebDAVLock  an HTTP write against a locked file must be 423, got 201
--- FAIL: TestHTTPDeleteHonoursWebDAVLock     deleting a locked file must be 423, got 204
--- FAIL: TestHTTPRenameHonoursWebDAVLock     renaming a locked file must be 423, got 200
```

Two reverse cases prevent over-blocking: without a lock, overwrite, rename, and delete must all succeed, or
the three tests would also pass against an implementation that refuses everything; and an expired lock must
not block writes, or a crashed client makes a file permanently unwritable.

### Round 12: §4.2, `create`, `overwrite`, and `rename` split apart

`PermWrite` was still checked directly at several operations while §4.2 lists three primitives, so "may add
files here but may not replace existing ones" was inexpressible.

| Operation | Before | After | Meaning |
| --- | --- | --- | --- |
| `mkdir`, upload into a directory | `PermWrite` | `PermCreate` | may create a new name in the directory |
| upload over an existing file | `PermWrite` | `PermOverwrite` | may replace someone else's content |
| `rename` | `PermWrite` | `PermRename` | may rename or move into a directory |

`create` is checked first and `overwrite` is added only when the target exists. The authoritative guard
stays the precondition in `WriteFile`, so a file created between the two is still not overwritable by a
caller lacking the bit, with no race and no lock.

`TestCreateWithoutOverwriteCannotReplace` uploaded over HTTP into another account's directory, but the path <!-- docIgnore: narrative naming a test that was renamed -->
is in the caller's own namespace, so it returned 404 before the permission check and stayed green when the
`overwrite` check was deleted — the third "passing for the wrong reason". Rewritten at the authorizer layer
as `TestCreateDoesNotImplyOverwrite`, the grant asserts `cap.Can(PermCreate)` true and
`cap.Can(PermOverwrite)` false, and restoring the implication fails with `got read+list+create+overwrite`.
<!-- docIgnore: narrative naming a test that was renamed -->

The eight remaining `PermWrite` sites are deliberate: WebDAV parent-directory and LOCK checks, where a
single PUT is inherently create-or-overwrite, and splitting them would add a round trip to every PUT.

### Round 11: ID-07, rules for IdP downtime

`NewProvider` performed OIDC discovery at startup and returned an error on failure, so the process refused
to start — taking down WebDAV, App Password authentication, browser sessions, and all file operations, none
of which contact the IdP. A single sign-on dependency had become an availability dependency.

| Situation | Behavior | Reason |
| --- | --- | --- |
| startup | the IdP need not be reachable; the warning is logged loudly | see above |
| existing sessions | keep working | a session is a credential this service issued; validation never involves the IdP and it expires on its own schedule |
| renewal | unaffected | renewal slides the expiry of a session this service issued |
| new login | `503 idp_unavailable` with an explanation | the user needs to know the account is fine and the IdP is unreachable |
| revocation | unaffected | validity is a database question (`sessions.revoked_at`) |
| App Passwords | keep working | verified against a local hash |
| role sync | deferred to the next successful login | as recorded for ID-06 |

Only first login cannot work offline, and the credentials are with the IdP. Discovery is lazy:
`ProviderHolder` tries once at startup and retries every 30 seconds, because an IdP restart is routine.
Rate limiting matters because a login page refreshes during a fault; without it the test reports `ran 6`
instead of 1. On a real instance with the IdP pointed at an unlistened `127.0.0.1:19999`:

```
process alive: True
healthz HTTP 200
readyz  HTTP 200   status=ready
existing session: /api/auth/me    -> 200
new login:        /api/auth/login -> 503 {"error":"idp_unavailable", ...}
app password over WebDAV: PROPFIND -> 207
```

### Round 10: ID-08, the account deletion policy

Only disabling existed, because `nodes.owner_id` is a `NO ACTION` foreign key, so an account with files
could not be deleted.

| Object | Disposition | Reason |
| --- | --- | --- |
| files | to the trash, not reassigned | handing documents to an administrator is silent data movement; the trash keeps a recovery window |
| shares | revoked | a share is a credential the account issued and cannot outlive it |
| ACL entries as subject, as grantor | deleted | the grantor is gone and nobody can review the grant |
| sessions, App Passwords | revoked | — |
| WebDAV locks | released | a lock outliving its holder makes a resource permanently unwritable |
| audit records | kept | audit is the record of what happened, not the account's property |
| the account row | a tombstone (`status='deleted'`), user name and email released | see below |

A tombstone rather than a deleted row: `nodes.owner_id`, `shares.owner_id`, and `versions.created_by` are
`NO ACTION` foreign keys and the audit log points at the account by id, so physical deletion is blocked or
requires cascading into the audit log. The tombstone also keeps identity from being reused, so an old
`(issuer, subject)` cannot open a new account with a stale token, and releases the email. An account must be
disabled before deletion, so mistakes hit the reversible step.

```
before: map[audit rows:3 live nodes:3 live sessions:1 live shares:1]
disable: 200
delete with wrong confirmation: 400
delete: 200 map[nodes_trashed:3 shares_revoked:1 deleted:true audit_rows_kept:3 ...]
after:  map[audit rows:3 live nodes:0 live sessions:0 live shares:0]
```

`audit rows` unchanged at 3 is the policy's most important line; after the trash is purged the blobs become
collectable. `0006_account_deletion.sql` widens `users_status_check` to allow `deleted`, keeping the set
closed. `AuditIsRecorded` asserted a global count of 1 while the deletion case leaves audit rows behind, so
it now counts the rows the case added.

### Round 9: §7 S3, from skipped to actually running

The `s3` conformance cases were `t.Skip` without `TEST_S3_ENDPOINT`, so they skipped every time, and the
README's "verified against moto" was stale, since moto had left the dependency list.
`internal/storage/s3fake` runs the three affected suites by default:

| Suite | Before | After |
| --- | --- | --- |
| `blobstore.TestS3Conformance` (layout layer) | skip | 16 of 16 pass |
| `s3.Test*` (blob contract layer) | skip | 7 of 7 pass |
| `TestOpenStorageBuildsS3` (wiring) | skip | passes |

24 cases, zero skips under `go test ./...`. The fake implements only the five operations used (BucketExists,
Stat, Get, Put, Remove, plus multipart and ListObjectsV2) and returns `501` outside that set, so a backend
that starts using a new operation fails instead of passing against a guessed fake.

Three protocol details, each of which would make a real object store go quietly wrong:

1. minio-go frames streaming uploads as `aws-chunked` (`<hex-size>;chunk-signature=...\r\n<data>\r\n`).
   Storing the framed bytes turned a 17 MiB multipart object into 17,850,444 bytes, 24,652 bytes of
   signature frames extra; only a content comparison finds it, and multipart parts are framed too.
2. With no `Last-Modified`, the client reports "cannot parse date" rather than "header missing"; a 206
   response also has to carry it, along with the ETag.
3. `BucketExists` uses a bare `GET /bucket/` and the 200 body must be valid XML; an empty body makes
   minio-go report `EOF`.

Making `Delete` a no-op turns `TestDeleteAndMissing` red (`Stat after delete = <nil>, want ErrNotFound`).
Not verified: interoperability with real AWS, MinIO, or R2. The fake proves backend logic, not protocol
compatibility; `TEST_S3_ENDPOINT` runs the same cases against a real endpoint.

### Round 8: INV-08, verifying blob content

The database dump and the blobs are independent backup paths, and a restore can reproduce a consistent node
tree pointing at blobs that were never copied or were truncated: every file listed, none openable. Step 6
copies the blob root, opens a content store on the copy, and reads every file node back.

The first version compared the restored bytes with the bytes on disk, so reversing one blob's first byte
still produced a PASS — both sides corrupt and therefore equal. Comparing against `blobs.sha256` catches it:

```
blobs            12 files, 11 verified, 31.3 KiB
VERDICT: FAIL
  - top.txt: restored bytes hash to c0d5f09b9940, metadata recorded 5029feaf3e6b
    — the backup is corrupt
```

The expected value must come from the address recorded at write time, not from any surviving copy, which is
also the check a downloading client performs.

Two real bugs were fixed: `Descendants` used `nodeColumns` without an alias in the recursive CTE,
conflicting with the CTE's `id` (`column reference "id" is ambiguous`), invisible to server tests, which run
the in-memory backend, and only triggered by the drill against PostgreSQL; and `BlobHash` did not exist,
since the store only had hash to id. Measured (4 accounts, 21 nodes, 12 blobs, 45 audit events):

```
dump size        78.2 KiB
tables compared  20
rows compared    89
audit chain      45 events, verified=true
blobs            12 files, 12 verified, 31.6 KiB
elapsed          1.12s (target 1h)
VERDICT: PASS
```

Not verified at this point: restoring from a restic repository itself (restic was not installed, so what was
verified is that bytes copied at the file level are correct and readable, not snapshot restore and the
deduplication index), large-database timing, and PITR.

### Round 7: INV-08, the first restore drill and its automation

`restore-drill.md` had carried `_awaiting first drill_` since it was written, so INV-08 was a claim without
evidence. The drill is now `newcloud drill`, because the manual procedure was never performed. Six steps,
each answering a real failure: dump; check the dump is readable (a truncated dump looks fine in `ls`);
restore into a scratch database; compare row counts table by table; verify the audit hash chain on the
restored copy; report elapsed time against the RTO.

```
dump size        77.2 KiB
tables compared  20
rows compared    75
audit chain      45 events, verified=true
elapsed          928ms (target 1h) — within target
VERDICT: PASS
```

The data set was written for real: 6 accounts, 2 directories, 5 files through the upload API, 1 group, 1
ACL, 40 audit events.

| Damage | Result |
| --- | --- |
| change the `action` of one audit record | `FAIL`: chain verification fails at row 0 |
| `SCRATCH_DSN == LIVE_DSN` | refuses to run |
| `SCRATCH_DSN` missing | refuses to run |

Every table and row count matched, and the chain check was the only thing that exposed the first case. Not
verified at this stage: blob content restore (the restic path), large-database timing (77 KiB does not
extrapolate to 40 TiB), and PITR, which differs from "restore from a dump" by an order of magnitude in RPO.
One mistake: `Get-Content` showed `restore-drill.md` as mojibake and the file was declared corrupt; it was
valid UTF-8 with 552 CJK characters and zero replacement characters, and the console rendering was the
problem. An encoding question is settled from the bytes.

### Round 6: §4.2, fine-grained permissions take effect

`read` and `download` had been separate bits since round 4 and every streaming endpoint checked `PermRead`,
so "you may see that this file exists but not take a copy" never applied. `expandComposite` also made
`PermRead` imply `PermDownload`, which turns the split into a lie once `download` is grantable.

| Rule | Location |
| --- | --- |
| `read` no longer implies `download` | `authz.expandComposite` |
| downloads check `download` rather than `read` | `handleDownload`, DAV `handleGet`, `handleSharedContent` |
| recursive directory operations check every node inside | `checkSubtreePerm` / `subtreeAllows` |
| MOVE needs `delete` on the source, COPY needs `download` | DAV `handleCopyMove`, `handleMove` |

Delivery was missing too: downloads resolve by path in the caller's own namespace, so a granted shared file
was unreachable from every content route. `GET /api/shared/{id}/content` addresses it by unguessable id and
still passes the authorizer every time. Verified on a real instance:

```
no-download:   metadata=200 perms="read+list"           content=404 (47 bytes)
with-download: metadata=200 perms="read+list+download"  content=200 (9 bytes)
```

The first version of `TestDownloadRequiresDownloadNotJustRead` still passed with the check reverted to
`PermRead`, because the path does not exist for the restricted account and the 404 came from "no such file"
rather than "insufficient permission". Moved to `/api/shared/{id}/content`, the revert fails with `got 200,
want 404`.

### Round 6, continued: The queue clock source behind the flake

`jobs.run_after` defaulted from the Go process clock while claiming compared the database's `now()`, so a few
milliseconds made a job due yet unclaimable — a failure that appeared only on PostgreSQL (the only
configuration with two clocks), only after long uptime, and reported `setup claim failed` far from the
cause. The default is now `COALESCE($4, now())`, pinned by `DefaultScheduleIsImmediatelyClaimable`; the
suite ran five times consecutively.

### Round 5: ID-04, the account approval API

The `pending` mode created accounts disabled and nothing could move them out, so a deployment configured as
documented locked every new user out with hand-editing as the only exit.
`GET /api/admin/users?status=pending` and `POST /api/admin/users/{id}/status` now handle approval,
disabling, and re-enabling as one status change, audited with `from`, `to`, and the number of credentials
cut. Two bugs surfaced: `UpsertIdPUser` hardcoded `Status: "active"`, making `pending` an alias for `open`
(round 4's test missed it because refusal happened before the account existed), and the middleware never
checked `Status`, so a session issued a millisecond before revocation, or a replica reading an older row,
still worked. Status is now checked per request.

### Round 5, continued: §10.2, the INSERT-only role becomes usable

The first migration created `newcloud_app` with the right grants, but the role was `NOLOGIN` and deployments
connected as the owner, a superuser that bypasses every GRANT — so "the audit log is append-only for the
application" was false in every deployment. `0005_app_role.sql` makes the role login-capable (still
`NOSUPERUSER`, `NOCREATEDB`, `NOCREATEROLE`), restates the grants, and adds an `app_role_status` view that
answers whether the current connection can rewrite the audit log. No password in the migration and no
`pg_hba` change; an operator runs `ALTER ROLE newcloud_app PASSWORD` once.

```
select * from app_role_status
  connected_as | is_superuser | audit_is_append_only
  newcloud_app | f            | t

INSERT INTO audit.events (...) RETURNING id  -> 2       -- allowed
UPDATE audit.events SET result='tampered'    -> permission denied
DELETE FROM audit.events                     -> permission denied
TRUNCATE audit.events                        -> permission denied
```

A latent parser bug was fixed with it: `CREATE FUNCTION ... $fn$ ... $fn$` failed with
`"events_default_pkey" is an index`, because `stripTxControl` tracked only anonymous `$$` delimiters and
stripped the `BEGIN` opening the PL/pgSQL block as transaction control. A delimiter state machine now tracks
the tags, so a `$$` inside `$fn$` does not close it early; `schema_test.go` has five cases, and the named-tag
case failed under the old code (`want exactly one BEGIN, got 0`).

### Round 5, continued: §11.1, the metrics endpoint

`/metrics` exposes request error rate, DAV conflicts, upload failures, queue backlog, GC, and audit
verification in Prometheus text format, with 11 tests. Status classes rather than one series per code,
because the question is "what share failed" and per-route, per-code series turn the endpoint into a memory
leak. Atomic counters rather than a locked map, because this is on the request path. Anchor verification at
the same cadence as publishing, since an anchor never compared is not an anchor; a mismatch logs `ERROR` and
increments `audit_anchor_verify_total{result="failed"}`. `/metrics` is unauthenticated: a scraper cannot
carry a cookie, the exposure is counters rather than user data, and a deployment that must not publish it
should not route the path at the proxy.

Evidence: `go test ./internal/metrics` (11 cases), `go test ./internal/store -run TestStripTxControl`
(5 cases), `go test ./internal/server -run TestAdmin` (5 cases). On a real instance the counters matched the
traffic (`GET 2xx=2`, `GET 4xx=1`, `PROPFIND 4xx=1`, `dav_requests_total=1`, `jobs{state=...}`,
`audit_events=1`, `audit_anchor_verify_total{result="ok"}=1`).

### Round 4: §11.1, the job queue gets a consumer (INV-06)

The `jobs` table had `run_after`, attempts, a lease, and `dedupe_key`, and no code read it: `Enqueue` was
called only from tests. Claiming now uses `FOR UPDATE SKIP LOCKED`; an attempt is counted at claim time, so a
worker that dies before reporting still consumed one; an expired lease allows reclaiming, which is why the
lease is in the database; errors retry with capped exponential backoff and a job past the cap stays in the
table with its error; completion is a timestamp, so "did it run" is answerable. Permanent failure is
terminal: `permanent=true` initially only cleared the lease, so it was retried; it now lowers
`max_attempts`.

Evidence: `go test ./internal/store -run Conformance/JobQueue` (7 subcases × 2 backends);
`go test ./internal/worker` (7 cases). On a real instance `thumbnail.warm` was consumed
(`attempts=1, completed`), and `blob.collect` without a `blob_id` failed once and stopped with
`job failed permanently`.

### Round 4, continued: §10.2, the external audit anchor

`audit.Anchor` existed and `RecordAnchor` was never called, so the whole tail could be deleted and the chain
still verified — inherent to a hash chain, which proves internal consistency, not completeness.
`internal/auditverify` publishes one checkpoint per completed day (chain head hash, event count, link to the
previous), one file per day since a rewritable anchor proves nothing; writes a temporary file and `rename`s
it; and `Verify` checks that checkpoints link, that there are no date gaps, and that the chain head is
recomputed from the published counts. The third matters: comparing only the latest checkpoint against the
current head misses "delete the tail, then keep appending".

Evidence: `go test ./internal/auditverify` (6 cases including truncation, mid-chain rewrite, and checkpoint
deletion). On a real instance, 5 events, a published checkpoint, then the newest 2 deleted:

```
chain now: 3 events
verify: ok=false matched=0/1
  PROBLEM: checkpoint 2026-10-22 covers 5 events but the chain holds only 3: events were deleted
```

The remaining 3 events still form a linked chain, which is why the anchor exists. The in-memory backend keeps
no sealed chain, so `Memory.AuditHead` returns `ErrNoChain` rather than inventing a self-consistent head.

### Round 4, continued: §6.3, isolating preview decoding (T-14/T-16)

The 50 MP pixel limit stops one very large file, not a hundred simultaneous legitimate 50 MP requests: the
reason for refusing one request gets multiplied by the concurrency. A semaphore of 4 concurrent decodes
(sized by CPU, since this is CPU-bound) and a 10-second decode deadline were added. A decoding goroutine
cannot be killed, so the deadline bounds the caller's wait and the slot occupancy, which is the honest limit
for a CPU-bound library call from Go; the pixel limit stays necessary. The package had no tests, so a
decompression-bomb defense was unverified. `go test ./internal/thumbnail` now has 7 cases, including a real
10000×10000 PNG, which is small on disk, which is why a size check cannot catch it.

### Round 4, continued: ID-04, the JIT provisioning policy

`status` carried the idea of `pending` and was never written: any identity the IdP asserted got an account on
first login, which means "anyone who can authenticate can store files" when the IdP tenant is wider than the
deployment's audience. Three modes: `open` creates the account immediately (historical behavior and the
default, since changing it would lock out existing deployments); `pending` creates it disabled
(`status='pending'`) for administrator approval, because refusing to create it leaves nothing to approve;
`deny` refuses, so accounts must be pre-provisioned. The domain allowlist applies in all three — a second
lock for a wide IdP tenant, not a substitute for the IdP. Existing accounts are never re-evaluated, since
approval is a one-time event and re-deciding on each login would turn a configuration change into an outage.
Evidence: `go test ./internal/server -run TestJIT` (6 cases).

### Round 3: §4.1, the ACL management API

The ACL could store, evaluate, and list grants, and nothing could create one except SQL. `POST /api/acl/`
and `DELETE /api/acl/{id}` were added. Granting is itself a permission (`manage_permissions`) rather than
owner-only, so an owner can delegate handing out access and revoking it is one deleted grant. Subjects are
named as people know them — user name, email, or group name — and resolved by the server, so a typo is 404
instead of a grant pointing at a nonexistent id. Revocation checks that the grant belongs to the node, since
the id appears in listings of every node the caller can manage. Evidence:
`go test ./internal/server -run TestACL` (7 cases).

### Round 3, continued: The ACL stub in the test harness

The test accessors in `internal/server` and `internal/dav` had `OwnerGroupPerm` returning `0, false, nil`, so
the whole HTTP and WebDAV surface ran with ACLs disabled and a handler that ignored authorization could not
turn a test red. Both harnesses now use the real `EffectivePermsFor`.

### Round 3, continued: §3.3, the local break-glass administrator

The key existed and nothing read it: the password file's existence was checked and the file never opened, so
a documented break-glass account was fictional at the moment it was needed. Now:
`POST /api/auth/breakglass` returns 404 when disabled, so the endpoint's existence is not disclosed; the
source subnet is checked before the password, from `RemoteAddr` rather than a forwarding header this process
cannot verify; user name and password are compared in constant time; every attempt is audited
(`auth.breakglass.used` / `.denied`, with a reason); the account is a real user row
(`idp_issuer = local://breakglass`); startup verifies the file is readable; the file is re-read on every
attempt, so rotating during an incident needs no restart; and `newcloud breakglass hash` reads from stdin to
stay out of shell history. Argon2 moved from `internal/share` to `internal/password`, so both password paths
share one implementation.

Evidence: `go test ./internal/server -run TestBreakglass` (6 cases), `go test ./internal/password` (7 cases),
and on a real instance 200 with `content_access: false`, 401 for a wrong password or user name, 403 for a
wrong subnet, and 401 for the old password after rotation.

### Round 3, continued: ID-02, the OIDC nonce

PKCE binds the code to the client and state binds the callback to the browser, but neither binds the ID token
to the request that started it. An authorization request nonce compared in constant time at the callback,
plus rejecting any callback without the nonce cookie, closes it. The first three nonce tests only asserted
that a nonce was sent and stayed green with the comparison deleted; a `TamperNonce` switch in the fake IdP
finally tests the comparison, whose removal reports `got 302, want 401`.

### Round 3, continued: A regression fix, administrators and downloads

`expandComposite` expanded `PermRead` to `PermDownload`, and the administrator branch relied on "read but not
download" to keep administrators out of user file content. After the expansion the bits said the opposite
while the comment did not. The branch now skips expansion, pinned by `TestAdminCannotDownloadUserContent`.

### Round 2: T-02, role and group changes reach live sessions

The last failing acceptance example of chapter 3. Roles were refreshed only at login while the middleware read
`users.role` on every request, so someone removed from the administrator group stayed an administrator for
the session TTL (12 hours by default), and WebDAV App Passwords never spoke to the IdP again, making the old
permissions permanent.

`Tx.ApplyIdPRole` compares the asserted role with the stored one at login. A demotion also revokes every
session and App Password, since those carry the old permission forward; a promotion touches no credential,
which would otherwise be "grant more, then log out"; an unchanged role is a no-op; and the change is audited
(`user.role.change`, a demotion as `denied`). `UpsertIdPUser` used to write the role first, so `ApplyIdPRole`
never saw a change and demotion never revoked; only `ApplyIdPRole` writes it now.

```
go test ./internal/store -run 'Conformance/ApplyIdPRole'   # three subcases, both backends
go test ./internal/server -run TestGroupRemovalRevokesTheLiveSession
```

The second is the acceptance case: save the old cookie, simulate the IdP removing the administrator group,
log in again, replay the cookie, expect 401.

### Round 2, continued: §4.1, resource ACLs take effect

`acl_entries` was dead SQL, read and written by no Go code, and `OwnerGroupPerm` returned `0,false,nil`, so
the documented three-layer model had two layers.

| Part | Implementation |
| --- | --- |
| primitives | added `download`, `create`, `overwrite`, `rename`, `move`, `manage_permissions`, and `view_history` |
| granting | `Tx.Grant` (user or group, exactly one), replacing rather than accumulating on one (node, subject), backed by a partial unique index |
| inheritance | one recursive CTE; `inherit = false` applies to that node only, the "protected subdirectory inside a shared folder" case |
| groups | real reads and writes of `groups` and `group_members`; a group grant applies only to members |
| resolution | `Tx.EffectivePermsFor` returns the union, with no deny bit |
| delivery | `Tx.SharedWithMe` plus `GET /api/shared` and `GET /api/shared/{id}` |

Composition: `write` implies `create + overwrite`, `read` implies `download`, neither implies `delete`.

Delivery was the valuable finding: `resolvePath` had `_ = user`, discarding the URL's user segment, so grants
were stored and evaluated correctly and no entry point routed a request into someone else's tree. Namespace
isolation was not loosened; `/api/shared` lists the granted roots and `/api/shared/{id}` addresses one by
unguessable UUID, still through the authorizer. `TestStoreGrantReachesTheAuthorizer` is a wiring test:
conformance proves the grant is stored, the authz matrix proves the authorizer uses it, and nothing proved
the two were connected, because they were not. Real HTTP:

```
GET /api/shared       owner    -> 200 []
GET /api/shared       granted  -> 200 [team(read+list+download)]
GET /api/shared       outsider -> 200 []
GET /api/shared/{id}  granted  -> 200
GET /api/shared/{id}  outsider -> 404 (indistinguishable from "does not exist")
```

### Round 2, continued: §11.1, the readiness probe checks dependencies

`/readyz` returned a fixed 200, so a container reported healthy with the database down. It now makes one
metadata round trip (the cheapest query proving the schema is usable) and one content store read, with a
3-second timeout, returning 503 and naming the dependency in `checks`.
`TestReadinessReflectsDependencyHealth` breaks the store while running.

### Round 2, continued: ID-01, an email reassigned by the IdP

`users.email` is UNIQUE, or "share with alice@example.com" would be ambiguous, so an email the IdP assigns to
a new `sub` raised a unique violation that surfaced as HTTP 500 — the account did not inherit files only
because login failed. It now maps to `409/403 identity_conflict` plus an audit event, and the in-memory
backend checks uniqueness too, which it did not, so tests passed while production failed. Evidence:
`go test ./internal/store -run TestRecycledEmailIsNotInherited`.

### Round 1: The initial contract pass

The work that preceded the round numbering.

#### INV-02 / T-03: Unified conditional writes

Four write paths existed and only the WebDAV one checked an ETag, in the handler, with the write in another
transaction — a lost-update window; browser upload and version restore had no precondition.

| Layer | Location | Role |
| --- | --- | --- |
| `store.Precondition` | `internal/store/store.go` | the single interpretation of `If-Match` / `If-None-Match` |
| `Tx.WriteFile` | `internal/store/pg.go`, `memory.go` | validates the precondition in the statement that changes the row |
| `tx.lockNode` | `internal/store/pg.go` | `SELECT … FOR UPDATE`, closing the window between check and write |

`dav.handlePut` and `server.publishFile` share it, and `UpdateFile` repeats the ETag predicate so removing
the row lock later still cannot overwrite another version. Two defects fixed: WebDAV `PUT` on overwrite
returned `201` where RFC 4918 §9.7.1 requires `204`; and version restore wrote the ETag as
`blobID.String()[:16]`, an unquoted UUID prefix, so a client's `If-Match` never matched and every subsequent
DAV write returned 412.

```
go test ./internal/store -run 'Conformance/ConditionalWrites'   # both backends
go test ./internal/dav   -run 'TestPutIfMatch|TestPutIfNoneMatchStar'
```

Both implementations pass the same cases deliberately: if one ignored preconditions and PostgreSQL enforced
them, local tests would be green and production would silently lose writes.

#### INV-01 / T-01: Cross-tenant restore, a real vulnerability

`handleRestore` accepted a node id without checking ownership, and `tx.Restore`'s SQL had no `owner_id`
predicate, so any logged-in user with a guessed UUID could restore another person's trashed file into that
person's directory. Restore now validates that the node is in the trash, that `OwnerID == actor.ID()`, and
that the caller can write the parent it is restored into; any failure returns the same 404 as "does not
exist". Evidence: `handleRestore` in `internal/server/files.go` and section 3.

#### §5.3 / §5.2: LOCK takes effect

`activeLock` was called only inside `handleLock`, so PUT, DELETE, MKCOL, COPY, and MOVE never checked locks.
`lockBlocks`, `requireUnlocked`, and `lockAncestors` were added: a locked target returns `423 Locked` with
`DAV:lock-token-submitted`, a `Depth: infinity` parent also refuses child creation, and the holder must
submit the token in the `If` header (RFC 4918 §10.4). Also fixed: directory `COPY` created the target
directory and returned, producing an empty folder while reporting success, and now copies the subtree
recursively with per-level authorization and depth protection; `MOVE` overwriting a target trashed it with no
authorization check and now requires `PermDelete`; and an unsatisfiable Range request returned `200` with the
full entity and now returns `416`.

```
go test ./internal/dav -run 'TestPutRefusedWhileLocked|TestDeleteRefusedWhileLocked|TestMkcolRefusedUnderLockedParent|TestCopyDirectoryIsRecursive|TestMoveOverwriteRequiresDeleteRight'
```

#### INV-07 / T-07: Version references and GC

`pushVersion` never incremented `blobs.refcount` and `trimVersions` never decremented it, while
`CollectableBlobs` looked only at `refcount = 0` and never at `versions`, so a replaced file's historical
version could have its blob collected and "restore an old version" became a permanent 404 months later. The
pair now increments and decrements together, and both `CollectableBlobs` and `DeleteBlob` exclude blobs
referenced by `versions` or `nodes`. Evidence:
`go test ./internal/store -run TestPGExpireVersionsKeepsBlobReferencesConsistent`.

#### §6.2 / T-08: `authorized_download_session`

Downloads were counted per HTTP request, so every Range request of a playback and every resume counted and a
limited quota vanished immediately, and the count happened before `Content.Open`, so a storage fault consumed
an allowance. `download_sessions` plus `Tx.AuthorizeDownload` bind a session to
`(share, node, guest credential)` and the ETag at the time: a Range request reuses it without charging, a
full GET is charged once, a different guest, file, or version is charged again, and content opens before the
charge.

```
go test ./internal/store -run 'Conformance/DownloadSessions'   # 4 subcases, both backends
go test ./internal/server -run TestShareDownloadLimit
```

Section 3 shows six Range requests counting as one download, and a quota of 2 refusing the third full
download.

#### SHARE-02 / T-09: One-time codes and attempt limits

The code was an HMAC ticket valid for 15 minutes with unlimited verification. `share_recipients.consumed_at`
makes it single-use through a conditional UPDATE, so of concurrent submissions only one succeeds;
`share_otp_attempts` records failures per `(share, email)` and locks after 5 in a 15-minute window; the
throttle is evaluated before the code, so a correct guess is refused too; and a throttled response is
identical to a wrong code, because distinguishing them confirms whether the share and the invitation exist.
Evidence: `go test ./internal/share -run
'TestCodeIsSingleUse|TestVerificationIsThrottled|TestThrottleIsPerAddress'`.

#### INV-09 / T-15: `audit verify` and a comparable chain head

The chain existed and no production code read it: `VerifyAuditChain` was referenced only by tests. An
unverified tamper-evident log is an ordinary log. `newcloud audit verify [--json] [--batch N]` recomputes the
canonical encoding and hash of every event in pages, trusting no stored value; compares the chain head, which
catches truncation, since the remainder after a deleted tail is still a valid prefix; and uses the exit code
as the contract, 0 intact and non-zero broken.

```
go test ./internal/auditverify                     # intact / rewritten / truncated / middle deletion / empty chain
go test ./internal/store -run TestPGAuditVerify    # rewrites one row in real PostgreSQL and asserts refusal
DATABASE_URL=… ./bin/newcloud audit verify
```

#### §8.1 / §8.2: Retention policy and a real delete path

There was no delete path at all (`grep 'DELETE FROM nodes'` returned nothing), the trash only grew, and
`trash_retention_days` existed in documentation but not in configuration. Four keys now live in
`config.LimitsConfig`: `version_retention_days`, `version_count_limit`, `trash_retention_days`, and
`upload_session_ttl_hours`. `Tx.PurgeTrash` deletes expired subtrees recursively, releasing quota, zeroing
references, and deleting versions with them; `Tx.ExpireVersions` expires historical versions while keeping
references correct; a worker runs every 6 hours; 0 means keep forever, and an unconfigured value warns
loudly at startup. Evidence: `go test ./internal/store -run 'TestPGPurgeTrash|TestPGExpireVersions'`.

#### T-19: Cursor pagination

Listing was "the first N plus a `truncated` boolean" with no cursor, and OFFSET over name ordering duplicates
or skips entries when siblings change concurrently. `Tx.ChildrenAfter(parent, after, limit)` uses the sort
key itself as the cursor, and `handleList` accepts `cursor` and `limit` and returns the next one. Evidence:
`go test ./internal/store -run 'Conformance/ChildrenAfter'`, including a case that inserts siblings during
pagination.

#### §3.2: App Password scope takes effect

`dav.authenticate` discarded the credential and granted `ScopeRead|ScopeWrite` unconditionally, so a
read-only App Password could write over WebDAV. The scope now comes from the credential and reaches
`authz.NewUserActor`, and `auth.ScopeForActor` is no longer dead code. `RevokeUserAppPasswords` was added,
and `DisableUser` revokes sessions **and** all credentials; it previously revoked only sessions, so WebDAV
credentials kept working for months.

#### §10.1: Missing login failure events

`auth.login.failure` and `user.disable` existed only as constants with no write site. Login failures (failed
validation, disabled account) and session revocation are now audited.

## 2. Remaining gaps

Implemented rows cite the round that closed them. Everything else is a capability this build cannot claim
today, and the impact column says what that costs.

| Requirement | Status | Impact |
| --- | --- | --- |
| ACL | Implemented — Round 30 | `FolderAccess` on the Access page loads a folder's direct grants by path, revokes one at a time, and creates grants; before it, sharing a folder required curl. Batch operations and a "who can access this file" view are outstanding. |
| Account approval UI | Implemented — Round 31 | `views/Admin.tsx` filters accounts by status and offers only the actions valid for the state; the navigation item is appended for administrators only, and the server returns 404 to others (`TestAdminEndpointsAreInvisibleToNonAdmins`). |
| §11.1 Metrics | Implemented — Rounds 32 and 33 | `/metrics` is scraped over HTTP by the tests, and the three families production code never wrote (`ObserveUpload`, `ObserveThumbnail`, `ObserveBlobGC`) are wired, guarded by `TestEveryMetricFamilyIsFedByProductionCode`. `deploy/monitoring/` ships a scrape configuration, 18 alert rules that pass `promtool check rules`, and the runbook chapters. Thresholds are starting values. |
| INV-08 restic | Implemented — Round 29 | `blob-backup.sh` ran end to end (backup, forget, prune, `check --read-data-subset=1/20`) and reported `no errors were found`; a Windows panic in `--one-file-system` and a restore command that produced the wrong layout were fixed. |
| PITR | Implemented — Round 41 | Base backup, deliberate deletion, and recovery to a target time on an `archive_mode=on` instance; the log reads `recovery stopping before commit` and the deleted row returns with the instance promoted. `archive_timeout=5min` was added, which is what makes a 15-minute RPO true on an idle system. |
| restic snapshot restore | Implemented — Rounds 29 and 40 | A snapshot restored into a scratch directory and copied into place yielded 8 of 8 blobs whose SHA-256 matches their file name, in the layout `ab/cd/<hash>` rather than a doubled `blobs/blobs`. |
| Large-database timing | Implemented — Round 37 | Three scale points (130 KiB, 1.2 MiB, 5.5 MiB) hold seconds per MiB between 6.94 and 9.38 across a 43-fold range, checked by `TestDrillScaleMeasurementsAreConsistent`. The range stops seven orders of magnitude below 40 TiB, so it supports no 40 TiB RTO figure. |
| Shared and group-owned spaces | Not implemented | Grants attach to a node with one owner, so a space that outlives an account is not expressible; group grants cover access, not ownership. |
| Shared locks | Not implemented | Locks are exclusive, one principal per lock; the RFC 4918 shared lock scope is absent. |
| REPORT and sync-collection | Not implemented | `REPORT` is absent, so a client wanting incremental DAV synchronization walks the tree with `PROPFIND`. |
| Real S3 interoperability (AWS, MinIO, R2) | Not tested | The fake and moto verify backend logic and minio-go's framing; signatures, redirects, and consistency semantics need a real endpoint, which `TEST_S3_ENDPOINT` provides and which has not been run for AWS, MinIO, or R2. |
| Immediate deprovisioning on IdP events | Bounded, not immediate — WC-GAP-001 | `auth.revalidate_minutes` refuses a session or App Password whose last IdP confirmation is older than the window, so a demotion applies within a documented bound rather than "whenever this person next signs in". `0` disables it and startup warns. A real event source (SCIM or a poll) would revoke at the moment of the change; this does not. |
| Process-level isolation for previews | Implemented — WC-GAP-004 | `limits.thumbnail_isolation` defaults to `process`: the decode runs in a re-executed child, the deadline kills it, input and output are size-capped, and the child gets a minimal environment. `TestAHangingDecodeIsKilledAndTheCallReturns` proves the child is gone. A real OS sandbox (seccomp, cgroup) is still absent. |
| Windows and macOS client validation | Not tested | T-18 needs real clients (Windows, macOS, Linux, rclone, Cyberduck); name normalization is verified at the byte level, which is not a client that folds case its own way. |
| Resumable uploads across restarts | Implemented — WC-GAP-002 | Each session writes a JSON record beside its staging file, fsynced after the bytes; startup recovers live sessions and truncates bytes the record does not acknowledge. `TestASessionResumesAfterARestart` covers it. Cross-node resume is not available: sessions are local to one staging directory. |
| Bundled dashboards | Not implemented | No Grafana dashboard JSON ships; panels depend on the deployment's Grafana version, so the alert rules and runbook chapters carry the operational information. |
| §9.5 Data import and export | Export implemented; import absent | `newcloud export --user --out` writes a standalone tree, a manifest and a verified SHA-256 per file. Versions, trash, ACLs and shares are not included, and there is no import path: moving a deployment between backends is still unsupported. |
| SHARE-08 to SHARE-11 | SHARE-08/09/10 implemented; SHARE-11 not (P2) | Group grants, delegated sharing and the two-direction share listing are implemented and cited by the traceability ratchet. Team-owned spaces remain absent: grants attach to a node with one owner. |
| COPY partial destination | Rolled back — WC-GAP-009 | A failed directory COPY trashes the destination subtree it created, but only when the destination did not pre-exist. `TestCopyDirectoryFailureRollsBackThePartialDestination` covers it, and `TestTheRollbackIsLoadBearing` proves the guard is what removes the artifact. |
| §10.2 Anchor placement | Operational requirement | File anchors detect truncation, but production must place them where database credentials cannot reach; code cannot enforce the choice. |
| §4.2 Composite write checks in DAV | Deliberate | Eight WebDAV parent-directory and LOCK checks use the composite `PermWrite`, because a single PUT is create-or-overwrite; splitting them adds a round trip to every PUT. |
| WAL backlog sampling without privileges | Partial | Without `pg_ls_waldir()` privileges, `newcloud_wal_segments_pending` keeps its previous value rather than reporting 0, so a restricted role sees a stale backlog, not a false one. |
| Archive drain after a recovered fault | Not tested | Archiving resuming once the fault clears has not been exercised on its own; the counters and alert would show it, but the path is unverified. |
| Audit slowdown | Not covered | The policy covers an unavailable audit store, not a slow one; no timeout turns a slow write into a refusal. |
| Real concurrency and kill -9 testing | Not tested | Crash and backup-window behavior is verified by deterministic construction, not by racing a real backup against writes, and there is no concurrent load test. |
| Derived resources after revocation | Not tested | Links, locks, and grants are covered; thumbnails and other derived artifacts of a revoked share have no test. |
| §11.1 Job kinds | Partial | The queue has a consumer and three job kinds; idempotence is each handler's responsibility, and more kinds are pending. |
| ID-08 Account recovery | Operational procedure | A tombstone cannot be undone; recovery means restoring files from the trash and creating a new account, with no automated path. |

## 3. Manual verification record

2026-10-22, single-node PostgreSQL with blobs on local disk, on an independent instance
(`127.0.0.1:18777`, database `newcloud@127.0.0.1:15437`):

```
# create -> 201; overwrite -> 204 (no longer 201)
PUT /dav/<user>/a.txt            -> 201  ETag "3bfc…"
PUT /dav/<user>/a.txt            -> 204  ETag "b10f…"

# T-03: a write with a stale ETag is refused, and the bytes did not land
PUT … If-Match:"3bfc…"           -> 412
GET …                            -> "someone-else-wrote"

# §5.3: the lock blocks writes
LOCK …                           -> 200  <opaquelocktoken:…>
PUT  … (no token)                -> 423
DELETE … (no token)              -> 423
PUT  … If:(<token>)              -> 204  -> content updated
UNLOCK …                         -> 204

# T-01: cross-account restore refused with 404; the owner's restore still 200
POST /api/files/restore {victim node id} (attacker)  -> 404
POST /api/files/restore {same node}      (owner)     -> 200

# COPY recursion
COPY /dav/<user>/src -> /dav/<user>/dst   -> 201
GET  /dav/<user>/dst/sub/b.txt            -> 200 "beta"

# T-08: Range requests are not charged again; only a full download is charged
6 × GET /s/<token>/download (Range)       -> all 206/416, download_count = 1
GET /s/<token>/download (full) ×2         -> 200, 200, download_count = 2/2
GET /s/<token>/download (full)            -> 404 (quota exhausted)
```

## 4. What "done" means

The acceptance criterion is that a test reproduces the claim, not that code exists:

```
go test ./...                                   # all pass
TEST_DATABASE_URL=… go test ./internal/store    # adds the PostgreSQL contract tests
./bin/newcloud audit verify                     # exit code 0
```

Of the 20 fault-injection cases in §14 of the requirements, those with a test are T-01, T-02, T-03, T-07,
T-08, T-09, T-12 (partially), T-15, and T-19. The rest remain to be added, and a capability these tests do
not cover is not reported as verified.

At the end of round 2, `go test ./...` passed (282 test cases including the PostgreSQL contract tests), gofmt
and go vet were clean, and the four migration files applied in order to an empty database and were no-ops
when re-applied.
