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

### 3. 恢复 Blob

```bash
restic restore latest --target /var/lib/newcloud --include /var/lib/newcloud/blobs
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
| 4 账号 / 21 节点 / 12 blob / 45 审计事件（dump 78.2 KiB） | 1.12 s | 1.22 s（本地文件复制） | 1.22 s | **通过** | 见下 |

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

- **restic 仓库本身的恢复**没有跑。这一轮验证的是"blob 字节在文件级复制后仍然正确且可读"，不是 restic 的快照恢复流程；restic 在当前环境没有安装。两者的差别在于前者验证内容完整性，后者还验证快照管理与去重索引。
- **大库耗时**未知。78 KiB 用时约 1.2 秒，不能外推到 40 TiB；RTO 达标与否必须用真实体量重测。
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
