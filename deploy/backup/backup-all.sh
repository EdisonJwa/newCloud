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
BACKUP_DIR="$BACKUP_DIR" BLOB_ROOT="$BLOB_ROOT" RESTIC_REPOSITORY="$RESTIC_REPOSITORY" \
	./deploy/backup/blob-backup.sh

printf 'blob_snapshot=restic:latest\nfinished_at=%s\n' "$(date -u +%Y%m%dT%H%M%SZ)" \
	>>"$BACKUP_DIR/snapshot-$STAMP.manifest"

log "backup complete: $BACKUP_DIR/snapshot-$STAMP.manifest"
log "restore with: deploy/backup/restore-drill.md (blobs first, then pg_restore)"
