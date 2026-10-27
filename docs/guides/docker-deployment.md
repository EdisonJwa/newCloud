# Docker deployment runbook

This is the step-by-step path from a fresh Linux host to a working newCloud behind your own reverse
proxy. It is deliberately command-by-command: every step can be checked before the next one runs.

[Deployment](deployment.md) is the reference document — it explains *why* the architecture is shaped
this way (origin split, disk layout, audit-role split, backup order). Read it once. This file is the
order of operations, and it repeats the handful of facts that break a deployment when skipped.

**What you provide.** A Linux host with Docker and the Compose plugin, two DNS names, a TLS
certificate (or Caddy's automatic one), and an OIDC identity provider. You own the reverse proxy;
the shipped `deploy/Caddyfile` is the reference implementation of what that proxy must do, and
section 7 explains how to translate it to nginx or Apache.

---

## 0. Before you start

| Requirement | Why it matters |
| --- | --- |
| Two hostnames, e.g. `cloud.example.com` and `usercontent.example.com` | The split is a security boundary, not branding: a stored-XSS payload in an uploaded file must not read the session cookie. Validation **rejects** a shared origin and a shared hostname. |
| A PostgreSQL 17 database (the compose file provides one) | Migrations need the owner role; requests use a different, non-superuser role. |
| An OIDC provider with a client for this deployment | Roles come from group claims. A client secret file is safer than a value in YAML. |
| `/var/lib/newcloud` on **one** filesystem | Publishing a blob is `rename(2)`. Across filesystems it becomes a full-file copy — at scale that is an outage, not a slowdown. Startup probes this and refuses to boot on failure. |
| PostgreSQL data on a **different disk** than blobs | A restic snapshot of content should not fight the database for IO. |
| A WAL archive path on a third disk, ideally off-host | This is what makes the stated RPO true. |

Install Docker and the Compose plugin, then confirm:

```sh
docker version --format '{{.Server.Version}}'
docker compose version
```

---

## 1. Get the source and create the directories

```sh
git clone https://github.com/EdisonJwa/newCloud.git
cd newCloud

sudo install -d -m 0750 /etc/newcloud
sudo install -d -m 0700 /var/lib/newcloud /var/lib/newcloud-pg
sudo install -d -m 0700 /mnt/wal-archive
sudo chown -R 65532:65532 /var/lib/newcloud   # the distroless nonroot uid
```

`65532` is `nonroot` in `gcr.io/distroless/static-debian12:nonroot`. This step is not optional
housekeeping: the image runs as a non-root user, and a content root it cannot write makes the
startup self-check fail with

```
newcloud: startup self-check failed: blob/local: create root: mkdir /var/lib/newcloud: permission denied
```

and the container exits. `migrate` hits the same permission on the blob-size backfill and only
*warns*, so a deployment can look successful and then fail to start. Verify ownership before
starting the server:

```sh
ls -ld /var/lib/newcloud          # expect 65532 65532
```

---

## 2. Configuration

```sh
sudo cp deploy/config.example.yml /etc/newcloud/config.yml
sudo chmod 0640 /etc/newcloud/config.yml
sudo $EDITOR /etc/newcloud/config.yml
```

Set at minimum:

```yaml
server:
  addr: ":8080"
  usercontent_addr: ":8081"                 # REQUIRED for the origin split; see 7
  public_url: "https://cloud.example.com"
  usercontent_url: "https://usercontent.example.com"
  # Trust the proxy, not the clients. Without this every request appears to come
  # from the proxy's IP and all per-IP throttling collapses into one bucket.
  trusted_proxies: ["172.16.0.0/12"]        # the proxy's network, not the internet

auth:
  issuer: "https://id.example.com"
  client_id: "newcloud"
  client_secret_file: "/run/secrets/oidc_client_secret"
  session_secret: "<openssl rand -base64 48>"
  group_role_map:
    "cloud-admins": admin
    "cloud-users": user

storage:
  layout: "blob"                            # choose deliberately; no migration tool exists
  local:
    root: "/var/lib/newcloud/blobs"
  upload_dir: "/var/lib/newcloud/uploads"

audit:
  anchor_dir: "/var/lib/newcloud/audit-anchor"
```

**On `trusted_proxies`:** list the network your proxy container is on (Docker's default bridge
networks fall inside `172.16.0.0/12`; a compose network is usually `172.18.0.0/16`). It is the
address of the proxy, never the clients'. The header is ignored entirely when this is empty, which
is correct only when nothing sits in front of the app.

The process refuses to boot on an invalid config. To see the validation error on the terminal
rather than in a container log, run the server once in the foreground after building the image
(section 4):

```sh
docker compose run --rm web serve     # exits non-zero with the reason if the config is invalid
```

`newcloud --version` prints the stamped build version; it does not read the config, so it is not a
substitute for the check above.

---

## 3. Secrets

Two database passwords that **must differ**, plus the OIDC secret:

```sh
umask 077
export PG_PASSWORD="$(openssl rand -base64 32)"
export PG_APP_PASSWORD="$(openssl rand -base64 32)"

cat > /etc/newcloud/.env <<EOF
PG_PASSWORD=${PG_PASSWORD}
PG_APP_PASSWORD=${PG_APP_PASSWORD}
NEWCLOUD_VERSION=$(git describe --tags --always)
EOF
chmod 0600 /etc/newcloud/.env
```

`docker compose` reads `.env` from its working directory, so put it in `deploy/` beside the compose
file (or export the two variables in the shell that runs compose). It must never be committed.

Place the OIDC client secret where the config points:

```sh
printf '%s' '<client-secret-from-your-idp>' | sudo tee /run/secrets/oidc_client_secret >/dev/null
sudo chmod 0400 /run/secrets/oidc_client_secret
```

**Why the two database roles differ.** The application connects as `newcloud_app`, which has
`SELECT`/`INSERT` on `audit.events` but no `UPDATE`, `DELETE` or `TRUNCATE`. That is what makes the
audit log append-only as a property of the database rather than a convention in the code. Sharing
one password gives that up.

---

## 4. Build and start PostgreSQL

```sh
cd deploy
docker compose build web
docker compose up -d postgres
docker compose ps                    # wait for "healthy"
```

The image build runs `go vet` and the full test suite **inside** the build, so a failing test stops
the image from being produced. Expect the first build to take several minutes. Note that the
in-image test run does not set `TEST_DATABASE_URL`, so the PostgreSQL suites are skipped there —
run them yourself (section 9) if you need that evidence for a release.

---

## 5. Apply the schema, then give the application role its password

```sh
docker compose run --rm \
  -e DATABASE_URL="postgres://newcloud:${PG_PASSWORD}@postgres:5432/newcloud?sslmode=disable" \
  web migrate

docker compose exec postgres psql -U newcloud -d newcloud \
  -c "ALTER ROLE newcloud_app PASSWORD '${PG_APP_PASSWORD}'"
```

The migration that creates `newcloud_app` deliberately leaves it passwordless: a password in a
migration file would be in version control forever and shared by every environment.

Confirm it worked:

```sh
docker compose exec postgres psql -U newcloud -d newcloud -c "\dt" | wc -l
docker compose exec postgres psql -U newcloud -d newcloud -tAc \
  "select has_table_privilege('newcloud_app','audit.events','UPDATE')"   # expect: f
```

---

## 6. Start the application and check readiness

```sh
docker compose up -d web
docker compose ps

curl -fsS http://127.0.0.1:8080/healthz && echo OK
curl -fsS http://127.0.0.1:8080/readyz  && echo READY
```

- `/healthz` answers when the process is alive.
- `/readyz` also probes the metadata store and the content store. Healthz-but-not-readyz is almost
  always the database.

Both listeners are published on **loopback only** by the compose file. That is intentional: session
cookies live on the application origin, so nothing but the proxy may reach it.

Then sign in through your IdP with an account whose group maps to `admin`. If you have none yet,
either put a test identity in that group, or enable break-glass temporarily with a narrow CIDR.
Break-glass has no web UI; its endpoint is `POST /api/auth/breakglass`, it is audited, and its use
raises a critical alert.

---

## 7. Point your reverse proxy at it

Your proxy must do six things. The shipped `deploy/Caddyfile` is the reference and comments the
reasoning for each rule; translate these, do not assume your defaults match.

1. **Terminate TLS** for *both* hostnames and redirect 80 → 443.
2. **Route by hostname.** `cloud.example.com` → `127.0.0.1:8080`; `usercontent.example.com` →
   `127.0.0.1:8081`. Mixing these up is the most common deployment failure.
3. **Refuse `/content/*` on the application vhost.** Return 404. Do *not* block
   `/s/{token}/download` or `/s/{token}/thumbnail` there — those must reach the application, which
   runs the share's gate and then redirects to the content origin with a short-lived capability.
4. **Keep `/metrics` and `/readyz` off the public vhost.** They are unauthenticated by design
   (`/readyz` echoes dependency errors, `/metrics` publishes operational detail). Scrape them from
   the host or an internal network.
5. **Strip the `Cookie` header** on requests to the content vhost.
6. **Preserve the `Host` header** on both vhosts. The app builds absolute URLs and the content
   router keys on host.

Caddy (as shipped) needs nothing beyond editing the two hostnames. For nginx:

```nginx
server {
    listen 443 ssl http2;
    server_name cloud.example.com;

    ssl_certificate     /etc/ssl/cloud.example.com/fullchain.pem;
    ssl_certificate_key /etc/ssl/cloud.example.com/privkey.pem;

    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
    add_header X-Content-Type-Options nosniff always;
    add_header X-Frame-Options DENY always;
    add_header Referrer-Policy no-referrer always;

    # 3. content paths belong to the other origin
    location ^~ /content/ { return 404; }
    # 4. operational endpoints are not public
    location = /metrics { return 404; }
    location = /readyz  { return 404; }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;                    # 6. not $proxy_host
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        client_max_body_size 0;                         # uploads are not proxy-limited
        proxy_request_buffering off;                    # stream, do not spool to disk
        proxy_read_timeout 3600s;
    }
}

server {
    listen 443 ssl http2;
    server_name usercontent.example.com;

    ssl_certificate     /etc/ssl/usercontent.example.com/fullchain.pem;
    ssl_certificate_key /etc/ssl/usercontent.example.com/privkey.pem;

    add_header X-Frame-Options DENY always;
    add_header Referrer-Policy no-referrer always;
    add_header X-Robots-Tag "noindex, nofollow, noarchive" always;

    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_set_header Host $host;
        proxy_set_header Cookie "";                     # 5. strip on the way upstream
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        client_max_body_size 0;
    }
}
```

For Apache, the equivalents are `ProxyPreserveHost On`, `RequestHeader unset Cookie` on the content
vhost, and `Require all denied` on `/content/`, `/metrics` and `/readyz`.

**Then set `trusted_proxies`** (section 2) to the address the app sees as the peer. If your proxy
and the container share a Docker network, that is the bridge gateway or the proxy container's
address. Verify it took effect: sign in and check that the audit log records your real client
address, not the proxy's.

---

## 7a. Sign the audit anchors (recommended)

`audit.anchor_dir` publishes a daily checkpoint of the audit chain head. A hash chain proves
internal consistency, not completeness: someone with database access can delete the newest rows and
the rest still verifies. The anchor closes that — but only if it is written somewhere the
application cannot rewrite, with credentials different from the database.

```sh
# once, on the host; keep the key OUTSIDE the anchor directory
docker compose run --rm web audit anchor-keygen --out /etc/newcloud/audit-anchor.key
sudo chmod 0400 /etc/newcloud/audit-anchor.key
```

Then set both keys in `config.yml` and restart:

```yaml
audit:
  anchor_dir: "/var/lib/audit-anchor"        # a WORM bucket or append-only volume
  anchor_signing_key_file: "/etc/newcloud/audit-anchor.key"
```

Turning `anchor_dir` on changes the hardening story from "the chain is internally consistent" to
"deleting the newest events is detectable". Left empty, startup logs a warning saying anchoring is
off.

---

## 8. Backups

Two artefacts, in this order:

1. `pg_dump` — metadata and the audit chain;
2. the content root — the blob store.

Taking them in the other order can record a blob whose metadata row was not yet dumped. Follow
[Deployment §6](deployment.md#6-backups) for the exact commands and the restore drill. A backup that
has never been restored is a hypothesis; `newcloud drill` exists to test the metadata side, and the
guide's restore procedure tests the rest.

At minimum, schedule:

```sh
# nightly metadata
docker compose exec -T postgres pg_dump -U newcloud newcloud | gzip > /backup/newcloud-$(date +%F).sql.gz
# content, with WAL archiving already running for point-in-time recovery
restic backup /var/lib/newcloud
```

---

## 9. Verification checklist

Run these before you call the deployment done. Each one has failed in a real deployment at least
once, either here or in the project's own review history.

```sh
# the process is healthy and its dependencies are reachable
curl -fsS https://cloud.example.com/healthz
curl -fsS https://cloud.example.com/readyz

# the origin split is enforced by the process, not only by the proxy
curl -s -o /dev/null -w '%{http_code}\n' https://cloud.example.com/content/  # 404
curl -s -o /dev/null -w '%{http_code}\n' https://usercontent.example.com/    # 404

# operational endpoints are not public.
#
# These two are the check that proves your proxy rules work: the application listener
# itself serves BOTH with 200. If you see 200 here, your proxy is forwarding them and
# /metrics (request volumes, upload failure rates, break-glass attempts, WAL state,
# scrub corruption counts) is on the internet.
curl -s -o /dev/null -w '%{http_code}\n' https://cloud.example.com/metrics   # 404
curl -s -o /dev/null -w '%{http_code}\n' https://cloud.example.com/readyz    # 404

# and confirm what the app listener really does, from the host:
curl -s -o /dev/null -w 'app /content/: %{http_code}\n' http://127.0.0.1:8080/content/x  # 404
curl -s -o /dev/null -w 'content root:  %{http_code}\n' http://127.0.0.1:8081/           # 404

# the app never connects as the owner
docker compose exec postgres psql -U newcloud -d newcloud -tAc \
  "select usename from pg_stat_activity where datname='newcloud'" | sort -u  # newcloud_app only

# the audit log is append-only at the database level
docker compose exec postgres psql -U newcloud -d newcloud -tAc \
  "select has_table_privilege('newcloud_app','audit.events','UPDATE')"       # f
```

Then, in the UI: upload a file, download it, create a link share, open the share in a private
window, and delete a file and restore it from the trash. A share that downloads is the single best
end-to-end check, because it crosses both origins.

### Prove the audit log is tamper-evident, in this environment

The chain is verified by `newcloud audit verify`, which also compares it against the anchor
checkpoints. Do this once on the real host, because it is the only way to know the property holds
where you run it — and it takes a throwaway database, not this one:

```sh
# 1. a disposable database for the experiment
docker run --rm -d --name audit-proof -e POSTGRES_PASSWORD=x -e POSTGRES_DB=nc \
  -p 5550:5432 postgres:17
export PROOF_DB="postgres://postgres:x@127.0.0.1:5550/nc?sslmode=disable"
DATABASE_URL="$PROOF_DB" newcloud migrate

# 2. append real events, publish an anchor, then delete the newest ones
go run ./tools/auditsmoke  -database "$PROOF_DB" -events 25
go run ./tools/anchorsmoke -database "$PROOF_DB" -dir /tmp/anchors -sign -truncate 5

# 3. the check must now FAIL, naming the truncation
DATABASE_URL="$PROOF_DB" newcloud audit verify -anchors /tmp/anchors -pubkey <printed above>
# audit verify: 1 of 1 checkpoints disagree with the chain: events have been removed …

docker rm -f audit-proof
```

A word about what this proves: without the `-anchors` comparison the same truncated log reports
`audit chain verified` and exits `0`, because a shortened hash chain is internally consistent.
That is not a defect in the chain — it is why anchors exist, and why the check compares them.

On the live deployment, set `audit.anchor_dir` and `audit.anchor_signing_key_file` (section 7a).
The first checkpoint appears after the first completed UTC day; until then `audit verify` warns
that it compared nothing, which is the honest answer.

Optional but recommended for a release:

```sh
# PostgreSQL-backed suites, skipped by a plain `go test`. -p 1 is required: the suites share one
# database and reset its schema, so parallel packages drop each other's tables.
TEST_DATABASE_URL="postgres://newcloud:${PG_PASSWORD}@127.0.0.1:5432/newcloud?sslmode=disable" \
  go test -race -count=1 -p 1 ./...
```

Point that at a disposable database, never at one whose contents matter.

---

## 10. Upgrades

```sh
git pull
cd deploy
docker compose build web
docker compose run --rm \
  -e DATABASE_URL="postgres://newcloud:${PG_PASSWORD}@postgres:5432/newcloud?sslmode=disable" \
  web migrate
docker compose up -d web
curl -fsS https://cloud.example.com/readyz
```

Migrations are additive. A binary rollback is safe only if the previous version does not need to
write the new columns; when in doubt, restore the backup rather than downgrade in place. The
compose file sets `stop_grace_period: 45s` because the server drains for 30 — without it every
`up -d` SIGKILLs in-flight uploads and skips the worker's lease release.

---

## 11. Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| Container restarts, log says the rename probe failed | `/var/lib/newcloud` spans filesystems | Make it one mount point |
| `readyz` fails, `healthz` passes | Database unreachable or migrations not applied | Check `DATABASE_URL`, re-run `migrate` |
| Every request logs the proxy's IP | `trusted_proxies` unset or does not include the proxy | Set it to the proxy's CIDR and restart |
| Share downloads 404, direct download works | Proxy blocks `/s/*/download` on the app vhost, or routes `/content/*` to the wrong port | Fix rule 3 and rule 2 |
| `413 Request Entity Too Large` | Proxy body limit | `client_max_body_size 0` (nginx), `LimitRequestBody 0` (Apache) |
| Login redirect lands on the wrong host | `Host` header rewritten | Preserve the incoming `Host` |
| Uploads fail with permission denied, or the container exits with `create root: ... permission denied` | Bind mount not owned by uid 65532 (the image is non-root) | `chown -R 65532:65532 /var/lib/newcloud` |
| `/metrics` and `/readyz` answer 200 on the public hostname | The proxy is forwarding them; the app serves both unauthenticated | Add the 404 rules from section 7 — this one publishes operational detail |
| First sign-in creates an admin nobody expected | `auth.jit.mode: open` (the default) | Use `pending` or `deny` and pre-create accounts |

---

## What this deployment does not give you

These are documented properties, not bugs, and they matter when planning:

- **Single node.** Resumable uploads resume after a restart but do not follow a client to another
  node; there is no HA story and the project does not claim one.
- **S3 is tested against a fake and a moto server, never real object storage.** If you select
  `storage.backend: s3`, exercise your endpoint before trusting it.
- **No immediate role revocation.** Group changes take effect within `auth.revalidate_minutes`,
  not instantly. There is no SCIM or push deprovisioning.
- **Restore timing is validated only to a 5.5 MiB metadata dump.** Do a restore rehearsal at your
  real size before promising yourself an RTO.
- **Previews decode in a child process, not an OS sandbox** (no seccomp or cgroups).

The complete dated list is in [limitations.md](limitations.md).
