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

# The connection string is passed through libpq's environment, not argv.
#
# `pg_dump --dbname=<DSN>` puts the database password in the process argument list, where
# every local user can read it in `ps` for the whole duration of a multi-gigabyte dump
# (2026-10-24 review, H1-ops). libpq reads the individual PG* variables, not a whole URI, so
# a postgres:// DSN is split into PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE here. Anything
# that is not a URI (a service name, or a keyword/value string) is passed through as given,
# because it may reference a password file rather than carrying one.
case "$CONNECTION" in
postgres://* | postgresql://*)
	_rest="${CONNECTION#*://}"
	_userinfo=""
	case "$_rest" in
	*@*) _userinfo="${_rest%%@*}"; _rest="${_rest#*@}" ;;
	esac
	_hostport="${_rest%%/*}"
	_dbpart="${_rest#*/}"
	_db="${_dbpart%%\?*}"
	if [ -n "$_userinfo" ]; then
		case "$_userinfo" in
		*:*) PGPASSWORD="${_userinfo#*:}"; PGUSER="${_userinfo%%:*}" ;;
		*) PGUSER="$_userinfo" ;;
		esac
	fi
	PGHOST="${_hostport%%:*}"
	case "$_hostport" in
	*:*) PGPORT="${_hostport#*:}" ;;
	esac
	[ -n "$_db" ] && PGDATABASE="$_db"
	export PGPASSWORD PGUSER PGHOST PGPORT PGDATABASE
	DUMP_TARGET=""
	;;
*)
	DUMP_TARGET="$CONNECTION"
	;;
esac

echo "[$(date -u +%FT%TZ)] dumping database to $BACKUP_DIR/newcloud-$STAMP.dump"
# shellcheck disable=SC2086 # DUMP_TARGET is intentionally empty for the URI form
pg_dump ${DUMP_TARGET:+--dbname="$DUMP_TARGET"} --format=custom --compress=9 \
	--file="$BACKUP_DIR/newcloud-$STAMP.dump"

# Verify the dump is readable AND is this application's database.
#
# A readable table of contents proves the file is not truncated; it does not prove the dump
# came from the database you meant. `pg_dump` of an empty or wrong database produces a
# perfectly valid TOC, and the backup report says "done" — which is how a nightly job
# against a recreated container silently backs up nothing (2026-10-24 review, M14).
echo "[$(date -u +%FT%TZ)] verifying dump table of contents"
TOC="$(pg_restore --list "$BACKUP_DIR/newcloud-$STAMP.dump")"
printf '%s\n' "$TOC" >/dev/null

# The tables that must be present for a restore to be a newCloud restore. Schema-qualified
# where the schema is not public, because pg_restore --list prints "TABLE DATA audit events".
for required in 'public nodes' 'public users' 'public shares' 'public blobs' 'audit events'; do
	if ! printf '%s\n' "$TOC" | grep -q "TABLE DATA $required"; then
		echo "pg-backup: the dump has no '$required' table — it is not a newCloud database" >&2
		exit 3
	fi
done

find "$BACKUP_DIR" -name 'newcloud-*.dump' -type f -mtime "+$RETENTION_DAYS" -print -delete

echo "[$(date -u +%FT%TZ)] done"
