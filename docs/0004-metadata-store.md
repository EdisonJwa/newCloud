# ADR-0004：元数据层（store）与 M1 接入约定

状态：已接受
日期：2026-10-21
关联：[0002](0002-backend-skeleton-adr.md) §5、[0003](0003-storage-layout.md)

---

## 0. 一句话

**元数据层只认 ID，不认路径，也不做授权判断。** 路径是给人看的，授权在 `authz` 里做，两者不混。

---

## 1. 为什么分成 `store` / `authz` / `storage` 三层

| 层 | 知道什么 | 不知道什么 |
|---|---|---|
| `storage`（layout / blobstore / tree） | owner 字符串 + 相对路径 → 字节 | 用户是谁、能不能读 |
| `store` | 节点树、配额、版本、审计入队 | 谁在操作、有没有权限 |
| `authz` | Actor + nodeID → Cap | 节点叫什么、字节在哪 |

**关键约束：`store` 的导出方法签名里不出现 `Actor`。**

这不是洁癖。如果 `store.GetNode(actor, id)` 能拿到 actor，就会出现"这里已经传了 actor，那我在 store 里顺手判断一下权限"的代码——授权逻辑随即分散到两个包，IDOR 就从这个缝里进来。所以 `store` 只接受 `uuid.UUID`，调用方必须先拿到 `authz.Cap` 才能取到 ID。

```go
// authz 层
cap, err := authorizer.Authorize(actor, nodeID)
if err != nil { return 404 }          // 注意：不是 403

// 只有拿到 cap 之后，才能通过它取到 nodeID 去查元数据
node, err := tx.Node(ctx, cap.NodeID())
```

**为什么统一返回 404 而不是 403**：403 会告诉攻击者"这个 ID 存在但你没权限"，等于免费提供一个枚举接口。

---

## 2. 事务模型：审计、任务、业务同生共死

`store.Tx` 里同时有 `CreateFile` / `Audit` / `Enqueue`。它们必须在同一个事务里，原因：

- **只写业务不写审计** → 有操作没记录，审计失去意义；
- **只写审计不写业务** → 日志说有下载，实际没发生，取证时误导；
- **业务提交了但任务丢了** → 文件可见但永远不会被扫描，病毒扫描形同虚设。

`Memory` 实现用快照回滚模拟这一点，并有测试专门验证：

```
TestTransactionRollbackLeavesNoTrace
```

它在一个事务里先建一个文件、写一条审计，然后故意让第二个文件超配额失败，断言：文件不可见、配额没被消耗、审计也没有残留。

---

## 3. 内存实现不是玩具，是契约的一部分

`store.Memory` 不是"等 PG 来了就删掉"的占位。它实现了和 PostgreSQL 相同的**不变量**，所以在这里发现的问题在 PG 里同样存在：

| 不变量 | Memory 怎么保证 | PostgreSQL 怎么保证 |
|---|---|---|
| 同级文件名唯一（回收站不算） | `checkName` 跳过已 trash 的 | partial unique index `WHERE trashed_at IS NULL` |
| 文件名安全 | `layout.ValidateSegment` | `CHECK (name !~ '[/\\]')` + 应用层校验 |
| 目录不能移进自己的子树 | 沿 parent 链上溯 | 应用层上溯（SQL 递归 CTE） |
| 配额按**增量**算 | `checkQuota(delta)` | 同样的 SQL |
| 回收站不释放配额 | 测试明确断言 | 同上（删除才释放） |

**`TestQuotaUsesDeltaOnOverwrite`** 值得单独说：它覆盖"重复上传同一文件把配额算两次"这个 bug。如果配额按 `new_size` 而非 `new_size - old_size` 计算，用户反复保存一个大文件就会提前撞墙。

**`TestConcurrentSameNameOnlyOneWins`** 覆盖并发竞态：8 个 goroutine 同时创建 `same.txt`，必须恰好 1 个成功、7 个 `ErrConflict`。这个测试正是 partial unique index 存在的原因。

---

## 4. 路径解析：大小写精确

```go
func (m *Memory) Resolve(ctx, ownerID, path) ([]Node, error)
```

返回**节点的祖先链**（含叶子），不是路径字符串。原因：调用方需要 ID 来做授权，而路径会变。

**大小写处理是一个刻意的选择**：

- 索引用 `strings.ToLower(name)`，因为查表要快；
- 但命中后**额外比较原始大小写**，不匹配就当不存在。

为什么：`Readme.md` 和 `README.md` 在 Windows/macOS 上是同一个文件，在 Linux 上是两个。如果服务端静默地把两者当成一个，用户从 Linux 客户端上传两个文件后，其中一个会莫名消失。宁可让第二个上传失败（明确的 `ErrConflict`），也不要静默合并。

---

## 5. 回收站语义

`Trash` 是递归的：删目录 = 整棵子树进回收站。这一点没有妥协空间——如果只 trash 父目录而子节点保持 live，`Resolve` 沿着 parent 链会走进一个"父节点已删但自己还在"的状态，属于典型的一致性问题。

`Restore` 目前只恢复单个节点（不递归）。**这是一个已知限制**：恢复目录时应当同时恢复其子树，否则会出现"目录回来了但里面是空的"。M1 收尾时必须补上，测试已经留了位置。

---

## 6. 版本与配额的关系

- `UpdateFile` 只在 `KeepVersion=true` 时保存旧版本；
- `MaxVersions` 是**上界，不是触发条件**：超过就丢弃最旧的；
- **`tree` 布局应该把 `MaxVersions` 调低**，因为每个版本都是完整副本。调用方应按 `Caps().CheapVersions` 决定：

```go
maxVersions := 10
if !store.Capabilities().CheapVersions {
    maxVersions = 3  // tree: 每个版本占完整空间
}
```

这就是为什么能力要显式暴露，而不是让调用方猜。

---


## 7. blob 布局如何拿到元数据

blob 布局把目录树只放在数据库里，磁盘上是扁平的哈希文件。于是 storage 层必须能向元数据层提问：`(owner, path) → hash`。

接口刻意做得很窄：

```go
type Resolver interface {
    HashFor(ctx, owner, path) (hash [32]byte, ok bool, err error)
    Record(ctx, owner, path, hash [32]byte, isDir bool) error
    Forget(ctx, owner, path) error
    IsDir(ctx, owner, path) (bool, bool, error)
    Children(ctx, owner, dirPath, limit) ([]ResolverNode, error)
}
```

**为什么 `owner` 是字符串形式的 UUID，而不是类型化的 ID**：解析失败的路径要能报错而不是 panic。`parseOwner` 失败返回 `ErrInvalidName`，调用方按"不存在"处理。

**为什么 `store` 不 import `storage`**：依赖方向必须是 storage → store，否则两层会互相引用。代价是 `store` 自己实现了 `layout.ValidateSegment` 的等价校验——这一点由共享的契约测试保证不漂移。

**`Walk` 返回 `ErrWalkUnsupported`，而不是空结果。** 空结果会让容量扫描、孤儿回收这类任务静默跳过全部数据；明确报错才能让调用方知道"这种布局不支持遍历，请改用元数据查询"。

---

## 8. M1 完成情况

| 项 | 状态 | 说明 |
|---|---|---|
| `store.Memory` | 完成 | 完整语义实现，非桩代码；与 PG 共用同一套契约测试 |
| `store` PostgreSQL 实现 | 完成 | `WithTx` + pgx，含递归 CTE 的回收站/恢复/移动 |
| `store.Restore` 递归 | 完成 | 不会复活被单独删除的后代 |
| `RestoreVersion` | 完成 | 两个实现都实现（早期 Memory 返回 `errNotImplemented`） |
| WebDAV `PROPFIND` | 完成 | 流式输出，有子项数量上限 |
| WebDAV `PUT` | 完成 | 先写内容，再在事务里写元数据与审计 |
| WebDAV `LOCK` | 完成 | 持久化锁表，含所有权校验与超时清理 |
| App Password | 完成 | 签发 / 校验 / 吊销 / 列表 |
| 回收站列表 | 完成 | 只列出被删子树的根，与恢复语义一致 |

---

## 9. 端到端验证暴露的真实缺陷

记录在此，因为它们说明"测试全绿"不等于"能跑"。

### 9.1 预建账号首次登录返回 500

`UpsertIdPUser` 用单条 `INSERT ... ON CONFLICT (idp_issuer, idp_subject)`。如果账号已由 SCIM 或管理员预建（有用户名、无 IdP 身份），插入会撞 `users_username_key` 唯一索引，用户第一次登录就收到 500。

更麻烦的是：**内存实现没有这个约束，所以单元测试全绿。**

修复方式是"先查后写"，而不是依赖 `ON CONFLICT`：PostgreSQL 一条语句失败后整个事务进入 aborted 状态（25P02），后续任何查询都会报错，所以"撞了再试另一条路"在这里不可用。查询顺序是：先按 IdP 身份找，找不到再按用户名找；按用户名认领时，如果该行已属于另一个 IdP subject，则拒绝——否则就是用户名碰撞导致的账号接管。

两侧实现都改，并把这一场景加进共享契约测试（`UpsertIdPUser/ClaimsPreprovisionedAccount`、`RejectsUsernameHijack`）。

### 9.2 WebDAV PUT 永远 404

`Handler.blobIDFor` 是个返回零值 UUID 的桩函数。PostgreSQL 的 `nodes.blob_id` 外键拒绝零 UUID，于是每一次 WebDAV 上传都失败，而客户端只看到一个 404。

**内存实现接受零 UUID，所以 DAV 测试套件全绿。**

修复是接上 `Store.BlobIDFor`。回归测试 `TestPutRecordsBlobID` 断言写完之后节点确实带上了 blob 引用——只看 HTTP 状态码抓不到这个缺陷。该测试已用旧实现反向验证过（改回桩函数即失败）。

### 9.3 会话 `jti` 不是 UUID

会话标识原本是 base64 随机串，而 `sessions.id` 是 `uuid` 类型。内存实现接受任意字符串，PostgreSQL 报 `22P02`。改为 `uuid.NewString()`：jti 只是吊销用的标识符，凭据是 HMAC 签名后的 ticket，没有理由不用 UUID。

### 9.4 上传缓冲区写在系统临时目录

`spoolToTemp` 用 `os.CreateTemp("")`，即系统临时目录。容器里 `/tmp` 常常不可写或与数据盘不同文件系统——后者会让发布 blob 从 `rename(2)` 退化为整文件复制。改为在存储根目录下开 `tmp/`。

### 9.5 三个列表接口返回三种不同的字段名

`{shares: []}`、`{items: []}`、`{app_passwords: []}`。前端按 `data.items || data` 兜底，拿到的是对象而不是数组，`for...of` 直接抛异常。

统一为 `{items: []}`。同时发现回收站接口是硬编码的空数组桩，补上了真实实现（`Tx.Trashed`，只返回被删子树的根）。

这个缺陷是**截图**发现的，不是测试发现的：当时的 UI 检查只断言"切到 Shares 后内容变了"，而视图抛异常后 toast 报错、内容也确实变了，检查因此通过。检查已改为断言视图自身的内容存在、且没有出现错误 toast。

### 9.6 邮箱分享没有校验收件人（安全缺口）

`shares.kind = 'email'` 的分享会记录 `share_recipients` 行，但 `Resolve` 从不检查它们。结果：**"只分享给某个邮箱"实际上等于公开链接**，只是顺带记下了本来想给谁。

这是本轮最重要的一处修复。现在：

- 邮箱分享的下载、上传、页面三者都过同一个 `gate()`，与密码门并列而不是合并；
- 验证凭证是发到该地址的一次性验证码（`ticket.PurposeEmailVerify`），绑定「分享 + 地址」两者——只绑分享，则任何受邀者转发验证码就能放人进来；只绑地址，则同一人的验证码能开所有分享；
- 未受邀地址请求验证码时返回与受邀地址**完全相同**的 202，否则这个端点变成"某人是否被邀请到某分享"的探测接口；
- 验证成功与拒绝都写审计。拒绝事件尤其重要：一串针对同一分享的拒绝，就是链接被转发的样子。

### 9.7 审计导出把最该看的事件过滤掉了

第一版导出对普通用户按 `actor_user_id` 过滤，语义是"我做过的事"。但分享所有者真正要问的是"我的文件发生了什么"——**匿名下载、被拒的收件人尝试，这些事件根本没有 actor 账号**，于是恰好被过滤掉。

改为 `ScopeToOwner`：`actor_user_id = 我 OR share_id IN (我拥有的分享)`。这两种情况是不相交的，正是区别所在。

### 9.8 `actor_email` 可为 NULL

审计表里 `actor_email` 允许为空（匿名事件），但读取路径扫描进 `string`，于是**导出刚好在匿名事件上 500**。用 `COALESCE` 修正，`VerifyAuditChain` 里同样的隐患一并修掉。

这个缺陷只在真实 PostgreSQL 上出现——内存实现的字段是 Go 字符串，永远不为 NULL。又一次说明契约测试必须跑两种后端。

---

## 10. 验收方式

- 单元与集成：`go test ./...`，含内存与 PostgreSQL 两套实现的共享契约测试。
- 真实链路：`tools/e2e` 对着跑起来的服务走完整 OIDC 登录、上传下载、分享计次、WebDAV 全流程。
- 界面：`tools/uicheck` 用 headless Chrome 加载真实页面，验证渲染、tab 切换、以及用户数据以文本进入 DOM。
- 部署：`docker build` 在构建阶段跑 `go vet` 与全部测试；容器内 `migrate`、`serve`、`healthcheck` 均验证过。
- 备份：逻辑备份 → 新数据库恢复 → 审计哈希链校验 → 用恢复后的数据启动服务并读回文件，全流程通过。
