# Deployment

The reference deployment is a single Linux host running three containers: `web` (newCloud),
`postgres` (PostgreSQL 17), and `caddy` (TLS and two virtual hosts). The process is a static
distroless binary and runs as a non-root user.

This guide describes a deployment that can be backed up and restored. It does not describe a
high-availability cluster, because newCloud does not require one and a badly built HA pair is worse
than a well-run single node.

## Host layout

| Path | Purpose | Notes |
| --- | --- | --- |
| `/etc/newcloud/config.yml` | Configuration | Read-only mount; contains no secrets if `*_file` keys are used |
| `/etc/newcloud/breakglass.pass` | Argon2id hash for break-glass | Only if break-glass is enabled |
| `/run/secrets/oidc_client_secret` | OIDC client secret | Docker secret or a mounted file |
| `/var/lib/newcloud` | Content root **and** upload staging | Must be one mount point |
| `/var/lib/newcloud-pg` | PostgreSQL data | A separate disk from the content root |
| `/mnt/wal-archive` | WAL archive | A separate disk, ideally off-host |

`/var/lib/newcloud` must be a single filesystem. `blobs/` and `blobs/tmp/` are both under it, and
publishing a blob is a `rename(2)`; across filesystems that becomes a full-file copy. At 40 TiB
that is not a slowdown, it is an outage. Startup performs a real rename probe and refuses to start
if the probe fails.

Keep PostgreSQL data and content on separate physical disks. A restic snapshot of the blob store
should not compete with the database for IO.

## 1. DNS and TLS

Create two names, both pointing at the host:

- `cloud.example.com` — the authenticated application;
- `usercontent.example.com` — uploaded bytes, previews, and share downloads.

The split is a security boundary, not a branding choice. A stored-XSS payload in an uploaded file
must not be able to read the session cookie or reach the application DOM. The configuration
validator rejects a deployment where the two URLs share an origin.

Edit `deploy/Caddyfile` and replace both hostnames. Caddy obtains certificates automatically; ports
80 and 443 must be reachable from the internet (or use DNS-01 if they are not).

## 2. Configuration

```sh
install -d -m 0750 /etc/newcloud
cp deploy/config.example.yml /etc/newcloud/config.yml
$EDITOR /etc/newcloud/config.yml
```

At minimum, set:

- `server.public_url` and `server.usercontent_url` to the two hostnames;
- `auth.issuer`, `auth.client_id`, and the client secret file;
- `auth.group_role_map` to the group names the IdP actually emits;
- `storage.layout` explicitly (`blob` is the default, but the choice should be deliberate);
- `storage.local.root` and `storage.upload_dir` under `/var/lib/newcloud`;
- `audit.anchor_dir` to a location the application cannot rewrite.

The full key reference is in [Configuration](configuration.md).

## 3. Secrets and passwords

The deployment uses two database passwords that must differ:

- `PG_PASSWORD` — the owner (`newcloud`), used by migrations and by the PostgreSQL container;
- `PG_APP_PASSWORD` — the application role (`newcloud_app`), used to serve requests.

The split is what makes the audit table append-only: `newcloud_app` has `SELECT` and `INSERT` on
`audit.events` but no `UPDATE`, `DELETE`, or `TRUNCATE`. The application never connects as the
owner.

```sh
export PG_PASSWORD="$(openssl rand -base64 32)"
export PG_APP_PASSWORD="$(openssl rand -base64 32)"
```

Store these in a secret manager or an `.env` file that is not committed and is readable only by
the deployment user. Avoid characters that need URL-encoding in a password, or URL-encode them
consistently.

Generate the break-glass hash only if break-glass is enabled:

```sh
newcloud breakglass hash < /run/secrets/breakglass-password
```

The command reads the password from stdin and prints the Argon2id hash for
`local_admin.password_argon2_file`.

## 4. First boot

Start PostgreSQL first, apply the schema as the owner, then set the application role's password:

```sh
cd deploy
docker compose up -d postgres

docker compose run --rm \
  -e DATABASE_URL="postgres://newcloud:${PG_PASSWORD}@postgres:5432/newcloud?sslmode=disable" \
  web migrate

docker compose exec postgres psql -U newcloud -d newcloud \
  -c "ALTER ROLE newcloud_app PASSWORD '${PG_APP_PASSWORD}'"

docker compose up -d
```

The migration that creates `newcloud_app` deliberately leaves it without a password. A password in
a migration file would be in version control forever and shared by every environment.

Check readiness before exposing the service:

```sh
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

`/healthz` answers when the process is alive. `/readyz` also probes the metadata store and the
content store. A container that answers `healthz` but not `readyz` normally has a database problem.

## 5. The first administrator

Sign in through the IdP with an account whose group maps to `admin`. If no such account exists:

1. Put a test identity in the group mapped to `admin` in the IdP, or
2. temporarily enable break-glass with a narrow CIDR, sign in, create the account, then disable it.

The break-glass account has no web UI by design. Its login endpoint is
`POST /api/auth/breakglass`; use is audited and increments a metric that raises a critical alert.

## 6. Backups

Two artefacts, taken in this order:

1. `pg_dump` — the metadata and the audit chain;
2. the blob store — the bytes.

The order follows the write invariant: content lands before its row. A write inside the backup
window therefore leaves an orphan blob (harmless) rather than a row whose bytes were never copied.
The reverse order can produce a restore that references missing bytes.

Restore in the opposite direction: blobs first, then `pg_restore`. That order fails closed — there
is a window with no metadata, so nothing is served incorrectly — while the reverse order serves
temporary `404`s for files whose bytes have not landed yet. Both converge; the recommended order is
the one that cannot look successful while being wrong.

```sh
deploy/backup/backup-all.sh
```

The script writes a manifest, dumps the database, runs restic, and verifies the result. The
procedure, the PITR path, and the drill record are in
[the restore drill runbook](../../deploy/backup/restore-drill.md).

Run `newcloud drill` after changing the schema or the backup scripts. It restores into a scratch
database, checks every table against the live schema, verifies the audit chain, and compares blob
bytes. A drill that has never failed is a drill that has never been tested; the suite includes
reverse tests that corrupt one event or one blob byte and require a failure.

## 7. Upgrades

1. Take a backup.
2. Pull or build the new image.
3. Run `migrate` with the owner URL.
4. Restart `web`.
5. Verify `/readyz`, a login, and one upload/download.

Migrations are additive. A rollback of the binary is safe only if the previous version does not
need to write the new columns; when in doubt, restore the backup rather than downgrade in place.

The build runs `go vet` and `go test` inside the image, so a failing test stops the image from
being produced.

## 8. Hardening checklist

- [ ] Both public hostnames terminate TLS; HTTP redirects to HTTPS.
- [ ] `public_url` and `usercontent_url` are different origins.
- [ ] The application connects as `newcloud_app`, not the owner.
- [ ] `audit.anchor_dir` is on different credentials from the database.
- [ ] `/var/lib/newcloud` is one mount point; the startup rename probe passes.
- [ ] PostgreSQL data and content are on separate disks.
- [ ] WAL archiving is enabled, `archive_timeout` is set, and the archive disk has free space.
- [ ] Backups run on a schedule and a restore drill has passed.
- [ ] `/metrics` is scraped; the alert rules are loaded; the runbook is reachable.
- [ ] Break-glass is disabled, or enabled with a narrow CIDR and alerting.
- [ ] Retention values are chosen; `0` is understood as "keep forever".
- [ ] The host firewall exposes only 80/443 and the administrative network.

## 9. S3 instead of local content storage

S3 removes the content volume, not the staging volume. Resumable uploads are staged on local disk
because the content hash must be known before an object can be named.

```yaml
storage:
  backend: s3
  layout: blob          # tree is rejected with S3
  upload_dir: /var/lib/newcloud/uploads
  s3:
    endpoint: s3.example.com
    bucket: newcloud
    region: us-east-1
    secure: true
```

The bucket must already exist. The server checks connectivity at startup and fails if the bucket
is unreachable. Creating buckets requires account-level permission that a storage server should
not need, and a typo in a bucket name should fail at boot rather than silently create a second one.

`tree` layout is rejected with S3: object storage has no directory hierarchy to mirror.
