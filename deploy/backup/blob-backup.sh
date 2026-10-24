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
#
# --json so the snapshot id can be captured. A manifest that records "restic:latest" does
# not identify anything: the next `forget --prune` invalidates "latest", and a restore then
# silently picks whatever is newest rather than what the dump was taken against. The id is
# printed on the last line of the JSON summary and read by backup-all.sh.
restic backup "$BLOB_ROOT" \
	--tag newcloud-blob \
	--exclude "${BLOB_ROOT}/tmp" \
	$RESTIC_EXTRA_FLAGS \
	--json >/tmp/newcloud-blob-backup.json

# The snapshot id, for the run manifest. `restic backup --json` emits one summary object;
# jq is not assumed to exist, so the id is extracted with sed.
BLOB_SNAPSHOT_ID="$(sed -n 's/.*"snapshot_id":"\([a-f0-9]\{64\}\)".*/\1/p' /tmp/newcloud-blob-backup.json | tail -n 1)"
if [ -z "$BLOB_SNAPSHOT_ID" ]; then
	echo "blob-backup: could not read the snapshot id from restic's output" >&2
	cat /tmp/newcloud-blob-backup.json >&2
	exit 1
fi
echo "$BLOB_SNAPSHOT_ID" >"${BLOB_SNAPSHOT_ID_FILE:-/tmp/newcloud-blob-snapshot-id}"

# Keep a decaying history: 7 daily, 4 weekly, 12 monthly.
restic forget \
	--tag newcloud-blob \
	--keep-daily 7 --keep-weekly 4 --keep-monthly 12 \
	--prune

# The prune above renumbers what "latest" points at, which is exactly why the id — not the
# word latest — is what the manifest records. Check that the snapshot still exists after
# the prune, so a backup that forgets its own newest snapshot is caught now rather than at
# restore time.
restic snapshots "$BLOB_SNAPSHOT_ID" >/dev/null

restic check --read-data-subset=1/20
