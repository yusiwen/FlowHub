# FlowHub：opencode 与 DevOps 编排集成方案（交接文档）

> 项目名：**FlowHub**（Go 实现）
> 定位：汇聚多种 agent 与 DevOps 节点的 workflow Hub，让 workflow 自动化流转，取代节点间的手工复制粘贴。
> 来源：一次 opencode 可行性分析会话（本机 opencode 1.18.31 实测）。
> 目的：交付给 **FlowHub**（Go）实现：统一 webhook/编排服务 + headless opencode 编排。

## 1. 背景与目标

- 项目名：**FlowHub**，使用 **Go** 编写。
- 目标：用常驻 headless opencode 响应 DevOps 事件（YouTrack / Gitea / Drone 等）。
- 触发方式：部分配置在 CI 流水线里，部分通过 webhook（统一由 FlowHub 承接）。
- 要求：
  1. 同一任务的多次对话必须落在**同一个 session**；
  2. 每一步（分析、修复、测试、发版）由 DevOps 节点通知推进；
  3. 最终由 opencode 回帖汇报结果；
  4. session 必须持久化，支持后续审计 / review；
  5. 希望 opencode 启动时**自动向 FlowHub 注册**。

## 2. 总体结论

- 技术可行。opencode 提供 `opencode serve`（HTTP + OpenAPI + SSE），插件系统，SQLite 持久化。
- **两个必须由 FlowHub 承担的核心职责**：
  1. **session 归属映射**：opencode 的 `sessionID` 由服务端生成，外部无法指定 → 必须自建 `task_key → session_id` 注册表。
  2. **每 session 串行投递**：v1 `prompt_async` 在 session busy 时会静默吞消息（见 §7），必须由调用方保证只在 idle 时投递。

## 3. 本机实测环境（版本与路径）

| 项 | 值 |
|---|---|
| opencode 二进制 | `/Users/yusiwen/.opencode/bin/opencode`（Mach-O arm64） |
| 版本 | **1.18.31** |
| 插件包 | `@opencode-ai/plugin` 1.14.18（`~/.opencode/node_modules/@opencode-ai/plugin`） |
| SDK | `@opencode-ai/sdk` 1.14.18，含 `gen/`(v1) 与 `v2/` |
| 数据目录 | `~/.local/share/opencode/` |
| 持久化库 | `~/.local/share/opencode/opencode.db`（SQLite，实测约 2.9GB，含 `-wal`/`-shm`） |
| 其他 | `auth.json`、`log/`、`snapshot/`、`storage/`、`tool-output/` |
| 当前运行进程 | `PID 55437 opencode acp`，父进程 Zed(`55315`) |

**重要实测结论**：`opencode acp` 进程本身在 `127.0.0.1:4096` 上监听，并对外提供完整 HTTP API（`GET /global/health` 返回 `{"healthy":true,"version":"1.18.31"}`，`/doc` 返回 OpenAPI spec）。即 **ACP 与 HTTP server 同进程、同状态、同 DB**，Zed 的 ACP session 与浏览器 4096 看到的是同一批 session。

## 4. CLI 能力清单（1.18.31 实测）

- `opencode serve [--port] [--hostname] [--mdns] [--cors]` — headless HTTP server（默认端口 4096）。
- `opencode run [message..]` — 参数含 `-c/--continue`、`-s/--session <id>`、`--fork`、`--attach <url>`、`--dir <path>`、`-p/--password`、`-u/--username`、`--auto`、`--format default|json`、`--title`。
- `opencode session list | delete <id>`。
- `opencode export [sessionID] [--sanitize]` — 导出 JSON（`--sanitize` 脱敏）。
- `opencode import <file>`。
- `opencode db [query]` / `opencode db path` — 直接查 SQLite。
- `opencode attach <url>` — 附着到运行中的 server。
- `opencode acp [--cwd]` — ACP（stdio）+ 内嵌 HTTP server。

## 5. 数据库结构（审计基础）

`opencode.db` 表：
`project, project_directory, session, session_context_epoch, session_input, session_message, session_share, message, part, todo, permission, workspace, event, event_sequence, account, account_state, control_account, credential, migration, data_migration, __drizzle_migrations`。

关键表：
- **`session`**：`id`(text, 形如 `ses_...` 时间有序)、`project_id`、`parent_id`、`slug`、`directory`、`title`、`version`、`share_url`、`summary_*`、`revert`、`permission`、`time_created/updated/compacting/archived`、`workspace_id`、`path`、`agent`、`model`、`cost`、`tokens_input/output/reasoning/cache_read/cache_write`、`metadata`。
- **`message` / `part`**：结构化消息与分段（tool call/result、reasoning、text、snapshot 等）。
- **`session_input`**：`id, session_id, prompt, delivery, admitted_seq, promoted_seq` — 内部「输入准入 + 排队/steer」机制（**HTTP 未暴露 delivery**）。
- **`event`**：事件溯源表，类型如 `session.next.*`、`message.part.updated.1`、`session.created.1`。
- **`workspace`**：`type, name, branch, directory, project_id` — worktree 支持。
- **`permission`**：已记住的权限决策（`project_id, action, resource`）。

## 6. HTTP API 要点（v1）

Base：`http://<host>:<port>`；Spec：`GET /doc`。

**通用**：所有 session 相关请求支持 `?directory=<path>`（v2 还支持 `?workspace=`），因此**一个 server 可服务多个仓库目录**。

| 用途 | 方法/路径 |
|---|---|
| 健康检查 | `GET /global/health` |
| 全局事件 | `GET /global/event`（SSE） |
| 实例事件 | `GET /event`（SSE，首选） |
| 项目 | `GET /project`、`GET /project/current` |
| 路径/VCS | `GET /path`、`GET /vcs` |
| 配置/Provider | `GET|PATCH /config`、`GET /config/providers`、`GET /provider` |
| 列/建 session | `GET /session`、`POST /session` |
| session 状态 | `GET /session/status` |
| session 详情/操作 | `GET|DELETE|PATCH /session/:id`、`/children`、`/todo`、`/init`、`/fork`、`/abort`、`/share`、`/diff`、`/summarize`、`/revert`、`/unrevert` |
| 权限应答 | `POST /session/:id/permissions/:permissionID` |
| 取消息 | `GET /session/:id/message`、`GET /session/:id/message/:messageID` |
| 同步投递 | `POST /session/:id/message` |
| **异步投递** | `POST /session/:id/prompt_async`（返回 204） |
| 命令/Shell | `POST /session/:id/command`、`POST /session/:id/shell` |
| Agent | `GET /agent` |
| 日志 | `POST /log` |
| 文件 | `GET /find`、`/find/file`、`/find/symbol`、`/file`、`/file/content`、`/file/status` |
| 其他 | `GET /lsp`、`/formatter`、`/mcp`、`POST /mcp`、`PUT /auth/:id`、`/tui/*` |

**鉴权**：环境变量 `OPENCODE_SERVER_PASSWORD` 启用 HTTP Basic Auth；用户名默认 `opencode`，可用 `OPENCODE_SERVER_USERNAME` 覆盖。

### 关键请求体

`POST /session`：
```
body: { parentID?: string, title?: string }   // 注意：不能指定 id
query: { directory?: string }
→ 200 Session
```

`POST /session/:id/prompt_async`（与 `/message` 同结构）：
```
body: {
  messageID?: string,                 // 客户端生成，可用于幂等去重
  model?: { providerID, modelID },
  agent?: string,
  noReply?: boolean,
  system?: string,
  tools?: { [key]: boolean },         // 已废弃
  parts: Array<TextPartInput | FilePartInput | AgentPartInput | SubtaskPartInput>
}
query: { directory?, workspace? }
→ 204
```
v2 额外支持 `format?: OutputFormat`（结构化 JSON 输出）、`variant?: string`。

### 关键类型

```
SessionStatus = {type:"idle"} | {type:"retry",attempt,message,next} | {type:"busy"}
Session = { id, projectID, directory, parentID?, title, version, time{...}, summary?, share?, revert? }
Permission = { id, type, pattern?, sessionID, messageID, callID?, title, metadata, time }
```

### 事件（SSE）

- 状态：`session.status`(busy/idle/retry)、`session.idle`（deprecated 但存在）
- 消息：`message.updated`、`message.part.updated`、`message.part.delta`、`message.removed`
- 权限：`permission.asked`、`permission.replied`（v2：`permission.v2.asked/replied`）
- 提问：`question.asked`、`question.replied`、`question.rejected`（v2 同名）
- 其他：`session.created/updated/deleted/diff/error/compacted`、`todo.updated`、`file.edited`、`vcs.branch.updated`、`server.connected`
- v2 精细事件：`session.next.prompt.admitted/prompted`（含 `delivery: steer|queue`）、`session.next.step.*`、`session.next.tool.*`、`session.next.text.delta`、`worktree.ready/failed`、`workspace.ready/status`

## 7. 已知缺陷与并发约束（**最关键**）

1. **issue #46842**：当 session 处于 `Running` 时 `POST /session/:id/prompt_async`，消息会落库但**不会触发新一轮**（fork 的 prompt 等现有 run 结束就返回）。实测版本 1.18.x 存在。
2. v1 `prompt_async` **没有 `delivery` 字段**（`steer`/`queue` 是 v2 内部 `session_input.delivery` 概念，未暴露；见 issue #32157、#3194）。
3. **issue #35472**：`GET /session/status` 可能在 prompt 流已结束后仍短暂（偶发长期）报 `busy`；状态映射有异步延迟。

**规避要求（FlowHub 必须做）**：
- 每 session 维护本地队列，**单飞投递**，仅在 idle 时发下一轮；
- 订阅 SSE `session.status`/`session.idle` 判完成；`prompt_async` 返回后**乐观标记 busy**（存在准入到首个事件之间的竞态窗口）；
- 幂等：投递带客户端 `messageID`，webhook 重试时去重；
- 超时 ≠ 失败：不要重置/删除 session，标注「仍在运行」，可后续 `GET /session/:id/message` 或 `/abort`；
- 完成判定稳健条件：idle 且最后一条为 assistant 消息（加超时兜底防 stuck busy）。

## 8. 会话归属方案（同一任务同一 session）

opencode 不允许指定 `sessionID`，方案：

1. 建表 `task_session(task_key, repo, session_id, agent, state, created_at)`；
2. 任务首个事件（如 YouTrack issue 创建）→ `POST /session`（带 `title=<issue key>`、`?directory=<repo>`）→ 存返回的 `session_id`；
3. 后续每个 DevOps 事件按 `task_key` 查 `session_id`，追加一轮 `prompt_async`；
4. **不要**按 title 搜索定位（不可靠，title 会被自动改写）；title 仅作辅助；
5. 可选 `parentID` 建树（根=issue，子=步骤），但推荐**单 session 多轮次**（上下文连续、审计简单）。

## 9. 轮询 vs webhook（编排模型）

- **opencode 不轮询任何 DevOps 系统**。原因：每 session 同时只允许一个 drain loop，轮询会占住执行循环、烧 token；人工审批等事件不可预测。
- 正确模型：**事件驱动、一轮一事件**
  1. YouTrack issue webhook → FlowHub 建 session + 第 1 轮（分析）；
  2. 本轮 `session.idle` → FlowHub 回帖 / 触发 Gitea PR / 触发 Drone；
  3. Drone/Gitea 完成 webhook 回来 → 往**同一 session** 追加第 2 轮（修复/测试/发版）；
  4. 末轮结束 → 回帖汇报。
- 例外：短时只读查询可给 agent 配 MCP/tool 同步调用；「触发并等待外部长任务」应拆两轮。
- 若某系统不支持 webhook，则由 **FlowHub 轮询该系统**，而非 opencode。

## 10. 回帖汇报

- 推荐 **FlowHub 回写**：监听目标 session idle → `GET /session/:id/message` 取最后 assistant 内容 → 发 YouTrack comment。确定性强。
- 也可给 opencode 配 YouTrack/Gitea MCP 让 agent 直接发过程评论；最终汇总仍建议 FlowHub 兜底。
- 结构化结果：优先用 v2 的 `format: json_schema`。

## 11. CI 集成

- Drone/Gitea CI 步骤统一回调 FlowHub（而非各自直连 opencode），保证 session 映射单一真相。
- 备选：CI 内 `opencode run --attach <url> --dir <repo> -s <sessionID> ...`（同步、阻塞流水线、绕过 registry，不推荐）。
- 结论：CI 与 webhook 都收敛到「统一入口 + task_key」。

## 12. 权限（无人值守）

1. 专用 agent 的 `permission` 规则放开常规操作（推荐，粒度可控）；
2. 监听 `permission.asked` → FlowHub 按策略自动应答 `POST /session/:id/permissions/:permissionID`；敏感操作转 YouTrack 待审批；
3. `--auto`（危险，仅测试）。
建议默认 deny 危险操作 + 白名单，审批请求落 YouTrack 形成审计闭环。

## 13. 审计与 review

- opencode 侧：SQLite 结构化全量历史（`message`/`part`/`event`）+ `opencode export <id>` JSON 归档 + `POST /session/:id/share` 分享链接。
- 编排侧：registry 表 + 每轮事件日志（task_key、session_id、webhook payload、prompt、messageID、结果、耗时、token/成本）。
- 注意 DB 已 ~2.9GB，需归档/清理策略（`opencode session delete`）。
- 生产自动化实例建议用**独立 data 目录/实例**，避免与 Zed 人工会话混在同一 `opencode.db`。

## 14. `opencode acp` vs `opencode serve`

| | `opencode serve` | `opencode acp` |
|---|---|---|
| 协议 | HTTP REST + SSE（OpenAPI 3.1） | JSON-RPC over stdio（ACP）+ **内嵌 HTTP server** |
| 面向 | SDK、TUI、Web、外部服务 | 编辑器/IDE 子进程（Zed/JetBrains/Avante/CodeCompanion） |
| 网络 | 监听 host:port | stdio 给父进程；**同时监听 4096** |
| 共享 | 同进程/状态/DB | 同进程/状态/DB |

实测：Zed 拉起的 `opencode acp` 即 4096 的监听者，故浏览器可访问。生产应单独跑 `OPENCODE_SERVER_PASSWORD=xxx opencode serve --hostname 0.0.0.0 --port 4096`，与 IDE 隔离。

## 15. 插件自注册方案

**支持。插件工厂函数在启动时执行，`PluginInput` 含 `serverUrl: URL`。**

- 位置：`.opencode/plugins/*.{js,ts}`（项目级）、`~/.config/opencode/plugins/`（全局）；或 npm 包 + `opencode.json` `plugin: [...]`。
- `PluginInput`：`{ client, project, directory, worktree, experimental_workspace, serverUrl, $ }`。
- `Hooks`：`event`、`config`、`tool`、`auth`、`provider`、`chat.message/params/headers`、`permission.ask`、`command.execute.before`、`tool.execute.before/after`、`shell.env`、若干 `experimental.*`。
- **无 shutdown/dispose 钩子**。
- 加载顺序：全局 config → 项目 config → 全局插件目录 → 项目插件目录。

**注册草图**（`.opencode/plugins/register.ts`）：
```ts
import type { Plugin } from "@opencode-ai/plugin"

export const RegisterPlugin: Plugin = async ({ serverUrl, directory, worktree, project }) => {
  const registry  = process.env.BEAP_REGISTRY_URL!
  const token     = process.env.BEAP_REGISTRY_TOKEN!
  const advertised = process.env.BEAP_OPENCODE_ADVERTISED_URL ?? serverUrl.toString()
  const instanceId = crypto.randomUUID()

  const beat = () => fetch(`${registry}/opencode/instances/${instanceId}/heartbeat`, {
    method: "POST", headers: { authorization: `Bearer ${token}` },
  }).catch(() => {})

  void (async () => {                 // 不阻塞工厂返回
    for (let i = 0; i < 10; i++) {
      try {
        await fetch(`${registry}/opencode/instances`, {
          method: "POST",
          headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
          body: JSON.stringify({ instanceId, url: advertised, directory, worktree, project: project?.id }),
        })
        break
      } catch { await new Promise(r => setTimeout(r, 3000)) }
    }
    setInterval(beat, 30_000)
  })()

  return {
    event: async ({ event }) => {
      // 可选：session.idle / permission.asked / question.asked 回报给 registry
    },
  }
}
```
配置：放 `.opencode/plugins/` 自动加载，或 `{ "plugin": ["./.opencode/plugins/register.ts"] }`。

**插件相关坑**：
1. `serverUrl` 默认 `127.0.0.1:<port>`，跨机时须用 env 覆盖为对外广告地址；
2. 工厂执行可能早于端口 ready → registry 使用前先 `GET /global/health` 探活；
3. 无 dispose 钩子 → 用**心跳 + TTL** 注销（`process.on('SIGTERM')` 仅 best-effort）；
4. 多目录 server 可能多次加载 → 按 `instanceId+directory` 去重；
5. 插件在 opencode 进程（Bun）内运行，工厂体须 try/catch 且不阻塞；
6. 注册带共享 token；registry 回调 opencode 用 `OPENCODE_SERVER_PASSWORD` Basic Auth。

**更稳的补充**：叠加进程管理器（systemd/docker/k8s/pm2）负责「拉起后注册、退出时注销」，插件只做心跳与事件回报。

## 16. 推荐架构

```
YouTrack / Gitea / Drone
        │ (webhook 或 CI step 回调，统一入口)
        ▼
┌──────────────────────────────────────────────┐
│ FlowHub (Go, 统一 Webhook/编排服务)          │
│  · task_key → session_id 注册表               │
│  · 每 session 串行队列 + idle 检测            │
│  · SSE 订阅 /event，判轮次结束                │
│  · 回写 YouTrack / 触发下一步 CI              │
│  · 接收 opencode 自注册 + 心跳 (TTL)          │
└──────────────────────────────────────────────┘
        │ REST + SSE (Basic Auth)
        ▼
  opencode serve (常驻, OPENCODE_SERVER_PASSWORD)
        │
        ├─ POST /session
        ├─ POST /session/:id/prompt_async (仅 idle)
        ├─ GET  /event (SSE)
        ├─ GET  /session/:id/message
        └─ POST /session/:id/permissions/:permissionID
```

## 17. 待确认问题（交给下一个项目时先定）

1. 任务粒度：一个 issue 一个 session 贯穿全流程，还是每阶段子 session（`parentID` 树）？推荐前者。
2. 一个 headless server 服务多仓库（`?directory=`），还是每仓库一实例？取决于并发与权限隔离。
3. ~~编排服务技术栈？~~ 已定：**Go**（FlowHub）。
4. 是否需要人在回路审批？审批入口是 YouTrack 评论回复还是别的？
5. 是否锁定 opencode 1.18.31？修复 `prompt_async` 吞消息缺陷后，编排层串行化可简化。
6. registry 与 opencode 是否同机？决定广告地址与网络策略。
7. 事件回报交给插件，还是外部适配层订阅 SSE？
8. 部署形态（裸进程/systemd/docker/k8s）？是否需独立 data 目录/固定端口。

## 18. 参考资料

- opencode Server API：https://opencode.ai/docs/server/
- opencode SDK：https://opencode.ai/docs/sdk/
- opencode 插件：https://opencode.ai/docs/plugins/
- ACP 支持：https://opencode.ai/docs/acp/
- issue #46842（prompt_async 吞消息）：https://github.com/anomalyco/opencode/issues/46842
- issue #35472（status busy 延迟）：https://github.com/anomalyco/opencode/issues/35472
- issue #32157（queue vs steer）：https://github.com/anomalyco/opencode/issues/32157
- Session 框架内部分析：https://jczhu.com/blog/opencode-session-framework-internals/

## 19. 同类开源项目调研

> 调研时间：2026-09（信息来自公开搜索与项目 README，star/许可可能变化）。
> 结论先行：**没有一个项目原生覆盖「Gitea + YouTrack + Drone + opencode」组合**，绝大多数以 GitHub 为中心；也**没有项目以 opencode 为执行引擎做服务端 session 持久化编排**。本 Hub 有明确空白位，但可复用大量现成组件与设计。

### 19.1 直接对标：issue → 开发 → 测试 → PR/CI/CD

| 项目 | 语言 / 许可 | 能力 | 与本项目的差异 |
|---|---|---|---|
| **OpenHands**（原 OpenDevin） | Python / MIT | 自主 SWE，本地或沙箱；接 GitHub / GitLab / Bitbucket；Gitea 仍在 feature request（#12351） | 无 Gitea 落地；不接 YouTrack / Drone |
| **SWE-agent** | Python / MIT | 学术向，读 issue 修 bug 跑测试 | 偏研究，非工作流 Hub |
| **Open SWE**（LangChain） | Python / MIT | 「software factory」：issue/Slack/Linear 触发 → 沙箱实现 → 验证 → PR → 盯 CI/评审；Deep Agents + LangGraph；FastAPI + swagger.json | 只接 GitHub/Slack/Linear；较重（依赖 LangGraph 运行时） |
| **Agent Orchestrator**（Untrivial-ai，原 ComposioHQ） | Apache-2.0 | 26 种 harness **含 opencode**；每任务独立 worktree + PR；自动重试 CI、处理 review、PR 生命周期 | 桌面 app + 本地 daemon，非服务端 Hub；不接 YouTrack / Drone |
| **Bernstein** | Python / Apache-2.0 | 确定性调度器（编排零 token）+ Janitor 合并前校验（lint/type/test） | 单机、单人维护；GitHub 向 |
| **Emdash** | Electron / Apache-2.0 | 34 种 CLI agent；ticket 入口含 **Gitea/Forgejo/Jira/Linear/GitLab**；`$EMDASH_PORT` 解决端口冲突 | 桌面 app；无 agent 间协作 |
| **Baton**（mraza007） | Python / MIT | 轮询 GitHub issue → Claude Code → worktree；`WORKFLOW.md` 配置 | 极简、仅 GitHub |
| **Code Conductor**（ryanmac） | MIT | GitHub issue 打标 `conductor:task`，agent 认领 | 仅 Claude Code，早期 |
| **Microsoft Conductor** | Python / MIT | YAML 定义多 agent 工作流；并行/子工作流/脚本步/MCP 步/人工 gate；Web 看板 + Fleet TUI | 通用编排，非 DevOps 节点集成 |
| **issue-orchestrator** | Python | GitHub issue 编排 + guardrails | GitHub 专用 |
| **Vibe Kanban** | Apache-2.0 | Kanban + MCP 任务分解；10+ agent | 人机协同 UI；已转社区维护 |
| **Claude Squad** | AGPL-3.0 | TUI + tmux + worktree，多 agent 并行 | 人机协同；AGPL 商用需注意 |
| **Nimbalyst**（原 Crystal） | 开源 | 桌面并行会话 + 可视化编辑 | 桌面 app |
| **Agent Kanban** | ELv2 | VS Code + Copilot 的 markdown Kanban | 仅 Copilot |

### 19.2 平台/编排层（可作 Hub 底座）

- **kagent**（CNCF Sandbox）：Kubernetes 原生 agent 框架，agent/session/tool 均为 CRD，天然 GitOps/RBAC/准入控制 + OTel。**最接近「agent + DevOps 汇聚」的云原生底座。** https://kagent.dev/
- **Durable execution**：Temporal / Restate / Inngest / DBOS —— 长流程、断点续跑、`durable human approval`（正好对应「等 CI / 等人审批再继续」）。
- **通用工作流**：n8n、Windmill（支持 Go/Python/TS 代码步骤）、Kestra（YAML 声明式）、Activepieces（MIT）、Node-RED —— 连接器丰富，但 agent 只是其中一个节点。
- **Dagger**：把 CI/CD 写成可编程 pipeline，可作为执行层。

### 19.3 连接协议（agent ↔ IDE / 工具）

- **MCP**（Model Context Protocol）：工具/数据接入事实标准。
- **ACP**（Agent Client Protocol，Zed 主导）：编辑器 ↔ agent，opencode 已支持。
- **opencode 自带 GitHub/GitLab 集成**：`/opencode`、`/oc` 评论触发；支持 `issue_comment`、`pull_request`、`issues`、`pull_request_review_comment`、`schedule`、`workflow_dispatch`；跑在 Actions runner 内。**无 Gitea 支持。**

### 19.4 结论与借鉴建议

1. **市场空白**：Gitea + YouTrack + Drone 这套自托管组合几乎无原生支持；且无项目以 opencode 为执行引擎做服务端 session 持久化编排（Agent Orchestrator 虽支持 opencode，但是桌面 worktree 模型，session 归其自管）。
2. **FlowHub 定位**：不重造 agent runtime，直接驱动 `opencode serve`；价值放在 **connector（YouTrack/Gitea/Drone）+ session 编排 + 审计**。
3. **可借鉴组件**：
   - **Bernstein**：确定性调度 + 合并前校验（Janitor）思路；
   - **Open SWE**：「thread = 持久会话 + 多次 invocation」模型（对应本项目「同任务多轮同 session」）；
   - **Microsoft Conductor**：YAML 工作流 + 人工 gate + Fleet 看板；
   - **kagent**：K8s 场景下作为底座；
   - **Temporal / Restate**：长流程持久化与人工审批。
4. **项目命名**：已定名 **FlowHub**（Go）。此前候选 `Baton`（mraza007/baton）、`Conductor`（microsoft/conductor）均已被占用，`Agent Orchestrator`、`Agent Hub` 亦有同名项目；采用 FlowHub 前建议再查一次 npm / GitHub / 域名重名。

### 19.5 调研参考链接

- OpenHands：https://github.com/OpenHands/OpenHands （Gitea 需求 #12351）
- Open SWE：https://github.com/langchain-ai/open-swe
- Agent Orchestrator：https://github.com/Untrivial-ai/agent-orchestrator
- Bernstein：https://github.com/sipyourdrink-ltd/bernstein
- Emdash：https://github.com/generalaction/emdash
- Baton：https://github.com/mraza007/baton
- Microsoft Conductor：https://github.com/microsoft/conductor
- issue-orchestrator：https://github.com/issue-orchestrator/issue-orchestrator
- Vibe Kanban：https://github.com/BloopAI/vibe-kanban
- Claude Squad：https://github.com/smtg-ai/claude-squad
- kagent：https://kagent.dev/ 、https://github.com/kagent-dev/kagent
- opencode GitHub 集成：https://opencode.ai/docs/github/
- 开源 agent orchestrator 综述：https://www.augmentcode.com/tools/open-source-agent-orchestrators
