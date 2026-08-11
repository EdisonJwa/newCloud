# 0010 — 跨模块契约的实现与验收记录

**状态：** 已实现（P0 契约）；本文件同时记录**仍未实现**的部分，避免"规划"被读成"已完成"。

**输入：** [0010-functional-requirements-v0.2.md](0010-functional-requirements-v0.2.md)（需求与缺口分析 v0.2）。

本文只回答一个问题：需求文档里那些"跨模块契约"，哪些现在有代码、有测试、可复现，哪些还没有。每条结论都指向一个可运行的测试或一条可以手动执行的命令。

---

## 1. 这次真正落地的契约








### 第一次真正执行备份脚本（第四十轮）

前三轮我写了三个备份脚本的顺序测试、不变量测试、以及恢复侧的性质测试——**但那些脚本从未被真正执行过一次**。测试校验的是"顺序对不对、配置读没读"，不是"它能不能跑"。这一轮装了 `pg_dump`（scoop `postgresql` 18.6）和 `restic`，在一份真实数据上把全链路跑了一遍。

**第一件事：文档里的顺序是散文，不是可执行的东西。** `pg-backup.sh` 和 `blob-backup.sh` 各自正确、互不知情，没有任何脚本按顺序调用它们。cron 指向哪一半、以什么顺序，全凭运维有没有读到那段话。补了 `deploy/backup/backup-all.sh`：先 dump、再 blob、中间写一份 manifest 把两者绑在一起。**顺序从"被记住的"变成"可执行的"。**

**第二件事，跑起来才发现的真缺陷**：`pg-backup.sh` 调的是**裸 `pg_dump`**，它从 `PG*` 环境变量取连接、**完全忽略 `DATABASE_URL`**。`backup-all.sh` 导出了 `DATABASE_URL`，结果 `pg_dump` 去连 `localhost:5432`，报 connection refused ——而数据库在 15437。在别的机器上它可能更糟：静默 dump 了 `postgres` 库，运维以为自己有备份。已改为 `pg_dump --dbname="$CONNECTION"`，缺失时**报错退出**而不是用默认值。

**第三件事：`backup-all.sh` 真的跑通了完整链路。**

| 步骤 | 结果 |
| --- | --- |
| dump | 88 508 字节，`pg_restore --list` 可读，含 `audit` schema |
| manifest | dump 路径 + 开始/结束时间 + blob 快照 |
| restic backup / check | 8 blob，4 521 字节，`no errors were found` |
| restic restore | 8/8，**每个文件的 SHA-256 与其文件名一致** |
| pg_restore（在 blob 之后） | 55 节点、200 审计事件 |
| **交叉校验** | 恢复后元数据所需的 blob 在恢复副本中**缺失 0 个** |

这才是 §11.2 "恢复验证不等于有备份文件"要的证据：不只是有文件，而是恢复后的库指向的每一个字节都确实在。

**一个测试写错的记录**：`TestBackupAllRunsTheDumpBeforeTheBlobs` 第一版在**整个文件**里搜索两个脚本名，于是匹配到的是文件头部注释里对顺序的解释，而不是真正的调用行。把 blob 调用挪到 dump 之前，测试照样通过。改成只扫描**可执行行**（跳过注释与空行）之后，同一个变异立刻红。**又一次是"删掉被测行为仍然通过"**——这已经是这个项目里第四次同类错误，共同点都是断言作用在了文本的存在上，而不是行为的位置上。


### 恢复顺序：把"顺序很重要"降级为准确的说法（第三十九轮）

上一轮我把恢复侧的顺序写成"先 `pg_restore` 再恢复 blob，反过来的话**同样**会出现库指向还不存在的字节"。这句话**夸大了**，而夸大方向的错误会让人去找并不存在的数据丢失。

把两种情况摊开：

| 顺序 | 窗口内 | 之后 |
| --- | --- | --- |
| 先恢复 Blob，再 pg_restore | 还没有元数据，**什么都读不到** | 一切正常 |
| 先 pg_restore，再恢复 Blob | 元数据说有文件、字节没到，**读取报 `blob not found`** | 同样恢复正常 |

**两个顺序最终都会收敛，都不会真正损坏数据。** 推荐"先 Blob"是因为它**失败关闭**（什么都读不到，比"读到的文件报找不到"更不容易被误判成数据损坏），而不是因为反过来会造成不可逆损坏。备份顺序搞错是**永久的**（字节从未被捕获），恢复顺序搞错是**暂时的**。两者不是一个量级的问题。

**而"两个顺序都安全"本身依赖一条性质：GC 是元数据驱动的。** `CollectableBlobs` 的候选集是 `FROM blobs WHERE refcount = 0 AND collect_after <= now()`，遍历的是**行**，从不扫描目录。所以"字节已恢复、还没有对应行"的文件对 GC 完全不可见。如果哪天收集器改成扫目录，"先恢复 Blob"就会变成亲手删掉整个恢复现场。

**六个测试，全部逐个跑过变异验证**：

- `TestUnreferencedBlobFileSurvivesCollection` —— 磁盘上有字节、库里没有行的文件不在候选集里；
- `TestPGCollectorIgnoresBlobFilesWithNoRow` —— 同样的事实在真实 PostgreSQL 上再测一次（内存库与 PG 的查询是两份实现）；
- `TestPGCollectorHonoursTheGracePeriod` —— 宽限期没过的不可回收，过了之后可回收；
- `TestPGCollectorSkipsReferencedBlobs` —— 见下，这个我写错了两版；
- `TestGCIsMetadataDriven` —— 源码级：候选集必须 `FROM blobs b`，且不得出现 `ReadDir`/`WalkDir`；
- `TestRestoreOrderAdviceIsNotOverstated` —— 校验文档不再夸大，并要求正确的表述在场。

**`TestPGCollectorSkipsReferencedBlobs` 我写错了两次，两个错误都值得记。**

第一版用一次普通上传，然后删掉查询里的 node join —— **测试照样通过**。原因是 `CreateFile` 会递增 `refcount`，所以真正拦住它的是 `refcount = 0` 那条，node join 只是冗余。**一个删掉被测守卫仍然通过的测试，测的不是那个守卫。**

第二版把 `refcount` 强制改成 0，但 node 仍然指向这个 blob——这个状态应用本身永远不会产生，**而这正是 refcount bug 会产生的状态**。现在删掉 node join，测试立刻红并说明"`refcount` 单独不够"。同时补了一个对照：把 node 行删掉之后，同一个 blob 必须变得可回收——否则"收集器什么都不返回"也能让测试通过。


### §11.2 第 4 项：备份窗口内的并发写入（第三十八轮）

需求原文："验证备份时间点附近的**并发写入不会产生丢失引用**"。这一项之前完全没有覆盖——文档里既没写备份的**顺序**，测试里也没构造过"窗口内发生写入"的情形。

备份是两个时刻的两份产物（数据库 dump 与 blob 目录副本），无法原子取得，中间总有窗口。窗口内的写入会不会导致恢复后引用不存在的字节，取决于**顺序**——而哪个顺序安全，取决于写入路径的一条不变量。

**我第一版的结论是反的。** 我先断定"先备份 Blob，再 pg_dump"更安全，理由听起来很顺："先取到的产物更旧，更旧就更安全"。写测试时才去确认那条不变量，发现是**内容先落盘、行后写入**（`files.go` 里 `Content.Put` 在 `publishFile` 之前）。代进去一算，安全顺序恰好相反：

| 顺序 | 窗口内的写入 | 恢复后 |
| --- | --- | --- |
| **先 pg_dump，再备份 Blob** | 字节已在副本、行不在 dump | 库里没这条记录，副本里多个没人引用的 blob。**孤儿，可回收** |
| 先备份 Blob，再 pg_dump | 行在 dump、字节在副本读完后落盘 | 元数据指向不存在的内容，**每次读取都失败**。恢复看起来成功，直到有人打开那个文件 |

孤儿 blob 由 GC 回收；悬空引用没有任何机制能修。所以正确顺序是 **先 dump，再备份 blob**。

**方向搞反的文档比没有文档更糟**：它读起来像建议，照着做的运维会精确制造出那段文字声称要防止的故障。我差点就把这段反向建议写进 `restore-drill.md` 了。

**四个测试固定住这件事**：

1. `TestDumpBeforeBlobsLosesNoReference` —— 安全顺序下，dump 里的每条记录都在副本里有字节（并断言窗口内的写入确实只留下孤儿）；
2. `TestBlobsBeforeDumpCanLoseAReference` —— 显式构造坏状态并断言它**确实坏**（只测安全顺序的话，两种顺序没差别时它也会通过）；
3. `TestBlobIsWrittenBeforeItsRow` —— 读源码校验那条不变量本身。在 `Content.Put` 之前插入一个 `publishFile` 调用，它立刻红；
4. `TestBackupDocsStateTheRightOrder` —— 校验文档写的是正确顺序，且反向指令没有以"指令"形态残留（表格里解释"为什么错"是允许的，所以只检查非表格行）。

把这两条不变量都放进测试，是因为**它们中任何一个变了，另一个的正确取值就翻转**。只测结论而不测推导依据，等于把结论冻在时间里。


### 大库耗时：三个规模点，以及一次数量级的算术错误（第三十七轮）

遗留清单里写着"78 KiB 用时约 1.2 秒，不能外推到 40 TiB"。这句话是对的，但它把问题停在了错的地方——真正无法回答的不是"40 TiB 要多久"（这里没人答得出），而是"**代价是随数据量平滑增长，还是过了某个阈值就爆掉**"。后者用两个跨数量级的测量就能回答。

写了 `tools/seedscale`（走真实 store，因此审计哈希链是真的），在 130 KiB / 1.2 MiB / 5.5 MiB 三个点上跑 `newcloud drill`：

| dump | 行数 | 耗时 | 秒/MiB |
| --- | --- | --- | --- |
| 130 KiB | 1 171 | 1.19 s | 9.38 |
| 1.2 MiB | 22 301 | 8.32 s | 6.94 |
| 5.5 MiB | 110 401 | 40.78 s | 7.41 |

三点全部 PASS，审计链各自 1 000 / 20 000 / 100 000 事件且 `verified=true`。**秒/MiB 在 43 倍的体积范围内稳定**，没有上升趋势——拐点被排除了，而拐点正是单点测量排除不掉、外推时最怕的东西。

**然后是这一轮真正的教训。** 我写"线性外推给 40 TiB 约 82 小时"。实际算术是 **约 92 000 小时**——差了**三个数量级**。而这不只是个笔误：82 小时接近 24 小时的 RTO 目标，读起来像"差不多达标"；92 000 小时则说明**线性外推这条路本身就是错的**，真实的 40 TiB 部署必然是另一条路径（并行 restore，或者大的只是 blob 字节而不是元数据）。同一个段落，数字错则结论反。

修好之后加了 `TestDrillScaleMeasurementsAreConsistent`：从文档表格里的**实测值**重算外推，与文档**声称的**数字比对，差 20% 以上就失败。把我原来那个错误数字放回去，它报 `a factor of 985`。散文里的算术靠人眼审不出来——这一轮之前我一直是在"读一遍觉得对"。

**顺带**：`seedscale` 第一版只写元数据不写 blob 内容，drill **立刻 FAIL**：`the restored copy cannot serve it: blob: not found`，100 个文件 0 个可读。一个"看起来有数据"的数据库和一个真的能服务的数据库，差别只有 drill 会告诉你。


### §3.3 break-glass 的"使用后告警"（第三十六轮）

遗留清单里 `§3.3 break-glass 的运维面` 那条写着"无 UI"。**先去看需求，再动手**——结果这一条的判断是错的，而且错在两个方向：

需求原文是"强认证、凭据离线保管、**使用后告警**、强制审计、定期恢复演练"。其中"使用后告警"**根本没有实现**：登录成功会写一条 `auth.breakglass.used` 审计记录，被拒绝会写 `auth.breakglass.denied`——但**没有任何东西能对审计表告警**，因为抓取器读的是 `/metrics`，不是数据库。那个要求当时的满足方式是"有人去看日志"，这不是告警。

而"无 UI"本身是**不该做**的：break-glass 是 IdP 挂掉时的恢复入口，给它一个网页界面只会扩大暴露面，与需求里"对公开网络暴露制定独立决策"相悖。所以这一条诊断错了。

**做对的事**：给 registry 加一个按 kind 标注的计数器 `newcloud_auth_events_total`，在 break-glass 的成功与失败两个点各自记录，再配两条规则：

| 规则 | 表达式 | 严重度 |
| --- | --- | --- |
| `NewCloudBreakglassUsed` | `increase(...{kind="breakglass_ok"}[5m]) > 0` | critical —— 首次使用就告警，"没人声明的使用"正是这条规则存在的理由 |
| `NewCloudBreakglassAttemptsFailing` | `increase(...{kind="breakglass_denied"}[15m]) > 5` | warning —— 密码输错很常见，成批才值得看 |

两个 kind 必须分开：合在一起会让"有人被用了"这条永远无法排期，因为"有人在猜"会经常触发。

**测试发现了一个真问题**：`TestAlertRulesNameOnlyMetricsThatExist`（第三十三轮写的守卫）立刻红了，报"规则选了 `kind="breakglass_ok"` 但服务器从来没发出这个值"。它是对的——带标签的计数器只在该标签被观测过之后才存在于暴露里，而我的测试 fixture 从没触发过 break-glass。守卫和以前的自己打了一架，然后我补了 fixture。**这正是那个守卫该做的事。**

`promtool check rules`：12 条规则通过。runbook 新增 `## breakglass` 章节，包含"who/from where"的 SQL、以及"检查它**没有**做什么"——break-glass 刻意不是超级用户，如果审计显示它读取了用户内容，那是权限 bug 而不是凭据事件。


### 遗留清单自身的失效（第三十四/三十五轮）

这一轮开场跑基线，发现**四项测试红了**。查下去不是代码问题：Docker Desktop 整个没在运行，PostgreSQL 容器连不上。启动引擎、拉起源码后 638 个测试全绿。记一笔是因为"基线红"和"代码坏了"是两件事，混为一谈会去改没有坏的东西。

**真正的问题是遗留清单本身。** 我逐行核对时发现表格里有两条**互相矛盾**的记录：

- 一条写着 `§4.1 ACL 的界面 | API 完整，**没有 UI** | 前端未接入`；
- 另一条（第三十轮加的）写着"ACL 与审批都已有界面"，还描述了界面长什么样。

两条都是当时如实写的，第一条只是**活得比事实久**。同样失效的还有 `§11.1` 说"没有随附的告警规则"——第三十三轮刚加完 `deploy/monitoring/alerts.yml`。

**一份"未完成"栏已经过期的状态文档是有害的**：它会让下一个人去重建已经能用的东西，或者干脆不再相信清单的其余部分。

**两个结构性测试防止再犯**：

1. `TestGapTableDoesNotClaimClosedGapsAreOpen` —— 把"文档声称不存在、而实际存在"的断言列成一张小表（ACL 界面、审批界面、告警规则、restic 脚本），一旦对应文件存在而文档还在说没有，就失败并指出该改哪一行。把原来那条 stale 记录放回去，它立刻红：`the gap table still says "前端未接入" ... but web/src/components/FolderAccess.tsx exists`。
2. `TestGapTableRowsAreWellFormed` —— 校验表格形状：三列、每条有现状和影响、**需求名不得重复**。重复正是"两条记录互相矛盾"的产生方式；手工按行拼接插入已经出过两次错。

**顺手关掉一个确有其事的缺口**：SHARE-06 的影响栏一直写着"移动文件后链接继续有效，但未单独测试"。未测试的设计决定就是下一次重构会破坏、而没人察觉的那一个。补了三个测试：重命名后链接仍有效且报告新名字、移动后仍有效**且下载仍可用**、重命名后再进回收站仍然拒绝。用"假装缓存了旧名字"的变异验证过会变红：`the link reports cached-old-name, want the new name`。


### 告警规则、运行手册，与三个"永远为 0"的指标（第三十三轮）

上一轮把 `/metrics` 端到端跑通之后，剩下的缺口是"没有随附的抓取配置或告警规则"。这一轮补上 `deploy/monitoring/`（`alerts.yml`、`prometheus.example.yml`、`runbook.md`），装了 promtool 校验——**然后在写规则的过程中发现三个指标从来没被生产代码写过**。

`ObserveUpload`、`ObserveThumbnail`、`ObserveBlobGC` 只在 `internal/metrics/metrics_test.go` 里被调用过。也就是说这三个家族的序列**格式完全正确、永远为 0**：11 个单元测试全绿，我照着它们写的告警规则永远不会触发。

一个没人写入的指标，和一个健康的指标**从外部看完全一样**——而且比缺失的指标更糟，因为它看起来像覆盖。

**补上接线**，每处都选在事件真正发生的那一个点：`publishFile` 里用 `defer`（简单上传与分块完成两条路径共用它，所以记一次而不是每个调用者各记一次，也就不会漏）；`writeUploadError` 里按错误类型分类（409 是"续传"，不是失败，混进失败率会让正常客户端看起来坏了）；缩略图在每个返回点分类（"不是图片"不是失败——一整个 PDF 目录否则看起来像渲染器坏了）；GC 在 `sweepBlobs` 的调用点，成功和失败都记（只记成功会让错误计数器永久为 0，那条告警就成了死的）。

**新增 `TestEveryMetricFamilyIsFedByProductionCode`**：从 `metrics.go` 自己解析出所有变更方法，再检查 `internal/metrics` 之外、`_test.go` 之外的代码是否有调用点。删掉 `ObserveThumbnail` 的接线立刻红：`1 metric method(s) are never called outside the metrics package and its tests: ObserveThumbnail`。

**新增 `TestAlertRulesNameOnlyMetricsThatExist`**：从活着的 harness 抓一份真实暴露文本，然后校验三件事——规则里的每个指标名都在暴露里、每个 label 选择器的值都真实存在、每个 `runbook.md#anchor` 都对应一个真实章节。三种都实测能让它变红。

**一个过程上的自我纠正**：这个测试第一次红在 `status_class="5xx"`，因为我的 harness 根本没产生过 5xx ——**测试是对的，缺的是素材**。补上 `ObserveRequest(200/404/500)` 才通过。promtool 只校验规则**语法**，这三个问题一个都盖不到。

### /metrics 端到端接线（第三十二轮）

"没有指标面板"这句我在遗留清单里挂了很多轮，含义模糊。打开看之后，实际情况是：**指标功能是完整实现的**——`internal/metrics` 有 11 个测试，覆盖暴露格式、重复序列、状态码归类、DAV 冲突、上传结果、队列深度、并发安全，甚至"边写边抓"；`main.go` 也真的把 registry 接进了 `server.Deps`。

缺的是**从头到尾没有任何东西通过 HTTP 抓过 `/metrics`**：测试 harness 从来不传 registry。于是三条路径从未被执行过——路由注册、喂数据的那层中间件、以及"registry 为 nil 时没有这个路由"的分支。一个定义好但没挂上的抓取端点，从包自己的测试看和一个正常工作的端点**一模一样**。

补上 `newHarnessTuned`（可在构建前改 `server.Deps`）与四个 HTTP 层测试后，立刻发现两处我原本会写错的地方：

**一、`/metrics` 在没有 registry 时返回 200，不是 404。** 我第一版断言 404，结果红了。原因是 `r.NotFound(s.ui.ServeHTTP)`——单页应用把未注册路径交给外壳，返回 `index.html`。断言"不是指标内容"才是对的：抓取器指向一个没开指标的部署时，必须**明显失败**（content-type 是 HTML），而不是每次静默记一个解析错误或者记成零值。

**二、"未知路径 404"这个测试素材本身是错的。** 我原本用 `/api/definitely-not-a-route` 制造 4xx，它同样落到外壳上返回 200，所以 4xx 计数永远为 0。改用 `/api/files/download?path=/nope/absent.txt` 才真的产生 404。

**三、我的断言一度弱到接不住变异。** 第一版只检查抓取文本里含有 `status_class="4xx"`；把中间件改成"每个请求都记成 method=MUTATED"**依然通过**。改成"必须存在 method="GET"、status_class 为 2xx/4xx 且值大于 0 的序列"之后，同一个变异立刻红：`no non-zero newcloud_http_requests_total{method="GET",status_class="2xx"}`。

这一轮真正学到的：**一个"断言通过了"的结果，只有在对应的变异会让它失败时才有意义。** 我连着两次写出没有牙的断言（第三次想起要跑变异），而真正的接线缺失就是这么溜过去的——11 个单元测试全绿，端点是死的。

### 账号审批界面（第三十一轮）

与上一轮的 ACL 缺口同型：**JIT 供应策略会把账号创建成 `pending`，而 `pending` 账号登录不了**。审批的 API（`GET /api/admin/users?status=pending`、`POST /api/admin/users/{id}/status`）早就做完并有 8 个测试，但界面上没有任何地方能调到它。也就是说，这条策略**能把一个人放进一个只有 curl 才能解开的处境**。

`views/Admin.tsx`：按状态筛选（待审批/已启用/已停用/全部）、一行显示账号/角色/状态/用量、按当前状态给出可执行的动作（pending → 批准或停用；disabled → 启用；active → 停用）。导航项**只对管理员追加**——一个永远 404 的入口比没有入口更糟。但隐藏只是呈现：服务端对非管理员返回 404，`TestAdminEndpointsAreInvisibleToNonAdmins` 管这一点。

**两个测试补的是"约定"而不是"形状"**。类型检查能保证我按 `openapi.yaml` 发请求，但保证不了 handler 按同一份规格读——两边都对着同一份文档却各自理解不同，是这类界面最常见的失效。所以 `adminui_test.go` 发出**与面板完全相同的请求**并断言服务端听得懂：

- `?status=pending` 必须只回待审批的那一个（不断言"总数够多"——那对忽略参数的实现也会通过）；
- `{"status":"active"}` 必须真的改变状态（只断言 200 的话，一个什么都没干的 handler 也能过）。

**反向验证**：把 handler 里的 `status := r.URL.Query().Get("status")` 改成忽略参数，第一个测试立刻红并报 `got 4`。

**一个配色上的自我纠正**：我先用硬编码的浅色写 `pending` 胶囊（`#8a5a00` on `#fdf6e7`），在任何深色主题上都会看不见。改用 `--warn` / `--warn-wash` 令牌后，`pnpm contrast` 在 light/dark/hc 三个主题下都通过（5.43 / 8.09 / 9.19）。这正是只检查浅色主题时会漏掉的那类问题。

**i18n 校验在这一轮发挥了作用**：新视图一次缺 15 个键，校验逐个列了出来，而不是在界面上渲染成 `admin.title`。

### 第四层 ACL 的前端入口 + i18n 校验（第三十轮）

从很早就在"遗留缺口"里写着一句"没有 ACL 管理界面"。这一轮打开前端看，发现缺口比那句话更具体：**API、授权器、`/api/shared` 视图全都做完且测试通过，但浏览器里没有任何地方能创建一条授权**。也就是说——把文件夹共享给同事这件事，只能 curl。

一个有能力却没有入口的功能，等于大多数部署里不存在的能力。

**补上的东西**：`FolderAccess` 面板（挂在 Access 页），可以按路径载入某个文件夹的直接授权、逐条吊销、以及新建授权（账号 + 权限复选框）。权限按存储词汇表列出（`read/write/create/overwrite/delete/share/manage_permissions`），`delete` 单独一项而不是并进 `write`——给编辑权限很少等于"顺便可以删掉"。授权只对当前节点按路径寻址，这与服务端模型一致；在界面上摊平成"全账号授权列表"会凭空造出一个 API 没有的视图。

**类型检查抓到一个我写错的契约**：`revokeGrant` 漏了 `path` 查询参数。服务端要求 id **和** path 同时在（id 不可猜，但误操作是可发生的），我按 id 写的第一版直接被 tsc 拒绝。这正是 `types.ts` 头注释说的——类型来自 `openapi.yaml`，手写一边就会漂。

**顺带发现 i18n 完全没有校验**，并写了 `web/scripts/i18n-check.mjs`：

1. 英文表里的每个键必须在其他语言里存在，**但**该语言复数规则永远选不到的形态除外。中文只有 `other` 一个类别（`Intl.PluralRules("zh-Hans").select(1)` 是 `"other"`，我实测确认），所以 5 个 `_one` 键在中文里缺失是**正确的**。这个豁免由语言的复数规则**计算**得出，不是手工列名单——手工豁免名单正是漏翻被赦免的方式。
2. 代码里每个 `t("...")` 引用的键必须存在。拼错不会让编译器报错，它会**把键名本身渲染出来**，按钮上出现 `acl.grant`。

**两种失效都实测能抓到**：删掉 `acl.granted` 的中文行 → `zh-Hans is missing "acl.granted"`；把调用改成 `t("acl.grantedTypo")` → `FolderAccess.tsx calls t("acl.grantedTypo") but en has no such key`。已接进 `pnpm i18n` 与 CI。

**一次自我纠正**：第一次跑这两个变异时，PowerShell 的相对路径没解析到文件，两个变异都没写入，于是"检查通过"了——但那两次运行**什么也没测**。改用绝对路径后立刻红。这与第二十五轮那次（改了三处没命中真实分支）是同一类错误：**验证没有生效时，先确认变异是否真的落盘，而不是相信绿色。**

### INV-08 补完 — restic 快照恢复实测（第二十九轮）

上一轮的经验是"本地跑不了通常只是没装"。这一轮把它用在 restic 上：`scoop install restic`（0.19.1）之后，从第八轮起一直挂着的"restic 快照恢复未验证"终于可以实测了。

**跑通了完整备份链**：`blob-backup.sh` 的 backup → forget（7 日/4 周/12 月）→ prune → `restic check --read-data-subset=1/20`，最后一步报 "no errors were found"。

**但第一次运行就崩了，而且是真 bug：**

```
restic: Device IDs are not supported on Windows
panic: ...
```

`--one-file-system` 在 Windows 上**直接 panic**。这个 flag 在 Linux 上是对的（防止备份钻进 blob 根下的挂载点），但脚本无条件带上它，于是任何 Windows 运维机器上这条备份**在保存一个字节之前就退出了**。一个跑不起来的备份脚本保护不了任何东西。修法：按 `uname` 条件添加，Linux/Darwin 保留，其他平台省略。

**第二个问题在文档里，不在代码里。** 我按文档写的恢复命令跑：

```bash
restic restore latest --target /var/lib/newcloud --include /var/lib/newcloud/blobs
```

结果是 0 个文件——因为快照里的路径是 `/C/Users/.../blobs`（备份时的**绝对路径**），`--include` 匹配不上。改用 `--target` 指向父目录也不行：`restic restore` 会把绝对路径**在 target 下重建**，得到 `target/var/lib/newcloud/blobs` 而不是 `target/blobs`。`--include` 只筛选**恢复什么**，不改变**落在哪里**。

**文档里的命令是错的，而且错得很危险**：按它恢复会得到一个空目录或一层多余嵌套，运维会以为恢复成功了。已改成两步——先恢复到 scratch，再 `cp -a` 搬运内容——并**实测通过**：

```
ab\cd\abcd1234   200 bytes   A48742653620D8AE...
ab\cd\abcd5678    50 bytes   21AB8B865E56EFD6...
```

两个 blob 的 SHA-256 与删除前的原文件**逐字节一致**，目录布局也正确（`ab/cd/<hash>`，不是 `blobs/blobs`）。

这一轮值得记的不是"又验证了一项"，而是**两处问题都只有真跑才会暴露**：一个 flag 的平台差异，一份文档里想当然的路径语义。十五轮来我一直把这两条写成"未验证"，而它们真正需要的只是安装一个二进制。
### CI 等价环境本地化（第二十八轮）

上一轮我写下两项"本地无法验证"：`go test -race`（缺 C 编译器）和 S3 对真实端点（缺 moto 服务）。**这两项都不是环境的固有限制，是我没去装。** 两者装上之后都跑通了。

**`-race`**：装了 mingw（`scoop install mingw`），`CGO_ENABLED=1` 下全量 `go test -race ./...` 通过，22 个包、零 DATA RACE。

但"没有检测到竞态"本身不是证据——除非能证明检测器真的会响。所以我在 `RecordDownload` 里故意加了一个裸的 `raceProbe++`，用并发下载测试跑：

```
WARNING: DATA RACE
--- FAIL: TestConcurrentDownloadsRespectLimit
```

**检测器确实在工作**，因此"全量零竞态"是一个有内容的结论，而不是"运行了一个什么也没检查的东西"。这一点值得单独做，因为 CI 的注释说竞态检测存在的理由是"分享下载计数与会话撤销是两处在生产中会静默的竞态"——而那些代码我在第五、七轮改过，此前**从未在竞态检测器下跑过**。

**S3 真实端点**：装了 `moto[server]`，起了 S3 兼容服务，建桶，然后按 CI 的环境变量跑：

```
TEST_S3_ENDPOINT=127.0.0.1:19000 ... go test -race ./...
```

结果：layout 合规用例 16/16、blob 契约 7/7、`TestOpenStorageBuildsS3` 全部通过。这说明第九轮的进程内假实现**没有掩盖协议层问题**——假实现验证后端自身逻辑，moto 验证 minio-go 与端点确实谈得拢，两者互补。

**最终：CI 的完整配置（`-race` + PostgreSQL + 真实 S3 端点）本地跑通，630 个测试、零跳过、零竞态。**

这条经验比两个具体修复更重要：**"本地跑不了"通常是"我还没装"，而不是"做不到"。** 上一轮我把两件十分钟能装好的事写成了环境限制。
### 版本控制与 CI 对齐（第二十七轮下半）

27 轮的工作一直是**未提交**状态。第十四轮已经用一条 `git checkout` 销毁过未提交的改动（靠测试密集才恢复），那是一次警告而不是意外。本轮把这批工作提交了：216 个文件、25,517 行新增。

提交前做了三项检查，而不是直接 `git add -A`：

- **排除 `design-studio/`**：那是与本次工作无关的设计稿目录（含图片），不属于这个提交；
- **`.pnpm-store/` 加入 `.gitignore`**：包管理器缓存不是项目源码；
- **确认没有二进制、日志或构建产物进暂存区**（staged 总计 2.2 MB，全是源码）。

**并对照 CI 检查了自己的验证方式**——这里发现一个真实差异：`ci.yml` 跑的是 `gofmt -l .`（**整棵树**），而我 27 轮一直只用 `gofmt -l cmd internal tools`。整树检查当前是干净的，但我的本地检查**范围比 CI 窄**，也就是说我一直没有验证 CI 会验证的那件事。现在已知这一点。

CI 的其余步骤逐个核对：

| CI 步骤 | 本地结果 |
| --- | --- |
| `gofmt -l .`（整树） | 通过 |
| `go vet ./...` | 通过 |
| 示例配置可解析 | 通过 |
| web `typecheck` | 通过（exit 0） |
| web `contrast`（WCAG AA） | 通过（exit 0） |
| web `gen:api:check`（生成类型未漂移） | 通过，无 diff |
| `go test -race` | **无法运行**：`-race` 需要 cgo，而这台机器没有 C 编译器（`gcc not found`） |
| S3 合规用例对真实端点 | **未运行**：本轮用进程内假实现（第九轮的成果），真实 S3 端点需要 CI 的 moto |

最后两项必须写成"未验证"而不是"通过"。特别是 `-race`：CI 的注释说它存在的原因是"分享下载计数与会话撤销是两处数据竞争在生产中会静默"——而那两处正是我在第五轮、第七轮改过的代码。**本地没有验证过它们**，这只能靠 CI。

### SHARE-06 — 文件被删后链接的行为（第二十七轮）

需求要求"链接吊销与文件删除/移动/权限变化的影响**明确**，覆盖缓存和衍生资源"。检查后发现一个**静默失败**：

`handleSharePage` 里把节点查询的错误**丢弃**了：

```go
_ = s.deps.Meta.WithTx(...)   // 错误在这里被扔掉
```

文件进回收站后，`tx.Node` 返回 `not found`，但那个错误没人看。响应于是变成：

```json
{"kind":"link","label":"","root_name":"","root_kind":"","children":[],"can_read":true}
```

**200，空列表，什么都没说。** 收件人看到的是一片空白，而空白和"这个文件夹本来就是空的"无法区分——比一个错误更糟，因为没有任何东西可以向任何人报告。

**修复**：处理错误，并把 `not found` 映射到新的 `store.ErrShareGone`。

**但这里有个设计问题我一开始想反了。** 我原本打算让"文件已删除"返回一个区别于"链接被吊销"的消息——听起来对用户更友好。可 `renderShareError` 的注释说明了它为什么把**所有**原因折叠成同一句"this link is not available"：

> 区分"已过期"、"已吊销"、"从未存在"会把端点变成一个令牌预言机。

也就是说，**给"文件已删除"一个独特消息，等于让任何持有候选令牌的人能测试它是否曾经有效**。这是安全漏洞，不是友好。所以 `ErrShareGone` 是**服务端内部**的哨兵：它让服务器停止静默返回空列表、让日志能记录真实原因，而收件人看到的仍然和其他失败完全一致。

**两个测试**：一个断言文件被回收后链接不再服务（404 而非 200 空列表）；另一个断言"文件已删除 / 已吊销 / 从未存在"三种情况的**响应体逐字节相同**——少见的必须断言"不可区分"的测试。

**反向验证**：把错误重新丢弃，第一个测试立刻红并打印 `got 200 with map[can_read:true children:[] root_name:]`，正是修复前那个静默空列表。


### T-01 续 — 上传会话不再是豁免（第二十六轮）

上一轮我把三条上传会话路由**豁免**了，理由是"会话由内存 Manager 持有，store 无法构造"。这个理由只对了一半：Manager 确实持有会话，但测试可以**直接用 Manager 创建**一个归属受害者的会话——测试已经能拿到它，我只是没有把引用接到 harness 上。

**这个豁免掩盖了真实风险。** 把它补上之后立刻发现两件事：

1. **`handleUploadChunk` 在校验身份之前先解析 `Content-Range`**，所以缺头时返回 400 而不是 404。这不是漏洞，但它意味着我的第一个版本测的是**范围解析器**而不是鉴权——断言在错误的层上通过了。补上合法的 `Content-Range` 之后才真正走到所有权检查。
2. 三条路由现在都有用例，且**反向验证有效**：把 `Manager.Get` 里的 `s.Owner != owner` 判断删掉，三个用例全部变红——`write a chunk` 返回 **200**（侵入者成功写进了别人的会话），`complete` 返回 409（能推进别人的上传）。

**12 条带 id 的路由现在全部有覆盖或写了理由的豁免**，只剩两条：`DELETE /api/acl/{id}`（由既有测试覆盖）和 `DELETE /api/admin/users/{id}`（由非管理员的列表/改状态用例证明了同一道门）。

这条经验值得记住：**"测试基础设施不方便"不是豁免的理由，是要修基础设施的信号**。我用一个 `uploads *upload.Manager` 字段解决的事，上一轮却写成了"无法构造"。


### T-01 — 逐路由的越权检查（第二十五轮）

T-01 的威胁是"用户 A 猜到用户 B 的文件 ID、版本 ID、缩略图或下载链接"。这一项之前只有**一个**越权测试（`TestACLRevokeRejectsForeignGrant`），而带 id 的路由有 **12 条**。

我逐个读了 handler，**每一条都做了正确的鉴权**。但这正是问题所在：IDOR 不是某个 handler 的 bug，而是"某个人最后加的那条路由忘了检查"。一个只覆盖"我想到了的三条"的测试，会在第四条泄漏时永远通过。所以我把它写成**对路由表**的表驱动测试，而不是对 handler 的抽样。

**每个用例断言三件事，不是一件**：

1. 状态码是**拒绝**，且必须是 404 而不是 403——403 等于确认"这个 id 存在"；
2. **拒绝的响应体本身不泄漏私有元数据**——一个 404 里带上文件名，照样是泄漏；
3. 变更型路由之后，资源**没有被改动**（对方分享仍是活跃的、对方账号仍是 active）。

**另有一个元测试** `TestIDORTableCoversEveryIdRoute`：把手工维护的 12 条路由清单与"必须覆盖或显式豁免并写明理由"对齐。豁免必须是**写下来的决定**，不能是遗漏。

**验证这个测试真的有牙**：删掉 `handleShareRevoke` 里的所有权检查，测试立刻红并报 `want 404 ..., got 204`——204 意味着侵入者**成功撤销了别人的分享**。

**这一轮我犯了个"验证方式本身错"的错误，值得记下来。** 我用三种方式改 `handleSharedNode` 想制造元数据泄漏（改 `cap.Can` 分支、改 `writeStoreError` 的文案、把 `node.Name` 塞进 404），三种都没有让测试变红。我一度以为泄漏断言是假的。加了一行 `println` 才发现 handler **确实**被调用，只是拒绝发生在 `Authorize` 返回 error 的那条路径上——我改的三处都不是实际走的分支。第四次对准那一条，测试立刻红并打印 `denied: secret-plan.txt`。

教训是：**断言没被触发时，先确认代码走到哪条分支，而不是反复改代码碰运气**。三次盲改之后我才想到打日志，而日志十秒就给出了答案。


### 文档完整性 — 引用的测试必须存在（第二十四轮）

上一轮我把两个**不存在**的测试名写进了策略文档，凭印象编的。被发现后我手工核对了一遍，但这只是一次性的动作，不是保障——文档会继续被写，同样的错误会再来。

现在有了 `TestDocumentsCiteOnlyRealTests`：扫描 `docs/`、`deploy/` 与 `README.md` 里所有反引号包裹的 `Test*` 名字，与 `cmd/`、`internal/` 下所有 `_test.go` 里的 `func Test*(` 求差集，缺一个就失败。

**它立刻又抓到三处**（都在 0011 里）。其中一处是我**完全没意识到的**：第十二轮写的 `TestCreateWithoutOverwriteCannotReplace` ——那个测试当时就被我改名了（改为 `TestCreateDoesNotImplyOverwrite`），但叙述里留的是旧名字。 <!-- docIgnore: narrative naming a test that was renamed or deliberately fake -->

三处里有三处是"关于错误的叙述"（"第一版叫 X，它不存在"），这种提及是**正确的**。所以规则不是删掉它们，而是要求这类行显式标记 `docIgnore:`——排除必须是一个动作，不能是一个模式。测试也会在找不到任何测试函数、或找不到任何文档时**失败**，避免"空集合比较"让检查恒定通过。

**验证**：往 `0012` 里塞一个 `TestThisIsNotReal`，测试立刻红并打印 `TestThisIsNotReal (cited in 0012-cursor-contract.md)`。 <!-- docIgnore: narrative naming a test that was renamed or deliberately fake -->

这类检查的价值不在它现在通过，而在于**下一次有人（包括我）凭记忆写测试名时会立刻被拦住**。上一轮我评论说"文档里点名一个不存在的测试比不写更糟"——把这句话变成代码，才是这一轮真正做的事。


### ID-03 — JWKS 密钥轮换与验证失败策略（第二十三轮）

需求三项：OIDC Discovery、**JWKS 密钥轮换**、验证失败策略。前两项里，轮换一直**无法被测试**——fake IdP 只有一把固定密钥、`kid` 写死为 `"test-key"`。于是一个"只在启动时抓一次 JWKS、之后从不刷新"的客户端，对单密钥 IdP 能通过**全部**测试。"支持轮换"因此是一句没人验证过的话。

**先让轮换变得可测**：`fakeidp.RotateKey()` 生成新密钥、**把旧密钥继续发布在 JWKS 里**（真实 IdP 的做法——立刻撤掉旧密钥会让刚签发的令牌失效，是自己造成的故障），`RetireOldKeys()` 结束轮换窗口。签名时读取当前 `kid` 与密钥（同一把锁下取，避免出现"头部写的 kid 与实际签名密钥不一致"的令牌）。

**三个端到端测试**（走真实登录流程，不是单元级模拟）：

| 断言 | 结果 |
| --- | --- |
| IdP 轮换后，同一进程的下一次登录成功 | 通过（换成另一个账号登录，缓存会话无法蒙混） |
| 已登录会话在轮换后仍然有效 | 通过（会话是自己的凭据，从不问 IdP——这是 ID-07 的性质） |
| 撤掉全部旧密钥再轮换，登录仍成功（需重新抓 JWKS） | 通过 |

**验证覆盖是真的**：把签名改成用一把**未发布**的密钥，前两个轮换测试立刻红。第一次我尝试的"改变密钥但不改 kid"没有让测试失败——因为 go-oidc 在**任何**校验失败时都会重取 JWKS，不只是遇到未知 kid。这个"失败的变异"本身是有信息量的：它纠正了我对库行为的假设。

**新增 `deploy/oidc-key-rotation.md`**：这是需求要的"策略"本身——轮换的四个步骤、九种验证失败各自的结果、以及**刻意不做的事**（不 pin 密钥：那会让轮换变成双方协同重新部署；不做磁盘 JWKS 缓存；没有密钥吊销推送——撤销对**新**校验生效，已缓存密钥的进程要等到重取或重启，这是库缓存的固有性质，写出来而不是粉饰）。

**一个自己造成的错误，记下来**：文档里我先写了两个测试名 `TestOIDCNonceMismatchIsRefused`、`TestCallbackRejectsForgedState`——**它们不存在**，是我凭印象编的。真实名字是 `TestMismatchedNonceIsRejected`、`TestForgedStateRejected`。发现后把文档里**每一个**测试名都用脚本对着源码核对了一遍。文档里点名一个不存在的测试，比不写更糟：它让人以为覆盖存在。 <!-- docIgnore: narrative about a test name that was wrong, not a citation -->


### INV-07 续 — 回填 blobs.size（第二十二轮）

上一轮我写"历史行的 size 仍是 0，无法回填（真实长度只有磁盘上的对象知道）"。**这句话的后半句正是前半句的反例，而我当时没看出来。**

对象是内容寻址的，还躺在磁盘上，哈希就是它的名字，而对象知道自己的长度——一次 `Stat` 就够了。所以回填是可能的，我上一轮的结论是错的，这一轮用代码纠正它。

**`store.RepairBlobSizes`** 在 `migrate` 里运行，与上一轮的 `RepairNameKeys` 并列：

- `stat` 函数作为参数传入，而不是在 store 包里打开存储。元数据层不该知道配置了哪种后端，这样本地磁盘与 S3 走完全相同的检查（两者都实现 `blob.Store.Stat`）。
- 用 keyset 之外的全表读取（blob 行数量远小于节点行，且这是运维动作）。
- **缺失对象不中止整个回填**：单个 blob 的字节不见了是真实的一致性问题，报告它；因为一个对象缺失就让另外一千行继续错着，是更糟的选择。

实测（先上传真实文件，再把所有 size 归零模拟历史 bug）：

```
{"msg":"blob sizes reconciled","scanned":5,"fixed":4,"recovered_bytes":2936,"missing":1}
第二次运行：{"msg":"blob sizes consistent","scanned":4}
```

**并修掉一个我自己引入的误诊。** 第一版把任何 `Stat` 失败都记成 `BackendError`，于是"五行里有一行悬空"会打印出"the content store reported errors"——把一个健康的存储报成坏的，运维会去查凭据和端点，而它们都没问题。现在改成**循环结束后统计**：只有**全部**对象都 stat 失败才算后端不可达（那确实是存储的问题），单个悬空行只逐条报告。这个区分正是这个函数存在的意义，第一版却把它搞反了。

**验证方式**：把写 size 的语句改回只读（旧行为），两个测试立刻红；把"缺失即后端错误"的推断加回去，分类测试立刻红并打印 `a healthy store must not be reported as failing: blob: not found`。


### INV-08 复验 — 演练表清单自检与 blobs.size（第二十一轮）

INV-08 的演练自第八轮之后就没再跑过，而这期间 schema 变了两次（0006 账号删除、0007 name_key）。P0 不变量不重跑，等于从来没有验证过。

**重跑之前先补了一个漏洞。** 演练的表清单是**手工维护**的——这是刻意的（"每张表都匹配"只有在集合事先确定时才有意义），但手写清单会**腐烂**：加了一张表、没人更新演练，那张表就永久地从恢复校验里豁免。`drill.go` 的注释甚至写着"`TestDrillCoversEveryTable` 会检查这个清单"——**那个测试根本不存在**。

现在它存在了，并且立刻抓到两张漏掉的表：

```
the drill does not verify 2 table(s): [public.group_role_map public.name_conflict_renames]
```

`name_conflict_renames` 是上一轮我自己加的。可见注释里的承诺靠不住，只有代码靠得住。

**然后演练跑出了一个真 bug。** 第一次重跑 **FAIL**：

```
blobs  14 files, 12 verified
VERDICT: FAIL
  - mine-report.txt: the restored copy cannot serve it: blob: not found
```

查下去不是备份的问题，是 `blobs` 表**每一行 size 都是 0**，而对应的 `nodes` 行声称 512/609/706 字节：

```sql
-- 修复前
       name      | node_size | blob_size
 mine-report.txt |         3 |         0
 file0.txt       |       512 |         0
```

原因：`BlobIDFor` 的 INSERT 把 size 硬编码成 `0`，真实长度被丢弃——而它**从来没有别的参数可以传**。没有用户可见的东西坏掉，因为节点行自己带着大小。只有两个消费者受影响：GC 的回收字节统计（`BlobRef.Size`），以及运维读这张表理解磁盘占用时。**两者被告知每个 blob 都是空的。**

这正是"演练"存在的理由：不重跑就发现不了。修复后：

```
 size | count
  512 |     1
  609 |     1
  706 |     1
  803 |     1
```

**修复后的演练通过：**

```
dump size        78.8 KiB
tables compared  22        ← 从 20 增加到 22
rows compared    57
audit chain      32 events, verified=true
blobs            12 files, 12 verified, 7.7 KiB
elapsed          1.002s (target 1h)
VERDICT: PASS
```

**顺带修掉一个测试卫生问题（第二次了）**：`TestBlobRowRecordsContentSize` 的清理注册在断言之后，于是在我故意打坏实现的那次运行里留下了一行 size=0 的 blob，恢复实现后测试仍然失败。现在 `t.Cleanup` 在第一个断言之前注册——和第十七轮同一个错误，说明这不是偶发失误，而是我写测试时的习惯性疏漏。


### T-20 — COPY/MOVE 失败不破坏源文件（第二十轮）

需求一句话，重点在**不该发生什么**：`操作不会破坏源文件，失败可以诊断与恢复`。复制失败留下完好原件只是麻烦；留下被截断的原件、或半填充却**看起来完整**的目标，是数据丢失。所以测试围绕"损坏"写，而不是围绕状态码。

**这一轮找到一个真实的数据效率 bug，而且是沉默的那种。**

写 `TestCopyDoesNotDuplicateBlobOnDisk` 时我预期相同内容共用一个 blob，结果不共用。查下去发现问题不在 COPY，而在内存后端的 `snapshot()`：

```go
// 第 220 行附近
blobIDs: make(map[[32]byte]uuid.UUID, len(m.blobIDs)),   // 声明了空 map
...
// 然后逐个 map 手工拷贝 entries —— nodes、children、roots、usage、quota、…
// blobIDs 的循环从来没写过
```

于是**每个事务都从空索引开始，提交时再用它覆盖真实索引**。后果：相同内容每次得到一个**新的** blob id，内容寻址去重**静默失效**——每次上传消耗完整大小，每次复制让磁盘占用翻倍。而 `BlobCount()` 永远是 0（现在测试会发现：修复前 `count=0`，修复后 `count=1`）。

这个 bug 只在**跨事务**时才显现：单个事务内 `BlobIDFor` 是幂等的，所以任何"上传一次、读一次"的测试都会通过。

**修复**：`snapshot()` 补上 blobIDs 的逐条拷贝；`BlobIDFor` 改为识别事务上下文，写入事务的快照而不是存储本身（否则提交时会被克隆的 map 覆盖）。

**新增 conformance 用例 `BlobDedupAcrossTransactions`**，两个后端都跑，断言：同一地址跨事务返回同一 id、不同地址返回不同 id（后者防止测试靠返回常量通过）。

**T-20 本身的结果**：

| 场景 | 结果 |
| --- | --- |
| 配额不足时 COPY | 拒绝，源文件字节不变，目标不可见 |
| 配额不足时 MOVE | 源文件仍在原处（移动不是先删后写） |
| 目标父目录不存在 | 404 —— **刻意不用 RFC 的 409**：调用者看不见的目标必须与不存在无法区分，否则该端点变成探测他人目录名的工具 |
| 目录复制遇到不可读的子项 | 在第一处失败，而不是静默跳过（"成功的"残缺复制比报错更糟） |

全部通过，且每条都反向验证过（移除修复后 `TestCopyDoesNotDuplicateBlobOnDisk` 立刻红）。


### T-19 — 游标一致性契约（第十九轮）

需求要的是"重复/遗漏行为**符合游标一致性契约**"——也就是说，行为必须是**写明的**契约，而不是碰巧如此。分页实现本身早已是游标式的（不是 offset），但契约从未写下来，而且只测了「插入」一种并发变更。

**先确认原有设计是对的**：`ChildrenAfter` 用 `name > cursor` 而不是 offset。这个名字排序上的 offset 会在两次请求之间有人增删兄弟节点时**静默地重复和漏掉行**——正是 T-19 描述的那个失败。

**新增 `docs/0012-cursor-contract.md`**，把契约写成表格。核心保证是**任何行都不会被返回两次**，而为此付出的代价是一处诚实的限制：

| 事件 | 保证 |
| --- | --- |
| 插入到游标**之后** | 本轮稍后能看到 |
| 插入到游标**之前** | 本轮看不到，下次完整列表可见；**不会重复** |
| 删除已见行 / 未见行 | 不丢东西 |
| 重命名未见的行到游标**之后** | 本轮稍后能看到 |
| **重命名未见的行到游标之前** | **本轮看不到**（仍存在，下次列表可见） |
| 任何情况 | **绝不返回同一行两次** |

最后一行值得要。重复会让客户端**做两次**——重复删除、重复上传、重复处理同一次分享下载。而"延迟到下次列表才看到"只是延迟。

**这个限制无法用 offset 修掉**（那是把罕见的漏行换成频繁的重复）。它能用快照隔离（`WHERE modified_at < $txn_start`）修，但那是一次 schema 与索引变更，本轮不做。所以它被写进文档、被测试钉住：`RenameAcrossCursorIsTheDocumentedLimitation` 在行为改变时会**故意失败**，并提示同步更新契约文档——契约不能与代码漂移。

**并验证了"游标不携带权限"这个安全声明。** 文档里写"每页都对目标目录重新鉴权"，那就得证明它：`TestCursorDoesNotOutlivePermission` 撤销授权后，用一个陈旧游标请求下一页必须失败而不是继续吐行。

**验证方式**：把 SQL 的 `name > $2` 改成 `name >= $2`（游标最容易犯的 off-by-one），四个用例立刻红，报出 `[a c c d d e e e e ...]` 这种边界行重复——说明"不重复"这条断言真的在检查东西。


### T-16 — 审计故障时的 fail-closed 策略（第十八轮）

需求要的是"遵循**事先约定**的 fail-open/fail-closed 策略"。之前的状态：代码事实上是 fail-closed（审计写不进就回滚），但**没有任何地方写明这是策略**，也没有一个测试证明它。磁盘写满、审计表不可用这些真实场景一次都没被演练过。

**策略：fail-closed。写不进审计的操作不发生。**

fail-open 很有诱惑力——审计故障时服务继续可用，听起来对用户更友好。但对这个平台是错的：

- 审计是**安全控制**（INV-09），不是遥测。它在压力下静默停止记录，比没有日志更糟，因为有人会拿它做判断；
- fail-open 让**"把磁盘写满"成为最便宜的攻击**。攻击者耗尽磁盘后所有操作都不留痕，而"日志里没有"与"什么都没发生"无法区分；
- 更糟的是：哈希链**依然会校验通过**。已写入的事件链接完整，缺失的事件不留任何痕迹——这是防篡改日志最不该有的性质。

**明确保留的例外：读操作继续可用。** 列表、下载、元数据、DAV GET/PROPFIND、审计校验、健康检查都不受影响。运维在排查"为什么写入失败"时需要读数据和读日志；把审计故障升级成全站故障，等于收走了诊断工具。这一点由 `TestReadsSurviveAnAuditOutage` 与 `TestReadsWorkDuringAuditOutage` 各自钉住。

**客户端看到什么**（新增 `store.ErrAuditUnavailable` 哨兵）：

```
HTTP/1.1 503 Service Unavailable
{"error":"audit_unavailable",
 "message":"the audit log is unavailable, so the change was refused and nothing was modified"}
```

503 而不是 500：请求本身没问题、数据也没问题，**是记录器停了**，恢复后重试即可成功。这个区分在运维上是"去查数据"和"去查审计表与磁盘"的差别。底层原因用双 `%w` 保留在错误链里（例如 `no space left on device`），日志里能看到真正的原因而不是只有"审计不可用"。

**新增 `deploy/audit-failure-policy.md`**：这是需求要求的"事先约定"本身——策略、失败/保留清单、客户端契约、运维 runbook，以及**这条策略没有覆盖的东西**（审计变慢而非不可用、提交后失败、积压重放——按设计没有重放，因为被拒绝的操作从未被接受）。

**验证**（每个都反向确认过会把测试打红）：

| 断言 | 测试 | 改成 fail-open 后 |
| --- | --- | --- |
| 变更回滚 | `TestAuditFailureRollsBackTheMutation` | 红：`must fail, not commit silently` |
| 读操作不受影响 | `TestReadsSurviveAnAuditOutage` | — |
| 漏配置也大声失败 | `TestMissingAuditWriterIsAlsoFailClosed` | — |
| 客户端拿到 503 | `TestAuditOutageReturns503` | 红：`got 204` |
| 上传路径同样 fail-closed | `TestUploadIsRefusedDuringAuditOutage` | 红：`got 201` |

**顺带修掉一个真 bug**：内存后端的 `snapshot()` 没有带上 `auditFault`，于是事务里的审计写入会绕过注入的故障——`WithTx` 拿到的是一份干净副本。这类"配置字段没进快照"的问题只在注入型测试里暴露。


### T-12 续二 — RepairNameKeys 改为分批迭代（第十七轮）

上一轮的修复实现有两个规模问题，在测试库上看不出来，在生产库上会让它不可用：

1. **整表读进内存**：`SELECT ... FROM nodes WHERE name <> ''` 一次取完，百万级节点就是一亿字节级的常驻内存；
2. **每行两条语句**：每个过期行先查一次兄弟冲突再更新一次，百万行就是两百万次往返。

现在改成 **keyset 分页 + 每批一个事务**（每批 1000 行）。选择 keyset（`id > cursor`）而不是 `OFFSET`，是因为并发插入会让 `OFFSET` 分页**跳过或重复**行——这正是修复任务最不能出的错。游标在**每一行**上推进，包括已经正确的行，否则一整页都正确的批次会原地打转。

实测跨页边界（1200 条过期行，页大小 1000）：

```
{"msg":"name keys reconciled","scanned":1206,"corrected":1200,"skipped":1}
elapsed: 3381ms
剩余过期行: 0
```

**新增 `TestRepairNameKeysCrossesPageBoundary`。** 这一点值得单独说明：其余四个测试都只种几行，**只走第一页**——一个"只修第一页就返回"的实现能通过它们全部。新测试种 1100 行（大于页大小），断言尾部也被修到。把循环改成第一页后直接返回，它立刻红：`want 1100 corrected across the page boundary, got 990 (scanned 1000)`。

990 这个数字本身就是证据：它精确等于第一页里名字需要折叠的行数，说明测试打在了分页逻辑上，而不是某个笼统的失败。

**顺带修掉一个测试卫生问题**：清理逻辑原本注册在测试末尾，于是**失败的运行永远不会清理**。我这次就撞上了——把实现改坏跑了一次，留下 1100 行；恢复实现后测试仍然失败，因为残留数据改变了断言。这类"失败会污染下一次运行"的测试会让人把真 bug 当成 flaky。现在 `t.Cleanup` 在任何断言之前注册。


### T-12 续 — name_key 的存量收敛（第十六轮）

上一轮把名称身份收敛到 `store.nameKey`，但留下一个尾巴：迁移 `0007` 的回填用的是 SQL `lower(name)`，而真正的键是一个 Go 函数（NFC + 完整折叠 + 剥离格式字符）。**ASCII 名字两者一致**，这恰恰是它容易被忽略的原因——差异只在真实客户端存进来的时候出现：

- macOS 的文件名是 **NFD**（分解形式）。Finder 上传的 `café` 到达时是 `cafe` + U+0301，`lower()` 保留组合符，于是永远匹配不上 Linux 上打出来的预组合拼写；
- `ß` 与 `ss`、连字 `ﬁ` 与 `fi`：`lower()` 不做完整折叠。

**过期的键不是外观问题。** 查找过滤的是 `name_key`，所以一个键与函数不一致的节点，**列表里看得见，按用户会打的那个拼写却找不到**。同时唯一性规则会漏掉一次本该立刻挡下的冲突。

因此在 `newcloud migrate` 里加了 `store.RepairNameKeys`：重算每个命名节点的键，只更新不一致的。选 operator 显式动作而不是又一次迁移，是因为它**应当可以反复运行**，将来键函数再改也不会有第二个迁移文件。

实测（植入用 `lower()` 写出的 `cafe`+U+0301 与 `straße`）：

```
{"msg":"name keys reconciled","scanned":4,"corrected":2,"skipped":0}

before: 63616665cc81 (cafe + U+0301)   73747261c39f65 (straße)
after:  636166c3a9 (café, NFC)         73747261737365 (strasse)

第二次运行： {"msg":"name keys already consistent","scanned":4}
```

**冲突时跳过而不是硬写。** 两个兄弟节点的名字可能折叠到同一个键——那正是唯一索引要挡的情况。修复遇到这种行时不写（写了会违反索引并中断），而是逐行报告 `skipped`，因为"两个文件谁是赢家"是 operator 的决定，不该由一个修复循环替用户选。

**仍未验证**：T-18 的实机客户端（Windows/macOS/Linux/rclone/Cyberduck）；`RepairNameKeys` 在百万级节点上的耗时未测（当前实现逐行读改写，生产库需要分批）。

**另外记一笔**：本轮进行中，整个仓库被外部重命名为 `github.com/EdisonJwa/newCloud`。我确认了 588 项测试在新名字下全绿、七次迁移可从空库应用、以及本轮的所有改动（含 `namekey_repair.go`）都完整保留，然后才继续。


### T-12 — 大小写 / Unicode 名称冲突（第十五轮）

T-12 把"利用路径穿越、符号链接及大小写/Unicode 名称冲突"列为攻击面。路径穿越早已处理，名称冲突没有——而且两个后端的行为**不一致**：

- 内存后端用 `strings.ToLower(name)` 作为兄弟索引键；
- PostgreSQL 的唯一索引是 `(parent_id, name)`，比较**原始字节**。

于是 `Readme.md` 和 `README.md` 在数据库里是两个兄弟节点，而内存后端拒绝第二个。在一个大小写不敏感的客户端（Windows、macOS 默认）里，用户只看到一个文件——另一次上传**静默覆盖**了它。

现在规则收敛到一个函数 `store.nameKey`，三处共用：兄弟唯一性、查找、以及各后端。

三步规范化，每一步都对应一个具体的失败：

| 步骤 | 防的是什么 |
| --- | --- |
| Unicode NFC | `café` 的预组合 é 与 e+组合重音是不同字节但同一文件名——不归一化会出现两个在目录列表里**看起来一样**的文件 |
| 完整大小写折叠 | 简单折叠不管 ß→ss、希腊词尾 σ、连字 ﬁ→fi，而这些正是用户无法区分的对 |
| 剥离格式字符 | 零宽连接符、软连字符、双向覆盖符渲染为**空**——一个名字可以完全由 RTL 覆盖符组成，在列表里显示为空白 |

**`0007_name_key.sql`** 新增 `name_key` 列与 `(parent_id, name_key)` 唯一索引。为什么是普通列而不是生成列：规范化是一个 Go 函数，SQL 表达不出来；用 SQL 重写它就是第二份会漂移的实现。

**迁移必须处理存量冲突，而不是失败。** 数据库里确实存在 `{Readme.md, README.md}` 这样的兄弟对——第一次尝试 `CREATE UNIQUE INDEX` 直接报 23505。把其中一个**删掉**不是迁移作者该做的决定，所以迁移保留最早的那个，把后来的重命名为 `README (conflict 2).md`，并写入 `name_conflict_renames` 表。字节、归属、版本、分享都不动。

实测（植入真实冲突后跑迁移）：

```
old_name  |        new_name
----------+------------------------
 README.md | README (conflict 2).md

          name          |        name_key
------------------------+--------------------------
 README (conflict 2).md | readme (conflict 2).md
 Readme.md              | readme.md
```

**两个文件都还在。** 这是这一轮最重要的断言。

**顺带修掉一个真 bug**：`identity_pg.go` 里有**第二个** `EnsureRoot`（事务内版本），它没设 `name_key`，于是 NOT NULL 违约把事务打成 aborted 状态，调用者看到的是 `current transaction is aborted` 而不是真正的错误。这类"错误信息指向的地方离原因很远"的问题，如果不是审计测试恰好在事务里调 EnsureRoot，会一直潜伏。

**仍未验证**：Windows/macOS 实机客户端（T-18 需要真机）；`name_key` 的 Go 规范化与 SQL `lower()` 回填在极少数组合字符上可能不同——差异会在下一次写入时自愈，但存量行不会主动纠正。


### §5.3 续 — Depth: 0 与 Depth: infinity 的区别（第十四轮）

RFC 4918 §7.4 区分两种集合锁：

- `Depth: 0`（默认）保护**成员关系**：不能增删子项，但**不**保护已存在子项的内容；
- `Depth: infinity` 覆盖整个子树，包括内容。

之前的 `lockAncestors` 完全不看 `Depth` 字段，于是 `Depth: 0` 在祖先路径上表现得和 `Depth: infinity` 一样。后果是**过度拦截**：客户端锁定一个文件夹后，再也无法编辑里面的任何文件——而 423 响应里没有任何线索说明是它自己的 `Depth: 0` 锁造成的。

更难察觉的是反向错误：DAV 的 PUT 路径只走 ancestor 检查，而新建子项（成员关系变更）本该被 `Depth: 0` 父锁挡住的地方，恰好因为同一个函数而侥幸对了；拆开后必须显式分支，否则会变成漏拦。

现在的规则，两个接口一致：

| 操作 | 性质 | 起作用的祖先锁 |
| --- | --- | --- |
| PUT 覆盖**已存在**子项 | 内容 | 仅 `Depth: infinity` |
| PUT 创建**新**子项 | 成员关系 | `Depth: 0` 即可 |
| MKCOL / 删除子项 | 成员关系 | `Depth: 0` 即可 |
| 目标节点自身的锁 | 内容 | 任意 Depth |

两者都有测试，且都做了反向验证（把 Depth 判断去掉，`Depth: 0` 的用例立刻红）。

**这一轮我犯了一个严重的操作错误，必须记下来。** 在重构 `internal/dav/resource.go` 时，我用 `git checkout` 恢复文件以清理一次失败的拼接——但那个文件有**大量未提交的改动**（PUT 的 201/204 状态码、`If-Match`/`If-None-Match` 前置条件、版本保留、锁检查）。HEAD 版本是项目初始提交，于是这些全部丢失，DAV 测试从 40 绿变成 8 红。

没有备份。我按测试契约逐条重建了 `handlePut`、`handleDelete`、`handleMkcol`，现在 40 项全绿，且每一项重建的行为都有对应用例（上面列出的 8 个）。但这是**靠测试救回来的**，不是靠流程——`git checkout <file>` 在有未提交改动时是破坏性操作，而我在这里把它当成了"撤销上一次编辑"。

教训：这个项目测试覆盖较密，所以损失可恢复；如果当时改的是测试稀薄的文件，同样的操作会无声地删掉功能。以后恢复单个文件前先 `git stash` 或先确认 `git status` 显示该文件是否为 `M`。


### §5.3 — 跨接口写入仲裁（第十三轮）

需求写得很明确："网页/API 和 WebDAV 必须进入相同的写入仲裁层"、"Lock 的语义需适用于跨接口操作；只有服务器内部可理解的锁不算完整兼容"。

之前的实现**只在一个方向上成立**，而这恰是最糟的半成品：WebDAV 检查锁，JSON API 完全不检查。浏览器可以覆盖一个 WebDAV 客户端已锁定的文件，而持锁的客户端毫不知情——它随后写入自己的副本，两边的修改静默丢失一边。**一个只有两条写路径之一尊重的锁不是锁，是对老实取锁的那个客户端的虚假承诺。**

现在每个变更型 JSON handler 都问与 DAV handler 相同的问题，走同一个 store 操作（`ActiveLock`）。两条路径无法漂移，因为判断是一个函数。

覆盖的操作：上传覆盖已存在文件、删除、重命名、移动。语义与 DAV 一致：**持锁者可以写，但必须提交 token**——否则一个陈旧的标签页会覆盖掉客户端以为受保护的文件。

**反向验证**（把守卫关掉，三个测试全部失败，且失败方式是真实的破坏而非断言噪音）：

```
--- FAIL: TestHTTPOverwriteHonoursWebDAVLock  an HTTP write against a locked file must be 423, got 201
--- FAIL: TestHTTPDeleteHonoursWebDAVLock     deleting a locked file must be 423, got 204
--- FAIL: TestHTTPRenameHonoursWebDAVLock     renaming a locked file must be 423, got 200
```

201 是"覆盖成功了"，204 是"删掉了"，200 是"改掉了"。这正是修复前会发生的事。

另有两个反向用例防止过度拦截：无锁时上传覆盖/重命名/删除都必须成功（否则上面三条在"什么都拒绝"的实现下也会通过——本项目已经踩过这个坑三次），以及**已过期的锁不得阻塞写入**，否则一个崩溃的客户端会让文件永久不可写。


### §4.2 续 — create / overwrite / rename 拆开（第十二轮）

上一轮把 `read`/`download` 拆开了，但 `PermWrite` 这个复合位还在几个具体操作上被直接检查，而 §4.2 明确把 `create`、`overwrite`、`rename` 列为独立原语。于是"你可以往这个目录加文件，但不能替换已有的"这种授权**无法表达**。

本轮改的三处：

| 操作 | 之前 | 现在 | 含义 |
| --- | --- | --- | --- |
| `mkdir` / 上传到目录 | `PermWrite` | `PermCreate` | 能在目录里新建名字 |
| 上传到**已存在**的文件 | `PermWrite` | `PermOverwrite` | 能替换别人的内容 |
| `rename` | `PermWrite` | `PermRename` | 能改名/移入目录 |

上传路径的形状值得说明：`create` 先过，然后**只有当目标已存在时**才追加 `overwrite` 检查。存在性检查是一次读，权威的守卫仍是 `WriteFile` 里的前置条件——在这两者之间新建的文件，缺少该位的调用者依然无法覆盖它，而是会在前置条件上失败。这样既不引入竞态，也不需要一次锁定。

**测试写错过一次，记下来。** 第一版 `TestCreateWithoutOverwriteCannotReplace` 走 HTTP 路径上传到别人的目录——但路径在调用者自己的命名空间里，请求在**权限检查之前**就 404 了。把 `overwrite` 检查删掉之后测试仍然是绿的。这是本项目第三次遇到"因为错误的原因而通过"。改到授权者层断言后（现名 `TestCreateDoesNotImplyOverwrite`），加回 `create → overwrite` 的蕴含会让它变红。 <!-- docIgnore: narrative about a test name that was wrong, not a citation -->

改成在授权者层断言：同一个 grant 下 `cap.Can(PermCreate)` 为真、`cap.Can(PermOverwrite)` 为假。现在把 `create → overwrite` 的蕴含加回去，测试立刻变红（`got read+list+create+overwrite`）。

**仍未拆分的 8 处 `PermWrite`** 是有意的：它们全是 WebDAV 的父目录检查与 LOCK 检查，而一次 PUT 本身就是"创建或覆盖"的二选一，用复合位是准确的。把它们拆开需要每个调用点先解析目标是否存在——那会让 PUT 多一次往返，而收益只是让一个已经能表达的组合更细。这一条写在这里而不是留在"待办"里，因为它是一个决定，不是遗漏。


### ID-07 — IdP 离线时的行为规则（第十一轮）

需求要的是"IdP 离线时的登录、新建会话、已有会话续期和撤销行为有明确规则"。之前的行为是**意外且糟糕**的：`NewProvider` 在启动时做 OIDC discovery，失败就 `return err`——**整个进程拒绝启动**。

这会把 WebDAV、应用密码认证、已签发的浏览器会话和所有文件操作一起打下来，而它们**没有一个会联系 IdP**。一个单点登录依赖被悄悄升级成了整个平台的可用性依赖。

现在的规则（`internal/auth/offline.go`），以及每条的理由：

| 场景 | 行为 | 理由 |
| --- | --- | --- |
| 启动 | 不要求 IdP 可达，并大声记录告警 | 见上 |
| 已有会话 | **继续可用** | 会话是我们自己签发的凭据，校验过程不涉及 IdP，按自己的时间表过期。IdP 故障不是把所有人登出的理由 |
| 会话续期 | 不受影响 | "续期"是滑动我们自己已签发会话的过期时间，不是回 IdP 复核 |
| 新登录 | 失败，`503 idp_unavailable`，并说明原因 | 用户需要知道"账号没问题、是身份提供方连不上" |
| 撤销 | 不受影响 | 会话是否有效是数据库问题（`sessions.revoked_at`），管理员在故障期间仍能撤销 |
| 应用密码 | 继续可用 | 本地校验的哈希，从不联系 IdP |
| 角色同步 | 推迟到下次成功登录 | 这正是 ID-06 已记录的行为 |

唯一真正无法离线工作的是**首次登录**，这不需要靠工程解决：凭据在 IdP 手里。

**懒发现**：`ProviderHolder` 在启动时尝试一次，失败后按 30 秒间隔重试。IdP 重启是常规事件，为此重启整个应用不是。限流是必要的——故障期间登录页被反复刷新，否则每次页面加载都会向已经吃力的 IdP 发一次 discovery 请求（已验证：去掉限流后测试立刻红，`ran 6` 而不是 1）。

**真实实例验证（IdP 指向未监听的 127.0.0.1:19999）：**

```
process alive: True
healthz HTTP 200
readyz  HTTP 200   status=ready
existing session: /api/auth/me    -> 200
new login:        /api/auth/login -> 503 {"error":"idp_unavailable", ...}
app password over WebDAV: PROPFIND -> 207
```

改动前，第一行就是"进程退出"。


### ID-08 — 账号删除策略（第十轮）

需求要的是**一个确定的策略**，不是"加个删除按钮"：账号删除/禁用后，文件归属、共享、审计和保留数据必须有确定策略。之前只有禁用——因为 `nodes.owner_id` 是 `NO ACTION` 外键，有文件的账号**根本无法删除**。

现在策略与实现（`internal/store/userdelete.go`，两个后端行为一致）：

| 对象 | 处置 | 理由 |
| --- | --- | --- |
| 文件 | 进回收站，不是转交 | 把一个人的文档转给管理员是没人要求的静默数据移动；也让"删除账号"变成管理员可以直接读文件的请求。进回收站保留可恢复窗口 |
| 分享 | 撤销 | 分享是账号签发的凭据，不能比账号活得久 |
| ACL（作为主体） | 删除 | — |
| ACL（作为授予者） | 删除 | 授予者不存在了，这条授权没人能复核或撤销 |
| 会话 / App Password | 撤销 | — |
| WebDAV 锁 | 释放 | 锁比持有者活得久会让资源永久不可写 |
| 审计记录 | **保留** | 审计不是账号的财产，是"发生了什么"的记录；删除它等于允许账号在离开时擦掉自己的历史 |
| 账号行 | 墓碑（`status='deleted'`），用户名与邮箱释放 | 见下 |

**为什么是墓碑而不是删行**：`nodes.owner_id`、`shares.owner_id`、`versions.created_by` 都是 `NO ACTION` 外键，且审计日志按 id 指向账号。物理删除要么被外键挡住，要么需要级联进审计——而策略明确要求审计保留。墓碑同时保证身份不可复用：旧的 `(issuer, subject)` 不会被一个过期的 IdP token 重新开户，邮箱也被释放以便真正的新账号使用。

**两步流程**：删除前必须已禁用。删除不可逆，而禁用可逆；强制先走可逆那步，错误就会在那里被挡住。

**真实验证（端到端，经 HTTP API）：**

```
before: map[audit rows:3 live nodes:3 live sessions:1 live shares:1]
disable: 200
delete with wrong confirmation: 400
delete: 200 map[nodes_trashed:3 shares_revoked:1 deleted:true audit_rows_kept:3 ...]
after:  map[audit rows:3 live nodes:0 live sessions:0 live shares:0]
```

`audit rows` 从 3 到 3——这是策略里最重要的那一行。回收站清理后 blob 变为可回收（已验证）。

**新增 `0006_account_deletion.sql`**：放宽 `users_status_check` 允许 `deleted`（保留闭合集合——这正是"禁用账号"与"打错状态列"的区别所在）。

**顺带修掉一个测试隔离缺陷**：`AuditIsRecorded` 断言全局审计数为 1，而删除用例会故意留下审计行，于是它在删除用例之后必然失败。这类"依赖用例执行顺序"的测试是定时炸弹，现在改为断言**本用例新增**的数量。


### §7 S3 — 从"跳过"变成"真的跑"（第九轮）

`s3` 后端的合规用例一直是 `t.Skip`——没有 `TEST_S3_ENDPOINT` 就跳过。在这个仓库里，它**每次都在跳过**。README 当时写的是"只在 moto 上验证过"，而 moto 早已不在依赖里，所以那句描述本身也过期了。

也就是说"`s3` 后端与本地磁盘通过同一套合规用例"这句话，在**任何一次实际运行中都没有被执行过**。

现在加了进程内 S3 假实现（`internal/storage/s3fake`），三处受影响的用例默认对它运行：

| 用例 | 之前 | 现在 |
| --- | --- | --- |
| `blobstore.TestS3Conformance`（layout 层） | skip | 16/16 通过 |
| `s3.Test*`（blob 契约层） | skip | 7/7 通过 |
| `TestOpenStorageBuildsS3`（接线） | skip | 通过 |

合计 24 项，`go test ./...` 里**零 skip**。

**假实现只实现这个代码库用到的 5 个操作**（BucketExists / Stat / Get / Put / Remove，外加 multipart 与 ListObjectsV2）。超出范围的请求返回 `501` 而不是编一个看起来合理的答案——这样后端将来改用新操作时会**立刻失败**，而不是在一个猜出来的假实现上通过。

写这个假实现本身发现了三个真实的协议细节，都是会让真实对象存储悄悄出错的东西：

1. **AWS chunked 编码。** minio-go 对流式上传使用 `aws-chunked` 分帧（`<hex-size>;chunk-signature=...\r\n<data>\r\n`）。假实现第一版直接把分帧后的字节存下来，于是 17 MiB 的多段对象存成了 17,850,444 字节——多了 24,652 字节的签名帧。**只有内容比对才会发现**，大小检查、上传状态码全是正常的。multipart 的每个 part 也是分帧的。
2. **`Last-Modified` 缺失时客户端报的是"日期无法解析"**，而不是"头缺失"——错误信息指向的地方离原因很远。206 响应同样需要携带它与 ETag。
3. **`BucketExists` 用裸 `GET /bucket/`**，且 200 的响应体必须是合法 XML；空响应体让 minio-go 报 `EOF`。

**并验证了用例真的会失败**：把 `Delete` 改成什么也不做，`TestDeleteAndMissing` 立刻红（`Stat after delete = <nil>, want ErrNotFound`）。这一点值得单独确认，因为上一轮刚吃过"测试通过但什么都没验证"的亏。

**仍未验证**：真实 AWS / MinIO / R2 的互操作。假实现证明的是后端自身逻辑正确，不是协议完全兼容——签名、重定向、一致性语义这些只有真端点能验证。设 `TEST_S3_ENDPOINT` 即可对真实端点跑同一套用例。
### INV-08 补充 — Blob 内容校验（第八轮）

第七轮的演练只验证了 PostgreSQL。数据库 dump 与 blob 是两条独立的备份路径，而**字节才是用户真正的数据**：一次恢复可能带回完美一致的节点树，指向从未复制或复制时被截断的 blob——部署能列出每个文件，却打不开任何一个。

现在第 6 步做 blob 校验：复制 blob 根 → 在副本上开内容存储 → 把每个文件节点读回来比对。

**这一步第一次写错了，值得记下来。** 第一版拿"恢复出来的字节"和"现存字节"比。结果：故意反转一个 blob 的首字节后，演练仍然 **PASS**——因为两边都坏，比对自然相等。这个检查看起来在工作，实际上什么都没验证，是"测试通过但无效"的又一个实例。

改成用元数据里记录的 `blobs.sha256` 校验之后，同样的破坏立刻被抓到：

```
blobs            12 files, 11 verified, 31.3 KiB
VERDICT: FAIL
  - top.txt: restored bytes hash to c0d5f09b9940, metadata recorded 5029feaf3e6b
    — the backup is corrupt
```

期望值必须来自**写入时记录的**内容地址，而不是来自任何一份现存副本。这也正是下载客户端会做的检查。

**顺带修掉两个真 bug：**

1. `Descendants` 的递归 CTE 里 `nodeColumns` 未加表别名，与 CTE 的 `id` 列冲突（`column reference "id" is ambiguous`）。这个问题在服务端测试里一直没暴露，因为那些测试跑的是内存后端；只有演练连真实 PostgreSQL 才触发。现在加了 `nodeColumnsQualified`，两种形式由同一个函数生成，不会漂移。
2. `BlobHash` 之前不存在——store 只有 hash→id 方向。补上 id→hash，两个后端都实现。

**实测（4 账号 / 21 节点 / 12 blob / 45 审计事件）：**

```
dump size        78.2 KiB
tables compared  20
rows compared    89
audit chain      45 events, verified=true
blobs            12 files, 12 verified, 31.6 KiB
elapsed          1.12s (target 1h)
VERDICT: PASS
```

**仍未验证**：restic 仓库本身的恢复流程（环境里没有 restic；本轮验证的是"文件级复制后字节仍正确且可读"，不是快照恢复与去重索引）；大库耗时；PITR。
### INV-08 — 恢复演练首次执行并自动化（第七轮）

`deploy/backup/restore-drill.md` 从写入起就带着一行 `_待首次演练_`。也就是说 INV-08 一直是**没有证据的声明**：手册写得再详细，也是"恢复能力未经证明"。

现在演练是一个子命令 `newcloud drill`，因为人工流程的实测结果就是**从未被执行过**。它按顺序做六件事，每件对应一种真实失败：dump、校验 dump 可读（截断的 dump 在 `ls` 里和好的没区别）、恢复到 scratch 库、逐表比对行数、**在恢复副本上验证审计哈希链**、报告耗时对 RTO。

**首次实测结果（2026-10-22）：**

```
dump size        77.2 KiB
tables compared  20
rows compared    75
audit chain      45 events, verified=true
elapsed          928ms (target 1h) — within target
VERDICT: PASS
```

数据集是真实写入的（6 账号 / 2 目录 / 5 文件走真实上传 API / 1 群组 / 1 ACL / 40 审计事件），不是手工构造的行。

**反向验证 —— 并证明演练会失败：**

| 破坏 | 结果 |
| --- | --- |
| 改一条审计记录的 `action` | `FAIL`：链校验在第 0 行失败 |
| `SCRATCH_DSN == LIVE_DSN` | 拒绝运行 |
| 缺 `SCRATCH_DSN` | 拒绝运行 |

第一条是重点：表数与行数**全部匹配**，唯一暴露问题的是链校验。

**诚实记录未验证的部分**：blob 内容恢复（restic 路径）没跑；大库耗时未知（77 KiB 不能外推到 40 TiB）；PITR 没演练（"从 dump 恢复"与"恢复到故障前一秒"的 RPO 差一个数量级）。这三条写在文档里"通过"的旁边，而不是藏在后面。

**一个我自己的错误，记录下来**：本轮开头我用 `Get-Content` 看到 `restore-drill.md` 全是乱码，直接判断"文件损坏、中文不可恢复"。实际上文件是**完全正确的 UTF-8**（552 个 CJK 字符，零替换字符）——是 PowerShell 控制台在这个 locale 下的渲染问题。`read` 工具和 .NET 解码都正确。差点因为一个显示问题去"修复"一个没坏的文件；判断编码问题必须以字节为准，不能以终端显示为准。
### §4.2 — 细粒度权限真正生效（第六轮）

这是本项目反复出现的那类缺陷的最后一个大块：**授权原语能表达、能存储、能列出，但没有任何处理器去问。**

具体来说 `read` 与 `download` 从第四轮起就是分开的位，ACL 也能只存 `read`——意图是"你能看到这个文件存在和它的元数据，但你不能拿走一份拷贝"。而所有流式下载端点检查的都是 `PermRead`，所以这个区分**从来没生效过**。

还发现更根本的一层：`expandComposite` 让 `PermRead` 蕴含 `PermDownload`。只要 `download` 不是单独可授予的，这个蕴含是合理的——一旦 §4.2 把它拆开，"read 蕴含 download" 就让拆分本身变成谎言：只授 `read` 也照样能取走字节，`download` 永远无法被扣留。

现在的规则：

| 规则 | 位置 |
| --- | --- |
| `read` **不再**蕴含 `download` | `authz.expandComposite` |
| 下载检查 `download` 而非 `read` | `handleDownload`、DAV `handleGet`、`handleSharedContent` |
| 目录递归操作检查**每个内部节点** | `checkSubtreePerm` / `subtreeAllows` |
| MOVE 需要源节点的 `delete`，COPY 需要 `download` | DAV `handleCopyMove`、`handleMove` |

**顺带补上一条送达通路。** 改完检查后发现 `download` 依然无法使用：下载端点按**路径**解析，而路径在调用者自己的命名空间里，所以被授权的共享文件对任何内容路由都不可达——`download` 授了等于没授。于是加了 `GET /api/shared/{id}/content`，按不可猜的 id 寻址，每次仍然过授权者。

真实实例验证（两个账号，一个只授 `read+list`，随后加上 `download`）：

```
no-download:   metadata=200 perms="read+list"           content=404 (47 bytes)
with-download: metadata=200 perms="read+list+download"  content=200 (9 bytes)
```

**测试是先证明会失败的。** 第一版 `TestDownloadRequiresDownloadNotJustRead` 在把检查改回 `PermRead` 之后仍然通过——因为它按路径访问，而该路径对受限账号根本不存在，404 是"文件不存在"而不是"权限不足"。这类"因为错误的原因而通过"的测试，是本项目里最需要警惕的东西。改成走 `/api/shared/{id}/content` 后，回退检查立刻变红（`got 200, want 404`）。

### 队列时钟源（第六轮，flake 根因）

`jobs` 的 `run_after` 默认值取自 Go 进程时钟，而领取时比较的是数据库的 `now()`。两个时钟只要有几毫秒偏差，任务就"到点"却又领不到，直到时钟漂移过去。

这解释了那个间歇性失败：只在 PostgreSQL 出现（只有它有两个时钟），只在机器跑久了之后出现，而且报错位置（`setup claim failed`）离原因很远。现在默认值交给 `COALESCE($4, now())`，由数据库提供；调用者显式给的 `RunAfter` 仍然照用。

新增用例 `DefaultScheduleIsImmediatelyClaimable` 钉住这一点；队列套件连续跑 5 次全部通过。
### ID-04 收尾 — 账号批准 API（第五轮）

第四轮加了 `pending` 模式，它**故意**把账号创建成禁用状态等待管理员批准——然后就没有然后了。store、API、CLI 里**没有任何操作**能把账号从 `pending` 改出来。一个按文档配了 `pending` 的部署，会把每一个新用户永久锁死，唯一的出路是手工改库。

现在有 `GET /api/admin/users?status=pending` 与 `POST /api/admin/users/{id}/status`：批准、禁用、重新启用都是同一个状态变更，审计行记录 `from`/`to` 以及被切断的凭据数量。

写这组测试时挖出两个真 bug：

1. **`UpsertIdPUser` 硬编码 `Status: "active"`。** 于是 `pending` 模式算出了一个谁都不存的 status——**它实际上是 `open` 的别名**。这点在第四轮的测试里没暴露，因为那个测试只断言"登录被拒绝"，而拒绝发生在建号之前，所以账号从未存在，也就从未被检查过状态。
2. **中间件从不检查账号状态。** 它每请求都读用户行（为了拿到当前角色），但只用了 `Role`，从未看 `Status`。即使禁用成功、会话也撤销了，一个在撤销提交前一毫秒签发的会话，或者在快照上读到旧行的副本，仍然能work。现在每请求检查——"禁用"不该依赖凭据是怎么来的。

### §10.2 — INSERT-only 角色真正可用（第五轮）

第一版迁移建了 `newcloud_app` 角色，权限也给了对：公网表 SELECT/INSERT/UPDATE/DELETE，审计表只有 SELECT/INSERT。**但角色是 NOLOGIN，部署以数据库属主连接**——属主是超级用户，超级用户绕过一切 GRANT。所以从第一版起文档里"审计日志对应用是只追加的"这句话，在**每一个实际部署里都是假的**。

`0005_app_role.sql` 把角色改成可登录（仍是 `NOSUPERUSER`/`NOCREATEDB`/`NOCREATEROLE`），重述授权，并加了一个 `app_role_status` 视图直接回答"这条连接能不能改写审计日志"。

**没有做的事**：迁移里不写口令（会永久留在版本控制里、且每个环境共用），不改 `pg_hba`（那是集群配置，不是 schema 的事）。运维用 `ALTER ROLE newcloud_app PASSWORD` 设一次。

真实验证（以 `newcloud_app` 身份连库）：

```
select * from app_role_status
  connected_as | is_superuser | audit_is_append_only
  newcloud_app | f            | t

INSERT INTO audit.events (...) RETURNING id  -> 2       -- 允许
UPDATE audit.events SET result='tampered'    -> permission denied
DELETE FROM audit.events                     -> permission denied
TRUNCATE audit.events                        -> permission denied
```

**顺带修掉一个潜伏的迁移解析 bug。** 写 `0005` 时 `CREATE FUNCTION ... $fn$ ... $fn$` 失败，报 `"events_default_pkey" is an index`——一个指向完全无关位置的错误。根因在 `stripTxControl`：它只跟踪匿名 `$$` 定界符，具名标签 `$fn$` 内的内容被当成普通 SQL，于是函数体里的 `BEGIN`（PL/pgSQL 块开头）被当作事务包装删掉了，函数体被破坏。

现在用一个真正的定界符状态机（跟踪标签本身，所以 `$fn$` 体里出现 `$$` 文本也不会提前闭合）。`schema_test.go` 里有 5 个用例，其中具名标签那个在旧实现下会失败（`want exactly one BEGIN, got 0`）。

### §11.1 — 指标端点（第五轮）

文档点名要的信号是：请求错误率、DAV 冲突率、上传失败率、队列积压、GC 与审计校验。

`/metrics` 以 Prometheus 文本格式暴露这些，全部有测试（11 个用例）。几个决定：

* **状态码分桶**（2xx/3xx/4xx/5xx）而不是每个码一条序列：真正的问句是"失败占比多少"，按路由×精确状态码建序列是让指标端点变成内存泄漏的标准做法。
* **计数器用原子量而非加锁 map**：它在请求路径上，一把锁会让所有请求排在一个指标后面——这是可观测性把故障放大的经典方式。
* **锚点校验与发布同频**：只写不比的锚点等于没有。校验不一致时打 `ERROR` 并计入 `audit_anchor_verify_total{result="failed"}`。
* `/metrics` 不鉴权：爬虫带不了会话 cookie，暴露的是运维计数而不是用户数据；不该公开的部署应该在反向代理层不路由这个路径。

**证据：** `go test ./internal/metrics`（11 个用例）、`go test ./internal/store -run TestStripTxControl`（5 个用例）、`go test ./internal/server -run TestAdmin`（5 个用例）。

真实实例验证：`/metrics` 返回真实计数（`GET 2xx=2`、`GET 4xx=1`、`PROPFIND 4xx=1`、`dav_requests_total=1`、`jobs{state=...}`、`audit_events=1`、`audit_anchor_verify_total{result="ok"}=1`）。
### §11.1 — 后台任务队列真正有消费者（第四轮，INV-06）

`jobs` 表从第一个迁移起就带着一整套东西：`run_after`、`attempts` / `max_attempts`、租约（`locked_until` / `locked_by`）、`dedupe_key`。**但没有任何代码读它**——`Enqueue` 只在测试里被调用过。任何把后续工作排进队列的操作，都排进了一个黑洞。

现在的语义：

* 领取用 `FOR UPDATE SKIP LOCKED`，两个 worker 不会拿到同一行，也不互相阻塞；
* 领取时**立即**计数一次尝试。worker 在报告任何结果前死掉，这一次尝试仍然被消耗掉了；
* 租约到期后可被重新领取——这是崩溃恢复路径，也是为什么租约要落库而不是存在内存里；
* 默认错误重试，退避是指数且有上限；超过上限后任务**留在表里**并带错误，可事后查看，不删除；
* 完成是时间戳而不是删除，所以"到底跑没跑"是可回答的。

**永久失败是终态的。** 这一条踩了坑：最初 `permanent=true` 只清租约而不调整 `attempts`，于是行仍然"有预算"，下一轮照样被领走——"永久"反而被重试了。现在永久失败会把 `max_attempts` 降到当前尝试数，让行真正终止。

**证据：** `go test ./internal/store -run Conformance/JobQueue`（7 个子用例 ×2 后端）；`go test ./internal/worker`（7 个用例）。

真实实例验证：手工插入 `thumbnail.warm` 与 `blob.collect`，前者被消费完成（`attempts=1, completed`），后者因 payload 缺 `blob_id` **只失败一次**即终止，日志为 `job failed permanently`。

### §10.2 — 外部锚点（第四轮，INV-09）

`audit.Anchor` 接口从第一轮就存在，`RecordAnchor` 从未被调用——链可校验，但**整条尾巴被删掉之后仍然校验通过**。这是哈希链的固有性质：它证明内部一致，不证明完整。

现在 `internal/auditverify` 有真正的实现与验证路径：

* 每个已完成的一天发布一个 checkpoint（链头哈希 + 事件数 + 指向上一个 checkpoint 的链接），**一天一个文件**——覆盖写的锚点证明不了它曾经的内容；
* 写临时文件再 `rename`，崩溃不会留下一个读起来像真锚点的截断文件；
* `Verify` 做三件事：检查 checkpoint 之间是否断链、检查日期是否出现空洞、以及**按发布的计数重新计算链头**并比对。

第三件事是关键。只比对"最新 checkpoint 与当前链头"会漏掉"先删尾巴、再继续追加"：链头最终会与所有旧 checkpoint 都不同，看起来就像正常增长。按计数重算才不会。

**证据：** `go test ./internal/auditverify`（6 个锚点用例，含截断、中段重写、删除 checkpoint 三种篡改）。

真实实例验证：写入 5 条真实审计记录 → 发布 checkpoint → 用 `DELETE` 删掉最新 2 条 → 验证输出

```
chain now: 3 events
verify: ok=false matched=0/1
  PROBLEM: checkpoint 2026-10-22 covers 5 events but the chain holds only 3: events were deleted
```

剩下的 3 条事件本身仍然是一条完美链接的链——这正是锚点存在的理由。

**一个诚实的限制：** 内存后端根本不维护密封哈希链（只有 `AuditEntry`，没有 `prev_hash` / `hash`），所以 `Memory.AuditHead` 直接返回 `ErrNoChain`，而不是伪造一个只能自洽的链头。配了锚点却在内存后端跑，会明确失败而不是假装有防篡改能力。

### §6.3 — 预览解码的隔离（第四轮，T-14/T-16）

像素上限（50 MP）之前就在，它挡住的是"一个文件很大"。它挡不住"一百个请求同时到达，每个都是合法的 50 MP"——单个请求被拒绝的理由被并发数乘了一遍。

现在加了两道：信号量限制同时解码数（4，按 CPU 而非请求率定，因为这是 CPU 密集工作），以及每次解码 10 秒的截止时间。解码 goroutine 无法被杀死，所以截止时间约束的是**调用方等待时长与槽位占用时长**，这是 Go 对 CPU 密集库调用的诚实上限——也因此像素上限仍然必需。

这个包此前**一个测试都没有**，于是一个存在了好几轮的解压炸弹防护从未被验证过。现在 `go test ./internal/thumbnail` 有 7 个用例，包括真实构造的 10000×10000 PNG（在磁盘上很小，正是大小检查抓不到它的原因）。

### ID-04 — JIT 开户策略（第四轮）

`status` 列带着 "pending" 的设想存在至今，**从未被写入**：任何 IdP 断言的身份都在首次登录时立即以组映射的角色建号。对一个只托管自己组织文档的部署这是合理的；对 IdP 租户范围大于"应该有账号的人"的部署，这等于"能认证的都能存文件"。

三种模式，登录时判定：

* `open` 立即开户（历史行为，也是默认值——改默认值会静默锁死现有部署）；
* `pending` 建号但禁用（`status='pending'`），需管理员批准。**拒绝建号会让管理员没有可批准的记录、用户也拿不到解释**；
* `deny` 直接拒绝，账号必须预先创建。

域名白名单在所有模式下都生效。它不是 IdP 的替代——IdP 才是真正的闸门——而是在 IdP 租户比"该访问这个部署的人"更宽时加的第二把锁。

**已存在的账号永不被重新判定**：批准是一次性事件，每次登录都重新判定意味着改配置就等于故障。

**证据：** `go test ./internal/server -run TestJIT`（6 个用例，覆盖三种模式 + 域名白名单 + 不重复判定）。
### §4.1 — ACL 管理 API（第三轮）

上一轮结束时 ACL 能存、能判、能列出，但**没有任何端点可以创建授权**——只能写 SQL。这一轮补上了 `POST /api/acl/`（授予）与 `DELETE /api/acl/{id}`（撤销）。

设计上三个决定：

* **授予本身是一项权限**（`manage_permissions`），不是仅限所有者。所有者可以把"分配权"委托出去，收回时也只是删一条授权，而不是特例。
* **主体按人知道的方式命名**——用户名、邮箱或群组名——由服务端解析。拼错就是 404，而不是创建一个指向不存在 id、静默无效的授权。
* **撤销时校验授权确实属于该节点**。id 不可猜，但它出现在调用者能管理的每个节点的列表里，而"手滑"不是不可猜的。

**证据：** `go test ./internal/server -run TestACL`（7 个用例，含越权、未知主体、未知权限、组授权、写别名、跨节点撤销）

### 测试脚手架的 ACL 桩（第三轮，重要）

写上面那组测试时发现：`internal/server` 与 `internal/dav` 的测试 accessor 里，`OwnerGroupPerm` 一直是 `return 0, false, nil`。也就是说**整个 HTTP 与 WebDAV 测试面都在 ACL 关闭的状态下运行**——上一轮做完的授权层，在服务层没有任何测试能覆盖到，一个"处理器忽略了授权"的 bug 不可能让测试变红。

现在两个脚手架都走真实的 `EffectivePermsFor`。这不是测试细节：它是"上一轮声称 ACL 生效、但服务层测试证明不了"这个缺口的修补。

### §3.3 — break-glass 本地管理员（第三轮）

配置项从第一版迁移就在，但**没有任何代码读过它**：口令文件在校验时被检查存在性，然后从未被打开。也就是说，运维按文档配了破窗账号，在真正需要它的时候会发现这个功能是虚构的——比不提供更糟。

现在：

* `POST /api/auth/breakglass`，未启用时返回 404（不暴露端点存在）；
* **先查来源网段再验口令**，暴力破解连 KDF 都到不了；判据是 `RemoteAddr` 而不是转发头——一个本进程无法验证的头会让唯一保护形同虚设；
* 用户名与口令都按常量时间比较；
* 每次尝试都落审计（`auth.breakglass.used` / `.denied`，带 reason）；
* 账号是真实的 user 行（`idp_issuer = local://breakglass`），所以可以被停用、被审计、被看到；
* 启动时校验口令文件可读——配了但用不了，要在能被修的时候失败，而不是在事故中；
* **口令文件每次尝试重新读取**：在事故中轮换凭据不应需要重启；
* `newcloud breakglass hash` 子命令生成哈希（从 stdin 读，不进 shell history），这也是运维原本必须写代码才能做到的事。

顺带把 Argon2 从 `internal/share` 提取成 `internal/password`：分享口令与破窗口令现在共用同一份实现，两份拷贝迟早会漂移成其中一份更弱。

**证据：** `go test ./internal/server -run TestBreakglass`（6 个用例）、`go test ./internal/password`（7 个用例）；真实实例验证：正确口令 200 且 `content_access: false`、错口令/错用户名 401、网段不符 403、轮换后旧口令 401。

### ID-02 — OIDC nonce（第三轮）

PKCE 把 code 绑到这个客户端，state 把回调绑到这个浏览器会话，但两者都不绑定 **ID token** 与发起它的那次请求。缺 nonce 时，一个为同一客户端另一次请求签发的 ID token 仍然能通过——签名、issuer、audience、有效期全都成立。

现在 authorization request 携带 nonce，回调时按常量时间比对 `id_token` 的 nonce claim；没有 nonce cookie 的回调直接拒绝（否则只要丢掉 cookie 就能跳过检查）。

**这一条值得单独说明的是测试**：最初写的三个 nonce 测试都只断言"nonce 被发出去了"，把比对代码删掉它们仍然是绿的。为此给 fake IdP 加了 `TamperNonce` 开关，让 IdP 回显一个没人请求过的 nonce，才真正测到比对本身——把比对代码注释掉后该测试立刻变红（`got 302, want 401`）。

### 回归修复 — 管理员可下载（第三轮）

写 break-glass 测试时发现上一轮的 `expandComposite` 引入了一个真实漏洞：`PermRead` 展开为 `PermDownload`，而管理员分支正是靠"有 read 但没有 download"来保证"管理员看不到用户文件内容"。展开之后管理员拿到了 `download`，注释仍然写着"admins do not read user content"，位图却已经相反。

现在管理员分支显式跳过展开，并由 `TestAdminCannotDownloadUserContent` 钉住。
### T-02 — 角色/群组变更传播到既有会话（第二轮）

需求文档第 3 章的三条验收示例里，这是最后一条仍然失败的。之前的实现只在**下次登录**时刷新角色，而中间件每次请求读的是 `users.role`，所以被移出管理员组的人在会话 TTL（默认 12 小时）内仍然是管理员；WebDAV 的 App Password 更是永远不再和 IdP 对话，等于永久保留旧权限。

现在的做法（`Tx.ApplyIdPRole`）：

* 登录时把 IdP 当前声明的角色与库里的比较，写入新角色；
* 若这是一次**降权**，同时撤销该账号的全部会话与全部 App Password——因为正是这些凭据把旧权限延续下去；
* 升级不动任何凭据（否则等于"给他更多权限，然后把他踢下线"）；
* 角色未变化是纯 no-op，否则每次登录都会写一条变更事件并把用户登出；
* 角色变更本身落审计（`user.role.change`，降权记为 `denied`）。

实现上踩到一个真实的坑并修掉：`UpsertIdPUser` 原本会先把角色写成新值，导致 `ApplyIdPRole` 永远看不到"变化"，降权因此从不触发撤销。角色现在只由 `ApplyIdPRole` 一个地方写。

**证据：**

```
go test ./internal/store -run 'Conformance/ApplyIdPRole'   # 三个子用例，双后端
go test ./internal/server -run TestGroupRemovalRevokesTheLiveSession
```

后者是真正的验收用例：它**先保存旧 cookie**，模拟 IdP 移除管理员组，重新登录，再用旧 cookie 重放——必须 401。

### §4.1 — 资源 ACL 真正生效（第二轮）

`acl_entries` 之前是死 SQL：建表后没有任何 Go 代码读或写它，`OwnerGroupPerm` 是 `return 0,false,nil`。于是文档里的三层权限模型实际只有两层——平台角色 + `owner_id` 相等。

现在：

| 部分 | 实现 |
| --- | --- |
| 授权原语 | 补齐 §4.1 要求的 `download`、`create`、`overwrite`、`rename`、`move`、`manage_permissions`、`view_history` |
| 授予 | `Tx.Grant`（用户或群组，二者必居其一）+ 同一 (node, subject) 上的**替换而非累加**，由部分唯一索引兜底 |
| 继承 | 祖先链一次递归 CTE 走完；`inherit = false` 只作用于本节点，这正是"共享文件夹里的受保护子目录" |
| 群组 | `groups` / `group_members` 的真实读写，群组授权只对成员生效 |
| 解析 | `Tx.EffectivePermsFor` 返回并集；**没有 deny 位**——两个 allow 产生并集，收回权限就是删除授权 |

组合语义：`write` 蕴含 `create + overwrite`，`read` 蕴含 `download`，但**都不蕴含 `delete`**——"能改"不等于"能删"。

| 送达 | `Tx.SharedWithMe` + `GET /api/shared`、`GET /api/shared/{id}` |

**送达这一段是本轮最有价值的发现。** ACL 做完并全部通过测试后，用真实 HTTP 验证时发现授权根本用不上：WebDAV 的 `resolvePath` 里有 `_ = user`，把 URL 里的用户名段直接丢弃，永远在调用者自己的根上解析；JSON API 同理。授权存得对、授权者判得对，但**没有任何入口能把请求路由到别人树里的那个节点**。

这里没有去放宽命名空间隔离（它正是"不能靠猜名字访问别人目录"的保证），而是加了一条显式的第二条路径：`/api/shared` 列出被授权的节点根，`/api/shared/{id}` 按 ID 访问。ID 是不可猜的 UUID，且每次访问仍然过授权者。这条路径放宽的是**能被发现什么**，不是**能被做什么**。

**证据：**

```
go test ./internal/store -run 'Conformance/ResourceACL'      # 7 个子用例，双后端
go test ./internal/store -run 'Conformance/SharedWithMe'     # 4 个子用例，双后端
go test ./internal/store -run 'TestStoreGrantReachesTheAuthorizer|TestOwnerStillOutranksACL'
```

`TestStoreGrantReachesTheAuthorizer` 是**接线测试**：conformance 证明授权存得对，authz 矩阵证明授权者会用，但没有测试证明两者接通——而它们此前确实没接通。

真实 HTTP 验证（两个账号 + 一个未授权账号）：

```
GET /api/shared       owner    -> 200 []
GET /api/shared       granted  -> 200 [team(read+list+download)]
GET /api/shared       outsider -> 200 []
GET /api/shared/{id}  granted  -> 200
GET /api/shared/{id}  outsider -> 404（与"不存在"不可区分）
```

### §11.1 — 就绪探针真的探测依赖（第二轮）

`/readyz` 之前返回固定的 200 和一段静态 JSON。数据库挂掉时容器仍报健康，编排器继续把流量导过去——这正是就绪探针存在的唯一理由。

现在它做浅探测：一次元数据往返（用能证明 schema 可用的最廉价查询）+ 一次内容存储读取，3 秒超时，任一失败返回 503 并在 `checks` 里说明是哪个依赖。

**证据：** `go test ./internal/server -run TestReadinessReflectsDependencyHealth`——它在运行中**破坏存储**再断言探针变红。

### ID-01 补充 — 邮箱被 IdP 重新分配（第二轮）

第 3 章的第三条验收示例：`users.email` 有 UNIQUE 约束（刻意的，否则"分享给 alice@example.com"会有歧义），当 IdP 把某个邮箱分配给新的 `sub` 时，PostgreSQL 抛唯一约束冲突，最终变成 **HTTP 500**。它确实"没有继承文件"，但那是因为登录直接失败了。

现在映射为明确的 `409/403 identity_conflict` + 审计事件，而不是服务器错误。同时修掉了后端分歧：内存实现原本不检查邮箱唯一性，所以测试通过而生产报错。

**证据：** `go test ./internal/store -run TestRecycledEmailIsNotInherited`


### INV-02 / T-03 — 统一条件写入

之前有**四条互不相干的写入路径**，只有 WebDAV 那条做了 ETag 检查，而且检查在 handler 里、写入在另一个事务里——这是典型的丢失更新窗口。浏览器上传和版本恢复**完全没有**前置条件。

现在的结构：

| 层 | 位置 | 作用 |
| --- | --- | --- |
| `store.Precondition` | `internal/store/store.go` | `If-Match` / `If-None-Match` 的唯一解释 |
| `Tx.WriteFile` | `internal/store/pg.go`、`memory.go` | 在**改行的同一语句**里校验前置条件 |
| `tx.lockNode` | `internal/store/pg.go` | `SELECT … FOR UPDATE`，消除检查与写入之间的窗口 |

三个入口共用它：`dav.handlePut`、`server.publishFile`（浏览器上传与分片上传收尾都走这里）。`UpdateFile` 的 `UPDATE` 自身也重复带 ETag 谓词——即使将来有人去掉行锁，写入也不会覆盖另一个版本。

**顺带修掉的两个真实缺陷：**

* WebDAV `PUT` 覆盖时返回 `201`，RFC 4918 §9.7.1 要求 `204`。客户端会误以为每次保存都新建了文件。
* 版本恢复写入的 ETag 是 `blobID.String()[:16]`——未加引号的 UUID 前缀。恢复之后客户端的 `If-Match` 永远匹配不上，所有后续 DAV 写入一律 412。

**证据：**

```
go test ./internal/store -run 'Conformance/ConditionalWrites'   # 两个后端都跑
go test ./internal/dav   -run 'TestPutIfMatch|TestPutIfNoneMatchStar'
```

两套存储实现（内存 / PostgreSQL）都过同一套用例。这是刻意的：如果内存实现忽略前置条件而 PostgreSQL 强制它，本地测试全绿，生产静默丢写。

### INV-01 / T-01 — 跨租户恢复（真实漏洞）

`handleRestore` 只接收一个节点 ID，**不检查归属**，`tx.Restore` 的 SQL 也没有 `owner_id` 谓词。任何登录用户只要拿到（或猜到）一个 UUID，就能把别人的回收站文件恢复到对方目录。

现在恢复流程依次校验：节点确实在回收站 → `OwnerID == actor.ID()` → 调用者对被恢复到的父目录仍有写权限。三项任一不满足都返回与"不存在"相同的 404。

**证据：** `internal/server/files.go` 的 `handleRestore`；手动验证见 §3。

### §5.3 / §5.2 — LOCK 真正生效

之前 `activeLock` 只在 `handleLock` 内部被调用一次。锁能创建、能刷新、能过期，但 **PUT / DELETE / MKCOL / COPY / MOVE 从不检查它**——客户端以为受到保护，实际没有。

现在新增 `lockBlocks` / `requireUnlocked` / `lockAncestors`：

* 写目标被锁 → `423 Locked` + `DAV:lock-token-submitted`；
* 父集合被 `Depth: infinity` 锁住 → 子项创建同样被拒；
* 持有者必须通过 `If` 头**提交令牌**才能写（RFC 4918 §10.4）。不能区分持有者与其他人，锁就只是建议。

同时修掉：

* 目录 `COPY` 之前只创建目标目录就返回，**产出空文件夹并报告成功**——比拒绝更糟，因为客户端相信数据已复制。现在递归复制整棵子树，逐层重新授权，带深度保护。
* `MOVE` 覆盖目标时直接 `Trash` 目标节点，**不做任何授权**。现在要求对目标具备 `PermDelete`，与显式 DELETE 一致。
* 可满足性失败的 Range 请求返回 `200` + 完整实体；现在返回 `416`。

**证据：**

```
go test ./internal/dav -run 'TestPutRefusedWhileLocked|TestDeleteRefusedWhileLocked|TestMkcolRefusedUnderLockedParent|TestCopyDirectoryIsRecursive|TestMoveOverwriteRequiresDeleteRight'
```

### INV-07 / T-07 — 版本引用与 GC

`pushVersion` 从不增加 `blobs.refcount`，`trimVersions` 也从不减少。配合 `CollectableBlobs` 只查 `refcount = 0`、**从不查 `versions` 表**，结果是：一个被替换过的文件，其历史版本引用的 blob 可能被回收——"恢复旧版本"在几个月后变成永久 404。

现在：`pushVersion` / `trimVersions` 成对增减引用；`CollectableBlobs` 与 `DeleteBlob` 都排除仍被 `versions` 或 `nodes` 引用的 blob。

**证据：** `go test ./internal/store -run TestPGExpireVersionsKeepsBlobReferencesConsistent`

### §6.2 / T-08 — `authorized_download_session`

之前下载次数按 **HTTP 请求** 计。一次媒体播放的每个 Range 请求、每次断点续传都会被计成一次下载，有限的分享额度瞬间耗尽；而计数发生在 `Content.Open` **之前**，存储故障会白吃一次额度。

现在有真正的下载会话（`download_sessions` 表 + `Tx.AuthorizeDownload`）：

* 绑定 `(share, node, 访客凭据)`，并记录当时的 ETag；
* **Range 请求续用已有会话，不重复计费**；
* 完整 GET 是一次新下载，计费一次；
* 换了访客、换了文件、或文件版本变了，都要重新计费；
* 先打开内容再计费。

**证据：**

```
go test ./internal/store -run 'Conformance/DownloadSessions'   # 4 个子用例，双后端
go test ./internal/server -run TestShareDownloadLimit
```

手动验证见 §3：6 次 Range 请求 = 1 次下载；额度 2 时第 3 次完整下载被拒。

### SHARE-02 / T-09 — 验证码单次使用与尝试限制

之前验证码是一个 HMAC 票据：15 分钟内**可以无限次验证**，没有任何失败计数或限速。持有分享链接的人可以对着被邀请地址随便猜，服务端只会在审计里留一行。

现在：

* `share_recipients.consumed_at` 让验证码**只能用一次**（条件 UPDATE，并发提交只有一个能成功）；
* `share_otp_attempts` 按 `(share, email)` 记录失败，15 分钟窗口内超过 5 次锁定；
* 限速**在验证码校验之前**判断，因此"猜到正确答案"也不会被接受；
* 被限速的响应与验证码错误完全一致——区分它们等于确认分享和邀请是否真实存在。

**证据：**

```
go test ./internal/share -run 'TestCodeIsSingleUse|TestVerificationIsThrottled|TestThrottleIsPerAddress'
```

### INV-09 / T-15 — `audit verify` 与外部可比对的链头

链条一直都在，但**没有任何生产代码读过它**。`VerifyAuditChain` 只被测试引用。一个从不校验的防篡改日志，和一个普通日志没有区别。

新增 `newcloud audit verify [--json] [--batch N]`：

* 分页重算每个事件的规范编码与哈希，不信任库里的值；
* 额外比对链头，捕获**截断**——逐行校验抓不到尾部删除，因为剩余部分仍然是一条合法前缀；
* 退出码即契约：0 完整，非 0 断裂。

**证据：**

```
go test ./internal/auditverify                     # 完整/改写/截断/中间删除/空链
go test ./internal/store -run TestPGAuditVerify    # 对真实 PostgreSQL 改写一行并断言被拒
DATABASE_URL=… ./bin/newcloud audit verify
```

### §8.1 / §8.2 — 保留策略与真正的删除路径

之前**代码里根本不存在删除节点的路径**（`grep 'DELETE FROM nodes'` 无结果）。回收站只会增长，`trash_retention_days` 这类参数在文档里有、在配置里没有。

现在：

* 四个配置项进入 `config.LimitsConfig`：`version_retention_days`、`version_count_limit`、`trash_retention_days`、`upload_session_ttl_hours`；
* `Tx.PurgeTrash` 递归删除过期子树，释放配额、归零引用、连带删除版本；
* `Tx.ExpireVersions` 按时间清理历史版本并保持引用正确；
* worker 每 6 小时执行一次；
* **0 表示永久保留**，且未配置时启动日志会明确警告——"配了保留策略"和"保留策略什么都没做"从外部看是一样的。

**证据：**

```
go test ./internal/store -run 'TestPGPurgeTrash|TestPGExpireVersions'
```

### T-19 — 游标分页

目录列表之前是"取前 N 条 + 一个 `truncated` 布尔值"，没有游标。基于名字排序做 OFFSET 会在并发增删时重复或漏掉条目。

现在 `Tx.ChildrenAfter(parent, after, limit)` 用**排序键本身**作游标（keyset），`handleList` 接受 `cursor` 与 `limit` 并返回下一页游标。

**证据：** `go test ./internal/store -run 'Conformance/ChildrenAfter'`（含"分页过程中插入兄弟节点"的用例）

### §3.2 — App Password 作用域真正生效

`dav.authenticate` 丢弃了凭据对象，无条件授予 `ScopeRead|ScopeWrite`。也就是说**一个只读的 App Password 可以通过 WebDAV 写入**——最需要只读保护的入口恰好忽略了它。

现在从凭据读取 scope 并传给 `authz.NewUserActor`；`auth.ScopeForActor` 不再是死代码。同时新增 `RevokeUserAppPasswords`，`DisableUser` 会同时撤销会话**和**全部凭据（此前只撤会话，WebDAV 凭据可以继续用几个月）。

### §10.1 — 缺失的登录失败事件

`auth.login.failure`、`user.disable` 之前只有常量、没有写入点。现在登录失败（校验失败 / 账号被禁用）与会话撤销都会落审计。

---

## 2. 明确仍然没做的部分

这些不是"下个迭代"，而是在**当前状态下不能对外宣称具备**的能力。

| 需求 | 现状 | 影响 |
| --- | --- | --- |
| 可观测性 | `/metrics` 端到端验证过（路由/中间件/标签）；**告警规则与运行手册已随附**（`deploy/monitoring/`：10 条规则、抓取配置、9 个 runbook 章节），`promtool` 校验通过。三个"从未被写入"的指标（上传、缩略图、GC）已接线，并有结构性测试防止再出现——没人写入的指标格式正确却永远为 0，看起来和健康指标一样 | 阈值是起始值而非实测结论，需要两周真实流量后再调；没有随附 Grafana 面板 |
| ACL / 账号审批 UI | **ACL 与审批都已有界面**：Access 页管理文件夹授权；账号页有审批队列（待审批/已启用/已停用）与状态切换，导航项仅对管理员出现（服务端仍返回 404，测试覆盖）。两个 Go 测试发出与面板完全相同的请求，防止"两边读同一份规格却理解不同" | 界面只覆盖单目录授权与账号状态；没有批量操作，也没有"谁可以访问这个文件"的反查视图 |
| 备份脚本**实际执行** | `deploy/backup/backup-all.sh` 按正确顺序调用两个脚本并写 manifest，**实测跑通全链路**：dump 88 508 字节 → restic backup/check → restore 8/8 blob（文件名即 SHA-256）→ pg_restore → 交叉校验缺失 0 个。跑起来才发现 `pg-backup.sh` 用裸 `pg_dump`、**忽略 `DATABASE_URL`**，已修 | 未在 cron/定时任务下长期运行；WAL 归档（PITR）仍未演练 |
| §11.2-4 备份与恢复的顺序 | 备份顺序固定为**先 pg_dump，再备份 Blob**（依据"内容先落盘、行后写入"）；恢复顺序**先恢复 Blob，再 pg_restore**。两者方向相反、理由也不同：备份搞反会永久丢字节，恢复搞反只是窗口内读取失败、随后自愈。恢复侧之所以安全，依赖 **GC 是元数据驱动**这一性质（遍历行、不扫目录），该性质有源码级与 PostgreSQL 行为级测试，且删掉 node join 会让测试变红 | 仍未做真实并发压测（测试确定性地构造两种顺序的产物状态，而非让真备份与写入赛跑） |
| INV-08 restic 恢复 | 已实测：`blob-backup.sh` 全链路（backup/forget/prune/check）通过，`restic restore` 后 blob 的 SHA-256 与原件逐字节一致。修掉两个真问题：`--one-file-system` 在 Windows 上 panic；文档里的恢复命令会得到错误的目录布局。**规模测量已补**：130 KiB / 1.2 MiB / 5.5 MiB 三点，秒/MiB 稳定在 6.9–9.4，43 倍体积内无拐点；文档里"约 82 小时"的外推是错的（实为约 92 000 小时），已修正并由 `TestDrillScaleMeasurementsAreConsistent` 守住 | 测量上限只有 5.5 MiB，与 40 TiB 差 7 个数量级，**仍不支持任何 40 TiB 的 RTO 数字**（而且线性外推给出的数字说明该路径本身不可用）；PITR 仍未演练 |
| SHARE-06 文件删除对链接的影响 | 已明确并测试三种情况：**删除**后链接返回 404 而非 200 空列表；**重命名与移动**后链接继续有效（授权挂在节点上，不在名字或路径上），且链接报告的是新名字、下载仍然可用；"文件已删/已吊销/从未存在"三者响应体逐字节相同（否则端点会成为令牌预言机）。重命名测试用"假装缓存了旧名字"的变异验证过会变红 | 缩略图等衍生资源未单测；链接会在文件被移动后仍然有效，这是设计决定，现已由测试固定 |
| T-01 越权访问 | 表驱动覆盖 12 条带 id 路由中的 10 条（其余 2 条各由既有测试覆盖并写明），每条断言 404 而非 403、响应体不泄漏元数据、变更型操作后资源未改动；元测试防止新路由被漏掉 | 上传分片路由在校验身份前先解析 `Content-Range`（缺头返回 400 而非 404）——不是漏洞，但错误顺序不同会让调用者较早得知端点存在 |
| 文档与代码的一致性 | `TestDocumentsCiteOnlyRealTests` 强制文档引用的每个测试名都真实存在（实测抓住了我自己编的三个）；讨论已废弃名字的句子须显式标记 `docIgnore:` | 只校验名字存在，不校验该测试是否真的断言了所声称的东西 |
| ID-03 的**轮换实测** | `fakeidp` 支持轮换与撤回，三个端到端测试验证：轮换后登录成功、已有会话不受影响、全量撤回后靠重取 JWKS 仍成功；策略写在 `deploy/oidc-key-rotation.md` | 撤销一把被盗密钥对**新**校验生效；已缓存该密钥的进程要重取或重启（go-oidc 的缓存性质，已在文档写明） |
| INV-08 复验 | 演练表清单改为自检（`TestDrillCoversEveryTable` 对着实际 schema 比对），补上漏掉的 2 张表；重跑通过：22 张表、12/12 blob 字节校验、审计链 32 事件 | 仍未做 restic 快照恢复、大库耗时、PITR |
| INV-07 blobs.size | 修复写入；历史行由 `RepairBlobSizes` 从磁盘对象回填（`migrate` 时运行，实测恢复 4 行 / 2936 字节），单行缺失只报告不臆造 | `tree` 布局不是内容寻址，该布局下无法回填，会明确跳过并告警 |
| T-20 COPY/MOVE 失败 | 已验证源文件不被破坏：配额不足、目标父目录缺失、目录复制中途失败均保留原件；配额不足时 MOVE 不先删源 | 目录复制逐节点提交，中途失败会留下部分目标（已记录，源不受影响）；无快照/回滚整个复制 |
| T-19 游标一致性 | 契约已写明（`docs/0012-cursor-contract.md`）并双向验证：绝不重复、删除不破坏遍历、重命名跨游标的限制被钉住、游标不携带权限 | 无快照隔离，因此重命名到游标之前的行本轮不可见；排序为字节序，不随客户端 locale |
| T-16 审计存储不可用 | 策略已写明（`deploy/audit-failure-policy.md`）并验证：写操作 fail-closed 且回滚、读操作保持可用、客户端得 `503 audit_unavailable` | 磁盘写满只做注入模拟，未在真实满盘演练；审计**变慢**（而非不可用）没有超时策略 |
| §5.3 锁的跨接口语义 | **API 与 DAV 共用同一套深度规则**：目标自身锁 / `Depth: 0` 父锁（成员关系）/ `Depth: infinity` 祖先锁（内容）三者在两侧都实现，均返回 423 且需提交 token；锁到期由 worker 每分钟清扫 | `COPY` 不复制锁（RFC 4918 也不要求），跨账号移动的锁语义未单独测试 |
| §4.1 ACL 的**界面** | API 完整，**且已有界面**（Access 页的 `FolderAccess`，见下文第三十轮） | 前端的授权操作只覆盖"按路径管理单个目录"；没有跨全账号的授权总览视图（服务端也不提供，这是设计） |
| §4.2 授权原语粒度 | `read`/`download`、`create`/`overwrite`、`rename` 均已拆开并在对应操作上检查；递归操作检查子树；MOVE 需源 `delete` | 8 处 WebDAV 父目录/LOCK 检查仍用复合 `PermWrite` —— 这是决定，因为一次 PUT 就是"创建或覆盖"的二选一 |
| §3.3 break-glass 的**运维面** | 登录、网段限制、审计、轮换、CLI 生成哈希均已实现；**"使用后告警"已实现**：`newcloud_auth_events_total{kind}` 记录成功与失败两种事件，配两条告警规则（使用即 critical、15 分钟内失败 >5 才 warning），runbook 有调查步骤 | **刻意没有 UI**：恢复入口的网页界面只会扩大暴露面，与需求"对公开网络暴露制定独立决策"相悖；`readSecretLine` 不做终端免回显（文档注明从管道读） |

| ID-04 JIT 的**界面** | 三种模式 + 域名白名单 + 批准 API（`/api/admin/users`） | 没有界面：批准目前通过 API 完成 |
| ID-06 群组变更的主动同步 | 降权在**下次登录**时生效（T-02 已验证该路径） | 没有 IdP 事件源或定期轮询，所以"移出组"到"权限消失"之间有一次登录的间隔 |
| ID-07 的**身份缓存** | 规则已实现并验证：启动不依赖 IdP，已有会话/应用密码/撤销照常工作，新登录 503 | 无 IdP 元数据缓存（每次重试仍是一次网络往返）；缓存还能让"IdP 恢复后首次登录"更快，但会引入密钥轮换的新失效面 |
| ID-08 的**恢复路径** | 删除策略已实现并端到端验证（文件进回收站、分享撤销、审计保留、墓碑释放标识） | 墓碑行不可撤销；恢复需管理员从回收站取回文件并新建账号 |
| §11.1 任务种类 | 队列已有消费者与 3 种 job；幂等由各 handler 自行保证 | 队列基建完整，业务侧待补更多种类 |
| §11.1 指标的**图表** | `/metrics` 暴露请求/DAV/上传/队列/审计/GC 信号；**告警规则与 runbook 已随附**（`deploy/monitoring/`，10 条规则） | 没有随附 Grafana 面板 JSON（面板取决于部署的 Grafana 版本，随附容易过期） |
| §10.2 锚点**存放位置** | 文件锚点已实现并验证能检测截断 | 生产环境必须把它指向与数据库不同凭据的位置；代码无法强制这一点 |
| §10.2 INSERT-only 的**运维步骤** | `newcloud_app` 已可登录、授权已重述、`app_role_status` 视图可直接自检；已验证 UPDATE/DELETE/TRUNCATE 被拒 | 口令需运维手动设置（迁移里故意不写）；`docker-compose.yml` 已改用该角色，但需先 bootstrap 口令 |
| §6.3 预览隔离 | 像素上限 + 并发上限 + 解码截止时间 | 仍在进程内解码：真正的隔离需要子进程 |
| §9.5 数据导入导出 | 无 | — |
| SHARE-08~11 | 未实现（符合文档中 P1/P2 定位） | — |

**恢复演练（INV-08 / T-17）** 已在第七轮执行并自动化（`newcloud drill`），见 §1。README 曾声称演练已通过而实际未执行，这一句当时已修正；现在它确实通过了。

---

## 3. 手动验证记录（2026-10-22，单节点 PostgreSQL + 本地产 blob）

用一个独立实例（`127.0.0.1:18777`，数据库 `newcloud@127.0.0.1:15437`）验证：

```
# 创建 → 201；覆盖 → 204（不再是 201）
PUT /dav/<user>/a.txt            → 201  ETag "3bfc…"
PUT /dav/<user>/a.txt            → 204  ETag "b10f…"

# T-03：过期 ETag 的写入被拒，且字节确实没落地
PUT … If-Match:"3bfc…"           → 412
GET …                            → "someone-else-wrote"

# §5.3：锁真正挡住写入
LOCK …                           → 200  <opaquelocktoken:…>
PUT  …（无令牌）                  → 423
DELETE …（无令牌）                → 423
PUT  … If:(<令牌>)                → 204  → 内容已更新
UNLOCK …                         → 204

# T-01：跨账号恢复被打回 404，本人恢复仍是 200
POST /api/files/restore {受害者节点ID}（攻击者身份） → 404
POST /api/files/restore {同一节点}（所有者身份）      → 200

# COPY 递归
COPY /dav/<user>/src → /dav/<user>/dst   → 201
GET  /dav/<user>/dst/sub/b.txt           → 200 "beta"

# T-08：Range 不重复计费，完整下载才计费
6 × GET /s/<token>/download (Range)      → 均 206/416，download_count = 1
GET /s/<token>/download（完整）×2         → 200, 200，download_count = 2/2
GET /s/<token>/download（完整）           → 404（额度已尽）
```

---

## 4. 关于"已完成"的口径

本次改动的验收标准是**测试可复现**，不是代码存在：

```
go test ./...                                   # 全部通过
TEST_DATABASE_URL=… go test ./internal/store    # 追加 PostgreSQL 契约测试
./bin/newcloud audit verify                     # 退出码 0
```

需求文档 §14 的 20 条故障注入用例中，现在有对应测试的是：T-01、T-02、T-03、T-07、T-08、T-09、T-12（部分）、T-15、T-19。其余仍待补充，未被这些测试覆盖的能力不应报告为已验证。

第二轮结束时：`go test ./...` 全部通过（282 个测试用例，含 PostgreSQL 契约测试），gofmt 与 go vet 干净，四个迁移文件可从空库依次应用且重复执行是 no-op。
