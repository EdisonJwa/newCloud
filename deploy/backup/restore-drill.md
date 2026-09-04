# Restore drill runbook

**This is an M5 acceptance item, not optional documentation.**

A backup exists only once it has been fully restored once. The drill records actual elapsed time and
writes the result into this file.

## Why a single-machine deployment needs this

A single-machine Docker deployment has no replica to take over. Total host failure, disk damage,
accidental deletion, ransomware: the only exit for all of them is the backup. Availability is
bounded by that one machine, so RTO depends entirely on how well the restore drill is practiced.

## RPO / RTO targets

| Data | RPO | RTO |
| --- | --- | --- |
| Metadata (PG) | `RPO <= 15 minutes` (continuous WAL archiving) | <= 1 hour |
| Blob content | <= 24 hours (restic incremental) | Depends on volume; 40 TiB requires <= 24 hours |

## Drill procedure

### 1. Prepare a clean environment

**Do not run the drill on a live instance.** Use an empty machine with equivalent configuration;
otherwise the drill pollutes production or produces misleading timings.

```bash
mkdir -p /var/lib/newcloud /var/lib/newcloud-pg
docker compose up -d postgres
```

### 2. Restore PostgreSQL

**Two paths; which one applies depends on whether the target is "the moment of the backup" or "the
moment before the failure".**

#### 2a. Restore to the backup moment (simple path)

```bash
pg_restore --clean --if-exists --no-owner \
  -d postgres://newcloud:$PG_PASSWORD@localhost:5432/newcloud \
  /srv/backup/pg/newcloud-<STAMP>.dump
```

Data returns to the moment of the dump; every write after the dump is lost. RPO equals the interval
between dumps (24 hours by default).

#### 2b. Restore to a point in time (PITR, RPO <= 15 minutes)

Three things are required: a **base backup**, continuous WAL archiving with `archive_mode=on`, and a
**target time**.

```bash
# 1. Base backup (online, does not block writes)
pg_basebackup -h localhost -U newcloud -D /srv/pitr/base -Ft -z -X stream

# 2. Unpack the base backup into the data directory
mkdir -p /srv/pitr/data
tar -xzf /srv/pitr/base/base.tar.gz -C /srv/pitr/data
mkdir -p /srv/pitr/data/pg_wal
tar -xzf /srv/pitr/base/pg_wal.tar.gz -C /srv/pitr/data/pg_wal

# 3. Set the replay target
cat >>/srv/pitr/data/postgresql.auto.conf <<'EOF'
restore_command = 'cp /mnt/wal-archive/%f %p'
recovery_target_time = '2026-10-22 09:05:07.74+00'
recovery_target_action = 'promote'
EOF
touch /srv/pitr/data/recovery.signal

# 4. Start. The log shows "recovery stopping before commit of transaction ..."
pg_ctl -D /srv/pitr/data start
```

Key points:

- `recovery_target_action = 'promote'`: without it the instance stops in a read-only state, so the
  recovery completes but the database cannot be used.
- `recovery.signal` (PG12+): recovery mode is entered only when the file exists. Some older
  documentation says `recovery.conf`; that path is no longer supported.
- **The target time must precede the failure.** Replaying a little less and re-entering data
  afterwards is preferable to replaying past the mistaken operation.

**Measured (round 41)**: on an instance with `archive_mode=on` and `archive_timeout=5s`, insert two
rows, record the time, delete the second row, then recover from the base backup plus WAL archive to
a point before the deletion. The recovery log showed
`recovery stopping before commit of transaction 742`; after recovery `select id from probe` returned
**1, 2** (the deleted row was back) and `pg_is_in_recovery()` returned `false`. The whole run took
about 0.3 seconds of redo.

Validation:

```sql
-- Row counts should match the backup
SELECT count(*) FROM nodes;
SELECT count(*) FROM audit.events;

-- Audit chain integrity: the head hash must match the most recent anchor record
SELECT day, encode(head_hash,'hex') FROM audit.anchors ORDER BY day DESC LIMIT 1;
```

### 2.5 Backup order: take the pg_dump first, then copy the blobs (enforced by `backup-all.sh`)

The order is not a style question. A backup is two artifacts taken at two separate moments — a
database dump and a copy of the blob tree — and they cannot be captured atomically, so there is
always a window between them. Whether a write inside that window produces a restore that references
bytes that do not exist depends on the **order**, and which order is safe depends on one invariant
of the write path:

> **Content is written to disk first; the metadata row is written after.**

(`Content.Put` runs before `publishFile` in `internal/server/files.go`;
`TestBlobIsWrittenBeforeItsRow` guards this invariant.)

Given that invariant:

| Order | Write inside the window | Result after restore |
| --- | --- | --- |
| **pg_dump first, then copy the blobs** | The bytes are already in the copy (written before the row); the row is not in the dump | The database has no record of the file and the copy holds one unreferenced blob. **The cost is a little space, and the file is simply not in this backup** — correct |
| Copy the blobs first, then pg_dump | The row is in the dump; the bytes landed after the copy read that directory | The restored metadata points at content that does not exist, **and every read fails**. The restore looks successful until someone opens that file |

Take the pg_dump first, then copy the blobs.

That leaves orphan blobs, not dangling references. GC reclaims orphan blobs; no mechanism repairs a
dangling reference.

This order is enforced by `deploy/backup/backup-all.sh`, not merely written down here. Before that
script existed, `pg-backup.sh` and `blob-backup.sh` were each correct and unaware of the other, and
the order lived only in that paragraph: whichever half an operator's cron pointed at, and in
whatever order, depended on whether the paragraph had been read. `backup-all.sh` calls the two in
sequence and writes a manifest between them that binds the dump to the blob snapshot; with it the
order is **executable**, not merely **remembered**. `TestBackupAllRunsTheDumpBeforeTheBlobs` guards
it (it reads only executable lines, not comments — see below).

**An earlier version of this section had the order backwards.** The first draft reversed the two
orders, reasoning that "the artifact captured first is older and therefore safer", without checking
**which of the two — bytes or row — lands first** in the write path. A document with the direction
reversed is worse than no document: it reads as advice, and an operator following it would produce
exactly the "restore succeeds but every read fails" state in the table above.
`TestBackupDocsStateTheRightOrder` now asserts the wording.

### 2.6 Restore order: blobs first, then the database

Restore order: restore the blobs first, then run pg_restore.

The direction is the opposite of the backup order, and **the reason is different**: here the
question is not whether data is lost, but what the window looks like.

| Order | During the window | Afterwards |
| --- | --- | --- |
| **Restore the blobs first, then run pg_restore** | No metadata yet; **nothing is readable** | Everything works once the database lands |
| Run pg_restore first, then restore the blobs | Metadata names files whose bytes have not arrived; **reads fail with `blob not found`** | Everything works once the bytes land |

**Both orders converge**, and neither actually corrupts data. "Blobs first" is recommended because
it **fails closed** — nothing readable is less likely to be mistaken for data corruption than "a
file that is read reports not found" — not because the reverse order causes irreversible damage.

**An earlier draft of this section was wrong in the opposite direction, by overstatement.** It
claimed the reverse order "would likewise leave the database pointing at bytes that do not exist".
That was too strong. A backup-order mistake is **permanent** (those bytes were never captured); a
restore-order mistake is **temporary** (the bytes arrive afterwards). Treating the two as the same
thing sends an operator looking for data loss that did not happen.

**Neither order pollutes data, because GC is metadata-driven**: the candidate set for
`CollectableBlobs` is a row scan — `FROM blobs b WHERE b.refcount = 0 AND b.collect_after <= now()`
— so it visits **rows** and never scans the blob directory. A file whose bytes have been restored
but which has no row yet is invisible to GC and cannot be deleted as garbage.
`TestGCIsMetadataDriven` guards this: if the collector ever grew a directory scan, "restore the
blobs first" would go from safe to deleting the entire restore site.

### 3. Restore the blobs

```bash
# Restore to a scratch directory, then move the blob subtree into place.
#
# Both steps are needed, and an earlier version of this page omitted the second. `restic restore`
# recreates the *absolute* path it backed up underneath --target, so restoring
# /var/lib/newcloud/blobs to --target /srv/restore produces
# /srv/restore/var/lib/newcloud/blobs — not /srv/restore/blobs. Pointing --target at the parent
# and hoping --include scopes it does not help either: --include selects what to restore, it does
# not flatten where it lands.
#
# Restoring straight onto the live root is the tempting shortcut and the wrong one: a partial or
# failed restore then leaves a half-populated blob store where the server is already reading.
restic restore latest --target /srv/restore

# Then place it. The trailing slash on the source copies the *contents* of blobs/ into
# $BLOB_ROOT; without it the result is $BLOB_ROOT/blobs/blobs.
BLOB_ROOT=/var/lib/newcloud/blobs
install -d "$BLOB_ROOT"
cp -a "/srv/restore/var/lib/newcloud/blobs/." "$BLOB_ROOT/"
```

Validation:

```bash
# Sample check that the hash matches the file name (the self-validating property of content addressing)
find /var/lib/newcloud/blobs -type f -name '[0-9a-f]*' -print0 \
  | shuf -z -n 1000 \
  | while IFS= read -r -d '' f; do
      want=$(basename "$f")
      got=$(sha256sum "$f" | cut -d' ' -f1)
      [ "$want" = "$got" ] || echo "CORRUPT: $f"
    done
```

### 4. Start and verify

```bash
docker compose up -d
curl -fsS localhost:8080/readyz
```

Functional checks:

- [ ] Login works (Pocket ID is healthy)
- [ ] The file tree is visible, with the correct directory hierarchy
- [ ] Ten random files download with the correct byte counts and hashes
- [ ] Share links resolve, and passwords and expiry still apply
- [ ] The audit page shows recent download records
- [ ] A WebDAV client connects (`rclone lsd` or a mapped network drive)

## Drill records

Fill this in after every drill. No record means it was not done.

| Date | Backup point | Data volume | PG restore time | Blob restore time | Total RTO | Result | Problems found |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-10-22 | First drill | 4 accounts / 21 nodes / 12 blobs / 45 audit events (dump 78.2 KiB) | 1.12 s | 1.22 s (local file copy) | 1.22 s | **PASS** | See below |
| 2026-10-22 | Scale point 1 | 10 accounts / 100 files / 1,000 audit events (dump 130 KiB, 1,171 rows) | — | — | 1.19 s | **PASS** | See "Scale measurements" below |
| 2026-10-22 | Scale point 2 | 50 accounts / 2,000 files / 20,000 audit events (dump 1.2 MiB, 22,301 rows) | — | — | 8.32 s | **PASS** | As above |
| 2026-10-22 | Scale point 3 | 100 accounts / 10,000 files / 100,000 audit events (dump 5.5 MiB, 110,401 rows) | — | — | 40.78 s | **PASS** | As above |

### First drill, 2026-10-22 (automated, `newcloud drill`)

The drill moved from "a manual checklist to follow" to a subcommand because the manual process was
measurably **never executed**: the previous version of this file carried `_pending first drill_`
from the day it was written until 2026-10-22.

The dataset was written for real (6 accounts, 2 directories, 5 files through the real upload API, 1
group, 1 ACL, 40 audit events), not constructed by hand:

```text
restore drill
  dump size        77.2 KiB
  tables compared  20
  rows compared    75
  audit chain      45 events, verified=true
  elapsed          928ms
  RTO              1s (target 1h0m0s) — within target

  VERDICT: PASS — the backup restores and the audit chain survives
```

A second run took 971 ms with the same result.

**Negative validation (essential)**: a drill that can never fail proves nothing, so the backups were
deliberately broken and the drill re-run:

| Break | Drill result |
| --- | --- |
| Change the `action` of one audit record | `FAIL` — `the restored audit chain does not verify at row 0 of 45: hash mismatch` |
| Flip the first byte of one blob | `FAIL` — `top.txt: restored bytes hash to ... metadata recorded ... — the backup is corrupt` |
| `SCRATCH_DSN` equal to `LIVE_DSN` | Refuses to run |
| `SCRATCH_DSN` unset | Refuses to run |

The first row matters most: row counts and table counts **all match**, and the chain verification is
the only thing that exposes the problem. Step 5 is not decorative.

### Blob content validation (added in round 8)

The database drill only proves that **metadata** survives; it says nothing about bytes, and bytes
are the user's actual data. A restore can bring back a perfectly consistent node tree pointing at
blobs that were never copied, or that were truncated during the copy: the deployment can then list
every file and open none of them.

The drill now performs blob validation at step 6: copy the blob root, open a content store on the
copy, and read **every file node** back for comparison.

Key design: **the expected hash comes from metadata, not from the surviving copy**. The first
version compared "restored bytes" with "existing bytes" and still reported **PASS** after a blob was
deliberately corrupted, because both sides were corrupt and therefore equal. After the change to
validate against the content address recorded in `blobs.sha256`, the same corruption is caught
immediately:

```text
blobs            12 files, 11 verified, 31.3 KiB
VERDICT: FAIL
  - top.txt: restored bytes hash to c0d5f09b9940, metadata recorded 5029feaf3e6b — the backup is corrupt
```

This is an instance of "the test passed but verified nothing", and it is the most valuable thing to
record from that round.

**Three things the drill had not verified at that point** (stated next to the pass, not buried
afterwards):

- ~~**Restoring the restic repository itself** was not run.~~ **Addressed (rounds 28 and 29)**:
  after installing restic 0.19.1, the full `blob-backup.sh` chain ran (backup → forget → prune →
  `restic check` reporting "no errors were found") and `restic restore` ran. The entire blob root
  was deleted and restored, and both blobs' SHA-256 matched the originals byte for byte. **Two real
  problems were found and fixed in the process**: (1) `--one-file-system` panics outright on Windows
  ("Device IDs are not supported on Windows"), and the script exited before storing a single byte;
  (2) the restore command in this document was wrong — `restic restore --target X` recreates the
  **absolute path** that was backed up underneath X, producing `X/var/lib/newcloud/blobs` rather
  than `X/blobs`, and `--include` selects **what** to restore without changing **where** it lands.
  The procedure is now the two-step "restore to a scratch directory, then move it" flow, measured
  end to end. The difference between the two is that the first validates content integrity while the
  second also validates snapshot management and the deduplication index.
- ~~**Timing for a large database** was unknown. 78 KiB took about 1.2 seconds and cannot be
  extrapolated to 40 TiB.~~ **Progress (round 37)**: see "Scale measurements" below. Three data
  points spanning 43x show that **elapsed time grows linearly with dump size**, with no superlinear
  knee — far more useful than a single measurement, but **still not an RTO conclusion for 40 TiB**;
  see the limitations in that section.
- **PITR** (recovery from WAL archiving to an arbitrary point in time) had not been drilled. What
  was verified was "restore from a dump", not "recover to the moment before the failure"; the two
  differ by an order of magnitude in RPO.

### Running on containerized PostgreSQL

When `pg_dump` and `pg_restore` run inside a container, the addresses differ on the two sides (5432
inside the container, 15437 published on the host):

```bash
DRILL_PG_CONTAINER=newcloud-postgres \
DRILL_DUMP_DSN=postgres://newcloud:$PG_PASSWORD@127.0.0.1:5432/newcloud?sslmode=disable \
DRILL_RESTORE_DSN=postgres://newcloud:$PG_PASSWORD@127.0.0.1:5432/newcloud_drill?sslmode=disable \
LIVE_DSN=... SCRATCH_DSN=... newcloud drill
```

Handing the host address to a tool inside the container produces `Connection refused`, which looks
like a broken database but is only a misconfigured address.

## Common pitfalls

| Symptom | Cause |
| --- | --- |
| `pg_restore` reports an audit schema permission error | Migration `0005_app_role.sql` restates the grants; a manual restore requires `GRANT INSERT, SELECT ON audit.events TO newcloud_app` |
| Files exist after a blob restore but cannot be opened | `blobs/tmp` and `blobs/` are on different mount points, so the startup self-check refuses; check the mounts |
| Restore takes far longer than expected | The backup and production share a disk; that is a replica, not a backup |
| Audit chain verification fails | The anchor and the database are out of sync, meaning an old dump was restored while the anchor is newer |

## Scale measurements (round 37)

### Method

`tools/seedscale` generates accounts, folders, files, and audit events at a specified scale (through
the real store, so the audit hash chain is real), then `newcloud drill` measures the full dump →
restore → verify time on the same machine.

```sh
LIVE_DSN=... SCRATCH_DSN=... DRILL_PG_CONTAINER=... \
DRILL_DUMP_DSN=... DRILL_RESTORE_DSN=... \
  go run ./cmd/newcloud drill
```

### Results

| Scale | Dump | Rows | Elapsed | s/MiB |
| --- | --- | --- | --- | --- |
| 10 accounts / 100 files / 1,000 events | 130 KiB | 1,171 | 1.19 s | 9.38 |
| 50 accounts / 2,000 files / 20,000 events | 1.2 MiB | 22,301 | 8.32 s | 6.94 |
| 100 accounts / 10,000 files / 100,000 events | 5.5 MiB | 110,401 | 40.78 s | 7.41 |

All three points were `PASS`, and the audit chain was `verified=true` after every restore (1,000 /
20,000 / 100,000 events respectively).

### What it supports

**Elapsed time grows linearly with dump size.** "Seconds per MiB" stays between **6.9 and 9.4**
across a 43x range of volume, with no upward trend as scale increases; volume growth of 9.4x and
4.6x produced time growth of 7.0x and 4.9x, slightly below linear (fixed cost is amortized). This
rules out "suddenly slower past some threshold", which is exactly what a single-point measurement
cannot rule out and what extrapolation fears most.

### What it does not support

**This is not an RTO conclusion for 40 TiB.** Three limitations must be read together:

1. **The measurement ceiling is 5.5 MiB**, seven orders of magnitude below 40 TiB. A linear
   extrapolation at the median 7.9 s/MiB gives about **92,000 hours (about 3,800 days)** for 40 TiB
   (= 41,943,040 MiB) — and that number is itself the conclusion: **if the cost really were purely
   linear, 40 TiB of metadata could not be restored within 24 hours.** Reality must therefore take a
   different path (parallel `pg_restore`, partition-level parallelism, or a metadata volume far
   smaller than 40 TiB with the bulk in blob bytes). The honest statement is that this measurement
   **supports no RTO figure for 40 TiB**, in either an optimistic or a pessimistic direction.
2. **Here the whole pipeline ran on one disk and one machine**, with no network round trip between
   `pg_dump` and `pg_restore`. In a real deployment the backup artifacts travel over the network,
   and that leg is governed by bandwidth, not by this measurement.
3. **`pg_size_pretty` reports logical size**, and in a 40 TiB deployment the blob byte count is far
   larger than the metadata. What was measured here is the **metadata** restore cost; the blob
   restore cost is a different curve (restic incremental restore, see above).

The conclusion should be read as: **linear up to 5.5 MiB, with no knee observed; anything beyond
that range has not been measured.** A real RTO conclusion still requires a rerun at production
volume on equivalent hardware.

> **A note about this section itself**: the first version of this section wrote the extrapolation as
> "about 82 hours" when the arithmetic gives about 92,000 hours — **three orders of magnitude
> apart**, and the figure drives the conclusion directly (82 hours is close to the RTO target and
> would suggest the target is nearly met; 92,000 hours shows that the linear extrapolation itself is
> the wrong path). `TestDrillScaleMeasurementsAreConsistent` now recomputes the number from the
> measured values in the table and fails when it differs by more than 20%. Putting the wrong number
> back makes it report `a factor of 985`. Arithmetic in prose cannot be reviewed by eye.

### Incidental finding

During the first seeding run, `tools/seedscale` wrote only file metadata and no blob content, and
the drill **immediately reported FAIL**: `the restored copy cannot serve it: blob: not found`, with
100 files and 0 readable. That is exactly why the drill exists: the difference between a database
that "looks like it has data" and one that can actually serve it is the difference only the drill
reports.

## First real execution of the backup scripts (round 40)

Before this, the three backup scripts **had never actually been run**: they had tests checking order
and configuration, but no one had executed them once. This round installed `pg_dump` (scoop
`postgresql` 18.6) and `restic`, and ran the whole chain against real data.

**The first problem it exposed**: `pg-backup.sh` invoked **bare `pg_dump`**, which takes connection
parameters from `PG*` environment variables and **ignores `DATABASE_URL` entirely**. On a machine
without those variables it dumps the `postgres` database on `localhost:5432` — or fails outright, as
it did here — while the operator believes a backup exists. It now runs
`pg_dump --dbname="$CONNECTION"` and exits with an error when `DATABASE_URL` (or `DSN`) is missing,
instead of silently using defaults. `TestPgBackupUsesTheConfiguredDatabase` guards it.

**Measured results** (`newcloud_bk`, 5 accounts / 40 files / 200 audit events):

| Step | Result |
| --- | --- |
| `pg_dump` | 88,508 bytes, readable by `pg_restore --list`, includes the `audit` schema |
| manifest | Records the dump path, start time, blob snapshot, and end time |
| `restic backup` | 8 blobs, 4,521 bytes |
| `restic check` | `no errors were found` |
| `restic restore` | 8/8 blobs restored, **each file's SHA-256 matches its name** |
| `pg_restore` (after the blobs) | 55 nodes, 200 audit events |
| **Cross-check** | Blobs required by the restored metadata that are **missing from the restored copy: 0** |

This is the evidence behind "a verified restore is not the same as having backup files": not just
that files exist, but that every byte the restored database points at is actually present.
