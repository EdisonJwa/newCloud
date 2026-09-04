# Functional requirements and architecture gap analysis (v0.2)

**Version:** v0.2 (design discussion draft) · **Research date:** 2026-10-21 · **Document type:** Product feature list, capability boundaries, technical risks, development priorities, and acceptance specifications

> **Scope note:** This document covers product features, protocols, system architecture, security, data reliability, operations, and acceptance only. It does not cover UI/UX, visual design, responsive layout, i18n, or accessibility.
>
> **Evidence standard:** "Already planned" means only that the capability appears in the product design list supplied by the owner; it does not mean the code implements it or that tests pass. "Recommended addition" does not mean a confirmed defect exists in the current implementation.

---

## 0. Executive summary

newCloud already has a clear initial position: an open-source, self-hosted file platform with OIDC single sign-on, WebDAV, controlled sharing, audit, and local or S3 storage. The largest gap today is not the number of missing features but the number of cross-module contracts that remain undefined.

**The six most important additions before release:**

1. **Unified authorization:** Personal namespaces, platform roles, resource ACLs, and external share credentials must form one enforceable permission model, and every entry point — web app, WebDAV, preview, thumbnail, version download — must follow it.
2. **Reliable transfer and concurrency:** Resumable upload, atomic commit, ETag and conditional writes, WebDAV LOCK, and cross-entry conflict handling must prevent silent overwrites and the exposure of incomplete files.
3. **Identity and credential lifecycle:** OIDC user deactivation, group changes, session and App Password revocation, and IdP outage policy must be explicit.
4. **Data consistency and recovery:** Commit, retry, cleanup, backup, and verified restore of object storage and metadata must form a closed loop.
5. **Share security semantics:** Email OTP, drop box isolation, download counting, link expiry, and revocation all need testable definitions.
6. **Operability:** Background jobs, queues, migrations, storage diagnostics, audit verification, alerts, and backup and restore are all required.

**Product scope recommendation:** Build the open and reliable file platform first. Do not add online Office, IM, AI assistants, complex workflow engines, or a first-party desktop sync client at this stage.

### Priority definitions

| Level | Definition |
| --- | --- |
| **P0 / Release blocker** | Affects security isolation, data integrity, protocol correctness, or basic operability; without it the product cannot be declared Production Ready. |
| **P1 / Core product** | Important features for daily use; some may slip in a safe technical preview, but all belong in a complete 1.0. |
| **P2 / Differentiation** | Enhancements that build long-term competitiveness on top of P0 and P1. |
| **P3 / Research** | Long-term directions that need separate analysis of cost, security boundaries, and maintenance obligations. |

---

## 1. Current feature baseline (per the supplied list, not treated as implemented)

| Area | Already planned | Still to define or add |
| --- | --- | --- |
| Authentication | Pocket ID or any OIDC provider, App Password, break-glass administrator | Subject identity, session revocation, deactivation propagation, credential scope, recovery policy |
| Multi-user | Independent namespaces, independent quotas, IdP group mapping | Resource ACLs, group sharing, lifecycle, quota accounting basis |
| WebDAV | RFC 4918 core methods, Range, conditional requests | Cross-entry lock semantics, atomic commit, interoperability, sync extensions |
| File management | Browse, search, drag-and-drop upload, rename, trash | Copy and move, bulk operations, conflict handling, resumable upload, recursive directory operations |
| Preview | Image, video, audio, PDF, text, and source preview; type allowlist and sandbox | Authorization for preview derivatives, resource budgets, parser isolation, cache revocation |
| Thumbnails | Server-side cache, PNG/JPEG/GIF decoded with the standard library | Cache lifecycle, failure retry, isolation and quotas |
| Sharing | Link, password, expiry, download limit, recipient OTP, drop box | Internal account and group sharing, effective permissions, revocation semantics, abuse protection |
| Audit | Hash chain, event filtering, CSV/JSON export, INSERT-only database role | External checkpoints, verifier, logging failure modes, redacted retention |
| Storage | Local or S3, blob or tree layout | Cross-storage consistency, reference counting, GC, directory safety, migration strategy |
| Search and history | Name and path search, version history and restore | Large-directory indexing, permission-filtered search, retention policy, disaster recovery |

**Important:** `Range` is HTTP GET semantics (RFC 9110), not a WebDAV method of its own, and "WebDAV is mountable" does not mean a complete offline sync client exists. [S3][S4]

---

## 2. Cross-module invariants (settle these before building features)

These invariants matter more than the number of pages or APIs.

| ID | Invariant | Release acceptance |
| --- | --- | --- |
| INV-01 | Every file read and write, search, preview, history access, and share access goes through the same object-level authorization | No entry point can bypass a resource or user boundary |
| INV-02 | All writes follow the same version, conditional request, and lock rules | Concurrent modification from the web app and WebDAV never produces a silent overwrite |
| INV-03 | File content becomes visible only after it is durably stored, verified, and committed in metadata | No partially written artifact is externally visible after fault injection |
| INV-04 | Share links and App Passwords are restricted credentials that can be revoked independently | Revocation takes effect within a defined time bound |
| INV-05 | File IDs and version IDs are separate from physical paths; rename and move do not change file identity | Every storage layout exposes the same public file semantics |
| INV-06 | Background jobs are idempotent, retryable, and able to identify leftover artifacts | Convergence is possible after a process interruption |
| INV-07 | Version history, trash, quota, and physical GC share verifiable reference and retention rules | Restore, cleanup, and deduplication never delete data by mistake |
| INV-08 | Recovery capability is proven by a real restore drill | Database, objects, configuration, and keys can be rebuilt consistently |
| INV-09 | Audit covers authorization decisions and sensitive operations and allows independent integrity verification | Missing events, reordering, tampering, and logging system failure are detectable |

Authorization uses a default-deny policy and is checked on every request. [S7]

---

## 3. Identity, sessions, and app credentials

### 3.1 OIDC baseline (P0)

| Requirement ID | Requirement | Priority |
| --- | --- | --- |
| ID-01 | Use **`(iss, sub)`** as the unique external identity key and map it internally to an immutable `user_id`; email must not be the primary key | P0 |
| ID-02 | Authorization Code Flow, with PKCE where applicable; validate issuer, audience, signature, lifetime, `state`, and `nonce` | P0 |
| ID-03 | OIDC Discovery, JWKS key rotation, and validation failure policy | P0 |
| ID-04 | JIT user creation on first login, with an administrative policy that allows or denies automatic account creation | P0 |
| ID-05 | IdP `groups` map to platform roles; unknown groups or missing claims must never grant elevated privileges | P0 |
| ID-06 | Revocation and demotion policy for existing sessions and App Passwords after user deactivation, group removal, or privilege reduction | P0 |
| ID-07 | Explicit rules for login, new session creation, existing session renewal, and revocation while the IdP is offline | P0 |
| ID-08 | A defined policy for file ownership, sharing, audit, and retained data after account deletion or disablement | P0 |
| ID-09 | Optional SCIM or admin API for automated account creation, deactivation, and group synchronization | P2 |

OIDC Core states that `(iss, sub)` is the stable identity pair that can be relied on, and the Pocket ID `groups` claim requires the corresponding scope to be requested. [S1][S2] Pocket ID checks on token refresh whether a user is disabled or removed from an allowed group, but **sessions and App Passwords issued by newCloud itself are not revoked as a result** and must be enforced separately. [S2]

### 3.2 App Passwords (P0)

- Each credential has an independent ID, name, creation time, optional expiry, last-used time, and revocable state.
- Generate a high-entropy secret from a cryptographically secure random source; the server stores only appropriate verification material, and plaintext never reaches the logs.
- Define **read-only, read-write, WebDAV, and automation** scopes explicitly, rather than granting every App Password full account privileges by default.
- An App Password must not authorize system administration or the creation of higher-privileged credentials (unless a restricted capability is separately granted).
- Support individual revocation, revoke-all, and bulk revocation after account deactivation; verify the revocation window under concurrent requests.
- Every credential is usable over HTTPS only; WebDAV clients typically carry app credentials in HTTP Basic, and transmission outside TLS is forbidden.
- Apply rate limits, failure audit, and brute-force protection.

### 3.3 Break-glass local administrator (P0)

- Independent of the external IdP, so necessary recovery operations remain possible while the IdP is unavailable.
- Separate from ordinary administrators; by default it holds only system recovery and configuration rights and **does not implicitly grant read access to every user's private files**.
- Strong authentication, offline custody of the credential, alerts after use, mandatory audit, and regular restore drills.
- Public network exposure, access scope, password reset, and lockout policy for this entry point require separate decisions.

**Acceptance examples:** An email address reassigned by the IdP to a new user must not inherit the original user's files; removal from the administrator group must remove administrative rights; a server-side session must actually be invalid after logout. [S1][S8]

---

## 4. Multi-user isolation and resource authorization

### 4.1 Recommended three-layer permission model (P0)

1. **Platform role:** decides platform actions such as account management, system configuration, and audit reads; it does not by itself imply reading personal files.
2. **Resource ACL:** decides operations on a specific file, directory, or space, and can arise from ownership, a direct grant, a group grant, or inheritance.
3. **Share capability:** a restricted, expirable, revocable external access capability granted by a link or a recipient OTP.

The internal permission primitives should at minimum distinguish `list`, `read`, `download`, `create`, `overwrite`, `rename`, `move`, `delete`, `share`, `manage_permissions`, and `view_history`. Preset roles may be simpler, but they must map onto these primitives.

### 4.2 The most important authorization rules

- `COPY`: verify source read permission, target create permission, conflicts, and quota.
- `MOVE`: verify source move and delete permission and target create permission; how inherited ACLs and share state change must be decided in advance.
- Recursive directory operations: must not check only the root directory and ignore specially protected files inside it.
- Version restore: restoring a historical version is a new write; verify current write permission and keep the existing history.
- File delete and permanent purge: distinguish ordinary delete, restore, permanent delete, and administrator retention policy.
- Permission inheritance: a parent directory ACL change needs a defined effect on already granted child permissions.
- Authorization queries must not depend on file paths the user can modify or on role fields supplied by the frontend.
- Any cross-user or cross-space cache and index must preserve the authorization boundary; a search hit must not reveal the existence of a file the caller may not access.

### 4.3 Quota definitions (P0)

Distinguish clearly:

- **Logical quota:** total size of the current versions of the files a user can see.
- **Retained usage:** historical versions, trash, incomplete uploads, and the accounting owner of shared space.
- **Physical capacity:** actual footprint after deduplication, compression, and storage replicas.

**Recommendation:** base the per-user quota on the easily explained logical quota first, and set separate server-side resource caps for incomplete uploads and retained versions. Do not conflate physical space saved by cross-user deduplication with a user's authorization or capacity quota.

---

## 5. File service and WebDAV

### 5.1 Core file operations (P0)

- Directory create, rename, copy, move, delete, and restore; file upload, read, overwrite, and download.
- Same-name conflict handling with explicit overwrite rules; no silent overwrite without confirmation.
- Unique file ID and stable version ID; identity is unchanged after a path rename or move.
- Large directories use pagination or cursors with stable ordering instead of listing every object at once.
- Recursive operations have idempotent handling, error aggregation, cancellation rules, and a traceable job ID.
- Path validation covers `..`, encoding and decoding differences, symlinks, case conflicts, Unicode normalization, and names that are illegal on each client.

### 5.2 WebDAV methods and protocol contract (P0)

| Category | Requirement |
| --- | --- |
| Discovery | `OPTIONS`, allowed methods, DAV level, and capability declarations are accurate |
| Metadata | `PROPFIND`, `PROPPATCH`, `207 Multi-Status`, live and dead properties |
| Files | `GET`, `HEAD`, `PUT`, `DELETE`, `MKCOL`, `COPY`, `MOVE` |
| Locks | `LOCK`, `UNLOCK`, lock tokens, timeout and refresh, Depth, and lock conflicts |
| Conditions | `ETag`, `If-Match`, `If-None-Match`, WebDAV `If`, and the correct status codes such as `412/423` |
| Partial reads | HTTP `Range`, `If-Range`, `206/416`, especially for media and resumable downloads |
| Recursive semantics | `Depth`, overwriting the target, directory recursion, and permission checks |
| Compatibility | Behavior regressions with clients such as Windows, macOS, Linux, rclone, and Cyberduck |
| Optional extension | RFC 6578 Collection Synchronization as a research item for incremental sync |

RFC 4918 is the core specification; RFC 9110 defines HTTP conditional requests and Range; RFC 6578 defines the WebDAV Collection Sync extension. [S3][S4][S5]

### 5.3 Unified concurrency rules (P0)

- The web app and its API, together with WebDAV, must enter the same write arbitration layer.
- A failed conditional write returns an explicit conflict instead of "the last write wins" without notifying the client.
- Lock semantics must apply across entry points; a lock that only the server internals understand is not full compatibility.
- Lock state must not become self-contradictory across a service restart, a lock timeout, MOVE or COPY, a version restore, or shared access.
- Version history is not a substitute for allowing concurrent overwrites.

### 5.4 Scope boundary between WebDAV and sync clients

| Capability | Current position |
| --- | --- |
| Remote file access and mounting | The core goal of WebDAV |
| Resumable upload | A separate upload protocol or capability, not provided automatically by RFC 4918 |
| Automatic local folder sync | A separate client with a sync state machine, not required for v1 |
| Conflict copies, selective sync, offline changes | Require separate specification in a future client project |
| Collection Sync incremental report | One possible foundation for future sync, not a replacement for a full client |

---

## 6. Upload, download, and derived file processing

### 6.1 Reliable upload (P0)

The recommended approach is a resumable upload channel independent of WebDAV `PUT`; evaluate **tus 1.0** or an equivalent, well-tested protocol first. [S6]

The following are required:

- Create an upload session bound to the destination parent directory, the authorizing subject, and the intended target.
- Chunk and offset validation, upload session expiry, idempotent retry, and the server state required for pause and resume.
- Limits on size, rate, concurrency, and temporary resource quota; protect a public drop box from exhausting disk.
- Verify content integrity in the completion phase, then expose the final file version atomically.
- If the target changed, revalidate ACLs, ETag, quota, link validity, and target conflicts at commit time.
- Interrupted incomplete uploads can be cleaned up; after a service restart they can be resumed or safely canceled.
- Direct-to-S3 upload and server-proxied upload produce the same final file semantics.

### 6.2 Downloads and download counting (P0)

- Ordinary downloads support Range, conditional responses, and streaming of large files.
- A share link's "maximum download count" cannot simply count HTTP requests, because one media playback or one resumable transfer may issue several Range requests.
- **Proposal: an `authorized_download_session`.** The download count is decremented atomically per successfully authorized logical download session, with a session TTL, binding to the file version, and replay protection. The exact accounting is an owner decision.
- When an S3 presigned URL is issued directly, strict "invalid after one download" semantics are not naturally available; AWS S3 documentation states explicitly that the same presigned URL can be reused for as long as it is valid. [S11]
- For shares that must be strictly revoked or counted, validate every request through the application gateway by default, or design an equivalent one-time authorization at the storage layer.

### 6.3 Safe previews and thumbnails (P0)

Already planned: image, audio, video, PDF, text, and source preview; PNG/JPEG/GIF thumbnails; SVG and HTML never inline. Keep these conservative defaults and add:

- Thumbnails, transcoding output, and caches are protected resources derived from the original file; they must not bypass authorization through a public, guessable permanent URL.
- Decoders and parsers run in isolated processes with timeouts and memory and pixel or frame budgets; restrict compression bombs, malicious media, and metadata parsing.
- Do not trust client MIME type or extension; detect allowed inline formats strictly and constrain how their content is processed.
- The processing queue is retryable, and failed results have a cooldown and reclamation policy; avoid exhausting resources through repeated retries of a malicious file.
- Access, invalidation, and GC of thumbnail and index caches are bound to the original file's permissions, version, and deletion state.
- Optional malware scanning and file quarantine; for anonymous drop boxes, provide at least a scanning or quarantine hook and a strict resource cap.

These controls are consistent with the OWASP file upload security guidance. [S9]

---

## 7. Sharing and collaboration

### 7.1 Planned external sharing (retained)

- Random, unpredictable share links.
- Password protection, absolute expiry, and a download count limit.
- One-time code verification for a recipient at a specified email address.
- Temporary "drop box" upload links.

### 7.2 Share semantics that must be completed (P0/P1)

| Requirement ID | Requirement | Level |
| --- | --- | --- |
| SHARE-01 | A link has its own ID, permission, invalidation state, and version or resource reference, with least privilege by default | P0 |
| SHARE-02 | Password and OTP verification have rate limits, an expiry, an attempt cap, and replay protection | P0 |
| SHARE-03 | The OTP is bound to the intended email address, so "knows the link" is not treated as recipient identity | P0 |
| SHARE-04 | Drop box Upload Only: cannot list directories, read files, or see other uploaders' content | P0 |
| SHARE-05 | Share uploads need limits on file count, per-file size, total space, rate, and retention period | P0 |
| SHARE-06 | The effect of link revocation and of file deletion, move, or permission change is explicit and covers caches and derived resources | P0 |
| SHARE-07 | Download counting is transactionally consistent and accounts for concurrent Range requests | P0 |
| SHARE-08 | Direct sharing of files or folders between accounts and between groups, with permission inheritance | P1 |
| SHARE-09 | Rules for re-delegation, transfer, and management rights of shared resources are explicit | P1 |
| SHARE-10 | A single view lists created and received shares with their actual effective authorization | P1 |
| SHARE-11 | Team-owned shared Spaces that do not depend on an individual account remaining active | P2 |

**Security policies to decide:** May a share link access historical versions? Does it follow a move? Does the link stay valid after permissions on the target directory are withdrawn? Who owns anonymously uploaded data, how is it counted against quota, and when is it deleted?

---

## 8. File versions, trash, and lifecycle

### 8.1 Required (P0)

- Every version has an unambiguous ID, creation time, content verification information, and actor reference.
- Restoring an old version creates a new version; existing history is not overwritten.
- Define what happens to versions, shares, and thumbnails after a file is deleted, and whether trashed data counts against quota.
- Support restore from trash, permanent deletion, and retention-period cleanup; prevent unauthorized restore into a directory the actor cannot write.
- Define the precedence between administrator forced retention and a user's "permanently delete".
- Versions, trash, shares, and in-flight downloads may all reference the same blob; physical GC must ensure that no valid reference remains.
- Cleanup jobs have audit and repeatable-execution semantics.

### 8.2 Configuration parameters to define

| Parameter | Description |
| --- | --- |
| `version_retention_days` | Maximum retention period for old versions; administrator override rules must be specified |
| `version_count_limit` | Version count or capacity limit per file |
| `trash_retention_days` | Default trash retention period |
| `upload_session_ttl` | Expiry for incomplete upload sessions |
| `share_max_ttl` | Maximum allowed share link lifetime |
| `public_upload_max_total_bytes` | Total upload volume accepted by one drop box |

**Note:** The table lists proposed configuration items, not agreed default values; specific numbers should follow a data retention policy review.

---

## 9. Storage abstraction, data consistency, and security

### 9.1 Recommended logical layering

```text
Identity / Session / Credential
             |
        Authorization
             |
     File Service API
             |
   Namespace + Metadata -------------- Audit Event / Outbox
             |
     Version / Lock / Quota
             |
    Transfer / Blob Lifecycle
             |
       Storage Adapter
          /      \
       Local     S3-compatible
          \      /
        Layout Adapter
        /          \
      Blob        Tree
```

This is a **responsibility diagram**, not a mandatory microservice topology; the MVP can be a single modular monolith process.

### 9.2 Shared blob and tree contract (P0)

- Expose one logical file ID, version ID, ETag, and namespace semantics externally.
- `blob` content addressing must handle reference counting, cross-version retention, transaction interruption, and GC completely.
- If cross-user physical deduplication is allowed, evaluate existence side channels, quota misrepresentation, and cross-account data lifecycle; **the MVP should limit deduplication to a single trust domain**.
- `tree` must state whether administrators may modify the directory directly outside the filesystem. If they may, design rescanning, conflict detection, and metadata repair; if they may not, write the operational restriction down explicitly.
- Prevent symlink escape, path traversal, directory races, and case-incompatible behavior.
- The two layouts may differ in performance and capacity characteristics, but must not provide different guarantees for file authorization or data integrity.

### 9.3 Data write state machine (P0)

Proposed:

```text
INITIATED -> RECEIVING -> VERIFIED -> STORED -> COMMITTED -> AVAILABLE
                  |          |         |          |
                  +----------+---------+----------+--> FAILED / CLEANUP
```

- Object persistence confirmation and the metadata transaction cannot simply be treated as one atomic transaction; design idempotent recovery and coordination.
- After metadata commit, event writes should use a transactional outbox or equivalent reliable delivery, avoiding a "write succeeded but audit or index events were lost" outcome.
- When the object is durable but the metadata commit fails, retain the temporary object and clean it up safely according to TTL and reference information.
- Keep object existence, metadata visibility, and user read permission strictly distinct.
- AWS S3 provides strong read-after-write consistency, but **that does not create a distributed transaction between S3 and the application database**; S3-compatible services must be verified individually. [S12]
- Clean up incomplete Multipart Uploads; AWS explicitly recommends lifecycle rules for leftover parts, which otherwise continue to incur cost. [S13]

### 9.4 Encryption and key management (P0/P1)

- TLS is required; the reverse proxy forwarding protocol and the trusted source list must be configured explicitly.
- Server-side encryption at rest for local disks and S3 is a configurable storage security policy; keys must not live in the same fault domain as an unprotected data backup.
- Define key rotation, encryption state verification, and the recovery boundary if a key is lost.
- **v1 makes no end-to-end encryption (E2EE) claim**, because server-side preview, deduplication, and full-text indexing conflict clearly with end-to-end confidentiality and require separate research.

### 9.5 Data import and export and migration (P1/P2)

- Export the files a user owns and the necessary metadata, so raw content is readable without the proprietary database.
- Plan database schema migrations, version rollback, and compatibility checks.
- Storage backend or layout migration needs validation, pause and resume, and a rollback plan before the switch; the MVP does not require online hot migration.

---

## 10. Audit and security events

### 10.1 Event scope (P0)

At minimum: authentication success and failure, session creation and revocation, App Password creation, use, and revocation, file download, upload, modification, deletion, and restore, share creation, use, modification, and revocation, ACL changes, role changes, administrative configuration changes, recovery operations, and audit verification failures.

Recommended fields: `event_id`, `occurred_at`, `actor_id`, `auth_method`, `source`, `action`, `target_id`, `target_version_id`, `result`, `reason_code`, `request_id`, `prev_hash`, `event_hash`. Store IP and device information only when necessary and define a retention policy; never record passwords, OTPs, full tokens, or full share links in the audit log.

### 10.2 Hash chain and verification (P0)

- Use canonical serialization and deterministic event ordering so the same event data produces the same digest.
- Define ordering control for concurrent writes; detect missing event numbers, chain breaks, and illegal reordering.
- Periodically export or sign audit checkpoints and store them in a trust domain **independent of main database administrator privileges**.
- Provide a standalone `audit verify` command and verify the correspondence between exported CSV/JSON and the original records.
- An INSERT-only database role reduces accidental deletion by the application layer but does not stop an attacker with higher database or system privileges from rewriting the chain.
- Optional WORM storage; AWS S3 Object Lock provides such a storage model, but equivalent guarantees must not be assumed for every S3-compatible implementation. [S10]

### 10.3 Audit failure modes (P0)

A policy is required: when the log database is full, a chain write fails, an external checkpoint is unreachable, or events are backlogged, which high-risk operations must fail closed and which only need a durable local buffer and an alert. The system must never report success to a user while losing an audit event that was promised as mandatory.

OWASP recommends logging application security events and testing log injection, resource exhaustion, write failure, and protection of access. [S14]

---

## 11. Reliable operations, deployment, and disaster recovery

### 11.1 P0 operational capabilities

| Area | Required capability |
| --- | --- |
| Configuration | Versioned configuration, validation before startup, separation of secret and ordinary configuration |
| Health probes | Liveness and Readiness; dependency diagnostics for storage, database, and queue |
| Background jobs | Job ID, idempotency, retry on failure, dead-letter or quarantine, timeout, cancellation, and recovery |
| Backup | Coordinated backup of file objects, metadata, identity mapping, configuration, and keys |
| Restore | Real restore into a new instance, permission and version consistency verification, defined RPO and RTO targets |
| Data integrity | Checksum and scrub, missing object and orphan object detection, repair reports |
| Observability | Request error rate, DAV conflict rate, upload failure rate, queue backlog, GC and audit verification metrics |
| Deployment | TLS and reverse proxy setup, trusted headers, storage directory permissions, least process privilege |
| Database | Migration, backup consistency, pre-upgrade checks, rollback on failure |
| Resource budgets | Limits for maximum file size, concurrent uploads, preview conversion, and background search and thumbnail load |

### 11.2 Restore verification is not the same as having backups

A restore drill covers at least:

1. Rebuild application configuration and the database in a clean environment.
2. Restore object and file data and the encryption keys (if enabled).
3. Verify file permissions, versions, trash, group mapping, and the audit chain.
4. Verify that concurrent writes near the backup point do not produce lost references.
5. Record the provable restore duration, recovery point, and remaining errors.

CISA recommends offline or encrypted backups tested periodically for availability and integrity. [S15]

### 11.3 Capabilities that must not be promised early

- Do not claim Active-Active or highly available multi-node writes before cross-node locking, sessions, job coordination, and consistency verification exist.
- Do not claim that any S3-compatible storage is fully equivalent before abnormal-path testing is complete.
- Do not claim "Dropbox-like bidirectional sync" before a stable change log and conflict state machine exist.

---

## 12. Feature priorities and release scope

### 12.1 P0 — release blockers

| Area | Deliverable required |
| --- | --- |
| Identity | Standard OIDC authentication; user ID mapping; deactivation and demotion policy; App Password and break-glass |
| Authorization | A single authorization model; cross-account and cross-group isolation; uniform authorization at every entry point |
| File operations | File and directory CRUD, COPY and MOVE, delete and restore, ETag and conflict handling, unified lock semantics |
| WebDAV | RFC 4918 required methods, core lock behavior, and verifiable compatibility |
| Transfer | Safe upload; resumable large files; integrity verification; atomic commit; Range download |
| Sharing | Security constraints, revocation, and abuse protection for the planned link, OTP, and drop box capabilities |
| Storage | The real support scope of Local and S3 and of the layouts, failure recovery, GC, and quota consistency |
| Versioning | Version restore, trash, and retention and reference rules |
| Preview | Type allowlist, safe processing, and authorization for derived resources |
| Audit | Required events, integrity verification, and logging failure policy |
| Operations | Background jobs, upgrade migration, health checks, and backup and restore drills |
| Security | TLS, privilege isolation, least privilege, upload attack surface, and rate control |

### 12.2 P1 — complete core product

- Internal user and group sharing with explainable effective permissions.
- Bulk copy, move, packaged download, and controlled recursive operations on files.
- Metadata search by type, size, and date beyond file name and path.
- Centralized share lifecycle management, plus user session and App Password management.
- File retention, quota, and improved audit event query and export.
- Administrator monitoring, notification and SMTP testing, data export tools, and runtime diagnostics.

### 12.3 P2 — extension and differentiation

- Team shared Spaces separate from individual ownership.
- Tags, saved searches, and virtual collections.
- Full-text search, optionally with a text or OCR indexing service.
- Webhooks and an Event API, plus user and data migration tools.
- Restricted text and Markdown editing, more preview formats, and pluggable handlers.
- RFC 6578 incremental sync support and its client compatibility tests.

### 12.4 P3 — separate research projects

- A desktop or mobile bidirectional offline sync client.
- Selective sync, automatic handling of conflict copies, and incremental block transfer.
- A complete independent threat model for end-to-end encryption and key recovery.
- Online multi-user Office collaboration, AI semantic file Q&A, and multi-region Active-Active.

---

## 13. Recommended implementation order and stage gates

| Gate | Research and implementation scope | Requirement to enter the next stage |
| --- | --- | --- |
| G0 — rule freeze | File ID, version, ACL, group, quota, share, concurrency, retention, and error contracts | Key design decisions signed off; conflict scenarios have deterministic outcomes |
| G1 — unified file kernel | Namespace, Metadata, Local Blob, versions, conditional writes, references, and GC | Basic file operations, multi-user isolation, and crash recovery pass |
| G2 — identity and WebDAV | OIDC, App Password, Lock, PROPFIND and PROPPATCH, HTTP conditional requests | Cross-entry conflicts and interoperability with real DAV clients pass |
| G3 — S3 and reliable transfer | S3 adapter, resumable upload, commit coordination, job recovery | Network loss, process interruption, and S3 error injection cause no data corruption |
| G4 — sharing and audit | OTP, drop box, download counting, credential revocation, hash chain and verification | Permission revocation, abuse testing, and audit verifiability pass |
| G5 — operations and release | Backup and restore, upgrade, metrics, installation and deployment, full security regression | Release acceptance matrix satisfied before Production Ready is allowed |

**Practical guidance:** every gate uses reproducible automated test evidence, not merely completed implementation code or a single manual test. G0 research can run in parallel, but large-scale parallel coding should not skip the core contracts.

---

## 14. Acceptance and fault-injection matrix that must exist

| Test ID | Scenario | Expected result |
| --- | --- | --- |
| T-01 | User A guesses user B's file ID, version ID, thumbnail, or download link | No access to content or private metadata |
| T-02 | An old session and App Password are used after removal from the administrator group | Privileges are reduced or access fails within the designed revocation window |
| T-03 | The web app and DAV overwrite the same file concurrently | Unified conditional write rules apply; there is never a silent overwrite |
| T-04 | The network drops at 80% of an upload, then the upload resumes | Confirmed chunks are not resent; verification passes after completion |
| T-05 | The storage write completes but the metadata commit fails | No visible partial artifact; the temporary object is eventually cleaned up |
| T-06 | The process crashes between the metadata commit and the event write | After retry, audit and job events are neither lost nor double-counted |
| T-07 | A restored file, the trash, and a historical version share one blob | GC does not delete content that is still referenced |
| T-08 | A share with a download limit receives concurrent Range requests | Counting is atomic; the limit cannot be exceeded or wrongly exhausted |
| T-09 | Email OTP brute force, replay, or reuse across links | Rejected at the threshold; the OTP is bound to the recipient and the share |
| T-10 | A drop box uploader tries to list a directory or read another uploader's file | Only the authorized upload operation is possible |
| T-11 | A revoked share is accessed through the thumbnail, a cache, or a presigned URL | The explicit revocation time bound is met; no bypass beyond the stated commitment exists |
| T-12 | Path traversal, symlinks, and case or Unicode name conflicts are attempted | No escape from the authorized namespace and no overwrite of an unexpected target |
| T-13 | An S3 multipart upload is unfinished and the process exits abnormally | The session can be repaired or safely reclaimed without unbounded resource use |
| T-14 | A corrupt media file, an oversized image, or a compression bomb is decoded | Resource budgets hold, the process is isolated, and the service stays available |
| T-15 | Audit rows are deleted, rewritten, or extended with an invalid event | The verification report detects a chain or external checkpoint mismatch |
| T-16 | Audit storage is unavailable or the disk is full | The previously agreed fail-open or fail-closed policy is followed |
| T-17 | The database, files, and keys are restored into a clean environment in another location | Permissions, history, and data integrity are restored; the actual RPO and RTO are recorded |
| T-18 | Mac Finder, Windows DAV, Linux mounts, rclone, Cyberduck | Every client in scope passes read, write, lock, and move regressions |
| T-19 | Items are continuously added, deleted, and renamed while a large directory is paged | No unauthorized access; duplicates and omissions follow the cursor consistency contract |
| T-20 | `COPY/MOVE` runs with a full target storage, changed permissions, or an expired link | The operation does not damage the source, and failures are diagnosable and recoverable |

**Note:** The `80%` in the table marks a fault-injection point, not an upload-resume performance promise; RPO and RTO values, revocation windows, and concurrency capacity must be set from measured scenarios.

---

## 15. Decisions pending with the product owner (not to be assumed by an agent)

| Decision ID | Open question | Suggested default direction |
| --- | --- | --- |
| D-01 | May administrators read private user files? | No by default; emergency access, if needed, requires a controlled grant and audit |
| D-02 | How are existing permissions handled when a group claim disappears or the IdP is unavailable? | No privilege escalation; define a bounded cache and revocation period |
| D-03 | What is the capability scope and default lifetime of an App Password? | Independent scopes, revocable, least privilege |
| D-04 | How is the download count for external shares counted? | An atomic authorized download session, not the number of Range requests |
| D-05 | What are the ownership and quota rules for drop box files? | Charged to the destination directory owner or space; separate temporary limits |
| D-06 | How are versions, trash, and incomplete uploads charged against capacity? | Logical user quota plus a server-side retention cap |
| D-07 | May the tree layout accept filesystem changes made outside the application? | Forbidden by default in the MVP; a detectable mode is separate research |
| D-08 | Are blobs deduplicated across users? | No cross-trust-domain deduplication in the MVP by default |
| D-09 | Does early production support a single node or multiple nodes? | Single node first, with no Active-Active commitment |
| D-10 | Which audit failures block business operations? | Tiered by operation risk and mandatory documentation |
| D-11 | What is the data retention and permanent deletion policy? | Distinguish user requests, administrator retention, and backup cycles |
| D-12 | What is the certification list and minimum capability for S3 compatibility? | Name the specifically verified backends; do not claim unlimited compatibility |
| D-13 | Are Local and S3 and Blob and Tree all production-ready in the same release? | Grant support status separately per real test matrix |
| D-14 | Must direct user and group sharing ship in v1? | Promote to P0 if the positioning is a team cloud drive; otherwise P1 |
| D-15 | Are a public file API and long-lived automation credentials offered? | Support only the necessary DAV and restricted App Passwords first |

These are questions that **require an explicit choice**, not a demand to make every item complex immediately. The owner may leave an item open, but the corresponding capability then cannot default to Production Ready.

---

## 16. Features not in the current main line

The following items are not rejected, but they would significantly expand the data model, the security attack surface, or the long-term maintenance burden:

- Online Office, real-time multi-user collaboration, IM, and video conferencing.
- AI file assistants without an explicit threat model, or third-party cloud document parsing.
- Full end-to-end encryption (requires re-balancing against server-side preview, search, sharing, and deduplication).
- A self-built plugin marketplace or an arbitrary third-party plugin execution environment.
- A complex workflow engine and a multi-tenant SaaS billing platform.
- Unverified multi-region active-active writes and strongly consistent cross-cluster filesystems.

---

## 17. Reference specifications and official material

Prefer material from protocol maintainers, standards bodies, project documentation, and security agencies. The links below were checked during research on 2026-10-21; source content may have changed since.

- **[S1]** OpenID Connect Core 1.0 — Stable Subject Identifier (`iss` + `sub`): https://openid.net/specs/openid-connect-core-1_0.html
- **[S2]** Pocket ID — Scopes, claims, and token lifetimes: https://pocket-id.org/docs/guides/scopes-and-claims
- **[S3]** RFC 4918 — HTTP Extensions for WebDAV: https://www.rfc-editor.org/rfc/rfc4918.html
- **[S4]** RFC 9110 — HTTP Semantics (conditional requests and Range): https://www.rfc-editor.org/rfc/rfc9110.html
- **[S5]** RFC 6578 — WebDAV Collection Synchronization: https://www.rfc-editor.org/rfc/rfc6578.html
- **[S6]** tus 1.0 — Resumable Upload Protocol: https://tus.io/protocols/resumable-upload
- **[S7]** OWASP — Authorization Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html
- **[S8]** OWASP — Session Management Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html
- **[S9]** OWASP — File Upload Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/File_Upload_Cheat_Sheet.html
- **[S10]** AWS — S3 Object Lock: https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html
- **[S11]** AWS — S3 Presigned URLs: https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html
- **[S12]** AWS — Amazon S3 Strong Consistency: https://aws.amazon.com/s3/consistency/
- **[S13]** AWS — Multipart Upload Cleanup: https://docs.aws.amazon.com/AmazonS3/latest/userguide/abort-mpu.html
- **[S14]** OWASP — Logging Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html
- **[S15]** CISA — #StopRansomware Guide (backup and restore verification): https://www.cisa.gov/stopransomware/ransomware-guide
- **[S16]** RFC 9700 — OAuth 2.0 Security Best Current Practice: https://www.rfc-editor.org/rfc/rfc9700.html

---

## 18. Final recommendation

newCloud does not lack a longer feature list. What needs to be frozen is eight contracts: cross-entry authorization, concurrency control, resumable upload, share limits, storage state, trash and retention, audit integrity, and disaster recovery.

**Recommended principles:**

1. Stabilize a single node, clear module boundaries, and testable data semantics before considering distributed deployment.
2. Reuse mature specifications (OIDC, RFC 4918, HTTP conditional requests, tus) instead of inventing protocols.
3. Keep feature claims and test evidence one to one; planned, implemented, integrated, and approved for production are different states.
4. Do not sacrifice the correctness of ordinary file operations to support every possible cloud-drive extension.
5. Base release gates on security, data integrity, recovery, and cross-client regression, not on a successful demo.

**Recommended product position:** an open, secure, reliable, independently deployable file and sharing platform; team spaces, richer search, and a sync ecosystem follow once the foundation is mature.
