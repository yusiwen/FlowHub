# opencode serve 自动化实操：会话、Web UI 与权限处理（实测补充文档）

> 项目：**FlowHub**（Go）
> 定位：本文是 [`opencode-devops-orchestration-design.md`](./opencode-devops-orchestration-design.md) 的**补充与校正**，聚焦「用 HTTP API headless 驱动 opencode」的接口细节，以及编排层最难处理的一环 —— **交互阻塞（权限询问）**。
> 来源：一次 opencode serve 实操会话（本机 opencode **1.18.31**，macOS，server 监听 `127.0.0.1:4096`）。
> 标记约定：**✅ 本机实测** ｜ **⚠️ 源码/客户端 bundle 推断**（本地 clone 为 1.14.21，与运行版有差异） ｜ **❓ 未验证**

---

## 0. 结论速览

| # | 问题 | 结论 |
|---|---|---|
| 1 | 能不能给「某个项目」建 session 并发起对话？ | **能**。`POST /session?directory=<绝对路径>` 建会话 → `POST /session/{id}/prompt_async` 投递 ✅ |
| 2 | session 归属怎么定？ | 由建会话时的 `directory` 决定，**创建后不必重复传**；`sessionID` 服务端生成，外部不能指定（与设计文档 §8 一致）✅ |
| 3 | 权限弹窗怎么处理？ | 两条路：**事前**给规则（session 级 `permission` / 项目 `opencode.json` / prompt 级 `tools`），**事中**轮询 `GET /permission?directory=` 自动应答 ✅ |
| 4 | 无人值守最容易踩的默认询问？ | `external_directory`（碰 worktree 外路径）、`doom_loop`（同一工具+同样参数连续 3 次）、`read *.env` ✅ |
| 5 | API 建的 session 在 Web UI 看得到吗？ | **看得到**，但首页/搜索默认看不到 —— 需要用深链接 `/{base64url(dir)}/session/{id}`，或把该目录「添加项目」✅ |
| 6 | v2 `/api/session` 能用于自动化吗？ | **不能**。它只把 prompt 写进持久事件日志，**没有 runner 就不会执行**（Web UI 就是它的 runner）✅ |

一句话给 FlowHub：**v1 端点 + `?directory=` + 轮询 `GET /permission` 自动应答**，就是无人值守的最小闭环。

---

## 1. 环境与验证方式

| 项 | 值 |
|---|---|
| opencode 版本 | `1.18.31`（`/Users/yusiwen/.opencode/bin/opencode`） |
| Server | `127.0.0.1:4096`，PID **55437**，`lsof` 显示为 `opencode` 进程 |
| OpenAPI | `GET /doc` → 478,968 bytes，**162 个 path**，OpenAPI 3.1 |
| 鉴权 | `components.securitySchemes` 为空、全局 `security: []` → **本机实例无鉴权**（生产必须 `OPENCODE_SERVER_PASSWORD`） |
| 模型 | `deepseek/deepseek-v4-flash`（`GET /provider` 的 `connected`: deepseek, opencode, openrouter, minimax-cn, ollama, dgxspark） |
| 验证方式 | 全部结论出自真实调用（临时目录 `/private/tmp/oc-*`），逐条记录请求与返回；服务端日志双源交叉验证（`~/.local/share/opencode/log/opencode.log`） |

> 与设计文档 §3 的交叉验证：§3 记录的 `PID 55437 opencode acp`（父进程 Zed 55315）与本轮 `lsof` 一致 —— **Zed 的 ACP 进程与 4096 是同一个**，本轮所有实测都发生在用户真实实例上。

---

## 2. 会话创建与「项目绑定」

### 2.1 建会话

```bash
curl -s -X POST "http://localhost:4096/session?directory=/private/tmp/oc-api-demo" \
     -H 'Content-Type: application/json' -d '{}'
```

返回（真样例）：

```json
{
  "id": "ses_f483b3ff0ffewU7YN75eua8VvL",
  "slug": "nimble-nebula",
  "projectID": "global",
  "directory": "/private/tmp/oc-api-demo",
  "path": "",
  "cost": 0,
  "tokens": {"input": 0, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}},
  "title": "perm-ask test",
  "version": "1.18.31",
  "time": {"created": 1789789519887, "updated": 1789789519887}
}
```

### 2.2 请求体字段（比设计文档 §6 记得的多）

| 字段 | 类型 | 说明 |
|---|---|---|
| `parentID` | `^ses` | 建子会话（设计文档 §8 的树形方案） |
| `title` | string | 会话标题；**API 建的会话之后会被自动改写**（实测：`{}` 建出来叫 “New session - 2026-09-19T01:24:10.828Z”，跑完一轮后变成「列出当前目录文件并查看 README 首行」）✅ |
| `agent` | string | `build` / `plan` / 自定义 agent |
| `model` | `{providerID, id, variant?}` | 会话默认模型（注意这里是小写 `id`，与投递时的 `modelID` 不同） |
| `metadata` | object | 任意元数据（可放 `task_key` 之类，便于审计） |
| `permission` | `PermissionRuleset` | **会话级权限规则**（本文 §5.3 的主角）✅ |
| `workspaceID` | `^wrk` | 绑定 workspace（worktree 场景） |

Query：`directory`（= 项目根，**必传**）、`workspace`。

### 2.3 ⚠️ 坑一：目录必须是**规范路径**，symlink 不会自动展开

macOS 上 `/tmp` 是 `/private/tmp` 的 symlink。实测两个 session：

| session | 建会话时传的目录 | 实际存下的 `directory` | Web UI 里可见？ |
|---|---|---|---|
| `ses_f48bc7a3…`（v1 建） | `/private/tmp/oc-api-demo` | `/private/tmp/oc-api-demo` | ✅ 可见 |
| `ses_f48bc529…`（v2 建） | `/tmp/oc-api-demo` | `/tmp/oc-api-demo`（**原样**） | ❌ 侧栏不出现，只能深链接打开 |

同一目录、两个不同的字符串 → 后续按目录过滤、Web UI 侧栏归类全都对不上。**FlowHub 落库前请 `realpath()`**（脚本里已这么写）。

### 2.4 项目（projectID）与过滤

- `GET /project` 返回 22 条；临时目录**不在其中**，所以这些 session 的 `projectID` 都是 `"global"`（内置默认项目，worktree=`/`）。
- `GET /session?directory=<abs>` 会做 **symlink 归一**：用 `/tmp/oc-api-demo` 去查也能命中 `/private/tmp/...` 的那条（v1 端点）✅。
- v2 的 `GET /api/session?location[directory]=<abs>` **过滤无效**：照样返回全部 50 条 ❌。

---

## 3. 发起对话

### 3.1 两个投递端点

| 端点 | 语义 | 返回 | 实测 |
|---|---|---|---|
| `POST /session/{id}/message` | **同步**：阻塞到本轮结束 | `{info, parts}`（assistant 消息本体） | 一轮 3.6s 返回 ✅ |
| `POST /session/{id}/prompt_async` | **异步**：立刻返回 | `204`，无 body | ✅ |

编排层（FlowHub）用 `prompt_async` + 轮询/SSE 判完成；本地脚本用同步端点更省心。

请求体（两者同构）：

| 字段 | 说明 |
|---|---|
| `parts` | **必需**。`[{type:"text",text:"..."}]`、`FilePartInput`、`AgentPartInput`、`SubtaskPartInput` |
| `model` | `{providerID, modelID}`（注意与建会话时的 `{id}` 字段名不同） |
| `agent` | 覆盖 agent（如 `build` / `plan`） |
| `tools` | `{"bash": false}` **可按条消息禁用工具** —— 设计文档 §6 标注「已废弃」，实际 1.18.31 仍生效 ✅ |
| `system` | 覆盖 system prompt ❓未测 |
| `noReply` | 只写入不触发模型 ❓未测 |
| `format` | 结构化输出 `OutputFormat`（v2 更完整）❓未测 |
| `messageID` | 客户端生成（`^msg`）用于幂等 ❓未测 |

> ⚠️ **调用方注意**：`POST` 到 session 的端点同时也接受 `?directory=`，但**创建后就不是必需的**（session 记住了目录）。实测：不传 `directory` 再发一条，assistant 的 `path.cwd` 仍是建会话时的目录 ✅。

### 3.2 响应结构（真样例）

`POST /session/{id}/message` 返回一条 assistant 消息，`info` 关键字段：

```json
{
  "id": "msg_0b7439c47001IJuFiQyApxUClX",
  "role": "assistant",
  "agent": "build",
  "providerID": "deepseek", "modelID": "deepseek-v4-flash",
  "parentID": "msg_...", "mode": "build", "finish": "stop",
  "cost": 9.2682e-05,
  "tokens": {"total": 15978, "input": 211, "output": 23, "reasoning": 0,
             "cache": {"read": 15744, "write": 0}},
  "path": {"cwd": "/private/tmp/oc-api-demo", "root": "/private/tmp/oc-api-demo"},
  "time": {"created": 1789781050828, "completed": 1789781051298}
}
```

`parts[]` 的类型与工具状态机：

- `step-start` / `text` / `reasoning` / `tool` / `step-finish`（一次「推理→调用→观察」= 一个 step）
- `tool` part 的 `state.status`：`pending` → `running` → `completed` | `error`
  - **`running` 卡住不动的典型原因就是「在等权限应答」**（实测：工具 part 停在 `running`，服务端日志同时出现 `message=asking`）✅
  - `error` 时带 `error` 文本，例如 `The user rejected permission to use this specific tool call.`

### 3.3 完成判定（实测，FlowHub 直接用）

```bash
# 运行中：只有带上 directory 才有内容
GET /session/status?directory=/private/tmp/oc-perm-test
→ {"ses_f483b3ff0ffewU7YN75eua8VvL": {"type": "busy"}}

# 空闲（以及不带 directory 参数时）
→ {}
```

要点：

1. **`directory` 不能省** —— 省掉后所有 session 端点按 server 自己的默认 location（本机是 `/Users/yusiwen/Documents`）过滤，返回 `{}` 或 `[]`，看起来像「没有任何 pending/任务」。这一条同时适用于 `GET /permission`、`GET /session/status`。✅
2. `session.status` 为 `{}` ≠ 任务完成：**权限 pending 期间状态是 `busy`**，所以「idle」本身足以说明没有卡在询问上（实测：per_ 出现时 busy，应答后立刻 `{}`）。✅
3. 更稳的完成判定：`status` 非 busy **且** 出现新的 `assistant` 消息且其 `info.time.completed` 存在 ✅（`time.completed` 只在真正结束时写入）。

---

## 4. Web UI 可见性（排障 / 人工 review 用）

Web UI 就是 `http://localhost:4096/`（根路径与 `/app` 返回同一份 SPA HTML，非 API 路径一律 fallback 到它）。

### 4.1 为什么首页看不到 API 建的 session

首页/侧栏只展示**「你在 UI 里添加过的项目」**下的会话，而这个项目列表存在**浏览器 localStorage**里（`opencode.global.dat:server`），与服务端 `GET /project` 无关：

```json
{"list":[],"projects":{"local":[{"worktree":"/private/tmp/oc-api-demo","expanded":true}]},
 "lastProject":{"local":"/private/tmp/oc-api-demo"},"recentlyClosed":{}}
```

所以 API 建的 session 在首页是空状态，顶部搜索框也搜不到（搜索是**项目内**搜索：加了项目后 placeholder 变成「在 oc-api-demo 中搜索会话」）。✅

### 4.2 深链接（最快，已两个 session 验证）

路由（来自客户端 bundle ⚠️ + 实测 ✅）：

```
/{base64url(目录绝对路径，无 padding)}/session/{sessionID}
  → 打开后自动跳到 /server/{base64url(server URL)}/session/{sessionID}
```

生成命令：

```bash
b64url() { printf '%s' "$1" | base64 | tr '+/' '-_' | tr -d '='; }
b64url /private/tmp/oc-api-demo     # → L3ByaXZhdGUvdG1wL29jLWFwaS1kZW1v
b64url /tmp/oc-api-demo             # → L3RtcC9vYy1hcGktZGVtbw
# 完整链接：
# http://localhost:4096/L3ByaXZhdGUvdG1wL29jLWFwaS1kZW1v/session/ses_f48bc7a33ffe5K9sinuttgCQ3e
```

打开后整段会话（含工具调用 `Shell pwd` → 输出）完整渲染 ✅；`/server/...` 形态里的 serverKey 是 `base64url("http://localhost:4096")` = `aHR0cDovL2xvY2FsaG9zdDo0MDk2`。

### 4.3 让它出现在侧栏（持久做法）

侧栏「添加项目」→ 目录选择框里直接贴绝对路径 → 选中。随后侧栏出现项目节点，「今天」下面列出该目录的 v1 session ✅。副作用：只写 localStorage，不动服务端；不想要就在 UI 里移除或清掉那个 key。

### 4.4 客户端还支持 header 传目录（Go 端可参考）

bundle 里 Web UI 一律用 `x-opencode-directory` header 传目录，并在发出前做转换：路径以 `/api/` 开头 → 转成 query `location[directory]=`，否则 → 转成 `directory=`（并删掉 header）。⚠️ 读代码所得

服务端**两种形式都接受**：我实测用 `x-opencode-directory: /private/tmp/oc-perm-test`（原始路径与 URL-encode 形式都试了）请求 `GET /permission` 与 `POST /permission/{id}/reply`，结果与 `?directory=` 完全一致 ✅。FlowHub 的 HTTP 客户端用哪种都行。

---

## 5. 权限系统（无人值守的核心）

### 5.1 默认规则集（源码 ⚠️ + 逐条实测 ✅）

`agent/agent.ts` 里的内置默认（用户 config 的 `permission` 会叠加在其后）：

```
"*": "allow"                                      # 默认全放行 → 普通 bash/edit 不会问
doom_loop: "ask"                                  # 同一工具 + JSON 完全相同的参数连续 3 次
external_directory: { "*": "ask", <skill 目录>: "allow" }   # 碰 worktree 之外的路径
read: { "*": "allow", "*.env": "ask", "*.env.*": "ask", "*.env.example": "allow" }
question: "deny"                                  # 模型不能反问（所以工具列表里没有 question）
plan_enter / plan_exit: "deny"
```

实测验证：

| 场景 | 结果 |
|---|---|
| 普通 bash / 读项目内文件 | 静默执行 ✅ |
| 读 worktree 之外的 `/private/tmp/outside-note.txt` | ⚠️ 询问 `external_directory`，`patterns: ["/private/tmp/*"]` ✅ |
| 读项目内 `.env` | ⚠️ 询问 `read`，`patterns: [".env"]` ✅ |
| 连续 3 次相同工具+参数 | `doom_loop` 询问（阈值 `DOOM_LOOP_THRESHOLD = 3`，源码 ⚠️，未构造复现） |

> 对 FlowHub 的含义：**跨仓库操作（读别的 repo、往 /tmp 写产物）会稳定触发 `external_directory` 询问**；`.env` 读取也会。这两类要么在配置里放行，要么纳入审批策略。

### 5.2 评估算法（源码 ⚠️，与实测行为一致）

```
evaluate(permission, pattern, ...rulesets):
    rules = flatten(rulesets)                      # 顺序：session ruleset → approved → 配置/默认
    match = rules.findLast(r => wildcard(r.permission, permission) && wildcard(r.pattern, pattern))
    return match ?? { action: "ask" }              # 都没有就默认 ask
```

`permission.ask(patterns[])` 对每个 pattern 逐一评估：

- 任一 → `deny`：立刻 `PermissionDeniedError`（提示相关规则），**工具不会执行**
- 全部 `allow`：直接放行，不产生任何事件
- 其余（`ask`）：挂起并发布 `permission.asked` 事件，**工具 part 停在 `running`，session 保持 `busy`**

`fromConfig` 会**把顶层 key 排序**（通配的 `*`、`mcp_*` 排在具体 key 之前），配合 `findLast` 实现「具体规则覆盖通配规则」，与 JSON 里 key 的书写顺序无关 ⚠️。

### 5.3 三层控制手段与优先级（全部实测）

| 手段 | 写法 | 实测结果 |
|---|---|---|
| **项目配置** | `<项目>/opencode.json`：`{"permission":{"bash":"allow"}}` | ✅ 生效 —— 但**必须在该目录实例首次加载之前就存在**。我后来把一个已经用过的目录改成 `bash:"deny"`，服务端仍按旧配置放行 → **改配置要重启 server**（或换新目录） |
| **会话级规则** | `POST /session` body 的 `permission: [{"permission":"bash","pattern":"*","action":"ask"}]` | ✅ 立即生效，且**优先级高于配置**（配置 `bash:"allow"` + 会话规则 `ask` → 实测仍然弹询问） |
| **prompt 级禁工具** | `POST .../prompt_async` body：`"tools":{"bash":false}` | ✅ 生效，模型收到 `The arguments provided to the tool are invalid: Model tried to call unavailable tool 'bash'`，随后改用别的方式 |
| 细粒度对象写法 | `{"permission":{"bash":{"*":"allow","date *":"ask"}}}` | ✅ 生效（`pwd` 静默 / `date` 命中 `date *` 询问） |
| `deny` 的效果 | `{"permission":{"bash":"deny"}}` | ✅ 工具**从工具表里消失**，模型自己说「我没有可用的 bash 执行工具」（源码 `Permission.disabled()`：仅当规则 pattern 为 `*` 且 action 为 `deny` 时移除工具） |
| ~~`PATCH /config`~~ | `PATCH /config?directory=<dir>` `{"permission":{"bash":"deny"}}` | ❌ **实测无效**：接口返回 200、往项目目录写了个 `config.json`，但 `GET /config` 仍显示旧值、bash 照样执行。**别用它做运行时开关** |
| `--auto` | `opencode serve --auto` | ❓ 未验证。主 `--help` 列出该开关（“auto-approve permissions that are not explicitly denied (dangerous!)”），但 `opencode serve --help` 未列；未为测试另起 server 实例（会共享同一 DB） |

**推荐组合**（FlowHub）：项目 `opencode.json` 里放宽常规操作（`bash/edit/external_directory: allow`），**危险操作保持 `ask` 或 `deny`**；需要审计时给会话加 session 级规则把关注工具设成 `ask`，由编排层自动应答 + 落 YouTrack。

### 5.4 发现与应答端点（v1）

| 用途 | 端点 | 实测 |
|---|---|---|
| **列出待批准权限** | `GET /permission?directory=<abs>` | ✅ 返回**所有 session** 的 pending 数组（自己按 `sessionID` 过滤） |
| **全局应答** | `POST /permission/{requestID}/reply?directory=<abs>`，body `{"reply":"once"\|"always"\|"reject","message":"可选反馈"}` | ✅ 返回 `true`；`message` 会作为反馈给模型（源码 `CorrectedError`）⚠️ |
| 会话内应答（旧） | `POST /session/{id}/permissions/{permissionID}?directory=<abs>`，body `{"response":"once"}` | ✅ 返回 `true`（**注意字段名是 `response` 不是 `reply`**） |
| 事件流 | `GET /event?directory=<abs>`（SSE）→ `permission.asked` / `permission.replied` | ✅ 收到（不带 directory 只收到 `server.connected` 与心跳） |
| 模型反问 | `GET /question`、`POST /question/{requestID}/reply`、`/reject` | 端点存在；`question` 默认 `deny`，本轮未触发 ❓ |
| ❌ 不要用 | `GET /api/session/{id}/permission`（v2） | 对 v1 会话**始终返回 `[]`**，加 `location[directory]` 也一样 |

事件体真样例（`permission.asked`）：

```json
{
  "id": "evt_0b7c5fdb80020AePEnwycr3gPl",
  "type": "permission.asked",
  "properties": {
    "id": "per_0b7c5fdb8001oVxJMFSLlpB9g8",
    "sessionID": "ses_f483b3ff0ffewU7YN75eua8VvL",
    "permission": "bash",
    "patterns": ["whoami"],
    "metadata": {"command": "whoami"},
    "always": ["whoami *"],
    "tool": {"messageID": "msg_...", "callID": "call_00_..."}
  }
}
```

注意 `always` 字段：它是「答复 `always` 时会被记住的模式」，由 bash 命令的 arity 表推导出来（`whoami` → `whoami *`，`uname -s` → `uname *`）。

服务端日志（排障用，`~/.local/share/opencode/log/opencode.log`）：

```
level=INFO message=evaluated permission=bash pattern=pwd action.permission=bash action.pattern=* action.action=ask
level=INFO message=asking id=per_0b7c4d2df001ezt2j4pYUeFXnw permission=bash patterns="[\"pwd\"]"
```

### 5.5 应答语义（源码 ⚠️ + 实测 ✅）

| 应答 | 行为 | 实测 |
|---|---|---|
| `once` | 仅放行本次 | ✅ `uname -s` 放行 → 输出 `Darwin` |
| `always` | 把 `always[]` 里的模式写入**该目录实例的已批准列表**（内存） | ✅ 之后同一目录**新会话**再跑 `whoami` 也直接执行，不再询问 |
| `reject` | 该工具调用直接失败，模型收到工具错误后继续 | ✅ 错误文本 `The user rejected permission to use this specific tool call.`；同一 session 内**其它 pending 请求会被一并拒绝**；期间不会挂死，run 正常收尾 |

补充说明（源码）：

- `always` 的「已批准」列表挂在**目录实例**上（`InstanceState`），**跨 session 生效、server 重启即失效**；DB 里那张 `permission` 表在运行期**没有写入路径**（只在旧的 JSON → SQLite 迁移里写过）⚠️ —— 设计文档 §5 把它描述为「已记住的权限决策」需要修正。
- 带 `message` 的 `reject` 会变成 `CorrectedError`，模型能读到你的反馈（`...with the following feedback: <message>`）⚠️。
- `reply` 端点只用 `requestID` 定位，不带 `sessionID`；**多 session 并发 pending 时要自己维护 `requestID → task_key` 映射**（本轮只在单 session 上验证）。

### 5.6 无人值守策略建议（对应设计文档 §12）

1. **事前**：项目 `opencode.json` 放宽常规工具；专门 agent（如 `.opencode/agent/devops.md`）里再叠一层 `permission`，粒度比全局更细。
2. **事中**：轮询 `GET /permission?directory=`（1–2s 间隔足够，实测权限请求在 2s 内可见）→ 按策略：
   - 只读、幂等命令 → `always`（减少后续噪音）
   - 危险命令（`sudo`、`rm -rf`、`git push`、`curl|sh`）→ `reject`（必要时带 `message` 给出替代指令）
   - 拿不准 → `reject` + 转 YouTrack 人工审批（`reject` 不会挂死 run，安全）
3. **兜底**：`session.status` 长时间 `busy` + 工具 part 长时间 `running` + 有 pending permission ⇒ 判定为「等待审批」，不要误判 idle 后重复投递（设计文档 §7 的 `prompt_async` 吞消息坑会因此被放大）。
4. 长任务注意 `doom_loop`（同一调用连续 3 次即询问）；重试逻辑应由编排层做，不要靠模型自己重试。

---

## 6. 参考实现（脚本）

可运行副本：`~/.local/bin/oc_run_task.py`（755）。它把上面的最小闭环串起来：建会话（带会话级规则）→ `prompt_async` → 轮询 `GET /permission` 按策略应答 → 用 `session/status` + `info.time.completed` 判完成 → 取最终文本。

```bash
oc_run_task.py /Users/yusiwen/git/mine/FlowHub "用 bash 看一下 git 状态并总结"
# 环境变量可选：
#   OPENCODE_URL=http://localhost:4096
#   OPENCODE_MODEL=deepseek/deepseek-v4-flash
#   OPENCODE_AGENT=build
```

```python
#!/usr/bin/env python3
"""Unattended opencode task runner over the local `opencode serve` HTTP API.

Verified against opencode 1.18.31 (macOS, 2026-09-19).

Flow:
  1. create a session bound to the project directory (canonical path, no symlink)
  2. send the task with prompt_async (204, non-blocking)
  3. poll GET /permission?directory=<dir> and auto-answer by policy
  4. wait for idle (GET /session/status?directory=<dir>) and print the final text

Usage:
  oc_run_task.py <project_dir> "<task>"

Env overrides:
  OPENCODE_URL    default http://localhost:4096
  OPENCODE_MODEL  default deepseek/deepseek-v4-flash  ("provider/model")
  OPENCODE_AGENT  default build
"""
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("OPENCODE_URL", "http://localhost:4096")
PROVIDER, _, MODEL_ID = os.environ.get(
    "OPENCODE_MODEL", "deepseek/deepseek-v4-flash").partition("/")
AGENT = os.environ.get("OPENCODE_AGENT", "build")

# Session-level permission ruleset (overrides project/global config, see doc §6).
RULESET = [
    {"permission": "bash", "pattern": "*", "action": "ask"},
    {"permission": "edit", "pattern": "*", "action": "ask"},
]

# Auto-reply policy for permission requests.
DENY_PREFIX = ("sudo ", "rm -rf /", "git push", "npm publish", "curl ", "wget ")
ALWAYS_PREFIX = ("pwd", "ls", "uname", "git status", "git branch",
                 "cat ", "head ", "tail ", "grep ", "wc ")


def api(path, method="GET", body=None, timeout=120, directory=None, **params):
    """One HTTP call. `directory` is sent as ?directory= (every session-scoped
    endpoint supports it; omitting it silently falls back to the server's own
    default location and returns empty results)."""
    if directory is not None:
        params["directory"] = directory
    url = BASE + path + ("?" + urllib.parse.urlencode(params) if params else "")
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:400]


def decide(patterns):
    cmd = " ".join(patterns)
    if cmd.startswith(DENY_PREFIX):
        return "reject"
    if cmd.startswith(ALWAYS_PREFIX):
        return "always"          # remembered per directory instance, cross-session, lost on restart
    return "once"


def completed_assistant(sid):
    """Assistant messages that actually finished (info.time.completed is set)."""
    _, msgs = api(f"/session/{sid}/message")
    return [m for m in (msgs or [])
            if m.get("info", {}).get("role") == "assistant"
            and m.get("info", {}).get("time", {}).get("completed")]


def last_text(sid):
    _, msgs = api(f"/session/{sid}/message")
    texts = [p.get("text", "") for m in (msgs or []) for p in m.get("parts", [])
             if p.get("type") == "text" and p.get("text")]
    return texts[-1] if texts else ""


def is_busy(sid, directory):
    _, status = api("/session/status", directory=directory)
    return (status or {}).get(sid, {}).get("type") in ("busy", "retry")


def run(project_dir, task, poll=1.5, deadline=600):
    project_dir = os.path.realpath(project_dir)          # /tmp vs /private/tmp matters!
    status, ses = api("/session", "POST",
                      {"title": task[:60], "permission": RULESET},
                      directory=project_dir)
    if status != 200:
        raise SystemExit(f"create session failed: {status} {ses}")
    sid = ses["id"]
    print(f"session {sid} in {ses['directory']}")

    baseline = len(completed_assistant(sid))
    status, _ = api(f"/session/{sid}/prompt_async", "POST",
                    {"agent": AGENT,
                     "model": {"providerID": PROVIDER, "modelID": MODEL_ID},
                     "parts": [{"type": "text", "text": task}]},
                    directory=project_dir)
    if status != 204:
        raise SystemExit(f"prompt_async failed: {status}")

    handled, t0 = 0, time.time()
    while time.time() - t0 < deadline:
        for req in (api("/permission", directory=project_dir)[1] or []):
            if req.get("sessionID") != sid:
                continue
            reply = decide(req["patterns"])
            api(f"/permission/{req['id']}/reply", "POST", {"reply": reply},
                directory=project_dir)
            handled += 1
            print(f"  permission {req['permission']} {req['patterns']} -> {reply}")
        if len(completed_assistant(sid)) > baseline and not is_busy(sid, project_dir):
            if not (api("/permission", directory=project_dir)[1] or []):
                return sid, handled, last_text(sid)
        time.sleep(poll)
    return sid, handled, None


if __name__ == "__main__":
    if len(sys.argv) < 3:
        raise SystemExit(__doc__)
    sid, n, text = run(sys.argv[1], sys.argv[2])
    print(f"\n== session {sid} finished, auto-answered {n} permission request(s) ==")
    print(text if text is not None else "(timeout - session may still be running)")
```

**脚本验证状态（如实说明）**：脚本的每一步（建会话+会话级规则、`prompt_async` 204、`GET /permission` 轮询、三种 reply、`session/status` 判定、取最终文本）都在本轮单独实测过；但**整脚本的一次端到端执行没有完成** —— 发起的那次 shell 调用在等待批准时超时被打断，未重试。首次使用请先用只读任务（如上面的 `git status`）试跑。

已知限制：

- 只处理**单 session**：`requestID → task_key` 的映射留给 FlowHub（reply 端点不带 sessionID）。
- 只在 `bash`/`edit` 上加了会话级审计规则；`external_directory`、`doom_loop`、`read *.env` 仍按默认询问策略走（会被自动应答逻辑接住）。
- 超时(`deadline`)到点即返回 `None`，不 abort 会话（与设计文档 §7「超时 ≠ 失败」一致）。

---

## 7. 对既有设计文档的修正与补充

| 现有文档位置 | 修正 / 补充 |
|---|---|
| §6 `POST /session` body 只有 `{parentID, title}` | 实际还接受 `agent` / `model` / `metadata` / **`permission`** / `workspaceID`；其中 `permission` 是**会话级权限规则，实测立即生效且优先于 config** ✅ |
| §6 `tools?: {[key]: boolean}` 标注「已废弃」 | **仍然生效**：`{"tools":{"bash":false}}` 会让模型收到 `unavailable tool 'bash'` ✅ |
| §6 权限应答只列了 `/session/:id/permissions/:permissionID` | 另有全局 `GET /permission`（列 pending）、`POST /permission/{requestID}/reply`（带 `message` 反馈）、`GET /question` + `reply/reject`。会话内那个端点**字段名是 `response`**，全局那个是 `reply` ✅ |
| §6「所有 session 相关请求支持 `?directory=`」 | 补充反例：**不带该参数时不会作用于调用方期望的目录**，而是 server 自己的默认 location（本机 `/Users/yusiwen/Documents`）→ `GET /permission`、`GET /session/status` 都返回空，极易误判 ✅ |
| §5 `permission` 表 =「已记住的权限决策」 | 运行期 `always` 决策只存在**内存**（按目录实例），跨 session 生效、重启失效；源码里该表只有迁移写入路径 ⚠️/✅ |
| §12 方案 2「监听 `permission.asked`」 | 可行（SSE 实测收到），但**轮询 `GET /permission?directory=` 更简单**，2 秒内即可看到 pending ✅ |
| §12 方案 3 `--auto` | ❓ 未验证；`opencode serve --help` 未列该开关 |
| §12 建议「默认 deny 危险操作 + 白名单」 | 与本次实测一致；补充：**`reject` 不会挂死 run**（模型收到工具错误继续），可以放心用作「转人工审批」的默认动作 ✅ |
| §7 issue #46842（busy 时 `prompt_async` 吞消息） | 本次未专门复现，但实测确认：**权限 pending 期间 session 一直是 `busy`**，工具 part 停在 `running` → 编排层若只按「busy 结束」判轮次，会把「等审批」误当成「在跑」，务必把 pending permission 计入状态机 ✅ |
| §8「不要按 title 搜索定位」 | 一致；补充：v1 建的会话跑完一轮后**会自动生成有意义标题**，v2 建的不会（一直是 “New session - …”）✅ |
| §6 未覆盖 `/api/*`（v2） | 补充：v2 `POST /api/session` 能建会话（body 用 `location.directory`，响应包一层 `{"data":…}`），但 `POST /api/session/{id}/prompt` **只把消息写进持久事件日志**（`session.next.prompt.admitted/prompted`），`/api/session/active` 始终为空、历史里永远没有 assistant —— **没有 runner 就不执行**（Web UI 就是 runner）→ **自动化请走 v1** ✅ |
| §3 运行进程 | 本轮 `lsof` 复核：`127.0.0.1:4096` 仍由 PID 55437 监听，与 §3 一致 |

---

## 8. 未验证 / 待确认

1. `opencode serve --auto` 是否对所有会话生效（未另起实例测试）。
2. `system` / `noReply` / `format`(json_schema) / `messageID`(幂等) 这几个投递字段未实测。
3. `doom_loop` 只读了源码阈值（3），未构造复现。
4. 多 session 并发 pending 时的应答路由（reply 端点不带 sessionID）未验证。
5. `x-opencode-directory` header 用在 `prompt_async` 等 POST 上的等效性（GET/POST reply 已验证）未测。
6. 开启 `OPENCODE_SERVER_PASSWORD` 后的鉴权行为（Basic Auth）未测；生产部署必须验证。
7. v2 permission/question 应答链路未验证（v2 会话不会自动执行，暂无意义）。
8. `always` 的已批准列表是否严格按**目录**而非**项目**隔离：本轮临时目录都属于内置 `global` 项目，未能区分（源码显示缓存按目录、DB 行按 project）⚠️。

---

## 9. 附录 A：本次实测的会话与临时产物

| session | 目录 | 用途 |
|---|---|---|
| `ses_f48bc7a33ffe5K9sinuttgCQ3e` | `/private/tmp/oc-api-demo` | v1 建会话 + 同步投递；后来用于验证“不传 directory 也记得目录” |
| `ses_f48bc5293ffeeWPVMjLu0A3H4R` | `/tmp/oc-api-demo` | v2 建会话，prompt 从未执行（论证 v2 无 runner） |
| `ses_f483b3ff0ffewU7YN75eua8VvL` | `/private/tmp/oc-perm-test` | 会话级 `bash:ask` 规则；once/always/reject 三种应答 |
| `ses_f4838d1fdffeaXVEhDc7GIFDtY` | `/private/tmp/oc-perm-test` | 配置改动未生效（实例缓存）的反例 |
| `ses_f48387647ffeTriOBFPqJrzSsN` | `/private/tmp/oc-perm-cfg` | 新目录 + `bash:deny` → 工具被移除 |
| `ses_f48378bcaffem05Gea0DTbx3Rw` | `/private/tmp/oc-perm-test` | 验证 `always` 跨会话生效 |

```bash
# 清理（均为 /private/tmp 下的临时目录与文件）
rm -rf /private/tmp/oc-api-demo /private/tmp/oc-perm-test /private/tmp/oc-perm-cfg \
       /private/tmp/oc-perm-allow /private/tmp/oc-perm-patch /private/tmp/oc-perm-patch2 \
       /private/tmp/outside-note.txt
# 删除测试会话（可选）
for s in ses_f483b3ff0ffewU7YN75eua8VvL ses_f4838d1fdffeaXVEhDc7GIFDtY \
         ses_f48387647ffeTriOBFPqJrzSsN ses_f48378bcaffem05Gea0DTbx3Rw \
         ses_f48bc5293ffeeWPVMjLu0A3H4R ses_f48bc7a33ffe5K9sinuttgCQ3e; do
  curl -s -X DELETE "http://localhost:4096/session/$s" >/dev/null; done
# Web UI 侧栏若残留临时项目：清掉 localhost:4096 的 localStorage key `opencode.global.dat:server`
```

## 10. 附录 B：最小 curl 合集

```bash
BASE=http://localhost:4096
DIR=$(python3 -c 'import os,sys;print(os.path.realpath(sys.argv[1]))' /private/tmp/oc-api-demo)

# 建会话（会话级权限规则）
SID=$(curl -s -X POST "$BASE/session?directory=$DIR" -H 'Content-Type: application/json' \
  -d '{"title":"demo","permission":[{"permission":"bash","pattern":"*","action":"ask"}]}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# 异步投递任务
curl -s -X POST "$BASE/session/$SID/prompt_async?directory=$DIR" \
  -H 'Content-Type: application/json' \
  -d '{"agent":"build","model":{"providerID":"deepseek","modelID":"deepseek-v4-flash"},
       "parts":[{"type":"text","text":"用 bash 执行 pwd，只回复输出"}]}' -w '%{http_code}\n'

# 看谁在等审批 → 应答
curl -s "$BASE/permission?directory=$DIR" | python3 -m json.tool
curl -s -X POST "$BASE/permission/<per_xxx>/reply?directory=$DIR" \
  -H 'Content-Type: application/json' -d '{"reply":"once"}'

# 判完成
curl -s "$BASE/session/status?directory=$DIR"
curl -s "$BASE/session/$SID/message" | python3 -m json.tool | tail -40
```

## 11. 参考资料

- opencode Server API：https://opencode.ai/docs/server/
- opencode 权限配置：https://opencode.ai/docs/permissions/
- opencode Agent 配置：https://opencode.ai/docs/agents/
- 本地 OpenAPI spec：`GET http://localhost:4096/doc`（1.18.31 实测 162 paths）
- 服务端日志：`~/.local/share/opencode/log/opencode.log`
- 姊妹文档：`opencode-devops-orchestration-design.md`（设计文档，本文为其补充与校正）
- YouTrack 侧真实报文的实测结论：`youtrack-webhook-and-flowhub-security.md` **§5.6**（2026-09-20，13 条真实投递）
  —— 本文不涉及 payload 细节，phase 2 实现触发规则时以那份实测为准
