# ADR-0001: Product scope and language choice

**Status:** Accepted · **Date:** 2026-10-21

> **Implementation note (2026-10-22).** This ADR records the scope and language decision. The
> frontend was subsequently fixed as React 19 + TypeScript + Vite with an OpenAPI-generated
> client; references to SvelteKit below describe the options considered, not the shipped stack.
> See [Interface redesign: Atrium](../reference/interface-atrium.md). The Go decision, the
> product scope, and the non-goals stand.

newCloud is a self-hosted file collaboration platform delivered as a single Go binary: OIDC single sign-on through Pocket ID, multi-user and group management, standards-based WebDAV, a web app for upload, download, and preview, the full sharing feature set (link, email recipient, and file request), and append-only audit as a first-class subsystem. Version 1 targets RFC 4918 WebDAV instead of Nextcloud client compatibility, supports local disk and S3-compatible storage with either a blob or a tree layout, and leaves online Office integration behind a WOPI seam. The architecture is designed for multiple replicas while deployment starts on a single Docker host, and user-uploaded content is served from a separate origin and never executed.

## 0. Summary of decisions

1. **Product positioning.** A **self-hosted file collaboration platform** (file storage + WebDAV + sharing + audit). The positioning deliberately excludes user counts, capacity, and deployment shape: those are **parameters of a particular installation**, not the product definition. The product must serve 200 people from a single Docker host and also scale horizontally to multiple replicas. Scale is decided by configuration and deployment, not by code.
2. **Features.** Converge on three tiers: P0 (required in v1), P1 (v1.1), and P2 (later). P0 = OIDC SSO + multi-user and groups + WebDAV + web upload and download + the full sharing feature set (password, expiry, download count, email) + file requests + audit. Everything else moves to P1 or P2.
3. **Language.** **Go.** The reasons are delivery speed, operational simplicity, and low cognitive load for I/O-heavy work. They are unrelated to any particular scale.
4. **The most important architectural decision is not the language.** **User content must be isolated from the main site**: a separate domain and origin, forced `attachment`, `nosniff`, no execution of any uploaded executable content, and no plugin system. The most damaging classes of issue in Nextcloud's security history — the third-party app marketplace, writable and executable `.php` on shared hosting, template and include handling, deserialization — are **architectural** problems. Changing the language does not remove them.
5. **Confirmed product decisions** (§8): IdP = **Pocket ID** (passkey-only); v1 does **not** implement Nextcloud client compatibility; storage is **local disk plus S3 only**, with a blob or tree layout; online Office is **interface only**; the compliance baseline is set by this document (§8.9).
6. **Scale parameters of the current installation** (not the product positioning; see §8.11): about 2000 people, 40 TiB, initially a single Docker host. These numbers decide how this installation is deployed, how disks are purchased, and how backups run. They do not decide how large the code can scale. **The architecture is designed for horizontal scaling; the deployment starts on a single machine.**

## 1. Background: where Nextcloud's "PHP security problems" come from

Splitting the risk apart shows what a language change can and cannot solve:

| Risk source                                                                                      | Does changing the language remove it?                                            | Notes                                                                                             |
|--------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------|
| Memory safety (use-after-free, out-of-bounds)                                                    | Rust: yes. Go: mostly (no use-after-free, but nil dereferences and races remain) | These bugs are actually a small share of file-server defects                                      |
| Deserialization RCE (`unserialize`)                                                              | Yes                                                                              | A new system simply does not expose arbitrary object deserialization                              |
| Authentication bypass through type juggling (`==` loose comparison)                              | Yes                                                                              | Strongly typed languages do not have it                                                           |
| LFI/RFI, `include_path`, broken `.htaccess`                                                      | Yes                                                                              | A single binary needs no web-server configuration                                                 |
| Third-party app marketplace / arbitrary plugin code                                              | Yes, provided no plugin system is built                                          | This is Nextcloud's largest attack surface                                                        |
| Deployment on shared hosting with `data/` inside the webroot                                     | Not language-dependent                                                           | Solved by deployment rules                                                                        |
| **Path traversal, missing authorization (IDOR), ZIP traversal, SSRF, stored XSS, missing audit** | **Not language-dependent**                                                       | These are the mistakes most likely in in-house code, and they require design and tests to prevent |
| Large dependency tree supply chain (composer + npm + app store)                                  | Partly                                                                           | Fewer dependencies mean less attack surface, but this holds for any language                      |

**Conclusion**: a language change eliminates "an entire class of PHP-specific bugs" but **cannot** eliminate the vulnerability types most common in file servers. Therefore:

- Language selection follows "engineering efficiency + maintainability + team capability".
- Security is maintained by **architectural constraints + type and interface design + test strategy** (see the Rust-style type safety approach in §4.4, which Go can adopt unchanged).

## 2. Compatibility strategy (decided first; it moves 30%+ of the work)

Three options with very different costs:

| Option                                            | Meaning                                                                                                               | Client availability                                                                  | Incremental cost       | Notes                                                                                                                          |
|---------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------|------------------------|--------------------------------------------------------------------------------------------------------------------------------|
| **A. Standard WebDAV first** (recommended for v1) | Implement RFC 4918 and common extensions only; the web app is self-contained                                          | Browsers, Windows File Explorer, macOS Finder, rclone, Cyberduck, Office "Open/Save" | Baseline               | Least effort, safest, most controllable                                                                                        |
| **B. Partly Nextcloud-compatible**                | Additionally implement `/status.php`, `/ocs/v2.php/cloud/capabilities`, `oc:` namespace properties, chunked upload v2 | Plus the official Nextcloud desktop and mobile clients                               | **+30–40%**            | Clients probe `status.php` and capabilities to decide their behavior; a missing field causes degradation or connection refusal |
| **C. Fully compatible**                           | B plus sharing, external storage, and the sync protocol aligned                                                       | Full client feature set                                                              | +100%, not recommended | Equivalent to rewriting Nextcloud                                                                                              |

> The probe surface used by the Nextcloud client (`status.php` → unauthenticated PROPFIND to detect Basic authentication → `ocs/v2.php/cloud/capabilities`) is documented by a community compatibility implementation: [karadav NEXTCLOUD.md](https://f-hub.org/Outmode/karadav/raw/commit/442d22eca5eb4de63e05db3709c62604e092b77c/NEXTCLOUD.md).

**Decision**: v1 takes **A** (decided; see §8.2), but reserves a `compat` middleware at the WebDAV layer. If official client support is wanted later, `status.php` and capabilities are implemented first as a probe, without touching the core.

## 3. Feature inventory

### 3.1 Authentication and identity (P0)

| Capability                     | Description                                                                                                                                                                                                                                     |
|--------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| OIDC login                     | **Pocket ID**, Authorization Code + PKCE; server-side session (HttpOnly `__Host-` cookie, SameSite=Lax); `state`/`nonce` validation; ID Token signature and `iss`/`aud`/`exp` validation                                                        |
| Just-in-time provisioning      | On first login, create the user from **`(issuer, sub)`** (`sub` never changes); map `preferred_username`/`email`/`name`/`groups` to the local profile and groups                                                                                |
| Group-to-role mapping          | Request the `groups` scope (Pocket ID requires an explicit request); map by **group name** (for example `media`) to local roles (admin / user / guest); the mapping table is configurable                                                       |
| Logout                         | RP-initiated logout plus local session invalidation. Note: Pocket ID is passkey-only, so there is no "invalidate other sessions after a password change"; revocation relies on local state plus Pocket ID's group check at refresh time         |
| Break-glass fallback           | **Local break-glass administrator (mandatory, not optional)**: separate account + Argon2id + TOTP, reachable only from the management network segment, with alerts on every action. A configuration-file-only emergency switch is also required |
| WebDAV / client authentication | SSO cannot be used inside Basic authentication, so the user generates an **App Password** in the web app; only the Argon2id hash is stored; name, expiry, read-only scope, and revocation are supported; failed authentication is audited       |
| Account lifecycle              | **SCIM 2.0 server endpoints** (Users/Groups subset) receive pushes from Pocket ID (5-minute delta + hourly full sync), which solves "a user was deleted or disabled and no notification was received"                                           |
| Administrator step-up          | Sensitive operations (quota changes, share revocation, audit export) require step-up (TOTP or re-authentication at the IdP)                                                                                                                     |
| Session management             | Users can see their device/session list and revoke one or all sessions                                                                                                                                                                          |

### 3.2 Users, groups, and permissions (P0)

- Users: `username`, `email` (unique), display name, status (active/disabled), quota, used space.
- Groups: membership management, group-level quota and policy (whether external links are allowed, default sharing policy, per-file size cap).
- Roles: `admin` (global), `user`, and `guest` (access only to content shared with it).
- Permission model: **ACL on the node** (owner + groups + explicit grants), not path-prefix authorization (paths change, IDs do not).
- Multi-user isolation: one logical namespace root per user; administrators can mount "shared spaces" or "department spaces".

### 3.3 Storage and quotas (P0)

- Storage backends: **exactly two — local disk (default) and S3-compatible (MinIO/AWS/Ceph RGW)**. The interface stays narrow (`Put/Open/Delete/Stat`); no third backend such as SMB or NFS is under discussion; see §8.7.
- Storage layout (optional at deployment level; see [0003](0003-storage-layout.md)):
  - `blob`: content is stored by SHA-256 as immutable objects (sharded as `ab/cd/<hash>`); the directory tree and attributes live in PostgreSQL. Deduplication applies, and versions consume only the difference.
  - `tree`: `<root>/<owner>/<path>`, with the disk as the directory tree. `ls` works, rsync works, there is no deduplication, and versions are full copies.
- **Separating metadata from content is the baseline shared by both layouts**: at scale, backups cannot keep up, migration has to move all data, and a rename walks the entire tree. With the split, backups track content incrementally and migration is a backend swap that leaves metadata in place.
- Quota: per user and per group; a soft warning threshold plus a hard cap; checked before upload (to avoid failing halfway). `used_bytes` is **maintained incrementally**; a full-table `COUNT(*)` is forbidden, and a periodic reconciliation job runs alongside.
- Per-file limits: global hard cap **1 TB**, default soft limit **10 GB** (raisable to 100 GB per group); over-limit requests return `507`.
- Trash: deletes go to the trash with configurable retention (default 30 days).
- Versions: keep the most recent N versions per node (default 10 versions / 30 days), restorable.
- Instant upload / deduplication: the same hash is written once (`blob` layout only), but **every user is logically independent** (quotas counted separately; one user's delete does not affect another).

### 3.4 WebDAV (P0)

Mandatory (RFC 4918 / 4918bis):

| Item                      | Requirement                                                                                                                                                                                                                                                             |
|---------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Methods                   | `OPTIONS` `PROPFIND` (Depth 0/1/infinity, infinity disableable by configuration) `PROPPATCH` `GET` `HEAD` `PUT` `DELETE` `MKCOL` `COPY` `MOVE` `LOCK` `UNLOCK`                                                                                                          |
| Dead property persistence | Windows and macOS depend on custom properties (for example `Win32LastModifiedTime`); they **must be persisted to the database**, not held only in memory                                                                                                                |
| Locks                     | Exclusive write locks with lock tokens stored in the database and released on timeout; `If` header lock token validation                                                                                                                                                |
| Range                     | `Range: bytes=` single range is mandatory (resume and scrubbing); multiple ranges optional; `If-Range` with `ETag`                                                                                                                                                      |
| Conditional requests      | `If-Match` / `If-None-Match` / `If-Modified-Since` / `If-Unmodified-Since`                                                                                                                                                                                              |
| Property aliases          | `DAV:quota-available-bytes`, `quota-used-bytes`, `resourcetype`, `getcontentlength`, `getetag`, `getlastmodified`, `creationdate`, `supported-report-set`                                                                                                               |
| Client quirks             | Windows mini-redirector: HTTPS with a valid certificate is mandatory, Basic authentication, and the default 50 MB limit must be raised; macOS Finder: `.DS_Store`/`._*` AppleDouble files, unusual `PROPFIND` sequences for folder upload, `LOCK` required before `PUT` |
| Error semantics           | `401` must carry `WWW-Authenticate: Basic realm=...`; `423` (Locked), `507` (Insufficient Storage), `412`, and `424` must be correct                                                                                                                                    |
| Large files               | Streaming reads and writes with memory use independent of file size; upload to a temporary file plus atomic rename (**partial writes must never be visible**); chunks of 64–256 MB                                                                                      |

**v1 constraints reserved for the v1.1 "Windows mapped network drive" work** (see §8.5): these are not a later redesign, they must be satisfied now. HTTPS with a trusted certificate, Basic authentication available (App Password), locks persisted to the database, dead properties persisted, `DAV: 1,2` and `MS-Author-Via: DAV`, and Unicode NFC normalization.

P1: `REPORT` (`sync-collection`, RFC 6578), needed for incremental client sync and high value for `rclone`-class tools.

P1: the `litmus` compliance suite in CI.

> Go's `golang.org/x/net/webdav` provides an RFC 4918 reference implementation, but it does **not persist dead properties**, keeps locks in memory, and has no quota or chunked upload, so it serves only as a skeleton ([pkg.go.dev](https://pkg.go.dev/golang.org/x/net/webdav)). On the Rust side, `dav-server` (a fork of `webdav-handler`) is semantically more complete ([crates information](https://packages.ecosyste.ms/registries/crates.io/keywords/webdav-server), [Debian packaging review](https://dfsg-new-queue.debian.org/reviews/rust-dav-server/0.11.0-1)), but storage and authentication still have to be wired up.

### 3.5 Web app basics (P0)

- Upload: drag and drop, multi-select, folder upload (`webkitdirectory`), **chunked with resumable upload** (**tus protocol** or an equivalent implementation), progress, retry, cancel.
- Download: single-file streaming; **multi-select ZIP download** (streamed ZIP generation, `Content-Disposition: attachment`, attention to Zip64 and 4 GB+ cases).
- Browsing: grid/list, breadcrumb, sorting, search (file name), rename, move, copy, delete, trash, version history.
- Preview (P0 basic): images, PDF, plain text and code, audio and video (within native browser support).
- Frontend stack: SvelteKit or React + Vite (independent of the backend language). **The BFF pattern is mandatory**: no long-lived token in the SPA; use the same-origin cookie session.

### 3.6 Sharing (P0, the core differentiator)

Three share types, modeled uniformly as `share` plus permission bits:

**(a) Public link sharing**

- Optional password (Argon2id hash), optional expiry (absolute time or N days), optional **maximum download count** (each successful download increments atomically under concurrency to prevent bypass).
- Permissions: preview only / read-only download / upload into that folder (drop box) / edit.
- "Preview only, download disabled" means the server transcodes a watermarked image or PDF stream and **never serves the original** (P1).
- Optional: IP/CIDR or country restriction on downloads, a domain Referer allowlist, or login required (internal users only).
- Link shape: `https://<share-domain>/s/<token>` (128-bit random token, **only the hash is stored in the database**); custom alias (P2).
- Security: `X-Robots-Tag: noindex`, rate limiting against token guessing, and a uniform 404 for expired links (no existence disclosure).

**(b) Share to a specific email address**

- The recipient enters an email, and a notification email is sent. On access:
  - if the email already belongs to an internal user, grant access directly to that account (recommended, safest);
  - if the email is external, send a one-time code or magic link by email and allow access only after verification. **This is how "only this person" is implemented**, preventing link forwarding abuse.
- Records: invitation time, first access, verification time, download count, expiry.
- The owner sees the recipient list and can revoke with one click.

**(c) Upload request / file request (drop box)**

- For "let someone send files into a designated folder once or a few times":
  - **upload only; no directory listing, no download, no delete** (enforced server-side, not hidden by the frontend).
  - Limits: link-level `max_uploads` (1 / N / unlimited), per-file size cap, total size cap, file count cap, MIME/extension allowlist, optional deadline.
  - Anonymous uploaders may leave a name and email plus optional email verification, for attribution; each upload becomes a separate node with the uploader recorded in metadata.
  - Completion notification (email or Webhook to the folder owner), with configurable "review before commit" (P2).
  - Anti-abuse: per-IP rate limiting, optional one-time code, immediate rejection over quota, and content sniffing of file types (not only the extension).
  - Automatic expiry and cleanup of the temporary folder.
- Difference from (a): these shares default to `hide_contents=true`, and the only permission bit is `create`.

**Sharing capabilities common to all types**

- Share list page (sent shares / received shares) with password change, expiry change, immediate revocation, and extension.
- Email reminder to the owner N days before expiry (P1).
- All share changes are audited.

### 3.7 Audit and compliance (P0)

This is the part that a PHP-based approach cannot paper over. It must be a first-class subsystem from day one.

**Event model** (append-only, write-heavy and read-light, separate table and separate database):

```sql
audit_events(
  id            bigserial,
  ts            timestamptz,
  actor_type    enum(user, anonymous, system, admin),
  actor_user_id uuid null,
  actor_email   text null,          -- identity for anonymous uploads and email shares
  actor_ip      inet,
  user_agent    text,
  action        text,               -- see the required action list below
  target_kind   enum(node, share, user, group, setting, session),
  target_id     text,
  share_id      uuid null,
  result        enum(ok, denied, error),
  bytes         bigint null,
  metadata      jsonb,              -- file name, path snapshot, old and new values, failure reason
  prev_hash     bytea,
  hash          bytea               -- sha256(prev_hash || canonical JSON)
)
```

Required actions (minimum set):

- `auth.login.success` / `auth.login.failure` / `auth.logout` / `auth.session.revoke` / `auth.apppassword.create|revoke`
- `share.create` / `share.update` / `share.revoke` / `share.password.verify.failure` / `share.expired` / `share.limit_reached`
- **`share.download`** (who, which share, which file, byte count, IP, user agent, which attempt, whether email verification passed)
- `share.view` / `share.upload` (drop box receipt)
- `file.read` / `file.write` / `file.delete` / `file.restore` / `file.move` / `file.download_zip`
- `user.create` / `user.disable` / `user.quota.change` / `group.member.change`
- `admin.setting.change` / `audit.export`
- `security.anomaly` (mass downloads from one IP, bulk access at an unusual time of day, and similar patterns)

**Tamper resistance**:

- A per-row hash chain (`prev_hash`) detects a single quietly edited row, but not a wholesale rewrite of a segment.
- Real tamper evidence needs **external anchoring**: write the chain head hash daily to independent WORM storage, a write-only object store, an external SIEM, or email it to the security administrator with S/MIME signing.
- Separate database privileges on the audit table: the application has `INSERT` only, no `UPDATE`/`DELETE` (PostgreSQL `REVOKE`).
- Retention: configurable (for example 1 year or 3 years); archive on expiry instead of deleting.

**Delivery and integration** (P1): `syslog` (RFC 5424 / CEF), OpenTelemetry, Webhook, and CSV/JSON export (the export action itself is audited).

**User-visible access log** (P1): share owners can see who downloaded how many times and when. This is a real requirement for many enterprise users.

### 3.8 Security baseline (P0, a cross-cutting constraint rather than a feature)

These constraints must be written into the design and enforced in CI:

1. **Content is not same-origin with the main site**: uploaded content is served from `usercontent.<domain>` (or a separate port or domain), and main-site cookies do not cross that boundary; `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox`.
2. **No inline rendering** of uploaded HTML/SVG/XML; previews convert to images or force a download. Stored XSS is the number one problem in file products.
3. Path safety: every user-supplied path passes through **one canonicalization function** that rejects `..`, absolute paths, NUL, Windows reserved names, and over-long names. The storage layer uses IDs, not paths, and a path appears exactly once, at resolution.
4. SSRF: external storage, webhooks, mail servers, and URL previews all go through an **outbound allowlist proxy**; `169.254.169.254`, `127.0.0.0/8`, and private ranges are blocked (openable by configuration).
5. Zip and extraction operations: path traversal and zip bombs (limit the expansion ratio and total size). (P1)
6. Uploaded content: virus scanning (ClamAV or ICAP) completes before content becomes visible; uploads land in a quarantine area and enter the store only after a clean scan. (P1, but strongly recommended for v1.1)
7. Authentication: Argon2id; all tokens stored only as hashes; constant-time comparison; rate limiting on login and share password verification (by IP + account + token).
8. HTTP headers: HSTS, `X-Frame-Options: DENY` (share pages may use `SAMEORIGIN`), a strict CSP, `Referrer-Policy: no-referrer`, and `Permissions-Policy`.
9. CSRF: all state-changing endpoints use double-submit tokens or `SameSite=Strict` plus Origin validation.
10. Dependency governance: minimal dependencies, `cargo audit` / `govulncheck` in CI, SBOM, pinned versions, and no dynamic plugin loading.
11. Log hygiene: never log passwords, tokens, or file content; truncate or retain IPs according to the compliance policy.

### 3.9 Recommended additions (not in the original requirements, but strongly advised)

Ordered by value and cost:

**Recommended for v1 (low cost, high value)**

| Feature                                                                    | Rationale                                                                                                                            |
|----------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------|
| **App Password / device authorization**                                    | Without it, WebDAV is unusable under SSO; a **hard dependency**, not an enhancement                                                  |
| **Virus scanning**                                                         | File sharing plus internet uploads is a malware highway; one ClamAV integration prevents most incidents                              |
| **Rate limiting and brute-force protection**                               | Required at three points: share tokens, passwords, and logins                                                                        |
| **Share access alerts**                                                    | Abnormal download volume on a single link (for example more than N in one hour) automatically pauses the link and notifies the owner |
| **Dynamic watermarking** (PDF and images, carrying email + time + IP)      | Very high value for leak-prevention requirements, simpler and more reliable than DRM                                                 |
| **Administrator step-up authentication plus complete administrator audit** | A mandatory item in internal-control and compliance audits                                                                           |
| **Trash and versions**                                                     | The minimum protection against mistakes and ransomware; under ransomware, versions are the one thing that saves the data             |
| **Webhook + REST API**                                                     | Integrates with WeCom, DingTalk, Slack, ticketing, and DLP at near-zero cost                                                         |
| **Prometheus / health checks / structured logs**                           | The operational baseline                                                                                                             |

**Recommended for v1.1**

| Feature                                                                     | Rationale                                                                  |
|-----------------------------------------------------------------------------|----------------------------------------------------------------------------|
| **Email verification and uploader identity for file requests**              | The requirement already implies "who sent this"; verification completes it |
| **Share visitor list (audit visualization)**                                | Self-service compliance and fewer IT tickets                               |
| **Preview enhancements**: video transcoding, HEIC, RAW                      | Affects experience, not security                                           |
| **Full-text search** (Tantivy / Meilisearch)                                | Past a certain file count, no search means no product                      |
| **Retention policy / legal hold**                                           | Required in compliance settings                                            |
| **External-link approval workflow** (administrators approve external links) | Common in finance, healthcare, and government                              |
| **Share expiry renewal reminders**                                          | Pure experience                                                            |

**Explicitly not recommended (to prevent scope creep)**

- **Plugins and app marketplaces**: the largest source of Nextcloud's attack surface and firmly out of scope. Integrations go through Webhooks, APIs, or separate services.
- **End-to-end encryption (E2EE)**: **fundamentally conflicts** with auditing who downloaded what, virus scanning, watermarking, and download prevention. Only one of the two is possible. v1 does not do it. If a need genuinely exists, implement it later as a separate "personal vault only" mode (P2) without any audit promise.
- **Federation**, **calendar/contacts/mail client**: not this project's value proposition.
- **An in-house online Office editor**: integrate OnlyOffice or Collabora over WOPI instead; an editor is another person-year.
- **Home-grown cryptography or token formats**: use standard libraries.

### 3.10 Non-goals (explicitly not in v1)

Federation, E2EE, plugins, calendar/contacts/mail, multi-tenant SaaS billing, a native mobile app (use a PWA), and an offline desktop sync client (use standard WebDAV with OS mounting).

## 4. Language selection

### 4.1 Candidate stacks

**Rust**

- Web: `axum` + `tokio` + `hyper` + `tower` middleware
- WebDAV: `dav-server` (`webdav-handler` fork with lock and dead-property support)
- Database: `sqlx` (compile-time SQL verification) + PostgreSQL
- Object storage: the `object_store` crate (unified S3, local, and GCS)
- Authentication: `openidconnect`, `argon2`, `rand`
- Mail: `lettre`; observability: `tracing` + `opentelemetry`; API: `utoipa`
- Frontend: SvelteKit (built separately, static assets embedded with `rust-embed`)

**Go**

- Web: `net/http` (1.22+ routing) + `chi`, or the standard library alone
- WebDAV: `golang.org/x/net/webdav` as a skeleton (XML types and method dispatch); propstore, lock persistence, and chunked upload implemented in-house
- Database: `pgx` + `sqlc` (type-safe generated queries) + `goose`/`atlas` migrations
- Object storage: `aws-sdk-go-v2` or `minio-go`
- Authentication: `coreos/go-oidc` + `golang.org/x/crypto/argon2`
- Mail: `wneessen/go-mail`; observability: `slog` + `otel`; API: `oapi-codegen`
- Frontend: as above

### 4.2 Fit against this project's key requirements

| Dimension                                                    | Rust                                                      | Go                                                     | Notes                                       |
|--------------------------------------------------------------|-----------------------------------------------------------|--------------------------------------------------------|---------------------------------------------|
| Streaming large files / Range / backpressure                 | Good (`Body::from_stream`)                                | **Very good** (`io.Copy` is direct)                    | Go has the lower cognitive load here        |
| WebDAV semantic completeness                                 | **Good** (`dav-server` is closer to the RFC)              | Fair (dead properties, locks, and quota must be added) | The gap is about 500–800 lines, manageable  |
| Dead property and lock persistence                           | Must wire the database                                    | Must wire the database                                 | Even                                        |
| Compile-time guarantee of path and authorization correctness | **Strong** (newtypes make a path mistake fail to compile) | Weak (conventions plus review)                         | See §4.4                                    |
| Development speed (first usable version)                     | 30–60% slower                                             | **Fast**                                               | Mostly the borrow checker and compile waits |
| Single binary deployment                                     | Very good (~20–40 MB)                                     | **Very good** (~15–25 MB, simpler cross-compilation)   | Even, slight edge to Go                     |
| Runtime memory (1k concurrent)                               | Lower                                                     | Low (GC is invisible in this workload)                 | The difference is not a deciding factor     |
| Production troubleshooting                                   | Good (`tracing`)                                          | **Very good** (`pprof` out of the box)                 | Slight edge to Go                           |
| Hiring and onboarding                                        | Hard                                                      | **Easy**                                               | An organizational reality with real weight  |
| Ecosystem maturity (OIDC, mail, object storage)              | Adequate                                                  | **More mature**                                        | `coreos/go-oidc` is the de facto standard   |
| Long-term maintenance (10 years)                             | Good (no GC, no implicit behavior)                        | Good                                                   | Even                                        |

### 4.3 Decision matrix (1–5 points, weighted)

Weight group A: **delivery speed and team sustainability first** (recommended for internal tools)

| Dimension                                         | Weight | Rust     | Go       |
|---------------------------------------------------|--------|----------|----------|
| Delivery speed / maintainable staffing            | 30%    | 3        | 5        |
| Security correctness (memory, types, concurrency) | 25%    | 5        | 3.5      |
| WebDAV ecosystem                                  | 15%    | 4        | 3        |
| Performance and resource use                      | 10%    | 5        | 4        |
| Operations and deployment                         | 10%    | 5        | 5        |
| Hiring and onboarding                             | 10%    | 2        | 5        |
| **Weighted**                                      |        | **3.95** | **4.23** |

Weight group B: **"security product" positioning first**

| Dimension                 | Weight | Rust     | Go       |
|---------------------------|--------|----------|----------|
| Delivery speed            | 20%    | 3        | 5        |
| Security correctness      | 40%    | 5        | 3.5      |
| WebDAV ecosystem          | 15%    | 4        | 3        |
| Performance and resources | 10%    | 5        | 4        |
| Operations                | 5%     | 5        | 5        |
| Hiring                    | 10%    | 2        | 5        |
| **Weighted**              |        | **4.15** | **4.00** |

**Reading**: the gap stays around 0.2 under both weightings. The language is therefore **not the deciding factor**; the real decision variable is whether the team can write it.

### 4.4 Decision and rationale

> **Settled (2026-10-21, third round)**: **Go**. The decision is locked and is not re-evaluated. The original argument is retained below as supporting evidence.

**Go**, for these reasons:

1. About 80% of this project's complexity is I/O, protocols, permissions, and audit rather than computation. Go's concurrency and streaming model is the least effort here, and so is `pprof`.
2. The WebDAV gaps (dead properties, locks, quota, chunked upload) are **well-defined and bounded** work that Go can absorb. Rust's `dav-server` would still need a code review and storage wiring, so the benefit is smaller than it appears.
3. Delivery speed decides whether the project survives. The biggest risk in rewriting Nextcloud is not vulnerabilities but **never finishing**.
4. A single binary with `CGO_ENABLED=0` cross-compilation makes deployment and rollback simplest: small scale is `docker compose up`, large scale is the same image started multiple times. Two deployment shapes do not need two code paths.
5. Go's runtime model (goroutines + `io.Copy` + the `net/http` standard library) has the lowest cognitive load for "many long-lived concurrent connections, each moving bytes", which is the main loop of a file service regardless of cluster size.

**Invalidated condition for switching to Rust**: the earlier condition was "the team has at least one year of production Rust experience". That condition is void because this round confirmed that language selection does not follow team experience. Go is final.

**Whichever language is chosen, adopt Rust's "typed safety" approach.** That is where the real security benefit comes from:

```go
// Counterexample: string paths everywhere, correctness by human memory
func (s *Store) Open(userID, path string) (...)
func (s *Store) Open(userID, path string) (...)   // forgot validation? The compiler does not care

// Good: IDs and paths are newtypes with private construction; crossing a boundary requires an explicit conversion
type NodeID struct{ /* private field, produced by ResolvePath or the database */ }
type VPath  struct{ /* private field, constructible only by Normalize(), already validated */ }

func (s *Store) Open(u UserID, id NodeID, v VPath) (*Blob, error)
// Authorization has exactly one entry point: Authorize(actor, node, perm); no other code holds a raw path
```

Supporting test strategy (language-independent):

- **Fuzz tests** for paths and object names (`go test -fuzz` / `cargo-fuzz`): `..`, NUL, Unicode normalization, `.`/`..`, over-long names, Windows reserved names.
- **Authorization matrix tests**: every API × every role × every ownership relation, exhaustively asserted. This is the only reliable defense against IDOR.
- Concurrent download counting for share tokens: atomicity plus race tests (`-race`).
- Dependency scanning and SBOM in CI.

### 4.5 The option of not rewriting (worth evaluating at least once)

The requirement list overlaps existing projects by 60–70%, so the opportunity cost of a rewrite has to be counted:

- **SFTPGo** (Go, AGPLv3): a unified WebDAV/FTP/SFTP/S3 gateway with sharing (password, validity, permissions), quotas, OIDC/SSO, audit events, and a REST API. What is missing is a good web app for ordinary users, plus file requests and email sharing.
- **Seafile / Pydio Cells**: close in features, but also large systems.
- **rclone serve webdav plus a custom frontend**: suitable only at very small scale.

**Judgment**: for an internal system that must be fully controllable, auditable, and free of plugin attack surface, building in-house is justified. If the goal is only to escape Nextcloud updates and security advisories, forking SFTPGo or moving to Seafile may cost one fifth of a rewrite. **Spend two days before M0 trialing these three options** and record the conclusion in this document.

> A note on AGPLv3: internal use has no effect. Offering SaaS externally triggers the network-use clause.

## 5. Architecture sketch (language-independent)

```text
                    ┌──────────────┐
Browser SPA ───────▶│  BFF / API   │──▶ session (cookie) ──▶ IdP (OIDC)
                    │  (same site) │
WebDAV client ─────▶│  DAV entry   │──▶ App Password authentication
                    └──────┬───────┘
                           │  authorization choke point: Authorize(actor, node, perm)
                    ┌──────▼───────┐        ┌──────────────┐
                    │ Metadata (PG)│◀──────▶│  Audit (PG)  │──▶ syslog / OTel / WORM anchor
                    └──────┬───────┘        └──────────────┘
                           │ node → blob key
                    ┌──────▼───────┐        ┌──────────────┐
                    │  Blob store  │        │ Async worker │──▶ virus scan / watermark / thumbnail / mail / Webhook
                    │   FS | S3    │        └──────────────┘
                    └──────────────┘
           Content domain: usercontent.<domain> (separate origin, forced attachment)
```

Key constraints:

1. **Downloads always pass through the server** (no plain S3 presigned URLs); otherwise "who downloaded" cannot be recorded, counted, or watermarked. A short-lived signed URL may point at **newCloud's own** download endpoint, but it must not turn that endpoint into a public CDN.
2. Uploads: small files go direct; large files are chunked (tus or S3 multipart). S3 direct upload would require scanning and registration in the completion callback, so v1 simplifies everything to server-side proxying.
3. Async work uses an **outbox table plus workers** with no additional message queue. The outbox is transactional and naturally supports multiple workers contending for the same table, so the same architecture runs from one process to many replicas; only the worker count changes. If a dedicated queue is ever required, only the outbox consumer is replaced and business code stays unchanged.
4. Audit writes and business writes share **one transaction** (outbox), guaranteeing "no operation without an audit record".
5. **Deployment shape is not an architectural premise**: the same binary must run under `docker compose up` on one host and as multiple replicas. The constraint is: **do not assume in business code that this process is the only one**. See §8.7 and §8.11.

Data model draft (PostgreSQL):

```sql
users(id uuid pk, username citext unique, email citext unique, display_name text,
      idp_issuer text, idp_subject text, role text, status text,
      quota_bytes bigint, used_bytes bigint, created_at timestamptz,
      unique(idp_issuer, idp_subject));        -- Pocket ID: sub never changes

groups(id uuid pk, name citext unique, display_name text, quota_bytes bigint);
group_members(group_id, user_id, pk(group_id, user_id));

-- mapping from IdP group name to local role/policy (Pocket ID's groups claim carries group names)
group_role_map(idp_group_name citext pk, role text, policy_id uuid null);

-- required for SCIM push idempotency: record externalId and version
scim_sync_state(entity text pk, external_id text, last_synced_at, etag text);

sessions(id uuid pk, user_id, created_at, last_seen_at, ip inet, ua text,
         auth_method text,                    -- oidc | local_breakglass
         amr text[], revoked_at timestamptz);

app_passwords(id uuid pk, user_id, label text, secret_hash bytea, scopes text[],
              expires_at, last_used_at, revoked_at);

nodes(id uuid pk, parent_id uuid null, owner_id uuid, name text, kind text,
      size bigint, mime text, etag text, blob_id uuid null,
      created_at, modified_at, trashed_at timestamptz, version_of uuid null);
-- uniqueness: unique(parent_id, name) where trashed_at is null
-- indexes: (owner_id, parent_id), (blob_id)

blobs(id uuid pk, sha256 bytea unique, size bigint, storage_backend text,
      storage_key text, refcount int);
-- at scale: the (sha256) index backs instant upload; storage_key looks like ab/cd/<sha256>

versions(id uuid pk, node_id, version_no int, blob_id, size, created_at, created_by);

shares(id uuid pk, token_hash bytea unique, node_id, owner_id,
       kind text,                 -- link | email | request
       password_hash bytea null, expires_at timestamptz null,
       max_downloads int null, download_count int default 0,
       max_uploads int null, upload_count int default 0,
       max_bytes_per_file bigint null, allowed_mime text[] null,
       permissions text[],        -- read | create
       hide_contents bool not null default false,
       revoked_at timestamptz, created_at);

share_recipients(id uuid pk, share_id, email citext, user_id uuid null,
                 invited_at, verified_at, first_access_at, last_access_at);

lock_tokens(node_id, token text, owner_xml text, scope text, timeout_sec int, expires_at);
-- locks must be stored in the database: in-memory locks fail on restart or with multiple
-- replicas and corrupt concurrent Office writes

dead_props(node_id, ns text, name text, value text, pk(node_id, ns, name));
-- Windows drive mapping and macOS Finder both depend on it

audit_events(...)  -- see §3.7; PARTITION by month, the application has INSERT only
```

**Indexing and partitioning requirements that matter at scale** (easy to get wrong in v1 and hard to fix later):

- The `unique(parent_id, name)` constraint on `nodes` must be a **partial index** (`where trashed_at is null`), otherwise the trash blocks recreating an identically named node.
- `dead_props` holds only a few properties per node but must be clustered by `node_id`, otherwise deleting a file turns `PROPFIND` into N amplified queries.
- `audit_events` is **partitioned by month**; one year reaches hundreds of millions of rows. Append only, and `DETACH PARTITION` into object storage on expiry.
- `PROPFIND Depth:1` on a large directory (more than 100,000 files) must **stream output with pagination protection**, otherwise a single request exhausts memory. Set a threshold on entries per directory (for example 100,000); above it require pagination or return `507` or a custom error.

## 6. Milestones and estimates (one person, full time, Go baseline)

| Phase                   | Content                                                                                                                                                                             | Person-weeks                                                                     |
|-------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------|
| M0 skeleton             | Repository and CI, **Docker Compose**, **Pocket ID OIDC login**, sessions, users and groups, **SCIM endpoints**, break-glass administrator, audit table and outbox, healthz/metrics | 2.5                                                                              |
| M1 storage and WebDAV   | **blob content-addressed storage (local and s3 implementations)**, node tree and permissions, all WebDAV methods + locks + dead properties, App Password                            | 4                                                                                |
| M2 web app              | Browsing, upload (chunked resume, large-file channel), download, ZIP packaging, trash, versions, rename and move                                                                    | 3.5                                                                              |
| M3 sharing              | Links (password, expiry, count), email sharing (one-time code), file requests, share management page                                                                                | 3.5                                                                              |
| M4 audit and hardening  | Audit visualization, export and syslog, alert rules, security headers, rate limiting, fuzz and authorization matrix tests, penetration test fixes                                   | 2.5                                                                              |
| M5 migration and launch | Nextcloud data migration tool, **backup and restore drill (acceptance item)**, operations documentation                                                                             | 2                                                                                |
| **Total**               |                                                                                                                                                                                     | **≈18 person-weeks (about 4.5 months for one person; about 2.4 months for two)** |

Not included: online Office integration (+3 person-weeks; v1 keeps only the interface), Nextcloud client compatibility (+5–7 person-weeks), and E2EE (not estimated).

v1.1 additions: Windows drive-mapping compatibility tuning + litmus + rclone/Office end-to-end (+2 person-weeks), and virus scanning (+1 person-week).

Conservative estimation assumptions: no existing account system to reuse, a frontend built from scratch, and one round of external penetration testing. **Historical experience: filesystem work typically takes 1.3–1.5 times the estimate**, with path and permission edge cases consuming most of the difference.

## 7. Principal risks

| Risk                                                                                                                   | Impact                 | Mitigation                                                                                                                                                                                                  |
|------------------------------------------------------------------------------------------------------------------------|------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Authorization logic scattered, causing IDOR                                                                            | **Highest**            | Single `Authorize()` entry point + exhaustive matrix tests + a mandatory code review checklist                                                                                                              |
| **Backups taken but never verified** (a backup that was never restored is no backup)                                   | Very high              | Restore drills must actually run and record the duration; monitor "time of last successful backup"                                                                                                          |
| **Code that assumes "this is the only process"** (sessions, locks, queues in process memory)                           | Very high              | All coordination through the database (`sessions`, `lock_tokens`, `jobs` tables); caches accelerate but never hold truth. See §8.7                                                                          |
| Long tail of client compatibility (Windows, macOS, Office)                                                             | High                   | v1 promises only "standard WebDAV, tested against three mainstream clients"; publish a compatibility matrix and run `litmus`/`rclone` smoke tests in CI                                                     |
| Concurrency holes in share counters and expiry                                                                         | High                   | Atomic database updates + `-race` tests + unit tests for "exactly the Nth attempt"                                                                                                                          |
| **Single deployment point** (if an installation runs on one machine, its availability is capped by that machine)       | High (deployment side) | A consequence of deployment choice, not a product defect: PG streaming replication, an S3 backend, and multiple app replicas all change deployment only; quarterly restore drills are an M5 acceptance item |
| Backup window insufficient at large capacity                                                                           | High                   | Incremental content (restic) + PG WAL archiving; write RPO/RTO into the documentation and drill quarterly                                                                                                   |
| **Pocket ID single point of failure** (passkey-only; a lost administrator device or a dead service locks everyone out) | High                   | Local break-glass administrator (mandatory) + configuration-file emergency switch + multiple administrator passkeys                                                                                         |
| Scope creep (preview, Office, E2EE)                                                                                    | High                   | §3.10 is the guardrail; new requirements must enter P1 or P2 with a cost estimate                                                                                                                           |
| In-house migration tool misreads Nextcloud data                                                                        | Medium                 | Migrate files and users only (not shares); verify read-only with hash comparison before cutover                                                                                                             |
| Over-promising "tamper-proof" audit                                                                                    | Medium                 | State "hash chain + external anchoring" explicitly; do not promise "absolute immutability"                                                                                                                  |
| **Deduplication causing "deleted but not freed" confusion**                                                            | Medium                 | Quotas are logical; the UI shows two columns, "actual usage" and "usage after deduplication" (blob layout only)                                                                                             |
| SCIM misconfiguration causing bulk disablement                                                                         | Medium                 | SCIM writes require a dry run with a diff preview; large batches need confirmation; 7-day soft delete                                                                                                       |
| **Temporary and final areas on different filesystems** (rename degrades to copy and crushes I/O)                       | Medium                 | Startup self-check: a real rename probe for the `blob` layout, and tmp inside the root for the `tree` layout                                                                                                |

## 8. Decision record (2026-10-21, rounds 2–4)

> **Fourth-round correction**: an earlier version wrote "user count / capacity / deployment shape" into the product positioning, which was wrong. These are **scale parameters of a specific installation** (§8.11), not the product definition. SFTPGo, Nextcloud, and ownCloud do not define themselves that way; doing so writes one deployment's capacity into the system's capability ceiling. The positioning is now "self-hosted file collaboration platform", the architecture is designed for horizontal scaling, and deployment starts on a single machine.

| #  | Question                        | Decision                                             | Type                  | Impact                                                                |
|----|---------------------------------|------------------------------------------------------|-----------------------|-----------------------------------------------------------------------|
| D1 | IdP                             | **Pocket ID** (passkey-only)                         | Product decision      | The authentication layer is cut to Pocket ID's capabilities; see §8.1 |
| D2 | Nextcloud client compatibility  | **Not in v1** (see §8.2)                             | Product decision      | Follows option A in §2; saves 30–40%                                  |
| D3 | Storage backend                 | **Local disk + S3-compatible; two is enough**        | Product decision      | Narrow interface; see §8.7                                            |
| D4 | Storage layout                  | **blob / tree, selectable** (deployment level)       | Product decision      | See [0003-storage-layout.md](0003-storage-layout.md)                  |
| D5 | Online Office                   | **Not in v1; interface only**                        | Product decision      | See §8.8 (WOPI seam)                                                  |
| D6 | Windows mapped network drive    | **Not in v1; v1.1**                                  | Product decision      | Underlying constraints met in v1; see §8.5                            |
| D7 | Compliance baseline             | **Set by this document**                             | Product decision      | "MLPS Level 2 reference + 12 months of audit"; see §8.9               |
| D8 | Language                        | **Go**                                               | Product decision      | See §4.4                                                              |
| D9 | Single-file limit               | **10 GB soft / 1 TB hard by default** (configurable) | Configuration default | See §8.4                                                              |
| S1 | Current installation capacity   | **About 40 TiB**                                     | Scale parameter       | Decides how disks are purchased; see §8.11                            |
| S2 | Current installation user count | **About 2000 people**                                | Scale parameter       | Decides whether one replica is enough                                 |
| S3 | Current deployment shape        | **Starts as single-host Docker Compose**             | Scale parameter       | Adding machines does not change code; see §8.7                        |

### 8.1 Pocket ID's practical consequences (two designs changed)

Pocket ID is a passkey-only, passwordless OIDC IdP whose capability boundaries directly rule out copying Nextcloud's account model:

1. **There are no passwords to migrate and no "password reset by email".** Account recovery in Pocket ID is a one-time login code issued by an administrator ([Account recovery](https://pocket-id.org/docs/troubleshooting/account-recovery)). The system therefore **must not** offer local password login as the primary path, or it immediately becomes the weakest link in the chain. Local passwords exist only for the break-glass administrator and are disabled by default.
2. **The `groups` claim is released only when the `groups` scope is explicitly requested**, and it carries **group names** (for example `media`, not display names) ([Scopes and claims](https://pocket-id.org/docs/guides/scopes-and-claims)). Role mapping therefore uses the strings in `groups`, and the configuration item is a "group name → role" map; **do not** use group UUIDs.
3. **`sub` never changes, and `preferred_username` is the username.** The user primary key is `(issuer, sub)`; the username is display data and may change.
4. **Known pitfall**: the initially issued ID Token contains `groups`, but one obtained through refresh token exchange may not ([issue #987](https://github.com/pocket-id/pocket-id/issues/987)). **Do not** use `groups` from the ID Token for real-time authorization; permissions come from the local database, and groups are synchronized once at login.
5. **On refresh, Pocket ID verifies that the user still exists, is not disabled, and is still in the client's allowed groups**; removal from a group takes effect at the next refresh (expiry after 30 days of inactivity) [same source]. Session lifetimes can therefore be generous (for example a 12 h session with silent renewal); **revocation depends on Pocket ID's group check plus group synchronization at each login and periodically**.
6. **Pocket ID supports SCIM push for users and groups** (5-minute delta push + hourly full sync, [SCIM documentation](https://pocket-id.org/docs/configuration/scim)). **Implement SCIM 2.0 server endpoints in v1** (a subset of GET/POST/PATCH/DELETE for Users/Groups).
   - Rationale: OIDC identifies a user only **at login**; **a deleted or disabled user is never reported**. This is the classic source of zombie accounts in in-house systems.
   - Cost: about 3–5 person-days. Benefit: automated account lifecycle and audit compliance. This is the one addition that most clearly belongs in v1.
7. Pocket ID signs RS256 only (changeable), and JWKS is served from the standard `jwks_uri`. Standard discovery through `coreos/go-oidc` is enough; no customization is needed.
8. **Administrators are passkey-only as well.** If Pocket ID is down or an administrator loses the device, nobody in the cluster can log in. **Break-glass is mandatory** (below).

**Two hard requirements follow:**

- **Local break-glass administrator** (upgraded from optional to **mandatory**): separate account + Argon2id + TOTP, reachable only from the management network, with every action audited and alerted by email. Purpose: Pocket ID unavailable, or a group-mapping mistake locking administrators out.
- **Emergency bypass switch**: the configuration key `auth.allow_local_admin=true` must be openable from configuration files alone (no database dependency), preventing a deadlock in which the database administrator account has been disabled.

### 8.2 Nextcloud client compatibility: not in v1, but leave an extension point

**Why not:**

1. The compatibility surface is not "a few APIs" but **an entire implicit protocol**: `status.php` capability negotiation, `oc:` namespace properties, chunked upload v2 assembly semantics, `X-Request-ID`, etag semantics for incremental sync, error code mapping, and more. Each item depends on a large amount of undocumented client behavior. Such a compatibility layer costs several times more than implementing a WebDAV service, and it cannot be estimated.
2. **The compatibility layer would become the largest technical debt**: every minor client release can change probe behavior, and the project would have to follow. The goal is precisely to leave someone else's release cadence behind.
3. **At large capacity, the official client's full-scan behavior is a disaster** (see §8.3).

**No client compatibility does not mean no sync.** Users can still:

| Scenario                         | Approach                                                                      | In v1?                         |
|----------------------------------|-------------------------------------------------------------------------------|--------------------------------|
| Desktop file sync                | Third-party WebDAV clients (rclone mount, Cyberduck, Mountain Duck, GoodSync) | Yes; standard WebDAV is enough |
| Phone photo backup               | FolderSync (Android), Documents by Readdle (iOS)                              | Yes; standard WebDAV is enough |
| Two-way sync with a local folder | `rclone bisync` / `rclone mount`                                              | Yes                            |
| Windows File Explorer            | Mapped network drive (see §8.5)                                               | v1.1                           |
| A differentiated experience      | A newCloud PWA plus a thin "sync assistant" client (optional in v2)           | No                             |

**Extension point**: the WebDAV layer becomes a `compat` middleware that is not mounted in v1. If official client support is wanted later, implement `status.php` and `capabilities` first as a probe; the core does not change.

### 8.3 What large capacity demands of the architecture (far more important than the language)

> This section discusses problems that **any** large deployment encounters, not something required because this installation happens to be 40 TiB. Capacity planning for this installation is in §8.11.

Past a certain capacity, files **cannot be treated as "a tree under the user's directory"**, or backup, expansion, migration, and deduplication all break:

1. **Content-addressed blob storage** (`blob` layout): `sha256` → immutable object, sharded by hash (`ab/cd/abcdef...`). The directory tree users see is only `nodes` rows in the database.
   - Benefits: natural deduplication (one copy for many users, quotas counted separately), incremental blob backup, and migration that swaps the backend without touching metadata.
2. **The `tree` layout is an explicit trade-off**: it puts the directory tree on disk in exchange for readability, rsync, and manual recovery, at the cost of no deduplication, full-copy versions, and slow renames of large directories. Both layouts are implemented in [0003-storage-layout.md](0003-storage-layout.md) and pass the same contract test suite.
3. **Metadata must be independent of storage**: PostgreSQL holds the tree, permissions, shares, and audit; bytes live in the backend.
   - Past a certain capacity, **object storage (S3/MinIO/Ceph RGW) is less trouble than local disk**, but local disk must be a first-class citizen, so **the storage backend is an interface from day one**.
4. **Backup**: full backups are not feasible at large capacity. The design must be **incremental content + daily full metadata (PostgreSQL is only a few GB)**. Recovery targets follow §8.11.
5. **The audit table fails first**: at scale it reaches hundreds of millions of rows. Use **monthly partitioning + append-only + archive on expiry**; a single table will not hold.
6. **No `COUNT(*)` quota accounting**: `used_bytes` is maintained incrementally (`UPDATE ... SET used_bytes = used_bytes ± n` on upload, delete, and version change), with a periodic reconciliation job.

### 8.4 Single-file limits: 10 GB soft by default, 1 TB hard

No number needs to be final now, but one is needed to decide the chunking strategy:

| Per-file limit                                 | Required mechanism                                                                                                                 | Cost                        |
|------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------|-----------------------------|
| ≤ 5 GB                                         | A single PUT suffices                                                                                                              | Low, but users hit the wall |
| **≤ 100 GB** (soft ceiling, per-group maximum) | Chunked upload (tus or S3 multipart, **chunks of 64–256 MB**), resumable upload, parallel chunks, server-side hash verification    | Medium; mandatory in v1     |
| ≤ 1 TB (hard cap)                              | The same, plus attention to `Content-Length` precision, Zip64, and the 4 GB limit of the browser File API (bypassed with chunking) | Medium                      |
| > 1 TB                                         | Requires dedicated transfer tools                                                                                                  | Not recommended             |

**Recommendation**:

- Global hard cap **1 TB** (a technical ceiling against abuse);
- Default soft limit **10 GB**, raisable per group to 100 GB; over-limit requests return `507`;
- **Very large files (>10 GB) use a separate "large-file channel"**: chunking, instant upload, and resumable upload, with **no thumbnail generation and no full-text indexing by default** (otherwise background tasks run away at large capacity).

### 8.5 Windows mapped network drive: get the foundation right in v1, enable the feature in v1.1

Support in a later release is accepted, but this is not "adding a feature". It means the v1 WebDAV implementation must satisfy a set of constraints, or v1.1 has to rewrite it.

Hard constraints imposed by Windows WebClient (the mini-redirector):

| Windows requirement                                                                                               | Constraint on v1                                                                                                                           |
|-------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| **HTTPS with a trusted certificate chain** (self-signed certificates are rejected unless the registry is changed) | Proper certificates from v1, with an internal CA on private networks                                                                       |
| **Only Basic and NTLM; no Bearer or Cookie**                                                                      | v1 **must** have App Passwords and Basic authentication (already P0)                                                                       |
| **`Basic` credentials are resent on every request**                                                               | Password verification must sustain high frequency (low Argon2id parameters, or a memory cache)                                             |
| Default request timeout ~30 s and a 50 MB byte limit, adjustable through the registry                             | Large files must be chunked, and the server must accept "connect in seconds, write slowly"                                                 |
| `PROPFIND Depth:1` lists a directory in one request; missing properties show "size unknown"                       | **Dead property persistence and a correct `getcontentlength`** (already P0)                                                                |
| `LOCK` is required to edit Office files safely                                                                    | Locks **must be persisted to the database**, never memory-only (already P0; an in-memory lock fails on restart and with multiple replicas) |
| Sensitive to the `DAV:` response header on `OPTIONS`                                                              | `DAV: 1,2` + `MS-Author-Via: DAV`                                                                                                          |
| Sensitive to non-ASCII paths and Unicode normalization                                                            | Path normalization must apply Unicode NFC plus an optional case-insensitive match                                                          |

**Conclusion**: the v1 WebDAV P0 items (Basic authentication, lock persistence, dead properties, quota properties, HTTPS) already cover 90% of what Windows needs. **v1.1 adds only compatibility tuning, documentation, and a test matrix**, not a rewrite.

Alongside Windows drive mapping, v1.1 should also cover:

- **rclone and Linux mount compatibility tests** (a passing `litmus` run);
- end-to-end verification of **opening and saving Office files directly (WebDAV + LOCK)**;
- **installation scripts and group policy documentation** for drive mapping.

### 8.6 Still open

All items are closed. This round (the third) confirmed: storage is local disk + S3 only; online Office is interface only; the compliance baseline is set by this document; the language is locked to Go. Remaining undecided items will be recorded as implementation-time decisions in ADRs and no longer block M0.

### 8.7 Deployment shape and scalability (not an architectural premise)

> **A wording correction first.** An earlier version wrote "single-host Docker" as the architectural baseline and derived "no Redis, no distributed locks, in-process cache is enough" from it. That mistook **the scale of one installation** for **the capability ceiling of the system**, which is exactly why SFTPGo, Nextcloud, and ownCloud do not define themselves that way.
>
> The correct split: **the architecture is designed for "multiple replicas are possible"; the deployment starts "single host for now".** The difference is that code must not assume it is the only process. It does not mean adopting Kubernetes now.

**Architectural constraints that always hold (independent of deployment size)**

| Constraint                                                                      | Why it holds now                                                 | Cost of violating it                                                                   |
|---------------------------------------------------------------------------------|------------------------------------------------------------------|----------------------------------------------------------------------------------------|
| No **unique** state in process memory (sessions, locks, task queues)            | Every one of them has to be redone once a second replica appears | Lost sessions and stale locks amount to a rewrite                                      |
| Locks and tasks coordinated through the database (`lock_tokens`, `jobs` tables) | PostgreSQL row locks are correct across replicas by construction | In-memory locks **fail silently** across replicas and Office co-editing corrupts files |
| Caches accelerate, never hold truth                                             | An in-process LRU can be dropped at any time                     | Inconsistent caches turn into dirty data with multiple replicas                        |
| Audit and business writes in one transaction                                    | Identical on one host or many                                    | Retrofitting after scaling leaves a gap in historical data                             |
| Atomic publication of file writes (temp + rename)                               | Needed on one host too                                           | More dangerous with multiple replicas                                                  |

**Deployment simplifications allowed now (future upgrades stay in deployment)**

| Item                 | Current single host        | When to upgrade                                    | Upgrade cost                                                        |
|----------------------|----------------------------|----------------------------------------------------|---------------------------------------------------------------------|
| Application replicas | 1                          | Concurrency or availability insufficient           | Add replicas + shared sessions (already stateless)                  |
| Cache                | In-process LRU             | When replicas need consistency                     | Add Redis; the interface is already abstracted                      |
| Task queue           | outbox + in-process worker | Workers cannot keep up or need independent scaling | Start more worker processes (already uses `FOR UPDATE SKIP LOCKED`) |
| Object storage       | Local disk                 | Capacity or single-point requirements              | Switch `storage.backend: s3`                                        |
| PostgreSQL           | Single instance            | Read pressure or high availability                 | Add streaming replicas                                              |
| Sharding             | Not done                   | One database cannot cope                           | **The real difficulty**; see below                                  |

**The one thing that is genuinely hard to add later: database scale.**

A single PostgreSQL instance on reasonable hardware handles tens of millions of `nodes` rows and hundreds of millions of `audit.events` rows. The risk is not the row count but **large-directory queries** and **audit table bloat**, and both must be handled in v1:

- `nodes(parent_id, name)` uses a partial unique index; large-directory `PROPFIND` has streaming output and a threshold guard;
- `audit.events` is **partitioned by month from day one**; adding partitioning to a table with hundreds of millions of rows later is an outage.

Beyond that, no distributed refactoring is needed in advance. **A stateless architecture with database coordination** is the whole secret of scaling smoothly from small to large.

**Storage layout** (local-disk option):

```text
/var/lib/newcloud/
├── pg/                    # PostgreSQL data (preferably a dedicated physical disk)
├── blobs/ab/cd/<sha256>   # blob layout: content-addressed, two-level sharding
│   └── tmp/               # upload staging (must share a filesystem with the final area)
├── files/<owner>/<path>   # tree layout: directory mirror (choose one: blobs or files)
├── audit-archive/         # audit archive (monthly)
└── config/                # configuration files (including break-glass config, kept apart from the DB)
```

> **Critical implementation detail**: staging and final areas **must be on the same filesystem**, otherwise `rename(2)` degrades to a copy and I/O collapses at large file sizes. A startup self-check enforces this (a real rename probe for `blob/local`, and tmp inside the root for `tree`).

**About single points: a deployment problem, not a code problem**

If an installation runs on one machine, its availability ceiling is that machine. That is a **consequence of deployment choice**, not a product defect. The product guarantees that adding a second machine requires no code change.

1. **Backups are mandatory in every shape.**
   - PostgreSQL: `pg_basebackup` daily plus continuous WAL archiving (shipped to another machine or object storage). RPO can reach minutes.
   - Content: daily full backups are impossible at large capacity, so use `restic`/`borg` incrementally (content addressing suits incrementals and unchanged blobs are not resent; the tree layout relies on file-level deduplication).
   - **Restore drills are mandatory**: an untested backup is no backup. An M5 acceptance criterion is "restore from bare metal to a serving state and record the duration".
2. **Three ways to remove single points, cheapest first**:

   | Approach                                        | What changes       | Effect                                                                   |
   |-------------------------------------------------|--------------------|--------------------------------------------------------------------------|
   | PG streaming replication to a second host       | Deployment only    | The database is no longer a single point (RPO ≈ 0)                       |
   | Storage on S3/MinIO (or replicated local disks) | Configuration only | Content is no longer a single point                                      |
   | Two or more application replicas                | Deployment only    | The process is no longer a single point (sessions are already stateless) |

   None requires business code changes, which is exactly what the architectural constraints table in §8.7 protects.
3. **Operational requirements (independent of scale)**:
   - **Disks are consumables**: RAID6/RAIDZ2, or at least RAID10, at large capacity, with **20% free space reserved** (snapshots, versions, temporary files);
   - Monitoring is mandatory: disk usage, PG replication lag, time of last successful backup, and the blob scrub task.

**Deployment checklist (`docker-compose.yml` mental model)**

| Service                         | Description                                                                |
|---------------------------------|----------------------------------------------------------------------------|
| `web`                           | The newCloud Go single binary (API + WebDAV + embedded frontend assets)    |
| `postgres`                      | Single instance with daily backups and WAL archiving                       |
| `blobstore` (optional)          | MinIO when the S3 backend is chosen; otherwise omit and use a local volume |
| `clamav` (v1.1)                 | Virus scanning                                                             |
| `caddy`/`nginx`                 | TLS termination and reverse proxy (Caddy automates certificates)           |
| `backup` (sidecar or host cron) | PG backups + restic incrementals                                           |

### 8.8 Online Office: interface only (WOPI seam)

v1 does not build Office integration. To keep future integration from becoming a refactor, v1 leaves a thin seam:

- The three preconditions for WOPI — **file locks, versions, and short-lived download URLs** — already exist in v1 (LOCK, versions, signed URLs), so **no additional design is needed**.
- The only thing to fix in advance is the **authorization method** from the blob store to an external document service: a separate short-lived signed token (`?t=<jwt>&exp=`) pointing at the newCloud download endpoint, which OnlyOffice or Collabora uses to fetch the file. **Define this token type now**; connecting WOPI later merely adds a caller.
- Conclusion: v1 writes no Office-related code. It only guarantees that the download endpoint accepts a one-time signed token.

### 8.9 Compliance baseline (set by this document)

No explicit compliance requirement was stated, so the baseline follows a **pragmatic** principle: pass an audit without blocking delivery. **Only low-cost, high-value items, and nothing that requires a certification body.**

**Baseline (v1 must satisfy)**

| Item            | Requirement                                                                                                                                                        | Cost           |
|-----------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------|
| Audit           | All 30+ events, including anonymous share access; **12-month** retention; hash chain                                                                               | Included in v1 |
| Audit integrity | The application's database user has `INSERT` only, no `UPDATE`/`DELETE`; the daily chain head hash is written to an independent location (object storage or email) | Low            |
| Access control  | Least privilege, a single authorization entry point, administrator actions recorded                                                                                | Included in v1 |
| Identity        | No local password as the primary path (Pocket ID); break-glass exceptions require audit alerts                                                                     | Included in v1 |
| Transport       | Site-wide TLS 1.2+ and HSTS                                                                                                                                        | Low            |
| Data            | Backups are recoverable (quarterly drill records), versions and trash                                                                                              | Included in v1 |
| Logging         | No passwords, tokens, or file content; IP retention of 12 months (aligned with audit)                                                                              | Low            |
| Deletion        | Users can delete their own data; administrators can force-purge; **blobs are actually reclaimed within N days of deletion**                                        | Low            |

**Explicitly not done (avoiding over-engineering that burns schedule)**

- MLPS assessment and ISO 27001 certification (they require third parties; not a code problem)
- Chinese national cryptography (SM2/SM3/SM4); unless regulation explicitly requires it, standard algorithms are safer
- A data masking or classification engine
- Four-eyes approval workflows, unless the organization genuinely requires them
- **Legal hold and external-link approval**: commonly requested in enterprises but not raised here, so **both are demoted to P2**. If needed later, the audit table is already partitioned by month and queryable, so implementation is inexpensive.

**Compliance items that keep an interface**: audit export (CSV/JSON plus syslog/OTel push) becomes an `AuditSink` interface; SIEM integration later only adds an implementation.

### 8.10 Scale parameters versus product capability

**Product capability** (in code, shared by every installation):

- Any user count (limited by deployment resources, not by a constant in code)
- Any capacity (the storage backend is replaceable and metadata does not need restructuring because of capacity)
- Single-host, multi-replica, and multi-AZ deployment (stateless architecture + database coordination)
- WebDAV, sharing, audit, and six credential types

**Scale parameters** (in configuration and deployment documentation; they affect only this installation):

| Parameter         | Current value          | What it affects                              | Does changing it affect code? |
|-------------------|------------------------|----------------------------------------------|-------------------------------|
| User count        | ~2000                  | Whether multiple replicas are needed         | No                            |
| Total capacity    | ~40 TiB                | Disk purchases, backup strategy, volume size | No                            |
| Deployment shape  | Single-host Docker     | Availability ceiling, backup requirements    | No                            |
| Single-file limit | 10 GB soft / 1 TB hard | Chunk size, timeout settings                 | No; configuration only        |

**Why this distinction matters**: designing for "<2000 users / single host" produces code that keeps sessions in memory, keeps locks in memory, and uses a channel as the task queue. It works perfectly on one host and fails entirely the day a second machine is added, and it fails as **silent data corruption** (for example two processes each holding an "exclusive" lock on the same file).

The rule for this project:

> **No assumption in code that this is the only process. The deployment may still run one machine.**

The cost is near zero (coordination through the database). The benefit is turning "scaling out" from a refactor into a deployment change.

### 8.11 Scale and capacity planning for this installation

The following numbers are valid **only for the current deployment**; another installation may be completely different.

**Capacity planning** (how many disks 40 TiB of user data actually requires):

| Item                  | Estimate                   | Notes                                                                          |
|-----------------------|----------------------------|--------------------------------------------------------------------------------|
| Raw user data         | 40 TiB                     |                                                                                |
| Version retention     | ×1.5 → 60 TiB              | Assuming 10 versions / 30 days; the actual figure depends on rewrite frequency |
| Trash                 | ×1.1 → 66 TiB              | 30-day retention                                                               |
| Deduplication savings | 20–40% reduction           | Blob layout only; highly dependent on how much duplicate content exists        |
| **Planned capacity**  | **About 50–60 TiB usable** | Do not buy disks for 40 TiB                                                    |
| Per volume or bucket  | ≤ 20 TiB                   | Beyond that, failure recovery time is unacceptable                             |

**The tree layout has no deduplication benefit**; plan 66 TiB, which is the main cost of choosing tree.

**Scale reference lines** (signals that it is time for the next step, not hard limits):

| Metric                        | One replica still comfortable | Consider multiple replicas |
|-------------------------------|-------------------------------|----------------------------|
| Concurrent active users       | < 500                         | > 1000                     |
| Daily requests                | < 5 million                   | > 20 million               |
| Metadata rows                 | < 50 million                  | > 200 million              |
| Content capacity (local disk) | < 20 TiB                      | Move to S3 or add disks    |

## References

- RFC 4918 (WebDAV), RFC 6578 (sync-collection), RFC 7233 (Range), [tus resumable upload protocol](https://tus.io/)
- Probe surface needed for Nextcloud-compatible implementations: [karadav NEXTCLOUD.md](https://f-hub.org/Outmode/karadav/raw/commit/442d22eca5eb4de63e05db3709c62604e092b77c/NEXTCLOUD.md)
- Go WebDAV reference implementation: [golang.org/x/net/webdav](https://pkg.go.dev/golang.org/x/net/webdav)
- Rust WebDAV: [dav-server crate](https://packages.ecosyste.ms/registries/crates.io/keywords/webdav-server) ([Debian packaging review](https://dfsg-new-queue.debian.org/reviews/rust-dav-server/0.11.0-1))
- Existing options worth evaluating: [SFTPGo](https://github.com/drakkan/sftpgo)
- The WOPI protocol (online Office integration)
- **Pocket ID**: [Scopes and claims](https://pocket-id.org/docs/guides/scopes-and-claims), [SCIM configuration](https://pocket-id.org/docs/configuration/scim), [account recovery (one-time login code)](https://pocket-id.org/docs/troubleshooting/account-recovery), [groups claim refresh pitfall, issue #987](https://github.com/pocket-id/pocket-id/issues/987)
- WebDAV compliance testing: [litmus](http://www.webdav.org/neon/litmus/)
