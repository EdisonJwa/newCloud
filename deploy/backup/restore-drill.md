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
| _待首次演练_ | | | | | | | |

## 常见坑

| 现象 | 原因 |
|---|---|
| `pg_restore` 报 audit schema 权限错误 | 恢复后需要重新 `GRANT INSERT ON audit.events TO newcloud_app` |
| blob 恢复后文件存在但打不开 | `blobs/tmp` 与 `blobs/` 落在不同挂载点，启动自检会拒绝；检查挂载 |
| 恢复耗时远超预期 | 备份与生产在同一块盘上——那本来就只是副本，不是备份 |
| 审计链校验失败 | anchor 与数据库不同步，说明恢复的是旧 dump 而 anchor 是新的 |
