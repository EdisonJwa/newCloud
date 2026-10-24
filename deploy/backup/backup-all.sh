#!/bin/sh
# One backup run, in the order that does not lose data.
#
# Two scripts already existed — pg-backup.sh and blob-backup.sh — and each is correct on its own.
# Neither knows about the other. The order between them was documented in restore-drill.md and
# enforced nowhere, which means the only thing standing between an operator and a restore that
# references bytes nobody captured was whether they read that paragraph.
#
# This script exists so the order is executable rather than remembered. Cron runs this, not the two
# halves.
#
# THE ORDER: database dump first, then the blob copy.
#
# That looks backwards — the blobs are the bulk of the data, so taking them first feels safer — and
# it is the opposite of the restore order. The reason is an invariant of the write path: content is
# stored before the row that references it (s.Content.Put precedes publishFile). Given that:
#
#   dump first, then blobs
#       a write in the window has its bytes in the copy but no row in the dump. The restored
#       database never mentions the file and the copy holds an unreferenced blob. Wasted space,
#       and the file is simply not in this backup. Correct: a backup is a point in time.
#
#   blobs first, then dump
#       a write in the window has a row in the dump whose bytes arrived after the copy read that
#       directory. The restored metadata references content that is not there, and every request
#       for that file fails. The restore looks successful until someone opens it.
#
# An orphan blob is collected by GC. A dangling reference has no repair mechanism.
#
# Tests: internal/server/backuporder_test.go (the property and the invariant),
# internal/store/blobgc_collect_test.go (the collector cannot see restored-but-unrow'd bytes).
set -eu

BACKUP_DIR="${BACKUP_DIR:-/srv/backup}"
RESTIC_REPOSITORY="${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY (s3:..., sftp:..., /mnt/backup)}"
BLOB_ROOT="${BLOB_ROOT:-/var/lib/newcloud/blobs}"
DATABASE_URL="${DATABASE_URL:?set DATABASE_URL}"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
umask 077
mkdir -p "$BACKUP_DIR/pg"

log() { echo "[$(date -u +%FT%TZ)] $*"; }

# ---- 1. The database dump ----
#
# Exported so pg-backup.sh uses this run's directory rather than its own default. A single run
# should produce one datestamp, not one per script: correlating a dump with a blob snapshot is
# impossible if the two disagree about when the backup happened.
log "step 1/2: database dump"
BACKUP_DIR="$BACKUP_DIR/pg" DATABASE_URL="$DATABASE_URL" ./deploy/backup/pg-backup.sh
DUMP_FILE="$(ls -1t "$BACKUP_DIR"/pg/newcloud-*.dump | head -n 1)"
log "dump written: $DUMP_FILE"

# The dump's name is what makes this snapshot identifiable later. Written before the blob copy so
# a snapshot manifest never points at a dump that does not exist.
printf 'database_dump=%s\nstarted_at=%s\n' "$DUMP_FILE" "$STAMP" >"$BACKUP_DIR/snapshot-$STAMP.manifest"

# ---- 2. The blob copy ----
#
# Deliberately second. See the header for why; the short version is that content is written before
# its row, so a window write between the two steps leaves an orphan blob rather than a dangling
# reference.
log "step 2/2: blob copy"
SNAPSHOT_ID_FILE="$BACKUP_DIR/blob-snapshot-$STAMP.id"
BACKUP_DIR="$BACKUP_DIR" BLOB_ROOT="$BLOB_ROOT" RESTIC_REPOSITORY="$RESTIC_REPOSITORY" \
	BLOB_SNAPSHOT_ID_FILE="$SNAPSHOT_ID_FILE" ./deploy/backup/blob-backup.sh

# The manifest records the ACTUAL restic snapshot id, not the word "latest".
#
# "restic:latest" identified nothing: the next `forget --prune` moved what "latest" meant, so a
# restore that followed the manifest silently restored whichever snapshot happened to be newest
# rather than the one this dump was taken against — and nothing in the record could tell the
# difference afterwards (2026-10-24 review, M14).
BLOB_SNAPSHOT_ID="$(cat "$SNAPSHOT_ID_FILE")"
printf 'blob_snapshot=%s\nfinished_at=%s\n' "$BLOB_SNAPSHOT_ID" "$(date -u +%Y%m%dT%H%M%SZ)" \
	>>"$BACKUP_DIR/snapshot-$STAMP.manifest"

log "backup complete: $BACKUP_DIR/snapshot-$STAMP.manifest"
log "blob snapshot: $BLOB_SNAPSHOT_ID"

# Publish the success as a Prometheus textfile metric, so "the backup stopped running" is an
# alert rather than something noticed during a restore.
#
# A backup failure is the only failure whose signal nobody sees: cron mails the operator,
# the mail is filtered, and the first evidence is a restore attempt months later. The
# NewCloudBackupStale alert in deploy/monitoring/alerts.yml reads this file through
# node_exporter's textfile collector; unset BACKUP_METRICS_FILE disables it for hosts that
# scrape the backup box some other way.
if [ -n "${BACKUP_METRICS_FILE:-}" ]; then
	mkdir -p "$(dirname "$BACKUP_METRICS_FILE")"
	# Written atomically: the collector must never read a half-written file.
	{
		printf '# HELP newcloud_backup_last_success_timestamp_seconds Unix time of the last successful backup run.\n'
		printf '# TYPE newcloud_backup_last_success_timestamp_seconds gauge\n'
		printf 'newcloud_backup_last_success_timestamp_seconds %s\n' "$(date -u +%s)"
	} >"$BACKUP_METRICS_FILE.tmp"
	mv "$BACKUP_METRICS_FILE.tmp" "$BACKUP_METRICS_FILE"
	log "backup success published to $BACKUP_METRICS_FILE"
fi

log "restore with: deploy/backup/restore-drill.md (blobs first, then pg_restore)"
