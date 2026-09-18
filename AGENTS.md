# Daycore — 给 AI 助手的仓库指南

> 本文件是对仓库的「指路 + 铁律」：先读它确定去哪、别踩什么，再按「文档地图」进 `docs/` 读细节。`docs/` 是**实时项目文档**（这个仓库现在是什么样），`docs/specs/` 是**对外协议**（别人要照着实现什么）。本文件 2026-08-01 全面核对重写。

## 这是什么

Daycore 是一个面向学生的「AI 自主规划 + 温和陪伴」应用（v2 beta）：把 Canvas 作业成绩、课程表、长期习惯交给它，每天一键生成合理的日程（`keep_manual` 保护手动改过的块），再通过聊天随口微调。功能面：重复/长期日程（含墓碑）、Canvas/ICS/截图导入、陪伴聊天（SSE 流式 + 15 个 agent 工具）、每用户长期记忆、主题工作室、心情打卡、一键撤销（append-only 操作日志）、收件箱/决策卡、许愿池、多语言（用户自选一主一副）。

仓库根即实现面，无子项目分层：**Go 单二进制后端 + Vite/React 前端 + Chrome MV3 插件**，全部在一个 module 里。

## 仓库布局

```
cmd/daycore/           入口：main.go（装配）+ install.go（提示词模板导出器，walk embed FS）
internal/              Go 后端（详见「代码组织与模块划分」）
api/                   API 契约唯一权威：openapi.yaml（生成物）+ FRONTEND_HANDOFF.md
  spec/                openapi 源：head.yaml + paths/<tag>.yaml（一个 tag 一个文件）+ components.yaml + bundle/ 合并器
  lock-rules.json      锁派生规则契约夹具
web/frontend/          现役 React 前端（Vite；将来被 design-ui 四端替换，替换完成前不要动它）
design-ui/             设计原型（只读参考，不参与构建）：四套前端 + core/daycore-core.js 共享 mock + HANDOFF/ 交接文档
docs/                  实时项目文档（架构/认证/Agent/数据/AI/路由总表/开发者手册/体验内核）
docs/specs/            对外协议：transport / storage-protocol / provider-protocol / frontend-manifest
extension/             Chrome MV3 插件（抓 Canvas → POST /api/import/canvas），无打包器
deploy/                Dockerfile（纯 API 镜像）/ docker-compose.yml / nginx.conf
config/                models.yaml（AI 模型目录，加模型零代码）—— 种子在 internal/{ai,auth}/seed/，由 `daycore install` 解压
locales/               <locale>.json 语言包目录（加语言零代码）；目前只有 README.md
testdata/              canvas-export.sample.json / sample.ics（端到端冒烟夹具）
tools/wirelog/         日志反代：看发给 provider 的原始请求（make wirelog）
```

## 技术栈

- **后端**：Go 1.23（module `daycore`），零 CGO（`modernc.org/sqlite` 纯 Go）。关键依赖：`pgx/v5`、`go-sql-driver/mysql`、`mongo-driver`、`golang-jwt/jwt/v5`、`x/crypto`（argon2id）、`godotenv`、`yaml.v3`；间接：`gorilla/websocket`（OneBot）、`robfig/cron/v3`（Worker）。**四个存储后端**：SQLite / PostgreSQL / MySQL（同一 `Dialect` 抽象）+ MongoDB（独立实现）。
- **前端**：React 18.3 + Vite 6，`react-markdown` + `remark-gfm` + `remark-math` + `rehype-katex` + `katex`。设计系统已 vendor 进 `web/frontend/src/ds/` 与 `src/vendor/ds-bundle.js`。
- **AI**：三个 wire-format（`formats/{openai,anthropic,ollama}`，自注册），模型目录 `config/models.yaml` 驱动，加模型零代码。
- **插件**：Chrome MV3 纯静态文件，无构建步骤。
- 许可证 **LGPL-3.0-or-later**（`COPYING.LESSER` + `COPYING`）；引新依赖前确认协议兼容（Apache-2.0/MIT/BSD/MPL-2.0 可以，GPL-only/SSPL/专有不行）。

## 构建与验证命令

```bash
# 后端（全部在仓库根）
go run ./cmd/daycore          # 开发跑（默认 SQLite，:8080；cp .env.example .env 后生效）
make build                    # CGO_ENABLED=0 → bin/daycore 静态单文件
make test                     # 先跑 i18n 校验再 go test ./...
make test-mongo               # 行为一致性套件对真机 MongoDB（需 mongod :27017）
make test-sql                 # 行为套件对真机 PostgreSQL :5432 + MySQL :3306
make test-models              # 活体模型工具调用测试（花钱，绝不上 CI）
make api-bundle / api-check / api-lock / api-surface   # openapi 重建 / 校验 / 锁版 / 路由表重生成
make wirelog                  # 日志反代（看发给 provider 的原始请求）

# 前端
cd web/frontend && npm install && npm run dev     # :5173，/api 代理到 :8080
cd web/frontend && npm run build                  # 产出 dist/，Go 用 STATIC_DIR 托管
node web/frontend/scripts/check-i18n.mjs          # zh-CN/en-US key 对齐校验

# 改动后的必跑项（CI 已删除，验证全在本机）
gofmt -l .                                        # 必须为空
go build ./... && go vet ./... && go test ./...
go build -tags lite ./... && go test -tags lite ./internal/resources/ ./internal/setup/
go test -race ./internal/adapters/ ./internal/weather/
```

## 运行时架构

- **装配顺序**（`cmd/daycore/main.go`）：config.Load → store.Open+Migrate → catalog/prompts → server.New → locale 覆盖层 → 两个 cleanup ticker → **Worker（无条件启动**，不再绑 OneBot）→ channels（仅当配了通道）→ inbound 消费循环。关停：SIGINT/SIGTERM → `httpSrv.Shutdown(15s)` → `WaitBackground` → `worker.Stop()`。
- **中间件链**（全局单链，无分组）：`recoverMW → requestIDMW → loggingMW → corsMW → sessionMW → userMW → dataSessionMW → mux`。三个身份中间件**「解析不强制」**（有效才注入 ctx，从不拦截）；真正的鉴权在 handler 内 `requireSession`（无 sid → 401 `no_session`）或 `adminAuthorized`。
- **路由注册模式**：每个 handler 文件在自己的 `init()` 里 `registerRoutes("<组名>", func(s *Server, mux Mux){…})`，注册表在 `internal/server/routes.go`。**不要去 `server.go` 加路由** —— 那里只剩静态 `/` 一条。`RouteTable(*Server)` 用零值 Server 重放注册（读路由表不需要数据库）。当前 **146 条路由 / 36 个组**（`docs/API_SURFACE.md` 是生成物、带权威数字，`make api-surface` 重生成）。
- **存储注册模式**：`internal/storage/registry.go` 的 `storage.Register(dbType, opener)`，sqlstore/mongostore 在 `init()` 自注册，main.go blank import。`domain.Store` 是组合接口（约 179 个方法），**业务代码只 import `domain`，绝不直接引用具体 store** —— 漏一个 accessor 是编译错误，这正是两个 store 同步的强制手段。
- **版本号只有一个**（2026-08-09 从三层合并；唯一真源 `internal/version/version.go`，同步 `web/frontend/package.json`）：`Version="2.3.0"` + `Channel="beta"`。`APIVersion`/`APIMinor` **从它推导**，`GET /api/version` 照旧报这两个字段 —— 线上形状没变，只是数字来源变了。契约面变了必须升版，由 `api/spec/contract-lock.json` + `go test` 强制。**它是批次标记不是发布号** —— `2.2 → 2.3` 意味着一整份规划实现完毕。发布节奏：v2 beta → 小范围内测 → v2 继续 → 公测 → v3 正式版。各前端自己的版本号在各自子仓库，与本仓解耦。
- **AI 子系统**（`internal/ai/`）：`AIProvider` 接口 + `RegisterFormat` 自注册 + Catalog（`config/models.yaml`）+ PromptService 三层（DB `prompt_overrides` 覆盖 → `PROMPTS_DIR/<locale>/<key>.tmpl` 磁盘逐文件覆盖 → `//go:embed` 内嵌）。**提示词模板必须 zh-CN / en-US 双 locale 成对**，缺一启动报错。15 个 key。**唯一的例外是 L1 硬边界**（`boundaries.go` + `prompts/boundaries.json`）：只有磁盘与内嵌两层，**没有 DB 层、没有端点**——能被控制台改写的边界等于能被删除，见 AI.md。视觉管线三分支（模型自带 vision / 转 vision 模型 / read_image+zoom_image 工具循环 ≤6 轮）。
- **Agent loop**（`internal/server/`）：`runCompanionAgent` 最多 `AGENT_MAX_ROUNDS`(6) 轮，15 个工具定义在 `agent_tools.go` 的 `companionToolDefs`（11 个原有 + 4 个捕捉工具 assignment_upsert/wish_add/mood_record/material_add，β0+ 补齐，实现集中在 `tool_capture.go`）。SSE v2 帧协议：`delta / reasoning / tool_start / tool_result / decision_card / error / done` + 心跳；tool_result 带 `opId` 供撤销。sink 体系：`sseSender`（同步）/ `discardSink`（通道回复，不注册 propose_decision）/ `recordingSink`（异步端点）。决策卡：纯内存 registry，每 session 同时一张，新卡顶旧卡（进程重启即丢，单实例假设）。异步端点写 pending 占位消息，`main.go` 启动时 `FailPendingMessages` 清扫崩溃遗留。旧的 `<plan_update>` 标签协议已废弃。每次模型调用（流式按轮）落一行 `ai_call_logs`（`s.logAICall`，best-effort）；第三方文本进提示词必须过 `untrustedWrap`（web_search 已接）。**附件**：`attachmentIds` → `attachments` 表（所有权）+ `internal/blob`（字节）→ 内联 `ContentPart` 挂到最后一条 user 消息；`Ref` 永不出服务端（`json:"-"`），见 DATA.md 与 AI.md。
- **Worker**（`internal/server/worker.go`）：cron 驱动；产出全部落库、由 App 读，推到通道只是可选的最后一步。按用户排程是**首次请求时懒排**（`SetScheduleOnUse`，`markAwake` 节流放行时调一次；⚠️ 代价是重启当天早上有个缺口）。三个定时时刻**真的由节律派生**（ζ-2 接线；此前是写死的 07:30/21:00，恰好等于 `Schedule(Cold())` 的输出，所以「派生」只在数值上成立）：`PlanAt = Wake − 3h30m`、`BriefAt = Wake`、`ReviewAt = Sleep − 90m`。⚠️ `PlanAt` 至今**没有消费者** —— 定时 auto-plan 作业不存在。另有 deadline 巡检、**节律学习作业**（日切跑，`rhythm_job.go`）、**Protector 20h 关怀**（`protector.go`，场次 key 是这段清醒的起点）。每会话 6 条 cron，全部经 `claim`/`finish` 走 job_runs 唯一索引互斥。
- **多语言三层**（`internal/i18n/`）：DB `locale_overrides` → `LOCALES_DIR/<locale>.json` → 内嵌 zh-CN/en-US。**给后端加一门语言是丢一个翻译文件，不用改代码、不用发版**；内嵌两种是「地板」不是全集。用户自选一主一副（`SessionPrefs.PrimaryLocale`/`SecondaryLocale`），部署只给默认值（`DEFAULT_PRIMARY_LOCALE`/`DEFAULT_SECONDARY_LOCALE`）。⚠️ 前端还不是这样 —— `web/frontend/src/i18n.js` 是硬编码双语言字典。

## 代码组织与模块划分

| 包 | 职责 |
|---|---|
| `internal/domain/` | 纯数据结构 + Repository/Store 接口，零外部依赖 |
| `internal/server/` | 路由、中间件、全部 handler、agent loop、cron Worker（一个文件一组 handler） |
| `internal/storage/sqlstore/` | SQL 三方言（SQLite/PG/MySQL，`Dialect` 抽象），每实体一文件；三份 DDL 由 `dialect_parity_test.go` 静态比对 |
| `internal/storage/mongostore/` | MongoDB，每实体一 repo 文件；`bson_test.go`（免真机）+ `conformance_test.go`（真机行为套件） |
| `internal/storage/storagetest/` | **行为一致性套件**（69 例）：所有后端跑同一份，加后端的验收标准 |
| `internal/ai/` | AIProvider 抽象、Catalog、PromptService、流式协议、vision 管线；`formats/{openai,anthropic,ollama}` 自注册 |
| `internal/auth/` | 密码(argon2id)/OAuth/JWT/签名 cookie |
| `internal/blob/` | **文件总线**：`Store` 注册表 + `localfs` 本机磁盘驱动 + `blobtest` 行为套件（11 例）。`DATA_DIR` 是仓库第一个可写路径；`nil` 是受支持的配置，需要字节的功能各自检查并明说。**鉴权不在这一层** —— 它只认 ref 不认会话，所有权在 `attachments` 表上（ε 批次接的第一个真使用者） |
| `internal/channels/` | 通道插件框架（Registry + OneBot 11 适配器） |
| `internal/config/` | 环境变量配置（godotenv）+ **配置分层**（`layer.go` 的 `Settings`：每个旋钮标启动期/运行时/密钥；新加字段没分类直接红，见 `docs/CONFIG.md`）|
| `internal/search/` | web 搜索（Tavily→DDG）+ MaterialSearcher（原生 FTS 优先 + 子串兜底） |
| `internal/weather/` | WeatherProvider registry（open-meteo/qweather/owm/wttr.in，30min 缓存） |
| `internal/version/` | 版本唯一真源（构建版本 + API 契约版本，别混） |
| `internal/rapport/` `internal/rhythm/` `internal/mood/` | 默契评分与主动性门控 / 节律学习 + 20h 关怀 / 心情窗口（趋势+衰减+新鲜度）—— 三个都是纯函数、零存储、读时派生 |
| `internal/schedule/` `internal/ics/` `internal/timeutil/` | 重复规则展开引擎 / 最小 iCalendar+RRULE 解析器（零依赖）/ 石化线与墙钟换算 |
| `internal/i18n/` | locale 协商 + 三层消息目录 + 用户级一主一副 `Pair` |

## 代码约定

- **副作用永远服务端执行**：前端决不能直接调 store 写操作，必须经过 agent 工具或 HTTP handler —— 撤销体系整个建立在这条上。
- **所有写路径必须调 `s.logOp`**（append-only 操作日志，`detail` 存 before/after 快照；**撤销是从 before 快照逐键重建的**，快照不全等于撤不回来）。best-effort（丢错不拦）。撤销注册表在 `handlers_ops.go`（`registerRevert(action, h)`，注册重复 panic；没注册的 action 返回 `irreversible`）。`revert` 自己永不可注册为可撤销。
- **错误处理四类**：agent 工具失败 → `toolResult{OK:false, ErrMsg}`（不中断 loop，注回上下文让模型重试）；HTTP handler → `s.writeErr(w, status, stableCode, humanMessage)`（**不要只给 humanMessage**，前端靠 stableCode 做 i18n）；存储层「找不到」→ 统一 `domain.ErrNotFound`；AI 流出错 → `chunk.Err` → SSE error 帧 + **done**（客户端必须收到 done）。
- **永不信任客户端上送的上下文**：companion handler 不接收 `todayPlan`/`moodHistory`/`date` 等，全部由服务端从 store 组装；对话历史做角色白名单（客户端不能注入 `system` 轮次）。
- **文件命名**：Go `snake_case.go`；React `PascalCase.jsx`（页面/组件）、`camelCase.js`（工具/状态）。
- **Go 里的用户可见文案一律 `i18n.Register` + `i18n.T`/`Tf`**，不要写 `if HasPrefix(locale,"en")`，也**不要直接 `i18n.Pick`**（绕开 DB/文件两层，让字符串变成不可翻译的）。同一 key 注册两次会 panic。带 `%` 动词的条目新语言必须保留同样的动词与顺序。
- **存储层取舍规则**：凡是出现在 `WHERE` 里、或被算术/`CASE` 更新的字段**必须是列**；其余可以进 JSON blob。需要条件写就用 JSON 路径写（`json_set` 配 `WHERE json_extract`，三方言都支持）。`NormalizeDSN` 模式：代码依赖的连接参数（SQLite `busy_timeout`+WAL、MySQL `clientFoundRows`）由方言自己补，不写进文档等运维抄全。
- **给已有表加列走 `ColumnMigration`**（`sqlstore/dialect.go` 的 `sessionColumnMigrations`，实际覆盖六张表），同时改三方言建表 DDL 与 mongostore doc struct；MySQL 的 TEXT 一律可空、读侧 COALESCE；新表不要用需要引号的列名。
- **实时文档铁律**：任何代码改动必须在同一批修改中更新 `docs/` 对应文件；加/删路由连着改 `api/spec/paths/<tag>.yaml` → `make api-bundle` → 升 `internal/version/version.go` 的 `Version` → `make api-surface`。⚠️ 路由↔openapi 双向核对与 bundle 新鲜度**有测试盯着**，版本号那一步**没有** —— 那是人守的。
- **设计约束也要进 docs，不只是事实**（2026-08-06 补）：**边界**（什么有意不做、什么绝对不能加）、**取舍**（选了什么、放弃了什么、代价多少）、**它防的哪个具体失败**。判据是「改这块代码的人不该需要先读一遍代码才知道哪些是有意为之」——**代码注释与 commit message 不算数**，它们不可检索也不随代码演进。这类约束被当成疏漏顺手「修掉」，是本仓最贵的一种回归。

## 测试策略

- **`internal/storage/storagetest` 行为套件是存储层改动的验收标准**：69 个用例，SQLite（`go test ./...` 内）与真机 Mongo（`MONGO_TEST_DSN`，`make test-mongo`）、真机 PG/MySQL（`make test-sql`）跑同一份。测的是**行为**（lease 只有一个持有者、rev CAS 拒绝陈旧写、`ProposalOp.Args` 数字回来是 `float64`、TTL 不对称、游标续读无重无漏……），不是「能存能取」。
- **`dialect_parity_test.go` 是静态比对**：三方言表集合/列集合/索引集合相同、MySQL TEXT 不带字面 DEFAULT、索引名 ≤63 字节、ColumnMigration 不出现「NOT NULL 无 DEFAULT」、仓库 SQL 引用的每张表都有建表语句。**失败时改 schema，不要放宽检查**。它不能替代真机（静态比对看不出 DDL 是否合法）。
- **`routes_test.go` 双向核对**：路由 ↔ `api/openapi.yaml`（服务了没写进契约 / 写进契约没人服务都红）、pattern 不重复、每条带方法；`api/spec/bundle` 测试断言签入的 openapi.yaml 与 shard 一致（契约过期是唯一没有别的症状的失败）。
- **`auth_surface_test.go` 强制公开端点名单**：对不在名单上的每条路由发无凭证请求必须 401，反向也查。
- **没有 CI**（2026-08-09 删除）。验证全在本机，且比原来那四个 job 更强 —— 多了 `-tags lite`、`-race` 与子进程 e2e。**丢掉的**：没有东西检查这台机器没跑过的分支，前端构建与插件 zip 只有人跑才跑。
- **其他测试网**：`bson_test.go`（免真机序列化往返）、`phase_test.go` + `api/testdata/petrify-vectors.json`（DST 行为表驱动，将来 TS 侧 vendored 同一份）、`livemodel_test.go`（`make test-models`，花钱）、`handlers_ops_test.go`（撤销注册表非空/重复 panic）、`locales_test.go`（三层接线）。

## 部署

- **单二进制模式**（推荐）：`cd web/frontend && npm run build` → `make build` → 上传 `bin/daycore` + `config/` + `web/frontend/dist`；`STATIC_DIR` 指向 dist，Go 同时托管前端与 `/api`（`/assets/` immutable 长缓存，SPA fallback 回 index.html；`STATIC_DIR=""` 或无构建产物 = 纯 API 模式）。
- **生产环境变量**：`APP_ENV=production` 后 `JWT_SECRET`/`COOKIE_SECRET` 缺失直接启动失败；**`ADMIN_TOKEN` 不是可选的**（不设时管理面鉴权退化成「只看 `APP_ENV` 是不是 production」，而管理面含 `GET`/`DELETE /api/admin/db/table/{name}` 裸库读删）；`SECURE_COOKIES=true`；`PUBLIC_BASE_URL` 给 OAuth 回调用；`HOST=127.0.0.1` 只监听本机由 nginx 对外（此时 `TRUST_PROXY_HEADERS=true` 限流才按真实 IP 分桶）。环境变量完整清单在 `docs/CONFIG.md`（**生成的总表**，每个旋钮标启动期/运行时/密钥；`.env.example` 是常用子集，完整清单以 CONFIG 为准）。
- **nginx**：`proxy_buffering off` 是 SSE 硬要求，`proxy_read_timeout 300s`（AI 请求慢）。样例 `deploy/nginx.conf`。
- **Docker**：`deploy/Dockerfile` 是纯 API 镜像（不 COPY 前端产物），`docker-compose.yml` 带 postgres/mysql/mongo 三个本地开发 profile（`make db-postgres` 等）。
- **数据库**：`DB_TYPE`/`DB_DSN` 一键切换 sqlite/postgres/mysql/mongodb。**MongoDB 是推荐部署**，但四个后端都要能跑（行为套件守着）。
- **浏览器插件**：设置页生成 Import Token，插件填服务器地址 + token；**Chrome 会弹窗请求该域名的访问权限，必须允许**（MV3 下没有 host permission 的跨域 fetch 被 CORS 拦掉）。

## 安全注意事项

- **凭证双轨**（cookie + header，header 优先）：匿名 session `dc_sid`（`sid.<hmac>`，HMAC-SHA256 keyed by COOKIE_SECRET，MaxAge 400 天）↔ `X-Session-Token`；登录用户 `dc_auth`（JWT HS256，claims 含 `tv`，MaxAge=JWT_TTL）↔ `Authorization: Bearer`。**token_version 撤销**：登出时 `IncrementTokenVersion`，全设备 JWT 即失效。cookie 统一 HttpOnly / Secure=`SECURE_COOKIES` / SameSite=`COOKIE_SAMESITE`（none 需 Secure，值不对启动失败）。
- **CORS 四分支**（`corsMW`）：`/api/import/*` 前缀 `ACAO:*` 无 credentials（插件直推，token 鉴权）；`ALLOWED_ORIGINS` 含 `*` 时反射 `*` 且**明确不与 credentials 组合**（防 CSRF）；显式 allowlist 回显 origin + credentials；其余不设头。OPTIONS 一律 204 短路。header 轨凭证必触发 preflight → 天然免 CSRF。
- **管理面**：`X-Admin-Token` 常量时间比较（`subtle.ConstantTimeCompare`），走请求头不走 URL。已知待改造：明文密钥长期躺在控制台 `sessionStorage`、永不过期、无法轮换（方向：一次登录换短 TTL JWT cookie，批次 F4）。
- **密码**：argon2id，PHC 编码，per-user cost jitter，可选 `PASSWORD_PEPPER` HMAC 混入，并发上限 4。
- **有意公开的信息披露**：`GET /api/healthz` 免鉴权返回 `db`/`env`/`version`/`channel`，是知情取舍（插件「测试连接」有真消费者）；ping 失败时**不回传驱动错误原文**（含 DSN 与主机片段）。
- **AI 端点鉴权**：所有 AI 端点必须 `requireSession`（2026-07-30 补的洞：三条 AI 端点只有 IP 限流，任何人都能烧模型额度）。限流：`AI_RATE_LIMIT_PER_MIN`(30)/IP、`AUTH_RATE_LIMIT_PER_MIN`(10)；`MAX_IMAGE_BYTES`(8MiB)。
- **第三方文本进 LLM 的注入面**（`docs/specs/` 贯穿规则）：客户端提供、要进 LLM 的文本（provider 描述、前端 `theme.rules`）**未经运维批准不使用** —— 没主张或没批准就按结构化字段机械生成，注入面默认为零。
- **部署事故史**：README 里有一段「注释写在续行 `\` 之后导致 `APP_ENV` 等五项丢失、管理面敞开」的真实事故，照 README 部署时必须把注释放独立行。

## 文档地图

| 想知道 | 看 |
|---|---|
| 仓库铁律、布局、常用事实（本文件的浓缩版） | [`CLAUDE.md`](CLAUDE.md) |
| **总规划：做什么、按什么顺序、为什么** | [`docs/ROADMAP.md`](docs/ROADMAP.md) |
| 架构、包结构、中间件、启动关停、多实例、两层时区 | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| **每个配置项能不能热改**（启动期/运行时/密钥，加字段必须分类） | [`docs/CONFIG.md`](docs/CONFIG.md)（表是生成的） |
| 实体、四个存储后端、加表加列、迁移事故史 | [`docs/DATA.md`](docs/DATA.md) |
| 认证三轨、CORS、鉴权旁路 | [`docs/AUTH.md`](docs/AUTH.md) |
| agent loop、SSE 帧协议、工具带、决策卡 | [`docs/AGENT.md`](docs/AGENT.md) |
| provider、提示词三层、加 wire-format | [`docs/AI.md`](docs/AI.md) |
| 有哪些 HTTP 路由、各在哪个文件 | [`docs/API_SURFACE.md`](docs/API_SURFACE.md)（生成的） |
| 端无关的产品语义（时间三层、提案、注意力阶梯、默契） | [`docs/EXPERIENCE_CORE.md`](docs/EXPERIENCE_CORE.md) |
| 战略认知、商业化方案、陪伴边界、全功能审计（讨论注入，待裁决后并入 ROADMAP/EXPERIENCE_CORE） | [`docs/STRATEGY.md`](docs/STRATEGY.md) |
| **PWA 与家庭模式**（计划书，未开工：可安装/离线/推送 + 家庭组与联动） | [`docs/PWA_AND_FAMILY_MODE.md`](docs/PWA_AND_FAMILY_MODE.md) |
| 加路由/工具/模型/语言的分步骨架、开发命令 | [`docs/DEVELOPING.md`](docs/DEVELOPING.md) |
| **别人照着实现什么**（存储 / provider / 前端适配层） | [`docs/specs/`](docs/specs/README.md) |
| API 契约 | [`api/openapi.yaml`](api/openapi.yaml)（**生成物**，源在 [`api/spec/`](api/spec/README.md)）+ [`api/FRONTEND_HANDOFF.md`](api/FRONTEND_HANDOFF.md) |

## 最容易踩的三条

1. **`api/openapi.yaml` 是生成物。** 改 `api/spec/paths/<tag>.yaml` 然后 `make api-bundle`。文件名必须等于 operation 的 tag，合并器会强制。
2. **路由在自己的 handler 文件里注册**（`init()` 里 `registerRoutes`），不在 `server.go`。加一条路由要连着改 openapi 分片、升 `APIMinor`、跑 `make api-surface` —— 三样都有测试盯着，漏一样就红。
3. **`docs/` 与代码同批更新。** 这不是风格要求：这个文件本身就是不守它的后果。
