# newCloud competitiveness — deep research analysis

**Date:** 2026-10-25 · **Tree:** `d219e52` + uncommitted (external-mounts work in progress)
**Companion document:** [`feature-parity-2026-10.md`](feature-parity-2026-10.md) — the function-level
feature matrix and gap inventory. This document adds the demand-side research, corrects one earlier
recommendation, scores the candidates, and sequences them into a roadmap.

> **Amendment (2026-10-25, owner decision):** the core-scope decision
> ([`core-scope-2026-10-25.md`](core-scope-2026-10-25.md)) adopts **Elasticsearch** for full-text
> search (superseding the PostgreSQL-tsvector recommendation in §6 M6.2 — the engine is made
> optional to preserve the single-binary posture) and adopts **tus** as the chunked-upload
> protocol (superseding ADR-0008's wire protocol; see core-scope §1). Roadmap §8 absorbs the
> core-scope waves.

---

## 1. Executive summary

1. **The landscape gained a fifth player.** OpenCloud (Feb 2025) is a fork of oCIS built by the
   ex-oCIS engineers after Kiteworks acquired ownCloud, backed by Heinlein Group and positioned on
   EU data sovereignty. The "lightweight Go file cloud" slot now has *two* well-resourced rivals
   (oCIS + OpenCloud). newCloud cannot win that slot on "lightweight Go" alone; it wins on
   **provable audit, correctness engineering, and ops-in-a-box** — the things neither fork has.
2. **One earlier recommendation was wrong and is corrected here.** RFC 6578 `sync-collection` is a
   CalDAV/CardDAV ecosystem mechanism; generic file clients — including Nextcloud's own desktop
   client — sync by **PROPFIND + ETag walks**. The must-have is not `sync-collection`; it is making
   the etag-walk path fast: **directory-ETag propagation, stable file ids, checksums, and cheap
   PROPFIND Depth:1**. Done right, we beat Nextcloud's own client at its most-complained-about job.
3. **The six must-haves** (POC-disqualifiers, evidence-backed): (1) fast sync surface for
   walk-based clients, (2) ship the already-built external mounts, (3) finish team spaces,
   (4) notifications + real mail, (5) antivirus scanning, (6) search that works — with PostgreSQL
   full-text as a **differentiator**, because Nextcloud's Elasticsearch path needs ~4 GiB RAM and
   days to index and we already run the database.
4. **Sequencing ≠ importance.** The scoring model (§6) ranks quick wins with existing groundwork
   first (mounts, trigram search, file-id/checksums) while the heavier spaces redesign proceeds in
   parallel.

---

## 2. Method and evidence base

Three inputs, kept separate so each claim can be traced:

- **Supply side:** the four function-level code inventories of the working tree (2026-10-25),
  cited as `file:line` in the companion matrix.
- **Demand side:** community complaint patterns (Nextcloud forum/GitHub/Reddit, oCIS forum),
  EFSS buyer's-guide checklists (OpenText, Kiteworks, Computerworld), dated 2018–2026.
- **Landscape:** vendor announcements and release notes for Nextcloud Hub 26, oCIS 8.x,
  Seafile 12, OpenCloud (§9 sources).

Competitor columns in any table reflect vendor documentation as of 2026-10; a `✗` in a competitor
column should be re-verified if decision-critical.

---

## 3. Market landscape 2026

| Player | Stack | Position | Strategic fact |
| --- | --- | --- | --- |
| Nextcloud Hub 26 | PHP + DB + Redis + apps | All-in-one collaboration suite (Talk, Groupware, Office, AI, 400+ apps) | Heaviest to operate; sync and search are its most-complained-about functions (§4) |
| ownCloud Infinite Scale 8.x (Kiteworks) | Go microservices | Corporate-backed successor to oc10 (oc10 is EOL) | Kiteworks acquisition (2023) pushed commercial focus; 8.0.x fixed share-metadata corruption; "no gated features" claim |
| **OpenCloud** (new) | Go, fork of oCIS | Community/sovereignty fork by the original oCIS engineers, Heinlein-backed, v1.0 Feb 2025 | Apache-2/AGPL mix; ex-ownCloud team; targets exactly our positioning |
| Seafile 12 (CE/Pro) | C/Python | Fastest sync engine (block-level delta); deliberately not a collaboration platform | Audit/AV/cluster/full-text paywalled in Pro |
| newCloud | Go, single binary | Files + sharing + provable audit, no PHP stack | Audit chain, restore drill, dual layouts — unique (§7) |

Implications:

- **Sovereignty is the tailwind** all five ride (EU exit from US hyperscalers, GDPR, TCO).
  It favors self-hosting generally; it does not favor us specifically.
- **The Go-file-cloud slot is contested.** oCIS and OpenCloud both have spaces, notifications,
  clients, and vendor/community funding. Differentiators that survive a feature war: things that
  are *architecturally hard to copy* — the hash-chained audit log with signed anchors, fail-closed
  writes, the restore drill, dual storage layouts. These are moats (§7); the must-have list exists
  to stop us losing deals *before* our moats get considered.
- **Kiteworks' commercial focus on oCIS** and OpenCloud's AGPL components leave room for a
  permissive (Apache-2), single-binary, audit-first alternative — which is precisely newCloud.

---

## 4. Demand-side evidence (what users and buyers actually say)

### 4.1 Nextcloud's most-complained-about functions — our opportunity map

| Complaint | Evidence | What it means for newCloud |
| --- | --- | --- |
| Desktop sync "checking for changes" is slow with many files; open issue since 2018 | nextcloud/desktop#691, #10560 (virtual files), r/NextCloud threads, help.nextcloud.com 102564/181139/213555 | Walk-based sync performance is the #1 pain in the category. A server whose directory ETags propagate changes upward and whose Depth:1 PROPFIND is index-backed can skip unchanged subtrees and win this |
| Full-text search requires Elasticsearch: ~4 GiB+ RAM, indexing takes days–weeks, breaks across versions | fulltextsearch#456, help.nextcloud.com 170077/167486/207088, TrueNAS forum, opsily.com | A built-in PostgreSQL search (trigram names now, tsvector content later) is a *differentiator*, not parity: zero extra infra |
| The stack needs constant tuning (PHP workers, Redis, DB) to feel fast | Contabo "fix slow Nextcloud" guides as a genre | Our single-binary story is credible exactly here; keep it true (no Redis, no job-runner, no cron sprawl) |
| Slow even on 10 Gbit LAN — client-side bottleneck | help.nextcloud.com 181139 | Server can't fix a slow client, but a lean protocol surface (fewer round trips per folder check) helps every client |

### 4.2 oCIS / OpenCloud friction — where the Go incumbents are beatable

- Setup friction still produces support volume (missing `config.json`, `OCS_URL` defaulting to
  `localhost:9200` tripping self-hosters) — central.owncloud.org 63582, r/selfhosted 1hlafny.
- **External storage:** oCIS dropped oc10's external-storage app; mounting SMB/SFTP backends is a
  recurring forum complaint. **Neither oCIS nor OpenCloud core has it.** Our mounts code — once
  wired — beats both and matches Nextcloud's app without an app framework.
- Share-metadata corruption fixes in 8.0.x show a still-stabilizing metadata layer; our
  correctness engineering (conditional writes in-statement, staged swaps, operation journal) is
  the answer to exactly this failure class.

### 4.3 Seafile

- Block-level delta sync remains the best sync engine in the category; we will not out-sync it.
- The compliance functions buyers ask for (audit, AV, full-text, cluster) are Pro-paid. An
  Apache-2 product with audit + AV + search in core undercuts the Pro tier's core argument.

### 4.4 What procurement checklists require (EFSS buyer's guides)

OpenText's EFSS buyer's guide, Kiteworks, Computerworld, GoAnywhere converge on: **SSO/MFA,
encryption, antivirus scanning, audit logging, legal hold, DLP-ish sharing policies, GDPR/HIPAA/PCI
alignment.** Mapping to us: SSO ✅ (OIDC-first), audit ✅✅ (ahead), encryption ◐ (delegated to
disk/S3), **antivirus ✗**, **sharing-policy administration ✗**, legal hold ✗. Two of the six
checklist lines are absent — both are in the must-have list; legal hold lands in Tier 2.

---

## 5. Analysis correction: what "sync support" actually means

Earlier analysis ranked "RFC 6578 sync-collection" as must-have #1. Research corrects this:

- RFC 6578 `sync-collection` is implemented broadly **in the CalDAV/CardDAV ecosystem** (DAVx5,
  Apple clients, Thunderbird — sabre/dav docs), **not** by generic file clients.
- File sync clients — Nextcloud's desktop client, rclone, most third-party tools — use
  **PROPFIND (Depth:1) + `getetag` walks** with local comparison; nextcloud/server#22584 discusses
  exactly this fallback. The Nextcloud client additionally relies on **stable file ids**
  (`oc:fileid`) so a rename is a rename, not delete+re-download.

Therefore the must-have sync package is:

1. **Directory ETag propagation** — a write anywhere in a subtree bumps the ETag of every ancestor
   collection, so a walking client can prune unchanged subtrees. (Today directory ETags are
   synthetic mtime-derived — `internal/dav/propfind.go:206-215` — and a deep change does not make
   the root's ETag change, forcing full walks.)
2. **Stable file ids exposed as WebDAV properties** (our node UUIDs, surfaced `oc:fileid`-style).
3. **Checksums on the DAV surface** (`oc:checksums`/`X-OC-Checksum`) — the sha256 already exists;
   only the property/header plumbing is missing.
4. **PROPFIND Depth:1 cost bounded by index, not directory size** — our own benchmarks flag
   listing as the operation that scales with directory size (`docs/guides/limitations.md`, row
   "Performance evidence").
5. *(Optional, later)* RFC 6578 for the niche clients that do use it; `REPORT` currently answers
   501 honestly.

Acceptance target, stated as a test: **a 100k-file tree with one changed file must be verifiable
by a walking client in O(changed path) requests, not O(tree)** — and the benchmark that proves it
belongs next to `bench_test.go`.

---

## 6. The must-have functions — analysis cards and scoring

### Scoring model

Five criteria, weighted; each function scored 1–5 (5 best; *risk* 5 = low risk). The score orders
**sequencing** (what to build first), not importance — a low score can still be strategically
vital (spaces) but argues for starting its *design* early and its implementation later.

| Criterion | Weight | Question it answers |
| --- | --- | --- |
| Adoption impact | 0.30 | Does its absence end a POC? |
| Differentiation | 0.20 | Does it win deals, or merely reach parity? |
| Effort economy | 0.20 | How much groundwork already exists? |
| Strategic fit | 0.15 | Does it reinforce single-binary / audit-first? |
| Risk | 0.15 | Chance of regressing the correctness story? |

### Cards

**M1 — Fast sync surface (dir-ETag propagation + file ids + checksums + PROPFIND index).**
Why: §5; the category's #1 complaint is walk slowness. Effort **M** (ancestor bump in the write
transaction — the machinery for conditional, same-statement updates already exists; watch hot-path
cost on deep trees; conformance on both layouts). Risk 3: touches the write path — must not
disturb the staged-swap/journal invariants; extend the shared conformance suite. Acceptance:
walk-pruning test above; `oc:fileid` stable across MOVE (we preserve node identity — pin it);
checksum header round-trip on PUT/GET. Competitive effect: **leapfrog** (beat NC's own client's
server-side experience).

**M2 — Ship external mounts.** Why: built-but-unreachable (handlers unrouted in
`server.go`; `main.go` never constructs the service; WebDAV provider only; DAV surface ignores
mounts); oCIS/OpenCloud core have no external storage at all. Effort **S** (wire routes, construct
service, config block, one more provider — SFTP — plus the DAV fork ADR-0014 already specifies).
Risk 4: read path mostly; keep cross-storage MOVE refusing as it does today
(`409`, `externalops.go`). Acceptance: mount creation audited; SSRF guard test already exists;
` limitations.md` row added. Competitive effect: **leapfrog** both Go rivals, parity+ with NC.

**M3 — Team spaces v1 (group-owned roots, enforced quota, per-space trash, UI).** Why: the #1
product gap; spaces currently die with their creator (`limitations.md:43` names the constraint —
`nodes_root_uniq` one-root-per-owner — and the rejected alternatives). Effort **L** (schema
decision first: group-owned roots; `owner_group_id` and `SpacesFor` exist; `groups.quota_bytes`
stored-never-enforced). Risk 3: quota accounting already survived the mixed-owner fixes of the
2026-10-25 review — reuse those helpers. Acceptance: space survives owner deletion; group quota
enforced at upload; per-space trash listing; `/api/spaces` visible in the web client. Competitive
effect: **parity+** (oCIS/OpenCloud parity; NC group-folders parity).

**M4 — Notifications + real mail.** Why: sharing without notification feels broken; mail package
has one template, synchronous, no retry (`internal/mail/mail.go`); all four competitors have
bell + email. Effort **M** (table + worker duty + 4 templates: share received, link expiring,
quota warning, AV verdict; SMTP retry queue rides the existing job queue). Risk 5: additive.
Acceptance: recipient of an email share gets the invitation without the owner hand-carrying a
link; expiry reminder fires once, audited. Competitive effect: **parity**.

**M5 — Antivirus (ClamAV/clamd).** Why: on every EFSS checklist (§4.4); all three competitors
have it (Seafile in Pro — undercutting it in core). Effort **S–M** (INSTREAM client; hook at
`publishFile` and at share-download gates; verdict → trash + audit event + owner notification —
which M4 provides). Risk 4: scanning must be bounded (deadline, size cap, isolation pattern
already exists in `internal/thumbnail/isolate.go`). Acceptance: EICAR upload quarantined, audited,
owner notified; clean file path unchanged. Competitive effect: **parity, checklist-closing**.

**M6 — Search that works (trigram now, content FTS later).** Why: search is filename-substring
with no filters or pagination today; NC's answer needs Elasticsearch. Phase 1 **S**: `pg_trgm`
GIN index + filters (type/size/date) + cursor pagination (contract already exists). Phase 2 **M**:
tsvector content index for text/PDF via an isolated extractor; rides the worker. Risk 4/3.
Acceptance: name search on 1M nodes < 100 ms indexed; phase 2 finds a document by a phrase in its
body, with indexing cost visible as a metric. Competitive effect: **differentiator**
("full-text search with no Elasticsearch" is a headline nobody else in the Go cloud slot has).

### Tier 1 (credibility, next two quarters): favorites, tags, empty-trash; server-rendered
previews (PDF render, video poster — reuse the isolated-child-process pattern); working DAV quota
properties (currently a 404-propstat stub); guest accounts; share-policy administration
(forced password/expiry defaults — the runtime-settings framework in `settings.go` is the seam).

### Tier 2 (enterprise doors, decide by segment): SCIM (the `scim_*` seam exists and our own
limitations doc names it as the proper deprovisioning fix); LDAP bridge or documented
AD-via-IdP pattern; retention/legal-hold on spaces (natural extension of audit-first; all three
competitors are weak here); theming/branding; more languages.

### Scores

| # | Function | Adoption | Diff. | Effort | Fit | Risk | **Weighted** |
| --- | --- | --- | --- | --- | --- | --- | --- |
| M2 | Ship external mounts | 4 | 4 | 5 | 5 | 4 | **4.35** |
| M6.1 | Search phase 1 (trigram+filters) | 4 | 3 | 5 | 5 | 5 | **4.30** |
| M1 | Fast sync surface | 5 | 4 | 3 | 5 | 3 | **4.10** |
| M6.2 | Search phase 2 (PG content FTS) | 4 | 5 | 2 | 5 | 4 | **3.95** |
| M4 | Notifications + mail | 4 | 2 | 4 | 4 | 5 | **3.75** |
| M5 | Antivirus | 4 | 2 | 4 | 5 | 4 | **3.75** |
| T1a | Favorites/tags/empty-trash | 3 | 2 | 5 | 4 | 5 | **3.65** |
| T2a | SCIM | 3 | 3 | 4 | 5 | 4 | **3.65** |
| M3 | Team spaces v1 | 5 | 3 | 2 | 4 | 3 | **3.55** |
| T1b | Previews (PDF/video) | 3 | 2 | 4 | 4 | 4 | **3.30** |
| T1c | Guests + share policy admin | 3 | 2 | 4 | 4 | 4 | **3.30** |

Read this as: **build M2 and M6.1 immediately (cheap, high leverage), start M3's schema design in
parallel because it is the longest pole, sequence M1 next with conformance-suite protection**, and
slot M4/M5 once the mail/worker plumbing from M4 lands. M3's mid-table score is an artifact of
effort, not importance — it is the single most consequential product gap and stays top of the
roadmap's second release.

---

## 7. Moats to defend (do not regress, do market)

1. **Cryptographic audit chain** — hash-chained, v1→v2 preimages, signed daily anchors stored
   outside the DB, offline `audit verify`, prune-with-proof, INSERT-only DB role, fail-closed
   writes. No competitor has any of it; OpenCloud/oCIS events are plain rows.
2. **`newcloud drill`** — restore drill with RTO verdict and freshness metric. Nobody ships this.
3. **Split-origin content serving** with short-lived capability tickets — structural XSS
   containment none of the three have.
4. **Correctness engineering** — conditional writes evaluated inside the row change, staged swaps
   with rollback, lock fencing on every mutating handler, durable resumable uploads, the ADR-0013
   operation journal. This is the answer to oCIS's share-metadata-corruption class of bugs.
5. **Single static binary, dual storage layouts** behind one conformance suite.

Marketing implication: the README's comparison table should lead with these, not with feature
parity — parity closes deals, moats win them.

---

## 8. Sequenced roadmap

**Release A — "reachable" (≈6 weeks):** wire external mounts (routes, `main.go`, config block,
SFTP provider, DAV fork, limitations row) · search phase 1 (trigram index, filters, pagination) ·
`oc:fileid` + checksum properties + working quota properties · empty-trash endpoint · favorites ·
*design doc for group-owned roots (M3 schema decision)*.

**Release B — "feels like a product" (≈1 quarter):** directory-ETag propagation + walk-pruning
benchmark (M1) · notifications + mail queue + four templates (M4) · antivirus with EICAR test
(M5) · tags.

**Release C — "teams" (≈1 quarter):** spaces v1 — group-owned roots, enforced quota, per-space
trash, web UI (M3) · search phase 2 content FTS for text/PDF (M6.2) · guest accounts · share
policy admin.

**Horizon (decide by segment):** SCIM · retention/legal hold on spaces · LDAP bridge pattern ·
RFC 6578 as a bonus · theming/i18n expansion · branding.

**Standing rule:** every release keeps `go test ./...`, the conformance suites (both layouts), the
doc-claim tests, and the drill green — the moats are only moats while they are true.

---

## 9. Sources

Landscape & competitors:
[Nextcloud Hub 26 announcement](https://help.nextcloud.com/t/nextcloud-hub-26-winter-reclaim-your-digital-autonomy/240540) ·
[Nextcloud Hub overview](https://nextcloud.com/hub) ·
[ownCloud Infinite Scale](https://owncloud.com/infinite-scale) ·
[oCIS 8.2.0 release](https://owncloud.com/blogs/ocis-8-2-0-release) ·
[oCIS 8.0.8 release (share-metadata fixes)](https://owncloud.com/blogs/ocis-808-release) ·
[Seafile 12 user manual](https://haiwen.github.io/seafile-user-manual/wiki/core_functions_overview) ·
[Seafile encrypted libraries](https://haiwen.github.io/seafile-user-manual/security_and_encryption/use_encrypted_libraries) ·
[wz-it.com comparison incl. OpenCloud (Oct 2026)](https://wz-it.com/en/blog/nextcloud-seafile-opencloud-owncloud-comparison) ·
[massivegrid enterprise comparison (Feb 2026)](https://massivegrid.com/blog/nextcloud-vs-owncloud-vs-seafile-enterprise-comparison) ·
[talos.tools Nextcloud vs oCIS (Apr 2026)](https://talos.tools/compare/nextcloud-vs-owncloud)

OpenCloud fork:
[opencloud.eu launch](https://opencloud.eu/en/news/opencloud-available-now) ·
[heise: ex-ownCloud devs, lawsuit threat](https://www.heise.de/en/news/Ex-ownCloud-devs-seek-new-start-at-OpenCloud-Owncloud-owner-wants-to-sue-10254438.html) ·
[opencloud-eu discussion #262](https://github.com/orgs/opencloud-eu/discussions/262) ·
[open-edge.io migration guide](https://open-edge.io/blog/owncloud-to-opencloud-migration)

Sync mechanism:
[RFC 6578](https://www.rfc-editor.org/info/rfc6578) ·
[sabre/dav WebDAV-Sync docs (client adoption)](https://sabre.io/dav/sync) ·
[nextcloud/server#22584 (etag-walk fallback)](https://github.com/nextcloud/server/issues/22584)

Nextcloud pain points:
[nextcloud/desktop#691 (sync slow, many files, since 2018)](https://github.com/nextcloud/desktop/issues/691) ·
[nextcloud/desktop#10560 (virtual files)](https://github.com/nextcloud/desktop/issues/10560) ·
[help.nextcloud.com 102564](https://help.nextcloud.com/t/sync-extremely-slow-with-large-number-of-files-on-desktop/102564) ·
[help.nextcloud.com 181139](https://help.nextcloud.com/t/nextcloud-desktop-client-very-slow/181139) ·
[fulltextsearch#456](https://github.com/nextcloud/fulltextsearch/issues/456) ·
[help.nextcloud.com 170077 (FTS slow)](https://help.nextcloud.com/t/fulltextsearch-index-with-groupfolders-terribly-slow/170077) ·
[help.nextcloud.com 207088 (FTS app breakage)](https://help.nextcloud.com/t/using-nextcloud-30-search-modules-become-more-and-more-incompatible/207088) ·
[TrueNAS forum (indexing "days to weeks")](https://www.truenas.com/community/threads/elasticsearch-for-full-text-search-in-nextcloud.87624) ·
[opsily.com (ES ~4 GiB RAM vs SQL backend)](https://opsily.com/blog/nextcloud-full-text-search) ·
[Contabo: fix slow Nextcloud](https://contabo.com/blog/fix-slow-nextcloud)

oCIS friction:
[central.owncloud.org 63582 (config.json)](https://central.owncloud.org/t/ocis-setup-missing-config-json/63582) ·
[r/selfhosted (OCS_URL default)](https://www.reddit.com/r/selfhosted/comments/1hlafny/) ·
[Cloudron forum thread](https://forum.cloudron.io/topic/2457/owncloud-infinite-scale)

Buyer checklists:
[OpenText EFSS buyer's guide](https://www.opentext.com/assets/documents/en-US/pdf/the-enterprise-file-sync-and-share-buyers-guide-en.pdf) ·
[Kiteworks security requirements](https://www.kiteworks.com/secure-file-sharing/enterprise-file-sharing) ·
[Computerworld EFSS buyer's guide](https://www.computerworld.com/article/3520801/buyers-guide-enterprise-file-sync-and-sharing-services.html) ·
[GoAnywhere compliance](https://www.goanywhere.com/solutions/enterprise-file-sync-and-share)

Codebase evidence: the four inventories of 2026-10-25, summarized in
[`feature-parity-2026-10.md`](feature-parity-2026-10.md) §2 with `file:line` citations.
