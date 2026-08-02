# newCloud

自托管的团队文件服务：**OIDC 单点登录 + WebDAV + 分享 + 审计**，单个 Go 二进制、单个数据库、单个进程。

用 Go 重写 Nextcloud 的核心能力，去掉 PHP 技术栈。定位是"自己的文件服务器"，不是 Nextcloud 的替代品：不追求应用生态，只把文件存取、分享和外发做到可靠、可审计、可运维。

## 文档

- [docs/0001-scope-and-language.md](docs/0001-scope-and-language.md) — 功能范围、语言选型与产品定位
- [docs/0002-backend-skeleton-adr.md](docs/0002-backend-skeleton-adr.md) — 后端骨架：认证、授权、审计、WebDAV
- [docs/0003-storage-layout.md](docs/0003-storage-layout.md) — 存储布局：blob / tree 两种摆放方式
- [docs/0004-metadata-store.md](docs/0004-metadata-store.md) — 元数据层契约与实现约定

## 产品能力

| 能力 | 实现情况 |
|---|---|
| 单点登录 | Pocket ID / 任意 OIDC；App Password 供 WebDAV 与脚本使用；break-glass 本地管理员 |
| 多用户 | 每个账号独立命名空间、独立配额；按 IdP 组映射角色 |
| WebDAV | RFC 4918：PROPFIND/PROPPATCH/PUT/DELETE/MKCOL/COPY/MOVE/LOCK/UNLOCK、Range、条件请求 |
| 网页端 | 内置前端：列视图 + 网格、跨树搜索、拖拽上传、**大文件分片续传**、重命名、批量删除、回收站、应用密码管理 |
| 预览 | 图片、视频、音频、PDF、文本与源码在浏览器内直接查看；内联渲染走类型白名单 + 内容嗅探 + CSP sandbox，SVG/HTML 永不内联 |
| 缩略图 | 服务端生成并缓存（仅标准库解码 PNG/JPEG/GIF），列表与网格显示真实缩略图而非图标 |
| 分享 | 链接分享、密码、有效期、下载次数上限；`email` 分享限定收件人（一次性验证码）；"投递箱"链接让别人向指定目录临时上传 |
| 审计 | 哈希链事件日志，记录谁在何时下载/上传/修改了什么；网页端时间线可按动作/结果/时间筛选，CSV/JSON 导出；INSERT-only 数据库角色 |
| 存储 | 本地磁盘或 S3；`blob`（内容寻址去重）或 `tree`（镜像目录结构）两种布局 |
| 检索 | 按名称/路径浏览；版本历史与恢复 |

## 设计决定

以下几条不是实现细节，是产品行为，改动前请先读对应的 ADR。

**拒绝一律返回 404，不返回 403。** 区分"不存在"和"无权访问"等于免费提供目录枚举接口。

**两个独立的凭据体系。** 浏览器会话与 App Password 走不同路径，WebDAV 不接受会话 Cookie（桌面客户端无法完成 OIDC 跳转）。所有 256 位随机令牌用 HMAC 派生索引；只有人选的密码（分享密码、break-glass）才用 Argon2id——给随机令牌上 Argon2 只是给每个 WebDAV 请求增加 60ms，不带来安全性。

**每一次写操作与它的审计记录在同一个事务里。** 审计写入拒绝在未启用链式写入时静默通过，宁可报错也不留下无法追溯的变更。

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

备份与恢复演练见 [deploy/backup/restore-drill.md](deploy/backup/restore-drill.md)。恢复演练已被执行过：逻辑备份 → 新数据库恢复 → 审计哈希链校验 → 用恢复后的数据启动服务并读回文件，全部通过。

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

- `s3` 后端已实现（bucket 必须事先存在，启动时校验连通性），但**只在 moto 的 S3 模拟上验证过**，
  没有对真实 AWS S3 / MinIO / R2 做过互操作测试。
- `tree` 布局只能在 `local` 后端上使用：它按定义是文件系统镜像。
- 群组共享空间未实现。授权层对群组授权一律返回"无授权"（fail-closed），避免桩代码悄悄放宽权限。
- 共享锁（shared lock）与 `REPORT`/`sync-collection` 未实现，客户端会收到明确的 501 而不是空结果。
- 管理员看不到用户文件内容，只能看元数据与强制删除。这是刻意的。
- 回收站不自动清理；空间计入配额。
- `email` 分享的验证码需要配置 `mail.*`。未配置时服务照常启动，并在每次生成验证码时告警，而不是假装发送成功。
- 审计导出对普通用户的范围是"我做过的事"加"关于我数据的匿名事件"（匿名下载、被拒的收件人尝试）；只有管理员能导出全部。
