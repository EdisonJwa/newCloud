# ADR-0003：存储布局可选（blob / tree）

状态：已接受
日期：2026-10-21
关联：[0001-scope-and-language.md](0001-scope-and-language.md) §8.7、[0002-backend-skeleton-adr.md](0002-backend-skeleton-adr.md) §6

---

## 0. 背景与决策

原方案把"内容寻址 blob"写死为唯一存储方式。你要求让用户可选：要么直接按目录树存文件，要么存 blob。

**决策：做成部署级配置项 `storage.layout`，两个实现，一套接口。**

```yaml
storage:
  backend: local       # 字节存在哪：local | s3
  layout: blob         # 怎么组织：blob | tree
```

**为什么是"部署级"而不是"用户级开关"：**

两个布局的差别不只是文件命名，而是数据结构本身：

- blob 布局下**目录树只存在于 PostgreSQL**，磁盘上是扁平哈希；
- tree 布局下**目录树就是磁盘上的目录**，数据库只是索引。

这意味着备份方式、迁移路径、容量核算、故障恢复流程全都不同。让单个用户在同一个实例里混用，等于要求系统同时维护两套真相，容量越大越难收场。

**允许的切换方式**：换配置 + 跑一次迁移工具（`newcloud migrate-storage`，M2 实现）。不允许在运行中混用。

---

## 1. 两种布局的实际差异

| 维度 | `blob`（内容寻址） | `tree`（目录树镜像） |
|---|---|---|
| 磁盘路径 | `<root>/ab/cd/<sha256>` | `<root>/<owner>/<用户看到的路径>` |
| 目录树 | 只在 PostgreSQL | 就是文件系统本身 |
| 去重 | ✅ 相同内容只存一份 | ❌ 各存各的 |
| 版本成本 | ✅ 新版本只增差异 blob | ❌ 每个版本都是完整副本 |
| 改目录名（含百万文件） | ✅ 改一行元数据 | ❌ rename 整个子树 |
| 管理员用 `ls` 查看 | ❌ 只能看到哈希 | ✅ 所见即所得 |
| rsync / rclone 直接同步 | ⚠️ 能同步但不可读 | ✅ 天然适合 |
| 路径穿越风险 | ✅ 磁盘上根本没有路径 | ⚠️ 每个路径段都必须校验 |
| 删除一个用户 | 要按 refcount 回收 | `rm -rf <owner>` |
| 磁盘占用 | 更小（去重 + 差异版本） | 更大（版本与回收站各占一份） |

### 选型建议

| 你的情况 | 建议 |
|---|---|
| 容量大、用户多、有大量重复文件（模板/镜像/归档） | **`blob`**（默认） |
| 需要运维直接看文件、用 rsync 备份、接现有 NAS 流程 | `tree` |
| 磁盘容量紧张 | `blob`（去重能省 20–40%） |
| 文件数少、单文件大、几乎不改写（如视频归档） | `tree`（去重收益小，可读性收益大） |
| 需要"删用户就删干净"的合规要求 | `tree` |

**容量大时我仍推荐 `blob`**：版本和回收站会让 tree 布局的实际占用接近 2 倍，而 blob 的去重 + 差异版本能把这部分吃掉。但如果你的人手少、更看重"出问题能直接进机器看文件"，`tree` 是合理的取舍——这正是它存在的理由。

---

## 2. 接口设计

两个布局实现同一个接口（`internal/storage/layout`）：

```go
type ContentStore interface {
    Mode() Mode
    Put(ctx, Node, r io.Reader, size int64) (PutResult, error)
    Open(ctx, Node) (io.ReadSeekCloser, error)   // 必须可 Seek（Range）
    Stat(ctx, Node) (Node, error)
    Mkdir(ctx, Node) error
    Move(ctx, src, dst Node) error
    Delete(ctx, Node) error
    List(ctx, dir Node, limit int) ([]Node, error)
    Walk(ctx, dir Node, fn func(Node) error) error
    Capabilities() Caps
}
```

**关键点：`Walk` 的语义差异必须显式暴露。**

- tree 布局能原生遍历（文件系统就有层级）；
- blob 布局**做不到**——目录树在数据库里。

如果 blob 布局的 `Walk` 返回空列表，调用方会以为子树为空，于是配额重算、病毒扫描、存储迁移这些任务会静默跳过数据。所以它返回 `ErrWalkUnsupported`，调用方必须改用元数据查询。

**`Capabilities()` 让 UI 提前知道差异**，而不是点下去才失败：

```go
type Caps struct {
    Deduplicated    bool  // blob: true, tree: false
    CheapVersions   bool  // blob: true, tree: false
    DirectFSInspect bool  // blob: false, tree: true
    EfficientRename bool  // blob: true, tree: false
}
```

界面上直接体现为：
- tree 模式下，版本页提示"每个版本占用独立空间"；
- blob 模式下，隐藏"存储路径"字段（没有意义）；
- tree 模式下，重命名大目录时给出进度提示（因为真的在 rename 百万个文件）。

---

## 3. tree 布局的安全设计（重点）

tree 是**明显更危险**的那条路：路径一旦算错就是任意文件读取。所以做了三层防御，而不是一层字符串校验。

### 第一层：段级校验

`layout.ValidateSegment` 拒绝：
- `..`、`.`、空段
- `/`、`\`、NUL、控制字符
- Windows 非法字符 `<>:"|?*`（`C:evil` 是驱动器相对路径，`file:stream` 是 NTFS 数据流）
- Windows 保留设备名（`CON`、`NUL`、`COM1`…）——否则文件同步到 Windows 客户端后打不开，用户会报"文件损坏"
- 尾部点或空格（Windows 会静默去掉，导致 `report.` 与 `report` 撞名）
- 超过 255 字节

### 第二层：系统调用级隔离（`os.Root`）

Go 1.24+ 的 `os.Root` 用 `openat(2)` 解析路径，**在内核层面拒绝逃逸**，包括符号链接。

这不是冗余——第一层有 bug 时，第二层仍然兜得住。有一个测试专门验证：在 store 里放一个指向外部文件的符号链接，读取必须失败。

```go
root, _ := os.OpenRoot(dir)
root.Open("../../etc/passwd")  // -> "path escapes from parent"
```

### 第三层：写入原子性

写入先落 `.newcloud-tmp/<random>`，fsync 后 `rename(2)` 到目标位置。

效果：上传中断、进程崩溃、磁盘满，**读者永远看到旧内容或新内容，不会看到半个文件**。测试 `TestInterruptedUploadKeepsOldContent` 覆盖这个场景。

### 其它约束

- **owner 也是路径段**，按同样规则校验（`owner=".."` 会让某账号的所有文件落到 store 外面）
- **禁止删除 store 根**（`path=""`），否则用户删自己的根目录等于删账号
- **禁止把目录 move 进自己的子树**（文件系统会返回 EINVAL，但提前给出清晰错误）
- **跨 owner 的 move 直接拒绝**，必须走"复制 + 删除"（否则等于越权改名）

---

## 4. blob 布局的实现说明

`internal/storage/blobstore` 是适配器，把已有的 `blob.Store`（内容寻址）接到统一接口。

**它需要元数据层来回答 `(owner, path) -> hash`。** 生产环境这来自 PostgreSQL；当前用 `internal/storage/resolve` 的内存实现先把链路打通，并在启动时**明确警告不可用于生产**：

```
WARN blob layout is using the in-memory node resolver;
     metadata will not survive a restart until the PostgreSQL resolver is wired
```

**几个刻意的决定：**

1. **`Delete` 不删字节。** 相同内容可能被其他用户、其他版本、回收站引用。引用计数在数据库里，不在存储层。适配器只删元数据映射。
2. **`Move` 不移动字节。** 只改映射。这正是 blob 布局"改目录名是 O(1)"的原因。
3. **元数据指向不存在的 blob 时会大声报错**，带上哈希。简单返回 404 会掩盖数据损坏——用户以为文件没了，实际是存储出了问题。
4. **大文件先 spool 到临时文件**。blob 存储需要先知道内容地址才能写，所以必须完整读一遍；但不能读进内存（100 GiB 上传会吃掉 100 GiB RAM）。临时文件在 Unix 上立即 unlink，句柄关闭即释放。

---

## 5. 一致性保证

`internal/storage/layouttest` 是一套共享的契约测试，**两个布局跑同一套用例**。

这保证了 WebDAV 和 API 层不需要 `switch layout { ... }`。如果某个行为在两种布局下确实不同（比如去重），它属于 `Caps`，由调用方显式检查，而不是散落在 handler 里。

已覆盖：写入/读取/覆盖、可 Seek、Size 不匹配拒绝、不安全路径拒绝、空 owner、目录不可 Put、Mkdir+List（不递归）、limit、Move、跨 owner Move 拒绝、删除语义、owner 隔离、Caps 与模式一致。

---

## 6. 迁移路径（M2 实现）

`newcloud migrate-storage --to tree` / `--to blob`

| 方向 | 做法 | 风险 |
|---|---|---|
| blob → tree | 按元数据遍历节点树，从 blob 读出写到 `<owner>/<path>` | 空间需要额外 1 份（不能边删边写）；大目录慢 |
| tree → blob | 遍历磁盘，逐个计算 SHA-256 写入 blob，同步更新元数据 | 需要先算完所有 hash；期间不能写入 |

**共同要求：**
- 迁移前强制全量备份 + 校验；
- 迁移必须在维护窗口做，或做双写过渡（只在 M3 之后考虑）；
- 迁移后跑 `fsck` 式校验：逐文件比对 hash 与大小。

---

## 7. 对 M1 的影响

因为先做了这一层抽象，M1 的 WebDAV 实现**不需要知道底层是哪种布局**。否则按原计划先写死 blob、之后再加 tree，会导致节点树、版本、配额、回收站全部返工。

具体接入方式：

```go
// WebDAV 的 PUT
res, err := h.store.Put(ctx, layout.Node{Owner: actor.Owner(), Path: vpath}, body, size)

// 响应的 ETag 用内容地址，两种布局都一致
etag := fmt.Sprintf(`"%x"`, res.Hash[:8])

// 版本策略看能力，而不是看类型
if h.store.Capabilities().CheapVersions {
    // 保留全部历史版本（blob：只占差异空间）
} else {
    // 提示用户每个版本占完整空间，或降低保留数
}
```

---

## 8. 配置示例

```yaml
storage:
  backend: local
  layout: tree
  local:
    # tree 布局：文件直接在这里，按 <owner>/<path> 组织
    root: /var/lib/newcloud/files

# 或者
storage:
  backend: local
  layout: blob
  local:
    # blob 布局：<root>/ab/cd/<sha256>
    root: /var/lib/newcloud/blobs
```

**启动时的日志会明确打印实际生效的布局**，`/readyz` 也会返回，避免"以为配了 A 实际跑了 B"。

---

## 9. 未决

1. **S3 + tree 组合被拒绝**（对象存储没有目录层级可镜像）。如果你希望 S3 也能"看起来像目录"，那其实是 blob 布局加一个可读的对象 key 前缀方案，属于另一个设计。
2. **迁移工具的具体实现**排在 M2。
3. **配额统计**：tree 布局用 `Usage()` 遍历求和（慢，但文件系统就在那）；blob 布局从数据库的 `used_bytes` 直接读。两者语义相同，性能差 3 个数量级，UI 上可能需要显示"正在计算"。
