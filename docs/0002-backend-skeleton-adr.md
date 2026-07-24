# ADR-0002：后端骨架、凭证与 WebDAV 实现方案

状态：已接受
日期：2026-10-21
上游决策：见 [0001-scope-and-language.md](0001-scope-and-language.md)
适用范围：M0 / M1 的实现基线。本 ADR 定的是"怎么搭"，不再讨论"要不要做"。

---

## 0. 一句话结论

**Go 1.24+ / `chi` 路由 / `pgx`+`sqlc` / `x/net/webdav` 作 XML 骨架但存储与锁全部自己接 PG / 凭证一律用"会话票据 + HMAC 域分离"而不是多套随机密钥 / 无状态应用（单机与多副本跑同一份代码）/ 前端 SvelteKit 静态内嵌。**

本 ADR 有三个决定值得单独说明，因为它们纠正了一个很容易走偏的设计：

1. **App Password 不再"只存 Argon2 哈希"** —— 那会让 WebDAV 每个请求都跑一次 Argon2。改为 **long-lived session JWT（HMAC-SHA256）+ 域分离**，见 §4。
2. **`Authorize()` 是唯一入口，且它返回"能力位图"，调用方拿不到节点的 owner 字段** —— 见 §5。
3. **WebDAV 不用 `x/net/webdav.FileSystem` 抽象**（它的 API 无法表达配额、版本、锁持久化），只复用它的 XML 类型与部分 handler 逻辑；PROPFIND 自己写，因为大目录必须流式。见 §6。

---

## 1. 版本与依赖基线

| 项 | 选择 | 理由 |
|---|---|---|
| Go | **1.23+**（`go.mod` 写 `go 1.23`） | 需要 `os.Root`（Go 1.24 才有）/ 实际用 1.24+ 更稳妥；至少用到 1.22 的 `net/http` 路由增强 |
| 路由 | `chi/v5` | 中间件模型简单，无反射；也可纯 `net/http`。**不用 gin/echo**（依赖重、中间件顺序易错） |
| DB 驱动 | `jackc/pgx/v5` + `pgxpool` | 唯一活跃的 PG 驱动 |
| 查询 | `sqlc` 生成 | 编译期类型安全，SQL 可读，避免 ORM 的隐式行为 |
| 迁移 | `golang-migrate` 或 `goose` | 选 `goose`（单二进制内嵌 `embed.FS` 即可跑） |
| OIDC | `coreos/go-oidc/v3` + `golang.org/x/oauth2` | 标准库事实标准；Pocket ID 是标准 OIDC，无需定制 |
| 密码/哈希 | `argon2id`（`golang.org/x/crypto/argon2`） | 仅用于 break-glass 口令与分享密码 |
| 随机 | `crypto/rand` | 所有 token |
| 日志 | `log/slog`（结构化 JSON） | 标准库，无依赖 |
| 配置 | `env` + 少量 `yaml`（`gopkg.in/yaml.v3`） | 配置项少，不上 viper |
| 邮件 | `wneessen/go-mail` | 发送分享通知/验证码 |
| 测试 | 标准 `testing` + `testcontainers-go`（PG） | 集成测试用真 PG，不用 sqlite 模拟 |

**最小依赖原则**：以上全部锁版本，`go mod tidy` 后提交 `go.sum`；CI 跑 `govulncheck`。

---

## 2. 仓库结构

```
newcloud/
├── cmd/
│   ├── newcloud/main.go          # 唯一二进制：API + WebDAV + 静态资源内嵌 + worker
│   └── migrate/main.go           # 迁移（也可由 newcloud 子命令承担）
├── internal/
│   ├── config/                   # 配置加载、校验、默认值
│   ├── auth/                     # Pocket ID OIDC、会话、App Password、break-glass
│   │   ├── oidc.go
│   │   ├── session.go
│   │   ├── apppassword.go
│   │   └── scim.go
│   ├── authz/                    # ★ 授权唯一入口
│   │   ├── authz.go
│   │   └── authz_test.go         # 授权矩阵表驱动测试
│   ├── store/                    # 元数据（PG）
│   │   ├── node.go
│   │   ├── share.go
│   │   ├── audit.go
│   │   └── queries/              # sqlc 的 .sql
│   ├── blob/                     # ★ 内容寻址存储
│   │   ├── blob.go               # BlobStore 接口
│   │   ├── local/                 # 本地盘实现
│   │   └── s3/                    # S3/MinIO 实现
│   ├── dav/                      # WebDAV 层
│   │   ├── handler.go            # 方法分发
│   │   ├── propfind.go           # 自己写的流式 PROPFIND
│   │   ├── props.go              # 死属性、属性别名
│   │   ├── lock.go               # 锁（PG 持久化）
│   │   └── xml.go                # 复用的 XML 类型
│   ├── api/                      # 网页端 JSON API (BFF)
│   ├── share/                    # 分享、file request、邮箱验证
│   ├── audit/                    # 事件定义、outbox、链式哈希、sink
│   ├── worker/                   # 异步任务（缩略图、邮件、清理）
│   └── web/                      # 静态资源内嵌 + SPA 回退
├── web/                          # SvelteKit 源码（构建产物拷到 internal/web/dist）
├── migrations/                   # *.sql + embed.FS
├── deploy/
│   ├── docker-compose.yml
│   ├── Caddyfile
│   └── backup/                   # 备份脚本（pg_dump + restic）
└── docs/
```

**分层规则（用 `go vet`/自定义 lint 守护）**：
`api`/`dav` → `authz` → `store`/`blob`。**禁止 `dav` 直接调 `store`**，必须经 `authz` 校验后由 `store` 返回已授权的数据；`store` 不得 import `authz`（避免循环）。

---

## 3. 配置与启动自检

配置来源优先级：**环境变量 > 配置文件 > 默认值**。所有配置在启动时校验，失败即退出（不要"能起来但行为不对"）。

```yaml
server:
  addr: ":8080"
  public_url: "https://cloud.example.com"        # 用于生成分享链接
  usercontent_url: "https://usercontent.example.com"  # ★ 必须与 public_url 不同源
  tls: { mode: "proxy" }                          # proxy = 由 Caddy 终止

auth:
  issuer: "https://id.example.com"
  client_id: "..."
  client_secret_file: "/run/secrets/oidc_client_secret"
  scopes: [openid, profile, email, groups]        # ★ groups 必须显式申请
  group_role_map:
    "cloud-admins": admin
    "cloud-users": user
  session_ttl: "12h"
  allow_local_admin: false                        # ★ break-glass 紧急开关（纯配置生效）
  local_admin:
    username: "breakglass"
    password_argon2_file: "/etc/newcloud/bg.pass"
    allowed_cidrs: ["10.0.0.0/8"]

storage:
  backend: "local"                                # local | s3
  local: { root: "/var/lib/newcloud/blobs" }
  s3: { endpoint: "...", bucket: "...", region: "..." }

limits:
  max_file_bytes: 1073741824 * 1024               # 1 TiB 硬上限
  default_file_quota_bytes: 10737418240           # 10 GiB 软上限
  max_propfind_children: 100000                   # ★ 大目录保护
  upload_chunk_bytes: 268435456                   # 256 MiB
```

**启动自检（失败即拒绝启动，这是防止"能起来但行为不对"的第一道闸）**：

1. `blobs/tmp` 与 `blobs/` 的 `st_dev` 必须相同（否则 rename 退化 copy）—— **§8.7 风险项的实现**。
2. `public_url` 与 `usercontent_url` 的 scheme+host+port 必须不同（否则内容域隔离失效）。
3. DB 用 `INSERT`-only 角色连接审计 schema；`SELECT`/`UPDATE` 用另一个角色（启动时各测一次，权限不对就退出）。
4. 存储后端可达性 + 写一个探针 blob 再删。
5. `allow_local_admin=true` 时 `allowed_cidrs` 不能为空（否则等于全开放），直接拒绝启动。

---

## 4. 凭证体系（★ 本 ADR 最重要的修正）

### 4.1 问题

早期方案是"App Password 只存 Argon2id 哈希"。**这在 WebDAV 下不可行**：

- Windows 资源管理器 / rclone 对**每个请求**都发 Basic 认证；
- Argon2id 单次 ~50–100 ms（安全参数下），一个目录展开 200 个 PROPFIND 子请求 = 20 秒；
- 降低参数（如 10ms）又会让哈希本身可被离线爆破。

**结论：长期凭证不能用"每次校验都要慢哈希"的形态。**

### 4.2 方案：会话票据（HMAC 票据），不是"多套密钥"

只保留**一个**主密钥 `session_secret`（32 字节），用 **domain separation** 派生所有票据用途：

```
token = base64url( payload || HMAC-SHA256(derive(key, purpose), payload) )
derive(key, purpose) = HMAC-SHA256(key, "newcloud/v1/" + purpose)
```

| purpose | 载体 | TTL | 存储 | 校验成本 |
|---|---|---|---|---|
| `session` | `__Host-wc_session` cookie | 12h（滑动） | 无状态 + DB 记录 `jti`（用于踢下线） | 1 次 HMAC |
| `apppassword` | Basic 用户名/密码 | 90d（可配置） | DB 只存 **HMAC 票据本身**+ 前缀明文（`wc_ab12...` 前 8 位给用户识别） | 1 次 HMAC + 1 次索引查询 |
| `share_access` | cookie（分享页验证密码后） | 1h | 无状态 | 1 次 HMAC |
| `share_token` | URL 路径 `/s/<token>` | 由分享设置 | DB 只存 **SHA-256(token)** | 1 次哈希 + 索引 |
| `download_token` | 一次性 URL 参数 | 5min | 无状态 | 1 次 HMAC |
| `email_verify` | 邮件里的链接 | 24h | 无状态 | 1 次 HMAC |

**为什么 App Password 可以用 HMAC 而不是 Argon2**：App Password 是 **128 bit 随机**，不是人选的弱口令。随机 token 不存在"字典爆破"风险，离线爆破 2^128 不可行，所以不需要慢哈希。**只有人选的密码（break-glass 口令、分享密码）才用 Argon2id。**

**这是关键区分**，也是很多自研系统踩的坑：误以为"所有密钥都要 Argon2"，结果性能崩了又偷偷降参数。

### 4.3 App Password 的完整流程

```
1. 用户在网页点"添加应用密码" → 选名称/scope(ro|rw)/过期时间
2. 服务端生成 secret = rand(32 bytes) → 编码为 wc_<base64url>
3. DB: app_passwords(id, user_id, label, prefix='wc_ab12', token_hash=sha256(secret),
                     scopes, expires_at, last_used_at, revoked_at)
4. 明文只返回给用户一次（页面展示 + 复制按钮）
5. WebDAV Basic 认证：取密码 → sha256 → 查 token_hash 索引（唯一索引，O(1)）
   → 检查 expires_at/revoked_at/scope → 构造 actor
6. 异步更新 last_used_at（不阻塞请求，用 outbox 或每 5 分钟批量）
```

**注意**：App Password 是**高价值凭证**，泄露即等于账号。因此：
- 生成时强制要求用户在**最近 5 分钟内**完成过 IdP step-up（重新 passkey）；
- 列表页显示 `prefix` + 最后使用时间 + IP，用户可自行吊销；
- 创建/吊销都进审计。

### 4.4 会话与踢下线

- 会话是无状态 HMAC 票据，但要支持"立即踢下线" → DB 存 `sessions(jti, user_id, expires_at, revoked_at)`。
- 校验顺序：HMAC → `jti` 未撤销。
- **缓存策略决定了这个设计能不能多副本运行**：进程内 LRU 缓存 `jti -> revoked`，TTL 5 秒。这带来一个有意的取舍：**踢下线最多延迟 5 秒生效**。
  - 为什么不追求"立即"：要做到立即，就得在撤销时广播到所有副本（需要 Redis pub/sub 或 DB 轮询），这是运维复杂度换来的 5 秒。
  - 为什么 5 秒可以接受：吊销凭证的威胁模型是"离职员工还在用旧会话"，5 秒与 0 秒在风险上没有区别；而每请求查一次库会让 WebDAV 的 PROPFIND 放大成 N 次查询。
  - **关键点**：缓存的是"是否已撤销"这个**否定信息**，默认值必须是"未撤销"才能命中，命中失败只会多查一次库、不会误放行。多副本下每个副本各自缓存，最坏情况是各延迟 5 秒，**不会出现不一致的授权决定**。
- worker 进程不校验会话（只读 DB），所以会话机制与 worker 数量无关。

### 4.5 break-glass

- 独立路由 `/local-login`（**不是** OIDC 的备用按钮，避免被扫）；
- 仅当 `auth.allow_local_admin=true` **且** 来源 IP 在 `allowed_cidrs` 内才可用；
- Argon2id 校验 + **TOTP 必填**；
- 每次登录/操作产出 `audit.security.breakglass_used` 并**同步发邮件**给所有 admin；
- 登出后不保留长期会话（TTL 缩短到 1h）。

### 4.6 SCIM

- 端点 `/scim/v2/Users`、`/scim/v2/Groups`（GET/POST/PATCH/DELETE + `filter` 最小子集）；
- 认证：`Authorization: Bearer <scim_token>`（独立 token，不是会话票据，单独配置项）；
- 幂等：用 `externalId`（Pocket ID 的 user id）作为 upsert 键；
- **写操作先进 `scim_pending_changes` 表做差异预览**，超过阈值（如单次 >20 个删除）拒绝并告警，避免配错批量停用（风险表项）。

---

## 5. 授权（`authz`）

### 5.1 唯一入口

```go
package authz

// Actor 由认证层构造，外部无法伪造（字段未导出）。
type Actor struct {
    kind   ActorKind // user | anonymous | system | breakglass
    userID uuid.UUID
    email  string
    role   Role      // admin | user | guest
    scopes ScopeSet  // app password 的 ro/rw 限制
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

// Authorize 是唯一的授权决策函数。返回的 Cap 只包含被允许的能力。
// 调用方拿不到 node.OwnerID，因此无法"顺手"绕过。
func (s *Service) Authorize(ctx context.Context, a Actor, nodeID uuid.UUID) (Cap, error)

type Cap struct {
    perm   Perm
    nodeID uuid.UUID
}
func (c Cap) Can(p Perm) bool { return c.perm&p != 0 }
```

**强制规则**（写进 CONTRIBUTING 与 CI lint）：

- `store` 包的所有导出方法签名里**不接受** `Actor`，只接受 `uuid.UUID` / `*Cap`；
- `dav`/`api` 包**禁止**直接 import `store`（用 `internal/` 内部包 + `go-arch-lint` 或自写 import 检查脚本）；
- 任何 `SELECT` 用户可见数据的查询必须带 `owner_id = $1 OR ...` 条件，禁止"先取出再在内存里判断"。

### 5.2 授权规则（v1）

| 场景 | 规则 |
|---|---|
| 自己的文件 | 全权 |
| 组共享目录 | 组角色：viewer(读) / editor(读写) / manager(读写+分享) |
| 他人分享给我 | 按 share 的权限位 |
| admin | **不自动拥有所有文件内容**（只读元数据 + 可强制清除）；如需查看必须走 `break-glass` 记录审计 |
| guest | 只能访问被显式分享的节点，无法列根目录 |
| 匿名 | 只能经 share token，且只能得到 `hide_contents=true` 时的上传能力 |

> **"admin 不能读用户文件内容"是一条产品决策**，我建议保留：它让"管理员滥用"变成可审计事件而非日常能力。若你要求 admin 可读，把规则改成"admin 读 = 生成 `audit.privileged_read` 并通知用户"。

### 5.3 授权矩阵测试

`internal/authz/authz_test.go` 用表驱动，穷举：

```
{角色} × {资源归属: 自己/同组/他人/被分享} × {操作: 11 种} × {share 权限位} 
```

**每个新 API 必须新增一行**，CI 覆盖率门限对该文件设 100%。

---

## 6. WebDAV 实现

### 6.1 为什么不用 `x/net/webdav` 的 `FileSystem`

`webdav.FileSystem` 接口是：

```go
type FileSystem interface {
    Mkdir(ctx, name, perm) error
    OpenFile(ctx, name string, flag int, perm) (File, error)
    RemoveAll(ctx, name string) error
    Rename(ctx, oldName, newName string) error
    Stat(ctx, name string) (os.FileInfo, error)
}
```

它**只有 string 路径、没有用户身份、没有事务**。用它就必须在一个 `name` 里塞 `userID`，再用全局 map 反查 —— 这正是我们想避免的"路径即授权"。而且它无法表达：

- 配额检查（`PUT` 前要知道字节数）
- 版本生成（`PUT` 覆盖时要保存旧版）
- 锁持久化（`webdav` 的锁是内存 `LockSystem`）
- 分享权限（不同 share 对同一 node 的权限不同）

**所以：只复用它的 XML 类型与 `Handler` 的方法分发骨架，存储层全部自己实现。** 代价约 500–800 行，换来完全可控的语义。

### 6.2 方法实现要点

| 方法 | 要点 |
|---|---|
| `PROPFIND` | **自己写，流式输出**：先 `SELECT ... LIMIT max_children+1` 取子节点，边查边写 XML，避免一次加载全部。超过 `max_propfind_children` 返回 `507` + 提示。`Depth: infinity` 默认拒绝（`403`），可配置开启 |
| `PROPPATCH` | 只允许持久化 `dead_props`；拒绝改 `DAV:` 保留属性（返回 `403` + 正确 `<D:error>`） |
| `PUT` | 流式写 `blobs/tmp/<uuid>` → 边写边算 SHA-256 → 校验 `Content-Length` → 入 blob（幂等）→ 事务内更新 node + 版本 + 配额 + 审计 → `rename` 原子可见 |
| `GET/HEAD` | 支持 `Range`（单区间）、`If-Range`、`ETag`（用 blob sha256 前缀）、`If-None-Match` |
| `DELETE` | 递归移到回收站（软删除 `trashed_at`），不删 blob（refcount） |
| `COPY/MOVE` | 只支持同存储内（v1）；`Overwrite` 头语义严格；MOVE 跨父目录要检查目标权限 |
| `LOCK/UNLOCK` | 锁**落 PG**：`lock_tokens(node_id, token, depth, owner_xml, timeout, expires_at)`；`If` 头 lock token 校验；超时清理由 worker 每分钟跑 |
| `OPTIONS` | 返回 `DAV: 1,2`、`MS-Author-Via: DAV`、`Allow` |
| 认证失败 | `401` + `WWW-Authenticate: Basic realm="newcloud"`（Windows 必需） |

### 6.3 路径解析

```go
// 唯一入口：把 WebDAV 的 URL 路径解析成 node，并附带 Cap
func (h *Handler) resolve(ctx context.Context, a authz.Actor, urlPath string) (authz.Cap, *Node, error)
```

- 相对路径基于 `public_url` 的 `/dav/<user>/...` 前缀剥离；
- 每一段做：URL 解码 → **Unicode NFC 归一化** → 拒绝 `.`/`..`/空段/NUL/`\`/控制字符/Windows 保留名（`CON`、`PRN`、`AUX`、`NUL`、`COM1..9`、`LPT1..9`）→ 长度限制（255 字节/段，4096 总长）；
- 解析结果缓存（请求内），不跨请求缓存；
- **任何一步失败统一返回 404**（不区分"不存在"和"无权限"，防探测）。

### 6.4 大文件

- 分片上传端点：`POST /api/uploads` 创建会话 → `PUT /api/uploads/{id}/{seq}` → `POST /api/uploads/{id}/complete`。
- 分片大小默认 256 MiB，服务端校验 `seq` 连续性与每片 hash（可选）。
- 服务端在 `complete` 时按序流式拼接入 `blobs/tmp`，同时计算整体 SHA-256（**不落一份完整副本到内存**）。
- 断点续传：`GET /api/uploads/{id}` 返回已收到的分片位图。
- 若用户选择 **tus 协议**：可直接实现 tus 1.0 的 `HEAD/PATCH/POST/DELETE`，浏览器端用 `tus-js-client`。**建议采用 tus**，省掉自研协议与客户端重试逻辑。

---

## 7. 部署（`deploy/docker-compose.yml`）

```yaml
services:
  web:
    image: newcloud:latest
    restart: unless-stopped
    environment:
      NEWCLOUD_CONFIG: /etc/newcloud/config.yml
      PGHOST: postgres
    volumes:
      - /etc/newcloud:/etc/newcloud:ro
      - /var/lib/newcloud:/var/lib/newcloud   # ★ 单一 bind mount：pg 除外
    ports: ["8080:8080"]
    depends_on: [postgres]

  postgres:
    image: postgres:17
    restart: unless-stopped
    command: >
      postgres -c wal_level=replica -c archive_mode=on
               -c archive_command='test ! -f /archive/%f && cp %p /archive/%f'
    volumes:
      - /var/lib/newcloud/pg:/var/lib/postgresql/data
      - /var/lib/newcloud/wal-archive:/archive
    environment: { POSTGRES_PASSWORD_FILE: /run/secrets/pg_pass }

  caddy:
    image: caddy:2
    restart: unless-stopped
    volumes: ["/etc/caddy/Caddyfile:/etc/caddy/Caddyfile:ro"]
    ports: ["80:80", "443:443"]
    depends_on: [web]
```

**关键点**：

1. `blobs/` 与 `blobs/tmp/` 在**同一个 bind mount** 下（`/var/lib/newcloud`），满足 rename 要求。
2. `pg/` 单独放在同一个挂载里也可以，但**不能与 blobs 竞争 IO** → 生产建议把 PG 放独立盘（compose 里改成单独 volume 路径），文档写明。
3. WAL 归档目录必须是**另一个物理盘或 NFS**，否则同盘故障时 WAL 一起丢。
4. Caddy 终止 TLS，`web` 只监听内网。

**备份（`deploy/backup/`）**：

- `pg-backup.sh`：`pg_dump -Fc` 每日 + 基础备份每周；WAL 持续归档。
- `blob-backup.sh`：`restic backup /var/lib/newcloud/blobs --tag newcloud-blob`，增量（内容寻址天然适合）。
- `restore-drill.md`：M5 验收用的恢复手册，要求记录实际耗时。

---

## 8. 可观测性

| 项 | 实现 |
|---|---|
| 日志 | `slog` JSON，字段固定：`ts, level, msg, trace_id, actor, action, target, result, dur_ms` |
| 指标 | `/metrics`（Prometheus）：请求数/延迟直方图、活跃上传、blob 写入字节、审计队列深度、锁数量、DB 池使用 |
| 追踪 | OpenTelemetry（可选开启），至少打通 HTTP → DB → blob |
| 健康 | `/healthz`（进程）、`/readyz`（DB + blob 探针 + 迁移版本匹配） |

---

## 9. 测试策略

| 层 | 手段 | 门限 |
|---|---|---|
| 单元 | `go test ./...` | 核心包 ≥80% |
| 授权矩阵 | 表驱动穷举 | `internal/authz` **100%** |
| 路径解析 | `go test -fuzz` | 至少 3 个 fuzz target：段解析、NFC、保留名 |
| 存储 | `testcontainers` 起真 PG | 无 mock DB |
| 并发 | `-race` + 分享计数竞态测试 | CI 强制 |
| WebDAV 协议 | `litmus` 容器化跑 | v1 目标：`basic` + `locks` 全过，`copymove` 允许少量已知偏差并文档化 |
| 端到端 | rclone 对容器做上传/下载/列目录 | CI 冒烟 |

---

## 10. 里程碑对应

| 里程碑 | 本 ADR 覆盖的部分 |
|---|---|
| M0 | §1–§5（骨架、配置、自检、认证、SCIM、authz）+ 审计表 |
| M1 | §6（WebDAV）+ blob 双后端 + App Password |
| M2–M5 | 按 0001 的功能清单推进 |

---

## 11. 未决事项

1. **`x/net/webdav` 具体复用范围**：是否只复用 `internal/xml` 类型与 `Handler` 的分发，还是连 `PROPFIND` 也复用后再包一层流式。实现时先做 spike 验证（0.5 天），结论补进本 ADR。
2. **前端框架**：SvelteKit vs React。本 ADR 不锁定，但要求"静态产物 + 同源 cookie 会话"，不用 SPA 持有的长期 token。
3. **`Admin 能否读用户文件内容`**：本文按"不能"设计，需要你确认（见 §5.2）。

---

## 参考

- [0001-scope-and-language.md](0001-scope-and-language.md)
- RFC 4918 / 4918bis、RFC 6578、RFC 7233
- [tus 可续传上传协议](https://tus.io/)
- [Go OIDC (`coreos/go-oidc`)](https://github.com/coreos/go-oidc)
- [litmus WebDAV 测试套件](http://www.webdav.org/neon/litmus/)
