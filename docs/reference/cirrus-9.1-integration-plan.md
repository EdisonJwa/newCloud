# Cirrus 9.1 → newCloud production integration plan

**Status:** Ready for Phase 0 decision gates · **Audited:** 2026-10-23 · **Baseline:** [`cirrus-9.1-r1`](../../design-studio/round-9.1-cirrus-ux/FREEZE.md)

**Audited tree:** `C:\Users\admin\Documents\edison-webcloud` at `6ed3f92b1634` plus the working-tree changes present on 2026-10-23 (the tree is not clean; the audit describes the working tree, including `web/openapi.yaml` and `web/src`).
**Audited surfaces:** `web/` (React 19 + TypeScript + Vite), `internal/server`, `internal/store`, `internal/authz`, `internal/share`, `internal/upload`, `internal/dav`, `migrations/`, `web/openapi.yaml`.
**Design authority:** `design-studio/round-9.1-cirrus-ux/` at freeze `cirrus-9.1-r1` (215 files, digest `3aaa2baf…f28f62d`). The prototype and its documentation win on appearance, copy and interaction. This document records every place production must differ and why.

---

## 摘要（中文）

1. **Cirrus 9.1 已冻结**：`FREEZE.md` 记录基线 ID、215 个文件的 SHA-256、69.8 MB 证据，并提供 `node freeze/verify.mjs` 校验；基线只覆盖设计资产，不覆盖测试结果和控制文件。
2. **后端基础比预期完整**：分块/断点上传、回收站与保留期、版本与恢复、ZIP、ACL/组、分享链接、邮箱分享、上传专用链接、下载会话计数、审计、内容源票据、App Password、管理后台都已存在，且有测试。
3. **前端需要换壳，不是换肤**：现有 Atrium 是列视图 + 顶栏六分区；Cirrus 是一列表 + 面包屑 + 三入口 + Inspector + 单 Viewer。路由目前只有 `replaceState`，没有 `pushState`/`popstate`，必须整体重写。
4. **设计假设了一个“团队工作区”，后端没有**：现有模型是个人根目录 + ACL 共享。建议 Phase 1 先把工作区解释为个人空间，删掉成员数/Team workspace 等不真实文案；团队空间（SHARE-11）单独立项。
5. **最大契约冲突是分享链接 URL 不可恢复**：后端只存 token 哈希，列表不返回 URL，而设计的“Links I manage”要能 Copy link。建议加密存储 token（独立密钥文件）并保留哈希查找；若否决，则改为“重新签发链接”并接受 UI 偏离。
6. **公开分享页有两处必须修的安全/隐私缺陷**：服务端在密码门之前返回 `label`/`hide_contents`，客户端在解锁前渲染 `landing.label`；两者都必须在 Phase 3 修掉并加回归。
7. **缺口集中在数据契约**：全库聚合、文件夹计数、服务端排序/筛选、收藏、最近打开、接收分享的 owner/time、分享 PATCH（访问模式切换/清空密码）、公开页 Download all、Viewer collection、按文件活动。计划新增约 15 个端点、5 个迁移。
8. **Viewer 基本是新建**：后端只区分 image/video/audio/pdf/text；表格、代码、设计、压缩包、PDF 页面渲染都要在客户端实现或诚实降级。建议 CSV/TSV 真渲染，XLSX 先降级卡片。
9. **现有能力不能丢**：Trash、Activity、Access/App Password、Admin 在 Cirrus 里没有主入口，必须在账户菜单/设置里有去处；邮箱分享的公开验证流程必须继续可用，否则既有链接会失效。
10. **计划分 6 个阶段**：0 决策（2–3 天）、1 基础（10–12 天）、2 Home+Files（13–15 天）、3 Sharing（10–12 天）、4 Viewer（8–10 天）、5 次级界面/i18n/a11y/移动端（9–11 天）、6 加固（5–7 天）。单人约 60–70 个工作日；两人并行可压到 7–8 周。

---

## 1. Verdict

The design is ready. The backend is stronger than the design assumes in storage, authorization and audit,
and weaker than the design assumes in **discovery metadata** (counts, collections, favorites, last-opened,
owner identity) and **link lifecycle** (URL recovery, access-mode changes, public archive download).
The current frontend is not a starting point for a restyle: it is a different information architecture
(Miller columns, permanent section list, no URL history), so the shell, routing and Files surface are a
rewrite while the API layer, i18n, dialogs and upload logic are reusable.

Three facts frame the whole plan:

- **Nothing in the prototype's P0 flow requires a new database engine.** Every gap is an additive table,
   a column, a query or a handler. No rewrite of `internal/store` or `internal/authz` is needed.
- **Four decisions precede code** (§2). Starting Phase 1 before them produces throwaway work in the
  workspace copy, the share inspector and the viewer.
- **The current tree is not clean.** `git status` shows modified `web/src`, `web/openapi.yaml`, `internal/`,
  and the whole `design-studio/` directory untracked. Phase 1 must begin from a reviewed commit; the
  baseline commit above is the audit reference, not a release tag.

### 1.1 What the audit found, in one table

| Area | Current state | Consequence for Cirrus |
| --- | --- | --- |
| Auth/session | OIDC + break-glass, `__Host-` cookie, SameSite=Lax, IdP revalidation | Reuse; fix public-page provider and 401 handling |
| File storage | Blob/tree layouts, S3, dedup, resumable uploads, trash, versions, ZIP | Reuse almost entirely |
| Authorization | Role + ACL (user/group) + share links + App Password scope; admin no-content | Reuse; add UI presets and two contract fixes |
| Shares | link/email/request, password, expiry, download/upload caps, rate, retention, recipients | Reuse; add URL recovery, PATCH transitions, public zip |
| Discovery | Listing is name-cursor only; search/recent exist; no all-files, counts, sort | New queries/endpoints needed |
| Audit | Append-only, hash chain, self-scoped browse, CSV export | Reuse; add target filter/names for per-file activity |
| Public pages | Server gate + JSON + content origin; client page exists | Reuse protocol; fix gate metadata leak, theme/i18n, add zip |
| Frontend shell | Atrium columns, 6 sections, `replaceState` only | Replace shell, routing, Files, Viewer |
| Design tokens | Atrium emerald, 3 themes, no large text | Replace with Cloud Blue token layer |
| Tests | Go backend suites strong; no frontend test runner | Add Playwright-core e2e + visual/a11y gates |

### 1.2 Audit method and evidence

- Read the route table (`internal/server/server.go:204-415`), every non-test file in `internal/server`,
  the store interfaces and PG/memory implementations, migrations `0001`–`0015`, `internal/authz`,
  `internal/share`, `internal/upload`, `web/openapi.yaml`, `web/src`, and the frozen prototype.
- Ran every Cirrus 9.1 suite against the frozen baseline in Chromium on 2026-10-23: acceptance 541/541,
  QA 240/240 (35 axe screens, 0 violations), share-identity 54/54, interaction 67/67, usability 36/36,
  navigation 149/149, text-audit 49/49.
- Ran `TestDocumentsCiteOnlyRealTests` after adding this document; it passes.
- Did **not** run PostgreSQL-only suites, a real S3 endpoint, real WebDAV clients, or a restore drill; this
  host has no Docker/PostgreSQL. The existing `TEST_DATABASE_URL` suites remain the gate for migrations.
- The working tree is not clean; this audit describes it as found, not a released commit.

### 1.3 Existing backend capabilities to reuse (do not rebuild)

| Capability | Where it already lives | Notes |
| --- | --- | --- |
| Resumable/chunked upload | `internal/upload`, `internal/server/uploads.go` | Durable across restart; client already has start/put/complete/abort |
| Single-shot upload | `internal/server/files.go` (`handleUpload`) | Multipart, quota and MIME checks |
| Trash/restore/retention | `internal/server/files.go`, `migrations/0002`, `0014` | `purge_at`/`retention_days` in payload |
| Versions/restore | `internal/server/files.go` (`handleVersions`, `handleVersionContent`, `handleRestoreVersion`) | `version_content_available` gates restore per layout |
| ZIP of a selection | `internal/server/transfer.go` (`handleZip`) | Authenticated, streams |
| Copy/move/batch | `internal/server/transfer.go`, `handleBatch` | Per-item results, partial failure |
| ACL grants | `internal/server/aclmanage.go`, `internal/store/acl*` | user/group, inheritance, 15 permission names |
| Groups/admin | `internal/server/admin_groups.go`, `admin_users.go`, `settings.go` | Admin-only, audited |
| Share links | `internal/server/shares.go`, `internal/share/share.go` | token hash, Argon2id password, expiry, caps, rate |
| Public gate | `internal/server/sharegate.go`, `shareverify.go`, `sharepages.go` | password + recipient OTP, per-share cookies |
| Download sessions | `internal/store/download_session*` | One logical download across Range requests |
| Audit | `internal/store/audit*`, `internal/server/audit*.go` | Append-only, hash-chained, self-scoped |
| Content origin | `internal/server/content.go`, config `usercontent_url` | Ticket per path, revocation recheck |
| Search/recent | `internal/server/search.go`, `recent.go` | Name-only search; recent by `modified_at` |
| Thumbnails | `internal/server/thumbnail.go`, `internal/thumbnail` | Process-isolated decode; `path=` or `id=` |

The detailed matrix in §4 marks each capability `EXISTS`, `ADAPT` or `MISSING` and gives the exact
production action.

---

## 2. Decision gates (Phase 0)

These are product/security decisions, not implementation details. Each has a recommendation so work can
start immediately if the owner accepts it.

| ID | Decision | Recommendation | Why it blocks |
| --- | --- | --- | --- |
| D-1 | **Workspace model**: real team workspace, or personal root presented as a workspace? | **Personal root for this integration.** Present the signed-in account as the workspace; remove members/team/role copy. Treat SHARE-11 (group-owned spaces) as a separate project. | Home metrics, "Owner" rows, "Private to …", All files, search scope, quota, and the workspace inspector all depend on it. |
| D-2 | **Link URL recovery**: store a recoverable token, or reissue on demand? | **Encrypt the token at rest** (AES-256-GCM, key file outside the DB, AAD = share id), keep `token_hash` for lookup, return `url` only to the owner. Add a rotate endpoint for key loss/compromise. | The design's Links I manage and Inspector are built around Copy link. Hash-only means a visible UX regression or a redesign. |
| D-3 | **Spreadsheet preview**: full XLSX renderer, or CSV/TSV only? | **CSV/TSV table in P0; XLSX/ODS get the honest fallback card** (name, type, size, Download). Revisit a parser only if product requires it. | The prototype's sheet is fabricated fixture data. Shipping it would be dishonest; a parser adds bundle weight and a new dependency. |
| D-4 | **Legacy surfaces**: where do Trash, Activity, App Passwords/WebDAV and Admin live? | **Account menu → Workspace settings**, with sub-routes; keep the existing capability and restyle with the new tokens. Do not add them to the three primary destinations. | Cirrus has no place for four existing capabilities. Deleting them is a product regression. |
| D-5 | **Email shares**: expose in the new create form? | **Keep the backend and public recipient-code gate working; hide "email" from the new create form in P0.** | Existing email links must keep opening. The design never specified the email form or the code screen. |
| D-6 | **Sharer identity on the gate**: show owner name before unlock? | **Show display name only** (no email unless the owner already shared it); this matches the prototype and is not protected content. | Requires a server field and a privacy sign-off; the current client deliberately shows no owner. |
| D-7 | **"Remove from my list" / "Report this link"**: implement or defer? | **Defer both** (no backend, no abuse workflow). Keep the prototype's honest unavailable/limitation posture. | They appear in the design but have no server contract. |
| D-8 | **Last-opened record**: server table or browser-only? | **Server table** (`node_opens`), so Home's "Continue" survives reload, device changes and cleared storage. | Home's resume band and the viewer's "remembered" state. |
| D-9 | **Thumbnail vs download permission**: is a thumbnail "read" or "download"? | **`download` by default.** If read-only previews are a product requirement, add a separate `PermPreview` that the non-admin read path grants and the admin branch does not; never use bare `PermRead`. Update `internal/server/thumbnail.go:62-68` and add the missing regression. | The code checks `PermRead` while its comment says download. With `PermRead`, an **administrator** — whose branch deliberately withholds `PermDownload` — can still fetch a thumbnail of any user's image by id. That contradicts the admin no-content rule. |
| D-10 | **Share access model**: keep `kind=request` as upload-only, or add an explicit access column? | **Keep kind/permissions**; `PATCH` may transition request↔link with validation. No migration for access mode. | The design's "View / Download" / "Upload only" control needs a server transition; a new column would be cleaner but is not necessary for P0. |

**Exit criteria for Phase 0:** D-1, D-2, D-4, D-8, D-9 signed off in writing (an ADR addendum or a
comment on this plan). D-3, D-5, D-6, D-7, D-10 may be accepted as defaults above.

---

## 3. Target architecture

### 3.1 Frontend shape

Keep React 19 + TypeScript + Vite, `openapi-fetch`, and the existing build/embed pipeline. Replace the
Atrium shell and Files surface; preserve the API layer and the dialogs' behaviour where it matches.

```text
web/src/
├── main.tsx
├── App.tsx                     route switch + providers
├── route.ts                    NEW: parse/serialize, push/replace, popstate
├── session.tsx                 NEW: me, auth state, 401 handling
├── data/                       NEW: typed hooks + cache/invalidation
│   ├── useListing.ts
│   ├── useCollection.ts
│   ├── useShares.ts
│   ├── useUploads.tsx
│   └── usePreferences.ts
├── shell/
│   ├── Shell.tsx               topbar, mobile tab bar, status bar
│   ├── TopBar.tsx              Home · Files · Shared, search, inspector, account
│   ├── AccountMenu.tsx         Appearance, Workspace settings, Storage, Shortcuts, Sign out
│   ├── SearchPanel.tsx         folders/files/links/actions
│   ├── Inspector.tsx           dock / bottom sheet, panes
│   └── Toast.tsx
├── views/
│   ├── Home.tsx
│   ├── Files.tsx               one listing + breadcrumb + filter/sort + selection
│   ├── Sharing.tsx             received / managed
│   ├── Viewer.tsx              one viewer, collection footer
│   ├── PublicShare.tsx         /s/{token}
│   └── Settings.tsx            trash / activity / access / admin sub-routes
├── components/
│   ├── FileRow, FolderRow, GridTile, KindTile, Thumb
│   ├── Breadcrumb, FilterChips, SortMenu, SelectionBar
│   ├── CreateLinkSheet, LinkInspector, PasswordForm, ExpirySelect
│   ├── Gate, UploadDrop, ShareFileList
│   └── EmptyState, ErrorState, Skeleton, ConfirmSheet
├── i18n.ts                     extend to every new string (en + zh-Hans)
├── styles/
│   ├── tokens.css              Cloud Blue roles + type/spacing/motion
│   ├── base.css
│   ├── shell.css
│   ├── home.css
│   ├── files.css
│   ├── sharing.css
│   ├── inspector.css
│   ├── viewer.css
│   ├── public.css
│   └── mobile.css
└── api.ts / api-types.ts / types.ts
```

**Routing without a dependency.** Add a small typed router (~200 lines) rather than `react-router-dom`:
the URL is query-based, the transitions are known, and the repository's zero-runtime-dependency argument
still holds. It must:

- parse `screen`, `space`, `file`, `from`, `tab`, `share`, `pane`, `q`, `sort`, `dir`, `view`, `kind`;
- `pushState` for navigation (Home/Files/Shared, folder entry, viewer open, share tab, committed search);
- `replaceState` for non-navigational state (selection, filter, sort, layout, inspector);
- restore **all** state from the URL on `popstate` and cold load;
- canonicalise invalid/unknown values (an unknown folder falls back to All files, not to a fixture);
- preserve `/s/{token}` as a separate path route, outside the app shell.

### 3.2 Production URL contract

The prototype's `?screen=` scheme is good enough to keep; it makes every state addressable without a
router library. Do **not** port prototype-only review switches.

| Param | Values | Authority |
| --- | --- | --- |
| `screen` | `home`, `files`, `sharing`, `preview`, `settings` | Route |
| `space` | `all`, `root`, `<folder-uuid>` | Files location |
| `file` | node UUID | Viewer file (requires `from`) |
| `from` | `folder:<uuid>`, `recent`, `search:<q>`, `shared`, `single:<uuid>` | Viewer collection |
| `tab` | `received`, `managed` | Sharing view |
| `share` | share UUID | Selected outbound link |
| `pane` | `details`, `versions`, `activity`, `share` | Inspector pane |
| `q`, `sort`, `dir`, `view`, `kind` | as in the design | Files state |
| `settings` | `trash`, `activity`, `access`, `admin` | Legacy surface sub-route |
| `theme`, `textsize`, `contrast` | preferences | Appearance (also localStorage) |

Rules: `sid`, `mode`, `access`, `requiresPassword` are **not** production URL state. A recipient page is
`/s/{token}`; an owner preview opens that URL in a new tab (or the same tab) after URL recovery. The
prototype's `?screen=public&sid=` is a review-only identity contract (see `docs/09`), not a production
route.

### 3.3 Backend shape

No service rewrite. Add handlers in the existing packages, keep `authz` as the only capability source, and
put every new query behind the store interface so both memory and PostgreSQL implementations are tested.

- `internal/server/files.go` — listing sort/filter, all-files, ancestors, counts, favorites.
- `internal/server/collection.go` (new) — the viewer collection contract.
- `internal/server/workspace.go` (new) — Home summary and last-opened.
- `internal/server/shared.go` — owner identity, shared_at, child metadata.
- `internal/server/shares.go` — PATCH transitions, rotate, URL field.
- `internal/server/sharepages.go` — gate payload privacy, post-gate metadata, zip, lock.
- `internal/server/auditbrowse.go` / `internal/store/auditread*` — target filter.
- `internal/store/favorite*`, `open*` (new) — memory + PG + conformance tests.
- `internal/store/share*` — encrypted token column support.
- `migrations/0016…0020` — additive only.

### 3.4 Content-origin flow

The design's viewer renders images, audio, video, PDF and text. The content origin is deliberately
**attachment-only** and `nosniff` (`internal/server/content.go:37-44`), so it is for **downloads**, not
for inline rendering. The production client must use two paths:

| Use | URL | Origin | Auth |
| --- | --- | --- | --- |
| Inline preview (`<img>`, `<video>`, `<audio>`, PDF iframe, text fetch) | `/api/files/download?path=…&inline=1` | App origin | Session cookie; server vets MIME/CSP |
| Download / Save as | `POST /api/files/content-ticket` → `{url}` from `server.usercontent_url` | Content origin | Short-lived ticket, account status rechecked |
| Shared-node preview | `/api/files/thumbnail?id=…` or `/api/shared/{id}/content` | App origin | Session + `PermDownload`/`PermRead` |
| Public download | `/s/{token}/download` (redirects to content origin when split) | Content origin | Share cookie/capability |

`api.ts` currently hardcodes same-origin URLs and never calls `content-ticket`; this is a real deployment
bug, not a nicety. Phase 1 must add a `contentUrl(path)` helper that calls the ticket endpoint when the
session reports a content origin, and uses inline URLs only for previews.

### 3.5 The one flow that touches every layer

```text
Open folder d1
  route ?screen=files&space=d1
  GET /api/files/?folder=<uuid>&sort=&dir=&limit=
  → {ancestors, items, total, filter_counts, cursor}
  render breadcrumb from ancestors, one listing, folder rows + file rows

Open file f1 from that folder
  route ?screen=preview&file=f1&from=folder:d1
  GET /api/files/collection?from=folder:d1&file=f1&sort=&dir=
  → {items, index, total, label, location}
  render viewer; filmstrip uses items; boundaries from index/total

Create link
  POST /api/shares/ {path, kind, label, password, expires_in_seconds, max_downloads}
  → {share, token, url}
  route ?screen=sharing&tab=managed&share=<id>&pane=share
```

---

## 4. Gap matrix

Legend: **EXISTS** = implemented and reusable with the same semantics; **ADAPT** = capability exists but
the contract, UI or data shape must change; **MISSING** = no production implementation.
Anchors are to the audited working tree.

### 4.1 Shell, routing, design system

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| S-01 | Three primary destinations Home · Files · Shared | `app/js/views/shell.js:67-84` | Six-item nav in `web/src/App.tsx:21-35,226-239` | ADAPT | Replace nav; move Activity/Trash/Access/Admin to account/settings | 1 |
| S-02 | Browser-history URL contract | `app/js/state.js:316-354,399-483,1619-1635` | `replaceState` only in `App.tsx:148-156`; no popstate | MISSING | New typed router; push/replace/popstate; deep links | 1 |
| S-03 | Mobile tab bar and section header | `app/css/mobile.css:136,225-266`; `shell.js:167-208` | No tab bar; one 760px block in `styles.css` | MISSING | Port responsive shell at 1180/1080/1040/960/860/820/520/380 | 5 |
| S-04 | Search panel with folders/files/links/actions | `shell.js:231-332`; `app.js:1059-1076` | `Palette.tsx` searches recent + files only | ADAPT | Extend server search to folders/paths; add link/action groups | 2 |
| S-05 | Account menu (appearance, workspace, storage, shortcuts, sign out) | `overlays.js:106-118` | Only theme/lang/logout buttons | ADAPT | Build menu; add shortcuts sheet; wire sign-out | 1 |
| S-06 | Inspector dock / mobile sheet with stable DOM | `app.js:331-412,557-741`; `inspector.js:534-562` | `Detail` aside in `Files.tsx:1153-1344`; no sheet | MISSING | New `Inspector` component; spring physics; panes | 4 |
| S-07 | Cloud Blue semantic tokens | `app/css/tokens.css:100-596` | Atrium emerald tokens in `web/src/styles.css:18-140` | MISSING | Replace token layer; keep semantic role names | 1 |
| S-08 | Type/spacing/radius/motion ladder | `app/css/structure.css:72-139` | Literal px values, two shadow tokens | ADAPT | Adopt `--type-*`, `--sp-*`, `--r-*`, `--dur-*` | 1 |
| S-09 | Light/dark/increased contrast | `app/css/tokens.css:100-409` | Three themes incl. `hc`; no `data-contrast` | ADAPT | Separate theme from contrast; add automatic `prefers-contrast` | 1 |
| S-10 | Large Text 1.18× | `app/css/structure.css:190-192` | Absent | MISSING | Add `data-textsize` and scale every type role | 1 |
| S-11 | Reduced motion/transparency | `app/css/tokens.css:417-427,588-596` | Reduced motion partially; no opaque material | ADAPT | Add token overrides and JS short-circuits | 5 |
| S-12 | Component CSS split | `app/css/*.css` (10 files) | One 2,372-line `styles.css` | ADAPT | Split by surface; delete dead classes/`--muted`/invalid byte | 1 |
| S-13 | Focus, 44px touch, skip link, live regions | `app/css/structure.css:276-330`; QA suite | Partial; no skip link; some icon buttons unlabelled | ADAPT | Port checklist; axe + touch matrix | 5 |
| S-14 | i18n for all new surfaces | Prototype is English-only | `web/src/i18n.ts` en/zh-Hans with parity check | ADAPT | Add ~300 keys; no string concatenation; both languages | 5 |

### 4.2 Home

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| H-01 | Compact resume band with content-adaptive preview | `home.js:45-169`; `home.css` | Home has no resume band; `Home.tsx:86-111` recents only | MISSING | New Home; CSS format diagrams; 94px band | 2 |
| H-02 | Honest "Continue" vs "Recently modified" | `ui.js:320-325`; `state.js:126-128` | No opened record | MISSING | `node_opens` + `GET/PUT /api/me/last-opened` (D-8) | 2 |
| H-03 | Recent rows with thumbs and collection entry | `home.js:171-182` | `api.recent(8)` rows open folder, not file | ADAPT | New row component; open Viewer with `from=recent` | 2 |
| H-04 | Shared-with-me column with sender/access/time | `home.js:188-200`; `sharing.js:56-106` | `/api/shared` lacks owner name/time/access label | ADAPT | Enrich `SharedNode`; map perms to Can view/edit | 3 |
| H-05 | Activity summary | `home.js:285-293` | `/api/audit/events` exists; action metadata sometimes lacks names | ADAPT | Map actions to sentences; add `metadata.name` where missing | 2 |
| H-06 | Upload in the page header | `home.js:249-252` | Upload only inside Files | ADAPT | Reuse upload provider with destination root | 2 |
| H-07 | Workspace metrics (used/quota/files/folders) | `home.js`; `data.js:6-20` | `/api/auth/me` has quota/used; no counts | ADAPT | `GET /api/workspace/summary` | 2 |
| H-08 | Workspace inspector/members | `inspector.js:492-532`; `account.members` | No group-owned space | MISSING | Drop members/team copy under D-1; show storage facts only | 2 |

### 4.3 Files

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| F-01 | One listing: folders then files, same row | `files.js:74-167`; `ui.js:272-276` | Miller columns, folders grouped first in each column | ADAPT | Rewrite `Files` as one list + grid | 2 |
| F-02 | Breadcrumb from real parent ids | `state.js:1797-1816`; `files.js:216-268` | Path only as column headers; no ancestor jump | MISSING | `ancestors[]` in listing; Breadcrumb component | 2 |
| F-03 | "All files" aggregation across the tree | `state.js:1726-1741`; `files.js:341,387` | No endpoint; only listing one directory | MISSING | `GET /api/files/all` with folders + paged files | 2 |
| F-04 | Physical root vs folder vs aggregation semantics | `state.js:85-89,1726-1741` | `path=""` root; no aggregation | ADAPT | Encode `space=all\|root\|uuid`; server resolves | 2 |
| F-05 | Filter chips with counts | `files.js:42-49,429-435`; `ui.js:287-303` | Client-only filter in deepest column | MISSING | Server filter `kind` + `filter_counts` | 2 |
| F-06 | Sort by date/name/size/kind, asc/desc | `files.js:55-62`; `overlays.js:95-105` | Server sorts by `name` only (`store/pg.go:218-238`) | MISSING | Server `sort`/`dir`, opaque sort-bound cursor | 2 |
| F-07 | List/grid switch | `files.js:454-461` | Yes, deepest column only | ADAPT | One list/grid for the location; persist `wc.view` | 2 |
| F-08 | Selection, range, select-all, keyboard traversal | `state.js:986-1042`; `app.js:1543-1577` | Split `sel`/`picked`, deepest column only | ADAPT | One selection model; stable focus; `aria-selected` from selection | 2 |
| F-09 | Drag a file onto a folder row | `app.js:1747-1872` | OS upload drag only; no move drag | MISSING | Pointer ghost + drop target; call `POST /api/files/move` | 2 |
| F-10 | Rename/move/new folder/trash with undo | `overlays.js:163-238`; `state.js:1162-1235` | Dialogs/APIs exist; undo not across reload | ADAPT | Keep APIs; optimistic row patch; undo = restore call | 2 |
| F-11 | Upload queue, progress, resume | `overlays.js:465-501` | Aggregate counter; no per-file rows/pause | ADAPT | Global upload provider; per-file rows; resume from `wc.pendingUploads` | 2 |
| F-12 | Cursor pagination | `tests/navigation.mjs`; API cursor | `Listing.cursor` never read (`api.ts:117`) | ADAPT | Implement load-more; fix `files.truncated` | 2 |
| F-13 | Folder item count | `files.js:96-108`; `ui.js` | No count in listing | MISSING | `child_count`/`file_count` per dir | 2 |
| F-14 | Favourites | `state.js:1240-1266`; filter chips | No API, no persistence | MISSING | `node_favorites` + `favorite` field + filter | 2 |
| F-15 | File-type tiles and thumbs | `files.css:462-479`; `ui.js:114-152` | Kind tiles + images exist | ADAPT | Port kind tile/CSS diagrams; use thumbnails for images | 2 |
| F-16 | Location column/path for aggregated rows | `files.js:341,394-395` | Recent/search return path; listing doesn't | ADAPT | Include `path`/`folder_name` in all-files rows | 2 |
| F-17 | Empty/loading/error states, 400ms skeleton | `files.js:270-330` | Partial; no 400ms delay | ADAPT | Standard states; one error surface | 2 |
| F-18 | Trash screen with retention | Not in prototype; current product has it | `/api/files/trash`, restore, `purge_at` exist | ADAPT | Settings → Trash; preserve capability | 5 |
| F-19 | OS file drop upload | `files.js:293` (copy only) | Exists in `Files.tsx:446-452,765-780` | EXISTS | Preserve; do not regress | 2 |
| F-20 | Multi-select Share / Download | `shell.js:143-160` | ZIP exists; multi-share doesn't | ADAPT | Download → zip; Share hidden or per-item (one link per item) | 2 |
| F-21 | Folder addressing by stable id | `state.js:108,414,1726-1741` uses folder ids | Listing resolves `path` only (`files.go:94-105`) | MISSING | Listing accepts `folder=<uuid>`; resolve and authorize by id; return `ancestors` | 2 |

### 4.4 Sharing — owner side

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| SH-01 | Two labelled views, URL-backed tab | `sharing.js:39-54`; `state.js:554-579` | Two local tabs in `Simple.tsx:25-58` | ADAPT | URL `tab`; new rows | 3 |
| SH-02 | Active/expired lists with usage and expiry | `sharing.js:109-190` | `OwnedLinks` table has uses/expiry | ADAPT | New list layout; meters; received counts | 3 |
| SH-03 | Copy link after reload | `inspector.js:389-395`; `sharing.js` | Token hash only, no URL in list (`shares.go:24-43,329-365`) | MISSING | D-2 encrypted token + `url`; or rotate | 3 |
| SH-04 | Six-section pre-creation form | `overlays.js:240-409`; `state.js:1307-1441` | Simple `ShareDialog` (kind/password/hours/uses) | ADAPT | Rewrite sheet; validation; live summary | 3 |
| SH-05 | Link name that follows the item until edited | `state.js:1307-1347` | No label input; API accepts `label` | ADAPT | Add name field; send `label` | 3 |
| SH-06 | Access View/Download vs Upload only | `state.js:1284-1287`; `public.js` | `kind=link` vs `kind=request` | EXISTS | Map to `kind`; UI copy; upload target must be a folder | 3 |
| SH-07 | Independent password, clear on toggle-off | `inspector.js:450-478`; `state.js:1456-1485` | API creates/updates password; clearing semantics absent | ADAPT | `clear_password` flag in PATCH; write-only UI | 3 |
| SH-08 | Expiry 7/30/90/never and download limit 25/100/unlimited | `state.js:1272-1282` | Dialog supports hours/max uses | ADAPT | Presets; send seconds/max_downloads | 3 |
| SH-09 | Change access mode after creation | `state.js:1499-1524`; docs/08 §4 | PATCH cannot change kind/permissions | MISSING | PATCH `kind`/`permissions` with confirmation | 3 |
| SH-10 | Share Inspector: URL, effective perms, usage | `inspector.js:364-490` | Detail shows links but no settings | MISSING | New LinkInspector; fetch one share | 3 |
| SH-11 | Revoke | `sharing.js`; `inspector.js:480-486` | API + button exist | EXISTS | Keep; confirm sheet | 3 |
| SH-12 | Email shares | Backend + old dialog | `kind=email`, recipients, OTP gate | EXISTS | Keep public gate; hide create under D-5 | 3 |
| SH-13 | Recipients count/list | `inspector.js:373` | API returns recipients for email only | ADAPT | Show count; link shares = 0 | 3 |
| SH-14 | Preview as recipient | `state.js:932-942` | Needs token; currently no URL | MISSING | Open real `/s/{token}` in a new tab after D-2 | 3 |
| SH-15 | "Shared" marker on file/folder | `inspector.js:153-169` | `share_count` markers exist | EXISTS | Surface in row/Inspector | 2 |
| SH-16 | Multi-item sharing | Claimed in `inspector.js:318`, absent | One share = one node | MISSING | Keep one-item; hide multi-share action | 2 |

### 4.5 Shared with me

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| R-01 | Received list: file, sender, access, time | `sharing.js:56-106` | `/api/shared` returns id/name/kind/size/mime/modified/owner_id/perms | ADAPT | Add `owner_name`, `shared_at`, mapped `access` | 3 |
| R-02 | Open a received folder/file | `sharing.js`; `public.js` | `/api/shared/{id}` + `/content` exist | EXISTS | Reuse; add preview/permissions on children | 3 |
| R-03 | Viewer collection "Shared with me" | `state.js:594-670` | No collection endpoint | MISSING | `from=shared` in collection API | 4 |
| R-04 | Remove from my list | Not in prototype (limitation) | Absent | MISSING | Defer under D-7 | — |
| R-05 | Can view / Can edit labels | `data.js:545-586`; `sharing.js:72` | Raw `read+list+download` string | ADAPT | Permission-preset mapping (§7.2) | 3 |
| R-06 | Child metadata for received folders | `sharing.js` rows | Children only id/name/kind/size (`shared.go:125-128`) | ADAPT | Add mime/preview/modified/perms | 3 |

### 4.6 Public recipient pages

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| P-01 | Generic password gate discloses no target | `public.js:151-174`; docs/09 §2 | Server gate returns `label`/`hide_contents` (`sharepages.go:64-71`); client renders `landing.label` before unlock (`ShareLanding.tsx:88-95`) | ADAPT | Minimal gate payload; render generic copy; regression | 3 |
| P-02 | Password verification, throttle, honest error | `sharegate.go`; `shareverify.go`; `share.go:313-372` | Server throttles 5/15min; client treats 429 as wrong password | ADAPT | 429 copy; no attempt count; test | 3 |
| P-03 | Email recipient code gate | `shareverify.go:76-151` | Server + current client support it | EXISTS | Preserve; restyle; no regression | 3 |
| P-04 | Upload-only + password matrix | `state.js:908-929`; docs/08 §1 | Server enforces gate before upload; client supports both | EXISTS | Keep four combinations tested | 3 |
| P-05 | Folder list + Download all | `public.js:112-142`; `public.js:220` | Per-file download only; no share zip | MISSING | `GET /s/{token}/zip`; count policy | 3 |
| P-06 | Single file Download | `shareDownloadURL` | Server + client exist | EXISTS | Use content origin when split | 3 |
| P-07 | Landing metadata (owner, scope, size, expiry, downloads left) | `public.js`; `publicFromShare` | Landing lacks owner/expiry/size; client hides expiry | ADAPT | Extend post-gate payload; show after unlock | 3 |
| P-08 | Upload rules and remaining slots | `public.js:192-247` | Partial: max_uploads/count; max bytes/MIME ignored by client | ADAPT | Render rules; compute remaining; refetch count | 3 |
| P-09 | Resumable public upload | Not in prototype | Single multipart only | MISSING | P1: chunked share upload or size cap | 5 |
| P-10 | Unavailable state, no fixture fallback | `public.js:179-190`; docs/09 | Current client shows unavailable card | EXISTS | Keep | 3 |
| P-11 | "Unlocked for this session · Lock again" | `public.js:106-110` | No lock endpoint; cookie HttpOnly | MISSING | `POST /s/{token}/lock` clears cookies | 3 |
| P-12 | Report this link | `public.js` | No endpoint/workflow | MISSING | Defer under D-7 or remove from UI | — |
| P-13 | Theme/language on public page | Not in prototype (QA only) | `ShareLanding` outside I18n/theme providers (`App.tsx:70-75`) | MISSING | Move providers above route switch; add toggle | 3 |

### 4.7 File Viewer

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| V-01 | One viewer for every entry point | `app.js:1579-1741`; docs/07 §1 | `Viewer.tsx` + duplicate `Stage` in `Files.tsx:1346-1381` | ADAPT | One component; remove duplicate | 4 |
| V-02 | URL-addressed collection and history | `state.js:594-812,1650-1683` | Viewer is local state; no URL | MISSING | Route `file`/`from`; push on open, replace on step | 4 |
| V-03 | Collection builder for folder/recent/search/shared/single | `state.js:594-670` | No server collection contract | MISSING | `GET /api/files/collection` (§5) | 4 |
| V-04 | Image/video/audio/PDF rendering | `renderer.js:36-260` | Video/audio are text notes; PDF is a label (`Files.tsx:1368-1379`) | ADAPT | Real `<img>/<video>/<audio>/<iframe>`; inline URLs | 4 |
| V-05 | Code/text rendering | `renderer.js:158-199` | Text fetch truncated to 2,000/100,000 chars | ADAPT | Full text with size cap; line numbers; no highlighting dependency | 4 |
| V-06 | Spreadsheet rendering | `renderer.js:120-155` | CSV is text; XLSX no preview | MISSING | CSV/TSV table; XLSX fallback (D-3) | 4 |
| V-07 | Archive/design/unknown fallback card | `renderer.js:248-305` | Generic "No preview" note | ADAPT | Honest card with name/type/size/Download | 4 |
| V-08 | Filmstrip, Previous/Next, boundaries, live position | `preview.js:85-179` | Filmstrip exists; boundaries/announcements partial | ADAPT | Port footer rules; disable at edges | 4 |
| V-09 | Zoom/pan and mobile swipe | `app.js:750-863`; `state.js:1610-1613` | Zoom absent; no swipe | MISSING | Port pointer/touch handlers; 48px threshold | 5 |
| V-10 | Thumbnail for shared nodes | `thumbnail.go` `id=` | Exists | EXISTS | Use `id` for shared rows | 4 |
| V-11 | Direct file link / cold reload | `state.js:1650-1683` | Path only; no file id route | MISSING | Resolve by id in collection API | 4 |
| V-12 | Versions/activity inside viewer | `inspector.js:190-273` | API versions exists; audit per file missing | ADAPT | Add target filter; reuse Inspector | 4 |

### 4.8 Inspector, metadata, legacy surfaces

| ID | Capability | Cirrus evidence | Current implementation | Status | Integration action | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| I-01 | File inspector: open/share/info/actions | `inspector.js:109-184` | `Detail` exists in the last column | ADAPT | New dock/sheet; reuse actions | 4 |
| I-02 | Owner, description, tags | `inspector.js:140-184` | Owner=self; description/tags absent from `nodes`/`FileNode` | MISSING | Omit empty sections in P0; optional `description/tags` P2 | 4 |
| I-03 | Versions list and restore | `inspector.js:65-84` | `/api/files/versions` + restore exist | ADAPT | Show version, author, date, size; gate restore on `version_content_available` | 4 |
| I-04 | Per-file activity | `inspector.js:86-101` | Audit has no target filter | MISSING | `target` filter + index; map action copy | 4 |
| I-05 | Share section on a file/folder | `inspector.js:153-169` | `share_count` markers; Detail lists links | ADAPT | Show "Shared · N links"; open LinkInspector | 4 |
| I-06 | Multi-selection inspector | `inspector.js:275-322` | Selection bar exists | ADAPT | New bar; bulk download/trash/move; no bulk share | 2 |
| I-07 | Folder/workspace inspector | `inspector.js:324-362,492-532` | Absent | MISSING | Folder facts; workspace summary under D-1 | 4 |
| I-08 | Appearance/Keyboard shortcuts sheets | `overlays.js:545-626` | Theme/lang buttons; no sheet | ADAPT | Port sheets; platform labels | 5 |
| L-01 | Trash screen | Not in prototype; current product | `Simple.tsx:324-394` | ADAPT | Settings → Trash; restyle; keep restore | 5 |
| L-02 | Activity screen | Home/inspector only | `Activity.tsx` with filters/export | ADAPT | Settings → Activity; restyle; add target filter | 5 |
| L-03 | App Passwords/WebDAV | Not in prototype; current product | `Simple.tsx:398-496`; `FolderAccess` | ADAPT | Settings → Access; restyle; keep ACL editor | 5 |
| L-04 | Admin users/groups/settings | Not in prototype; current product | `Admin.tsx` | ADAPT | Settings → Admin; restyle with tokens | 5 |
| L-05 | Breakglass login | Backend only | API documented, no UI | EXISTS | Keep out of the web UI (deliberate) | — |
| L-06 | Sign out | `overlays.js:106-118` | `App.tsx:278-289` | EXISTS | Keep; enable | 1 |

### 4.9 Contract, security, data

| ID | Capability | Current state | Status | Action | Phase |
| --- | --- | --- | --- | --- | --- |
| C-01 | OpenAPI covers every client call | 40 paths / 47 ops in `web/openapi.yaml`; server registers 69 routes | ADAPT | Add missing endpoints/schemas/security scheme; regenerate | 1 |
| C-02 | Share PATCH in client/spec | Server has it (`server.go:315`); spec/client do not | ADAPT | Add `PATCH`, `updateShare`, rotate | 3 |
| C-03 | Content origin in client | Ticket API exists; `api.ts:300-314` hardcodes app origin | ADAPT | `contentUrl()` + ticket fetch; inline vs download split | 1 |
| C-04 | CSRF/security model documented | SameSite=Lax + same-origin fetch; no token | EXISTS | Document; keep; no new cross-origin writes | 1 |
| C-05 | Share token recovery | Hash-only; no URL | MISSING | D-2 migration + field + rotate | 3 |
| C-06 | Per-file audit filter | No target filter/index | MISSING | Filter + index | 4 |
| C-07 | Folder counts, sort, all-files | Name-only listing | MISSING | Queries/endpoints | 2 |
| C-08 | Favorites/last-opened | Absent | MISSING | Tables/endpoints | 2 |
| C-09 | Team spaces | `nodes.owner_group_id` stored but unused in authz | MISSING | Defer (SHARE-11); D-1 | — |
| C-10 | Public archive download | No share zip | MISSING | `/s/{token}/zip` | 3 |
| C-11 | Gate metadata privacy | Server/client leak label | ADAPT | Minimal gate payload + test | 3 |
| C-12 | Thumbnail permission ambiguity | Comment says download, code checks read | ADAPT | Decide read; update comment/test | 1 |

**Matrix totals:** 116 rows — **EXISTS 14**, **ADAPT 60**, **MISSING 42**.
The distribution is the finding: the backend supplies the primitives; the missing third is mostly
read-model/contract work, not storage.

---

## 5. API work

### 5.1 Reuse without contract change

These need no server change; they need a typed client call and a UI:

| Endpoint | Used for |
| --- | --- |
| `GET /api/auth/me`, `POST /api/auth/logout` | Session, quota, role |
| `GET /api/files/trash`, `POST /api/files/restore` | Trash |
| `POST /api/files/mkdir`, `/rename`, `/move`, `/batch`, `DELETE /api/files/` | Mutations |
| `POST /api/files/uploads`, `PUT /{id}`, `POST /{id}/complete`, `DELETE /{id}` | Resumable upload |
| `POST /api/files/upload` | Small upload |
| `GET /api/files/zip` | Bulk download |
| `GET /api/files/versions`, `GET /version`, `POST /restore-version` | History |
| `GET /api/files/thumbnail?path=…` or `?id=…` | Previews |
| `GET/POST /api/acl/`, `DELETE /api/acl/{id}` | People/group access |
| `GET /api/shared`, `/api/shared/{id}`, `/api/shared/{id}/content` | Received shares (contract enriched) |
| `GET/POST /api/shares/`, `DELETE /api/shares/{id}` | Share list/create/revoke |
| `GET /api/audit/events`, `GET /api/audit` | Activity/export |
| `GET/POST/DELETE /api/apppasswords/` | App passwords |
| Admin endpoints | Workspace settings |

### 5.2 New or changed endpoints

Each row is a deliverable with a handler, store method (memory + PG), OpenAPI entry and tests.

| # | Method + path | Purpose | Contract | Phase |
| --- | --- | --- | --- | --- |
| 1 | `GET /api/workspace/summary` | Home metrics | `{name, display_name, file_count, folder_count, received_count, outbound_count, quota_bytes, used_bytes, trash_bytes, version_retention_days, trash_retention_days}` | 2 |
| 2 | `GET /api/me/last-opened` · `PUT /api/me/last-opened` | Resume band | GET `{node: FileNode, opened_at}` or 204; PUT `{node_id}` 204 | 2 |
| 3 | `GET /api/files/all` | All-files aggregation | `{folders: FileNode[], items: FileNode[], total, cursor, truncated, counts}`; folders are top-level only | 2 |
| 4 | `GET /api/files/` extensions | Sort/filter/ancestors/counts, folder-id addressing | Query `folder=<uuid>` **or** `path=…` (exactly one), `sort=name\|modified\|size\|kind`, `dir=asc\|desc`, `kind=…`; response adds `ancestors`, `total`, `filter_counts`, `child_count` on dirs | 2 |
| 5 | `GET /api/files/collection` | Viewer collection | Query `from=folder:<id>\|recent\|search:<q>\|shared\|single:<id>`, `file=<id>`, `sort`, `dir`; response `{key, kind, label, location, items, index, total, truncated}` | 4 |
| 6 | `POST /api/files/favorite` | Favourite/unfavourite | `{path, favorite}` 204; `favorite` on `FileNode`; `kind=favorite` filter | 2 |
| 7 | `GET /api/shared` fields | Received identity | Add `owner_name`, `shared_at`, `access`, `preview`, `child_count` | 3 |
| 8 | `GET /api/shared/{id}` fields | Received browsing | Add child `mime`, `preview`, `modified_at`, per-child `permissions` | 3 |
| 9 | `GET /api/shares/{id}` | Link inspector deep link | Single `Share` with `url` when recoverable | 3 |
| 10 | `PATCH /api/shares/{id}` extension | Access/password lifecycle | Add `kind` (request↔link), `permissions[]`, `clear_password`, `max_bytes_per_file`, `max_total_bytes`, `retention_seconds`, `allowed_mime`, `max_upload_rate`; validate transitions | 3 |
| 11 | `POST /api/shares/{id}/rotate` | Reissue URL | Owner only; returns `{token, url, note}`; old token stops working; audited | 3 |
| 12 | Public landing payload | Gate + post-gate | Gate: `{kind, requires_password, requires_email, owner_name}` only. Post-gate adds `expires_at`, `root_name`, `root_kind`, `root_size`, `root_mime`, `download_count`, `max_downloads`, `downloads_left`, `can_download`, `children[]` with preview/size/mime, upload rules | 3 |
| 13 | `GET /s/{token}/zip` | Download all | Gate + `CanRead`; streams ZIP; one logical download charged; size cap; `Content-Disposition`. When the content origin is split, the app-origin route verifies the gate and mints an archive capability; the content origin streams the ZIP (same redirect pattern as `handleShareDownloadRedirect`) | 3 |
| 14 | `POST /s/{token}/lock` | Lock again | Clears share/recipient cookies for this token; 204 | 3 |
| 15 | `GET /api/audit/events?target=<uuid>&target_kind=node` | Per-file activity | Filter + index; same page shape | 4 |
| 16 | `GET /api/files/versions` fields | Version metadata | Add `created_by_name`, `retention_days`; keep `version_content_available` | 4 |
| 17 | `POST /s/{token}/uploads` (P1) | Resumable public upload | Same four-step protocol scoped to share; or explicit size cap | 5 |

### 5.3 OpenAPI and client rules

- Add `securitySchemes.cookieAuth` (`apiKey`, `in: cookie`, name `__Host-wc_session`) and apply it to every
  `/api/*` operation except login/callback/breakglass; document that share routes are anonymous and
  gate-based.
- Add the six server routes currently absent from the document: `content-ticket`, `version` content,
  `PATCH /api/shares/{id}`, audit CSV export, admin user create/delete, and all `/s/{token}/*` sub-routes.
- Add a `Problem`/`Error` schema reuse and `429`, `409`, `413` responses to the operations that can emit them.
- Every new endpoint gets a schema type in `api-types.ts`; no `as never` casts for new code.
- Cursor is opaque. A cursor minted for one sort/filter must be rejected for another (`400 invalid_cursor`)
  rather than silently starting elsewhere.
- Pagination: `limit` is a hint bounded by `limits.max_profind_children`; `total` is best-effort and may be
  omitted for very large aggregations; `truncated` is authoritative.

---

## 6. Database work

### 6.1 Existing model (relevant tables)

| Table | What it already gives us | Gap for Cirrus |
| --- | --- | --- |
| `users` | role, status, quota/used, display_name, idp_confirmed_at, trash_bytes | no per-user preferences (fine: use localStorage) |
| `groups`, `group_members` | group ACL subjects | no group-owned spaces |
| `nodes` | tree, owner, size/mime/etag, trash, retain_until, owner_group_id | no description/tags, no child count, owner_group_id unused |
| `versions` | version_no, blob, size, created_by, created_at | no note; author name needs join |
| `acl_entries` | node/user/group, perms, inherit, created_by, created_at | no UI preset; no "shared_at" in API |
| `shares` | kind, label, token_hash, password_hash, expiry, caps, permissions, hide_contents, received_bytes, retention, rate, allowed_mime | no recoverable token, no last_access, no access column |
| `share_recipients` | email, verified/consumed/access timestamps | not exposed in owner list |
| `download_sessions` | one logical download across Range | good |
| `share_password_attempts`, `share_otp_attempts`, `share_upload_events` | throttles/rates | good |
| `audit.events` | hash-chained events, share_id, metadata | no `target_id` index/filter |
| `sessions`, `app_passwords` | auth, scope | good |

### 6.2 Migrations (additive; ship before the client that uses them)

**0016_favorites.sql**

```sql
CREATE TABLE node_favorites (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    node_id    uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, node_id)
);
CREATE INDEX node_favorites_node_idx ON node_favorites (node_id);
```

**0017_recent_opens.sql**

```sql
CREATE TABLE node_opens (
    user_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    node_id   uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    opened_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, node_id)
);
CREATE INDEX node_opens_user_time_idx ON node_opens (user_id, opened_at DESC);
```

`PUT /api/me/last-opened` is an upsert on `(user_id,node_id)`; a delete/trash cascades the row away. The
Home "Continue" query is one indexed row, not a scan.

**0018_share_token_recovery.sql** (only if D-2 accepts encryption)

```sql
ALTER TABLE shares ADD COLUMN IF NOT EXISTS token_enc bytea;
ALTER TABLE shares ADD COLUMN IF NOT EXISTS token_key_id text;
COMMENT ON COLUMN shares.token_enc IS
  'AES-256-GCM ciphertext of the URL token. token_hash remains the lookup key; decryption requires the key file, not a database dump.';
```

No backfill: existing shares have no ciphertext and therefore no recoverable URL. The owner-facing list
returns `url` only when `token_enc` decrypts; otherwise the UI offers "Reissue link" (the rotate endpoint).
The encryption key lives in a file configured by `shares.token_key_file` (or an env var), with the key id
recorded for rotation. AES-GCM AAD is the share UUID; nonce is random per write. If the key is absent,
the feature degrades to hash-only and startup logs a warning — never silently stores plaintext.

**0019_audit_target_index.sql**

```sql
CREATE INDEX IF NOT EXISTS audit_events_target_idx
    ON audit.events (target_id, ts DESC);
```

**0020_share_access_metadata.sql** (optional, only if the UI needs "last opened by recipient")

```sql
ALTER TABLE shares ADD COLUMN IF NOT EXISTS last_access_at timestamptz;
```

Recommendation: defer 0020. The design shows usage, not last access. `download_count`/`upload_count` and
`share_recipients.last_access_at` already answer the owner's questions.

### 6.3 Query work that needs no migration

- **Folder counts:** in the listing query, `LEFT JOIN LATERAL (SELECT count(*) FROM nodes c WHERE c.parent_id = n.id AND c.trashed_at IS NULL)`, returning `child_count` and a `file_count`/`dir_count` split. Index `nodes_parent_idx` already exists.
- **All files:** recursive CTE from the caller's root (depth cap 64, cycle guard), returning `path`, `folder_name`, `folder_id`, ordered by the requested key. Cursor must be sort-bound.
- **Sort:** name uses `name_key` (existing); modified/size/kind need a composite cursor `(sort_key, id)`. Add indexes only if measurements show a scan: `nodes_owner_modified_idx (owner_id, modified_at DESC)`, `nodes_owner_size_idx (owner_id, size DESC)`.
- **Received owner name:** join `users` on `nodes.owner_id` in `sharedWithMeQuery`; add `acl_entries.created_at` as `shared_at`.
- **Audit target:** `AuditFilter.TargetID` + `TargetKind`, both backends; index above.
- **Per-file versions author:** join `users` on `versions.created_by`.

### 6.4 Rollout

1. Apply 0016–0018 (and 0019) in one migration run. They are additive and safe for the old client.
2. Deploy server with the new endpoints disabled by a config flag (`features.cirrus`) or simply unused.
3. Deploy the new web bundle behind a beta host or `?ui=cirrus` switch.
4. Backfill nothing. Favorites/opens start empty; share URLs for old links fall back to "Reissue".
5. Drop the old bundle and the flag only after Phase 6 acceptance.

---

## 7. Permission model

### 7.1 The four layers that already exist

| Layer | What decides it | Where | UI consequence |
| --- | --- | --- | --- |
| Platform role | `users.role` (`admin`, `user`, `guest`) + `status` + `idp_confirmed_at` | `internal/authz/authz.go:37-57,314-327`; middleware | Admin section only for `admin`; disabled/pending accounts get 401 |
| Resource ACL | `acl_entries` per node/user/group, `inherit`, 15 permission bits | `authz.Authorize`; `store/acl*` | People/group sharing; "Can view/edit/manage" |
| Share link | `shares.kind`, `permissions` (`read`/`create`), password/recipient gate, caps | `share` package + share handlers | Public page, upload-only, expiry/limit |
| App Password scope | `ro` / `rw` narrows the actor | `authz.applyScope` | WebDAV clients; never widens |

Admin is deliberately **metadata-only** for content: `PermRead|Delete|ListChildren`, no `PermDownload`
(`authz.go:321-327`). The new admin UI must not add a preview/download action that would contradict this.

### 7.2 Mapping permissions to the design's words

The design only has "Can view" and "Can edit" on received rows. Do not collapse the fine-grained bits into
a boolean; derive labels and expose the real set in the Inspector.

| UI label | Permission set (all must be present unless noted) | Notes |
| --- | --- | --- |
| **Can view** | `read` + `list` | `download` optional; thumbnail visibility follows D-9 (download by default, or an explicit `preview` bit) |
| **Can download** | `download` | Shown separately if `read` exists without `download`; also gates thumbnails under the default policy |
| **Can edit** | `read` + `write` (implies `create`/`overwrite`) + `rename` + `move` | **Does not imply `delete`**; the authorizer deliberately refuses that implication |
| **Can manage access** | `manage_permissions` | Owner or delegated |
| **Can share** | `share` | Link creation; separate from edit |
| **Full control** | `delete` + `share` + `manage_permissions` + edit | Reserved for owner/manager preset |
| **Upload only** | `create` + `upload_only` (share request) | No list/read; server enforces |

Presets for the ACL editor: **Viewer** (`read,list,download,view_history`), **Editor** (`viewer + write +
rename + move`), **Manager** (`editor + delete + share + manage_permissions`), **Custom**. The current
`FolderAccess` UI exposes raw checkboxes and never sends `inherit` (`web/src/components/FolderAccess.tsx:22,64`);
Phase 5 replaces it with presets + an advanced toggle.

Share links remain simpler: `read` (view/download) or `create` (upload only). They are **not** converted
into `authz` caps: the public handlers check `Share.CanRead`/`CanCreate` and serve or accept bytes
directly. This is a second authorization path, and it must be kept as narrow as it is today. The comment at
`internal/authz/authz.go:399-403` refers to a `share.PermissionsToACL` helper that does not exist in the
tree; correct or delete the comment in Phase 1 so the next reader is not told a conversion happens.

### 7.3 Security invariants and the tests that pin them

| Invariant | Current evidence | Cirrus work |
| --- | --- | --- |
| A denial is indistinguishable from absence (404, not 403) | `authz.ErrDenied` mapping | Keep for every new endpoint |
| Taking bytes requires `download`, not just `read` | `TestDownloadRequiresDownloadNotJustRead` | Keep; use it for the ticket and shared content |
| A thumbnail is content; its permission is explicit | `internal/server/thumbnail.go:62-68` contradicts itself; `PermRead` would let an admin fetch user thumbnails | D-9: require `download` (or a separate `preview` bit not granted to admin), update comment, add regressions for read-only grants **and** admin |
| A content ticket dies with the account | `TestATicketStopsWorkingWhenTheAccountIsDisabled` | Keep; client must actually use tickets |
| Public gate returns no protected metadata | `sharepages.go:64-71` leaks `label` | **P0 fix**: minimal gate payload + client test |
| Share passwords are write-only and throttled | `share.go:313-372`; `TestSharePasswordBruteForceIsThrottled` | Keep; client must surface 429 honestly |
| Email recipient codes are single-use and throttled | `shareverify.go`; `share_otp_attempts` | Keep; do not break existing links |
| Upload-only links cannot list/read | `sharepages.go:83-95`; `hide_contents` | Keep; public upload UI must not show counts/names |
| IdP staleness is bounded | `TestAStaleIdPAccountIsRefused` | Keep; no effect on UI |
| Audit writes fail closed | `audit` package | Keep; new events use the same transaction |

### 7.4 Specific fixes required by this integration

1. **Gate privacy (P0).** `handleSharePage` must return only `{kind, requires_password, requires_email}`
   before the gate (plus the owner display name under D-6). `ShareLanding` must render generic copy before
   unlock. Add a rendered-page test that asserts no title, filename, count, scope, expiry, thumbnail or
   `label` appears in the gated DOM.
2. **Thumbnail comment/code (P1).** Make the comment, code and tests agree under D-9. The tempting answer
   ("read") is wrong for administrators: their branch deliberately withholds `PermDownload`, but a
   `PermRead` thumbnail check hands them every user's image pixels by id. Either require `download` or add
   a `preview` bit granted only to owners and ACL readers. Add a regression for both a read-only grant and
   an administrator.
3. **Token storage (P0 decision).** If D-2 is accepted, `token_enc` is AES-GCM and the key is never in the
   database. `Share.URL` must be returned only to the owner; a public landing never receives it.
4. **Access-mode transition (P1).** Widening request→link changes `kind`, sets `permissions=["read"]` and a
   default `max_downloads`, and is refused for a file target. Narrowing link→request clears download caps,
   sets `permissions=["create"]`, and is refused for a file target. Both audit before/after.
5. **Clear password (P1).** `PATCH` with `clear_password: true` removes the hash; the UI never re-renders a
   password. An empty string must not silently set a password of `""`.
6. **Admin content boundary (P1).** Admin screens must not offer preview/download for user files; the
   authorizer already refuses, but a button that always fails is a lie.
7. **Content origin (P1).** All byte-taking actions go through `content-ticket`; inline previews stay on the
   app origin with `inline=1`. The client must not hardcode one origin for both.
8. **Stale authorization comments (P2).** `internal/authz/authz.go:399-403` names a `PermissionsToACL`
   helper that does not exist, and `internal/store/acl.go:145-159` says `read` implies `download` while
   `authz.expandComposite` deliberately does not. Fix the comments (or delete the unused
   `store.ExpandComposite`) so the next security review does not reason from a false premise.

---

## 8. Design system and frontend implementation

### 8.1 Token layer

Port `app/css/tokens.css` into `web/src/styles/tokens.css` with the same role names. Key mappings:

| Cirrus token | Use | Current Atrium token |
| --- | --- | --- |
| `--canvas`, `--surface`, `--surface-raised` | Page and raised surfaces | `--bg`, `--surface`, `--surface-2` |
| `--fill-1/2/3` | Chips, rows, controls | `--surface-2/3` |
| `--line`, `--line-strong` | Rules, boundaries | `--line`, `--line-strong` |
| `--ink`, `--ink-2`, `--ink-3` | Text hierarchy | same names |
| `--accent`, `--accent-hover`, `--accent-soft`, `--accent-line` | Selection/focus/primary | Atrium emerald `--accent`/`--accent-wash` |
| `--danger*`, `--warn*`, `--ok*`, `--info*` | Semantics | partial |
| `--selection`, `--row-hover`, `--selection-edge` | Selection | partial |
| `--kind-*-bg/fg` | File types | absent |
| `--chrome-bg`, `--popover-bg`, `--scrim` | Materials | absent |
| `--shadow-1/2/3` | Elevation | two shadows |

Keep the existing contrast script but update its token list; add a check that every role used in CSS is
defined (this would have caught `--muted`). Add Large Text as `--text-scale` on `html[data-textsize=large]`
and express every type role as `calc(<base> * var(--text-scale))`.

### 8.2 Component map

| Prototype component | New source | Notes |
| --- | --- | --- |
| `topbarMarkup` / `primaryNav` | `shell/TopBar.tsx`, `Shell.tsx` | Three destinations; selection replaces right side |
| `accountButton` / account menu | `shell/AccountMenu.tsx` | Include Settings sub-routes and sign out |
| `searchField` / search panel | `shell/SearchPanel.tsx` | Groups Folders/Files/Links/Actions; server search |
| `inspectorView` | `shell/Inspector.tsx` | Desktop dock, mobile sheet, panes |
| `homeMarkup` | `views/Home.tsx` | Resume, Recent, Received, Activity |
| `filesMarkup` | `views/Files.tsx` | One list, breadcrumb, filter, sort, selection |
| `folderRow` / `fileRow` / grid | `components/FileRow.tsx`, `FolderRow.tsx`, `GridTile.tsx` | Same grid columns; folder count/metadata |
| `sharingMarkup` | `views/Sharing.tsx` | Tabs, lists, meters |
| `linkDraft` + sheet | `components/CreateLinkSheet.tsx` | Six sections; validation; live summary |
| `shareInspector` | `components/LinkInspector.tsx` | URL, perms, usage, access, password, revoke |
| `previewMarkup` + renderer | `views/Viewer.tsx`, `components/renderers/*` | One viewer; per-type renderers |
| `publicView` | `views/PublicShare.tsx`, `components/Gate.tsx` | Generic gate; upload/download |
| `toastMarkup` | `shell/Toast.tsx` | aria-live |
| menus/sheets | `components/Menu.tsx`, `Sheet.tsx` | Focus trap; Escape; epoch guard |

Delete the duplicate `Stage` in `Files.tsx`; one renderer only.

### 8.3 State model

Use URL as the source of truth for navigation/filters; use React state for ephemeral layers; use a small
cache for lists.

| State | Where | Persistence |
| --- | --- | --- |
| `me`, auth status | `session.tsx` | memory; 401 → login redirect once |
| route (`screen`, `space`, `file`, `from`, `tab`, `share`, `pane`, `q`, `sort`, `dir`, `view`, `kind`) | `route.ts` | URL + history |
| preferences (`theme`, `contrast`, `textsize`, `lang`) | `usePreferences` | localStorage + URL override |
| selection/anchor/focus | Files component | memory; URL `select` only for tests, not product |
| listings/collections | `data/` cache | memory; invalidate on mutation |
| upload queue | `useUploads` | memory + `wc.pendingUploads` for resume |
| share draft | CreateLinkSheet local state | memory; cancel discards |
| toasts | provider | memory |

Fix the current `reloadKey` bug (`App.tsx:91` has no setter). Replace it with an explicit cache
invalidation function: `invalidate('listing')`, `invalidate('shares')`, etc.

### 8.4 Internationalization

- Every visible string, `aria-label`, placeholder, empty state, error and toast goes through `t()`.
- Add keys for the new surfaces: shell, home, files, sharing, link form, public, viewer, inspector, settings,
  shortcuts, errors.
- Keep `en` and `zh-Hans`; run `pnpm i18n` (key parity + referenced-key check) in CI.
- Dates, sizes, relative times and plurals use `Intl` as today. Do not concatenate sentences.
- Platform shortcut labels come from a `platform.ts` module (port `app/js/platform.js`); the handler accepts
  both Ctrl and ⌘.
- The prototype's copy is the source for English; translate deliberately, not word-for-word.

### 8.5 Accessibility acceptance

- Axe: 0 violations on Home, Files, Sharing, Viewer, Create link, Link Inspector, Public gate/unlocked,
  Settings, mobile at 390/320.
- Focus: visible 2px focus ring; focus trap in sheets/menus/dialogs; focus restored on close; the skip link
  reaches `<main>`.
- Keyboard: every Files action reachable; arrows/Home/End/Enter/Space/Delete/⌘A/⌘I/⌘K; no key handler on a
  non-focusable row (put handlers on the focusable container, as the prototype does).
- Touch: every visible control ≥44×44 at 320/390, measured, not assumed.
- Large Text: 49 surfaces at 7 widths (port the design's text audit) with no clipping/overflow.
- Reduced motion/transparency: preference and OS settings both honored.
- Screen readers: listbox/option semantics with `aria-activedescendant`; `aria-selected` reflects selection,
  not focus; live region for selection count, upload progress and viewer position.

### 8.6 CSS cleanup (do this before adding new rules)

1. Fix the invalid UTF-8 byte at `web/src/styles.css:~2286` (it currently breaks strict editors and tools).
2. Remove or define `--muted`; remove dead classes `.check`, `.loadmore`, `.langmark`, `.landing-note` or add
   their rules.
3. Add the missing icons `folder`/`plus` or stop referencing them.
4. Move the ACL/admin/groups sections (appended after the responsive block) into the new split files with
   breakpoints.
5. Delete the old Atrium column CSS once the new Files surface ships; do not keep two layout systems.

---

## 9. Phased delivery plan

Estimates are dev-days for one senior full-stack engineer who knows the codebase. Parallelizable work is
marked. Each phase ends with a demonstrable build and a gate.

### Phase 0 — Decisions and baseline (2–3 d)

| Task | Output |
| --- | --- |
| 0.1 Sign D-1, D-2, D-4, D-8, D-9 | Decision record appended to this plan or an ADR |
| 0.2 Create `feature/cirrus-9.1` from a clean commit | Branch; freeze manifest still verifies |
| 0.3 Stand up the frontend test harness | `web/tests/harness.mjs` + `playwright-core`; runs against `tools/demoinmem` |
| 0.4 Inventory the 69 server routes vs 47 OpenAPI ops | A checked list; file issues for the six missing |

**Gate:** decisions signed; harness opens the current app and a `/s/{token}` page in Chromium.

### Phase 1 — Foundations (10–12 d)

| Task | Files | Depends |
| --- | --- | --- |
| 1.1 Token layer + split CSS + fonts + large text | `web/src/styles/*`, `web/index.html` | 0 |
| 1.2 Router + providers + URL contract | `web/src/route.ts`, `App.tsx`, `session.tsx` | 0 |
| 1.3 Shell skeleton: topbar, account menu, mobile bar | `shell/*` | 1.2 |
| 1.4 API contract: OpenAPI additions, `contentUrl`, `updateShare`, collection types | `web/openapi.yaml`, `api.ts`, `types.ts` | 0.4 |
| 1.5 DB migrations 0016–0019 + store methods | `migrations/*`, `internal/store/*` | D-2, D-8 |
| 1.6 Workspace summary + last-opened endpoints | `internal/server/workspace.go`, store | 1.5 |
| 1.7 Content-origin client path | `api.ts`, `session` | 1.4 |
| 1.8 Test harness: login, seed, route assertions | `web/tests/*` | 0.3 |

**Gate:** new shell renders Home/Files/Sharing placeholders at 1440/390 with Cloud Blue tokens; deep links
survive reload/Back/Forward; `pnpm typecheck`, `pnpm gen:api:check`, `pnpm i18n`, `pnpm contrast` pass;
`go test ./...` passes; migrations apply to memory and PG.

### Phase 2 — Home + Files (13–15 d)

| Task | Files | Depends |
| --- | --- | --- |
| 2.1 Home: resume band, Recent, Received column, Activity, Upload | `views/Home.tsx`, `styles/home.css` | 1.2, 1.6 |
| 2.2 Files: one listing, folder rows, counts, grid | `views/Files.tsx`, `components/File*`, `styles/files.css` | 1.2, 1.4 |
| 2.3 Breadcrumb + All files + root/folder semantics | Files + collection/ancestors API | 1.4 |
| 2.4 Filter/sort/counts + cursor pagination | Files + listing API | 1.4 |
| 2.5 Selection, keyboard, drag-to-folder, move/rename/trash | Files + transfer API | 2.2 |
| 2.6 Upload provider: queue, per-file progress, pause/retry/resume | `data/useUploads.tsx`, `components/Upload*` | 1.4 |
| 2.7 Favourites + last-opened wiring | Files/Home + API | 1.5 |
| 2.8 File-type tiles/thumbs/CSS diagrams | `components/KindTile.tsx`, `Thumb.tsx` | 1.1 |

**Gate:** a user can browse from All files into a nested folder, filter/sort/select/move/rename/trash/undo,
upload a large file with resume, favourite an item, and return via Back with state intact. E2E covers
1440/390/320, dark and Large Text.

### Phase 3 — Sharing (10–12 d)

| Task | Files | Depends |
| --- | --- | --- |
| 3.1 Shared with me: owner/time/access, folder browsing | `views/Sharing.tsx`, shared API | 1.4 |
| 3.2 Links I manage: active/expired, usage, expiry, URL | Sharing + share API + D-2 | 1.5 |
| 3.3 Create-link sheet: six sections, validation, summary | `components/CreateLinkSheet.tsx` | 3.2 |
| 3.4 Link Inspector: perms, usage, access change, password, revoke, preview | `components/LinkInspector.tsx` | 3.2 |
| 3.5 PATCH transitions + rotate + clear password | `shares.go`, `share.go`, store | D-2 |
| 3.6 Public landing: generic gate, post-gate metadata, upload/download | `views/PublicShare.tsx`, `Gate.tsx` | 3.5 |
| 3.7 Public zip + lock + rules | `sharepages.go` | 3.6 |
| 3.8 Gate privacy + 429 + no-leak regressions | Go tests + e2e | 3.6 |

**Gate:** the four access/password combinations work against the real server; a wrong password reveals no
title/name/count; a folder link downloads all; an existing email link still opens; Copy link works after
reload (or Reissue is the explicit fallback).

### Phase 4 — Viewer + Inspector (8–10 d)

| Task | Files | Depends |
| --- | --- | --- |
| 4.1 Collection API + `from=` resolution | `internal/server/collection.go`, store | 2.4 |
| 4.2 Viewer shell: URL, history, footer, filmstrip, boundaries | `views/Viewer.tsx`, `styles/viewer.css` | 4.1 |
| 4.3 Image/video/audio/PDF renderers | `components/renderers/*` | 3.6 |
| 4.4 Text/code + CSV table + honest fallback | renderers | 4.3 |
| 4.5 Inspector: details/versions/activity/share, dock + sheet | `shell/Inspector.tsx` | 4.1 |
| 4.6 Per-file activity endpoint + target index | `auditbrowse.go`, store | 1.5 |
| 4.7 Zoom/pan/swipe | Viewer | 4.2 |

**Gate:** Space/Enter/double-click/tap/URL all open the same viewer; Back returns to the exact folder with
selection/filter/scroll; boundaries disable; images zoom, mobile swipe changes files only when unzoomed;
versions restore where available; per-file activity is real.

### Phase 5 — Legacy surfaces, i18n, a11y, mobile (9–11 d)

| Task | Files | Depends |
| --- | --- | --- |
| 5.1 Settings shell: Trash, Activity, Access, Admin | `views/Settings.tsx`, existing views | 1.3 |
| 5.2 Trash retention/restore; Activity filters/export; Access ACL presets | Settings + APIs | 5.1 |
| 5.3 Admin restyle + user lifecycle (create/delete) | `views/Admin.tsx` | 5.1 |
| 5.4 Full i18n pass (en + zh-Hans) | `i18n.ts` | 2–4 |
| 5.5 Axe/focus/keyboard/touch/Large Text audits | components + tests | 5.4 |
| 5.6 Mobile sheet physics, tab bar, 320px layouts | `styles/mobile.css`, Inspector, Viewer | 5.4 |
| 5.7 Resumable public upload (P1) or explicit size cap | `sharepages.go`, upload manager | 3.7 |

**Gate:** 0 axe violations on 35 screens; all 44px targets measured; 49 Large Text surfaces clean; existing
Trash/Access/Activity/Admin capabilities reachable and functional; both languages complete.

### Phase 6 — Hardening and rollout (5–7 d)

| Task | Output |
| --- | --- |
| 6.1 Performance: 1k/10k-file folder, virtualized rows if needed, cursor paging | Measurements + fixes |
| 6.2 Security review: gate, tickets, token key, rate limits, IDOR sweep | Checklist + tests |
| 6.3 Resilience: 401/offline/partial failure/retry; audit outage copy | States + tests |
| 6.4 Visual review against frozen screenshots at 5 widths/3 appearances | Signed-off diff list |
| 6.5 CI: `make webcheck` + e2e job + migration test | Green pipeline |
| 6.6 Remove old shell/CSS/flags; update docs | Release |

**Gate:** all Phase 1–5 gates still green; no P0/P1 defect open; rollback plan tested (old bundle + additive
migrations left in place).

### Critical path

```text
D-1/D-2 ─┬─ 1.1 tokens ─ 1.2 router ─ 1.3 shell ─ 2.2 Files ─ 2.4 sort/cursor ─ 4.1 collection ─ 4.2 viewer
         ├─ 1.4 API ─ 1.5 migrations ─ 1.6 workspace ─ 2.1 Home
         └─ 1.5 ─ 3.2 shares ─ 3.3 form ─ 3.4 inspector ─ 3.6 public
```

Two engineers can parallelize at Phase 2 (frontend surfaces vs backend endpoints) if the API contract is
frozen at the end of Phase 1. The viewer and the public gate should not start before their contracts exist.

---

## 10. Test and acceptance strategy

### 10.1 Backend

- Every new store method runs in the shared conformance suite against memory **and** PostgreSQL
  (`internal/storetest`). A memory-only implementation is not accepted.
- Every new handler gets a table-driven authorization test: owner, direct grant, group grant, share link,
  admin, anonymous, expired/revoked, cross-account IDOR.
- Migrations get an apply/reapply test; the PG suites remain skipped without `TEST_DATABASE_URL`.
- Share token encryption gets: round-trip, wrong key id, tampered ciphertext, missing key degradation,
  rotate invalidates old token, audit row written.
- Gate privacy gets a response-level assertion (no `label`, no `root_name`, no count) plus a rendered-page
  assertion.

### 10.2 Frontend

Add `web/tests/` using `playwright-core` (already used by the design studio; no new runtime dependency) and
a harness that starts `tools/demoinmem` + the built bundle. Suites:

| Suite | What it asserts |
| --- | --- |
| `smoke.mjs` | Login, Home, Files, Sharing, Viewer, Settings open at 1440/390 without console errors |
| `routes.mjs` | Deep links, Back/Forward, duplicate-entry collapse, unknown values canonicalise |
| `files.mjs` | One listing, breadcrumb, filter/sort/counts, selection, move/rename/trash/undo, upload resume |
| `sharing.mjs` | Two tabs, create form, validation, access change, password set/clear, revoke, copy/reissue |
| `public.mjs` | Gate privacy, four combinations, email legacy, download all, upload rules, lock |
| `viewer.mjs` | Every entry point, collection boundaries, renderers, zoom, swipe, direct link reload |
| `a11y.mjs` | Axe on 35 surfaces; focus trap/return; keyboard paths |
| `responsive.mjs` | 1440/1024/768/390/320, dark, Large Text, no overflow/clipping |
| `visual.mjs` | Screenshot each surface; compare against the frozen reference for layout (not pixel-identical data) |

Selectors: add `data-testid` only where role/name is ambiguous; prefer accessible roles/names so the tests
also prove the accessibility tree.

### 10.3 CI gates

```text
go test ./...
go vet ./...
gofmt -l cmd internal tools
cd web && pnpm typecheck && pnpm contrast && pnpm i18n && pnpm gen:api:check && pnpm build
node web/tests/smoke.mjs && node web/tests/routes.mjs && node web/tests/public.mjs
```

The e2e suites may run as a separate job; they must not be skipped on release branches. Add a check that
`internal/webassets/dist` was rebuilt from the current `web/src` (the current tree has a dist newer than
sources, but that is not enforced).

### 10.4 Definition of done for the integration

- Every Phase 1–5 gate green.
- The frozen design's P0 flows are implemented or an explicit, owner-approved deviation is recorded:
  team spaces, XLSX, multi-item links, remove-from-list, report-link, email-create.
- No capability in the current product is lost without a recorded decision (Trash, Activity, Access, Admin,
  email links, breakglass).
- `FREEZE.md` still verifies; no prototype file was edited.
- The plan's decision log, API table and matrix are updated to "done" with test anchors.

---

## 11. Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
| --- | --- | --- | --- |
| The design's team-workspace copy ships on a personal backend | High if D-1 is skipped | Trust: false claims | D-1 first; remove members/team strings; review all copy |
| Link URL recovery weakens "DB dump cannot replay links" | Medium | Security review | Separate key file, AES-GCM, AAD, rotate; document the tradeoff; fallback to hash-only + reissue |
| Public gate leaks metadata again | Medium | Privacy defect | Minimal gate payload by construction; rendered-DOM regression; no `label` in the gate view |
| Content-origin split is deployed but the client uses app-origin URLs | High (current code) | Downloads break or bypass the boundary | Phase 1 `contentUrl()`; e2e with `demoinmem` (it serves a content origin) |
| All-files recursive query is slow on large trees | Medium | Home/Files latency | Cursor + limit, depth cap, indexes; measure with `tools/seedscale`; fall back to recent-only if needed |
| Server-side sort/cursor bugs skip/duplicate rows | Medium | Data loss appearance | Sort-bound opaque cursor; conformance tests; keyset `(sort_key,id)` |
| Viewer format claims exceed backend | High | Dishonest UI | D-3; honest fallback; no fabricated sheet/PDF content |
| Legacy capabilities disappear in the new IA | Medium | Product regression | Phase 5 explicit routes; acceptance checklist; owner sign-off |
| i18n/a11y treated as polish | High | Rework | Phase 5 budget; key parity and axe in CI from Phase 2 |
| Mobile sheet/swipe physics only tested synthetically | Medium | Poor real-device feel | Test on a physical device; keep button fallbacks |
| Prototype deviations accumulate silently | Medium | Unreviewable design | Every deviation logged in §12/decision record; freeze untouched |
| Uncommitted tree / design-studio untracked | High | Lost work | Commit the audited state before Phase 2; keep `design-studio` tracked or archived |

---

## 12. Open questions for the owner

1. **D-1 workspace:** confirm personal-space interpretation for this release. If team space is required, add
   a separate epic for group-owned roots, group quota and membership management; it changes Home, Files,
   search, sharing and the permission model.
2. **D-2 link recovery:** approve encrypted token storage, or accept "Reissue link" after reload.
3. **D-3 spreadsheet:** is CSV/TSV enough for launch, or is XLSX a hard requirement? The latter needs a
   parser and a bundle-size/security review.
4. **D-6 sharer identity:** is showing the owner's display name to an unauthenticated visitor acceptable?
5. **Legacy surfaces:** should App Passwords/WebDAV stay, and under what name ("Access", "Devices", "Apps")?
6. **Version retention copy:** the prototype says "90 days"; production has `limits.version_retention_days`.
   Confirm the default and whether the UI should state it or only show the actual configuration.
7. **Download-all counting:** should a folder ZIP cost one download from the link's allowance, or one per
   file? The prototype shows a single "downloads left" counter; one per archive is the simplest honest rule.
8. **Email shares:** keep creating them from the new UI, or only preserve existing links? (D-5 recommends
   preserve-only for P0.)

---

## Appendix A — File map for Phase 1–4

**New frontend files**

```text
web/src/route.ts
web/src/session.tsx
web/src/data/{useListing,useCollection,useShares,useUploads,usePreferences}.ts(x)
web/src/shell/{Shell,TopBar,AccountMenu,SearchPanel,Inspector,Toast}.tsx
web/src/views/{Home,Files,Sharing,Viewer,PublicShare,Settings}.tsx
web/src/components/{FileRow,FolderRow,GridTile,KindTile,Thumb,Breadcrumb,FilterChips,SortMenu,SelectionBar,CreateLinkSheet,LinkInspector,PasswordForm,Gate,UploadDrop,ShareFileList,Menu,Sheet,EmptyState,ErrorState,Skeleton,ConfirmSheet}.tsx
web/src/components/renderers/{Image,Video,Audio,Pdf,Text,Code,Sheet,Fallback}.tsx
web/src/styles/{tokens,base,shell,home,files,sharing,inspector,viewer,public,mobile}.css
web/src/platform.ts
web/tests/{harness,smoke,routes,files,sharing,public,viewer,a11y,responsive,visual}.mjs
```

**New backend files**

```text
internal/server/workspace.go
internal/server/collection.go
internal/store/favorite.go, favorite_memory.go, favorite_pg.go
internal/store/open.go, open_memory.go, open_pg.go
migrations/0016_favorites.sql … 0019_audit_target_index.sql
```

**Modified (materially)**

```text
web/src/App.tsx, api.ts, api-types.ts, types.ts, i18n.ts, main.tsx, index.html
web/src/views/Admin.tsx, Activity.tsx, Simple.tsx, ShareLanding.tsx
web/openapi.yaml
internal/server/files.go, shares.go, shared.go, sharepages.go, auditbrowse.go, server.go
internal/store/{store.go, pg.go, memory.go, shared.go, share*.go, auditread*}.go
cmd/newcloud/main.go (wiring)
docs/reference/implementation-record.md, docs/guides/limitations.md, docs/README.md
```

---

## Appendix B — Audited endpoint counts

- `internal/server/server.go` registers **69** routes (including WebDAV dispatch, health, metrics, share
  landing and content origin).
- `web/openapi.yaml` documents **40 paths / 47 operations** and no `securitySchemes`.
- The client calls **24** distinct operations through `openapi-fetch` plus 7 raw-fetch paths (uploads, share
  landing, public verify/upload).
- Missing from the document but present on the server: content ticket, version content, share PATCH, audit
  CSV, admin user create/delete, and the `/s/{token}/{verify,recipient,recipient/verify,upload,download,
  thumbnail}` routes.
- The design prototype has **5 screens** (home/files/sharing/preview/public) and **no** admin, trash, access
  or full activity surface.

## Appendix C — Current API surface (audited, grouped)

This is the production surface the new UI consumes or must not break. It is grouped; the route table in
`internal/server/server.go:204-415` is authoritative.

| Group | Methods and paths |
| --- | --- |
| Ops | `GET /healthz`, `GET /readyz`, `GET /metrics` |
| Auth | `GET /api/auth/login`, `GET /api/auth/callback`, `POST /api/auth/logout`, `POST /api/auth/breakglass`, `GET /api/auth/me` |
| Files — read | `GET /api/files/`, `/recent`, `/search`, `/download`, `/thumbnail`, `/trash`, `/versions`, `/version`, `/zip` |
| Files — write | `POST /api/files/mkdir`, `/upload`, `/rename`, `/move`, `/batch`, `/restore`, `/restore-version`, `POST /api/files/content-ticket`, `DELETE /api/files/`, `POST /api/files/uploads`, `PUT/DELETE /api/files/uploads/{id}`, `POST /api/files/uploads/{id}/complete` |
| Shared with me | `GET /api/shared`, `/api/shared/{id}`, `/api/shared/{id}/content` |
| ACL | `GET/POST /api/acl/`, `DELETE /api/acl/{id}` |
| Shares | `GET/POST /api/shares/`, `PATCH/DELETE /api/shares/{id}` |
| Admin | `GET/POST /api/admin/users`, `POST /api/admin/users/{id}/status`, `/quota`, `DELETE /api/admin/users/{id}`, `GET/POST /api/admin/groups`, `POST/DELETE /api/admin/groups/{id}/members[/{userID}]`, `GET /api/admin/settings`, `PUT/DELETE /api/admin/settings/{key}` |
| Audit | `GET /api/audit`, `GET /api/audit/events` |
| App passwords | `GET/POST /api/apppasswords/`, `DELETE /api/apppasswords/{id}` |
| WebDAV | All RFC 4918 verbs via `s.davInterceptor` on `/dav/*` (not a chi route) |
| Public share | `GET /s/{token}` (HTML/JSON), `POST /s/{token}/verify`, `/recipient`, `/recipient/verify`, `/upload`, `GET /s/{token}/download`, `/thumbnail` |
| Content origin | `GET /content/*` (ticket), plus the redirects for share download/thumbnail when split |

## Appendix D — Current database tables (audited)

| Table | Purpose |
| --- | --- |
| `users` | Account, role, status, quota/used, display name, IdP identity, `idp_confirmed_at`, `trash_bytes` |
| `sessions` | OIDC/break-glass sessions, partitioned by expiry |
| `app_passwords` | WebDAV credentials, `ro`/`rw`, hashed |
| `groups`, `group_members`, `group_role_map` | Group ACL subjects and IdP role mapping |
| `blobs` | Content-addressed storage, refcount, collect-after |
| `nodes` | Tree: file/dir, owner, `owner_group_id`, size/mime/etag, trash, `retain_until` |
| `versions` | Version number, blob, size, created_by/at |
| `dead_props`, `lock_tokens` | WebDAV properties and durable locks |
| `acl_entries` | Per-node user/group grants, permission names, inherit, created_by/at |
| `shares` | Link/email/request, token hash, password hash, expiry, download/upload caps, permissions, hide_contents, received_bytes, retention, rate, allowed MIME |
| `share_recipients` | Email recipients, verification/consumption/access timestamps |
| `download_sessions` | One logical download across Range requests |
| `share_password_attempts`, `share_otp_attempts`, `share_upload_events` | Throttles and rolling-window upload rate |
| `audit.events`, `audit.anchors`, `audit.prune_marks` | Hash-chained audit, daily anchors, pruned-chain boundary |
| `jobs` | Transactional work queue (leases/retries) |
| `scim_pending_changes` | Staged SCIM changes (no SCIM endpoint yet) |
| `settings` | Runtime overrides for limits |

The new tables in §6.2 (`node_favorites`, `node_opens`, encrypted token columns, audit target index) are
the only schema additions this plan requires.

## Appendix E — Prototype-only routes and switches (do not ship)

| Prototype | Why not production |
| --- | --- |
| `?screen=public&sid=<id>` | Review identity for a session-only share; production recipient pages are `/s/{token}` |
| `?mode=download`, `?mode=password`, `?mode=upload` | Standalone demo fixtures; not a product route |
| `?resume=none` | Forces the "nothing opened" state for review |
| `?ds=empty`, `?ds=loading`, `?ds=error` | Forces UI states |
| `?select=…` | Preloads a selection for QA |
| `?search=1`, `?sheet=…` | Opens a panel for screenshots |
| `?rtl=1` | Review switch; production follows `dir` from the language |
| `?capture=1` | Freezes URL/history for stable screenshots |
| `?theme/textsize/contrast` | These are real preferences in production, but URL override is optional; keep localStorage as the source of truth |

---

*This plan is a living document during integration. Update the status column and the decision log; do not
edit the frozen prototype. If a change to the design itself is required, open `round-9.2` and freeze it
separately.*
