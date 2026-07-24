# newCloud — 设计文档 0001：功能范围与语言选型

状态：草案 v2（已吸收 2026-10-21 第二轮决策，见 §8）
日期：2026-10-21
范围：本文件回答 **① 做哪些功能 ② 用什么语言 ③ 已确认的技术约束**。架构细节另开文档。

---

## 0. 结论摘要

1. **产品定位**：一个**自托管的文件协作平台**（文件存储 + WebDAV + 分享 + 审计）。定位里不写用户数、容量、部署形态——那些是**某次安装的参数**，不是产品定义。产品必须能在单机 Docker 上为 200 人服务，也能横向扩展为多副本；规模由配置和部署决定，不由代码决定。
2. **功能**：按 P0（v1 必须有）/ P1（v1.1）/ P2（以后）三档收敛。P0 = OIDC SSO + 多用户/组 + WebDAV + 网页上传下载 + 分享全功能（密码/期限/次数/邮箱）+ 上传请求（file request）+ 审计。其余全部砍到 P1/P2。
3. **语言**：**Go**。理由是交付速度、运维简单、IO 密集场景心智负担低；与任何特定规模无关。
4. **最重要的架构决策不是语言，而是这条**：**用户内容必须与主站隔离**（独立域名/独立 origin、强制 attachment、nosniff、不执行任何用户上传的可执行内容、没有插件系统）。Nextcloud 的安全史里，最致命的几类问题（第三方 app 市场、共享主机上 `.php` 可写可执行、模板/include、反序列化）都是**架构问题**，换语言不会自动消失。
5. **已确认的产品决策**（§8）：IdP = **Pocket ID**（passkey-only）；v1 **不做** Nextcloud 客户端兼容；存储**只做本地盘 + S3**，布局可选 blob / tree；在线 Office **只留接口**；合规基线由本文定（§8.9）。
6. **当前安装的规模参数**（不是产品定位，见 §8.11）：约 2000 人 / 40 TiB / 初始单机 Docker。这些数字决定"这一次怎么部署、盘怎么买、备份怎么做"，但不决定代码能支持多大。**架构按可横向扩展设计，部署按单机起步。**

---

## 1. 背景：Nextcloud 的"PHP 安全问题"到底来自哪里

把风险拆开看，才知道换语言能解决什么、不能解决什么：

| 风险来源 | 换语言能自动解决？ | 说明 |
|---|---|---|
| 内存安全（UAF/越界） | Rust 能，Go 大部分能（无 UAF，但有 nil/竞态） | 文件服务器里这类 bug 比例其实不高 |
| 反序列化 RCE（`unserialize`） | ✅ 能 | 新系统不引入"任意对象反序列化"能力即可 |
| 类型混淆导致的鉴权绕过（`==` 弱比较） | ✅ 能 | 强类型语言天然没有 |
| LFI/RFI、`include_path`、`.htaccess` 失效 | ✅ 能 | 单二进制不需要 web 服务器配置 |
| 第三方 app 市场 / 插件任意代码 | ✅ 能（只要**不做**插件系统） | 这是 Nextcloud 最大的攻击面 |
| 部署在共享主机，`data/` 目录落在 webroot 内 | ⚠️ 语言无关 | 靠部署规范解决 |
| **路径穿越、授权缺失(IDOR)、ZIP 穿越、SSRF、存储型 XSS、审计缺失** | ❌ **语言无关** | 这才是我们自研时最容易犯的错，必须靠设计和测试来防 |
| 大依赖树供应链（composer + npm + app store） | ⚠️ 部分 | 少依赖 = 少攻击面，但对任何语言都成立 |

**结论**：换语言能消除"一整类 PHP 特有 bug"，但**不能**消除文件服务器最常见的漏洞类型。因此：
- 语言选型按"工程效率 + 可维护性 + 团队能力"来定；
- 安全性靠**架构约束 + 类型/接口设计 + 测试策略**来保（见 §4.4 的 Rust 型安全思路，Go 也能照搬）。

---

## 2. 兼容性策略（必须第一个定，影响 30%+ 工作量）

三个选项，成本差异巨大：

| 方案 | 含义 | 客户端可用性 | 增量成本 | 备注 |
|---|---|---|---|---|
| **A. 标准 WebDAV 优先**（推荐 v1） | 只实现 RFC 4918 + 常见扩展；网页端自带 | 浏览器、Windows 资源管理器、macOS Finder、rclone、Cyberduck、Office「打开/保存」 | 基线 | 最省力，最安全，最可控 |
| **B. 半兼容 Nextcloud** | 额外实现 `/status.php`、`/ocs/v2.php/cloud/capabilities`、`oc:` 命名空间属性、chunked upload v2 | + Nextcloud 官方桌面/移动客户端可连 | **+30~40%** | 客户端会探测 `status.php` 和 capabilities 来决定行为，字段缺失就降级或拒绝连接 |
| **C. 完整兼容** | B + 分享/外部存储/同步协议全部对齐 | 客户端全功能 | +100%，不建议 | 等于重写一遍 Nextcloud |

> Nextcloud 客户端的探测面（`status.php` → 无认证 PROPFIND 判断 Basic 认证 → `ocs/v2.php/cloud/capabilities`）可参考社区对兼容实现的说明：[karadav NEXTCLOUD.md](https://f-hub.org/Outmode/karadav/raw/commit/442d22eca5eb4de63e05db3709c62604e092b77c/NEXTCLOUD.md)。

**建议**：v1 走 **A**（已决策，见 §8.2），但在 WebDAV 层预留一个 `compat` 中间件，未来若要支持官方客户端，先只做 `status.php` + capabilities 试水，不动核心。

---

## 3. 功能清单

### 3.1 认证与身份（P0）

| 能力 | 说明 |
|---|---|
| OIDC 登录 | **Pocket ID**，Authorization Code + PKCE；服务端会话（HttpOnly `__Host-` cookie，SameSite=Lax）；`state`/`nonce` 校验；ID Token 签名与 `iss`/`aud`/`exp` 校验 |
| 自动开户 (JIT provisioning) | 首次登录按 **`(issuer, sub)`** 建用户（`sub` 永久不变）；`preferred_username`/`email`/`name`/`groups` 映射到本地资料与组 |
| 组 → 角色映射 | 申请 `groups` scope（Pocket ID 需显式申请），按**组名**（如 `media`）映射到本地角色（admin / user / guest）；映射表可配置 |
| 登出 | RP-initiated logout + 本地会话作废；**注意**：Pocket ID 是 passkey-only，不存在"改密码后失效其他会话"的机制，撤销全靠我们本地 + 刷新时 Pocket ID 的组校验 |
| 断链兜底 | **break-glass 本地管理员（必须，非可选）**：独立账号 + Argon2id + TOTP，仅管理网段可访问，所有操作告警。另需纯配置文件的紧急开关 |
| WebDAV / 客户端认证 | SSO 无法在 Basic 认证里用 → 用户在网页端生成 **App Password（应用专用密码）**，只存 Argon2id 哈希，可设名称/过期时间/只读 scope/随时吊销；认证失败记录审计 |
| 账号生命周期 | **SCIM 2.0 服务端端点**（Users/Groups 子集），接收 Pocket ID 的推送（5 分钟合并 + 每小时全量），解决"用户被删/停用我们不知道" |
| 管理员二次验证 | 敏感操作（改配额、吊销分享、导审计）要求 step-up（TOTP 或重新过 IdP） |
| 会话管理 | 用户可见自己的设备/会话列表，可单个或全部踢下线 |

### 3.2 用户 / 组 / 权限（P0）

- 用户：`username`、`email`（唯一）、显示名、状态（active/disabled）、配额、已用空间。
- 组：成员管理、组级配额与策略（是否允许外链、默认分享策略、单文件大小上限）。
- 角色：`admin`（全局）、`user`、`guest`（只能访问被分享给它的内容）。
- 权限模型：在 node 上做 **ACL（owner + 组 + 显式授权）**，不用"路径前缀"授权（路径会变，ID 不会）。
- 多用户隔离：每个用户一个逻辑命名空间根；管理员可挂载"公共空间/部门空间"。

### 3.3 存储与配额（P0）

- 存储后端：**只支持两种 —— 本地盘（默认）+ S3 兼容（MinIO/AWS/Ceph RGW）**。接口做窄（`Put/Open/Delete/Stat`），不再讨论 SMB/NFS 等第三种；见 §8.7。
- 存储布局（部署级可选，见 [0003](0003-storage-layout.md)）：
  - `blob`：内容按 SHA-256 存为不可变对象（`ab/cd/<hash>` 分片），目录树与属性在 PostgreSQL。去重、版本只占差异。
  - `tree`：`<root>/<owner>/<路径>`，磁盘即目录树。可 `ls`、可 rsync，无去重、版本是完整副本。
- **元数据与内容分离是两种布局共同的底线**：容量一大，就会遇到"备份追不完、迁移要搬全部数据、改名要遍历整棵树"的问题。分开存之后，备份只需增量追内容，迁移 = 换后端不动元数据。
- 配额：按用户/组；软告警阈值 + 硬上限；上传前检查（避免写到一半失败）。`used_bytes` **增量维护**，禁止全库 `COUNT(*)`，另配定期对账任务。
- 单文件上限：全局硬上限 **1 TB**，默认软上限 **10 GB**（可按组调高至 100 GB），超限返回 `507`。
- 回收站：删除进回收站，可配置保留天数（默认 30 天）。
- 版本：同一 node 保留最近 N 版（默认 10 版 / 30 天），可恢复。
- 秒传/去重：同 hash 不重复写（仅 `blob` 布局），但**逻辑上每个用户独立**（配额各自计、删除不影响他人）。

### 3.4 WebDAV（P0）

必做（RFC 4918 / 4918bis）：

| 项 | 要求 |
|---|---|
| 方法 | `OPTIONS` `PROPFIND`（Depth 0/1/infinity，infinity 可配置禁用）`PROPPATCH` `GET` `HEAD` `PUT` `DELETE` `MKCOL` `COPY` `MOVE` `LOCK` `UNLOCK` |
| 死属性持久化 | Windows/macOS 依赖自定义属性（如 `Win32LastModifiedTime`），**必须持久化到 DB**，不能只放内存 |
| 锁 | 支持 exclusive write lock，锁 token 落库，超时释放；`If` 头的 lock token 校验 |
| Range | `Range: bytes=` 单区间必做（断点/拖动进度条），多区间可选；`If-Range` + `ETag` |
| 条件请求 | `If-Match` / `If-None-Match` / `If-Modified-Since` / `If-Unmodified-Since` |
| 属性别名 | `DAV:quota-available-bytes`、`quota-used-bytes`、`resourcetype`、`getcontentlength`、`getetag`、`getlastmodified`、`creationdate`、`supported-report-set` |
| 客户端怪癖 | Windows mini-redirector：必须 HTTPS + 有效证书，Basic 认证，默认 50MB 上限需调；macOS Finder：`.DS_Store`/`._*` AppleDouble 文件、文件夹上传的奇怪 `PROPFIND` 序列、要求 `LOCK` 先于 `PUT` |
| 错误语义 | `401` 必须带 `WWW-Authenticate: Basic realm=...`，`423`（Locked）、`507`（Insufficient Storage）、`412`、`424` 要正确 |
| 大文件 | 流式读写，内存占用与文件大小无关；上传临时文件 + 原子 rename（**不能部分写入可见**）；分片 64–256 MB |

**为 v1.1「Windows 映射网络驱动器」预留的 v1 约束**（详见 §8.5，不是重做而是"现在就要满足"）：HTTPS + 受信任证书、Basic 认证可用（App Password）、锁持久化到 DB、死属性持久化、`DAV: 1,2` 与 `MS-Author-Via: DAV`、Unicode NFC 归一化。

P1：`REPORT`（`sync-collection`，RFC 6578）—— 客户端增量同步需要，也是 `rclone` 类工具的高价值项。
P1：`litmus` 兼容性测试套件进 CI。

> Go 的 `golang.org/x/net/webdav` 提供 RFC 4918 参考实现，但默认**不持久化死属性**、锁是内存态、也没有配额/分块上传，只能当骨架用（[pkg.go.dev](https://pkg.go.dev/golang.org/x/net/webdav)）。Rust 侧有 `dav-server`（`webdav-handler` 的 fork），语义更完整（[crates 信息](https://packages.ecosyste.ms/registries/crates.io/keywords/webdav-server)、[Debian 打包评审](https://dfsg-new-queue.debian.org/reviews/rust-dav-server/0.11.0-1)），但同样需要自己接存储与鉴权。

### 3.5 网页端基础功能（P0）

- 上传：拖拽、多选、文件夹上传（`webkitdirectory`）、**分片 + 断点续传**（建议直接上 **tus 协议**或自研等价物）、进度条、失败重试、取消。
- 下载：单文件流式；**多选打包下载**（Zip 流式生成，`Content-Disposition: attachment`，注意 Zip64 与 4GB+ 场景）。
- 浏览：网格/列表、面包屑、排序、搜索（文件名）、重命名、移动、复制、删除、回收站、版本历史。
- 预览（P0 基础）：图片、PDF、纯文本/代码、音视频（浏览器原生支持范围内）。
- 前端技术栈：SvelteKit 或 React + Vite（独立于后端语言）。**必须走 BFF 模式**，不要给 SPA 发长期 token；用同样例 Cookie 会话。

### 3.6 分享（P0，核心差异化）

三种分享类型，统一建模为 `share` + 权限位：

**(a) 公开链接分享**
- 可选密码（Argon2id 哈希）、可选过期时间（绝对时间 / N 天后）、可选**最大下载次数**（每次成功下载计数，并发要原子递增，防绕过）。
- 权限：仅预览 / 只读下载 / 可上传到该目录（投递箱）/ 可编辑。
- 「仅预览、禁止下载」= 服务端转码出水印图/PDF 流，**不下发原文件**（P1）。
- 可选：允许下载的 IP/CIDR 或国家限制、域名 Referer 白名单、需登录（内部用户）才能访问。
- 链接形态：`https://<share-domain>/s/<token>`（token 为 128bit 随机，**DB 只存哈希**），可自定义别名（P2）。
- 安全：`X-Robots-Tag: noindex`、防暴力猜 token 的限流、失效链接返回统一 404（不泄露存在性）。

**(b) 分享给指定邮箱**
- 收件人填邮箱 → 发通知邮件；访问时：
  - 若该邮箱已是内部用户 → 直接授权到其账号（推荐，最安全）；
  - 若是外部邮箱 → 邮件发送一次性验证码/魔法链接，验证通过才可访问（**这就是"只给某人"的实现方式**，避免链接被转发滥用）。
- 记录：邀请时间、首次访问、验证时间、下载次数、失效时间。
- 收件人可见列表 + 一键撤销。

**(c) 上传请求 / File Request（投递箱）**
- 面向"让对方往指定目录临时传一次/几次文件"：
  - **只能上传，不能浏览目录内容、不能下载、不能删除**（服务端强制，不是前端隐藏）。
  - 次数限制：链接级 `max_uploads`（1 次 / N 次 / 不限）、单文件大小上限、总大小上限、文件数量上限、MIME/扩展名白名单、可选截止时间。
  - 匿名上传者可留姓名/邮箱 + 邮件验证（可选），用于"谁传的"追溯；上传成为独立 node，uploader 记在元数据里。
  - 上传完成通知（邮件/Webhook 给目录所有者），可配置"审核后入库"（P2）。
  - 反滥用：每 IP 限流、验证码（可选）、超配额直接拒绝、文件类型嗅探（不仅是扩展名）。
  - 到期后自动失效并清理临时目录。
- 与 (a) 的区别：这类分享默认 `hide_contents=true`，且权限位只有 `create`。

**分享通用能力**
- 分享列表页（我发出的 / 分享给我的），可改密码/改期限/立即撤销/延长。
- 到期前 N 天邮件提醒所有者（P1）。
- 分享变更全部进审计。

### 3.7 审计与合规（P0）

这是"不能让 PHP 方案糊弄过去"的部分，必须从第一天就是一等公民。

**事件模型**（append-only，写多读少，独立表/独立库）：

```
audit_events(
  id            bigserial,
  ts            timestamptz,
  actor_type    enum(user, anonymous, system, admin),
  actor_user_id uuid null,
  actor_email   text null,          -- 匿名上传/邮箱分享时的身份
  actor_ip      inet,
  user_agent    text,
  action        text,               -- 见下表
  target_kind   enum(node, share, user, group, setting, session),
  target_id     text,
  share_id      uuid null,
  result        enum(ok, denied, error),
  bytes         bigint null,
  metadata      jsonb,              -- 文件名、路径快照、旧值/新值、失败原因
  prev_hash     bytea,
  hash          bytea               -- sha256(prev_hash || 规范化 JSON)
)
```

必记 action（至少）：

- `auth.login.success` / `auth.login.failure` / `auth.logout` / `auth.session.revoke` / `auth.apppassword.create|revoke`
- `share.create` / `share.update` / `share.revoke` / `share.password.verify.failure` / `share.expired` / `share.limit_reached`
- **`share.download`**（谁、哪个分享、哪个文件、字节数、IP、UA、第几次、是否通过邮箱验证）
- `share.view` / `share.upload`（投递箱收件）
- `file.read` / `file.write` / `file.delete` / `file.restore` / `file.move` / `file.download_zip`
- `user.create` / `user.disable` / `user.quota.change` / `group.member.change`
- `admin.setting.change` / `audit.export`
- `security.anomaly`（同一 IP 大量下载、异常时间段批量访问等）

**防篡改**：
- 单条 hash 链（`prev_hash`）能防"悄悄改一条"，但防不了"整段重写"。
- 真防篡改需要**外部锚定**：每日把链尾 hash 写入独立 WORM 存储 / 只写对象存储 / 发到外部 SIEM / 每天邮件+S/MIME 签发给安全管理员。
- 审计表 DB 用户权限分离：应用只有 `INSERT`，没有 `UPDATE`/`DELETE`（PostgreSQL `REVOKE`）。
- 保留期：可配置（如 1 年/3 年），到期归档而非删除。

**投递与集成**（P1）：`syslog`（RFC 5424 / CEF）、OpenTelemetry、Webhook、CSV/JSON 导出（导出动作本身也记审计）。

**用户可见的「访问记录」**（P1）：分享所有者能看到"谁在什么时候下载了几次"，这是很多企业用户的真实诉求。

### 3.8 安全基线（P0，横向约束，不是功能）

必须写进设计约束并进 CI：

1. **内容与主站不同源**：用户上传内容通过 `usercontent.<domain>`（或独立端口/独立域名）提供，主站 cookie 不跨域；`Content-Disposition: attachment`、`X-Content-Type-Options: nosniff`、`Content-Security-Policy: sandbox`。
2. **禁止内联渲染**用户上传的 HTML/SVG/XML（预览时转成图片或强制下载）。存储型 XSS 是文件类的头号问题。
3. 路径安全：所有用户输入路径走**唯一的规范化函数**，拒绝 `..`、绝对路径、NUL、Windows 保留名、超长；存储层用 ID 而非路径，路径只在解析时出现一次。
4. SSRF：外部存储/Webhook/邮件服务器/URL 预览一律走**出站白名单代理**，禁止访问 `169.254.169.254`、`127.0.0.0/8`、内网段（可配置放开）。
5. Zip/解压类操作：路径穿越、Zip 炸弹（限制解压比与总大小）。（P1）
6. 上传内容：病毒扫描（ClamAV 或 ICAP）在写入"可见"状态前完成；上传先落隔离区，扫描通过再入库。（P1，但强烈建议 v1.1 就上）
7. 认证：Argon2id；所有 token 只存哈希；恒定时间比较；登录/分享密码验证限流（按 IP + 账号 + token）。
8. HTTP 头：HSTS、`X-Frame-Options: DENY`（分享页可单独 `SAMEORIGIN`）、严格 CSP、`Referrer-Policy: no-referrer`、`Permissions-Policy`。
9. CSRF：所有状态变更接口双提交 token 或 `SameSite=Strict` + Origin 校验。
10. 依赖治理：最小依赖、`cargo audit` / `govulncheck` 进 CI、SBOM、锁定版本、禁止动态插件加载。
11. 日志脱敏：不记录密码、token、文件内容；IP 按合规要求做截断/留存策略。

### 3.9 建议新增的功能（不在原始需求里，但强烈建议）

按价值/成本排序：

**建议纳入 v1（低成本高收益）**

| 功能 | 理由 |
|---|---|
| **App Password / 设备授权** | 没有它 WebDAV 在 SSO 环境下根本没法用，是**硬依赖**而非增强 |
| **病毒扫描** | 文件分享 + 外网上传 = 恶意文件高速公路；一条 ClamAV 集成能挡住大部分事故 |
| **限流与反暴力破解** | 分享 token、密码、登录三处必须有 |
| **分享访问告警** | 单链接异常下载量（如 1 小时 > N 次）自动暂停并通知所有者 |
| **动态水印**（PDF/图片，印上"邮箱+时间+IP"） | 对"防泄漏"诉求性价比极高，比 DRM 简单可靠 |
| **管理员操作二次认证 + 全量管理员审计** | 内控/等保审计必查项 |
| **回收站 + 版本** | 防误删/防勒索的最低配置；勒索软件场景下版本是唯一救命的东西 |
| **Webhook + REST API** | 对接企业微信/钉钉/Slack、工单、DLP，几乎零成本 |
| **Prometheus / 健康检查 / 结构化日志** | 运维基础 |

**建议 v1.1**

| 功能 | 理由 |
|---|---|
| **文件请求的邮箱验证 + 上传者身份** | 你的需求里已隐含"谁传的"，加上验证才完整 |
| **分享访问者列表（审计可视化）** | 用户自助合规，减少 IT 工单 |
| **在线预览增强**：视频转码、HEIC、RAW | 影响体验，不影响安全 |
| **全文检索**（Tantivy / Meilisearch） | 文件一多，没搜索等于没有 |
| **保留策略 / 法务保留 (legal hold)** | 合规场景必需 |
| **外链审批流**（外链需管理员批准） | 金融/医疗/政企常见要求 |
| **分享链接到期自动续期提醒** | 纯体验 |

**不建议做（明确列出，防止范围蔓延）**

- ❌ **插件/应用市场**：这是 Nextcloud 最大的攻击面来源，坚决不做。集成一律走 Webhook/API/独立服务。
- ❌ **端到端加密（E2EE）**：与"审计谁下载了""病毒扫描""水印""禁止下载"**根本冲突**。二者只能选一个。v1 明确不做；如确有需求，做成"仅个人保险箱"模式的独立功能（P2），且不做审计承诺。
- ❌ **联邦分享（Federation）**、**日历/通讯录/邮件客户端**：不是本项目的价值点。
- ❌ **自研在线 Office 编辑器**：接 OnlyOffice / Collabora（WOPI 协议）即可，这是另一个人年。
- ❌ **自研加密算法/自研 token 格式**：用标准库。

### 3.10 Non-goals（v1 明确不做）

联邦、E2EE、插件、日历/联系人/邮件、多租户 SaaS 计费、移动原生 App（用 PWA）、离线桌面同步客户端（用标准 WebDAV + 系统挂载）。

---

## 4. 语言选型

### 4.1 候选技术栈

**Rust**

- Web：`axum` + `tokio` + `hyper` + `tower` 中间件
- WebDAV：`dav-server`（`webdav-handler` fork，支持 lock / 死属性）
- DB：`sqlx`（编译期 SQL 校验）+ PostgreSQL
- 对象存储：`object_store` crate（统一 S3/本地/GCS）
- 认证：`openidconnect`、`argon2`、`rand`
- 邮件：`lettre`；可观测：`tracing` + `opentelemetry`；API：`utoipa`
- 前端：SvelteKit（独立构建，静态资源 `rust-embed` 内嵌）

**Go**

- Web：`net/http`（1.22+ 路由）+ `chi` 或纯标准库
- WebDAV：`golang.org/x/net/webdav` 作骨架（XML 类型 + 方法分发），自己实现 propstore/锁持久化/分块上传
- DB：`pgx` + `sqlc`（生成类型安全查询）+ `goose`/`atlas` 迁移
- 对象存储：`aws-sdk-go-v2` 或 `minio-go`
- 认证：`coreos/go-oidc` + `golang.org/x/crypto/argon2`
- 邮件：`wneessen/go-mail`；可观测：`slog` + `otel`；API：`oapi-codegen`
- 前端：同上

### 4.2 对本项目关键需求的匹配度

| 维度 | Rust | Go | 说明 |
|---|---|---|---|
| 流式大文件 / Range / 背压 | 好（`Body::from_stream`） | **很好**（`io.Copy` 直白） | Go 在这个场景的心智负担更低 |
| WebDAV 语义完整度 | **好**（`dav-server` 更接近 RFC） | 一般（需自己补死属性/锁/配额） | 但补齐约 500–800 行，可控 |
| 死属性/锁持久化 | 需自己接 DB | 需自己接 DB | 平手 |
| 编译期保证路径/授权正确 | **强**（newtype 可让路径错误编译不过） | 弱（靠约定 + review） | 见 §4.4 |
| 开发速度（首个可用版本） | 慢 30–60% | **快** | 主要差在 borrow checker 与编译等待 |
| 单二进制部署 | 很好（~20–40MB） | **很好**（~15–25MB，交叉编译更简单） | 平手偏 Go |
| 运行内存（1k 并发） | 更低 | 低（GC 对本场景无感） | 差异不构成决策依据 |
| 排查线上问题 | 好（`tracing`） | **很好**（`pprof` 开箱即用） | Go 略优 |
| 招聘/团队上手 | 难 | **易** | 组织现实，权重不低 |
| 生态成熟度（OIDC/邮件/OSS） | 够用 | **更成熟** | `coreos/go-oidc` 是事实标准 |
| 长期维护（10 年） | 好（无 GC、无隐式行为） | 好 | 平手 |

### 4.3 决策矩阵（1–5 分，加权）

权重组 A：**以交付速度与团队可持续性为先**（推荐给内部工具）

| 维度 | 权重 | Rust | Go |
|---|---|---|---|
| 交付速度/可维护人力 | 30% | 3 | 5 |
| 安全正确性（内存/类型/并发） | 25% | 5 | 3.5 |
| WebDAV 生态 | 15% | 4 | 3 |
| 性能/资源占用 | 10% | 5 | 4 |
| 运维/部署 | 10% | 5 | 5 |
| 招聘/上手 | 10% | 2 | 5 |
| **加权** | | **3.95** | **4.23** |

权重组 B：**以"安全产品"定位为先**

| 维度 | 权重 | Rust | Go |
|---|---|---|---|
| 交付速度 | 20% | 3 | 5 |
| 安全正确性 | 40% | 5 | 3.5 |
| WebDAV 生态 | 15% | 4 | 3 |
| 性能/资源 | 10% | 5 | 4 |
| 运维 | 5% | 5 | 5 |
| 招聘 | 10% | 2 | 5 |
| **加权** | | **4.15** | **4.00** |

**读法**：两种权重下差距都在 0.2 左右 —— 说明**语言不是决定性因素**，真正的决策变量是"团队会不会写"。

### 4.4 结论与推荐

> **已定案（2026-10-21 第三轮）**：**Go**。决策已锁定，不再重新评估。下面保留原始论证，作为决策依据备查。

**推荐：Go**，理由：

1. 本项目 80% 的复杂度在 IO/协议/权限/审计，不在计算；Go 的并发与流式模型在这里最省事，`pprof` 也最省事。
2. WebDAV 的缺口（死属性、锁、配额、分块上传）是**明确的、有界的**工作，Go 补得起；反过来 Rust 的 `dav-server` 仍然要审一遍代码并接存储，收益没有想象的大。
3. 交付速度决定这个项目能否活下去 —— 重写 Nextcloud 的最大风险不是漏洞，而是**做不完**。
4. 单二进制 + `CGO_ENABLED=0` 交叉编译，部署和回滚最简单：小规模是 `docker compose up`，大规模是同一个镜像横向起多份，不需要为两种部署写两套代码。
5. Go 的运行模型（goroutine + `io.Copy` + `net/http` 标准库）在"很多并发长连接、每连接都在搬字节"这件事上心智负担最低，而这正是文件服务的主循环，与集群有多大无关。

**（已失效）改选 Rust 的条件**：原定为"团队有 1 年以上生产 Rust 经验"等。本轮已确认不按团队经验做选型，故该条件作废，Go 为最终选择。

**不管选谁，都要照搬 Rust 的"类型化安全"思路**（这是真正的安全收益来源）：

```go
// 反例：到处都是 string 路径，靠人记
func (s *Store) Open(userID, path string) (...)
func (s *Store) Open(userID, path string) (...)   // 忘了校验？编译器不管

// 正例：ID 与路径都是私有构造的新类型，跨界必须显式转换
type NodeID struct{ /* 私有字段，由 ResolvePath 或 DB 产出 */ }
type VPath  struct{ /* 私有字段，只能由 Normalize() 构造，已校验 */ }

func (s *Store) Open(u UserID, id NodeID, v VPath) (*Blob, error)
// 授权检查只有一个入口：Authorize(actor, node, perm) —— 其余地方拿不到 raw path
```

配套测试策略（语言无关）：

- 路径/对象名 **fuzz 测试**（`go test -fuzz` / `cargo-fuzz`）：`..`、NUL、Unicode 归一化、`.`/`..`、超长、Windows 保留名。
- **授权矩阵测试**：每个 API × 每种角色 × 每种资源归属，穷举断言（这是防 IDOR 的唯一可靠办法）。
- 分享 token 的并发下载计数：原子性 + 竞态测试（`-race`）。
- 依赖扫描 + SBOM 进 CI。

### 4.5 不重写的选项（应至少评估一次）

老实说，你的需求清单有 60–70% 与现成项目重合，重写的机会成本要算清楚：

- **SFTPGo**（Go，AGPLv3）：WebDAV/FTP/SFTP/S3 统一网关，有分享（密码、有效期、权限）、配额、OIDC/SSO、审计事件、REST API。缺的是"面向普通用户的好用网页端"和"file request / 邮箱分享"。
- **Seafile / Pydio Cells**：功能接近，但同样是大系统。
- **rclone serve webdav + 自建前端**：只适合极小规模。

**判断**：如果你要的是"完全可控、审计可定制、无插件攻击面"的内部系统，自研成立；如果只是"受不了 Nextcloud 更新和安全公告"，fork SFTPGo 或换 Seafile 的成本可能只有自研的 1/5。**建议在 M0 之前花 2 天做一次这三个的试用评估**，结论写进本文件。

> 注意 AGPLv3：内部自用无影响；如果要对外提供 SaaS，AGPL 有网络使用条款。

---

## 5. 架构草案（语言无关）

```
                    ┌──────────────┐
   浏览器 SPA ─────▶│  BFF / API   │──▶ 会话(cookie) ──▶ IdP (OIDC)
                    │  (同源)      │
   WebDAV 客户端 ──▶│  DAV 入口    │──▶ App Password 认证
                    └──────┬───────┘
                           │  授权 choke point: Authorize(actor, node, perm)
                    ┌──────▼───────┐        ┌──────────────┐
                    │ 元数据 (PG)  │◀──────▶│  审计 (PG)   │──▶ syslog / OTel / WORM 锚定
                    └──────┬───────┘        └──────────────┘
                           │ node → blob key
                    ┌──────▼───────┐        ┌──────────────┐
                    │ Blob 存储    │        │ 异步 Worker  │──▶ 病毒扫描 / 水印 / 缩略图 / 邮件 / Webhook
                    │ FS | S3      │        └──────────────┘
                    └──────────────┘
           内容域: usercontent.<domain>（独立 origin，强制 attachment）
```

关键约束：

1. **下载永远经服务端代理**（不用纯 S3 presigned URL），否则无法记录"谁下载了"、无法计次、无法加水印。可以用短时签名 URL 指向**自己**的下载端点，避免把它变成公共 CDN。
2. 上传：小文件直传；大文件分片（tus 或 S3 multipart）。若用 S3 直传，需在完成回调里做扫描与入库 —— 简化为 v1 全部走服务端代理。
3. 异步任务用**outbox 表 + worker**，不引入额外 MQ。outbox 是事务性的，天然支持"多个 worker 争抢同一张表"，所以从单进程到多副本都不需要换架构（只是 worker 数量变化）。将来真需要独立队列时，outbox 的消费端换掉即可，业务代码不动。
4. 审计写入与业务写入**同事务**（outbox），保证"没审计就没有操作"。
5. **部署形态不是架构前提**：同一份二进制既要能 `docker compose up` 单机跑，也要能多副本跑。约束是"**不要在业务代码里假设自己是唯一进程**"——具体见 §8.7 与 §8.11。

数据模型草案（PostgreSQL）：

```sql
users(id uuid pk, username citext unique, email citext unique, display_name text,
      idp_issuer text, idp_subject text, role text, status text,
      quota_bytes bigint, used_bytes bigint, created_at timestamptz,
      unique(idp_issuer, idp_subject));        -- Pocket ID: sub 永久不变

groups(id uuid pk, name citext unique, display_name text, quota_bytes bigint);
group_members(group_id, user_id, pk(group_id, user_id));

-- IdP 组名 → 本地角色/策略 的映射表（Pocket ID 的 groups claim 是组名）
group_role_map(idp_group_name citext pk, role text, policy_id uuid null);

-- SCIM 推送幂等所需：记 externalId / 版本
scim_sync_state(entity text pk, external_id text, last_synced_at, etag text);

sessions(id uuid pk, user_id, created_at, last_seen_at, ip inet, ua text,
         auth_method text,                    -- oidc | local_breakglass
         amr text[], revoked_at timestamptz);

app_passwords(id uuid pk, user_id, label text, secret_hash bytea, scopes text[],
              expires_at, last_used_at, revoked_at);

nodes(id uuid pk, parent_id uuid null, owner_id uuid, name text, kind text,
      size bigint, mime text, etag text, blob_id uuid null,
      created_at, modified_at, trashed_at timestamptz, version_of uuid null);
-- 唯一性: unique(parent_id, name) where trashed_at is null
-- 索引: (owner_id, parent_id), (blob_id)

blobs(id uuid pk, sha256 bytea unique, size bigint, storage_backend text,
      storage_key text, refcount int);
-- 大容量: (sha256) 索引支撑秒传; storage_key 形如 ab/cd/<sha256> 分片

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
-- 锁必须落库：内存锁在多副本 / 重启后会失效，导致 Office 协同写入损坏

dead_props(node_id, ns text, name text, value text, pk(node_id, ns, name));
-- Windows 映射 / macOS Finder 都依赖它

audit_events(...)  -- 见 §3.7；按月 PARTITION，应用仅有 INSERT 权限
```

**大容量相关的索引/分区要求**（容易在 v1 埋雷）：

- `nodes` 的 `unique(parent_id, name)` 必须是 **partial index**（`where trashed_at is null`），否则回收站会阻塞同名重建。
- `dead_props` 每条 node 最多几个属性，但要按 `node_id` 聚簇，否则删除文件时 `PROPFIND` 会放大成 N 次查询。
- `audit_events` **按月分区**，1 年上亿行；只追加 + 到期 `DETACH PARTITION` 归档到对象存储。
- 大目录（单目录 >10 万文件）的 `PROPFIND Depth:1` 必须**流式输出 + 分页保护**，否则一次请求打爆内存。建议对单目录条目数设阈值（如 10 万），超限要求分页或返回 `507`/自定义错误。

---

## 6. 里程碑与估算（单人全职、Go 基线）

| 阶段 | 内容 | 人周 |
|---|---|---|
| M0 骨架 | 仓库/CI/**Docker Compose**、**Pocket ID OIDC 登录**、会话、用户与组、**SCIM 端点**、break-glass 管理员、审计表与 outbox、healthz/metrics | 2.5 |
| M1 存储与 WebDAV | **blob 内容寻址存储（local + s3 双实现）**、节点树与权限、WebDAV 全方法 + 锁 + 死属性、App Password | 4 |
| M2 网页端 | 浏览/上传（分片断点、大文件通道）/下载/打包/回收站/版本/重命名移动 | 3.5 |
| M3 分享 | 链接（密码/期限/次数）、邮箱分享（验证码）、file request、分享管理页 | 3.5 |
| M4 审计与加固 | 审计可视化、导出/syslog、告警规则、安全头、限流、fuzz + 授权矩阵测试、渗透测试修复 | 2.5 |
| M5 迁移与上线 | Nextcloud 数据迁移工具、**备份恢复演练（验收项）**、运维文档 | 2 |
| **合计** | | **≈18 人周（单人约 4.5 个月；双人约 2.4 个月）** |

不含：在线 Office 集成（+3 人周，v1 只留接口）、Nextcloud 客户端兼容（+5~7 人周）、E2EE（不估）。
v1.1 追加：Windows 映射兼容调优 + litmus + rclone/Office 端到端（+2 人周）、病毒扫描（+1 人周）。

估算保守假设：无现成账号体系可复用、需要自建前端、需要过一轮外部渗透测试。**历史经验：文件系统的实际工期通常是估算的 1.3–1.5 倍**，"路径/权限的边界情况"是主要吞噬者。

---

## 7. 主要风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| 授权逻辑分散导致 IDOR | **最高** | 单一 `Authorize()` 入口 + 穷举矩阵测试 + 代码评审必查项 |
| **备份做了但没验过**（"备份等于没有备份"） | 极高 | 恢复演练必须真实执行并记录耗时；监控"最后成功备份时间" |
| **代码里出现"我是唯一进程"的假设**（会话/锁/队列存进程内） | 极高 | 协调一律走 DB（`sessions`、`lock_tokens`、`jobs` 表）；缓存只做加速做不了真相。见 §8.7 |
| 客户端兼容长尾（Windows/macOS/Office） | 高 | v1 只承诺"标准 WebDAV + 三个主流客户端实测通过"，列兼容矩阵并在 CI 跑 `litmus`/`rclone` 冒烟 |
| 分享计数/期限的并发漏洞 | 高 | DB 原子更新 + `-race` 测试 + 单测覆盖"刚好卡在第 N 次" |
| **部署单点**（若某次安装只有一台机器，其可用性上限就是那台机器） | 高（部署侧） | 这是部署选择的后果，不是产品缺陷：PG 流复制 / S3 后端 / 应用多副本都是**只改部署不改代码**；季度恢复演练为 M5 验收项 |
| 大容量下全量备份窗口不足 | 高 | 内容增量（restic）+ PG WAL 归档；RPO/RTO 写进文档并季度演练 |
| **Pocket ID 单点故障**（passkey-only，管理员丢设备/服务挂 = 全员进不去） | 高 | break-glass 本地管理员（必须）+ 纯配置文件紧急开关 + 多管理员 passkey |
| 范围蔓延（预览、Office、E2EE） | 高 | 本文档 §3.10 作为护栏，新需求必须进 P1/P2 并标成本 |
| 自研迁移工具读错 Nextcloud 数据 | 中 | 只迁移文件与用户（不动分享），先做只读校验（hash 比对）再切换 |
| 审计"防篡改"承诺过度 | 中 | 明确写"链式哈希 + 外部锚定"，不承诺"绝对不可篡改" |
| **去重引发的"删了但没释放"误解** | 中 | 配额按逻辑计，界面显示"实际占用 vs 去重后占用"两栏（仅 blob 布局） |
| SCIM 配错导致批量停用 | 中 | SCIM 写操作先做"干跑 + 差异预览"，大批量变更需二次确认；7 天软删除 |
| **临时区与正式区跨文件系统**（rename 退化为 copy，拖垮 IO） | 中 | 启动时自检：`blob` 布局做真实 rename 探测，`tree` 布局把 tmp 放在 root 内 |

---

## 8. 决策记录（2026-10-21 第二、三、四轮）

> **第四轮修正**：早期把"用户数 / 容量 / 部署形态"写进了产品定位，这是错的。这些是**某次安装的规模参数**（§8.11），不是产品定义。SFTPGo、Nextcloud、ownCloud 都不这样定义自己——否则等于把一次部署的容量写成了系统的能力上限。产品定位改为"自托管的文件协作平台"，架构按可横向扩展设计，部署按单机起步。

| # | 问题 | 决定 | 类型 | 影响 |
|---|---|---|---|---|
| D1 | IdP | **Pocket ID**（passkey-only） | 产品决策 | 认证层按 Pocket ID 的能力裁剪，见 §8.1 |
| D2 | Nextcloud 客户端兼容 | **v1 不做**（见 §8.2） | 产品决策 | 走 §2 方案 A，省 30–40% |
| D3 | 存储后端 | **本地盘 + S3 兼容，两个就行** | 产品决策 | 接口做窄，见 §8.7 |
| D4 | 存储布局 | **blob / tree 可选**（部署级） | 产品决策 | 见 [0003-storage-layout.md](0003-storage-layout.md) |
| D5 | 在线 Office | **v1 不做，只留接口** | 产品决策 | 见 §8.8（WOPI seam） |
| D6 | Windows 映射网络驱动器 | **v1 不做，v1.1 再做** | 产品决策 | 底层约束 v1 满足，见 §8.5 |
| D7 | 合规定级 | **由本文定基线** | 产品决策 | "等保二级参考 + 12 个月审计"，见 §8.9 |
| D8 | 语言 | **Go** | 产品决策 | 见 §4.4 |
| D9 | 单文件上限 | **默认 10 GB 软 / 1 TB 硬**（可配） | 配置默认值 | 见 §8.4 |
| S1 | 当前安装容量 | **约 40 TiB** | ⚠️ 规模参数 | 决定盘怎么买，见 §8.11 |
| S2 | 当前安装用户数 | **约 2000 人** | ⚠️ 规模参数 | 决定单副本是否够用 |
| S3 | 当前部署形态 | **单机 Docker Compose 起步** | ⚠️ 规模参数 | 加机器不改代码，见 §8.7 |

### 8.1 Pocket ID 的实际影响（重要，改变了两个设计）

Pocket ID 是 passkey-only、无密码的 OIDC IdP，其能力边界直接决定了我们**不能抄 Nextcloud 的账户模型**：

1. **没有密码可迁移，也没有"邮箱找回密码"**。Pocket ID 的账户恢复是管理员发一次性登录码（[Account recovery](https://pocket-id.org/docs/troubleshooting/account-recovery)）。⇒ 我们的系统**绝对不能**提供本地密码登录作为主路径，否则我们立刻变成整个体系的薄弱环节。本地口令只保留给 break-glass 管理员，默认关闭。
2. **`groups` claim 需要显式申请 `groups` scope 才下发**，且下发的是**组名**（如 `media`），不是显示名（[Scopes and claims](https://pocket-id.org/docs/guides/scopes-and-claims)）。⇒ 我们按 `groups` 里的字符串做角色映射，配置项写成"组名 → 角色"的映射表，**不要**用组 UUID。
3. **`sub` 永久不变，`preferred_username` 是用户名**。⇒ 用户主键用 `(issuer, sub)`，用户名只做展示，允许后续变更。
4. **已知坑**：ID Token 首签有 `groups`、用 refresh token 换出来的可能没有（[issue #987](https://github.com/pocket-id/pocket-id/issues/987)）。⇒ **不要**用 ID Token 里的 groups 做实时授权判断，权限以本地库为准；每次刷新只在登录时同步一次组。
5. **刷新时 Pocket ID 会校验用户仍存在、未禁用、仍在客户端的允许组内**，移出组后下次刷新即失效（30 天不活动过期）[同上]。
   ⇒ 我们的会话生命周期可以放长（例如 12h 会话 + 静默续期），**权限撤销依赖 Pocket ID 的组校验 + 我们每次登录/定时同步的组变更**。
6. **Pocket ID 支持 SCIM 推送用户/组**（5 分钟合并推送 + 每小时全量，[SCIM 文档](https://pocket-id.org/docs/configuration/scim)）。⇒ **强烈建议 v1 就实现 SCIM 2.0 服务端端点**（只做 Users/Groups 的 GET/POST/PATCH/DELETE 子集）。
   - 理由：OIDC 只在用户**登录时**告诉我们他是谁，**用户被删/被停用我们永远不会知道**。这是自研系统最典型的"僵尸账号"来源。
   - 成本：约 3–5 人日；收益：账号生命周期自动化 + 满足审计要求。这是我在这轮回答里**唯一想加进 v1 的功能**。
7. Pocket ID 只签 RS256（可改），JWKS 走标准 `jwks_uri`。⇒ 直接用 `coreos/go-oidc` 的标准发现即可，无需定制。
8. **管理员本身也是 passkey-only**。⇒ 如果 Pocket ID 挂了/管理员丢了设备，整个集群没人能登录。**必须有 break-glass**（见下）。

**因此新增两条硬性要求：**

- **Break-glass 本地管理员**（从 P0 的"可选"升级为**必须**）：独立账号 + Argon2id + TOTP，仅允许从管理网段访问，所有操作强制审计并邮件告警。用途：Pocket ID 不可用、组映射配错把自己锁在外面。
- **紧急绕过开关**：配置项 `auth.allow_local_admin=true` 需要能纯配置文件开启（不依赖数据库），防止"数据库里的 admin 被自己禁用"这种死锁。

### 8.2 关于"要不要兼容 Nextcloud 客户端"——我的建议：**不做，但留出扩展点**

**为什么建议不做（同意你的判断，且理由比"省工作量"更强）：**

1. Nextcloud 客户端的兼容面不是"实现几个 API"，而是**一整个隐式协议**：`status.php` 的能力协商、`oc:` 命名空间属性、chunked upload v2 的组装语义、`X-Request-ID`、增量同步的 etag 语义、错误码映射……每一项都有大量未文档化的客户端行为依赖。这类兼容层的工作量是"实现一个 WebDAV 服务"的数倍，且不可估算。
2. **兼容层会成为我们最大的技术债**：客户端每次小版本更新都可能改变探测行为，我们需要跟着它跑。而我们的目标恰恰是"摆脱别人的节奏"。
3. **大容量下，官方客户端的全量扫描行为是灾难**（见 §8.3）。

**但要注意：不做客户端兼容 ≠ 不能同步。** 用户仍然可以：

| 场景 | 方案 | 是否 v1 |
|---|---|---|
| 桌面文件同步 | 第三方 WebDAV 客户端（rclone mount、Cyberduck、Mountain Duck、GoodSync） | ✅ 标准 WebDAV 即可 |
| 手机相册备份 | FolderSync（Android）、Documents by Readdle（iOS） | ✅ 标准 WebDAV 即可 |
| 与本地文件夹双向同步 | `rclone bisync` / `rclone mount` | ✅ |
| Windows 资源管理器 | 映射网络驱动器（见 §8.5） | v1.1 |
| 差异化体验 | 我们自己的 PWA + 「同步助手」薄客户端（v2 可选） | ❌ |

**留的扩展点**：WebDAV 层做成 `compat` 中间件，v1 不挂载；未来若要支持官方客户端，只需实现 `status.php` + `capabilities` 两个端点先试水，不需要改核心。

### 8.3 大容量对架构的要求（这条比选语言重要得多）

> 这一节讨论的是"**任何**大容量部署都会遇到的问题"，不是"因为这次是 40 TiB 才要这么设计"。具体的容量规划见 §8.11。

容量一大，就**不能把文件当成"用户目录下的一棵树"**，否则备份、扩容、迁移、去重全部失效：

1. **内容寻址 blob 存储**（`blob` 布局）：`sha256` → 不可变对象，按 hash 分片目录（`ab/cd/abcdef...`）。用户看到的目录树只是 DB 里的 `nodes` 记录。
   - 好处：天然去重（同文件多用户只存一份，但配额各算）、备份只需增量追 blob、迁移 = 换存储后端不动元数据。
2. **`tree` 布局是显式的取舍**：它把目录树放在磁盘上，换取"可读、可 rsync、可手工恢复"，代价是没有去重、版本是完整副本、大目录改名很慢。两者都在 [0003-storage-layout.md](0003-storage-layout.md) 里实现并通过同一套契约测试。
3. **元数据必须独立于存储**：PostgreSQL 存树/权限/分享/审计，字节存后端。
   - 容量大到一定程度后 **对象存储（S3/MinIO/Ceph RGW）比本地盘更省心**；但本地盘必须是一等公民，所以**存储后端从第一天就是接口**。
4. **备份**：大容量全量备份不可行。必须 **内容增量 + 元数据每日全量（PG 只有几 GB）**。恢复目标按 §8.11 定。
5. **审计表会是最先爆的**：量一大就上亿行。⇒ **按月分区 + 只追加 + 到期归档**，不要指望单表。
6. **不要做全库 `COUNT(*)` 类的配额统计**：`used_bytes` 用增量维护（上传/删除/版本变化时 `UPDATE ... SET used_bytes = used_bytes ± n`），并提供定期对账任务。

### 8.4 单文件上限：建议 **100 GB 软 / 1 TB 硬**

不用现在拍死，但需要一个数字来决定分片策略：

| 单文件上限 | 需要的机制 | 代价 |
|---|---|---|
| ≤ 5 GB | 单次 PUT 即可 | 低，但用户会撞墙 |
| **≤ 100 GB**（建议软上限） | 分片上传（tus 或 S3 multipart，**分片 64–256 MB**）、断点续传、并行分片、服务端校验 hash | 中，必须 v1 做 |
| ≤ 1 TB（硬上限） | 同上 + 注意 `Content-Length` 精度、Zip64、浏览器端 File API 的 4GB 限制（用分片绕开） | 中 |
| > 1 TB | 需要专用传输工具 | 不建议 |

**建议**：
- 全局硬上限 **1 TB**（技术上限，用于防滥用）；
- 默认软上限 **10 GB**（可按组调高到 100 GB），超限返回 `507`；
- **超大文件（>10GB）走独立的"大文件通道"**：分片 + 秒传 + 断点续传，且**默认不生成缩略图/不参与全文索引**（否则大容量下后台任务会失控）。

### 8.5 Windows 映射网络驱动器：**底层 v1 就要做对，功能 v1.1 再开**

同意"后续版本支持"，但要注意：**这件事不是"加一个功能"，而是"v1 的 WebDAV 实现必须满足一组约束"，否则 v1.1 要重写。**

Windows WebClient（mini-redirector）的硬约束：

| Windows 的要求 | 对 v1 的约束 |
|---|---|
| **必须 HTTPS + 证书链受信任**（不接受自签，除非改注册表） | v1 就要上正规证书（内网也要有内部 CA） |
| **只支持 Basic 和 NTLM**，不支持 Bearer/Cookie | v1 **必须**有 App Password + Basic 认证（已在 P0） |
| **`Basic` 认证的凭据会在每次请求重发** | 密码校验必须能扛住高频（Argon2id 参数不能太高，或加内存缓存） |
| 默认请求超时 ~30s，字节上限 50MB，需注册表调 | 大文件必须走分片，且服务端要能接受"几秒内建连、慢速写"的模式 |
| `PROPFIND Depth:1` 一次列目录，属性缺失会显示"大小未知" | **死属性持久化 + `getcontentlength` 必须正确**（已在 P0） |
| 需要 `LOCK` 才能安全编辑 Office 文件 | 锁必须**持久化到 DB**，不能只在内存（已在 P0，强调：单实例内存锁在重启/多副本下会失效） |
| 对 `OPTIONS` 的 `DAV:` 响应头敏感 | `DAV: 1,2` + `MS-Author-Via: DAV` |
| 中文路径/Unicode 归一化敏感 | 路径规范化必须做 Unicode NFC 归一化 + 大小写不敏感匹配选项 |

**结论**：v1 里 WebDAV 的 P0 项（Basic 认证、锁持久化、死属性、quota 属性、HTTPS）已经覆盖了 Windows 需要的 90%。**v1.1 只需补"兼容性调优 + 文档 + 测试矩阵"**，而不是重做。

除了 Windows 映射，v1.1 建议同批做：

- **rclone/Linux 挂载的兼容测试**（`litmus` 测试套件跑通）；
- **Office 直接打开/保存（WebDAV + LOCK）** 的端到端验证；
- **驱动器映射的安装脚本/组策略文档**。

### 8.6 仍然待确认

已全部关闭。本轮（第三轮）确认：存储只做本地盘 + S3；在线 Office 只留接口；合规基线由本文件确定；语言锁定 Go。剩余未定项将作为实现期决策在 ADR 中逐一记录，不再阻塞 M0。

### 8.7 部署形态与可扩展性（不是架构前提）

> **先纠正一个措辞错误。** 早期版本把"单机 Docker"写成架构基线，进而推出"不需要 Redis / 不需要分布式锁 / 进程内缓存足够"。这是把**某次安装的规模**当成了**系统的能力上限**——正是 SFTPGo、Nextcloud、ownCloud 都不这么定义自己的原因。
>
> 正确的分法：**架构按"可能有多副本"设计，部署按"当前单机"起步。** 差别在于代码里不能有"我是唯一进程"的假设；而不是现在就上 K8s。

**架构层面必须守住的（与部署规模无关）**

| 约束 | 为什么现在守 | 违反的代价 |
|---|---|---|
| 不在进程内保存**唯一**状态（会话、锁、任务队列） | 一旦要多副本，全部返工 | 会话丢失/锁失效，等于重写 |
| 锁与任务用数据库做协调（`lock_tokens`、`jobs` 表） | PG 行锁在多副本下天然正确 | 单机内存锁在多副本下**静默失效**，Office 协同会写坏文件 |
| 缓存只做加速，不做真相 | 进程内 LRU 可随时丢弃 | 多副本时缓存不一致变成脏数据 |
| 审计与业务同事务 | 单机多机都一样 | 迁移到多副本时才补，历史数据断层 |
| 文件写入原子发布（temp + rename） | 单机也需要 | 多副本下更危险 |

**部署层面可以现在简化的（将来只改部署，不改代码）**

| 项 | 当前单机 | 何时需要升级 | 升级成本 |
|---|---|---|---|
| 应用副本数 | 1 | 并发或可用性不足 | 加副本 + 共享会话（已无状态） |
| 缓存 | 进程内 LRU | 多副本需一致时 | 接 Redis，接口已抽象 |
| 任务队列 | outbox + 进程内 worker | worker 抢不动或需独立扩缩 | 多起几个 worker 进程即可（已用 `FOR UPDATE SKIP LOCKED`） |
| 对象存储 | 本地盘 | 容量或单点要求 | 换 `storage.backend: s3` |
| PG | 单实例 | 读压力大/高可用 | 加流复制副本 |
| 分库分表 | 不做 | 单库撑不住 | **真正的难点**，见下 |

**唯一一个"将来很难补"的地方：数据库规模。**

单库 PostgreSQL 在合理硬件上支撑千万级 `nodes` 行、亿级 `audit.events` 行没有问题。风险点不在行数，而在**大目录查询**和**审计表膨胀**，这两个在 v1 就要处理好：

- `nodes(parent_id, name)` 走 partial unique index；大目录 `PROPFIND` 有流式输出 + 阈值保护；
- `audit.events` **从第一天就按月分区**——在亿行表上事后加分区是停机事故。

除此之外，没有需要提前做的"分布式改造"。**架构无状态化 + 协调走数据库**，就是能从小到大平滑扩展的全部秘密。

**存储布局**（本地盘方案）：

```
/var/lib/newcloud/
├── pg/                    # PostgreSQL 数据（建议独立物理盘）
├── blobs/ab/cd/<sha256>   # blob 布局：内容寻址，2 层分片
│   └── tmp/               # 上传暂存（必须与正式区同一文件系统）
├── files/<owner>/<path>   # tree 布局：目录镜像（与 blobs 二选一）
├── audit-archive/         # 审计归档（按月）
└── config/                # 配置文件（含 break-glass 配置，与 DB 分离）
```

> ⚠️ **关键实现细节**：临时区与正式区**必须在同一个文件系统**上，否则 `rename(2)` 退化为 copy，大文件规模下会拖垮 IO。这一点已由启动自检强制（`blob/local` 做真实 rename 探测，`tree` 把 tmp 放在 root 内）。

**关于单点：这是部署问题，不是代码问题**

如果这次安装只有一台机器，那可用性上限就是那台机器——但这是**部署选择的后果**，不是产品缺陷。产品要保证的是：加第二台机器时不需要改代码。

1. **备份在任何形态下都是必须的。**
   - PG：`pg_basebackup` 每日 + WAL 归档连续备份（推到另一台机器/对象存储）。RPO 可到分钟级。
   - 内容：大容量不可能每日全量 → 用 `restic`/`borg` 增量（内容寻址天然适合增量，未变 blob 不重传；tree 布局则依赖文件级去重）。
   - **必须做恢复演练**：没有演练过的备份等于没有备份。M5 的验收标准之一是"从裸机恢复到可服务状态，记录耗时"。
2. **消除单点的三条路（按成本从低到高）**：
   | 方案 | 改什么 | 效果 |
   |---|---|---|
   | PG 流复制到第二台 | 只改部署 | 数据库不再单点（RPO≈0） |
   | 存储换 S3/MinIO（或多副本本地盘） | 只改配置 | 内容不再单点 |
   | 应用起 2+ 副本 | 只改部署 | 进程不再单点（会话已无状态） |
   
   三条都不需要改业务代码——这正是 §8.7 那张"架构约束"表要守住的东西。
3. **运维要求（与规模无关）**：
   - **磁盘是消耗品**：大容量建议 RAID6/RAIDZ2 或至少 RAID10，且**预留 20% 空间**（快照/版本/临时文件）；
   - 监控必须有：磁盘使用率、PG 复制延迟、备份最后成功时间、blob 校验任务。

**建议的部署清单（`docker-compose.yml` 心智模型）**

| 服务 | 说明 |
|---|---|
| `web` | 我们的 Go 单二进制（API + WebDAV + 前端静态资源内嵌） |
| `postgres` | 单实例 + 每日备份 + WAL 归档 |
| `blobstore`（可选） | MinIO，若选 S3 后端；否则省略，用本地卷 |
| `clamav`（v1.1） | 病毒扫描 |
| `caddy`/`nginx` | TLS 终止 + 反代（Caddy 自动证书更省事） |
| `backup`（sidecar 或宿主 cron） | PG 备份 + restic 增量 |

### 8.8 在线 Office：只留接口（WOPI seam）

同意 v1 不做。为了不让未来的接入变成重构，v1 需要埋一个很薄的缝：

- **文件"锁 + 版本 + 短时下载 URL"** 这三样是 WOPI 的前置条件，v1 已经有了（LOCK、版本、签名 URL）→ **不需要额外设计**。
- 唯一要提前定的是 `blob` 到"外部文档服务"的**签权方式**：用一个独立的 short-lived signed token（`?t=<jwt>&exp=`）指向我们自己的下载端点，OnlyOffice/Collabora 用这个 URL 取文件。**现在就把这个 token 类型定义好**，将来接 WOPI 只是多一个调用方。
- 结论：v1 不需要写任何 Office 相关代码；只需保证"下载端点支持一次性签名 token"。

### 8.9 合规基线（由本文确定）

你没有明确要求，所以我按"**务实、不阻塞交付、但能通过审计**"的原则定基线。原则：**只做低成本高收益的项，不做需要认证机构的项**。

**基线（v1 必须满足）**

| 项 | 要求 | 成本 |
|---|---|---|
| 审计 | 全部 30+ 事件、含匿名分享访问；保留 **12 个月**；链式哈希 | v1 已含 |
| 审计完整性 | 应用 DB 用户仅有 `INSERT`，无 `UPDATE`/`DELETE`；每日链尾 hash 写入独立位置（对象存储/邮件） | 低 |
| 访问控制 | 最小权限、单一授权入口、管理员操作留痕 | v1 已含 |
| 身份 | 无本地密码作为主路径（Pocket ID）；break-glass 例外需审计告警 | v1 已含 |
| 传输 | 全站 TLS 1.2+，HSTS | 低 |
| 数据 | 备份可恢复（季度演练记录）、版本与回收站 | v1 已含 |
| 日志 | 不落密码/token/文件内容；IP 保留 12 个月（与审计一致） | 低 |
| 删除 | 用户可删除自己数据；管理员可强制清除；**删除后 blob 在 N 天内真正回收** | 低 |

**明确不做（避免过度设计浪费工期）**

- ❌ 等保测评、ISO 27001 认证（需要第三方机构，不是代码问题）
- ❌ 国密算法（SM2/SM3/SM4）—— 除非有明确监管要求，否则标准算法更安全
- ❌ 数据脱敏/分级分类引擎
- ❌ 双人复核（four-eyes）审批流 —— 除非你的组织确实要求
- ⚠️ **法务保留（legal hold）与"外链审批"**：这两项企业常要，但你没有提，**降级到 P2**。如果未来需要，审计表已按月分区 + 可查询，实现成本不高。

**"保留接口"的合规项**：审计导出（CSV/JSON + syslog/OTel 推送）做成 `AuditSink` 接口，未来对接 SIEM 只需加实现。

### 8.10 规模参数与产品能力的区别（重要澄清）

**产品能力**（写进代码，所有安装共享）：

- 支持任意用户数（受限于部署资源，不受限于代码里的常量）
- 支持任意容量（存储后端可换，元数据不因容量而重构）
- 支持单机、多副本、多可用区部署（架构无状态 + 协调走数据库）
- WebDAV / 分享 / 审计 / 六种凭证类型

**规模参数**（写进配置和部署文档，只影响这一次安装）：

| 参数 | 当前值 | 影响什么 | 改起来影响代码吗 |
|---|---|---|---|
| 用户数 | ~2000 | 是否需要多副本 | ❌ 不需要 |
| 总容量 | ~40 TiB | 盘怎么买、备份策略、单卷大小 | ❌ 不需要 |
| 部署形态 | 单机 Docker | 可用性上限、备份要求 | ❌ 不需要 |
| 单文件上限 | 10 GB 软 / 1 TB 硬 | 分片大小、超时设置 | ❌ 只是配置 |

**为什么这个区分重要**：如果按"<2000 人 / 单机"来设计，会写出"会话存内存"、"锁存内存"、"任务队列用 channel"这类代码——它们在单机下完美工作，在加第二台机器的那一天全部失效，而且失效方式是**静默的数据损坏**（比如两进程各自持有同一文件的"独占锁"）。

所以本项目的规则是：

> **代码里不出现"我是唯一进程"的假设。部署里可以只有一台机器。**

这条规则的成本几乎为零（用数据库做协调而已），收益是把"扩容"从一次重构降级为一次部署变更。

### 8.11 本次安装的规模与容量规划

以下数字**只对当前这次部署有效**，换一个安装可以完全不同。

**容量规划**（40 TiB 用户数据实际要买多少盘）：

| 项 | 估算 | 说明 |
|---|---|---|
| 原始用户数据 | 40 TiB | |
| 版本保留 | ×1.5 → 60 TiB | 假设 10 版 / 30 天，实际取决于改写频率 |
| 回收站 | ×1.1 → 66 TiB | 30 天保留 |
| 去重收益 | −20%~40% | 仅 `blob` 布局；高度依赖是否有大量重复内容 |
| **规划容量** | **约 50–60 TiB 可用** | 别按 40 TiB 买盘 |
| 单卷/单桶 | ≤ 20 TiB | 超过则故障恢复时间不可接受 |

**tree 布局下没有去重收益**，规划容量按 66 TiB 算——这是选择 tree 的主要成本。

**规模参考线**（不是硬限制，是"该考虑下一步"的信号）：

| 指标 | 单副本仍舒适 | 该考虑多副本 |
|---|---|---|
| 并发活跃用户 | < 500 | > 1000 |
| 每日请求量 | < 500 万 | > 2000 万 |
| 元数据行数 | < 5000 万 | > 2 亿 |
| 内容容量（本地盘） | < 20 TiB | 换 S3 或加盘 |

---

## 参考

- RFC 4918 (WebDAV)、RFC 6578 (sync-collection)、RFC 7233 (Range)、[tus 可续传上传协议](https://tus.io/)
- Nextcloud 兼容实现所需的探测面：[karadav NEXTCLOUD.md](https://f-hub.org/Outmode/karadav/raw/commit/442d22eca5eb4de63e05db3709c62604e092b77c/NEXTCLOUD.md)
- Go WebDAV 参考实现：[golang.org/x/net/webdav](https://pkg.go.dev/golang.org/x/net/webdav)
- Rust WebDAV：[dav-server crate](https://packages.ecosyste.ms/registries/crates.io/keywords/webdav-server)（[Debian 打包评审](https://dfsg-new-queue.debian.org/reviews/rust-dav-server/0.11.0-1)）
- 可评估的现成方案：[SFTPGo](https://github.com/drakkan/sftpgo)
- WOPI 协议（在线 Office 集成）
- **Pocket ID**：[Scopes and claims](https://pocket-id.org/docs/guides/scopes-and-claims)、[SCIM 配置](https://pocket-id.org/docs/configuration/scim)、[账户恢复（一次性登录码）](https://pocket-id.org/docs/troubleshooting/account-recovery)、[groups claim 的 refresh 坑 issue #987](https://github.com/pocket-id/pocket-id/issues/987)
- WebDAV 符合性测试：[litmus](http://www.webdav.org/neon/litmus/)
