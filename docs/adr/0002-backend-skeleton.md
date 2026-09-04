# ADR-0002: Backend skeleton, credentials, and WebDAV

**Status:** Accepted · **Date:** 2026-10-21

This ADR fixes the implementation baseline for the M0 and M1 milestones and follows the upstream decision in [ADR-0001](../adr/0001-scope-and-language.md): it decides how to build, not whether to build. The stack is Go 1.26 with `chi` routing, `pgx` with hand-written SQL, and `x/net/webdav` only for its XML types, with storage and locking implemented directly against PostgreSQL; browser sessions and short-lived share tickets are HMAC-signed, while App Passwords and share links are random values stored only as SHA-256 hashes; the application is stateless, so a single host and multiple replicas run the same code; and the React 19 + TypeScript + Vite client is embedded as static assets. Three corrections merit separate mention because they steer away from an easy mistake: an App Password is a random token verified by a fast hash rather than Argon2, which would run Argon2 on every WebDAV request (§4); `Authorize()` is the single entry point and returns a capability bitmap, so callers never see a node's owner field (§5); and WebDAV does not use the `x/net/webdav.FileSystem` abstraction, whose API cannot express quotas, versions, or lock persistence — only its XML types and parts of the handler dispatch are reused, and PROPFIND is written locally because large directories must stream (§6).

---

> **Implementation note (2026-10-22).** The load-bearing decisions in this ADR are implemented:
> `chi`, `pgx`, `x/net/webdav` only for XML types, HMAC session tickets, a single `Authorize()`
> entry point, PostgreSQL-backed locks, a stateless process, and an embedded web client. Several
> supporting choices changed during implementation: hand-written SQL instead of `sqlc`, embedded
> SQL migrations instead of `goose`, `net/smtp` instead of a mail framework, React + TypeScript +
> Vite instead of SvelteKit, and `TEST_DATABASE_URL` instead of testcontainers. Where this ADR and
> the code differ, [Architecture](../reference/architecture.md) and the code are authoritative; the
> decisions below stand.

## 1. Version and dependency baseline

| Item | Choice | Rationale |
| --- | --- | --- |
| Go | **1.26+** (`go.mod` declares `go 1.26`) | `os.Root`'s write methods (`MkdirAll`, `Rename`, `RemoveAll`) are what give the tree layout syscall-level containment; they require Go 1.25 or newer. The Docker image and CI pin 1.26 |
| Routing | `chi/v5` | Simple middleware model, no reflection; plain `net/http` also works. **No gin or echo** (heavy dependencies, error-prone middleware order) |
| DB driver | `jackc/pgx/v5` + `pgxpool` | The only actively maintained PostgreSQL driver |
| Queries | Hand-written SQL in `internal/store` | No code generator. The in-memory and PostgreSQL implementations pass the same contract suite, which is stronger than generated-type agreement |
| Migrations | Embedded SQL in `migrations/`, applied by `newcloud migrate` | No external migration library; the binary carries its own schema |
| OIDC | `coreos/go-oidc/v3` + `golang.org/x/oauth2` | The de facto standard; Pocket ID is standard OIDC and needs no customization |
| Password hashing | `argon2id` (`golang.org/x/crypto/argon2`) | Used only for the break-glass password and share passwords |
| Randomness | `crypto/rand` | All tokens |
| Logging | `log/slog` (structured JSON) | Standard library, no dependency |
| Configuration | `gopkg.in/yaml.v3` plus a small environment overlay | Few configuration keys, so no viper |
| Email | `net/smtp` inside `internal/mail` | Share notifications and one-time codes; no third-party mail framework |
| Frontend | React 19 + TypeScript + Vite, with types generated from `web/openapi.yaml` | One build step, committed output embedded with `go:embed`; `go build` still produces a single binary |
| Testing | Standard `testing` plus real PostgreSQL via `TEST_DATABASE_URL` | The in-memory store is a second implementation, not a mock; both run the same contract suite |

**Minimal dependency principle:** pin every version above, commit `go.sum` after `go mod tidy`, and run `govulncheck` in CI.

---

## 2. Repository structure

```text
newcloud/
├── cmd/
│   ├── newcloud/                 # serve, migrate, healthcheck, audit verify, breakglass hash, drill
│   └── devidp/                   # development-only fake OIDC provider
├── internal/
│   ├── config/                   # configuration loading, validation, defaults
│   ├── auth/                     # OIDC, sessions, App Passwords, break-glass
│   ├── authz/                    # the only authorization entry point
│   ├── store/                    # metadata: PostgreSQL and in-memory implementations
│   ├── storetest/                # the shared contract suite both implementations run
│   ├── storage/                  # ContentStore interface, blob/tree layouts, layouttest
│   ├── blob/                     # local and S3 content-addressed stores
│   ├── dav/                      # WebDAV handlers, locks, conditional requests
│   ├── server/                   # JSON API, share pages, content origin, metrics wiring
│   ├── share/                    # sharing, recipient codes, download accounting
│   ├── audit/                    # event definitions, hash chain, append
│   ├── auditverify/              # standalone chain verification
│   ├── upload/                   # resumable upload sessions
│   ├── thumbnail/                # bounded image decoding
│   ├── worker/                   # retention, GC, scrub, job consumer
│   ├── metrics/                  # Prometheus registry
│   ├── password/                 # Argon2id helpers
│   ├── ticket/                   # HMAC ticket formats
│   ├── mail/                     # SMTP transport
│   └── webassets/                # embedded React build (dist/)
├── web/                          # React + TypeScript + Vite source
├── migrations/                   # *.sql + embed.go
├── deploy/
│   ├── docker-compose.yml
│   ├── Caddyfile
│   ├── Dockerfile
│   └── backup/                   # backup scripts (pg_dump + restic)
├── tools/                        # e2e, uicheck, demo, demoshot, seedscale
└── docs/
```

**Layering rules (guarded by `go vet` and custom lint):** `api` and `dav` → `authz` → `store` and `blob`. **`dav` must not call `store` directly**; `store` returns authorized data only after `authz` has validated the request, and `store` must not import `authz` (to avoid an import cycle).

---

## 3. Configuration and startup self-check

Configuration precedence: **environment variables > configuration file > defaults**. All configuration is validated at startup and the process exits on failure (never "starts up but behaves wrongly").

```yaml
server:
  addr: ":8080"
  public_url: "https://cloud.example.com"        # used to build share links
  usercontent_url: "https://usercontent.example.com"  # ★ must be a different origin than public_url
  tls: { mode: "proxy" }                          # proxy = TLS terminated by Caddy

auth:
  issuer: "https://id.example.com"
  client_id: "..."
  client_secret_file: "/run/secrets/oidc_client_secret"
  scopes: [openid, profile, email, groups]        # ★ groups must be requested explicitly
  group_role_map:
    "cloud-admins": admin
    "cloud-users": user
  session_ttl: "12h"
  allow_local_admin: false                        # ★ break-glass emergency switch (effective from configuration alone)
  local_admin:
    username: "breakglass"
    password_argon2_file: "/etc/newcloud/bg.pass"
    allowed_cidrs: ["10.0.0.0/8"]

storage:
  backend: "local"                                # local | s3
  local: { root: "/var/lib/newcloud/blobs" }
  s3: { endpoint: "...", bucket: "...", region: "..." }

limits:
  max_file_bytes: 1073741824 * 1024               # 1 TiB hard cap
  default_file_quota_bytes: 10737418240           # 10 GiB soft cap
  max_propfind_children: 100000                   # ★ large-directory protection
  upload_chunk_bytes: 268435456                   # 256 MiB
```

**Startup checks (a failure refuses startup; this is the first gate against "starts up but behaves wrongly"):**

1. `blobs/tmp` and `blobs/` must share the same `st_dev` (otherwise rename degrades to copy) — **the implementation of the §8.7 risk item**.
2. `public_url` and `usercontent_url` must differ in scheme, host, and port (otherwise content-domain isolation fails).
3. The database connects to the audit schema with an INSERT-only role; `SELECT` and `UPDATE` use a different role (test each once at startup, and exit if the privileges are wrong).
4. Storage backend reachability, plus writing and then deleting a probe blob.
5. When `allow_local_admin=true`, `allowed_cidrs` must not be empty (an empty list means fully open), and startup is refused.

---

## 4. Credential system (★ the most important correction in this ADR)

### 4.1 Problem

An earlier design stored only an Argon2id hash of each App Password. **That does not work under WebDAV:**

- Windows Explorer and rclone send Basic authentication on **every request**;
- a single Argon2id verification costs ~50–100 ms at secure parameters, so expanding a directory with 200 PROPFIND subrequests costs 20 seconds;
- weakening the parameters (for example to 10 ms) makes the hash itself brute-forceable offline.

**Conclusion: a long-lived credential cannot take the form of a slow hash on every verification.**

### 4.2 Approach: session tickets (HMAC tickets), not "several keys"

Keep exactly **one** master key, `session_secret` (32 bytes), and derive every ticket purpose from it through **domain separation**:

```
token = base64url( payload || HMAC-SHA256(derive(key, purpose), payload) )
derive(key, purpose) = HMAC-SHA256(key, "newcloud/v1/" + purpose)
```

| purpose | Carrier | TTL | Storage | Verification cost |
| --- | --- | --- | --- | --- |
| `session` | `__Host-wc_session` cookie | 12h (sliding) | stateless, plus a `jti` row in the database (for forced logout) | one HMAC |
| `apppassword` | Basic username and password | 90 days (configurable) | the database stores **SHA-256(secret)** plus a plaintext prefix (the first characters of `wc_...`, so users can recognize the credential) | one SHA-256 + one index lookup |
| `share_access` | cookie (after password verification on the share page) | 1h | stateless | one HMAC |
| `share_token` | URL path `/s/<token>` | set by the share | the database stores only **SHA-256(token)** | one SHA-256 + index lookup |
| `download_token` | one-time URL parameter | 5min | stateless | one HMAC |
| `email_verify` | link in an email | 24h | stateless | one HMAC |

**Why an App Password can use SHA-256 instead of Argon2:** an App Password is **256 bits of randomness**, not a weak human-chosen password. A random token carries no dictionary-attack risk; offline brute force over 2^256 is infeasible, so a slow hash is unnecessary. **Only human-chosen passwords (the break-glass password and share passwords) use Argon2id.**

**This distinction is the key one**, and it is a common trap in self-built systems: assuming that every secret needs Argon2, then finding that performance collapses and quietly weakening the parameters.

### 4.3 Complete App Password flow

```
1. A user clicks "add App Password" in the web app and chooses a name, scope (ro|rw), and expiry.
2. The server generates secret = rand(32 bytes) and encodes it as wc_<base64url>.
3. DB: app_passwords(id, user_id, label, prefix='wc_ab12', token_hash=sha256(secret),
                   scopes, expires_at, last_used_at, revoked_at)
4. The plaintext is returned to the user exactly once (page display plus a copy button).
5. WebDAV Basic authentication: take the password -> sha256 -> look it up in the token_hash
   index (unique index, O(1)) -> check expires_at/revoked_at/scope -> construct the actor.
6. Update last_used_at asynchronously (without blocking the request: outbox, or a batch every 5 minutes).
```

**Note:** an App Password is a **high-value credential** whose leak is equivalent to the account. Therefore:

- Creation requires an authenticated browser session. A separate step-up re-authentication (a fresh passkey assertion) was proposed but is **not implemented**;
- The list page shows the `prefix`, the last-used time, and the IP, and the user can revoke any entry;
- Creation and revocation both go to audit.

### 4.4 Sessions and forced logout

- A session is an HMAC ticket plus a `sessions(jti, user_id, expires_at, revoked_at)` row. The ticket is the capability; the row is what makes individual revocation possible.
- Validation order: verify the HMAC, then ask the database whether `jti` is live. A revoked, expired, or unknown session is the same error to the caller.
- **There is no in-process revocation cache.** Forced logout takes effect on the next request because liveness is a database read. The proposed five-second negative cache was not adopted: it would make revocation eventually consistent for a saving of one indexed query, and the metadata store is already queried on the same request.
- The worker does not validate sessions; it only reads the database. Session handling is therefore independent of the number of workers or replicas.

### 4.5 break-glass

- A separate endpoint, `POST /api/auth/breakglass` (**not** a fallback button on the OIDC page, to avoid scanning). There is deliberately no web UI;
- Usable only when `auth.allow_local_admin=true` **and** the source IP is inside `allowed_cidrs`;
- Argon2id verification of the password hash;
- Every attempt emits an audit event, and the outcome is counted in `newcloud_auth_events_total{kind}`: successful use raises a critical alert, failed attempts raise a warning only after five failures in fifteen minutes;
- The resulting session uses the normal session TTL. The proposed mandatory TOTP and synchronous email to administrators were **not implemented**; the alert is the notification path.

### 4.6 SCIM

**Not implemented.** The `scim_token` configuration key and the `scim_pending_changes` table exist, but no `/scim/v2` endpoint is served. Provisioning is OIDC JIT or administrator action.

The original design was:

- Endpoints `/scim/v2/Users` and `/scim/v2/Groups` (GET, POST, PATCH, DELETE plus a minimal `filter` subset);
- Authentication: `Authorization: Bearer <scim_token>` (a separate token, not a session ticket, with its own configuration key);
- Idempotency: use `externalId` (the Pocket ID user id) as the upsert key;
- **Write operations first land in the `scim_pending_changes` table as a diff preview**; a change above a threshold (for example more than 20 deletions in one call) is rejected with an alert, which prevents a misconfigured bulk deactivation (risk table item).

---

## 5. Authorization (`authz`)

### 5.1 Single entry point

```go
package authz

// Actor is constructed by the authentication layer and cannot be forged externally (fields are unexported).
type Actor struct {
    kind   ActorKind // user | anonymous | system | breakglass
    userID uuid.UUID
    email  string
    role   Role      // admin | user | guest
    scopes ScopeSet  // ro/rw restriction from the App Password
}

type Perm uint16

const (
    PermRead Perm = 1 << iota
    PermWrite
    PermDelete
    PermShare
    PermListChildren
    PermReadVersions
    PermRestoreVersion
)

// Authorize is the only authorization decision function. The returned Cap contains only allowed capabilities.
// Callers cannot obtain node.OwnerID, so they cannot bypass authorization incidentally.
func (s *Service) Authorize(ctx context.Context, a Actor, nodeID uuid.UUID) (Cap, error)

type Cap struct {
    perm   Perm
    nodeID uuid.UUID
}
func (c Cap) Can(p Perm) bool { return c.perm&p != 0 }
```

The shipped permission set is finer-grained than this sketch: it adds `PermUploadOnly`, `PermDownload`, `PermCreate`, `PermOverwrite`, `PermRename`, `PermMove`, `PermManagePermissions`, and `PermViewHistory`. `PermWrite` remains as a composite that expands to create or overwrite where a handler asks for it. The contract is recorded in [ADR-0004](0004-metadata-store.md).

**Mandatory rules** (written into CONTRIBUTING and enforced by CI lint):

- No exported method in the `store` package **accepts** an `Actor` in its signature; it accepts `uuid.UUID` or `*Cap` only;
- The `dav` and `api` packages **must not** import `store` directly (use internal packages plus `go-arch-lint` or a custom import check script);
- Every `SELECT` of user-visible data must carry an `owner_id = $1 OR ...` condition; "fetch first and decide in memory" is forbidden.

### 5.2 Authorization rules (v1)

| Scenario | Rule |
| --- | --- |
| Own files | Full control |
| Group-shared directory | Group roles: viewer (read) / editor (read-write) / manager (read-write + share) |
| Files shared with the actor by others | The permission bits of the share |
| admin | **Does not automatically own all file content** (read-only metadata plus forced purge); viewing content must go through `break-glass` with an audit record |
| guest | Access only to explicitly shared nodes; cannot list the root directory |
| anonymous | Only through a share token, and only upload capability when `hide_contents=true` |

> **"An admin cannot read user file content" is a product decision**, and this ADR recommends keeping it: it turns "administrator abuse" into an auditable event instead of a routine capability. If read access for admins is required, change the rule to "admin read = emit `audit.privileged_read` and notify the user".

### 5.3 Authorization matrix tests

`internal/authz/authz_test.go` is table-driven and exhausts:

```
{role} × {resource ownership: own/same group/other/shared} × {operation: 11 kinds} × {share permission bits}
```

**Every new API adds a row**, and the CI coverage gate sets 100% for this file.

---

## 6. WebDAV implementation

### 6.1 Why not `x/net/webdav`'s `FileSystem`

The `webdav.FileSystem` interface is:

```go
type FileSystem interface {
    Mkdir(ctx, name, perm) error
    OpenFile(ctx, name string, flag int, perm) (File, error)
    RemoveAll(ctx, name string) error
    Rename(ctx, oldName, newName string) error
    Stat(ctx, name string) (os.FileInfo, error)
}
```

It carries **only string paths, no user identity, and no transactions**. Using it means packing a `userID` into a `name` and doing a reverse lookup through a global map — exactly the "path is authorization" design this ADR avoids. It also cannot express:

- quota checks (the byte count must be known before a `PUT`)
- version creation (a `PUT` overwrite must keep the old version)
- lock persistence (`webdav` locks live in an in-memory `LockSystem`)
- share permissions (different shares grant different permissions on the same node)

**Therefore: reuse only its XML types and the method dispatch skeleton of `Handler`, and implement the entire storage layer locally.** The cost is roughly 500–800 lines, in exchange for fully controlled semantics.

### 6.2 Method implementation notes

| Method | Key point |
| --- | --- |
| `PROPFIND` | **Written locally, with streaming output**: first `SELECT ... LIMIT max_children+1` to fetch child nodes, then write XML as rows arrive, avoiding loading everything at once. Beyond `max_propfind_children`, return `507` with an explanation. `Depth: infinity` is refused by default (`403`) and can be enabled by configuration |
| `PROPPATCH` | Only persisted `dead_props` are allowed; changing `DAV:` reserved properties is refused (return `403` with a correct `<D:error>`) |
| `PUT` | Stream to `blobs/tmp/<uuid>` → compute SHA-256 while writing → verify `Content-Length` → insert into blob storage (idempotent) → update node, version, quota, and audit in one transaction → `rename` for atomic visibility |
| `GET/HEAD` | Support `Range` (single interval), `If-Range`, `ETag` (blob sha256 prefix), and `If-None-Match` |
| `DELETE` | Recursively move to trash (soft delete via `trashed_at`) without deleting blobs (refcount) |
| `COPY/MOVE` | Same storage only (v1); strict `Overwrite` header semantics; MOVE across parent directories must check target permission |
| `LOCK/UNLOCK` | Locks **persisted in PostgreSQL**: `lock_tokens(node_id, token, depth, owner_xml, timeout, expires_at)`; validate the lock token from the `If` header; the worker clears expired locks every minute |
| `OPTIONS` | Return `DAV: 1,2`, `MS-Author-Via: DAV`, and `Allow` |
| Authentication failure | `401` with `WWW-Authenticate: Basic realm="newcloud"` (required by Windows) |

### 6.3 Path resolution

```go
// Single entry point: resolve a WebDAV URL path to a node, with a Cap attached
func (h *Handler) resolve(ctx context.Context, a authz.Actor, urlPath string) (authz.Cap, *Node, error)
```

- Relative paths strip the `/dav/<user>/...` prefix of `public_url`;
- Every segment goes through: URL decoding → **Unicode NFC normalization** → rejection of `.`, `..`, empty segments, NUL, `\`, control characters, and Windows reserved names (`CON`, `PRN`, `AUX`, `NUL`, `COM1..9`, `LPT1..9`) → length limits (255 bytes per segment, 4096 total);
- Parsed results are cached per request and never across requests;
- **Any failure returns 404 uniformly** (without distinguishing "does not exist" from "not permitted", to prevent probing).

### 6.4 Large files

The shipped resumable-upload protocol is recorded in [ADR-0008](0008-resumable-uploads.md):

```text
POST   /api/files/uploads                -> {id, offset, chunk_size}
PUT    /api/files/uploads/{id}           Content-Range: bytes a-b/total
POST   /api/files/uploads/{id}/complete
DELETE /api/files/uploads/{id}
```

The server owns the offset; a mismatch returns `409` with the true offset, and a failed chunk truncates the staging file back to the previous offset. Sessions are process-local and do not survive a restart. tus and S3 multipart were evaluated and deliberately not used: both are complete protocols whose state machines exceed the capability needed here.

---

## 7. Deployment (`deploy/docker-compose.yml`)

The shipped Compose file is the reference:

```yaml
services:
  web:
    image: newcloud:latest
    restart: unless-stopped
    environment:
      NEWCLOUD_CONFIG: /etc/newcloud/config.yml
      # The application connects as newcloud_app, not as the owner.
      DATABASE_URL: postgres://newcloud_app:${PG_APP_PASSWORD}@postgres:5432/newcloud?sslmode=disable
    volumes:
      - /etc/newcloud:/etc/newcloud:ro
      - /var/lib/newcloud:/var/lib/newcloud   # one mount: blobs/ and blobs/tmp/
    ports: ["127.0.0.1:8080:8080"]
    depends_on: [postgres]

  postgres:
    image: postgres:17
    restart: unless-stopped
    command: >
      postgres -c wal_level=replica -c archive_mode=on
               -c archive_command='test ! -f /archive/%f && cp %p /archive/%f'
               -c archive_timeout=5min
    volumes:
      - /var/lib/newcloud-pg:/var/lib/postgresql/data
      - /mnt/wal-archive:/archive
    environment: { POSTGRES_PASSWORD: ${PG_PASSWORD} }

  caddy:
    image: caddy:2
    restart: unless-stopped
    volumes: ["./Caddyfile:/etc/caddy/Caddyfile:ro"]
    ports: ["80:80", "443:443"]
    depends_on: [web]
```

**Key points:**

1. `blobs/` and `blobs/tmp/` live under **the same bind mount** (`/var/lib/newcloud`), which satisfies the rename requirement.
2. PostgreSQL data lives on a **separate** mount (`/var/lib/newcloud-pg`), and WAL archives on another (`/mnt/wal-archive`), so a blob snapshot does not compete with the database for IO.
3. The application password (`PG_APP_PASSWORD`) and the owner password (`PG_PASSWORD`) differ; the owner is used only by migrations and by the PostgreSQL container.
3. The WAL archive directory must be **another physical disk or NFS**; on the same disk, a disk failure loses the WAL with everything else.
4. Caddy terminates TLS; `web` listens only on the internal network.

**Backup (`deploy/backup/`):**

- `pg-backup.sh`: `pg_dump -Fc` daily plus a weekly base backup; WAL is archived continuously.
- `blob-backup.sh`: `restic backup /var/lib/newcloud/blobs --tag newcloud-blob`, incremental (content addressing suits it naturally).
- `restore-drill.md`: the restore runbook for M5 acceptance, requiring the actual elapsed time to be recorded.

---

## 8. Observability

| Item | Implementation |
| --- | --- |
| Logs | `slog` JSON on stdout, one line per request plus lifecycle events |
| Metrics | `/metrics` (Prometheus): request counts by method and status class, DAV conflicts, upload and thumbnail outcomes, job depth, audit chain state, blob GC and scrub results, WAL archive state, auth events |
| Tracing | Not implemented. The metric and log surface is the supported observability interface |
| Health | `/healthz` (process), `/readyz` (metadata and content store, plus the active storage layout) |

---

## 9. Test strategy

| Layer | Approach | Threshold |
| --- | --- | --- |
| Unit | `go test ./...` | all packages |
| Authorization matrix | table-driven, per route with an ID | every such route |
| Store contract | the same `internal/storetest` suite against the in-memory and PostgreSQL implementations | both implementations |
| Concurrency | `-race`, plus deterministic race tests | mandatory in CI |
| WebDAV protocol | protocol-level tests for class 2 methods, conditional writes, and lock depth | methods and failure codes |
| Browser | `tools/uicheck` in headless Chrome | rendering, focus, and user data as text |
| Documentation | `TestDocumentsCiteOnlyRealTests` and the gap-table guards | every cited test exists |

---

## 10. Milestone mapping

| Milestone | Part of this ADR |
| --- | --- |
| M0 | §1–§5 (skeleton, configuration, self-checks, authentication, authz) plus the audit tables |
| M1 | §6 (WebDAV) plus both blob backends plus App Passwords |
| M2–M5 | Follow the feature list in 0001 |

---

## 11. Open questions

1. **Exact reuse scope of `x/net/webdav`**: reuse only the `internal/xml` types and `Handler` dispatch, or reuse `PROPFIND` as well and wrap it for streaming. **Resolved:** only the XML types and parts of dispatch are reused; `PROPFIND` is written locally so large directories stream.
2. **Frontend framework**: SvelteKit vs React. This ADR does not lock the choice, but it requires "static build output plus same-origin cookie sessions", never a long-lived token held by the SPA. **Resolved:** React 19 + TypeScript + Vite, with a client generated from the OpenAPI document. See [Interface redesign: Atrium](../reference/interface-atrium.md).
3. **Whether an admin may read user file content**: this document is designed for "no" and requires owner confirmation (see §5.2). **Resolved:** no. Administrators see metadata and can force deletion; they cannot read file bytes.

---

## References

- [ADR-0001: Scope and language](../adr/0001-scope-and-language.md)
- RFC 4918 / 4918bis, RFC 6578, RFC 7233
- [tus resumable upload protocol](https://tus.io/)
- [Go OIDC (`coreos/go-oidc`)](https://github.com/coreos/go-oidc)
- [litmus WebDAV test suite](http://www.webdav.org/neon/litmus/)
