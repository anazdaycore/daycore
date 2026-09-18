# 架构总览

> 这个仓库现在是什么样、为什么这样：包结构、装配与关停、多实例、两层时区、分发与降级启动。改架构必须同批更新本文件。最后全面核对：2026-07-14；2026-08-13 重构结构与交叉引用（内容未逐行重核）。

## 目录

| 节 | 内容 |
|---|---|
| 包结构 | 包职责表 |
| 四个存储后端 | 推荐 Mongo、条件写用列、行为套件 69 例 |
| 第五种后端 | HTTP / 子进程协议 |
| 节律与 Protector | 学习作业、三个时刻、20h 关怀 |
| 外部能力源控制台面 | F2-A：批准门、改地址重建 |
| 分发 | 一个二进制 + install + lite |
| 降级启动 | 一次性、服务什么、readiness 语义 |
| 每会话地点 / 时区 | 两层时区、提示不覆盖用户选择 |
| 多实例 | 选主 + 场次占有，各背一半承诺 |

## 包结构（仓库根，Go module `daycore`，go 1.23）

| 包 | 职责 |
|---|---|
| `cmd/daycore/` | 入口 main.go（装配 config/store/catalog/prompts/server/worker/channels）+ install.go（prompt 模板导出器，walk embed FS，新模板自动导出） |
| `internal/domain/` | 纯数据结构 + Repository 接口，零外部依赖 |
| `internal/server/` | HTTP 路由、中间件、全部 handler、agent loop、cron Worker |
| `internal/storage/sqlstore/` | SQL 三方言（SQLite/PostgreSQL/MySQL），每实体一文件 |
| `internal/storage/mongostore/` | MongoDB 实现，每实体一 repo 文件；`bson_test.go` + 真机行为一致性套件（`make test-mongo`） |
| `internal/ai/` | AIProvider 抽象、Catalog、PromptService（embed+DB override）、流式协议、vision 管线 |
| `internal/auth/` | 密码(argon2id)/OAuth/JWT(token.go)/签名 cookie(session.go) |
| `internal/channels/` | 通道插件框架（Registry + OneBot 11 适配器） |
| `internal/config/` | 环境变量配置（godotenv，见下） |
| `internal/search/` | web 搜索（Tavily→DDG）+ MaterialSearcher（原生 FTS 优先 + 子串兜底，见 DATA.md） |
| `internal/weather/` | WeatherProvider registry（open-meteo/qweather/owm/wttr.in，30min 缓存） |
| `internal/version/` | 版本唯一真源：Version="2.2.0" Channel="beta"；APIVersion 契约常量（阶段 1 加） |
| `internal/rapport/` `internal/rhythm/` `internal/mood/` | 默契评分与主动性门控 / 节律学习 + 20h 关怀 / 心情窗口（趋势+衰减+新鲜度）—— 三个都是纯函数、零存储、读时派生，见 DATA.md |
| `internal/schedule/` `internal/ics/` `internal/i18n/` | 规则展开引擎 / ICS 解析 / locale 协商 + **三层消息目录**（DB → 文件 → 内嵌 zh-CN·en-US）+ 用户级一主一副 `Pair`，见 DATA.md |

## 推荐 MongoDB，但四个后端都要能跑（2026-07-29）

**MongoDB 是推荐部署**，理由是这个数据模型确实更贴它：主题变量、提案的 rows/ops、rapport 分数、节律日 —— 一半的新实体本来就是自由文档，Mongo 存它们不需要「塞进一个 JSON 列再整体重写」。

**但兼容不是可选的。** 推荐与要求之间那条线要划清楚，否则「支持四个后端」会悄悄变成「只有 Mongo 真的测过」—— 而今天之前 `mongostore` 恰好是那个**零测试**的，方向还是反的。现在两边都跑同一套行为套件（见下）。

### SQL 侧的取舍规则：条件写用列，其余用 JSON

「大键值是个 JSON、每次整体重写」在**大部分**字段上是对的取舍 —— 省掉三方言 DDL、加字段零迁移、与 Mongo 的形状对齐。但它不能一刀切，因为整体重写**放弃了条件写**：

> **凡是出现在 `WHERE` 里、或被算术/`CASE` 更新的字段，必须是列。其余一律可以进 JSON blob。**

批次 C 已经是这么分的，把规则写下来是为了后面不走偏：

| 必须是列（参与条件写） | 可以是 JSON blob |
|---|---|
| `rev`（CAS）、`state`、`expires_at`、`ttl_policy`、`delivered_at`、`deliver_after`、`merge_key`、`created_at` | `rows_json`、`ops_json`、`applied_op_ids`、`accept_op_ids` |
| `first_min`/`last_min`/`signals`（`CASE WHEN` 扩边界） | — |
| `last_signal_at`/`run_since`（`WHERE last_signal_at < ?`） | — |
| `attempts`、`status`、`started_at`（占有与接管判定） | `error_text` |
| `fence`、`holder`、`expires_at`（选主） | — |
| `cursor_created_at`/`cursor_id`（游标续读） | `scores_json` |

把 `first_min` 塞进 blob，扩边界就退成读-改-写 —— 两个标签页开着就丢信号。这不是性能取舍，是正确性取舍。

### 行为一致性套件（`internal/storage/storagetest`，已落地）

一套按 `domain.Store` 写的行为套件，**四个后端都跑同一份**：sqlstore 覆盖 SQLite / PostgreSQL / MySQL（后两个靠 `PG_TEST_DSN`/`MYSQL_TEST_DSN`，`make test-sql`），mongostore 用真机 Mongo（`MONGO_TEST_DSN` 未设则跳过，`make test-mongo`）。69 个用例，全部来自审查与对抗验证抓到的真实分歧 —— 不是「能存能取」，而是：

lease 只有一个持有者且 fence 只在交接时动 / `Acquire` 永不返回别人的行 / 空 holder 被拒 / 场次占有互斥 / 完成的场次不再被占 / 失败重试到上限 / 崩溃接管有界且回报真实 attempts / **接管轮换占有令牌使僵尸的 `Finish` 落空** / `Prune` 保留 running / nil 切片回来是空切片而非 nil / 指向零值时间的指针算「不存在」 / **`ProposalOp.Args` 的数字在每个后端都回来是 `float64`** / `Validate` 在 `Create` 与 `Update` 两侧都生效 / `rev` CAS 拒绝陈旧写 / TTL 不对称 / **同毫秒并列时 Supersede 恰好留一张** / 已投递的卡不被退休 / keeper 缺失是非事件 / 可投递集合排除过期与压后 / 序列化失败拒绝写入 / rapport 游标往返 / **学习作业不擦掉活的清醒标记** / `Touch` 只向前 / 分钟 0 是有意义的值 / 并发首写不丢信号 / 语言包往返与整语言卸载 / `RevertedBy` 精确且不跨会话 / `Scan` 最旧优先且游标续读无重无漏 / **空 canvas id 的 upsert 被拒而不是覆盖上一条无键行** / **每个 List 的默认页大小与天花板四后端一致** / **`Delete` 限定在本会话内、删不存在的行报 `ErrNotFound`** / 提案的 level 谓词与三态戳谓词 / **`Count` 忽略 `Limit`**（否则预算数到一页就饱和）/ **「堆叠里几张卡」是合取不是问戳**（`delivered_at` 永不清除）/ **`Prune` 放过 pending**（老的 pending 是 `Expire` 还没扫到，删它是抹掉用户还欠着的卡）/ `OwnerInstance` 可查。

**加后端的验收标准就是这套套件通过**，包括计划中的 HTTP 转换层。这也是让第五个后端负担得起的唯一办法：两两分歧数随后端数平方增长，共享套件把它压平。

✅ **四个后端全部真机验证完毕**（首次 2026-07-29，最近一次 2026-08-08 —— θ-F4b 的 `settings` 一例在四个后端上逐条跑过）：

| 后端 | 行为套件 | 建表 | 原生全文索引 |
|---|---|---|---|
| SQLite | 43/43 | ✅ | ✅ FTS5 external-content + 三触发器 |
| PostgreSQL 16 | **43/43** | ✅ 34 张表 | ✅ tsvector 生成列 + GIN |
| MySQL 8 | **43/43** | ✅ 34 张表 | ✅ FULLTEXT ngram |
| MongoDB 8 | 43/43 | ✅ | ✅ text index |

**pg 与 MySQL 是第一次真机执行**（此前只有 `dialect_parity_test.go` 的静态比对，而静态比对只能看出三份 DDL 互相不一致、看不出其中任何一份是否合法 —— 这个洞放跑过三次真事故）。原生索引那一列也是第一次验：不只断言 `condApplied` 为真，还真发一次查询，因为索引建起来不等于查询语法对，而查询语法错只在有人搜索时才报。

本机跑：`make test-mongo` + `make test-sql`，各用例自建自删 schema/数据库。

⚠️ **CI 于 2026-08-09 删除**，此前它起 mongo:8 + postgres:16 + mysql:8 三个 service，并有一步**断言它们没有静默 skip**。那一步的理由现在落到人身上，而它当时是对的：**跳过的套件读起来和通过的一样**（`ok  daycore/internal/storage/mongostore`，无论跑了 44 例还是 0 例），而 DSN 环境变量正是那种会悄悄不再被设置的东西。跑真机套件时**看用例数**，别看那行 ok。

## 存储的第五种后端：HTTP / 子进程转换层

**完整协议在 [`docs/specs/storage-protocol.md`](specs/storage-protocol.md)**，传输规则在 [`specs/transport.md`](specs/transport.md)。这里只记为什么与顺序。

「转换层 + 内部高效适配」这个模式扩到存储层是对的，天气/搜索/通道（F2）已经这么设计。但**最小接口不是基础 CRUD**：

`domain.Store` 有 179 个方法、**20 处条件写**。它们不是优化，是这个仓库唯一的互斥手段（**全包无事务**，`grep BeginTx` 零命中）—— 选主、任务场次占有、提案行级接受、节律日边界、清醒标记。纯 CRUD 表达不了任何一条，**少了条件写这些保证会全部静默降级成「通常能用」**，且降级不报错，只在并发下偶尔出错。

协议底线是 **CRUD，其中 insert 在 id 冲突时失败而不是覆盖**。有了这一个原语，条件写要么原生支持、要么适配层用一把锁补出来（代价：一次写从 1 个往返变成 4 个；而一把**错**的锁比没有锁更糟 —— 没锁是偶尔丢更新，锁错是整张表卡死。配方与四条硬要求见协议文档）。

**传输两种**：`http`（服务，可远端、可扩容）与 `exec`（后端监管的子进程）。`exec` 不是降级 —— 崩溃给**退出码 + stderr**，而 HTTP 适配层崩了只剩 `connection refused`。顺带记下：Go 的 `plugin` 包不适合第三方生态（仅 Linux/macOS、要求完全相同的工具链与依赖版本、无法卸载），所以「子进程 + 协议」不是绕开 Go 的弱点，**它就是 Go 的标准答案**。

⚠️ **顺序：先套件，后后端。** 批次 C 的审查在现有两个后端之间抓到约十五处行为分歧，而当时没有任何测试断言两者行为相同。再加一个后端只会把分歧从 O(1) 对变成 O(n²) 对。

### SQL 侧存自由数据的三种写法

「一个大 JSON 每次整体重写」是最省事的一种，但不是唯一的，也不总是最好的：

| 写法 | 适用 | 代价 |
|---|---|---|
| **JSON 路径写**：`json_set(doc,'$.k',?) WHERE json_extract(doc,'$.k') > ?` | 自由结构**且**需要条件写 | 三方言语法不同（`json_set`/`jsonb_set`/`JSON_SET`），要走 `Dialect` 方法；索引靠生成列或表达式索引 |
| **侧表** `(entity_id, key, value)` | 键集开放**且**要按键单独查 | 一次 join、行数放大 |
| **热字段提列 + 其余 blob** | 大多数情况 | 加字段要动 DDL |
| **整包 blob 重写** | 从不被查询、从不被条件写 | 放弃条件写 |

已实测：SQLite（modernc，带 JSON1）**一条语句就能做条件式部分更新**，`json_set` 配 `WHERE json_extract(...)` 影响行数正确。Postgres 是 `jsonb_set`/`->>`，MySQL 8 是 `JSON_SET`/`->>`。

**Mongo 不是「不需要」，是同一个问题的另一种写法** —— `rhythmRepo.Observe` 用的 `$min`/`$max` 就是路径写，与 SQL 的 `json_set(...) WHERE json_extract(...)` 同构。所以「统包 blob」在两边都不是必需的。

**主题变量是侧表更合适的那一类**：F7 的补算要问「哪些主题缺 token X」，侧表是一个 `WHERE`，blob 是全表扫加逐个解析；「给所有主题加一个 token」也变成每主题一次 INSERT 而不是整体重写。

## 节律学习与 Protector（ζ-2 / ζ-3，2026-08-07）

### 在此之前，`internal/rhythm` 是本仓第六次「写完、测过、没人调用」

515 行纯函数，**两个生产调用方**，都在 `awake.go`、都只是算一个 day key。`Learn` / `LearnDays` / `Schedule` / `Cold` / `Pin` 与整个 `schedule.go` 一个都没有。仓储侧同样：`Get`/`Save`/`Touch`/`Days`/`PruneDays` 全部零调用方，只有 `Observe` 是活的。

顺带戳破一句文档谎言：ARCHITECTURE 说三个定时时刻「由节律派生」—— 那**只在数值上成立**。`Schedule(Cold())` 恰好产出 04:00 / 07:30 / 21:00，正是 cron 里写死的那三个数，所以「学到别的东西会改变什么」这件事根本无法被观察到。

### 学习作业（`rhythm_job.go`）

跑在 `DayCutHour`（默认 04:00），因为那一刻节律日刚闭合、昨天那行才是终值。**代价说清楚**：对 04:00 前起床的人，新画像落在今天的 PlanAt 之后，改动晚一天生效。备选是「PlanAt 前 15 分钟」，代价是作业的触发时刻由它自己算出来的东西决定 —— 自举，对「其它一切都由它排程」的那一块是个坏性质。

**两道过滤都是必须的，不是保险**：

| 过滤 | 不做会怎样 |
|---|---|
| `>= windowStart` | `rhythm.LearnDays` **不做窗口过滤**（窗口活在 `DaysFrom`，那是信号列表那条路）。而 `Days(ctx, sid, 21)` 是**行数上限不是日期过滤** —— 间歇性用户最近 21 行可能横跨半年，直接喂进去就是拿去年冬天的作息学今天的节律。静默、无报错。 |
| `< todayKey` | 当天那行还在长（`LastMin` 一直涨到人睡觉），纳入会把 Sleep 越学越早。04:00 跑时当天几乎没有行，看起来无害 —— 直到有人手动触发。 |

**证据变薄不遗忘**（`mergeLearned`）。`LearnDays` 回答的是「**这些天**说了什么」，天数不够时答 `Cold()`，对那个问题是正确的。把它直接写回去不是：休假三周会把三个月的学习成果扔掉，一个人的早报一夜之间退回 07:30，没有解释、也不是他做了什么。

所以策略放在**调用方**而不是改纯函数：保留学到的时刻，如实汇报（更低的）天数。画像停止增长信心，但不丢失已有的。代价是真的换了作息的人会在攒够 `MinDays` 新证据前继续用旧时刻 —— 几天的轻微偏差，换的是不被重置成一个从来不适合他的默认值。

**按「不分工作日/周末」实现**（EXPERIENCE_CORE §5 留的口子，ROADMAP 仍挂着）—— 分开学要改 `Day`、`Profile` 与三方言 DDL + mongostore + 行为套件。

### Protector（`protector.go`）

EXPERIENCE_CORE §5 + 共识 28：**只看连续清醒时长，与任何日界线机制无关**。不看午夜、不看 04:00 日切、不看石化线。

**致命断点先修**：`rhythm_profiles.run_since` / `last_signal_at` **没有任何生产写者** —— 日行在写、两个活标记没在写，于是 `Live.Run` 读到零 `RunSince`（它正确地理解为「睡了」），`NeedsProtector` **恒为假**。不先接 `foldAwakeMark`（`awake.go`）就接 worker，得到的是「编译过、用伪造数据测试全绿、线上永不响且一行日志都没有」。

**场次 key 必须是「这段清醒的起点」**，别的都错：

| key | 后果 |
|---|---|
| `dayKey` | 跨本地午夜的 30 小时清醒**响两次** |
| `slotKey` | **每半小时响一次** |
| `run:<RunSince>` ✅ | 一段清醒恰好一次 —— 这才是「同一个长夜」的意思 |

按 **UTC** 格式化：`RunSince` 是绝对时刻，按会话时区格式化会让一段跨 DST 的清醒凭空换个 key 再响一次。

**TTL 取的是 ask-first（`silence_rejects`），与设计稿相反**。`design-ui` 的 mock 写的是过去时（「我先帮你顺延**了**」）= act-first。这里选了反的：第 20 个小时的人同样可能正在赶 deadline、马上要用到上午那些块。**凌晨四点的沉默不是同意**，是有人在专注或者已经睡了，而在他底下挪他的早上，代价比问一句更大。要翻的话改的是一个常量。

**Do Not Disturb 抑制的是整条关怀，不只是推送**：一张凌晨四点静静出现、中午才被读到的关怀卡，说的是关于昨天的一个事实。

**推送预算 ≤3/天**（共识 24），从**已有的行**数出来（带 `pushedAt` 且落在窗口内的提案），不另设计数器 —— 计数器是第二个真源，第一次推送半途失败两者就分叉。读不到预算时**不推**：关怀卡在 app 里仍然在，而挤在一个本来就吵的日子上正是预算要防的事。

### 顺带修掉的既有 bug：`parseBlockTime` 忽略它自己的日期参数

签名收 `dateStr`，函数体里用的却是 `time.Now()`。两个既有调用方碰巧都传今天，所以这个参数是**一个没有任何东西能抓到的谎**。Protector 是第一个需要对另一个日期讲道理的调用方，于是「昨天的块」被当成「今天还没到」。

## 外部能力源的控制台面（F2-A，2026-08-08）

`GET·PUT /api/admin/providers`。可写的只有三项，且这条线是 F1 那条规则**逐字段**应用的结果 —— *进程有没有用它造出别的东西*：

|住哪|字段|
|---|---|
|`config/providers.yaml`（只能手改）|`id` / `format` / `base_url` / `token_env`|
|`provider_overrides`（控制台可改，立即生效）|`enabled` / `description` / `approved`|

⚠️ **`base_url` 于 2026-08-09 从「只能改文件」改成可改**，而被推翻的那条论证值得留着，因为它听起来很有道理：

> 「它是 SSRF 入口，所以要求登机器才能改就是全部防线。」

**那句话不成立**，而同一批代码里就写着反例 —— `internal/adapters/baseurl.go` 自己论证过：*能改 `providers.yaml` 的人本来就有这台机器*。「谁能设这个值」在两个方向上都是弱防线。

**真正在挡的是两条与「谁设的」无关的东西**：`ValidateBaseURL` 拒 link-local 与内嵌凭据，客户端拒绝跟随重定向。它们对一个从网页输进来的值，和对一个从文件读出来的值，一模一样地生效。

**真正失去的，说清楚**：够到这个字段以前要 shell，现在要一张控制台凭据 —— 而后者能被钓鱼、能被浏览器里的脚本偷走，shell 不能。缓解是值仍然过校验，且改动可见（是一行带时间戳的覆盖）。

不写 YAML 这条**理由变了但结论不变**：它不再是「哪些字段敏感」，而是**安全地重写一份运维也在手改的文件本身就是个难题** —— 丢注释、丢顺序、覆盖并发编辑、读到写之间有窗口。所以控制台要能改的字段就住进表里，表压过文件。

### 改地址要重建，而且要清空健康

这是 `ApplyOverride` **唯一**不在原地改的字段。旧的健康记录属于**旧地址那台机器**：带过去，就是让一个刚指过去的适配层在第一次调用之前就背上三次失败 —— 而那三次是关于另一台服务器的。反过来更糟：一条「健康」的记录为一个没人试过的地址作保。

`Sources.Rebind` 重建 client 并换一个新的 `Health`。而**其它字段一律不重建**，正是为了「打开一个描述」不会顺手把已知死掉的源标成健康 —— 两条断言分别守着这两个方向。

⚠️ 一处闸门：`Entry.BaseURL` 是**文件说的那个**，运维一改地址它就不再是真相。全仓只有 `internal/adapters` 能读它，其余一律走 `Source.BaseURL()`，由 `TestNothingOutsideAdaptersReadsTheFileBaseURL` 走源码树守着 —— 这个错误编得过、读起来很自然，而且症状最坏：控制台显示新地址、覆盖行是新地址、健康检查打新地址，**只有真正的请求发去了旧地址**。

### 批准门有两道，缺一它就是装饰

1. **存的是描述的哈希**，不是一个布尔。绕过端点直接改库里的描述 → 哈希对不上 → 掉回未批准。
2. **端点上：改了描述就撤销批准**，除非同一个请求里重新批准。

**第二道是写测试时才发现缺的，而且它是一个真 bug**：只有第一道时，一个改描述的 patch 会用**新文字**重算哈希，于是批准悄悄延续到没人读过的文字上 —— 每一个可见行为都正常。这正是这套机制存在要防的那件事，也说明了为什么它必须有断言而不是只有注释。

### 就地更新，不重建

`ApplyOverride` 只改那三个字段。重建会造出新的 HTTP client，更要紧的是造出新的 `Health` —— 把这个进程学到的「哪些源还答话」全部丢掉。于是「打开一个描述」会顺手把一个已知死掉的源标成健康，运维会看着它因为一个与自己动作无关的理由重新进入工具带。

### 健康是每进程的，所以响应里带实例名

适配层常常与后端同机，A 够不着的源 B 可能够得着。两个控制台**合理地**给出不同答案 —— 而没有实例名的话，那是一个没人能解释的现象。

### 边界：没有密钥，也没有任何替身

`tokenEnv` 是**变量名**（不是凭据），`tokenSet` 说那个变量当前有没有值。永远没有值、没有前缀、没有长度 —— 打码的密钥仍然告诉攻击者它多长、两次读之间变没变，而这两样控制台都不需要。

## 分发：一个二进制 + `daycore install`（2026-08-08）

**分发单位是一个静态二进制**，前端另外部署。零 CGO（`modernc.org/sqlite` 纯 Go），所以 `go build` 出来的就是静态文件；提示词模板、L1 边界、模型目录种子都内嵌。目标机器上不需要源码树、Go 工具链或包管理器。

### 这个命令唯一的规矩：它写出来的东西必须能启动

**此前不能。** `install` 建了一个空的 `config/` 就完事，而 `LoadCatalog` 读不到 `config/models.yaml` 直接返回错误、`main` 变成 `exit 1`（`ai.LoadCatalog` 是启动路径上**唯一**零容忍缺文件的加载器 —— 没有模型就没有 AI，硬起来只会让每个功能在稍后各报一个不同的错）。于是照它自己打印的 Quick Start 走下去，得到的是一个报告成功、然后起不来的目录。这是一个安装命令能有的最坏形状。

闸门是 `cmd/daycore/install_test.go` 的 `TestInstalledTreeBoots`：装进临时目录，**用生成的 `.env` 走一遍 `config.Load()`**，再跑 main 跑的那几个加载器。走 `config.Load` 而不是手读 `.env`，是因为有一半的失败方式在文本上完全正常 —— 键名写错、种子里的 id 与 `DEFAULT_CHAT_MODEL` 的默认值对不上、路径只在源码树里解析得开。

### 边界

- **install 只写文件，不连任何东西。** 不发网络请求、不连数据库、不校验它收集的 API key。需要联网的安装步骤没法用来装一个内网部署；连数据库的安装步骤把「DSN 打错了」变成一个**在有东西能读到错误之前**就发生的失败。错值留到首次启动暴露，那里服务器能把话说清楚。
- **种子是种子，不是地板。** 提示词与 L1 边界每次启动都从内嵌读、磁盘只是覆盖；`models.yaml` / `oauth.yaml` 写出来一次就归运维了，之后没有任何东西再读种子。做成地板意味着运维删掉的一个模型条目下次启动又回来。
- **重跑 install 不覆盖已改的文件**（`-force` 才覆盖）。人们确实会重跑它 —— 为了找回丢掉的 admin token，或者升级之后。
- **`STATIC_DIR=` 写成空赋值是无效的**：`getEnv` 把「设了但为空」当作没设（`config.go`），所以那行只会读起来像配置过、实际什么也没配。API-only 本来就是「目录里没有 index.html」时的自然结果，启动日志现在报的是 `Server.StaticRoot()`（真正在服务的那个），不是 `cfg.StaticDir`（配置的那个）。

### 取舍

- **种子里的上游模型名会过期。** 半年后装的人得到的目录里写着可能已经下线的模型名。替代方案是安装时去拉一份当前列表 —— 那会让安装需要访问一个本项目并不运行的服务。**一个运维要改的过期种子，胜过一个离线就装不上的安装程序。**
- **安装期的连通性检查是可选的、失败也不中断。** DSN 打错是最常见的安装错误，所以值得问一句；但做成强制的就等于「数据库必须先于应用存在」，而新机器上的正常顺序恰恰相反。检查不建库、不跑迁移 —— 迁移属于服务器启动路径，那里失败有地方可报、有东西去报，从安装器里跑等于留下一个半成品 schema，而唯一的目击者是一个已经被关掉的终端。

## `install` / `config`：一条命令配完后端（2026-08-08）

`internal/setup` 是两条命令共用的交互流程：`daycore install` 按顺序跑完全部八段，`daycore config <段名>` 对一个已经存在的部署重跑其中一段。

段：`db` / `models` / `keys` / `weather` / `oauth` / `channels` / `server` / `secrets`。

- **顺序不是随意的**：数据库排第一，因为它是最可能填错的那个答案，而失败离刚敲下的东西越近越好用；密钥排最后，因为 admin token 要是屏幕上的最后一屏 —— 印在二十行之上的凭据会被滚走。
- **一段只能写它自己的 key**，而且是**查出来的不是信任的**：`Configure` 事后 diff 一遍 `.env`，越界就拒绝保存。`daycore config oauth` 悄悄改掉 `DB_DSN` 会把「换个登录方式」变成一次故障，而跑它的人没有任何理由去复查一个他只打算改一行的文件。
- **未知的 key 会被保留**。运维手加的 `HTTPS_PROXY` 不能因为跑了一次 `config models` 就消失 —— 默默丢掉别人的编辑，比这个流程问的任何一个问题都糟。
- **`daycore config secretz` 这种拼错是按名字拒掉的**，不会落到「那就全跑一遍」——那意味着一次拼错轮换掉签名密钥。

### 模型目录的「稍后再说」仍然写文件

它是启动路径上唯一缺文件即 `exit 1` 的东西，所以一个「稍后」如果真的不写，就是安装器主动提供一个起不来的部署。「稍后」= 写起手目录 + 提示 `daycore config models`。

### CLI 的多语言

走的是服务端同一套 `i18n.Reg` / `i18n.T`，语言包也是同一个 `LOCALES_DIR/<locale>.json` —— **一份翻译同时管 CLI 和 API**，翻译者做完一个就等于做完另一个。CLI 自己解析语言（`-lang` → `DAYCORE_LOCALE` → `LC_ALL`/`LC_MESSAGES`/`LANG` → 部署默认），因为 `install` 跑在有数据库之前、通常也在有配置文件之前，操作系统是唯一可读的信号。

**边界**：请求的语言没有包时说一句然后用默认，**不静默切换**。设了 `LANG=ja_JP` 却拿到中文的人会以为参数坏了；被告知「没有 ja-JP 包，用 zh-CN —— 丢一份 ja-JP.json 到 LOCALES_DIR」的人知道该干什么，而**这句话本身就是第三方语言包的全部说明**。

**边界**：这里没有一条字符串去插值另一条翻译过的字符串，只插值值（路径、数字、provider id）。两个翻译半句拼出来的句子，翻译者无法调整语序，而语序恰恰是这些语言之间不同的地方。

## `-tags lite`：真的不内嵌任何东西（2026-08-08）

	默认构建     所有资源内嵌，二进制自给自足
	-tags lite   不内嵌任何东西，全部从 DataDir() 读

内容集中在 `internal/resources/data/`（提示词模板 / `boundaries.json` / 两份种子，共 62 KB），消费方一律调 `resources.FS()`，不知道也不需要知道自己在哪个构建里。替代方案是把 `if lite` 撒进提示词加载器、边界加载器和安装器 —— 三处要对一件只有其中一处看得见的事达成一致。

### 上一个 `-tags lite` 是假的，这次靠闸门

它宣称「不内嵌文件、二进制更小」，两样都不成立：`//go:embed` 写在 `prompts.go`，**那个文件没有 build tag**，模板照样编进去。实测差 20 KiB / 23.5 MB（0.08%），全是那个访问器自己。而它真正做到的是让 `WalkPromptFS` 变成 no-op，于是 `daycore install` 在它下面解压零个模板 —— **唯一可观察的效果是把安装命令弄坏**。

现在的诚实数字：**23.660 MB → 23.629 MB，省 31 KB（0.13%）**。

**所以 lite 的价值不是体积，别拿体积去论证它。** 是这三条：

1. 内容在磁盘上 —— 可编辑、可 diff、可进配置管理、可翻译，**只有一个真源**；
2. 没有一份看不见的第二拷贝可以和磁盘上那份悄悄不一致；
3. 缺文件是一个**带路径的错误**，而不是静默回落到一份运维根本不知道存在的东西。

两道闸门守着它，因为「一个仍然带着模板的 lite 二进制」从外面和从里面看都和不带的一模一样：
- `TestLiteBuildEmbedsNothingOutsideTheFullFile` 走源码树，`//go:embed` 只允许出现在 `resources_full.go`（按行首匹配 —— 一个分不清指令和散文的闸门会对着解释它为什么存在的那段注释开火）。
- 一条从 `boundaries.json` **算出**探针的检查（此前在 CI 里，2026-08-09 随 CI 一起删；判据仍然成立，重新加时照这个形状）：断言它在完整二进制里**在**、在 lite 里**不在**。两个方向都查 —— 只查一边的话，探针字符串哪天漂走了，`grep -q` 什么都找不到，检查从此永远通过、永远什么也没断言。

### lite 的数据从哪来：三条路，同一个二进制

1. **release tarball 旁置** —— `make build-lite` 同时产出 `dist/daycore-lite` 与 `dist/daycore-data-<版本>.tar.gz`，解开放在二进制旁边。**pack 不是可选包装，是这个产物的另一半**，单发其中一个就是发一个起不来的东西。
2. **完整版装过的目录** —— `DAYCORE_DATA_DIR` 指过去。
3. **`daycore install -fetch`** —— 按**本二进制的版本**下载对应的 pack。

查找顺序：`DAYCORE_DATA_DIR` → 可执行文件旁边的 `data/` → `./data`。显式设置排第一、工作目录排最后，所以某人 home 目录里一个无关的 `./data` 永远赢不过一个真实部署。

**边界：`-fetch` 只在 `install` 里、只在被要求时发生，永远不在启动路径上。** 一个会自己下载自己行为的二进制无法审计、在本项目在意的内网部署里直接坏掉，还会把一次 DNS 故障变成「助手说的话变了」。**也没有 `latest`** —— 按版本钉死，一个跑着去年代码、配着今年提示词的 lite 二进制是没人测过、也没法从版本号复现的组合。

**边界：解包拒绝任何逃出目标目录的条目，也拒绝符号链接与设备文件。** pack 的 URL 来自一个构建 flag 可以改写的变量（自部署要走镜像），所以那些字节不保证是我们的；一个叫 `../../etc/cron.d/x` 的成员就是全部攻击，一次 `filepath.Rel` 就堵上。单文件解压有 8 MiB 上限 —— 资源路径上的解压炸弹本不该能撑爆一台只是想装个软件的机器的磁盘。

**先失败，别先问八段。** lite 路径的第一版把八段问完、用 nil 种子写出一个空的 `models.yaml`（`LoadCatalog` 拒绝它，而重跑会因为它存在而**跳过**），然后才说自己没有资源 —— 一个在第一秒就有足够信息拒绝的命令，产出了一个永久损坏的部署。`Preflight()` 在第一个问题之前查。它落地当天就抓到一条真漏：`seed/oauth.yaml` 根本没写进 embed 清单。

## `GET /api/admin/health`：控制台被承诺过、但一直拿不到的那条信息（F5，2026-08-09）

`/api/healthz` 是未鉴权的，所以它**不能说任何有用的话** —— driver 错误 routinely 带 DSN，DSN routinely 带密码。它自己的注释写着「运维通过控制台看到原因」。

**而那句话在此之前对任何代码都不成立。** `DegradedReason()` 写好了、文档写着它是控制台的路径，而全仓只有一个调用方：`main.go` 里的一行日志。控制台够不着它。

三样东西只在这里有：

|字段|为什么|
|---|---|
|`reason`|真正的 driver 错误。存储坏了的时候部署里最有用的一个字符串，也正是公开端点必须不说的那一个|
|`uptimeSec` / `startedAt`|「是不是刚重启过」是运维区分「配置问题」与「崩溃循环」的方式，而此前这个代码库**没有任何地方记过启动时间**|
|`instance`|健康是每进程的，所以负载均衡后面的两个控制台**合理地**给出不同答案。没有名字挂上去，那就是一份没人能解释的 bug 报告|

**三种状态，不是两种**：降级（存储从来没打开过）、跑着但数据库够不着（`dbReachable:false`）、健康。中间那种是布尔表达不了的，而它恰恰是运维最可能正盯着的那一种。

⚠️ **三种都返回 200** —— 对本服务器的这次请求成功了并且查出了东西。503 会让控制台在一个「其实就是答案」的结果上盖一条「管理 API 坏了」。

⚠️ 降级分支**不报 `dbReachable`**（不是报 false）：根本没有数据库可以够，报 false 会暗示试过。

## 降级启动（θ-F4a 的另一半，2026-08-07）

**存储打不开不再带走进程。** 此前 `storage.Open` 或 `Migrate` 失败就 `return err`，在任何 supervisor 下都是一个**崩溃循环**：容器每几秒重启一次，唯一的证据是一行你得知道去哪找的日志，而**能让你修好配置的那个界面，正是由那个不断死掉的进程提供的**。

### 一次性，不可逆

降级是**启动期状态，进不去也出不来**。「盯着数据库、好了自己切回来」是另一套设计（请求正好卡在切换中间怎么办、几次失败算数、抖动怎么阻尼），而它的每一部分都是一种**间歇性地出错**的方式。而且一个发现数据库回来了的进程还得重跑迁移 —— 在活着的服务器上做这件事，恰恰是那种想要一个人在场的操作。所以：要么以降级启动，要么不；出去的办法是重启。

### 服务什么

| 路径 | 为什么能活 |
|---|---|
| `/api/healthz` | 必须答，而且答 **503** |
| `/api/version` | 契约协商不读任何行 |
| `/api/admin/**` | 控制台 —— **F4a 的鉴权路径就是为此设计成一处都不碰数据库的** |

`/api/admin` 底下大部分今天仍然会失败（提示词覆盖、统计、DB 浏览器都要读行），但它们**各自报自己的错，而不是一个笼统的 503**。运维站在一个已经加载出来的控制台前面，「这一屏需要数据库」比「API 挂了」有用得多。

### 中间件的位置是关键

`degradedMW` 在 `corsMW` 之后、**三个身份中间件之前**。没有 store 时，`requestLocale` 与会话查找会直接解引用 nil —— 而被 `recoverMW` 接住的 panic 是一个什么都不说的 500。早拒绝才能在每一条受影响的路径上给出同一个带理由的答案。

### healthz 是 **readiness** 不是 liveness

降级时答 **503**，好让负载均衡停止把用户流量送过来。⚠️ **把它接成 liveness 探针，编排器就会重启进程 —— 那正是降级启动要取代的崩溃循环。**

而且 **healthz 不带任何细节**：它是未认证端点，而驱动错误里常常带着 DSN，DSN 里常常带着密码。理由留给运维，从控制台走，不上线。

## 每会话地点：一条以沉默结尾的阶梯（F2-A，2026-08-08）

简报要查天气，而它**没有模型可问** —— 早晚简报走的是一次不带 Tools 的 Chat，模型连工具带都看不见。所以「地点」必须是存在会话上的东西。

|档|来源|
|---|---|
|1|**前端报的**（`POST /api/ai/companion` 的 `location`）|
|2|**用户记忆**里明确说过住哪（`locationFromFacts`）|
|3|**什么都没有 → 简报里没有天气这一行**|

### 第三档是答案，不是待填的坑

此前简报查的是**硬编码的北京**，给全世界每一个用户，旁边写着 `// future: session setting`。

**一个错的城市比没有城市更糟。**「17 度有雨」是一句有人照着穿衣服的话，每天早上安静地说错一次，会把简报另外四句话的可信度一起磨掉。而少一行的代价就是少一行。

这个判断也是第二档保守的原因。

### 第二档为什么是保守的匹配而不是聪明的解析

`MemoryFact` 是**一条自由文本**（没有 key/value），由模型用用户自己的话写下来。任何宽到能覆盖大多数说法的启发式，也就宽到能从「想去京都看樱花」里抠出一个「京都」—— 而那正是上面说的那种错法。

所以：只在**明确的标记词**（`我住在` / `现居` / `lives in` / `location:` …）后面取一段，遇标点即停，超过 64 字符判定为「这是一个碰巧含标记词的句子，不是地名」，直接落到第三档。

**想让它可靠，答案是设置页（第一档），不是更好的正则。**

### 前端提示永不覆盖用户自己选的

与时区同一条规则、同一个理由：有人特意把家乡城市设好（因为那是他安排一天所围绕的地方），不该在他第一次从火车上打开 app 时被改掉。

⚠️ 两者仍然是两个文件，因为有一处不对称：**改时区要重排 cron，改地点不用** —— 地点改的是简报说什么，不是它什么时候发。

### 为什么不是坐标、也不从时区推

- **不是坐标**：本仓每一个天气源都自己解析地名。在这里做地理编码等于自己养一个 geocoder，并且和源给出不同的答案。
- **不从时区推**：`Asia/Shanghai` 覆盖一个国家。

### 入口是对话，不是设置页

**设置页是没人会花的摩擦。** 而且这一项还是鸡生蛋：天气那一行要有地点才出现，所以没有任何东西会提示用户去设置里填它。

时区那半没有这个问题 —— 浏览器免费给（`Intl.DateTimeFormat().resolvedOptions().timeZone`，现役前端 [`store.js:21`](../web/frontend/src/store.js) 已经在每次请求里报了）。**地点没有对应的东西**：浏览器要么弹权限框给一对坐标，要么走第三方 IP 查询。前者意味着这里得自己养一个 geocoder，后者意味着把用户的事发给别人。

所以加了 `set_home_location` 工具：「我在上海」是人会主动说的话，这个工具让助手把它**正确地**记住（第一档、`source=user`），而不是指望第二档那个保守的正则去捞。

⚠️ **工具描述花了大半篇幅在防同一件事**：把「这周在东京」存成常住。

	存下来的地点   住哪 —— 简报用，而简报没有模型可问
	工具的参数     任何地方 —— 那趟旅行、下周的城市、朋友那儿

`get_weather` 的 location 是参数，所以问别处根本不需要存任何东西。**把出差目的地存成常住，用户接下来一个月每天早上都会收到错的城市的天气** —— 而他从没做过任何看起来像「设置常住地」的动作。

它写的是与设置页同一个字段、同一个 `source=user`（用户确实说了，只是说出口而不是填进表单），所以之后的前端提示覆盖不掉它 —— 这对「常住」是对的，也正是描述里那句警告存在的理由。

**撤销回「什么都没有」必须成立，且要连 source 一起清掉**。否则撤销之后用户被钉在一个他从没选过的值上、而字段声称是他选的，之后前端提示永远被拒。这是撤销最容易做错的一档，因为 `""` 很容易被当成「没记录 before」。

### 工具那条路不需要这些

`get_weather(location, …)` 的 location 是参数，模型**本来就能问任何地方** —— 正在商量的那趟旅行、下周要飞的城市。那与「这个人现在在哪」是两个问题，只有后者需要存在会话上。工具描述里明写了这一点，否则模型会以为它只能查用户所在地。

## 每会话时区（ζ-4，2026-08-06）

在这之前，服务端**每一个钟表**都读同一个部署级 `WORKER_DEFAULT_TZ`。对不在那个时区的用户，这不是个装饰性的偏移，它挪的是本该锚在「他自己那一天」上的东西：

| 被挪的 | 症状 |
|---|---|
| 石化线（`plan_guard.go`） | 他的傍晚提前或推迟冻结 |
| 节律 day key（`awake.go`） | 他的「一天」从别人的午夜开始，而 day 正是学习器的工作单位 |
| 早报时刻 | 07:30 到达，但是在一个他不住的城市的 07:30 |
| 每日场次 key | 他的某一天收到两次早报、另一天一次都没有 |

### 为什么值放在 preferences 而不是列

仓库规则是「出现在 `WHERE` 里、或被算术更新的字段必须是列」。时区按会话读、从不跨会话查询，所以 blob 是对的位置 —— 与 `PrimaryLocale` 同一个地方，同一个理由。

### 为什么允许客户端告诉我们（这不是「永不信任客户端上下文」的漏洞）

那条规则挡的是**服务端本来就能自己算出来的数据**（`todayPlan`、`moodHistory`、`date`）—— 接受它意味着客户端可以在用户自己的历史上撒谎。**时区服务端根本算不出来**：只有设备知道它在哪个时区。拒绝这个提示不会更安全，只会更错。

提示**不能**做的是覆盖用户自己的选择。所以是两个字段而不是一个：

```
Timezone        值
TimezoneSource  "user"（设置页）或 "detected"（客户端提示）
```

| 场景 | 结果 |
|---|---|
| 没有值 + 收到提示 | 采纳，标 `detected` |
| `detected` + 收到新提示 | 更新 —— 它本来就只是个猜测 |
| `user` + 收到提示 | **忽略**。否则一个特意把日程留在家乡时间的人，第一次在机场打开 app 就被悄悄挪走 |
| `user` + PATCH `""` | 清空，回到跟随设备 |

这套「谁决定的」与「是什么」分开记的做法，`MoodCheckin.Source` 与 `TimeBlock.LockSource` 已经在用，理由相同。

### AI 端点的钟表：提示是提示，不是答案（2026-09-18）

ζ-4 把时区做成了每会话的值，但 **AI 端点当时并没有读它**：companion handler 把请求体里的 `timezone` 直接喂给提示词构造器。客户端不送时（四端的前端就是这样 —— `askCompanion` 根本不带这个字段）`time.LoadLocation("")` 失败，而兜底写的是 `time.UTC`。

症状是所有者亲手碰到的：芝加哥 23:44 问「现在几点了」，陪伴回答「都四点四十了，你是还没睡？」—— 那是 UTC 的 04:44。

同一个错误还有一个反向症状：请求体里的值**被当成答案**，于是设备在机场报一个时区，就盖掉了用户在设置页特意留的家乡时间 —— 与上面那张表里 `user + 收到提示 → 忽略` 正好相反。两个症状是一个错（把「客户端送来的」当答案，而不是提示），所以用一个入口关掉：

```
aiClockTimezone(ctx, sid, clientHint)   // 先记提示（规则不变），再返回会话自己的值
```

每一条 AI 路径（companion、异步 companion、plan、auto-plan）都从这里取钟表，并且**不得**把请求字段再传给下游：传下去会让 `tool_plan.go` 把空值写成块时区 `"UTC"`、让 `tool_capture.go` 按 UTC 记心情的日键。

判据在 `internal/server/ai_clock_test.go`，断言的是**模型实际收到的请求体**而不是内部函数 —— 缺陷在接线处，只测 `sessionTimezone` 的测试对这个 bug 全程绿灯：

| 输入 | 模型必须被告知 |
|---|---|
| 会话记得 `America/Chicago`，客户端什么都不送 | 芝加哥的今天 |
| 会话没有任何时区，客户端什么都不送 | 部署默认（`WORKER_DEFAULT_TZ`），**不是** UTC |
| 客户端送 `Europe/Berlin` | 柏林，且这次之后会话也记住了 |
| 用户在设置页选了东京，客户端送柏林 | 东京（提示不许覆盖用户） |

⚠️ 客户端那个字段的**定位没变**，仍然是「只有设备知道」的那个提示；变的是**下游读谁**。新加 AI 端点照这条接：提示进 `aiClockTimezone`，时钟从它出来。

### 改了时区必须当场重排 cron

cron 条目里编着**旧时区**的 `CRON_TZ`。一个存了但 cron 不反映的时区是个没人读的值。所以 `PATCH /api/session/preferences` 与 `noteClientTimezone` 都会调 `ScheduleUser` —— 而它先摘旧条目再挂新的（ζ-1 那一批教会它的），否则同一份早报会按两个时区各发一次。

### `validTimezone` 的定位（别误解它）

`time.LoadLocation` 才是真正拒绝非法名字的那个。`validTimezone` 里的预筛是**一道上界**：不让一个来自请求体的无界字符串走到 tzdata 查找那一步。它是廉价保险，不是安全保证 —— 测试断言的是**结果**（这些都被拒），不是机制。

## 两层时区：会话时区 vs 事件时区（裁决 #10，2026-08-07）

ζ-4 解决了「用户现在在哪」。这一层解决的是**另一个问题**：「这个时间属于哪的墙钟」。

| | 是什么 | 存在哪 | 谁改 |
|---|---|---|---|
| **会话时区** | 用户**现在**在哪 | `SessionPrefs.Timezone` | 设备提示 / 设置页 |
| **事件时区** | 这条事件的时间属于**哪座城市的墙钟** | `ScheduleRule.Timezone` | ICS 导入（`X-WR-TIMEZONE`）/ 资料页改 |

留学生场景是这两层不能合并的证明：一个在伦敦的学生导入上海的课表，「09:00」的意思是 **09:00 中国时间**，不是 09:00 伦敦时间。把它锚到用户所在地，整个学期都会偏八小时。

### 顺带修掉的真 bug：floating 时间按**服务器**时区解析

`parseDateTime` 原来对没有 `Z` 也没有 `TZID` 的时间用 `time.Local` —— **服务器的时区**，它跟用户和课表都没有任何关系。一个跑在 UTC 容器里的部署和一台上海的笔记本，把同一个文件解析成相差八小时的时间，而且没有任何地方说过这件事。

`ics.Parse` 现在**必须**收一个 `*time.Location`（不是可选回退），且 `X-WR-TIMEZONE` 覆盖它 —— 文件自己说它的墙钟属于哪座城市，是比调用方任何猜测都好的证据。

### 什么时候问用户（只问一次，且只在答案会改变结果时）

三个条件同时成立才问：**文件说了时区** ∧ **那不是会话时区** ∧ **文件里有 floating 时间**（意思真的取决于这个答案）。

- 两者一致 → 没什么可决定的，静默导入。
- 每个时间都带 `Z` 或 `TZID` → 没有 floating 的东西要重新解释，问了也白问。
- `preview` **永不被拦** —— 那正是客户端拿来给用户看「你要决定的是什么」的东西。

问的方式是 `409 timezone_mismatch` + **什么都不导入**，客户端带 `tzConfirmed`（必要时连同 `timezone`）重发。为常见情况挡一个弹窗去保护罕见情况，是弹窗变成「一路点过去」的方式。

## 多实例：选主与场次占有（ζ-1，2026-08-06）

### 两个机制，各背一半承诺 —— 不要合并

| | 承诺什么 | 靠什么 |
|---|---|---|
| **`job_runs` 行** | **正确性**：「早报只发一次」 | `(session_id, job_name, run_key)` 唯一索引 —— 四个后端唯一共有的互斥手段（sqlstore 全包无事务） |
| **`leases` 行** | **节流**：N 个实例不必各自醒来、建上下文、调模型，然后 N−1 个发现自己白干 | 一条带条件的 UPDATE + TTL |

**正确性绝不能压在 lease 上**，因为 lease 压在时钟上，而不同机器的时钟不一致。凡是「两个实例都以为自己是 leader 就会出错」的事，必须由行来守。有一条测试专门制造这个分裂（`TestOccurrenceIsClaimedOnceEvenIfBothLead`：强行让 b 相信自己 lead，断言它仍然抢不到那个场次）。

### `Claim` 的位置是这套东西最容易写错的地方

**在抑制门之后、在真正干活之前。** 不是作业开头。

`JobRunRepository.Claim` 的注释自己写着理由：「Writing a row every half hour to record that nothing happened would bury the rows that mean something.」rolling replan 每天每会话触发 48 次、绝大多数次决定什么都不做；在开头 claim 会让这张表每天每会话多出 48 行「无事发生」，而它唯一的读者是一个人在问「我的早报到底发了没有」。

于是三处的落点各不相同：

| 作业 | claim 在哪一步之后 | run key |
|---|---|---|
| 早报 / 晚复盘 | 用户开关 + 免打扰之后 | `dayKey` = **用户本地日期** |
| deadline 提醒 | 算完发现 `len(urgent) > 0` 之后 | `slotKey(2h)` |
| rolling replan | 算完发现 `len(overdue) > 0` 之后 | `slotKey(30min)` |

**`dayKey` 必须是本地日期**：用 UTC 的话，靠近日界线的用户会在自己的某一天收到两次早报、另一天一次都收不到。

**`slotKey` 是纯截断，不留 grace 窗口。** robfig/cron 从 `time.Timer` 触发，Go 保证定时器不会提前，所以作业体里的 `time.Now()` 永远 ≥ 槽边界。加一个 grace 只会把被延迟的一跳推进**下一个槽**，抢走一个还没发生的场次，让真正的下一跳发现已占而跳过 —— 方向正好是反的。

### 三个分支必须保持三个

`renewWorkerLease` 的 `Acquire` 有三种结果，**合并后两种就是两个 leader 的做法**：

| 结果 | 动作 | 为什么 |
|---|---|---|
| `ok=true` | 延长持有；fence 变了就记一条交接日志 | — |
| `ok=false, err=nil` | **立刻**放弃 | 我们确知输了。这是 N−1 个实例的常态，所以是 Debug 不是 Warn |
| `err != nil` | **保持不动** | `LeaseRenewIn = LeaseTTL/3` 存在的全部理由。一次慢查询就交出租约，会把一个抖动的数据库变成 leader 来回跳，那比多当一个周期的陈旧 leader 更糟 |

### fence 用来做什么、不用来做什么

`Lease.Fence` 只在**交接**时 +1，续期不动 —— 于是一个停顿过久的持有者能分辨「还是我的」与「别人拿过之后又回到我这」。我们**记录它并在交接时打日志**。

**我们有意不拿它给写操作盖章**：场次行在接管时已经轮换了 claim id，僵尸迟到的 `Finish` 自然落空。在一个已经生效的守卫上再叠一个更弱的守卫，那是抄仪式不是防御。

### 这套东西不提供什么（说清楚，免得有人以为它提供）

**重试。** `JobMaxAttempts` / `JobMaxCrashAttempts` 描述的是 `Claim` **允许**什么，但**没有任何东西会去重驱一个失败的场次**：每个作业由恰好一次 cron 触发，到下一次触发时 run key 已经变了。一次失败的早报就是一个没有早报的早上 —— `domain/coordination.go` 的注释早就警告过这一点。补它需要一个扫描失败行并重驱的 sweeper，那是另一件事，且有它自己的失败模式（07:31 失败、09:00 重试，用户收到的是一个他没要过的时刻的早报）。

### 启动与关停的顺序（改之前先读这里）

**启动**：`StartWorkerLease()` 必须在 `worker.Start()` **之前** —— `LeadsWorker()` 在第一次续租落地前是 false，反过来会让第一分钟无保护地跑。`StartWorkerLease` 用的是 `everyTickNow`（先跑一次再进循环），否则每次重启后有 `LeaseRenewIn` = 20 秒没有 leader。

**关停**：`httpSrv.Shutdown` → **`StopTicks()`** → **`ReleaseWorkerLease()`** → `WaitBackground` → `worker.Stop()`。

⚠️ **`StopTicks` 必须在 `Release` 之前。** 反过来的话，还在跑的续租循环会把本进程刚释放的租约重新抢回来（`Release` 把 `expires_at` 置 0，正好命中 `Acquire` 的接管分支），fence+1、再持有一整个 TTL —— 下一个实例等的是一个已经退出的 leader。这个顺序错了不会有任何报错。

### 丢主时不做的两件事

- **不杀在途作业**：场次归属靠 `Claim` 不靠 lease，接管会轮换 claim 令牌，迟到的 `Finish` 自动落空。
- **不 `cron.Stop()`**：cron 是进程级的，Stop 之后条目全丢，而 `ScheduleUser` 只由 `markAwake` 与通道验证懒触发 —— 新 leader 要等每个用户各自再发一次请求才重新排上。作业体各自查 `LeadsWorker()` 就够了。

### 实例身份

`InstanceID()` = 主机名 + 进程内生成的 UUID，**绝不来自配置**。同名重启的 pod 否则会继承自己上一次的租约与占有，整套机制跨重启静默失效。

## 文件总线的 HTTP 面：取舍与边界（ε，2026-08-06）

存储侧在 [DATA.md「附件与文件总线」](DATA.md)。这里是端点这一层为什么长这样。

### 上传体是原始字节，不是 multipart，也不是 base64 JSON

| 方案 | 为什么不 |
|---|---|
| **base64 进 JSON** | 每个字节涨三分之一，两端都要把整份文件拿在内存里。文件总线的存在理由第一句就是「装不进 JSON 体的字节」，用 JSON 送它是自相矛盾 |
| **multipart/form-data** | 单文件场景下什么都没买到：这边多一个解析器，四个前端各多一段 FormData 代码 |
| **原始字节 + `Content-Type`**（选中） | `fetch(url, {method:'POST', headers:{'Content-Type': file.type}, body: file})` 就说完了；`blob.Store.Put` 本来就收 `io.Reader`，可以直接流进去不落内存 |

**文件名走 query 而不是 header**：非 ASCII 文件名放 header 需要客户端做 RFC 5987 编码，放 query 只需要 `encodeURIComponent`。

### 下载一律代理，永远不给签名 URL

`blob.Store` 有可选的 `URLSigner`（S3 这类能签，本机目录不能）。**端点仍然只代理**：让客户端处理两种形状，实际结果是它只会被测到作者部署的那一种，另一种在别人的部署上第一天就坏。签名是以后的优化，`GET /api/files/{id}` 是契约。

### 三个必须存在的响应头

| 头 | 少了它会怎样 |
|---|---|
| `X-Content-Type-Options: nosniff` | 浏览器嗅探内容类型，一个上传的 `.html` 变成同源页面 |
| `Content-Security-Policy: default-src 'none'; sandbox` | 上传的 `.svg` 打开就是**同源脚本**，能读走本站 cookie |
| `Content-Disposition`（经 `domain.SafeFilename` + `mime.FormatMediaType`） | 客户端送的文件名是唯一能从请求走进**响应头**的自由文本 —— CRLF 就是响应头注入 |

`ETag` 用内容 SHA-256，所以 `If-None-Match` 是精确的，可以配 `immutable` 长缓存。

### 状态码分工（客户端据此决定要不要重试）

| 码 | 含义 |
|---|---|
| `400` | 没有 `Content-Type`，或**零字节** —— 零字节永远是客户端 bug（空 File、读了一半），不是谁想留的文件。这时已落盘的字节会被删掉，不留一条解析成空洞的行 |
| `404` | 这个会话没有这个附件（含「是别人的」） |
| `409` | 它已经跟着消息发出去了 —— 删消息才能删它 |
| `410` | 行还在、字节没了（清扫做了一半、或者目录被还原时漏了文件）。与 404 分开是因为**客户端该不该继续问**不一样 |
| `413` | 超过 `MAX_UPLOAD_BYTES` |
| `503` | 这个部署没有 `BLOB_STORE`。**是受支持的配置，不是故障** |

### 两个上限是两个问题，不要合并

- `MAX_UPLOAD_BYTES`（默认 32 MiB）—— **放得下吗**。
- `MAX_IMAGE_BYTES`（默认 8 MiB）—— **塞得进一次模型请求吗**。

一份 30 MiB 的 PDF 是个正常上传、是个糟糕的提示词。合并成一个数，就只能取两者里小的那个，于是「能存但当前模型读不了」这个完全正常的状态变成了「传不上去」。超过内联上限的附件走「模型读不了」那条路：把文件名念给模型听。

### `features.files`

`GET /api/version` 的 `features` 里多一位，由**有没有配 `BLOB_STORE`** 决定（其余四位由模型目录派生）。前端据此决定画不画回形针 —— 否则用户点了得到 503，读起来像 bug。

## 四个后端的已知行为差异（调用方必须绕开的）

行为一致性套件把绝大多数差异变成了「不许不一样」。下面这些是**留在调用方这一侧**的，套件管不到，每一条都真的咬过人：

| 差异 | 后果 | 绕法 |
|---|---|---|
| **`ChatRepository.AppendMessages` 不把生成的 id 写回调用方切片**（mongostore 侧） | 依赖回读 id 的代码只在四个后端里的一个上失败，而且只在特定路径（ε：只在用户发了附件时绑不上） | 调用方**预生成 id**（`uuid.NewString()` 后放进 `ChatMessage.ID`）。async companion 早就这么做，注释里写着原因 |
| **空字符串 vs 字段缺失**（BSON `omitempty`） | `{"field": ""}` 在 Mongo 上匹配不到省略了该字段的文档，同一个谓词在 SQL 上匹配所有默认行 —— 清扫/筛选静默变成空操作 | 参与查询的字段**不加 `omitempty`**，显式存 `""`。见 `mongostore/attachment_repo.go` 的 doc |
| **JSON 数字回来的 Go 类型**（BSON int32 vs encoding/json float64） | `args["minutes"].(float64)` 在一个后端上断言失败 | proposals 的 rows/ops 两边都过 `encoding/json`，代价是两边同样有精度上限（>2^53 的整数不能进工具参数） |
| **nil 切片 vs 空切片** | 一边回 `nil` 一边回 `[]`，JSON 序列化出 `null` 与 `[]` 两种 | 套件已断言统一（`RoundTrip` 类用例），新 repository 照做 |
| **Mongo 的 sparse 唯一索引不跳过显式 `null`**（2026-08-10 发现，见下） | 写 `{"email": null}` 会让所有无邮箱用户挤进同一个索引键，第二个直接 E11000。SQL 的唯一索引允许多个 NULL，所以**只在一个后端上炸，而且只炸匿名用户** —— 也就是绝大多数用户 | 可空且被唯一索引覆盖的字段，nil 要 `$unset` 而不是 `$set` 成 null。见 `mongostore/user_repo.go` 的 `Upsert` |

**加一条差异时**：先问它能不能变成套件里的一条用例（那样它就消失了）；只有在 `domain.Store` 接口表达不了的时候，才记到这张表上。

## 中间件链（server.go 底部，全局单链，无分组）

```
recoverMW → requestIDMW → loggingMW → corsMW → sessionMW → userMW → dataSessionMW → mux
```

三个身份中间件是**「解析不强制」**：cookie/header 有效才往 ctx 注入，从不拦截。真正的鉴权在 handler 内 `requireSession`（无 sid → 401 no_session）。

**管理面不在这条链上**：`/api/admin/*` 的凭据与权限检查发生在**路由注册**时（`admin_gate.go` 的 `adminGate` 包住 `Mux`），不是中间件。原因是中间件拿不到「匹配到了哪条 pattern」，只能从 URL 反推 —— 那等于把 ServeMux 的优先级规则再实现一遍。详见 [AUTH.md](AUTH.md)「权限本体：落地形状」。

## 路由注册模式

**分散注册**：每个 handler 文件在自己的 `init()` 里 `registerRoutes("<组名>", func(s *Server, mux Mux){ … })`，`Handler()` 遍历注册表。注册表在 `internal/server/routes.go`。当前 **109 条路由 / 24 个组**，`server.go` 只剩静态 `/` 那一条（它有条件，只在 `STATIC_DIR` 存在时挂）。

原先是 `Handler()` 里 100 行集中注册，让 `server.go` 成了全仓最抢手的文件（12 个工作项都要改同一份清单）。**顺序无关紧要** —— Go 1.22 的 ServeMux 按 pattern 具体度而非注册顺序裁决，所以打散不会改变谁胜出，`/` 兜底也永远输给任何真路由。

`Mux` 是个只有 `HandleFunc` 的接口，不是 `*http.ServeMux`：注册打散之后就没有任何一处能读到完整 HTTP 面了，而 `*http.ServeMux` 无法枚举自己收了什么。换成接口，`RouteTable(*Server)` 就能拿一个记录器把注册重放一遍，把那份清单还回来 —— 传 `&Server{}` 即可，闭包只取方法值不调用，所以**读路由表不需要数据库**。

于此之上三条测试（`routes_test.go`）：pattern 不重复（ServeMux 撞了是 panic，但要等到起服务才炸）、每条都带方法、以及**与 `api/openapi.yaml` 双向核对** —— 服务了没写进契约、写进契约没人服务，两个方向都报错。这是实时文档铁律里「加了路由就改 openapi」那一条第一次真正由测试兜住（不是 CI —— CI 已删，这三条在 `go test ./...` 里）。

路由→handler 文件映射见 API_SURFACE.md，REST 细节以 `api/openapi.yaml` 为准。

## main.go 启动/关停

- 启动顺序：config.Load → store.Open+Migrate → catalog/prompts → server.New → locale 覆盖层 → 两个 cleanup ticker → **Worker（无条件启动）** → channels（仅当配了通道）→ inbound 消费循环。
- **后台 tick 循环现在停得下来**（`everyTick` / `everyTickNow` / `StopTicks`，2026-08-06）。原先它们连个停止信号都没有 —— 没有 ctx、没有 channel、没有返回值。**只做无状态清理时这是站得住的**（「进程退出即弃，无碍」），一旦某个 tick **持有**东西就不成立：ζ 要加的续租循环若在关停后继续跑，会把本进程刚释放的租约重新抢回来，下一个实例得等满一个 TTL 才等到一个已经退出的 leader。`StopTicks` 在 `httpSrv.Shutdown` 之后、`WaitBackground` 之前调用，等在途的那一跳跑完。`everyTickNow` 是「先跑一次再进循环」的变体：ticker 的第一跳在一整个 interval 之后，这对清理是对的（新进程没有陈旧数据），对**建立状态**的循环是错的。
- 每个会话现在挂 **6 条** cron：早报（学到的起床时刻）/ 晚复盘（睡前 90 分钟）/ 节律学习（日切）/ Protector（半小时）/ deadline（2 小时）/ rolling replan（半小时）。
- **`ScheduleUser` 是幂等的**（2026-08-06 修好；此前不是）。守卫读 `w.jobs[sid+":"+tz]`，而四处写的是带 `:morning`/`:evening`/`:deadline`/`:replan` 后缀的键 —— **那个键从来没被写过，守卫恒假**。`markAwake` 每会话每 5 分钟放行一次并调它，于是连续活跃一小时就攒下 12 套 cron 条目：下一个半点 `checkRollingReplan` 跑 12 遍，12 次模型调用、12 次推送。这不是多实例问题，是**单实例今天就在犯**的。改成按 sid 存一组 EntryID + 记住排给它的时区；换时区先摘旧条目（否则同一份早报按两个时区各发一次）。
- **未知时区回退而不是部分失败**（同批）。`CRON_TZ=<bad>` 会让早报与晚复盘两条 `AddFunc` 报错，而 deadline / replan 两条没有 CRON_TZ 前缀、照样注册成功 —— 于是会话被记成「已排程」，那个用户**从此再也收不到简报，也没有任何东西会重试**。现在先 `resolveTZ`：坏时区退到 `WORKER_DEFAULT_TZ` 再退到 UTC，记一条 warn。与 `markAwake` 记节律信号是同一个取舍 —— 时刻错了看得见，压根不发看不见。
- **Worker 不再绑在 OneBot 上**（2026-07-29）。它曾经只在 `ONEBOT_WS_URL` 非空时启动，于是没绑 QQ 的用户拿不到早报、晚复盘、定时 auto-plan、deadline 巡检 —— 产品「主动」那一半被一个无关设置整体关掉。Worker 做的事没有一件需要通道：产出全部落库、由 App 读，推到通道只是可选的最后一步（`sendToChannels` 在 registry 为 nil 时只记日志，那个分支本来就有）。
- **按用户排程改成首次请求时懒排**（`Server.SetScheduleOnUse`，由 `markAwake` 在节流放行时调一次，`ScheduleUser` 本身幂等）。原来的种子是「每个已验证的通道绑定」——同一个耦合的第二处。改用枚举会话则需要给 `SessionRepository` 加方法、四个后端各实现一遍，而且会给每个曾经调过一次 API 的人都挂上 cron 项。⚠️ 代价是**重启当天早上有个缺口**：那天还没发过请求的人没有 cron 项，07:00 重启后 07:30 的早报对他不触发。补它需要正是这里在回避的会话枚举，且必须与 Lease 选主同批（否则每个实例都会给每个用户排程）—— 批次 ζ。
- inbound 消费：每消息经 `srv.GoTracked` 起 goroutine（背景 ctx，不绑请求），纳入 `Server.asyncWG`。
- 启动清扫：Migrate 后 `store.Chats().FailPendingMessages` 把崩溃遗留的 pending 占位消息标为 error。
- 关停：SIGINT/SIGTERM → `httpSrv.Shutdown(15s)` 等 in-flight HTTP → `srv.WaitBackground(shutdownCtx)` 等后台 agent（异步聊天/通道回复 + 节律信号写入）→ `worker.Stop()`（等 `cron.Stop().Done()`）→ `cancelRoot()`。TempContext/ChannelBinding 两个 cleanup ticker 仍是自由 goroutine（无状态，进程退出即弃，无碍）。
- **两个 cleanup ticker 现在逐 tick 收 panic**（`Server.everyTick`，2026-07-29）。它们原本是裸 `go func(){ for range ticker.C {…} }`，而 `recoverMW` 只包 HTTP handler —— 自由 goroutine 上的 panic 会带走整个进程。一次 tick panic 只杀那一次：清理失败是几行陈旧数据，清理循环停掉是无界增长。这也是降级启动的前置（store 为桩时不会 panic，但 nil store 会，症状是「启动成功、一分钟后无声死掉」）。

## 文件总线（`internal/blob`，2026-08-01 落地本机磁盘）

Daycore 此前**没有地方放一个字节**：上传 ≤1 MiB 的被 base64 塞进 `temp_contexts` 一小时 TTL，更大的字节直接丢；图片只能以 base64 内联在请求里到达模型；`Material.StorageRef` 是三方言建了列、Mongo 也持久化、openapi 也发布，却**没有任何代码生产或解析过一个 ref** 的空壳。四个功能在等同一样东西 —— 图片上传、PDF、文生图、给适配层子进程递产物。

**注册表形状照抄 `internal/storage`**：驱动名选实现，驱动 `init()` 自注册，`main.go` blank import。加一个后端是一个包加一次 `Register` —— 与数据库层同一个交易，理由也一样（没人该为了指向自己的对象存储而 fork 这个仓库）。计划中的驱动：本机 / 从机 / S3 / OSS / COS / 七牛 / 又拍 / OBS / KS3 / OneDrive / DB blob 列，外加 `http` 与 `exec` 两个通用兜底。

**`Store` 不是鉴权**。它把 id 映射到字节，对会话一无所知 —— 与参考 MCP 那套「文件总线是哑的、编排器决定谁能读」同构。发 ref 的人负责决定谁能用它。**也不是数据库**：没有列举、没有查询、没有事务，一个只会 PUT/GET 的后端是合法实现，这正是「指向 S3、或一个目录、或一个列」能成为真选项而不是口号的原因。

`URLSigner` 是**可选**接口：对象存储都能签，而签不了的两个（本地目录、数据库列）恰好是代理转发很便宜的那两个。所以调用方永远要有代理路径，把签名当优化。

**`DATA_DIR` 是仓库的第一个可写路径。** 此前每个目录配置（`STATIC_DIR`/`LOCALES_DIR`/`PROMPTS_DIR`/`MODELS_CONFIG`）都是只读输入，`internal/` 里没有一处 `os.WriteFile`。`BLOB_STORE` 为空 = 没有文件总线，这是**受支持的配置**：需要它的功能各自检查并说明，而不是让进程为一个多数部署第一天用不到的能力拒绝启动。

**ε 批次给了它第一个真正的使用者**：`POST /api/files` 存字节、`attachments` 表存所有权、`GET /api/files/{id}` 按会话解析。在那之前 `blob.Store` 是写完、跑过 11 例行为套件、接进 `Server` 却零调用方的一层。分工没变 —— 总线仍然只认 ref 不认会话，鉴权全在 `attachments` 行上，见 DATA.md。

本机驱动的三个要点：**先写临时文件再 rename**（同目录内 rename 是原子的，读者永不会看到半个 blob，崩溃留下的是游离临时文件而不是看起来像数据的截断文件）；**rename 前 sync**（否则崩溃可能留下一个名字对、内容空的文件，而那正是这套动作要防的）；**ref 必须是本 store 铸的 64 位十六进制**，因为 ref 来自外部（数据库列、工具参数），把攻击者选的字符串拼到根目录上正是文件总线变成任意文件读的方式。

**行为套件先于第二个驱动存在**（`internal/blob/blobtest`，11 例）。存储层是用昂贵的方式学到这一课的：两个后端上线、没有任何测试断言它们行为相同，事后审查找出约十五处分歧。两两分歧数随后端数平方增长，而上面那张驱动清单很长。

其中一条值得单独说：**相同内容必须产生不同的 ref**。内容寻址会让两个用户的相同上传共享存储，然后一个人的删除会拿走另一个人的文件 —— 或者更糟，用户能探测某个文件是否已存在。去重不值这个代价。

## 存储不可用时的降级启动（2026-07-29 定案，2026-08-07 落地）

> 下面保留的是**当初的推演**，因为里面每一条「会踩的坑」都在实现时真的踩到了，而实现之后的代码看不出这些是有意为之。落地事实见 `internal/server/degraded.go`、`cmd/daycore/main.go` 与 [`AUTH.md`](AUTH.md)；本节末尾两条已按落地后的事实改写。

**决定**：存储不可用**不拒绝启动**。CLI 把话说明白，HTTP 照常起来只服务控制台与健康面，需要库的端点给统一的不可用信封。协议侧的措辞与理由在 [`docs/specs/transport.md`](specs/transport.md#存储型cli-说清楚--web-ui-仍然能上)，这里记落地事实。

**现状是一条全或无的直线**：`main()` 只做 `run(logger)`，失败就一行 JSON 日志 + `os.Exit(1)`（`main.go:51-54`）；`run` 里每一步都是 `return fmt.Errorf(...)`。`storage.Open` 内部真的 `Ping`（`sqlstore/store.go:52`、`mongostore/store.go:41`），所以「库没起来」在 `main.go:75` 就退出了，连 `Migrate` 都到不了。

**HTTP 层其实早就写好了答案，只是到不了**：`GET /api/healthz` 在 Ping 失败时返 503 `{"ok":false,"db":…,"error":"database unavailable"}`（`handlers_misc.go:21-27`）—— 进程既然拒绝启动，这段分支**永远执行不到**。所以这条改动是让代码自洽。

### 启动路径上真正的硬依赖只有五处

| `main.go` | 是什么 | 降级时怎么办 |
|---|---|---|
| `75-78` | `storage.Open`（含 Ping） | 不 return，换成 null store |
| `81-83` | `store.Migrate` | 不 return，但**要更响** —— 表结构半成品比连不上更危险 |
| `93-95` | `FailPendingMessages` | 已经是「失败也继续」（`err != nil` 时既不 return 也不 log —— 顺带记：这个静默吞错本身是缺陷） |
| `107` | `ai.NewPromptService(store.Prompts())` | `prompts.go:144-146` **已有 nil-repo 降级语义**（读走内嵌、写报错），现成 |
| `141-180` | channels 块（含 `ListAllVerified`） | 整块跳过 |

`server.New` / `weather.New` / `search.NewMaterialSearcher` / `ai.LoadCatalog` / `auth.LoadOAuthProviders` **全都不碰库**；路由注册也不需要 store（`routes.go` 的 `RouteTable` 用零值 `Server` 就能跑）。所以 HTTP 面本身没有障碍。

### ⚠️ 两个会让「起来一分钟后自己死」的 goroutine

`main.go:135-136` 无条件启动的两个清理 ticker（`StartTempContextCleanup` / `StartChannelBindingCleanup`）**内部没有 recover**。store 为 nil 时它们在第一次 tick（1 分钟 / 10 分钟）就 nil-panic，**把整个进程带走** —— 而 `recoverMW` 只包 HTTP handler，管不到自由 goroutine。表现是「启动成功、一分钟后无声无息地死」，是这次改动里最容易踩且最难查的一个坑。

### null store 胜过 nil store

中间件链里只有 `userMW` 碰库（`middleware.go:138`），而且只在请求带**签名有效的 JWT** 时才碰。听起来无害 —— 但后果是：**浏览器只要还带着一个有效的 `dc_auth` cookie，连 `GET /` 的 index.html 都会 panic**，被 `recoverMW` 变成 JSON 500，用户看到白屏。「控制台能上」当场落空。

所以传一个所有方法都返回 `domain.ErrUnavailable` 的桩，而不是 nil：

- **不漏**。nil 方案要逐个 handler 检查（100 条路由），漏一个就是一次 panic；桩方案是「默认安全」。
- 用 `RouteTable()` 在中间件层按路由分类挡掉也不行 —— 那张分类表得手工维护、必然漂移，而且 `userMW` 跑在路由裁决**之前**，它挡不住。
- 代价是约 115 个方法的样板。生成它；`storagetest` 可以顺手断言它从不返回 `nil, nil`。

### CLI 提示与 i18n

运行期现在只有 slog JSON（`main.go:50`），失败就是 `{"level":"ERROR","msg":"fatal","err":"open db (sqlite): …"}` —— **仓库里没有任何「给人看的启动错误」先例**。唯一漂亮的 CLI 是 `daycore install` 子命令（`install.go:28-31` 的 heading/done/hint/prompt + ANSI 色），直接复用。

**这会是第一条需要本地化的启动期文案，而它恰好可行**：`config.Load` 在打开存储**之前**就已经 `i18n.Std().LoadDir(LOCALES_DIR)`（`config.go:193-205`），所以那一刻文件层与内嵌层都在，只有 DB 覆盖层没有 —— 这正是「内嵌是地板」当初要覆盖的场景。

### 安全：「`ADMIN_TOKEN` 未设 = dev 全开」这一档已经删掉了

当初的隐患是 `adminAuthorized` 在 `ADMIN_TOKEN` 为空时返回 `!IsProduction()` —— 降级模式下 DB 支撑的会话全没了，管理面就是**唯一**的门，于是一个 `APP_ENV != production` 的部署，存储一挂就变成挂在网上的**无鉴权配置界面**。

**已消除，而且是从源头消除的**：`config.Load` 在未设时**自动生成一个并打印**，所以「没有配置凭据」不再是一个可达状态；鉴权那一侧没有任何「全开」分支（见 [`AUTH.md`](AUTH.md)）。两件事必须同批设计的价值在这里兑现 —— 降级模式下控制台登录仍然能用，**正是因为鉴权那一半是按「一处都不碰数据库」做的**。

⚠️ 由此得到降级模式的一条硬约束：**除 root 凭据外，降级进程里没有任何权限能通过**。角色是从库里读的，库没了就读不到 —— 这不是缺陷而是这套设计的形状：破窗轨存在的理由就是「别的全没了的时候」。

### 控制台：从「还没有可上的」到内嵌进二进制

当初这一条是阻塞项（web UI 只有 `design-ui/liuli/admin/` 的纯 mock 原型，`STATIC_DIR` 里没有可上的控制台，三个无 DB 仍有意义的分区后端端点一个都不存在）。现在：

- 控制台在 `web/console/`，构建产物进 `internal/resources/data/console`，由二进制自己在 `/admin` 托管（见「控制台为什么内嵌」一节）。**「下一个二进制、跑起来、开 /admin」在最需要它的那种情况下成立。**
- 服务配置 / 能力源两屏有真后端；模型与 OAuth 是只读屏；用户与权限屏随权限本体落地。
- 无库时改配置仍然要写 `.env` + 重启 —— `POST /api/admin/restart` 尚未实现（见 [ROADMAP](ROADMAP.md)）。这是这条链上**唯一还欠的一块**。

## 数据库浏览器：唯一一处标识符进查询的地方（2026-08-10）

控制台的「数据库」屏要回答「这里面有什么、有多少」，而这必然意味着一个来自 URL 的表名要变成一次查询。全仓其它每一条 SQL 的表名都是字面量；这里是唯一的例外，所以它只有一扇门：

```
domain.TableByName(name)  → 命中一条目录项，或者到此为止
t.Name / t.OrderBy / …    → 我们自己的常量
s.d.Quote(…)              → 方言自己的引号
```

**调用方给的那个字符串永远不是进查询的那个字符串**，哪怕两者相等。这在写下它的那一行看起来像迷信，其实不是：下一个在这里加过滤条件的人会照着看到的形状抄，而看到的形状必须是安全的那个。

- **边界**：**没有调用方可以指定的列、排序方向或谓词**。真要加，先加进目录。也因此这一屏**没有搜索、没有排序、没有过滤** —— 每一样都会把这道门捅开。
- **取舍**：目录是手写的 37 条，不是运行时反射。四个引擎回答「你有哪些表」的方式各不相同，而且**没有一个带得上真正要紧的那两件事**：哪些行是别人的日记、哪一列不能出进程。手写的代价是会漂移，用 `TestEveryTableIsCatalogued`（走 DDL 双向比对）+ `TestCataloguedColumnsExist`（拿真引擎逐表 `SELECT … LIMIT 0`）顶住。
- **它防的是哪个具体失败**：① 一个 `Redact: "password_hash"` 在列改名之后**不报错，只是不再 redact 了**，然后浏览器开始把哈希发出去；② 一张新表没人分类 → 浏览器里看不见 → 它长到磁盘满那天才有人发现；③ `roles` 从这里被删掉定义而留下成员行 → 重名的新组静默恢复一批人的权限。

详见 [DATA.md](DATA.md)「数据库浏览器」。⚠️ `Browser` 挂在 `domain.Store` 上但**不是 repository** —— 其它每一个接口都在建模领域概念并隐藏 schema，这一个故意暴露 schema。**除 admin handler 外任何东西都不许调它**：想要行的功能要的是一个 repository 方法。

## 静态托管（static.go）

- `STATIC_DIR`（默认 `web/frontend/dist`）非空且有 index.html 时：`/api/` 前缀永不被静态遮蔽；`/assets/` immutable 长缓存；其余 SPA fallback 回 index.html（no-cache）。
- `STATIC_DIR=""` 或无构建产物 → **纯 API 模式**（不挂 `/` 路由）。`deploy/Dockerfile` 不 COPY 前端产物，本就是纯 API 镜像。

## 配置（internal/config/config.go，环境变量）

关键项：`APP_ENV`/`HOST`/`PORT`；`STATIC_DIR`；`ALLOWED_ORIGINS`（CSV，空=同源）；`DB_TYPE`(sqlite)/`DB_DSN`；`JWT_SECRET`/`COOKIE_SECRET`（prod 缺失报错，dev 回退不安全默认）；`JWT_TTL`(168h)；`SECURE_COOKIES`（prod 自动 true）；`AI_REQUEST_TIMEOUT`(120s)；`AI_RATE_LIMIT_PER_MIN`(30)/`AUTH_RATE_LIMIT_PER_MIN`(10)；`TRUST_PROXY_HEADERS`（只在可信反代后设 true，否则 XFF 可伪造绕过按 IP 限流）；`AGENT_MAX_ROUNDS`(6)；`MAX_IMAGE_BYTES`(8MiB，模型请求里内联附件的上限)/**`MAX_UPLOAD_BYTES`**(32MiB，`POST /api/files` 的上限 —— 与前者是两个问题：一个是「塞得进一次模型请求吗」，一个是「放得下吗」)；`ADMIN_TOKEN`；`ONEBOT_WS_URL`/`ONEBOT_TOKEN`；`MODELS_CONFIG`/`OAUTH_CONFIG`；**`BLOB_STORE`**（文件总线驱动名，空=没有）/**`DATA_DIR`**（唯一的可写路径，`local` 驱动用）；**`PROMPTS_DIR`**（空=只用内嵌；设了则 `<dir>/<locale>/<key>.tmpl` 逐文件覆盖内嵌模板，**且 `<dir>/boundaries.json` 覆盖 L1 硬边界 —— 那一份没有 DB 层，是控制台唯一够不着的提示词**，见 AI.md）；天气三项；`COOKIE_SAMESITE`（lax|strict|none，none 需 Secure）；**`DEFAULT_PRIMARY_LOCALE`/`DEFAULT_SECONDARY_LOCALE`/`LOCALES_DIR`**（前两个是**新用户的默认**一主一副，不限制用户能选什么，`Load()` 里 `i18n.NewPair` 校验、值不对启动失败；`LOCALES_DIR` 放 `<locale>.json` 语言包 —— 加语言不用重新编译，见 DATA.md「多语言机制」）。

## 仓库级布局

- `api/` = 契约唯一权威（openapi.yaml + FRONTEND_HANDOFF.md）。**`openapi.yaml` 是生成物**，源在 `api/spec/`（`head.yaml` + `paths/<tag>.yaml` 一个 tag 一个文件 + `components.yaml`），`make api-bundle` 拼回同一个路径 —— 下游生成器不受影响。理由与三条强制规则见 `api/spec/README.md`；`go test ./...` 会因产物过期而红。
- `docs/` = 实时项目文档，随代码同批更新；`EXPERIENCE_CORE.md` 是端无关语义总纲（v2.3）。
- `extension/` = Chrome MV3 插件（Canvas 抓取 → 导入）。
- `design-ui/` = 设计原型，只读参考不参与构建。四端源码（liuli / zhiyu / ting / liuli-classic）+ `core/daycore-core.js` 共享 mock + `HANDOFF/` 交接文档 + `_ds/` 设计系统原件。其 `API_CONTRACT.md` 的路径命名非权威（见该目录 CLAUDE.md 顶部裁决）。
- `web/frontend/` = 现役前端，将来被四端替换。
- 设计系统已 vendor 进 `web/frontend/src/ds/` 与 `src/vendor/ds-bundle.js`；原件在 `design-ui/_ds/`。

## 目标架构：前后端分离（进行中）

后端收敛为纯 API 服务，四个前端各自独立部署、做成 git 子仓库，只靠 **API 契约 + 版本协商**耦合：

- **后端报告**：`GET /api/version` → `{apiVersion, apiMinor, minClient, build, channel}`
- **前端声明**：各子仓 `package.json` 声明最低支持的 `apiVersion`/`apiMinor`，启动握手不满足则降级提示（不白屏 —— 语气铁律「给死路一条岔路」）
- **升版规则**：breaking 升 `APIVersion`，additive（新端点/新字段）升 `APIMinor`。整批工作统一升一次，不要每个改动各升各的。

版本三层的区分见根 `CLAUDE.md`「目标架构」一节。

## 配置分层（现状与待改造，2026-07-26 核实）

配置目前分三处，**问题是「能不能热改」没有被设计过**：

| 层 | 载体 | 生效方式 | 现状 |
|---|---|---|---|
| 启动期 | 25 个环境变量（`internal/config/config.go`） | 改了要重启 | 合理的部分：密钥、`DB_DSN`、`HOST`/`PORT` |
| 同上 | 但也塞了本该热改的：`WEATHER_PROVIDER`、`TAVILY_API_KEY`、`ONEBOT_WS_URL`、各阈值 | 改了要重启 | **错位** —— 运维控制台永远配不到 |
| 数据驱动 | `config/models.yaml`、`config/oauth.yaml` | 重启加载 | 方向对，但无端点、多实例不同步 |
| 运行时覆盖 | `prompt_overrides` 表 + `PUT /api/admin/prompts/{key}` | 立即生效 | **唯一做对的范式**，模型/OAuth/服务配置应照抄 |

两处 registry 不一致，也在待改造之列：

- `internal/weather` —— `Register(name, Factory)` + 四个 provider 子包 init 自注册 + `WEATHER_PROVIDER` 选择。**这是正确形状**。
- `internal/search` —— 硬编码 `if TavilyKey != "" { Tavily } else { DuckDuckGo }`，**无注册表**，物理上无法增删搜索通道。
- `internal/channels` —— 有 `Registry`，但 `cmd/daycore/main.go:141` 是 OneBot 硬编码单例，且**拿 `ONEBOT_WS_URL` 是否为空来决定要不要启动 Worker** —— 后果是没绑通道的用户，节律学习、定时 auto-plan、20h 关怀全都不跑。这是既有缺陷，落地节律后会非常显眼。

运维控制台**留在主仓库不做独立子仓库**（它与后端版本强绑定，同仓同版本发布最省事；只有四个用户端前端需要独立的版本节奏）。

✅ **上面这段「端点全部不存在」已经过时**（2026-08-10）：`web/console` 已实现，十一个分区全部有真后端，见 [ROADMAP.md](ROADMAP.md) F5。分区数比原型的 8 个多出三个，因为后来的批次自己长出了需求 —— 集群配对、前端 family、能力源。

## 目标：外部能力走 HTTP 适配器，不再改代码

**接一个新的搜索源／天气源／消息通道，不应该需要改 Go 代码或写插件** —— 只应该需要写一个 HTTP 适配层，它可以独立部署、放在别的服务器、独立扩缩容。

设计沿用 `config/models.yaml` 已验证的模式（`format` 绑定 registry 里注册的实现，`base_url` 指外部服务），推广到 weather / search / channels：内置 provider 保持 Go 实现走各自 format，**外部适配器统一走 `format: http`**。对上层透明 —— `WeatherProvider` / `Searcher` / `Channel` 三个接口一行不用改。

两类协议形状不同，**载体也不同**：

**查询型（天气、搜索）= HTTP** —— 无状态请求／响应，天然可并发，WS 反而累赘：
```
GET  /manifest   → {name, displayName, logo, type:"query", capabilities:["weather"|"search"]}
POST /weather    {lat, lon, date, tz}   → {temp, condition, …}
POST /search     {query, limit, locale} → {results:[{title, url, snippet}]}
```

**通道型（QQ/napcat 等）= WebSocket** —— 双向、有状态、需要身份。只在真的收发消息时才有流量。帧协议：
```
后端 →  {"t":"hello", "protocol":1}
适配器 → {"t":"manifest", "name","displayName","logo","features":{attachments,markdown}}
适配器 → {"t":"inbound", "externalUserId","externalName","avatar","messageId","text","attachments","ts"}
后端 →  {"t":"send", "to":"<externalUserId>","text","replyTo":"<messageId>"}
后端 →  {"t":"ping"}   // 断线重连沿用 onebot 现有逻辑
```

**连接方向必须两种都支持** —— 取决于谁在 NAT 后面，协议不该替部署做决定：

| `mode` | 谁连谁 | 适用 |
|---|---|---|
| `dial` | 后端作 client 连出去（适配器当 server） | 适配器有可达地址。**现有 onebot 就是这种**（`websocket.DefaultDialer.DialContext`） |
| `listen` | 后端开 WS 端点，等适配器连进来 | 适配器在 NAT／内网后面，后端有公网地址 |

OneBot 11 本身就规定了三种接入方式，现有实现只覆盖第一种，都要补：

| `providers.yaml` 的 mode | OneBot 官方术语 | 现状 |
|---|---|---|
| `dial` | 正向 WebSocket（napcat 当 server） | ✅ 已实现 |
| `listen` | 反向 WebSocket（napcat 主动连后端） | ⬜ 待补 |
| `http` | HTTP 上报 + HTTP API 调用 | ⬜ 待补 |

`internal/channels` 的 `Channel` 接口（`Send(externalID, msg)` + `Start(inbound chan<-)` + `Stop()`）对三种模式**都适用** —— `Start` 里是拨出去还是挂个 handler 等连接，是实现细节。**零接口变更**。

`manifest` 必须自带 `displayName` 与 `logo` —— 控制台与前端据此渲染，**接一个新通道零前端改动**。

### 内置 vs 外部：写了的直接配置，没写的走转换层

| 能力 | 内置（Go 实现，直接配置） | 外部（写适配层） |
|---|---|---|
| 天气 | openmeteo / qweather / openweathermap / wttrin（已有四个） | `format: http` |
| 搜索 | tavily（有 key）+ DuckDuckGo（免费兜底） | `format: http` |
| 通道 | napcat/OneBot（已有）+ 后续常见可接 bot 的平台 | `format: ws` |

### 搜索有三层来源，第一层目前是死代码

1. **模型厂商原生搜索** —— 走 **Anthropic 的 web search server tool 规范**（DeepSeek 的 Anthropic 兼容端点即此；kimi 等同类可比照）。**结果不是「融进回答的黑盒」** —— 规范返回的是 `web_search_tool_result` 块，其 `.content` 是一个 **`web_search_result` 结构化列表**（带 citations），后端拿得到、可落库、可展示来源。

   ⚠️ **现状：完全未接线（2026-07-26 核实）**。`config/models.yaml` 的 `deepseek_search: true` 是死配置：
   - `Capabilities.DeepseekSearch` 只被写入（`internal/ai/models.go:86`），**没有任何消费方**
   - `ToolDef.ServerSide`（`internal/ai/provider.go:44`）**全仓零赋值**
   - `internal/ai/formats/anthropic/anthropic.go:106` 序列化 tools 时只输出 `{Name, Description, InputSchema}`，**没有 type-based server tool 分支**
   - 同文件只解析 `text` / `tool_use` / `content_block_delta` / `message_stop` 四种块，**不认识 `server_tool_use` 与 `web_search_tool_result`**

   要接通需要两件事：发请求时按 `{"type": "web_search_20250305", "name": "web_search", "max_uses": N}` 序列化，收响应时解析 `web_search_tool_result` 并把 `.content` 映射成 `SearchResult`。

2. **内置工具搜索** —— agent 调 `web_search` 工具 → `Searcher` 接口 → tavily / DuckDuckGo。已实现。
3. **外部适配器** —— 协议转换层，`format: http`。

三层的**执行位置不同，结果模型应当统一**：第 1 层由模型服务端执行，第 2、3 层由后端执行，但都产出「标题 + URL + 摘要」的列表，都该落到同一个 `SearchResult`，这样账本、引用展示、前端渲染只有一套。

### 厂商原生搜索有两种形状，配置入口不同

| 形状 | 机制 | 配置放哪 |
|---|---|---|
| **返回独立结果** | server tool 规范，响应里带结构化列表（Anthropic `web_search_tool_result` 即此，DeepSeek 走它） | **搜索配置** —— 要注册工具、要解析结果、要映射成 `SearchResult` |
| **直接嵌入提示词** | 没有结构化返回，靠提示词引导模型自己去搜并在回答里带上 | **模型配置** —— 本质是一段随模型走的提示词片段 |

模型有没有原生搜索、是哪种形状，由 `models.yaml` 的能力声明表达（把现有的 `deepseek_search` 布尔泛化成 provider 无关的 `server_search`）。

### 多个搜索源并存：把选择权交给模型

不做「后端挑一个搜索源」的硬路由，而是**把多个搜索源各自注册成工具**，每个带自己的描述，模型按当前问题自己选（这与体验内核共识 5「不设关键词硬规则、错了再改」同构）。描述通过提示词模板动态注入：

```gotemplate
{{if .Searches}}
## 可用的搜索来源
{{range .Searches}}- `{{.ToolName}}`：{{.Description}}
{{end}}{{end}}
```

## 配置驱动的提示词片段：哪里用得到，哪里就能改

**提示词片段应该跟着配置走，就近编辑，而不是全堆在 Prompt 管理页。** 一个搜索源该怎么描述给模型、一个消息通道有什么特性（QQ 不支持 markdown、回复要短），都是那个 provider 自己的属性 —— 编辑入口就应该在它自己的配置卡片旁边，而不是让人去 Prompt 页翻一个巨大的模板。

现有基建已经够用，不需要新机制：

- `internal/ai/prompts.go` 用 `text/template`，`Render(ctx, key, locale, data any)` 接任意数据 —— `{{if}}` / `{{range}}` 天然可用，且现有模板里已有 5 处 `{{if}}` 先例。
- `PromptService` 有 `Validate`（parse 校验），控制台保存模板时能挡住语法错误。
- `prompt_overrides` 表 + `PUT /api/admin/prompts/{key}` 已是「文件作种子 + DB 存覆盖 + 立即生效」的正确范式，provider 描述照搬。

落地形状：每个 provider 在 `providers.yaml` 里带一段 `description`（**zh-CN / en-US 双份 —— 提示词双 locale 是启动期硬校验**），控制台在该 provider 的配置卡片旁给「编辑描述」入口，改动落 DB 覆盖层，`companion_agent.tmpl` 用 `{{range}}` 消费。同一套机制覆盖搜索源、消息通道、天气源。

⚠️ `companion_agent.tmpl` 目前是**零插值**的纯规则清单（有意为之，见 `AGENTS.md`）。引入 `{{range}}` 是对它的第一次结构性改动，两个 locale 必须同批改。

落地计划见批次 F。
