# 恢复演练手册

**这是 M5 的验收项，不是可选文档。**

一个备份只有在被完整恢复过一次之后才算存在。演练要求记录实际耗时，并把结果写进本文件。

## 为什么单机部署必须做这个

单机 Docker 没有副本可以接管。整机故障、磁盘损坏、误删、勒索软件——所有这些的唯一出口都是备份。可用性的上限就是那台机器，所以 RTO 完全取决于恢复演练做得够不够熟。

## RPO / RTO 目标

| 数据 | RPO | RTO |
|---|---|---|
| 元数据（PG） | ≤ 15 分钟（WAL 归档连续） | ≤ 1 小时 |
| Blob 内容 | ≤ 24 小时（restic 增量） | 视数据量，40 TiB 需 ≤ 24 小时 |

## 演练步骤

### 1. 准备干净环境

**不要在运行中的实例上演练。** 用一台等价配置的空机器，否则演练会污染生产或得出错误的耗时。

```bash
mkdir -p /var/lib/newcloud /var/lib/newcloud-pg
docker compose up -d postgres
```

### 2. 恢复 PostgreSQL

```bash
# 数据库为空
pg_restore --clean --if-exists --no-owner \
  -d postgres://newcloud:$PG_PASSWORD@localhost:5432/newcloud \
  /srv/backup/pg/newcloud-<STAMP>.dump

# 如果 RPO 要求回放到故障点，继续应用 WAL 到目标时间。
# 单机方案下这一步决定你到底丢了 15 分钟还是 24 小时。
```

校验：

```sql
-- 行数应与备份时一致
SELECT count(*) FROM nodes;
SELECT count(*) FROM audit.events;

-- 审计链完整性：链尾 hash 必须与最近一次 anchor 记录一致
SELECT day, encode(head_hash,'hex') FROM audit.anchors ORDER BY day DESC LIMIT 1;
```

### 2.5 备份顺序：先 pg_dump，再备份 Blob

这个顺序不是风格问题。备份是两个独立时刻的两份产物——数据库 dump 和 blob 目录副本——它们无法原子地同时取到，中间总有一个窗口。窗口里如果有写入，是否会产生"恢复后引用了不存在的字节"，取决于**顺序**，而哪个顺序安全取决于写入路径的一条不变量：

> **内容先落盘，元数据行后写入。**

（`internal/server/files.go` 里 `Content.Put` 在 `publishFile` 之前；`TestBlobIsWrittenBeforeItsRow` 守住这条不变量。）

给定这条不变量：

| 顺序 | 窗口内的写入 | 恢复后的结果 |
| --- | --- | --- |
| **先 pg_dump，再备份 Blob** | 字节已在副本里（先于行落盘），行不在 dump 里 | 库里没有这条记录，副本里多一个没人引用的 blob。**代价是一点空间，文件只是不在这次备份里**——正确 |
| 先备份 Blob，再 pg_dump | 行在 dump 里，字节在副本读完之后才落盘 | 恢复后的元数据指向不存在的内容，**每次读取都失败**。恢复看起来是成功的，直到有人打开那个文件 |

所以：**先 dump，再备份 blob**。会留下孤儿 blob，不会留下悬空引用。孤儿 blob 由 GC 回收；悬空引用没有任何机制能修。

**这一条我写错过一次。** 初稿把两个顺序写反了，理由是"先取到的产物更旧更安全"——但没有去确认写入路径里**字节和行哪个先落**。方向搞反的文档比没有文档更糟：它读起来像建议，照着做的运维会精确地制造出上一段表格里那种"恢复成功但每次读取都失败"的状态。`TestBackupDocsStateTheRightOrder` 现在会校验这句话的措辞。

恢复之后的顺序仍然成立：先 `pg_restore` 数据库，再恢复 blob。反过来的话，同样会出现"库指向还没有的字节"。
### 3. 恢复 Blob

```bash
# Restore to a scratch directory, then move the blob subtree into place.
#
# Both steps are needed, and the first version of this page omitted the second. `restic restore`
# recreates the *absolute* path it backed up underneath --target, so restoring
# /var/lib/newcloud/blobs to --target /srv/restore produces
# /srv/restore/var/lib/newcloud/blobs — not /srv/restore/blobs. Pointing --target at the parent
# and hoping --include scopes it does not help either: --include selects what to restore, it does
# not flatten where it lands.
#
# Restoring straight onto the live root is the tempting shortcut and the wrong one: a partial or
# failed restore then leaves a half-populated blob store where the server is already reading.
restic restore latest --target /srv/restore

# Then place it. The trailing slash on the source copies the *contents* of blobs/ into
# $BLOB_ROOT; without it you get $BLOB_ROOT/blobs/blobs.
BLOB_ROOT=/var/lib/newcloud/blobs
install -d "$BLOB_ROOT"
cp -a "/srv/restore/var/lib/newcloud/blobs/." "$BLOB_ROOT/"
```

校验：

```bash
# 抽样校验 hash 与文件名一致（内容寻址的自校验特性）
find /var/lib/newcloud/blobs -type f -name '[0-9a-f]*' -print0 \
  | shuf -z -n 1000 \
  | while IFS= read -r -d '' f; do
      want=$(basename "$f")
      got=$(sha256sum "$f" | cut -d' ' -f1)
      [ "$want" = "$got" ] || echo "CORRUPT: $f"
    done
```

### 4. 启动并验证

```bash
docker compose up -d
curl -fsS localhost:8080/readyz
```

功能抽查：

- [ ] 能登录（Pocket ID 正常）
- [ ] 能看到自己的文件树，目录层级正确
- [ ] 随机下载 10 个文件，字节数与 hash 正确
- [ ] 分享链接可访问，密码/期限仍生效
- [ ] 审计页能查到最近的下载记录
- [ ] WebDAV 客户端能连上（`rclone lsd` 或映射网络驱动器）

## 演练记录

每次演练后填写。没有记录 = 没做过。

| 日期 | 备份时间点 | 数据量 | PG 恢复耗时 | Blob 恢复耗时 | 总 RTO | 结果 | 发现的问题 |
|---|---|---|---|---|---|---|---|
| 2026-10-22 | 首次演练 | 4 账号 / 21 节点 / 12 blob / 45 审计事件（dump 78.2 KiB） | 1.12 s | 1.22 s（本地文件复制） | 1.22 s | **通过** | 见下 |
| 2026-10-22 | 规模点 1 | 10 账号 / 100 文件 / 1 000 审计事件（dump 130 KiB，1 171 行） | — | — | 1.19 s | **通过** | 见下方"规模测量" |
| 2026-10-22 | 规模点 2 | 50 账号 / 2 000 文件 / 20 000 审计事件（dump 1.2 MiB，22 301 行） | — | — | 8.32 s | **通过** | 同上 |
| 2026-10-22 | 规模点 3 | 100 账号 / 10 000 文件 / 100 000 审计事件（dump 5.5 MiB，110 401 行） | — | — | 40.78 s | **通过** | 同上 |

### 2026-10-22 首次演练（自动化，`newcloud drill`）

演练已从"人工照做的手册"变成子命令，因为人工流程的实测结果就是**从未被执行**——本文件的上一版从写入到 2026-10-22 一直是 `_待首次演练_`。

数据集是真实写入的（6 个账号、2 个目录、5 个文件走真实上传 API、1 个群组、1 条 ACL、40 条审计事件），不是手工构造的行：

```
restore drill
  dump size        77.2 KiB
  tables compared  20
  rows compared    75
  audit chain      45 events, verified=true
  elapsed          928ms
  RTO              1s (target 1h0m0s) — within target

  VERDICT: PASS — the backup restores and the audit chain survives
```

第二次运行 971 ms，结果一致。

**反向验证（关键）**：一个永远不会失败的演练证明不了任何事，所以故意破坏后重跑：

| 破坏方式 | 演练结果 |
|---|---|
| 修改一条审计记录的 `action` | `FAIL` —— `the restored audit chain does not verify at row 0 of 45: hash mismatch` |
| 翻转一个 blob 的首字节 | `FAIL` —— `top.txt: restored bytes hash to ... metadata recorded ... — the backup is corrupt` |
| `SCRATCH_DSN` 与 `LIVE_DSN` 相同 | 拒绝运行 |
| 不提供 `SCRATCH_DSN` | 拒绝运行 |

第一条尤其重要：行数与表数**全部匹配**，唯一暴露问题的是链校验。这说明第五步不是装饰。

### Blob 内容校验（第八轮补上）

数据库演练只证明**元数据**活得下来，它完全没说字节的事——而字节才是用户真正的数据。一次恢复可能带回一棵完美一致的节点树，指向从未被复制、或复制时被截断的 blob：结果是部署能列出每个文件，却打不开任何一个。

现在演练第 6 步做 blob 校验：把 blob 根复制一份、在副本上开一个内容存储、把**每一个文件节点**读回来比对。

关键设计：**期望的哈希来自元数据，不是来自现存副本**。第一版我拿"恢复出来的字节"和"现存字节"比，结果在故意破坏一个 blob 之后仍然 **PASS**——因为两边都坏，比对自然相等。改成用 `blobs.sha256` 里记录的内容地址校验后，同样的破坏立刻被抓到：

```
blobs            12 files, 11 verified, 31.3 KiB
VERDICT: FAIL
  - top.txt: restored bytes hash to c0d5f09b9940, metadata recorded 5029feaf3e6b — the backup is corrupt
```

这是一个"测试通过了但什么都没验证"的实例，也是这一轮最值得记下来的东西。

**本次仍未验证的三件事**（写在"通过"旁边，而不是藏在后面）：

- ~~**restic 仓库本身的恢复**没有跑。~~ **已补上（第二十八/二十九轮）**：装上 restic 0.19.1 后完整跑通了 `blob-backup.sh`（backup → forget → prune → `restic check` 报 "no errors were found"）与 `restic restore`。删除整个 blob 根后恢复，两个 blob 的 SHA-256 与原文件逐字节一致。**过程中发现并修掉两个真问题**：① `--one-file-system` 在 Windows 上直接 panic（"Device IDs are not supported on Windows"），脚本在保存一个字节之前就退出了；② 本文档原先的恢复命令是错的——`restic restore --target X` 会把备份时的**绝对路径**在 X 下重建，于是得到 `X/var/lib/newcloud/blobs` 而不是 `X/blobs`，`--include` 只筛选**恢复什么**、不改变**落在哪里**。已改为"恢复到 scratch 再搬运"的两步流程并实测通过。两者的差别在于前者验证内容完整性，后者还验证快照管理与去重索引。
- ~~**大库耗时**未知。78 KiB 用时约 1.2 秒，不能外推到 40 TiB。~~ **有进展（第三十七轮）**：见下"规模测量"。三个跨 43 倍的数据点表明**耗时随 dump 体积线性增长**，没有出现超线性拐点——这比单一测量点有用得多，但**仍不等于 40 TiB 的 RTO 结论**，见该节的限制说明。
- **PITR**（WAL 归档恢复到任意时间点）没有演练。本次验证的是"从 dump 恢复"，不是"恢复到故障前一秒"，两者 RPO 差一个数量级。

### 在容器化 PostgreSQL 上运行

`pg_dump`/`pg_restore` 在容器内时，两边地址不同（容器内 5432，宿主机发布 15437）：

```bash
DRILL_PG_CONTAINER=newcloud-postgres \
DRILL_DUMP_DSN=postgres://newcloud:$PG_PASSWORD@127.0.0.1:5432/newcloud?sslmode=disable \
DRILL_RESTORE_DSN=postgres://newcloud:$PG_PASSWORD@127.0.0.1:5432/newcloud_drill?sslmode=disable \
LIVE_DSN=... SCRATCH_DSN=... newcloud drill
```

把宿主机地址交给容器内的工具会得到 `Connection refused`，看起来像数据库坏了，其实只是地址配错。

## 常见坑

| 现象 | 原因 |
|---|---|
| `pg_restore` 报 audit schema 权限错误 | 迁移 `0005_app_role.sql` 会重述授权；若手工恢复则需重新 `GRANT INSERT, SELECT ON audit.events TO newcloud_app` |
| blob 恢复后文件存在但打不开 | `blobs/tmp` 与 `blobs/` 落在不同挂载点，启动自检会拒绝；检查挂载 |
| 恢复耗时远超预期 | 备份与生产在同一块盘上——那本来就只是副本，不是备份 |
| 审计链校验失败 | anchor 与数据库不同步，说明恢复的是旧 dump 而 anchor 是新的 |

## 规模测量（第三十七轮）

### 方法

`tools/seedscale` 生成指定规模的账号、文件夹、文件与审计事件（走真实的 store，因此审计哈希链是真的），然后用 `newcloud drill` 在同一台机器上测量 dump → restore → 校验的完整耗时。

```sh
LIVE_DSN=... SCRATCH_DSN=... DRILL_PG_CONTAINER=... \
DRILL_DUMP_DSN=... DRILL_RESTORE_DSN=... \
  go run ./cmd/newcloud drill
```

### 结果

| 规模 | dump | 行数 | 耗时 | 秒/MiB |
|---|---|---|---|---|
| 10 账号 / 100 文件 / 1 000 事件 | 130 KiB | 1 171 | 1.19 s | 9.38 |
| 50 账号 / 2 000 文件 / 20 000 事件 | 1.2 MiB | 22 301 | 8.32 s | 6.94 |
| 100 账号 / 10 000 文件 / 100 000 事件 | 5.5 MiB | 110 401 | 40.78 s | 7.41 |

三个点全部 `PASS`，审计链在每次恢复后都 `verified=true`（分别是 1 000 / 20 000 / 100 000 事件）。

### 能得出什么

**耗时随 dump 体积线性增长。** "秒/MiB" 在 43 倍的体积范围内稳定在 **6.9 – 9.4**，没有随规模上升的趋势；体积增长 9.4× 与 4.6× 时，耗时增长 7.0× 与 4.9×，略低于线性（固定开销被摊薄）。这排除了"某个阈值之后突然变慢"——那正是单点测量无法排除、而外推时最害怕的情况。

### 不能得出什么

**这不是 40 TiB 的 RTO 结论。** 三点限制，必须一起读：

1. **测量上限只有 5.5 MiB**，与 40 TiB 差 7 个数量级。按中位数 7.9 秒/MiB 做线性外推，40 TiB（= 41 943 040 MiB）需要约 **92 000 小时（约 3 800 天）**——这个数字本身就是结论：**如果代价真是纯线性的，那 40 TiB 的元数据根本不可能在 24 小时内恢复。** 所以现实中必然是另一条路径（并行 pg_restore、分区并行、或者元数据规模远小于 40 TiB 而大的只是 blob 字节数）。诚实的说法是：本次测量**不支持**任何 40 TiB 的 RTO 数字，无论朝着乐观还是悲观的方向。
2. **在这里全流程都在同一块盘、同一台机器**，pg_dump 与 pg_restore 没有网络往返。真实部署里备份产物要在网络上搬运，那一段的耗时由带宽决定，与本次测量无关。
3. **`pg_size_pretty` 报的是逻辑大小**，而 40 TiB 的部署里 blob 字节数远大于元数据。本次测量的是**元数据**恢复成本；blob 的恢复成本是另一条曲线（restic 增量恢复，见上文）。

结论应当这样读：**在 5.5 MiB 以内线性，没有观察到拐点；超出这个范围的任何数字都还没有测过。** 真实的 RTO 结论仍然只能靠在等价硬件上用真实体量重跑一次。

> **一个关于本节自身的记录**：这一节的第一版把外推写成"约 82 小时"，实际算术是约 92 000 小时——**差了三个数量级**，而且这个数字直接决定结论（82 小时接近 RTO 目标，会让人以为"差不多达标"；92 000 小时则说明线性外推这条路本身就是错的）。`TestDrillScaleMeasurementsAreConsistent` 现在会从表格里的实测值重算这个数字，差 20% 以上就失败。把错误数字放回去，它报的是 `a factor of 985`。散文里的算术靠人眼审不出来。

### 顺带发现

第一次 seeding 时 `tools/seedscale` 只写了文件元数据、没有写 blob 内容，drill **立刻报 FAIL**：`the restored copy cannot serve it: blob: not found`，100 个文件 0 个可读。这正是 drill 存在的理由——一个"看起来有数据"的数据库和一个真的能服务的数据库，差别只有 drill 会告诉你。