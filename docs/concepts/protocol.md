# 协议契约

> **读者**：要改协议、或写一个跨服务参与链的服务的人 ｜ **读完能做**：判断一个改动是否合规、按契约收发帧 ｜ **状态**：有效（SPv2.1，`v: "spv2.1"`）｜ **最后校验**：2026-11-06

这份文件只写**不可违反的约定**。线格式的完整定义（字段、校验、错误码、卡片）以 [spv2.md](spv2.md) 为准；本文不复述 spec，只写各服务必须遵守的契约。为什么这样设计见 [architecture.md](architecture.md)；动手写服务见 [../guides/add-a-service.md](../guides/add-a-service.md)。

## 1. 独立性硬约束

1. **独立进程**：可单独启动 / 停止 / 重启；除「设置中心地址」这一条引导配置外，不依赖任何其他服务正在运行。
2. **独立存储**：只读写自己的库与目录；**不得**直接访问其他服务的库表或文件。派生数据（缓存 / 向量 / 索引）必须可丢弃、可重建——**权威数据只有一份**，且在它的归属服务里。
3. **只经网络访问**：按地址点对点直连，不经过任何中转。
4. **不驻留链状态**：跨步状态原则上随消息携带。唯一例外是 reasoner 的**任务窗口黑板**——进行中任务的最新消息链与在跑工具表留在进程内（窗口 = 任务在跑期，交付 / 被取代即释放，不落盘、不跨重启）。
5. **语言自由**：任何语言都可实现。`src/protocol/`（Go 包）只是**参考实现之一**，不是规范本身。

> 判断一个改动是否合规的最快问法：**它需要别的服务配合修改吗？** 需要，就是设计走偏了。

## 2. 引导与生命周期

引导只靠三条配置：**自己的名字、自己的监听地址、设置中心地址**（其余是服务自己的业务配置）。

```text
启动 → 监听 /spv2 → 向设置中心 register（call；首次注册同步完成，中心不可达则启动失败）
     → 每 15s 发 heartbeat（event；间隔随名片声明 heartbeat_ms，中心据此判定离线）
     → 订阅 roster event（名册更新即替换本地通讯录）
     → 受理 call / event（按 method 分派 → call 回 reply、event 无回复）
     → 退出前发一条 status="bye" 的 heartbeat（中心立即摘除名册）
运行中断线：指数退避（1s → … → 30s 上限）重连并重新注册（名片无变化不广播——重连去重）
离线判定（中心侧）：2 × 声明心跳未见 → 摘除名册并广播；心跳晚到可从墓碑复归
```

- 服务名：小写字母开头的短名（`memory` / `images` / `qq` / `reasoner`）。
- 地址：`ws://host:port/spv2`。**所有服务只暴露 `/spv2`**；不存在 v1 `/ws` 业务端点。
- 重复注册必须幂等（同名重注册 = 更新名片）。

## 3. 传输与帧

- **传输**：WebSocket；**编码**：MessagePack（二进制帧），`content_type` 也允许 `application/json` / `text/plain` / `application/octet-stream`。
- 落地库（不是规范，只是各语言的现成实现）：Go `vmihailenco/msgpack`、Python `msgspec`、TS `msgpackr`。
- **线协议只有一个单元：`Frame`**。没有信封套壳，没有 v1 的 `jump` / `response` 消息类型，也没有 `boss` 字段。
- **访问模型**：默认短连接、用完即走；只有「工具调用那一跳」等 reply（其余发出即完）。与设置中心的控制连接常驻，用来收 `roster` / `settings` 事件。
- **信任边界**：内部 WS **不做身份认证**——`from` 是发送方自称的字段，服务只在敏感动作上比对它（如 `approval` 只认门户裁决）。因此部署前提是「可信本机 / 内网」，**不要直接暴露公网**（见 [approval.md](approval.md) 第 9 节）。这条是部署假设，不是代码保证；要上公网得先补认证。

**帧**（`src/spv2/frame.go`）：

```jsonc
{
  "v": "spv2.1",          // 协议版本，必填且只能等于 spv2.1
  "id": "01J...",         // 帧 id；reply 用 reply_to 指回它
  "type": "call",         // call / reply / event / stream
  "work": "01J...",       // 必填；同一条用户请求全链共享（见 §7），也是 work 身份
  "from": { "kind": "service", "id": "qq", "authority": "none" },
  "to":   { "kind": "service", "id": "reasoner" },
  "method": "message",
  "content_type": "application/msgpack",
  "body": { }             // method 的数据；MsgPack 编码的字节
}
```

- 必填：`v` / `id` / `type` / `work` / `from` / `to`；`call` 与 `event` 还必须有 `method`。
- 可选：`reply_to` / `content_type` / `schema` / `idempotency_key` / `stream` / `status` / `error`。
- `reply`：`reply_to` 必填、`status` 只能是 `ok` / `error`，`error` 与 `status` 严格对应；`reply` 不得带 `method` / `stream`。
- `event`：单向通知，不产生 reply；`event` 不得带 `reply_to` / `status` / `error` / `stream`。
- `stream`：`reply_to` 与 `stream{id, seq, end}` 必填；终态块带 `status`（`end=true`），非终态不得带 `status` / `error`。
- `from` 的 `authority` 帧级规则：human 只能 `owner` / `none`；agent 允许 `none` / `runtime`（`delegated` 未设计）；service / system 只能 `none`。
- **未知字段必须忽略**；`to.kind=capability` 在 v2.1 未设计，回 `not_supported`。

## 4. 帧类型与控制面

| type | 方向 | 语义 |
| --- | --- | --- |
| `call` | 任何服务 → 任何服务 | **唯一的「访问」动作**；目标可以是自己（自跳 = 循环的下一步）；等一条 `reply` |
| `reply` | 被调方 → 调用方 | `status=ok` 带 body，`status=error` 带 `error{code,message,retryable?,details?}` |
| `event` | 任何服务 → 任何服务 | 单向通知（如 `heartbeat`）；不产生 reply |
| `stream` | 被调方 → 调用方 | 流式响应块；`stream.end=true` 表示终态 |

设置中心是普通服务，只是 method 特殊：

| method | 形态 | 方向 | 语义 |
| --- | --- | --- | --- |
| `register` | call | 服务 → 设置中心 | 注册（body = 名片 `RegisterPayload`）；**reply 里带全量名册 `RosterPayload`** |
| `heartbeat` | event | 服务 → 设置中心 | 报活（默认 15s）；`status="bye"` = 退出 |
| `roster` | event | 设置中心 → 各服务 | 全局清单推送（全量 + revision） |
| `settings` | event | 设置中心 → 各服务 | 设置变更与下发结果 |
| `roster` / `ping` | call | 任何服务 → 设置中心 | 主动拉名册 / 连通性（推送丢失或重启后的自愈手段） |
| `report` | call | 服务 → 设置中心 | 错误上报 |
| `list_settings` / `set_setting` / `reset_setting` / `report_setting` | call | 任何服务 → 设置中心 | 设置查询与变更（见 [add-a-service.md](../guides/add-a-service.md)） |
| `configure` | call | 设置中心 → 服务 | 设置下发（由 `src/service/` 骨架统一承接） |
| `hello` / `health` / `metrics` | call | 任何服务 → 任何服务 | 取服务卡 / 健康 / 指标（`spv2.Server` 内置） |

名册只放 `name` / `addr` / `title` / `description` / `provides` / `heartbeat_ms`（`protocol.ServiceCard`），完整 method schema 由服务卡 `hello` 提供。

## 5. Method 词汇与工具暴露

### 5.1 保留 method（链机制，语义不可改）

`message`（唯一入口：human / 入口 service / agent）/ `context`（reasoner 同步取上下文与任务）/
`step`（自跳继续）/ `deliver`（终点直达结果归属方）/
`store`（记忆写）/ `save` `fetch`（图片 / 文件链）/ `report_result`（异步受理的补报）。

这些是**协议级词汇**：即便某个服务提供了同名 method，也**不作为 LLM 工具**暴露。
另有**骨架保留 method** `configure`（设置下发专用，由 `src/service/` 骨架统一承接，同样不进 `provides`、不作 LLM 工具）。

### 5.2 服务自定义 method

在服务内唯一即可（`to` 已指明服务，不需要全局前缀）。命名：小写 + 下划线。

### 5.3 工具名映射与暴露规则

LLM 工具名 = **`<service>__<method>`**（由 reasoner 依据名册 `provides` 生成）。

**暴露 = 显式声明**：method 要在 `provides` 里声明 **`tool: true`** 才暴露给 LLM；未声明的 method 只可被「直接调用」访问，对模型不可见。另有两道硬闸（即便声明也不暴露）：

1. 链机制保留 method（§5.1）；
2. 调试 / 测试 method（如 reasoner 的 `echo` / `debug_jump`）。

两闸同时作用于**执行面**：模型若仍指名调用未提供（或保留 method）的工具，reasoner 在执行前直接拒绝，错误作为工具结果回给模型——不会发生真实调用。

> 为什么必须显式声明：第三方服务的动作无法被 reasoner 的硬编码表覆盖，必须由服务自己表态。

**工具并发**：同一会话内的工具调用并发执行；reasoner 侧默认单会话上限 10（可配置，`-1` = 不限制），超限排队。同一服务的多个调用可并发重复进行，不做去重。

**给新服务的建议**：管理 / 调试类动作（改配置、清库、发号施令型代理）**默认不要暴露**给 LLM；能力类动作（查、算、取、发）才是给模型的。

**结果多大由提供方决定**：如果能力类动作回的是大块数据（图片、文件、长列表），提供方要自己给结果设上限并按需分片（例：`images.fetch` 默认只回缩略图，要看细节用矩形参数取局部），不要把「会不会撑爆上下文」的判断留给 reasoner——reasoner 只有一张硬编码黑名单，第三方服务它认不出来。

## 6. 异步：受理即回执与补报

有些动作的耗时不可预期（跑一条命令、跑一次训练）——让调用方等结果既会撞上工具超时，也违反「边界」：等待不该跨服务传递。

**声明**：`provides` 里 `async: true` 表示该 method **受理即回执**——reply 只表示「已受理」，并给一个句柄：

```jsonc
// 调用方 → 服务（call）
{ "method": "run", "body": { "command": "ls -la" } }
// 服务的 reply（status=ok；body 只表示已受理）
{ "item_id": "01J…", "phase": "queued" }
```

**补报**：后续每条进展都是服务 → 调用方的一次独立 `call`（`method: "report_result"`，`to` = 原先的调用方，`from` = 服务自己）：

```jsonc
{ "item_id": "01J…", "phase": "approved", "text": "审批通过，已开始执行\n$ ls -la", "final": false }
{ "item_id": "01J…", "phase": "done", "text": "命令执行完成（退出码 0，耗时 12ms）…", "final": true,
  "data": { "exit_code": 0, "stdout": "…", "stderr": "", "duration_ms": 12 } }
```

- `item_id`：受理回执里的句柄（调用方据此把补报配回自己的记账）；补报的 `phase` 与 `data` 内容由服务自定；受理回执里的 `phase` 与补报同一词汇（受理就是第一个阶段；shell 当前：`queued`）。**字段名不能改**：reasoner 的 `beginAsync`（`reasoner/server/async.go`）只认这个键。
- `text` 是可直接阅读的正文（调用方据此注入上下文 / 呈现）；`final: true` = 该次受理的最后一条。
- **零阻塞**：受理回执不等审批、不等执行；补报各自独立一跳；调用方离线 = 补报发不出去（服务只记日志，不排队补发）。
- 调用方缺失句柄对应关系时的补报（重复 / 服务重启过）**忽略即可**，不是错误。
- **reasoner 侧行为**见 [agent-loop.md](agent-loop.md)。
- **与「工具超时」的关系**：异步动作的受理是瞬时动作，单次工具超时（默认 60s）不再约束实际执行时长（由服务自己的执行超时管）。

## 7. 链机制要点（写「参与链」的服务必读）

- **结果归属（`from`）**：SPv2.1 没有 `boss` 字段，**结果归属方就是帧头的 `from`**。每跳的 `from` 由发送方自定、**不可丢失**；任务发起者必须收到最终结果。入口服务必须把 `from` 设成**自己**（qq 与门户都如此）：少了它或认错，回答照样生成，但**回不来**——reasoner 只按 `from` 交付，认错就是静默丢弃。交付失败只记日志并按「送达后不重试」停住（v2 没有独立于结果归属方的第二入口）。
- **`work`（链路 id）**：同一条用户请求全链共享，用于调试 / 审计；现有实现把 `work` 当 v1 的 `trace` 用，逐跳原样传递。
- **deliver**：链终点判定结束后**直达结果归属方**（不沿链逆向回传）。发起任务的接入服务（如 qq）必须实现 `deliver`。
- **同步取上下文**：入口 `message` 到达后，reasoner 同步调 memory `context`；**拿到组装好的 messages/task 才开黑板、调 LLM API**，失败直接回错误。任务内的各轮由黑板延续——不再每轮重复取上下文。
- **工具结果入账**：结果到达 → 注入黑板 → 按通知模式唤醒下一轮（`each` 逐条 / `batch` 攒批）。**工具参数的数字一律按整数归一**（LLM 的 JSON 数字是 float64，直接进 msgpack 会让服务侧的整数字段解码失败）。
- **发出即完**：一步 = 收到 → 处理 → 发出下一跳 → 结束；没有长挂起的链（`wait` 是唯一的阻塞点）。
- **用户附注（`notes`）**：入口服务把「用户在界面上直接做的操作」以 `notes` 块随下一跳投递——与用户说的话在数据层分开，reasoner 渲染成前缀块。交给 `batch` 的 `user_hold` 的 `Notes` **不再重复**放进消息的 `notes`；被停手时未开始的项已作废、**不会自动重跑**，附注必须写明这一点，否则模型会以为任务还完整。规则见 [batch/README.md](../../src/batch/README.md)。
- **来源标注（`message.sender`）**：入口服务把「谁说的、在哪说的、多少人看得见」作为**结构化事实**随消息带上（`channel` / 发送者 id 与名字 / 群 id 与群名 / `group_size` = 扩散面 / `owner` / `mentioned`）。渲染成正文标签的实现在协议包里只有一份（`protocol.Sender.Label()`），所以**记忆里存的、模型看到的**是同一句话：

  ```text
  [私聊 · 张三(10001) · 主人] 你好
  [群聊 · 群「编程交流」(67890) · 12 人可见 · 张三(12345) · 非主人 · 未@我] 你好
  ```

  标签是**事实不是权限**：授权与否由模型按主人的设置判断（`owner` 只说明这次发言是不是授权人本人）。取不到的项宁可写「人数未知」也不编数字——猜错的方向必须保守。单用户入口（门户）不带 `sender`，行为与旧版一致。

  同一条事实也以 `meta.sender`（JSON，键名与线路一致）随记录落库：入口服务直接落库（qq 的「只记录」路径）与推理侧代写记忆都调用 `protocol.MessageMeta`，所以**两条路径写出来的记录长得一样**；附件 id 只在有时才写，空数组不占键。
- **来源标签与任务名**：正文带标签后，从第一行推任务名会推出一串标签，而任务名要显示在界面会话树里。入口服务因此在 `store`（`task_name`）与 `context`（`context.task_name`）里显式给出人话标题；不给则沿用「取正文第一行」的旧规则。`store` 是异步落库、`context` 是同步取上下文，两条路径都要给。
- **失败处理（现状）**：拨号失败（连接未建立、消息可证明未送达）自动安全重试（+0.5s、+2s）；送达后的失败不重试——可能已被处理，重试需要幂等键（待定）。工具调用失败作为结果消息注入（错误文本交回模型决断）；单次工具超时（默认 60s）后按失败入账；任务被同会话新消息取代时在跑结果丢弃（日志可见）；交付结果归属方失败按「送达后不重试」原则停住（日志可见；失败上报归属方待定）。

## 8. 兼容与版本

- 帧的 `v` 是协议版本（当前 `"spv2.1"`；`Frame.Validate` 只接受它）。
- **method 与字段只增不改**：已有 method 的语义与 body 字段只做加法；删改 = 破坏性变更。
- **未知字段必须忽略**（解码时不得因多字段报错）；**未知 method 的 `call` 必须回错误 reply**（`status=error`、`code=not_found`），不得静默丢弃；未知 method 的 `event` 无法回复，只记指标 `sump_unknown_event_total`。
- 不使用 `reply_to` 之外的关联字段：请求 ↔ 回复只靠 `reply_to` 配对。
- 【待定】`src/protocol/` 是否对外发布版本（当前为仓库内本地模块，`v0.0.0` + `replace`）。

## 9. 命名基线（防漂移）

**名字只增不改；改名 = 破坏性变更，须在本表登记替代名与迁移说明。** 新增任何名字前先对照本表：

| 类别 | 规则 | 现有值 |
| --- | --- | --- |
| 帧类型（`type`） | 裸名词，四选一 | `call` `reply` `event` `stream` |
| 控制面 method | 小写下划线 | `register` `heartbeat` `roster` `settings` `ping` `report` `list_settings` `set_setting` `reset_setting` `report_setting` `configure` `hello` `health` `metrics` |
| 业务 method（`call.method`） | 小写下划线、动词_名词；保留 method 见 §5.1 | `embed` `run` `report_result` `approval_request` `approval_pending` `approval_decide` `approval_list` `capabilities`；timer：`sleep` `cancel` `list`；memory：`remember` `forget` `note` `dig` `lineheads` `prune` `publish_session` `shared_sessions` `shared_forget` `conversation_open` `conversation_archive`；files：`save` `fetch` `read` `write` `edit_word` `edit_ppt` `edit_pdf` `outline` `read_range` `find` `patch` `verify` |
| 载荷字段 | snake_case；可选即省略（omitempty）；跨服务新字段先在本表落一行 | `key` `value` `default` `overridden` `cause` `service` `results` `ok` `error` `heartbeat_ms`；设置项三类标记：`secret` `readonly` `set`；服务自报：`actual` `reported`；用户消息：`images` `files` `notes`（附注不是用户说的话）；上下文消息：`image_ids` `file_ids`；batch：`on_error`（`continue` 默认 / `stop`）`kind`（`once` / `resident`）`wake`；文件链：`filename` `files` `file_ids` `truncated` `sheet` `cell` `edits` `updated` `operations` `op` `old` `new` `replaced` `pages` `degrees` `properties` `applied`（`patch` 也用它）；文档链：`doc_kind` `digest` `depth` `node` `nodes` `node_count` `chars` `tables` `parts` `warnings` `offset` `limit` `total` `next` `regex` `matches` `run` `start` `end` `context` `range` `anchor` `exact_anchor` `exact_digest` `diff` `before` `after` `contains` `not_contains` `failures`；节点字段：`type` `level` `children`；embedding：`texts` `query` `vectors` `dim` `model`；异步受理与补报：`item_id` `phase` `text` `data` `final`；shell：`command` `work_dir` `exit_code` `stdout` `stderr` `stdout_bytes` `stderr_bytes` `timed_out` `truncated` `duration_ms`；timer：`ms` `note` `fire_at` `seq` `timers` `caller` `status`；共享经验库（跨会话只放提炼物）：`session_id` `task_id` `title` `goal` `outcome` `capabilities` `private` `sessions` `archived` `exclude` |
| 审批载荷字段 | snake_case；结构由 `src/protocol/approval.go` 定义 | `schema_version` `plan` `decision` `decision_id` `plan_digest` `policy_version` `reason_code` `valid_for_ms` `llm_feedback` `summary` `unknowns` `rule_ids`；补报新增 `approval`；第二层：`requested_by` `created_ms` `expires_ms` `note` |
| exec 域执行描述字段 | 当前 `plan` 是命令域专用结构；每个字段只保留一个语义 | `version` `command` `work_dir` `shell_path` `args` `dialect` `platform` `profile` `timeout_ms` `max_output_bytes`；`dialect` 只表示 shell 方言，`profile` 只表示执行环境约定 |
| 域判别字段（将来） | 新增第二个执行域时定义独立字段；当前协议尚未发送，不复用 `dialect` | 建议 `kind`，取值如 `exec` / `fs` / `http`；其他域各自声明执行字段组，裁决包、摘要绑定、有效期、fail-closed 共用 |
| 审批枚举 | 取值由 approval 服务定义，见 [approval.md](approval.md) | `decision`：`allow` / `deny` / `review`；`reason_code`：`RULE_ALLOW` `POLICY_DENY` `DYNAMIC_BEHAVIOR` `NO_MATCHING_RULE` `PARSE_ERROR` `UNSUPPORTED_DIALECT` `CONTEXT_UNKNOWN` `RUNTIME_MISMATCH` `LIMIT_EXCEEDED`，第二层用户裁决：`USER_APPROVED` `USER_DENIED` |
| 服务名 | 小写短名 | `settings-center` `reasoner` `memory` `images` `files` `qq` `shell` `timer` `batch` `mcp` `frontend` `embedding` `approval` |
| 设置键 | `<域>.<名>` 小写 + 下划线；由服务自己声明 | `tool.notify_mode` `tool.concurrency` `tool.timeout_ms` `concurrency` `max_concurrency` `max_inflight` `max_items` `item_timeout_ms` `preview_bytes` `job_ttl_ms` `resume_on_start`（batch 现状是裸键；按本行规则改名属于破坏性变更，见 TODO.md 的命名统一） `context.history_limit` `context.core_limit` `context.core_max_chars` `context.rel_limit` `context.rel_max_chars` `context.embedding` `share.on` `share.sessions_limit` `share.digest_chars` `shared_db` `max_mb` `read_max_chars` `owner` `group.mode` `sweep_ms` `heartbeat_fallback_ms` `exec.shell` `exec.root` `exec.timeout_ms` `exec.max_output_kb` `exec.max_running` `approval.service` `approval.timeout_ms` `policy.json` `max_ms` `max_pending` `portal` `llm.base` `llm.model` `llm.key` `memory` `images` `files` `auto_approve` `reflect.on` `name` `addr` `center` `settings` `napcat.url` `napcat.token` `reasoner` `http.token` `db` `dir` `web` |
| 名片字段（`provides[]`） | 小写下划线；跨语言一致 | `action` `input` `output` `tool` `async` `batchable` |

审批设置键补充：`policy.json`（approval 声明的规则快照配置）。`approval_result` 没有注册为动作；审批结果作为 `approval_request` 的响应返回，并由 shell 的 `report_result` 承载后续反馈。

MCP 设置键补充：`http.token` 与 `remote.<id>.token`（秘密）、`http.addr` / `config.path`（只读）。MCP 的方法名、连接与工具目录属于 `mcp` 的外部协议，不新增 SUMP 帧字段。外部请求进入 SUMP 时，由 `mcp` 生成独立 work（当链路 id 用）并作为结果归属方（`from=mcp`，结果归属即 `from`）；反向调用沿用原始 SUMP 请求的 `reply_to` 关联。受理回执不得当作最终执行结果。

MCP 导入动作的 `provides[].batchable` 为 `false`；导入授权不包含 batch 代理执行授权。batch 对此返回 `BATCH_NOT_BATCHABLE`，直接调用及其并发不受影响。

MCP 同步导入结果：`phase` 为 `completed` / `failed`，`is_error` 表示远端工具是否明确失败，`content` 保留 MCP 内容块，`structured_content` 保留已校验的结构化结果；工具明确失败仍是已成功翻译的响应，`reply.status=ok`。传输或契约失败使用 `reply.status=error`，不可据此认定远端没有执行。详见 [mcp/README.md](../../src/mcp/README.md)。

MCP 异步导入由绑定配置 `async: true` 固定声明 `Provide.Async`。先返回 `{item_id, phase:"accepted"}`，再沿原来源、work 发送现有 `report_result`；`text` 是有界可读投影，`data` 保留翻译结果，`final: true` 表示本地观察结束。派发后失去结果时 `phase=outcome_unknown`，不能解释为操作未执行或已停止。`cancel_import` 输入仅 `item_id`，校验原调用服务身份；返回 `status=observation_cancelled/unknown` 和 `remote_outcome=unknown`，只取消本地等待。失败数据可带 `execution_outcome=not_submitted/unknown`，不新增帧字段或全局调度状态。

异步调用方在派发前登记待回执，补报必须以来源服务、trace、item_id 共同关联。早到缓冲有界，确定终态仅交付一次。MCP 对外的 `sump://jobs/{job_id}` 观察资源归 HTTP 认证主体所有，不依赖传输会话；它是模块自己的临时记录，不是原生任务表或 MCP Tasks。

文档链字段的取值约定（`files` 的 `outline` / `read_range` / `find` / `patch` / `verify`）：`node` 是结构树上的节点地址（如 `body/p12`、`body/tbl1/r2/c1/p1`），`run` 是命中起点所在的 run 地址；`start` / `end` 都是 rune 偏移、左闭右开；`digest` 是整篇可见文本的规范化指纹（折叠空白），可当乐观锁用——`patch` 的 `anchor` 与 `verify` 的 `digest` 都拿它比对，对不上就拒绝而不是猜；`exact_digest` / `exact_anchor` 是不折叠空白的原始文本指纹，带 `range` 的编辑用它，空白一变（旧 rune 坐标可能已失效）就拒绝；`next` 非 0 表示还有下一块，续读时原样当 `offset` 传回（整数地址，服务端不存游标）；`doc_kind` 是文档类型（当前只有 `docx`，其余格式明确报错而不是猜）。动作语义与限制见 [files/README.md](../../src/files/README.md)。

