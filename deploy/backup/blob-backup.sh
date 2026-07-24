#!/bin/sh
# Blob backup with restic.
#
# Content addressing makes this cheap: a blob whose bytes have not changed keeps
# the same path, so restic deduplicates it against the previous snapshot. A 40 TiB
# repository does not mean 40 TiB of nightly transfer — only new blobs move.
#
# The repository must be on different hardware from the primary. A backup on the
# same disk is not a backup.
set -eu

RESTIC_REPOSITORY="${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY (s3:..., sftp:..., /mnt/backup)}"
BLOB_ROOT="${BLOB_ROOT:-/var/lib/newcloud/blobs}"

restic backup "$BLOB_ROOT" \
	--tag newcloud-blob \
	--exclude "${BLOB_ROOT}/tmp" \
	--one-file-system

# Keep a decaying history: 7 daily, 4 weekly, 12 monthly.
restic forget \
	--tag newcloud-blob \
	--keep-daily 7 --keep-weekly 4 --keep-monthly 12 \
	--prune

restic check --read-data-subset=1/20
