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

# The connection is taken from DATABASE_URL.
#
# This script used to call a bare `pg_dump`, which takes its connection from the PG* environment
# variables and ignores DATABASE_URL entirely. That works on a host where those variables happen to
# be set and fails everywhere else — including under backup-all.sh, which exports DATABASE_URL and
# got "connection refused on localhost:5432" while the database was on another port. A backup
# script that silently connects to the wrong database, or to no database, is worse than one that
# refuses to start.
#
# DSN is the fallback name so an existing cron job exporting DSN keeps working.
CONNECTION="${DATABASE_URL:-${DSN:-}}"
if [ -z "$CONNECTION" ]; then
	echo "pg-backup: set DATABASE_URL (or DSN) to the database to dump" >&2
	exit 2
fi

mkdir -p "$BACKUP_DIR"
umask 077

echo "[$(date -u +%FT%TZ)] dumping database to $BACKUP_DIR/newcloud-$STAMP.dump"
pg_dump --dbname="$CONNECTION" --format=custom --compress=9 \
	--file="$BACKUP_DIR/newcloud-$STAMP.dump"

# Verify the dump is readable, not merely present. A truncated dump looks
# exactly like a good one in `ls`.
echo "[$(date -u +%FT%TZ)] verifying dump table of contents"
pg_restore --list "$BACKUP_DIR/newcloud-$STAMP.dump" >/dev/null

find "$BACKUP_DIR" -name 'newcloud-*.dump' -type f -mtime "+$RETENTION_DAYS" -print -delete

echo "[$(date -u +%FT%TZ)] done"
