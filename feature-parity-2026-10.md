# Feature-parity comparison: newCloud vs Nextcloud, ownCloud, Seafile

Date of comparison: **2026-10-25**, against working tree at commit `d219e52` plus uncommitted
changes (which include the external-mounts work in `internal/mount` / `internal/external`).

Competitor baselines: **Nextcloud Hub 26** (Feb 2026), **ownCloud Infinite Scale 8.x** (oCIS;
ownCloud Classic 10 is past end-of-life and treated as legacy), **Seafile 12** (CE, with Pro
features marked). Competitor columns are compiled from vendor documentation and release notes;
a `✗`/`◐` in a competitor column is worth re-verifying if it is decision-critical for you.

Legend: ✅ implemented · ◐ partial/limited · ✗ absent · 🔒 deliberate non-goal (ADR-0001 / limitations.md)

---

## 1. TL;DR — what is still missing

Ranked by how much it costs us against *all three* competitors:

1. **No sync client story.** No desktop/mobile client, no sync protocol (`REPORT`/`sync-collection`
   answered 501), no `capabilities`/OCS compatibility endpoints. Every competitor ships a client.
   Generic WebDAV mounting works, but there is no delta sync, no selective sync, no virtual files.
2. **No team-owned spaces.** Spaces are creator-bound folders with no per-space quota (stored,
   never enforced) and **no web UI at all**. NC has group folders, oCIS has project spaces
   (quota + roles + per-space trash), Seafile's libraries are natively team units.
3. **Collaboration metadata: zero.** No favorites, no tags, no comments, no in-app notifications,
   no activity stream (the Activity view is a filtered audit log). All three competitors have most
   of these; our mail package can send exactly one email (share verification codes).
4. **Search is filename-substring only.** No content/full-text search, no filters, no pagination.
   Nextcloud and Seafile Pro have Elasticsearch-based full text; oCIS at least filters by tag/type.
5. **External mounts are dead code in the shipped binary.** Handlers exist but no `/api/mounts`
   routes are registered, `main.go` never constructs the service, only the WebDAV provider exists
   (no FTP/SFTP/SMB), and the DAV surface ignores mounts entirely. NC/oC10 have external storage
   (SMB/SFTP/S3/WebDAV); wiring this would put us ahead of oCIS and Seafile core, which have none.
6. **No antivirus scanning** on upload/download (all three have it, Seafile CE excluded).
7. **No encryption option of our own**: no app-layer at-rest encryption, no client-side/E2EE
   option (Seafile's encrypted libraries, NC's server-side encryption + E2EE module). We only
   delegate to disk/S3 SSE.
8. **Server-side preview generation stops at PNG/JPEG/GIF thumbnails (320 px).** No PDF/office
   preview rendering, no video poster frames, no OCR, no transcoding. NC renders all of these.
9. **Enterprise identity gaps**: no LDAP, no SAML, SCIM key accepted but inert, no built-in 2FA
   (delegated to the IdP), no password policy, no share/link policy administration at runtime
   (runtime settings cover six size limits only).
10. **No import/migration path** (export exists; nothing comes back in), single-writer deployment
    only, no theming/branding, 2 UI languages, no guest accounts, no federation.

Where we are **ahead** of all three: a tamper-evident, hash-chained audit log with signed external
anchors and offline verification; fail-closed auditing; OOTB Prometheus metrics + 21 alert rules +
runbook + an automated restore drill; a single static Go binary; two contract-tested storage
layouts; and unusually hardened share links (Argon2id passwords, per-link throttles, one-time
mailbox-verification codes, uniform-404 token-oracle resistance, download caps charged per session).

---

## 2. Function-level matrix

### 2.1 Core file management

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Browse / list / grid / column views | ✅ | ✅ | ✅ | ✅ |
| Upload (single + multipart) | ✅ | ✅ | ✅ | ✅ |
| Resumable chunked upload, durable across restart | ✅ own protocol (parallel, out-of-order, durable range set) | ✅ chunking-v2 | ✅ tus | ✅ own protocol |
| Parallel/out-of-order chunks | ✅ (browser sends up to 3 at once; server reserves ranges) | ✅ | ✅ | ✅ |
| Download w/ Range, resume, conditional GET | ✅ | ✅ | ✅ | ✅ |
| Streaming ZIP of folder / multi-select | ✅ | ◐ (zip via client or apps) | ✅ | ✅ |
| Rename / move / copy incl. batch | ✅ (≤200, all-or-nothing subtree proof) | ✅ | ✅ | ✅ |
| Conflict handling (ETag preconditions in-write) | ✅ (conditional inside the SQL statement) | ◐ (etag checks) | ◐ | ◐ |
| File locking | ◐ exclusive WebDAV locks only, enforced on every write path; no shared locks | ✅ (web + office locks) | ✅ (web + office locks) | ✅ (server-side + office auto-lock) |
| Checksums exposed to client | ◐ per-version sha256 in JSON only; no `oc:checksums` / `X-OC-Checksum` | ✅ | ✅ | ✅ (fixed blocks) |
| Versioning | ✅ count+age+byte caps; **tree layout retains no addressable old bytes** (restore 409) | ✅ configurable expiry | ✅ (default 30) | ✅ per-file history, snapshots |
| Trash + restore | ✅ per-user, retention, trash *capacity* cap (507) — unique | ✅ retention | ✅ per-space | ✅ per-library |
| User-initiated "empty trash" | ✗ (purge only via worker) | ✅ | ✅ | ✅ |
| Per-user quota | ✅ enforced at upload | ✅ | ✅ | ✅ |
| Favorites / starred | ✗ | ✅ | ✅ | ✅ (starred files) |
| Tags / labels | ✗ | ✅ (system + free, automation input) | ✅ (tag manager, filter) | ✗ |
| Comments / discussions | ✗ | ✅ | ✗ | ✅ (file discussions) |
| Search | ◐ filename substring, no filters, no pagination | ✅ name + full-text (ES) + filters | ◐ name + tag/type filter, no content index | ◐ CE name-only; ✅ Pro full-text |
| Activity stream / recent changes | ◐ audit-log view only | ✅ activity app | ◐ (versions + notifications) | ◐ recent changes per library |
| In-app notifications (bell/push) | ✗ | ✅ (+ push app, emails) | ✅ | ✅ + emails |
| Photo albums / memories / media apps | ✗ | ✅ | ◐ | ◐ (gallery-ish) |

### 2.2 Clients and protocols

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| WebDAV server | ✅ RFC 4918 c1+c2 at `/dav` (Basic + app password) | ✅ (Sabre) | ✅ (reva) | ✅ (seafdav) |
| `Depth: infinity` PROPFIND | ✗ refused (DoS posture) | ✅ (configurable) | ✅ | ✅ |
| WebDAV sync-collection / RFC 6578 (`getctag`, `sync-token`) | ✗ 501 | ✅ | ✅ (ctag) | n/a (own protocol) |
| Desktop sync client | ✗ (generic WebDAV mount / rclone only) | ✅ virtual files, selective sync | ✅ (spaces as drives) | ✅ + "Drive" cloud-drive client |
| Delta / block-level sync | ✗ | ◐ file-level (etag+chunks) | ◐ file-level | ✅ content-defined chunking — its signature feature |
| Mobile apps | ✗ (mobile web) | ✅ | ✅ | ✅ |
| Capabilities discovery (`/capabilities`, `status.php`) | ✗ | ✅ | ✅ | ✅ |
| OCS API (share/provision/etc.) | ✗ 🔒 (ADR-0001 reserves a `compat` seam, unmounted) | ✅ | ◐ compat layer | ✗ (own web API) |
| Public REST API + spec | ✅ OpenAPI 3.1, 40 paths | ✅ | ✅ (CS3/Graph) | ✅ |
| Webhooks / outbound events | ✗ | ◐ (apps) | ✗ | ✗ |
| Upload protocol standard (tus) | ◐ own protocol today; **tus adopted as core scope 2026-10-25** (`core-scope-2026-10-25.md`) | ✗ (own) | ✅ | ✗ (own) |

### 2.3 Sharing

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Internal share user→user / group | ✅ via ACL grants + `/api/shared`, id-addressed reads (no share "records") | ✅ (share records, resharing) | ✅ (shares w/ roles) | ✅ (libraries/subfolders, rw/ro/cloud-edit) |
| Delegated resharing | ✅ ACL `share` perm; link dies with grant | ✅ | ✅ | ✅ |
| Public link: view/download | ✅ | ✅ | ✅ | ✅ |
| Public link: password | ✅ Argon2id + throttle | ✅ | ✅ | ✅ |
| Public link: expiry | ✅ TTL or absolute | ✅ | ✅ | ✅ |
| Public link: download caps | ✅ atomic, session-charged | ◐ (verify current core) | ◐ | ✅ |
| Upload-only / file-drop links | ✅ (never lists, server-enforced) | ✅ | ✅ | ✅ (upload links) |
| Link stable across rename/move | ✅ (node-id) | ✅ | ✅ | ◐ (links yes; subfolder shares fragile) |
| Per-link MIME allowlist / byte caps / rate limit | ✅ (all three) | ✗ | ◐ | ◐ |
| Email recipient shares w/ mailbox proof | ✅ one-time code, no link in mail (anti-phishing) | ◐ (password link share to email / guest) | ◐ | ◐ |
| Recipient *edit* links (edit in browser) | ✗ (perm vocabulary is `read`+`create` only) | ✅ (edit role) | ✅ (editor role) | ◐ (cloud-edit role) |
| Federated shares (server↔server, `user@server`) | ✗ | ✅ | ✅ (CS3/Sciencemesh) | ✗ |
| Guest accounts (external users with accounts) | ✗ | ✅ (guests app) | ✅/◐ (verify) | ✗ |
| Share notifications to recipient/owner | ✗ (no mail templates beyond codes) | ✅ | ✅ | ✅ |
| Link policy administration (defaults, forced password) | ✗ | ✅ | ✅ (password policy service) | ✅ (config) |
| Watermark / secure-view on links | ✗ | ✗ | ✅ (enterprise) | ✗ |
| "Shared with me" surface | ✅ | ✅ | ✅ (shares jail) | ✅ |

### 2.4 Spaces / team storage

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Team space concept | ◐ space = group-owned-grant folder **inside a creator's tree** | ✅ group folders app (ACLs, quota) | ✅ project spaces (first-class) | ✅ libraries (first-class unit) |
| Per-space quota | ✗ (column stored, never enforced) | ✅ | ✅ | ✅/◐ (library quota) |
| Per-space trash / lifecycle | ✗ | ◐ | ✅ | ✅ |
| Space survives creator's account deletion | ✗ (dies with owner) | ✅ | ✅ | ✅ |
| Member roles (viewer/editor/manager) | ◐ stored for display; authz via ACL bits | ✅ (advanced ACLs) | ✅ | ✅ |
| Spaces web UI | ✗ (API only; web client makes no `/api/spaces` call) | ✅ | ✅ | ✅ |

### 2.5 Editing & productivity (all 🔒 non-goals in our ADRs — listed for completeness)

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Online office / co-editing | ✗ 🔒 | ✅ Collabora/OnlyOffice built in | ✅ WOPI app provider | ✅ OnlyOffice |
| Chat / video calls | ✗ 🔒 | ✅ Talk (+ federation) | ✗ | ✗ |
| Calendar / Contacts / Mail (CalDAV/CardDAV) | ✗ 🔒 | ✅ | ✗ | ✗ |
| AI assistant (transcribe/summarize/OCR) | ✗ 🔒 | ✅ Assistant (local LLMs) | ✗ | ✗ |
| Wiki / notes / knowledge base | ✗ 🔒 | ✅ (Collectives, Notes) | ✗ | ✅ (sdoc Wiki — flagship in v12) |
| Whiteboards, forms, deck/tables | ✗ 🔒 | ✅ | ✗ | ✗ |
| App store / plugin API | ✗ 🔒 | ✅ 400+ apps | ◐ (extensions) | ◐ |

### 2.6 Previews & media

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Image thumbnails | ✅ PNG/JPEG/GIF, 320 px, process-isolated decoder, bomb guards | ✅ | ✅ | ✅ |
| Video posters / audio waveform | ✗ (browser-decoded) | ✅ (ffmpeg) | ◐ | ◐ |
| PDF thumbnail / server-side render | ✗ (browser viewer only) | ✅ (LibreOffice) | ✅/◐ | ✅/◐ |
| Office → preview/PDF conversion | ✗ 🔒 | ✅ | ✅ (via office) | ✅ (via OnlyOffice) |
| Text/code preview in browser | ✅ (allowlisted extensions, sniffed, sandboxed) | ✅ | ✅ | ✅ |
| OCR | ✗ | ✅ (workflow/AI) | ✗ | ✅ Pro (via search) |
| Video transcoding / HLS / adaptive | ✗ | ◐ (apps) | ✗ | ✗ |

### 2.7 Identity & access

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| OIDC login | ✅ the only front door (PKCE, nonce, rotation-tolerant, outage policy) | ◐ (user_oidc app) | ✅ core | ✅/◐ |
| LDAP / AD user backend | ✗ | ✅ | ✅ (embedded + external) | ✅ |
| SAML | ✗ | ✅ | ✅/◐ | ✅ |
| SCIM provisioning | ✗ (`auth.scim_token` inert; table exists) | ✅ app | ✅/◐ (verify edition) | ✗ |
| JIT provisioning + domain allowlist | ✅ (open/pending/deny; verified-email rule) | ◐ | ✅ | ◐ |
| Local accounts | ◐ break-glass admin only (CIDR, throttled, alerted) | ✅ | ✅ | ✅ |
| App passwords / tokens for clients | ✅ (ro/rw scopes only) | ✅ (per-app names) | ✅ | ✅ (app passwords) |
| Sessions: revoke, revalidation window | ✅ | ✅ | ✅ | ◐ |
| OIDC refresh / single-logout (front/back channel) | ✗ (no refresh; local logout only) | ✅ | ✅/◐ | ✗ |
| 2FA (TOTP/WebAuthn) | ✗ (delegated to IdP) | ✅ | ◐ (via IdP/embedded IdP) | ✅ TOTP |
| Password policy | ✗ | ✅ | ✅ | ◐ |
| Brute-force / rate protection | ◐ share + break-glass throttles; OIDC is the IdP's job | ✅ (firewall) | ◐ | ✅ |
| Group→role mapping from IdP | ✅ | ◐ | ◐ | ◐ |
| Roles beyond admin/user/guest | ✗ | ✅ (sub-admins etc.) | ✅ | ✅ Pro (role permissions) |

### 2.8 Security & compliance

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Audit log | ✅✅ **hash-chained, signed daily anchors, offline verify, prune-with-proof** — ahead of all three | ✅ (DB rows) | ✅ (events) | ✅ Pro |
| Audit fail-closed (no audit ⇒ no write) | ✅ | ✗ | ✗ | ✗ |
| Content-origin XSS isolation | ✅ (split origins, capability tickets) | ✗ (same-origin) | ◐ | ✗ |
| Server-side encryption at rest (app-managed) | ✗ (disk / S3 SSE only) | ✅ module | ✗ (storage-level, same stance as us) | ✗ server-side |
| Client-side / E2EE option | ✗ 🔒 | ◐ (E2EE module) | ✗ (secure view instead) | ✅ encrypted libraries |
| Antivirus (ClamAV etc.) | ✗ | ✅ app | ✅ | ✅ Pro |
| Ransomware rollback/protection | ◐ versions+trash only | ✅ | ◐ | ✅ |
| Retention policies on content | ◐ trash/version retention only | ✅ (workflow retention) | ◐ | ✅/◐ |
| Legal hold / WORM / eDiscovery | ✗ | ◐ | ✗ | ✗ Pro (audit-grade) |
| Security headers / CSP / cookie hardening | ✅ (`__Host-`/`__Secure-`, sandboxed previews) | ✅ | ✅ | ✅ |
| Vulnerability-disclosure / hardening docs | ✅ | ✅ | ✅ | ✅ |

### 2.9 Storage architecture

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Local filesystem backend | ✅ | ✅ | ✅ (decomposed posix) | ✅ |
| S3 / object storage backend | ✅ (fake+moto tested; real-endpoint interop unverified) | ✅ | ✅ | ✅ Pro (S3/Ceph/Swift) |
| Content-addressed dedup (whole-file) | ✅ blob layout | ✗ (unless object store) | ✅ (decomposed FS) | ✗ (block-level instead) |
| Block-level / intra-file dedup + delta | ✗ | ✗ | ✗ | ✅ — signature feature |
| Filesystem-mirroring layout (`ls`/`rsync`) | ✅ tree layout — unique | ✗ | ✗ | ✗ |
| External storage mounts (SMB/SFTP/FTP/WebDAV/S3) | ◐ **code exists but unwired** (WebDAV provider only) | ✅ | ✗ | ✗ |
| Integrity scrubbing | ✅ (cursor-swept, corrupt-vs-missing) | ✗ | ◐ | ✅ (seaf-fsck) |
| Orphan/GC with refcounts | ✅ | n/a | ✅ | n/a (blocks GC) |
| Version cost model | ✅ blob layout = near-free; tree = full copies, no restore | full copies | decomposed | blocks (cheap) |

### 2.10 Admin & operations

| Function | newCloud | Nextcloud | oCIS 8 | Seafile 12 |
| --- | --- | --- | --- | --- |
| Admin web console | ✅ users/groups/6 runtime settings/audit browse | ✅ full | ✅ full | ✅ full |
| Runtime-tunable settings | ◐ six size limits only | ✅ | ✅ | ✅ |
| Prometheus metrics OOTB | ✅ 19 families | ◐ (apps) | ✅ | ✗/◐ |
| Alert rules + runbook bundled | ✅ 21 rules | ✗ | ◐ | ✗ |
| Backup scripts + automated restore drill | ✅ (`newcloud drill` measures RTO) | ✗ | ✗ | ◐ scripts |
| Health/readiness probes | ✅ | ✅ | ✅ | ◐ |
| HA / scale-out serving | ✗ single writer (lease-enforced) 🔒 | ✅ (web tier + object store + Redis) | ✅ multi-instance | ✅ Pro cluster |
| DR story | ✅ WAL archiving + restic + drill (RPO ≤ 15 min) | operator-supplied | operator-supplied | operator-supplied |
| Multi-tenancy | ✗ | ✗ (per-instance) | ✗ | ✅ Pro |
| Theming / branding (logo, colors, name) | ✗ | ✅ | ✅ | ✅ |
| Web UI languages | ◐ 2 (en, zh-Hans) | ✅ 60+ | ✅ ~25 | ✅ ~15 |
| Account export | ◐ files+manifest (no versions/trash/ACLs), **no import** | ◐ | ◐ | ✅ Pro (GDPR export) |
| Update mechanism / migration tooling | ✅ embedded migrations, advisory-locked | ✅ UI updater | ✅ | ✅ |
| Update/import from competitors | ✗ | ◐ community tools | ◐ | ◐ community tools |

---

## 3. The gaps in plain words

### 3.1 Gaps inside our own stated scope (these contradict our own ambitions)

1. **External mounts are built but not shipped.** `internal/server/mounts.go` handlers are never
   routed, `cmd/newcloud/main.go` never constructs the mount service (`deps.Mounts` is nil, so
   every `externalPath` fork in `files.go` is dead), `deploy/config.example.yml` has no `mounts:`
   block, `docs/guides/limitations.md` has no row for it, and the WebDAV surface has no mount
   fork although ADR-0014 claims both surfaces consult `ResolveTarget`. Only the WebDAV provider
   exists; FTP/SFTP are named in defaults and the migration CHECK but have no code; SMB is
   explicitly refused.
2. **Spaces are half a feature.** API yes (`POST/GET /api/spaces`), UI no. Team-owned roots,
   per-space quota (`groups.quota_bytes` is stored and scanned, never enforced) and lifecycle are
   the documented missing half — and the current model ties a space's life to one person's account.
3. **The share permission vocabulary stops at `read` + `create`.** No recipient-edit links, no
   delete-for-others, no per-file grants inside a link share — the "remaining P1/P2 share modes"
   our own limitations page names.
4. **Mail is one template.** Share received? Link expiring? Quota warnings? Account events?
   Nothing emails anyone, and sends are synchronous best-effort with no queue or retry.
5. **Small but user-visible file-service gaps:** no empty-trash button, no favorites/tags,
   filename-only search with no filters, DAV quota properties stubbed to a 404 propstat,
   tree-layout versions recorded but not restorable, no checksums on the DAV surface for clients
   to verify transfers.
6. **Identity operations:** SCIM is a config key with no endpoint; no session-listing UI ("where
   am I logged in?"); app-password scopes are only ro/rw (no per-folder or per-app).

### 3.2 Table-stakes features every competitor has (scope decisions to revisit)

- Desktop sync client + efficient change detection (sync-collection at minimum; or ship a client).
- Antivirus scanning hooks.
- In-app notifications + share emails.
- Full-text search (or an explicit partnered answer like oCIS's "no index" stance).
- Server-rendered previews beyond small raster images.
- LDAP/SAML as *additional* backends (we are OIDC-only).
- Theming/branding and broader i18n.
- Guest accounts.
- Import tooling (even a one-way "import from WebDAV/Nextcloud export" would differentiate).
- Scale-out/read-replica deployment path (we are single-writer by design).

### 3.3 Signature competitor features we have no answer to (and whether we should)

| Competitor feature | Our position |
| --- | --- |
| Seafile block-level delta sync + content-defined chunking | No answer. Our blob layout dedups whole files only. Anything less than a client protocol makes this moot; if we ever build a client, chunk-level addressing is the differentiator to study. |
| Seafile client-side encrypted libraries | Deliberate non-goal (E2EE incompatible with server previews/sharing). Documented and defensible. |
| Nextcloud Talk/Groupware/Office/AI/whiteboards | Deliberate non-goal (ADR-0001). Integration seam (WOPI-style app provider) would be the compromise if ever needed. |
| Nextcloud workflow engine / automated tagging / retention automation | No answer; our runtime settings cover six size limits. A rules surface (tag + time + action) is the missing generalization. |
| oCIS project spaces maturity (quota, image, per-space trash, spaces-as-drives) | Partially planned (limitations row). The concrete deltas: group-owned roots, enforced quota, per-space trash, UI. |
| oCIS tus uploads | Deliberate no (ADR-0008); our own protocol is durable, restart-safe, and now supports parallel/out-of-order chunks, which many tus deployments do not. Low priority. |
| oCIS/Nextcloud federation (cross-server sharing) | Absent everywhere in our design; a large feature; decide if multi-org sharing is ever in scope. |
| Nextcloud app ecosystem / OCS compatibility | Deliberate non-goal; the reserved `compat` middleware seam is the escape hatch if third-party app ports ever matter. |

### 3.4 What we have that they don't (do not regress these)

1. **Cryptographic audit trail** — hash chain, v1→v2 versioned preimages, signed daily anchors
   stored outside the DB, `audit verify` offline, prune-with-proof, fail-closed writes, INSERT-only
   DB role. Nextcloud/ownCloud/Seafile audit capabilities are plain rows an admin (or DBA) can edit.
2. **Restore drill as a first-class command** (`newcloud drill`) with RTO verdict and a freshness
   metric; nobody ships that.
3. **Split-origin content serving** with short-lived capability tickets — structural XSS containment
   none of the three have.
4. **Share-link hardening**: Argon2id link passwords with pre-compare throttle counting, per-link
   rate limits, one-time mailbox-verification codes with no clickable link, uniform-404 token
   oracle resistance, download caps charged per session (Range resumes join one paid session).
5. **Two storage layouts behind one contract suite** — content-addressed dedup *or* an
   `ls`/`rsync`-inspectable mirror, chosen per deployment.
6. **Correctness engineering in the write paths**: conditional writes evaluated inside the row
   change, staged swap with rollback for COPY/MOVE overwrites, lock fencing on every mutating
   handler, durable resumable uploads surviving crash and graceful restart, operation journal
   (ADR-0013) with recovery/escalation duty.
7. **Single static binary** with embedded migrations, no PHP/Redis/cron-stack, honest failure
   modes (404-not-403 denial, `content_missing` vs 404 distinction).

---

## 4. Suggested priority order (if parity is the goal)

| # | Item | Why first |
| --- | --- | --- |
| 1 | Wire external mounts (routes, main.go, config, FTP/SFTP provider, DAV fork, docs row) | Code is ~90% written; it is currently invisible. Biggest win per line of code. |
| 2 | Spaces: group-owned roots, enforced quota, per-space trash, web UI view | The #1 competitive product gap; schema groundwork (`owner_group_id`) exists. |
| 3 | Notifications + mail templates (share received/viewed/expiring, quota warnings) + mail queue | Smallest step that makes sharing feel like a product rather than an API. |
| 4 | WebDAV `sync-collection`/`getctag` + `capabilities` endpoint | Unblocks efficient third-party sync tools and our own future client without OCS. |
| 5 | Favorites + tags + empty-trash + search filters/pagination | Cheap, expected everywhere, and tags are the input any future workflow/retention engine needs. |
| 6 | Antivirus hook (ClamAV daemon, scan on upload/download, verdict-audited) | Compliance checkbox in every procurement list. |
| 7 | Checksums on DAV (`X-OC-Checksum`/`oc:checksums`) + working quota properties | Client-visible integrity and quota reporting; data (sha256) already exists. |
| 8 | Full-text search (pg_trgm for names now; content index later) | We already run PostgreSQL; a name-index via trigram is an afternoon, content FTS a quarter. |
| 9 | Preview generation beyond raster thumbnails (PDF render, video poster) | Removes the most visible "feels unfinished" moment in the UI. |
| 10 | Guest accounts + share policy admin (forced password/expiry defaults) | Closes recurring enterprise-采购 objections without an app ecosystem. |

---

*Sources for version baselines: Nextcloud Hub 26 announcement (nextcloud.com / help.nextcloud.com),
ownCloud Infinite Scale 8.1 release notes (owncloud.com/blogs), Seafile 12 user manual
(haiwen.github.io/seafile-user-manual). Function-level claims about newCloud come from the four
code inventories taken on 2026-10-25 and cite `file:line` in the working tree.*
