# 开发者手册

> 开发命令、扩展点、加路由/工具/模型/语言的分步骨架、切仓流程、写路径操作日志的完整约定。2026-08-13 重构：通用代码约定收敛回 AGENTS.md，本文只留开发者特有内容。

> ⚠️ **本文引用的 `/api/…` 路径实际服务在 `/api/v2/…` 下**（2026-08-11 起，规则见 `internal/apipath`）。
> 正文按**资源**写，因为哪个 major 在服务它是另一件事、只决定一次。
> 三条例外留在 `/api/` 外面：`/api/version`（发现）、`/api/healthz`（存活探针）、
> `/api/auth/oauth/{provider}/callback`（注册在第三方控制台里的重定向目标）。
> 权威的完整线上路径见 `docs/API_SURFACE.md` 与 `api/openapi.yaml`。


> 从 README 搬来（2026-07-29）—— README 只留「这是什么 / 怎么跑起来」，其余瀑布式下沉到 `docs/`。

### 项目结构

```
cmd/daycore/main.go         装配：注册驱动/格式 → 打开 Store → 迁移 → 起 HTTP（优雅关闭）
internal/
  domain/                   实体 + 通用 Store/Repository 接口（界面分离核心）
  config/                   环境配置（HOST/PORT/STATIC_DIR/…）
  version/                  版本号唯一来源（构建版本 + API 契约版本，别混）
  auth/                     argon2id / JWT / OAuth / 会话 cookie
  i18n/                     locale 协商 + 三层消息目录（DB → LOCALES_DIR → 内嵌）+ 用户级一主一副 Pair
  ai/                       AIProvider 接口 + catalog + 视觉编排 + 提示词
    formats/{openai,anthropic,ollama}/   wire-format（自注册）
    prompts/<locale>/*.tmpl 提示词（14 key × 2 locale；PROMPTS_DIR 可逐文件覆盖）
    prompts/boundaries.json L1 硬边界（只有磁盘+内嵌两层，控制台改不到，见 AI.md）
  schedule/                 重复规则展开引擎（纯函数）
  ics/                      最小 iCalendar + RRULE 子集解析器（零依赖）
  timeutil/                 石化线与墙钟换算（纯函数）
  rhythm/ mood/ rapport/    节律学习 / 心情窗口 / 默契评分 —— 三个都是纯函数、零存储、读时派生
  channels/                 通道插件框架（Registry + OneBot 11 适配器）
  search/                   web 搜索（Tavily→DDG）+ MaterialSearcher（原生 FTS + 子串兜底）
  weather/                  WeatherProvider registry + 四个 provider 子包（自注册）
  storage/{sqlstore,mongostore}/  SQLite+PG+MySQL（Dialect 抽象）/ MongoDB
  storage/storagetest/      行为一致性套件（69 例，四个后端跑同一份）
  server/                   路由（分散注册，见 routes.go）+ 中间件 + handlers + agent loop + cron Worker
api/                        openapi.yaml（生成物）+ spec/（按 tag 分片的源）+ FRONTEND_HANDOFF.md
package.json                ⚠️ 不是一个包，是 workspace 根（core + 四端；
                            web/frontend 与 web/console 有意不在里面）
scripts/split-repos.sh      阶段 κ：切成兄弟仓 + 挂回 submodule（默认空跑）
scripts/core-min-api.py     算 @daycore/core 的最低兼容 API 版本（它的版本号就是这个数）
packages/core/              @daycore/core：四端共享层（HTTP / 握手 / 多语言 / 后端地址）
web/frontend/               现役 React 前端（Vite）。⚠️ 将来被下面四端替换，替换完成前不要动它
web/ting/                   汀 · 此刻（单件流）        :5175
web/zhiyu/                  纸屿 · 顺流（叙事流）      :5176
web/liuli/                  琉璃 · 长卷（时间画布）    :5177
web/liuli-classic/          琉璃初版 · 页面制（对照组）:5178
web/console/                运维控制台（⚠️ 唯一编进 Go 二进制的前端）
deploy/                     Dockerfile / docker-compose / nginx
testdata/                   canvas-export.sample.json / sample.ics
docs/                       实时项目文档（架构/认证/Agent/数据/AI/路由总表）
extension/                  Chrome MV3 插件（抓 Canvas → POST /api/import/canvas）
design-ui/                  设计原型：四套范式级不同的前端 + 共享 mock + 交接文档（只读参考，不参与构建）
docs/specs/                 对外协议：传输层 / 存储适配 / provider 适配 / 前端 manifest
```

### 扩展点（registry / 驱动模式）

1. **新增数据库**：实现 `domain.Store` + `storage.Register("foo", opener)`（SQL 类只需加一个 `Dialect`），main.go 加 blank import。**验收标准是 `internal/storage/storagetest` 的行为套件通过** —— 它测的是四个后端必须一致的行为，不是「能存能取」。不想写 Go 的话，将来可以走 [`docs/specs/storage-protocol.md`](specs/storage-protocol.md) 的 HTTP/子进程协议。
2. **新增 AI 厂商格式**：实现 `ai.AIProvider` + `ai.RegisterFormat("gemini", New)`，`models.yaml` 引用 `format: gemini`。
3. **新增模型（零代码）**：编辑 `config/models.yaml`，重启生效；视觉模型标 `vision: true`。
4. **新增 OAuth provider（零代码）**：`config/oauth.yaml` 加条目，回调 `<PUBLIC_BASE_URL>/api/auth/oauth/<name>/callback`。
5. **编辑提示词（运行时）**：`PUT /api/admin/prompts/{key}?locale=zh-CN|en-US`（`X-Admin-Token`），覆盖存 `prompt_overrides`。

### 开发命令

```bash
make run / test / vet / build / docker
make test-mongo                                     # 行为套件对真机 MongoDB 跑（需本机 mongod）
cd web/frontend && npm run dev / build
node web/frontend/scripts/check-i18n.mjs            # zh-CN / en-US key 对齐校验

# 前端依赖在根上装一次就够（workspace），@daycore/core 解析成 packages/core 的符号链接
npm install
for a in ting zhiyu liuli liuli-classic; do (cd web/$a && npm run build); done

make check-core-pack                                # 证明 core 是个真包（切仓的前提）
make submodules                                     # 切仓之后：别人 clone 的第一步
make core-dev / core-pinned                         # 测未发布的 core / 回到钉住的 tag
make core-min-api                                   # 重算 core 的最低兼容 API 版本
```

### 切仓之后，改 `@daycore/core` 的完整流程

四端各自钉一个 core 的 tag，**建的就是钉的那个**（workspace 根**不会**覆盖 git tag
依赖 —— npm 会给每个包塞一份嵌套拷贝，嵌套的赢；`overrides` 与直接依赖冲突被 npm
拒绝。两条都实测过）。

```bash
make core-min-api                                   # 先重算版本号：core 的版本 = 兼容的最低 API 版本
make core-dev                                       # 四端先指向工作区的 packages/core
# 改 packages/core，四端 npm run build 验它
cd packages/core && git commit && git push && git tag vX.Y.Z && git push origin vX.Y.Z

# 四端各自：改 pin，并在 workspace 之外**增量**重解析 lock，然后 commit + push
for a in ting zhiyu liuli liuli-classic; do (
  cd web/$a
  # 把 package.json 的 @daycore/core 改成 …#vX.Y.Z
  tmp=$(mktemp -d) && cp package.json package-lock.json "$tmp/"
  ( cd "$tmp" && npm install --package-lock-only --allow-git=all )
  cp "$tmp/package-lock.json" package-lock.json  # 只该动两条：依赖声明 + node_modules/@daycore/core
  git commit -am '钉 @daycore/core vX.Y.Z' && git push origin HEAD:refs/heads/main
); done

npm install --allow-git=all                                  # 超级仓：重解析四份嵌套 core
git add packages/core web/* package-lock.json && git commit  # bump 五个 gitlink
```

⚠️ **这套流程的三个坑（2026-09-17 实跑修正）**：

1. **不要 `rm -f package-lock.json && npm install`。** 那是上一版文档的写法，会把整棵
   依赖树重新解析 —— 实测 rollup / vitest / @types/node 等 **51 条无关依赖**跟着升级。
   发布要的是「只动 core 这一条」，所以保留原 lock 做**增量**重解析
   （`--package-lock-only`），改完核对 diff 恰好两条。
2. **lock 必须在 workspace 之外生成。** 在 `web/$a` 里跑 `npm install`，npm 认的是
   workspace 根，它只写**根** `package-lock.json`；子仓那份不但不会更新，**删了就再也
   不会出现**。所以上面用 `mktemp -d` 在树外跑。
3. **npm ≥ 12 默认禁 git 依赖**（`allow-git=none`），不加 `--allow-git=all` 会直接死在
   `EALLOWGIT`。上一版流程写于 npm 12 之前，照着跑必然失败。

⚠️ **只改 `package.json` 不重生成 `package-lock.json` 是不够的，而且症状只在别人
那边出现。** lock 记的是解析后的 **commit**；改了 pin 不重生成，独立 clone 的人
拿到的仍然是旧 core（`package.json` 说 v2.4.0，lock 说上一个 tag 那个 commit，npm
听 lock 的），`tsc` 报 `has no exported member`。

超级仓里发现不了 —— 那边跑的是**根上**那份 lock，各仓自己的 lock 在 workspace 下
根本不参与。这条是真的踩过一次，是「独立 clone 出来构建」这一步验出来的。

⚠️ **版本号不是随便挑的**：`@daycore/core` 的版本 = **兼容的最低 API 版本**（`make
core-min-api` 重算，不要手填），
见 [ROADMAP](ROADMAP.md)。`internal/server/core_client_test.go` 会拦住对不上的。

⚠️ **切仓之后**（阶段 κ），`packages/core` 与 `web/ting|zhiyu|liuli|liuli-classic`
都是 submodule。读它们的那些契约闸门在目录缺席时**跳过** —— 那个跳过是对的（可选
checkout 不该变成必需的），但它意味着一次普通 `git clone` 拿不到任何契约检查还报绿。
`make test` 里的 `submodules-present` 挡这个；单独跑 `go test ./...` 不挡（后端单独
干活的人需要它）。

⚠️ **四端的多语言校验不是那个 `check-i18n.mjs`**，是各自 `src/locales.test.ts`，跑在
`npm run build` 里 —— 一个没人记得跑的检查等于不存在。它比脚本那份严：双向对齐、
占位符跨语言一致、源码里不许有漏网的中文字面量，还要求每个 key 都带点（那是提取器
用来区分 key 与三元比较值的判据，所以它被断言而不是被假设）。

### 端到端冒烟（curl）

见 `testdata/` 夹具；核心流：`POST /api/session/init` → `POST /api/import/token` → `POST /api/import/canvas`（X-Import-Token）→ `POST /api/import/ics` → `POST /api/rules` → `GET /api/plan?date=`（规则虚拟合并）→ `POST /api/ai/auto-plan` → SSE `/api/ai/companion`（**工具调用**，不是早先的 `<rule_update>` 标签协议；帧格式见 `api/FRONTEND_HANDOFF.md` §B）→ `GET /api/memory`。

### 相对 v1 的改进

- 主模式从「手动输入日程」变为「自主规划」（资料导入 → auto-plan → 聊天微调）。
- 新增重复/长期日程、Canvas/ICS/截图导入、每用户长期记忆、自定义主题 + AI 配色、i18n（含提示词双语）、版本体系与单二进制静态托管部署。
- 修复无鉴权数据端点（服务端签名 httpOnly cookie）、`incrementInteractionCount` 占位符 bug、日程加载 N+1、v1 MoodScreen `exerciseOffered` 异步 bug（前端已按正确方式实现）。

---


## 操作日志与撤销（写路径的完整约定）

> 副作用归属、错误处理四类、文件命名等通用约定见 [AGENTS.md](../AGENTS.md)「代码约定」——那是它们的家，这里只保留写路径操作日志的**细节**（AGENTS.md 只有摘要）。

### 操作日志

所有写路径（plan add/update/remove/upsert/auto-plan、rule create/update/delete/batch、memory add/delete/clear、assignment create/patch、mood create、material create/update/delete，以及所有 agent 工具执行）必须调 `s.logOp`：

```go
func (s *Server) logOp(ctx context.Context, l *domain.OperationLog) string {
    if l.ID == "" { l.ID = uuid.NewString() }
    if l.Actor == "" { l.Actor = domain.ActorUser }
    if l.Status == "" { l.Status = domain.OpStatusOK }
    if l.RequestID == "" { l.RequestID = requestIDFrom(ctx) }
    _ = s.store.OpLogs().Add(ctx, l)
    return l.ID  // 返回 opID 供撤销链
}
```

best-effort（`_ =` 丢错），`detail` 存 before/after 快照 —— **撤销是从 before 快照逐键重建的**，所以快照不全等于那条操作撤不回来。

⚠️ **快照写错比不写更糟**，这三条是 2026-08-12 补六条 HTTP 写路径时买来的：

- **更新类的 before 必须是拷贝。** 就地改的对象（`existing.Field = …` 那种）到记账
  时已经是 after 了，记它等于把 after 记两遍 —— 撤销把改动还原到它自己身上、报成功
  而什么也没变。
- **读不到 before 就拒绝写**，不要退化成 `before: null`。逆操作靠这个字段区分
  「恢复」与「删掉」，null 会让撤销**删掉**一条读者只是编辑过的东西。
- **删除类要在删之前读**。删完就没地方读了。

⚠️ **HTTP 与 agent 两条门要写出同一形状的 op**（同 action、同 Detail 结构），因为
逆操作读的是 `Detail`，不关心是哪扇门进来的。此前六条 HTTP 路径干脆不入账，而 agent
的同名动作是对的 —— 于是账本从任何人看过的角度都健康，只有前端真的调那些端点时才
显形。见 `internal/server/http_writes_undoable_test.go`。

### 错误处理，四类各有约定

| 哪一层 | 怎么报 | 为什么 |
|---|---|---|
| Agent 工具失败 | `toolResult{OK: false, ErrMsg: "…"}` | **不中断 loop** —— 作为 `role=tool` 消息注回上下文，模型看到错误可以改参数重试一次，或者向用户口头解释 |
| HTTP handler | `s.writeErr(w, status, stableCode, humanMessage)` | `stableCode` 给前端做 i18n，`humanMessage` 只是回退。**不要只给 humanMessage** —— 那样前端只能拿字符串匹配 |
| 存储层「找不到」 | `domain.ErrNotFound` | 所有 repo 统一用它，行为一致性套件断言这一点 |
| AI 流出错 | `chunk.Err` → SSE error 帧 + done | 客户端必须收到 done，否则它会一直转 |

### 文件命名

- Go：`snake_case.go`
- React：`PascalCase.jsx`（页面/组件）、`camelCase.js`（工具/状态）

## 加东西的分步骨架

### 加一条 API 路由

1. 在 `internal/server/` 下写 handler（`sid, ok := s.requireSession(w, r)` 开头）。
2. **在同一个文件的 `init()` 里注册**：`registerRoutes("<组名>", func(s *Server, mux Mux) { mux.HandleFunc("GET /api/xxx", s.handleXxx) })` —— 不要去 `server.go`，那里已经没有集中清单了（见 ARCHITECTURE.md「路由注册模式」）。
3. **写进契约**：改 `api/spec/paths/<tag>.yaml`（文件名必须等于 operation 的 tag），然后 `make api-bundle`。跳过这步 `go test ./...` 会红 —— `routes_test.go` 与 openapi 双向核对。
4. **契约面变了就升版**：`internal/version/version.go` 的 `APIMinor`（additive）或 `APIVersion`（breaking）。一批只升一次，`api/spec/contract-lock.json` 会要求至少升过一次。
5. `make api-surface` 重生成 `docs/API_SURFACE.md`。
6. 前端要用就在 `store.js` 加函数调 `api.get/post/patch/del`；有新文案就在 `i18n.js` 两个语言块各加一条。

### 加一个 Agent 工具

1. 在 `agent_tools.go` 的 `companionToolDefs` 里加 `ai.ToolDef{Name, Description, Parameters: schemaObj(...)}`。**如果这个工具在某些场景下不可能工作，就别把它加进工具带** —— 先例是 `interactive`：回不了卡的沉降口不给 `propose_decision`，因为「不给」胜过「让 agent 等一个永远等不到的答复」。
2. 在 `runCompanionTool` 的 switch 里加 case。
3. 照 `toolMemoryAdd` 的骨架实现：

```go
func (s *Server) toolMyNewTool(ctx context.Context, sid, rawArgs string) toolResult {
    var args struct{ /* … */ }
    if err := json.Unmarshal([]byte(rawArgs), &args); err != nil { return toolFail("…") }
    // 调 s.store.* / s.weather.* / s.search.*
    opID := s.logOp(ctx, &domain.OperationLog{ /* … */ })
    return toolResult{OK: true, OpID: opID, Data: map[string]any{}, Summary: "…"}
}
```

4. **写操作要能撤销**：在 `handlers_ops.go` 用 `registerRevert(action, handler)` 注册逆操作，每种 `action` 至少一个用例。
5. 影响前端缓存（plan/rule/memory）的，在 `store.js` 的 `applyToolResult` 里加映射。

### 加一个模型

只改 `config/models.yaml` 一条记录，零代码 —— 只要 `format` 名已注册。重启生效。

### 加一门语言

丢一个 `<locale>.json` 进 `LOCALES_DIR`，或从控制台粘一份进 DB。**不改代码、不发版。** 前端目前还不是这样（`i18n.js` 是硬编码字典）。

## 项目规则

- **实现域唯一**：仓库根即实现面，后端在 `internal/`、前端在 `web/frontend/`。`design-ui/` 是只读设计参考，不要往里写业务代码。
- **无包袱直切**：Beta 阶段不做双轨/灰度/迁移脚本。前后端同 PR 合入，回滚靠 git revert。
- **测试是回归网**：日期解析、tool_call 解析、agent loop、operation_logs、存储行为一致性全覆盖。`make test` 先跑 i18n 校验再跑 Go test。
- **非构建/验证期的 Bash 不要做危险操作**（删库、`rm -rf` 等）。


### 文档去哪找什么

| 想知道 | 看 |
|---|---|
| **做什么、按什么顺序、为什么是这个顺序** | `docs/ROADMAP.md` |
| 这个仓库现在是什么样、为什么这样 | `docs/ARCHITECTURE.md` `docs/DATA.md` `docs/AUTH.md` `docs/AGENT.md` `docs/AI.md` |
| 有哪些 HTTP 路由 | `docs/API_SURFACE.md` + `api/openapi.yaml`（唯一权威；**改的是 `api/spec/paths/<tag>.yaml` 然后 `make api-bundle`**） |
| 端无关的产品语义（时间三层、提案、注意力阶梯、默契） | `docs/EXPERIENCE_CORE.md` |
| **别人要照着实现什么**（适配器、前端） | `docs/specs/` |
| 前端要对接的 API 细节 | `api/FRONTEND_HANDOFF.md` |

**实时文档铁律**：任何代码改动必须在同一批修改中更新 `docs/` 对应文件。文档与代码不同步视为改动未完成。

