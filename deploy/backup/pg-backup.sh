#!/bin/sh
# PostgreSQL backup: a compressed logical dump every night plus a weekly base
# backup. WAL archiving (configured in docker-compose.yml) covers the gap
# between dumps; without it the RPO would be 24 hours, not 15 minutes.
#
# A backup that has never been restored is a rumour. Run restore-drill.md
# quarterly and record the measured time.
set -eu

BACKUP_DIR="${BACKUP_DIR:-/srv/backup/pg}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

mkdir -p "$BACKUP_DIR"
umask 077

echo "[$(date -u +%FT%TZ)] dumping database to $BACKUP_DIR/newcloud-$STAMP.dump"
pg_dump --format=custom --compress=9 --file="$BACKUP_DIR/newcloud-$STAMP.dump"

# Verify the dump is readable, not merely present. A truncated dump looks
# exactly like a good one in `ls`.
echo "[$(date -u +%FT%TZ)] verifying dump table of contents"
pg_restore --list "$BACKUP_DIR/newcloud-$STAMP.dump" >/dev/null

find "$BACKUP_DIR" -name 'newcloud-*.dump' -type f -mtime "+$RETENTION_DAYS" -print -delete

echo "[$(date -u +%FT%TZ)] done"
