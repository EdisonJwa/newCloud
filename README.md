# newCloud

**A self-hosted file service for teams that need SSO, WebDAV, sharing, and an audit trail — without the PHP stack.**

One Go binary. One PostgreSQL database. One process.

[![CI](https://github.com/EdisonJwa/newCloud/actions/workflows/ci.yml/badge.svg)](https://github.com/EdisonJwa/newCloud/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL 17](https://img.shields.io/badge/PostgreSQL-17-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org)

newCloud is a small, opinionated replacement for the file-sharing core of Nextcloud: a
namespace per account, a WebDAV endpoint that desktop clients can rely on, link and email
sharing, versioning, a trash can, and a hash-chained audit log. It is built for operators who
would rather run one binary and one database than maintain a PHP application, a job runner, a
cache, and an app ecosystem.

It is not a Nextcloud replacement. There is no app store, no calendar, no chat, and no plugin
API. The scope is deliberately narrow: **store files, share them safely, and be able to prove
who did what.**

## Why newCloud

- **OIDC is the front door.** Pocket ID and any compliant OpenID Connect provider work. Roles come
  from IdP groups. WebDAV and scripts use scoped App Passwords, never browser cookies.
- **WebDAV that behaves like a filesystem.** RFC 4918 class 2, real locks enforced on every write
  path, `If-Match` / `If-None-Match` arbitration shared with the web UI, Range requests, recursive
  COPY, and correct `Depth` semantics.
- **Sharing with an exit.** Password-protected links, expiry, download caps, email shares with a
  single-use code, and drop-box links that accept uploads into a chosen folder.
- **An audit log that verifies offline.** Events are hash-chained. `newcloud audit verify` detects
  edits and mid-chain rewrites, and daily anchors make deletion of the newest events visible.
- **Storage that fits the deployment.** Content-addressed `blob` layout for deduplication, or a
  `tree` layout that mirrors the user-visible filesystem. Local disk or S3.
- **Honest failure modes.** Denials are `404`, not `403`. Audited writes fail closed. A write
  that cannot be audited does not happen.

## Features

| Area | Capability |
| --- | --- |
| **Authentication** | OIDC authorization code flow with PKCE, `state`, and `nonce`; group-to-role mapping; JIT provisioning (`open`, `pending`, `deny`) with a domain allowlist; break-glass local admin restricted by CIDR, with alerts on use |
| **Accounts and quotas** | Per-account namespace, per-account quota, admin user management, account disable/delete with an auditable tombstone |
| **Files** | Web UI with list and grid views, search, drag-and-drop upload, resumable chunked uploads, rename, bulk delete, trash, and version history |
| **WebDAV** | `PROPFIND`, `PROPPATCH`, `PUT`, `DELETE`, `MKCOL`, `COPY`, `MOVE`, `LOCK`, `UNLOCK`, Range, ETags, conditional writes, and lock enforcement shared with the JSON API |
| **Sharing** | Link shares with password, expiry, and download limits; email shares with one-time recipient codes; drop-box links; stable links across rename and move |
| **Previews** | Images, video, audio, PDF, text, and source code in the browser; a type allowlist plus content sniffing; SVG and HTML are never rendered inline; server-side thumbnails with pixel, concurrency, and decode-time limits |
| **Audit** | Hash-chained event log, external daily anchors, `audit verify`, CSV/JSON export, INSERT-only database role, fail-closed policy |
| **Storage** | Content-addressed `blob` layout with deduplication or filesystem-mirroring `tree` layout; local disk or S3-compatible object storage; orphan GC, version retention, and periodic integrity scrubbing |
| **Operations** | Single static binary, `/healthz`, `/readyz`, `/metrics`, 18 Prometheus alert rules, a complete runbook, backup scripts, an automated restore drill, and a `newcloud drill` subcommand |

## Architecture

```text
                    ┌─────────────────────────────────────────┐
   browser ────────▶│  newcloud (single Go process)           │
   WebDAV client ──▶│                                         │
   share visitor ──▶│  JSON API  ·  web UI  ·  WebDAV         │
                    │  public share pages  ·  OIDC callback   │
                    │                                         │
                    │  authz  ──▶  store  ──▶  audit          │
                    │                 │                       │
                    │                 └──▶ worker (GC, scrub) │
                    └────────┬────────────────────┬───────────┘
                             │                    │
                    ┌────────▼────────┐  ┌────────▼─────────────┐
                    │ PostgreSQL 17   │  │ blob/tree storage    │
                    │ metadata, audit │  │ local disk or S3     │
                    └─────────────────┘  └──────────────────────┘
```

The metadata tree, versions, quota accounting, shares, locks, sessions, jobs, and the audit chain
all live in PostgreSQL. File bytes live in the content store. That split is what makes `blob`
layout deduplicate and what lets `tree` layout be inspected with `ls` and backed up with
`rsync`.

## Quick start

Prerequisites: Go 1.26+ and Docker.

The fastest way to see the product is the self-contained demo. It starts PostgreSQL in Docker, a
bundled fake identity provider, a TLS proxy, and the real `newcloud` binary — no configuration
file to edit:

```sh
git clone https://github.com/EdisonJwa/newCloud.git
cd newCloud
go run ./tools/demo
# open https://localhost:8443 (self-signed certificate)
```

The demo is for evaluation only. It uses a fake IdP that accepts any login and a self-signed
certificate.

For the standard development loop:

```sh
docker run -d --name newcloud-pg \
  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=newcloud \
  -p 15432:5432 postgres:17

export DATABASE_URL="postgres://postgres:test@127.0.0.1:15432/newcloud?sslmode=disable"
cp deploy/config.example.yml config.yml   # set public_url, usercontent_url, auth.issuer,
                                          # storage.local.root, storage.upload_dir, storage.layout

go run ./cmd/newcloud migrate
NEWCLOUD_CONFIG=./config.yml go run ./cmd/newcloud serve
```

Without a real OIDC provider, run the development IdP in another terminal:

```sh
go run ./cmd/devidp -addr 127.0.0.1:19000 -client-id newcloud
```

`cmd/devidp` accepts every request. It must never be deployed.

See [docs/guides/quickstart.md](docs/guides/quickstart.md) for the full first-run walkthrough.

## Production deployment

The reference deployment is Docker Compose with Caddy for TLS:

```sh
cd deploy
cp config.example.yml /etc/newcloud/config.yml   # edit it first
export PG_PASSWORD="$(openssl rand -base64 32)"
export PG_APP_PASSWORD="$(openssl rand -base64 32)"

docker compose up -d postgres
docker compose run --rm \
  -e DATABASE_URL="postgres://newcloud:${PG_PASSWORD}@postgres:5432/newcloud?sslmode=disable" \
  web migrate
docker compose exec postgres psql -U newcloud -d newcloud \
  -c "ALTER ROLE newcloud_app PASSWORD '${PG_APP_PASSWORD}'"
docker compose up -d
```

The migration runs as the owner; the application then connects as `newcloud_app`, whose password is
deliberately absent from migrations.

Three details are load-bearing:

1. **`/var/lib/newcloud` must be a single mount point.** `blobs/` and `blobs/tmp/` must share a
   filesystem, or publishing a blob degrades from `rename(2)` to copying the whole file. Startup
   probes this with a real rename and refuses to start if the probe fails.
2. **`server.public_url` and `server.usercontent_url` must be different origins.** Session cookies
   live on the application origin; user content is served from a separate origin so a stored-XSS
   payload in an uploaded file cannot reach the session cookie or the app DOM.
3. **Choose `storage.layout` deliberately.** `blob` is the default, but the choice changes the shape
   of the data on disk and cannot be mixed per user. `blob` gives deduplication and cheap versions;
   `tree` gives a filesystem that `ls` and `rsync` can inspect.

The full deployment guide covers the application database role, TLS, backups, retention, and
capacity planning: [docs/guides/deployment.md](docs/guides/deployment.md).

## Documentation

| Start here | |
| --- | --- |
| [Documentation index](docs/README.md) | Every guide, reference page, and decision record |
| [Quick start](docs/guides/quickstart.md) | Local development and a first login |
| [Configuration](docs/guides/configuration.md) | Every configuration key, with defaults |
| [Deployment](docs/guides/deployment.md) | Production topology and first boot |
| [Operations](docs/guides/operations.md) | Health, metrics, alerts, backups, audit, key rotation |
| [Architecture](docs/reference/architecture.md) | How the process, metadata, storage, and audit fit together |
| [Limitations](docs/guides/limitations.md) | What is deliberately not implemented, and why |

Design decisions are recorded as ADRs in [docs/adr/](docs/adr/):

- [ADR-0001: Product scope and language choice](docs/adr/0001-scope-and-language.md)
- [ADR-0002: Backend skeleton, credentials, and WebDAV](docs/adr/0002-backend-skeleton.md)
- [ADR-0003: Optional storage layouts (blob and tree)](docs/adr/0003-storage-layout.md)
- [ADR-0004: Metadata store contract](docs/adr/0004-metadata-store.md)
- [ADR-0007: S3 storage backend](docs/adr/0007-s3-storage.md)
- [ADR-0008: Resumable chunked uploads](docs/adr/0008-resumable-uploads.md)
- [ADR-0012: Cursor pagination contract](docs/adr/0012-cursor-contract.md)

The evidence behind the claims in this README — the tests, the manual verification, and the
remaining gaps — is in [docs/reference/implementation-record.md](docs/reference/implementation-record.md).

## Design principles

These are product behavior, not implementation details. Changing one requires an ADR.

**Deny with `404`, never `403`.** Distinguishing "does not exist" from "not allowed" hands an
attacker a directory-enumeration oracle for free.

**Two credential systems, deliberately.** Browser sessions and App Passwords take different
paths. WebDAV never accepts a session cookie, because desktop clients cannot complete an OIDC
redirect. Browser sessions and short-lived share/email tickets are HMAC-signed; App Passwords and
share links are random values stored only as SHA-256 hashes. Only human-chosen secrets (share
passwords, the break-glass password) use Argon2id.

**A write and its audit record share one transaction.** Audit writes fail closed: if the event
cannot be recorded, the operation is refused rather than performed invisibly.

**Conditional writes are decided in the same SQL statement that changes the row.** Read-then-write
loses updates under concurrency. The web API and WebDAV cannot end up with different rules.

**Locks are enforced on the write path, not merely recorded by `LOCK`.** A lock that exists only
in the creation endpoint is worse than no lock, because clients believe they are protected.

**Storage layout is an operational choice.** `blob` deduplicates and makes versions nearly free,
but `ls` shows hashes. `tree` is directly inspectable and `rsync`-friendly, but every version is a
full copy and Windows reserved names apply. Both are verified by the same contract test suite.

**The code does not assume it is the only process.** Sessions, locks, and the job queue live in
PostgreSQL, using `FOR UPDATE SKIP LOCKED` and leases. Single-node deployment is a deployment
choice, not an architectural premise.

## Testing

```sh
go test ./...                                   # unit and integration tests
go test ./... -race                             # requires CGO and a C compiler
TEST_DATABASE_URL=... go test ./internal/store  # adds the PostgreSQL contract suite
make check                                      # gofmt, vet, example config, web checks, race
```

The same `internal/storetest` contract suite runs against both the in-memory and PostgreSQL
implementations. That is not ceremony: the two once disagreed on first login for a
pre-provisioned account, and only a shared suite exposed it.

Documentation is tested too. `TestDocumentsCiteOnlyRealTests` fails when a document cites a test
that does not exist, and `TestGapTableDoesNotClaimClosedGapsAreOpen` fails when a status row claims
something is missing after it has been implemented.

Two end-to-end tools need a running instance:

```sh
go run ./tools/e2e     -app http://127.0.0.1:18500 -idp http://127.0.0.1:19000
go run ./tools/uicheck -app http://127.0.0.1:18500 -public-host 127.0.0.1:18600
```

## Status and limitations

The core file-service contracts are implemented and covered by tests. This is not a claim that
every feature in the requirements document exists. Notable gaps:

- **No shared or group-owned spaces.** Resource ACLs grant users and groups access to a folder;
  team ownership of a space is not implemented.
- **`REPORT` and `sync-collection` are not implemented.** Clients receive an explicit `501`, not
  an empty result. Shared locks are also absent.
- **S3 is tested against an in-process fake and a moto server, not real object storage.** The
  backend logic is covered, including multipart behavior and fault injection, but interoperability
  with AWS S3, MinIO, Ceph, and R2 has not been exercised against those services.
- **No IdP event source.** A role or group change takes effect at the account's next login. There
  is no SCIM or push-based deprovisioning.
- **Previews decode in-process.** Pixel, concurrency, and time limits bound the work; true
  isolation would require a subprocess.
- **Resumable upload sessions do not survive a restart.** Content integrity is unaffected; the
  upload must be restarted.
- **Restore timing is measured to a 5.5 MiB metadata dump.** The measurements are linear within
  that range. There is no validated RTO for a 40 TiB deployment; a linear extrapolation produces
  an unusable number, which is itself the finding.
- **No bundled Grafana dashboards.** Prometheus rules and runbooks are included; dashboards depend
  on the operator's Grafana version and are intentionally not shipped.
- **No import from Nextcloud or other file services.**
- **No `blob`/`tree` migration tool.** The procedure is designed, not implemented; choose the layout
  before the first upload.

The complete, dated list with evidence is in
[docs/guides/limitations.md](docs/guides/limitations.md) and
[docs/reference/implementation-record.md](docs/reference/implementation-record.md).

## Contributing

Issues and pull requests are welcome. Before opening a pull request:

1. Run `gofmt` and `go test ./...`.
2. Behavior changes require an update to the matching ADR and the tests that pin it.
3. A new document or claim keeps its evidence link next to it. Documents that cite tests are
   checked by the test suite.

The project favors small, reviewable changes with a stated failure mode over broad rewrites. A
correctness fix should come with the test that would have caught the bug.

## Security

Do not open a public issue for a vulnerability. Use GitHub's private vulnerability reporting for
this repository.

Two properties are worth knowing before deploying:

- The application database role (`newcloud_app`) can `SELECT` and `INSERT` the audit table but
  cannot `UPDATE`, `DELETE`, or `TRUNCATE` it. The role's password is bootstrapped manually and is
  deliberately absent from migrations.
- Audit anchors must be stored where the application cannot rewrite them. The code can detect a
  truncated chain only if the anchor directory has different credentials from the database; a
  directory on the metadata volume makes the anchor decorative.
