# newCloud

自托管的团队文件服务：**OIDC 单点登录 + WebDAV + 分享 + 审计**，单个 Go 二进制、单个数据库、单个进程。

用 Go 重写 Nextcloud 的核心能力，去掉 PHP 技术栈。定位是"自己的文件服务器"，不是 Nextcloud 的替代品：不追求应用生态，只把文件存取、分享和外发做到可靠、可审计、可运维。

## 文档

- [docs/0001-scope-and-language.md](docs/0001-scope-and-language.md) — 功能范围、语言选型与产品定位
- [docs/0002-backend-skeleton-adr.md](docs/0002-backend-skeleton-adr.md) — 后端骨架：认证、授权、审计、WebDAV
- [docs/0003-storage-layout.md](docs/0003-storage-layout.md) — 存储布局：blob / tree 两种摆放方式
- [docs/0004-metadata-store.md](docs/0004-metadata-store.md) — 元数据层契约与实现约定
- [docs/0010-functional-requirements-v0.2.md](docs/0010-functional-requirements-v0.2.md) — 功能需求与架构缺口分析（输入）
- [docs/0011-contract-implementation-record.md](docs/0011-contract-implementation-record.md) — **跨模块契约的实现与验收记录（以及仍然缺什么）**

## 产品能力

| 能力 | 实现情况 |
|---|---|
| 单点登录 | Pocket ID / 任意 OIDC（PKCE + state + **nonce**）；App Password 供 WebDAV 与脚本使用（作用域在 WebDAV 上生效）；**break-glass 本地管理员**：网段限制、审计、无需重启即可轮换 |
| 多用户 | 每个账号独立命名空间、独立配额；按 IdP 组映射角色；**资源 ACL**（用户/群组授权、继承、`inherit=false` 中断、`manage_permissions` 可委托）；`POST /api/acl/` 管理授权 |
| WebDAV | RFC 4918：PROPFIND/PROPPATCH/PUT/DELETE/MKCOL/COPY/MOVE/LOCK/UNLOCK、Range、`If-Match`/`If-None-Match` 与 412；**锁在所有写方法上生效且区分 `Depth: 0`（成员关系）与 `Depth: infinity`（子树内容）**，网页/API 写入同样受锁约束（423，需提交 token）；目录 COPY 递归、PUT 覆盖返回 204 |
| 并发 | 网页上传、分片上传收尾、WebDAV PUT 共用同一套 `If-Match`/`If-None-Match` 仲裁，冲突返回 412 而非静默覆盖 |
| 网页端 | 内置前端：列视图 + 网格、跨树搜索、拖拽上传、**大文件分片续传**、重命名、批量删除、回收站、应用密码管理；目录列表用**游标分页** |
| 预览 | 图片、视频、音频、PDF、文本与源码在浏览器内直接查看；内联渲染走类型白名单 + 内容嗅探 + CSP sandbox，SVG/HTML 永不内联 |
| 缩略图 | 服务端生成并缓存（仅标准库解码 PNG/JPEG/GIF），有像素上限、并发上限与解码截止时间 |
| 分享 | 链接分享、密码、有效期、下载次数上限；`email` 分享限定收件人（一次性验证码，**单次使用 + 尝试限速**）；"投递箱"链接让别人向指定目录临时上传 |
| 下载计数 | 按 `authorized_download_session` 计数：一次播放的多个 Range 请求只算一次下载，换访客/换文件/换版本重新计费 |
| 回收站 | 恢复、按 `trash_retention_days` **真正永久删除**并释放配额；`version_retention_days` 清理历史版本 |
| 审计 | 哈希链事件日志；`newcloud audit verify` 可独立校验并检测改写与**截断**；CSV/JSON 导出；INSERT-only 数据库角色（见限制） |
| 存储 | 本地磁盘或 S3；`blob`（内容寻址去重）或 `tree`（镜像目录结构）两种布局 |
| 检索 | 按名称/路径浏览；版本历史与恢复 |

## 设计决定

以下几条不是实现细节，是产品行为，改动前请先读对应的 ADR。

**拒绝一律返回 404，不返回 403。** 区分"不存在"和"无权访问"等于免费提供目录枚举接口。

**两个独立的凭据体系。** 浏览器会话与 App Password 走不同路径，WebDAV 不接受会话 Cookie（桌面客户端无法完成 OIDC 跳转）。所有 256 位随机令牌用 HMAC 派生索引；只有人选的密码（分享密码、break-glass）才用 Argon2id——给随机令牌上 Argon2 只是给每个 WebDAV 请求增加 60ms，不带来安全性。

**每一次写操作与它的审计记录在同一个事务里。** 审计写入拒绝在未启用链式写入时静默通过，宁可报错也不留下无法追溯的变更。

**条件写入在改行的同一条语句里判断。** 先读后写在并发下必然丢更新；`WriteFile` 把前置条件、行锁和写入放在一起，网页与 WebDAV 因此不可能各用一套规则。

**锁在写路径上强制，而不只在 LOCK 里记录。** 一个只存在于创建接口的锁，会让客户端误以为受到保护——这比没有锁更糟。

**存储布局的选择影响运维，不只是性能。** `blob` 去重、省空间、版本几乎免费，但 `ls` 看不到原文件名；`tree` 可以直接用 `rsync`/`ls` 操作，但每个版本是一份完整副本，且继承 Windows 保留字限制。两种都由同一套契约测试验证。

**代码不假设"我是唯一进程"。** 会话、锁、任务队列都走数据库（`FOR UPDATE SKIP LOCKED` + lease）。单机部署是可以的，但这是部署选择，不是架构前提。

## 快速开始（本地开发）

需要 Go 1.26+ 与 Docker。

```sh
# 数据库
docker run -d --name wc-pg \
  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=newcloud \
  -p 15432:5432 postgres:17

export DATABASE_URL="postgres://postgres:test@127.0.0.1:15432/newcloud?sslmode=disable"

# 应用 schema
go run ./cmd/newcloud migrate

# 跑测试（PG 部分无 TEST_DATABASE_URL 时自动跳过）
export TEST_DATABASE_URL="$DATABASE_URL"
go test ./...

# 启动
NEWCLOUD_CONFIG=./config.yml go run ./cmd/newcloud serve
```

没有 OIDC 提供方时，用内置的假 IdP 打通整条登录链路：

```sh
go run ./cmd/devidp -addr 127.0.0.1:19000 -client-id newcloud
```

**`cmd/devidp` 只能用于开发。** 它对任何请求都放行，绝不可部署。

## 部署

生产部署见 [deploy/](deploy/)：

```sh
cd deploy
PG_PASSWORD='...' docker compose up -d
```

组成：`web`（应用，distroless 非 root）、`postgres`（17，开启 WAL 归档）、`caddy`（自动 TLS）。配置在 `/etc/newcloud/config.yml`，参考 [deploy/config.example.yml](deploy/config.example.yml)。

两个必须注意的点：

1. **`/var/lib/newcloud` 必须是单个挂载点。** `blobs/` 与 `blobs/tmp/` 跨文件系统会让发布 blob 从 `rename(2)` 退化成整文件复制。启动时会做真实的 rename 探测，不满足就拒绝启动。
2. **`storage.layout` 必须显式指定**（`blob` 或 `tree`），没有默认值。这个选择改变磁盘上的数据形态，不适合由我替你做。

备份与恢复演练见 [deploy/backup/restore-drill.md](deploy/backup/restore-drill.md)。备份脚本存在且可用，**但恢复演练尚未执行**：记录表仍是"待首次演练"，因此 INV-08 要求的"经真实恢复验证"目前没有证据，不应对外声称具备。

## 目录结构

```
cmd/newcloud      主程序：serve / migrate / healthcheck
cmd/devidp        开发用假 OIDC 提供方（不可部署）
internal/
  auth            OIDC 握手、会话、App Password
  authz           Actor + Cap；授权结论不可伪造
  audit           哈希链事件日志
  blob, storage   内容存储：blob（去重）/ tree（镜像）
  store           元数据层：节点树、配额、版本、审计、分享、锁
  dav             WebDAV 处理器
  server          HTTP 层：JSON API + 分享页 + 静态前端
  share           分享业务逻辑
  ticket          HMAC 令牌
  worker          过期锁清理、孤儿 blob 回收
  webassets       内嵌前端（无构建步骤、无 npm 依赖）
tools/            开发期验证脚本
migrations/       SQL schema
docs/             决策记录
```

前端刻意不引入框架与构建步骤：三个静态文件内嵌进二进制。这样交付物就是被审查过的代码本身，没有传递依赖需要审计。

## 测试

```sh
go test ./...                                  # 单元 + 集成
go test ./... -race                            # 需要 CGO 与 C 编译器
TEST_DATABASE_URL=... go test ./internal/store # 追加 PostgreSQL 契约测试
```

同一套 `internal/storetest` 契约测试同时跑在内存实现和 PostgreSQL 实现上。这不是形式主义：两者曾经在"预建账号首次登录"上行为不一致，内存实现通过、PostgreSQL 返回 500，只有让两套实现过同一个测试才能发现。

开发期还有两个端到端脚本，需要真实运行的服务：

```sh
go run ./tools/e2e     -app http://127.0.0.1:18500 -idp http://127.0.0.1:19000
go run ./tools/uicheck -app http://127.0.0.1:18500 -public-host 127.0.0.1:18600
```

`uicheck` 用 headless Chrome 加载真实页面，验证渲染、tab 切换、以及文件名等用户数据是以文本而非 HTML 进入 DOM 的。

## 已知限制

**未实现的能力**（详见 [docs/0011-contract-implementation-record.md](docs/0011-contract-implementation-record.md) §2）：

- 资源 ACL 的 API 完整（`POST /api/acl/` 授予、`DELETE /api/acl/{id}` 撤销、`GET /api/shared` 发现、`GET /api/shared/{id}/content` 取内容），但**没有界面**。`read`/`download`、`create`/`overwrite`、`rename` 都是真正独立的权限：只授 `read` 可以看元数据但取不到字节；只授 `create` 可以新建但不能替换已有文件。群组共享**空间**（团队所有权）仍未实现。
- break-glass 已有登录入口（`POST /api/auth/breakglass`）与 `newcloud breakglass hash` 生成器，但没有界面；口令从管道读取，不做终端免回显。
- 账号删除已实现（`DELETE /api/admin/users/{id}`，需 `confirm_username` 确认，且必须先禁用）：文件进回收站、分享与凭据撤销、WebDAV 锁释放、**审计记录保留**、账号墓碑并释放用户名与邮箱。墓碑不可撤销。
- JIT 开户支持 `open` / `pending` / `deny` 三种模式与域名白名单，**批准 API 已就绪**（`GET /api/admin/users?status=pending`、`POST /api/admin/users/{id}/status`），但没有界面。IdP 离线时服务照常启动：已有会话、应用密码与 WebDAV 不受影响，只有新登录返回 503，IdP 恢复后无需重启即可登录。角色降权会在**下次登录**时生效并撤销该账号的全部会话与 App Password（T-02 已验证）；没有 IdP 事件源，所以"移出组"到"权限消失"之间隔着一次登录。
- 后台任务队列已有消费者：`jobs` 表带租约、重试退避与永久失败，`FOR UPDATE SKIP LOCKED` 领取，租约到期可被重新领取。目前注册了 3 种 job（blob 回收、缩略图预热、凭据清理），`jobs` 不再是一张只写不读的表。
- 兄弟名唯一性按 **Unicode NFC + 完整大小写折叠 + 剥离格式字符**判定（`store.nameKey`），内存与 PostgreSQL 两个后端一致，所以 `Readme.md` 与 `README.md` 不可能同时存在。存量冲突由迁移 `0007` 重命名（保留最早的那个，新的改为 `<名字> (conflict N)<扩展名>`）并记入 `name_conflict_renames`，不删除任何文件。存量行的键由 `migrate` 里的 `RepairNameKeys` 收敛：keyset 分页每批 1000 行、每批一个事务，重算并只更新不一致的；会与兄弟冲突的行跳过并逐行报告，不替用户选赢家。（1200 条过期行实测 3.4 秒。）未在 Windows/macOS 实机客户端上验证（T-18）。- COPY/MOVE 失败不破坏源文件：配额不足、目标父目录缺失、目录复制中途失败都保留原件（配额不足时 MOVE 不会先删源）。目录复制逐节点提交，中途失败会留下部分目标——这一点写在限制里，而不是假装是原子的。
- 目录分页用**游标**（`name > cursor`）而非 offset：offset 在名字排序上遇到并发增删会静默重复和漏行。契约写在 [docs/0012-cursor-contract.md](docs/0012-cursor-contract.md)：**任何行绝不返回两次**；代价是把未见的行重命名到游标之前会漏到下次列表。游标不携带权限，每页重新鉴权。
- **审计故障时 fail-closed**：审计写不进的操作**不会发生**（`503 audit_unavailable`），读操作继续可用。策略、客户端契约与运维 runbook 见 [deploy/audit-failure-policy.md](deploy/audit-failure-policy.md)。选 fail-closed 的理由是：fail-open 会让"写满磁盘"成为最便宜的攻击，且缺事件不留痕、哈希链依然校验通过。- `/metrics` 以 Prometheus 文本格式暴露请求错误率、DAV 冲突率、上传结果、队列积压、审计链与锚点校验；只有端点，没有随附面板或告警规则。`/readyz` 会探测元数据与内容存储。
- 审计链有**外部锚点**（每个完成日一个 checkpoint，含链头与事件数），可检测截断与中段重写；但生产环境必须把锚点目录指向与数据库**不同凭据**的位置——代码无法强制这一点。应用角色 `newcloud_app` 现已可登录并被 `docker-compose.yml` 采用，审计表对它只允许 SELECT/INSERT（已验证 UPDATE/DELETE/TRUNCATE 被拒）；口令需运维手动 bootstrap，迁移里故意不写。
- 预览/缩略图有像素上限 + 并发上限 + 解码截止时间；仍在进程内解码，真正的隔离需要子进程。
- 共享锁（shared lock）与 `REPORT`/`sync-collection` 未实现，客户端会收到明确的 501 而不是空结果。
- 管理员看不到用户文件内容，只能看元数据与强制删除。这是刻意的。
- `email` 分享的验证码需要配置 `mail.*`。未配置时服务照常启动，并在每次生成验证码时告警，而不是假装发送成功。
- 审计导出对普通用户的范围是"我做过的事"加"关于我数据的匿名事件"（匿名下载、被拒的收件人尝试）；只有管理员能导出全部。
- `s3` 后端（bucket 必须事先存在，启动时校验连通性）现在跑满**全部** layout 合规用例与 blob 契约用例（共 24 项：覆盖写、Range/Seek、multipart 17 MiB 上传、删除、去重、owner 隔离），默认使用进程内 S3 假实现，因此**不再跳过**。仍**没有对真实 AWS S3 / MinIO / R2 做过互操作测试**——假实现验证的是后端自身逻辑，不是协议兼容性；设置 `TEST_S3_ENDPOINT` 可对真实端点跑同一套用例。
- `tree` 布局只能在 `local` 后端上使用：它按定义是文件系统镜像。
- 分享链接在文件被删除后停止服务（404，而非 200 空列表）；"文件已删/链接已吊销/从未存在"三者响应完全相同——区分它们会让端点变成令牌预言机。
- 越权（T-01）按**路由表**逐条测试而非抽样：12 条带 id 的路由中 10 条有独立用例，其余 2 条各由既有测试覆盖并写明理由。每条断言返回 404（不是 403，403 会确认 id 存在）、拒绝的响应体不泄漏文件名等元数据、变更型操作后对方资源未被改动。
- 文档引用的每个测试名都必须是真实存在的函数：`TestDocumentsCiteOnlyRealTests` 扫描 `docs/`、`deploy/` 与 README，缺一个就失败。（讨论已废弃名字的句子须显式标记 `docIgnore:`。）
- OIDC 密钥轮换无需重启：客户端遇到未知 kid 会重取 JWKS；轮换步骤与九种验证失败的策略见 [deploy/oidc-key-rotation.md](deploy/oidc-key-rotation.md)。已有会话不受轮换影响（会话是自己的凭据，从不问 IdP）。
- `blobs.size` 曾长期被写成 0（`BlobIDFor` 硬编码），GC 的回收字节统计因此失真；`migrate` 里的 `RepairBlobSizes` 从内容寻址对象回填真实长度（单行缺失只报告、不臆造）。
- 恢复演练的表清单由 `TestDrillCoversEveryTable` 对着实际 schema 自检——手写清单会腐烂，注释里承诺的检查若不写成代码就等于没有。
- 恢复演练已自动化（`newcloud drill`）：20 张表、元数据行、审计哈希链、以及**每个 blob 的字节**在 dump→restore 后都完整。已反向验证演练**会失败**——篡改一条审计记录、或翻转一个 blob 字节，都会 FAIL。未覆盖：restic 快照恢复本身、大库耗时、PITR。
