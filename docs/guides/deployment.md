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
validator rejects a deployment where the two URLs share an origin, and it also rejects a shared
**hostname**: browsers scope cookies by hostname alone, so `cloud.example.com` and
`cloud.example.com:8443` are two origins but one cookie jar, and the content listener would receive
`__Host-wc_session`. Use two names, ideally in different registrable domains.

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
- [ ] `public_url` and `usercontent_url` use different hostnames (not only a different port), or the
      `allow_same_host_content` exception is set, logged, and Cookie is stripped on content paths.
- [ ] The application connects as `newcloud_app`, not the owner.
- [ ] `audit.anchor_dir` is on different credentials from the database.
- [ ] `/var/lib/newcloud` is one mount point; the startup rename probe passes.
- [ ] `server.usercontent_addr` is set and the content vhost proxies to it, so a content path on the
      application origin is refused by the process and by the proxy rather than only by convention.
- [ ] The content vhost strips `Cookie` and serves only `/content/*` and `/s/*/download|thumbnail`.
- [ ] PostgreSQL data and content are on separate disks.
- [ ] WAL archiving is enabled, `archive_timeout` is set, and the archive disk has free space.
- [ ] Backups run on a schedule and a restore drill has passed.
- [ ] `/metrics` is scraped; the alert rules are loaded; the runbook is reachable.
- [ ] Break-glass is disabled, or enabled with a narrow CIDR and alerting.
- [ ] Retention values are chosen; `0` is understood as "keep forever".
- [ ] The host firewall exposes only 80/443 and the administrative network.

## 8.1 Running the PostgreSQL test suite (release evidence)

Most of the store, audit and DAV-over-PostgreSQL tests are **skipped** unless `TEST_DATABASE_URL`
is set, so a plain `go test ./...` on a workstation proves nothing about the backend production
uses. To produce actual evidence:

```powershell
# A disposable cluster. Never point this at a database whose contents matter:
# the schema is applied idempotently, but several audit tests DROP and recreate it.
initdb -D $env:TEMP\nc-pg -U newcloud --pwfile=pw.txt -A scram-sha-256 --no-locale
pg_ctl -D $env:TEMP\nc-pg -o "-p 55432" -l pg.log start
createdb -h 127.0.0.1 -p 55432 -U newcloud newcloud_test

$env:TEST_DATABASE_URL = "postgres://newcloud:...@127.0.0.1:55432/newcloud_test?sslmode=disable"
go test -p 1 ./... -count=1
```

**`-p 1` is required, not cosmetic.** Several audit tests reset the schema with
`DROP SCHEMA ... CASCADE`. `go test ./...` runs one process per package in parallel, so the store
package and the audit package will drop the schema under each other and both fail with errors that
look like product bugs. Run the PG suite serialized, or give each package its own database.

The DAV overwrite path is covered on real PostgreSQL by `TestCopyOverwriteOnPostgreSQL` (both
layouts) and `TestTheOverwriteIsAuditedOnPostgreSQL`, which assert the live bytes, the trash entry
and both audit rows in one committed transaction.

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

### 9.1 Backend support matrix

The client is minio-go, which speaks the S3 API rather than a specific vendor's extensions. That
makes most compatible stores work; it does **not** make them equivalent, and this table says which
ones have actually been exercised rather than implying they all have.

| Backend | Tested how | Integrity readback (`verify_after_put`) | Notes |
| --- | --- | --- | --- |
| In-process fake (`internal/storage/s3fake`) | Every run: `internal/blob/s3` and the layout conformance suite | Yes — simple and multipart paths | Default in `go test`. Models the operations this server performs and returns `NotImplemented` for anything else, so a backend change that starts using a new operation fails loudly instead of passing against a guess. |
| Local disk (`local`, `blob` layout) | Every run | Not applicable — writes are `rename(2)` and the blob name is its SHA-256 | No object store between the process and the bytes. |
| AWS S3 | Not run in this repository | Not verified | Enable `verify_after_put` and set `verify_max_bytes: 0` unless you have measured the egress cost. Multipart is used above 16 MiB. |
| MinIO | Not run in this repository | Not verified | The audience minio-go targets first; expected to work, unproven here. |
| Ceph RGW | Not run | Not verified | — |
| Cloudflare R2 | Not run | Not verified | Path-style addressing is used; R2 supports it. |
| Backblaze B2 / Wasabi | Not run | Not verified | — |

Run `TEST_S3_ENDPOINT`, `TEST_S3_BUCKET`, `TEST_S3_ACCESS_KEY`, `TEST_S3_SECRET_KEY` (and
`TEST_S3_SECURE=1`) against a disposable bucket to exercise the conformance suite, the multipart
path and the verification readback on a real provider. Without that run, "the S3 backend works" is a
claim about the fake and about minio-go's framing, not about your object store.

### 9.2 Recommended production profile for object storage

- **`verify_after_put: true`.** The default is off because the readback doubles egress. Turn it on
  wherever the store is not trusted to return what it acknowledged — which, for a self-hosted
  deployment on a smaller object store, is most of the time. WC-GAP-003 is only closed for a
  deployment that enables it.
- **`verify_max_bytes: 0`** unless the cost of re-reading a very large object has been measured and
  accepted in writing. Note that the largest uploads are exactly the ones that take the multipart
  retry path, so a size bypass removes the guard where it matters most.
- **A lifecycle rule that aborts incomplete multipart uploads** older than
  `upload_cleanup_grace_hours`, in addition to the built-in sweep: object stores bill for stranded
  parts indefinitely and no listing shows them.
- **`sse: sse-s3` or `sse-kms`** if the bucket policy does not already encrypt. Leaving `sse: ""`
  is a legitimate choice only when the bucket enforces encryption itself; an unknown value refuses
  to start rather than writing plaintext a deployment believes is encrypted.

### 9.3 Concurrent cleanup and the residual ownership window

When verification proves an object is corrupt, the blob is removed so that a later upload can
repair the address. That removal is **guarded**: the ETag the store returned from `PutObject` is
compared against the stored object immediately before the delete, and if another writer has replaced
the object in the meantime the delete is skipped and the address is left to the integrity scrub.

The residual window is the `StatObject` → `RemoveObject` pair. S3 offers no conditional DELETE
without object versioning, so a writer that replaces the object inside that window could still lose
its copy. Two things keep this narrow and non-silent:

- Content addressing means every correct writer sends the same bytes; the copies can differ only
  when one of them is corrupt, which is the case being cleaned up.
- The object was already proven not to match its address, so leaving it in place would serve
  corruption to every subsequent reader — the worse outcome.

Deployments that cannot accept even that window should enable bucket versioning: a delete then
creates a delete marker, the previous version remains recoverable, and the address can be repaired
by a re-upload. Record the choice in the deployment notes; `verify_after_put` and versioning are the
two settings that determine what this window costs.
