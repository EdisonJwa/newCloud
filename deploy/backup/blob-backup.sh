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

# --one-file-system is conditional, not unconditional.
#
# It stops the backup from wandering into a mounted filesystem underneath the blob root — the
# right default on Linux, where a stray mount under /var/lib/newcloud/blobs would silently pull
# unrelated data into the repository. But restic on Windows *panics* on the flag ("Device IDs are
# not supported on Windows"), which took the whole script down before a single byte was saved.
# A backup script that cannot run on an operator's machine protects nothing, so the flag is
# applied only where it means something.
RESTIC_EXTRA_FLAGS=""
case "$(uname -s)" in
Linux | Darwin) RESTIC_EXTRA_FLAGS="--one-file-system" ;;
esac

# shellcheck disable=SC2086 # the flags are intentionally word-split
restic backup "$BLOB_ROOT" \
	--tag newcloud-blob \
	--exclude "${BLOB_ROOT}/tmp" \
	$RESTIC_EXTRA_FLAGS

# Keep a decaying history: 7 daily, 4 weekly, 12 monthly.
restic forget \
	--tag newcloud-blob \
	--keep-daily 7 --keep-weekly 4 --keep-monthly 12 \
	--prune

restic check --read-data-subset=1/20
