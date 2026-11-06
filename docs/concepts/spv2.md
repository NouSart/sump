# SPv2.1 — SUMP Protocol v2.1

> **状态**：草案 v0.1。核心方向冻结；v3 之前只做加法。  
> **范围**：线协议、work 模型、握手/服务卡、核心 method、错误、流式、可观测性、安全、迁移。  
> **不设计**：多租户、spawn/自复制、跨 owner、企业高可用、认证。它们在 §18 标为 `not designed`，只留字段，不假装能用。  
> **一句话**：一个帧，一个 work，一切皆工作。

---

## 目录

1. [设计原则](#1-设计原则)
2. [术语](#2-术语)
3. [线格式](#3-线格式)
4. [Ref 与寻址](#4-ref-与寻址)
5. [握手、服务卡、控制面](#5-握手服务卡控制面)
6. [Work 模型](#6-work-模型)
7. [核心 Methods](#7-核心-methods)
8. [错误](#8-错误)
9. [幂等、重试、deadline](#9-幂等重试deadline)
10. [流式与背压](#10-流式与背压)
11. [A2A：work 子树](#11-a2awork-子树)
12. [可观测性](#12-可观测性)
13. [安全](#13-安全)
14. [Schema 与演进](#14-schema-与演进)
15. [迁移 v1 → SPv2.1](#15-迁移-v1--spv21)
16. [示例](#16-示例)
17. [验收与测试](#17-验收与测试)
18. [明确不做 / 未设计](#18-明确不做--未设计)
19. [实现顺序与 v3 触发](#19-实现顺序与-v3-触发)

附录 A：六条红线  
附录 B：许愿式编程审计

---

## 1. 设计原则

1. **Work 是唯一名词**：用户消息、A2A thread、spawn 出来的 agent 都是 work；工具调用是 work 内的一次 call，不新建 work。
2. **一个帧，body 不透明**：协议只认 header；业务数据由 method + schema 决定。
3. **method 是数据，服务卡是契约**：协议不定义业务 payload 类型。
4. **lease 先于 work**：没有预算/截止时间的工作不能无限跑。
5. **authority 不来自文本**：agent 消息永远 `authority=none`；权限只来自 work 角色或 capability。
6. **memory 是唯一 work 登记处**：reasoner 只缓存；工具不建 work 表。
7. **观测是协议的一部分**：每帧带 `work` / `method` / `status`；`health` / `metrics` 是 well-known method。
8. **只做加法**：字段可加，语义不改；未知字段忽略，未知 method 报错。
9. **不许愿**：每个“支持 X”必须写清 owner、失败路径、观测和验收。

---

## 2. 术语

| 术语 | 定义 |
|---|---|
| **Frame** | 唯一线上单元：header + body |
| **Work** | 一次有 owner、有 lease、有生命周期的工作 |
| **Call** | work 内的一次 method 调用；工具调用也是 call |
| **Ref** | 通用引用：`{kind, id, authority?, tenant?}` |
| **Method** | 服务声明的可调用能力 |
| **Service Card** | 服务自描述：method、schema、limits、observability |
| **Lease** | work 的 deadline + 预算 |
| **Authority** | `owner` / `runtime` / `none` / `delegated` |
| **Owner** | work 的最终归属者（人类 actor） |
| **Agent** | 执行 work 的 agent actor |
| **Entry** | 人类入口服务（frontend / qq） |
| **Participant** | owner 邀请的 agent/human，只能 message/event |
| **Thread** | `work.class=thread` 的 work 子树 |
| **Spawn** | `work.class=spawn`；v2.1 not designed |
| **Profile** | 预留扩展：enterprise / spawn / cross-owner / evolution |

**命名基线**：一个词只允许一个含义。

- `frame.type`：`call | reply | event | stream`
- `ref.kind`：来源 `human | agent | service | system`；目标 `service | agent | work | capability`
- `method.shape`：`call | event`
- `work.class`：`request | thread | spawn`
- `work.status`：见 §6.3
- `authority`：见 §13

不得把 `kind` / `type` / `shape` / `class` 混用。

---

## 3. 线格式

### 3.1 最小帧（8 个必填字段）

```jsonc
{
  "v": "spv2.1",
  "id": "01J...",
  "type": "call",
  "work": "01J...",
  "from": { "kind": "human", "id": "portal", "authority": "owner" },
  "to":   { "kind": "service", "id": "reasoner" },
  "method": "message",
  "body": { "text": "改一下门户配色" }
}
```

### 3.2 可选字段

```jsonc
{
  "reply_to": "01J...",                    // reply / stream
  "content_type": "application/msgpack",   // 缺省 msgpack
  "schema": "reasoner.message@1",          // 可选
  "idempotency_key": "01J...",             // 重试时
  "stream": { "id": "s1", "seq": 1, "end": false }, // stream
  "status": "ok",                          // reply / stream end
  "error": { ... }                         // reply / stream end
}
```

**帧里不得出现**：

- `parent`（在 work 对象里）
- `lease`（在 work 对象 / `renew` body 里）
- `tenant`（在 profile 里，v2.1 reserved）
- `meta`（不设万能扩展袋；需要字段就加字段）
- `ok`（用 `status`）
- `mode`（用 `type`）

### 3.3 四种 `type`

| type | 语义 | 必须字段 |
|---|---|---|
| `call` | 请求，期待 reply | `method` / `work` / `from` / `to` / `body` |
| `reply` | 对某个 call 的响应 | `reply_to` / `status`；成功带 `body`，失败带 `error` |
| `event` | 单向事件/进度 | `method` / `work` / `from` / `to` / `body` |
| `stream` | 流式响应块 | `reply_to` / `stream`；最后一块 `end=true` + `status` |

- `event` 不是终态；终态只走 `reply` 或 `stream end`。
- `status` 与 `error` 互斥。
- 未知 `type`：接收方回 `invalid_argument`（如果能回）。

### 3.4 内容类型

- `application/msgpack`（默认）
- `application/json`
- `text/plain`
- `application/octet-stream`

接收方按 `content_type` 解码；不认识的类型回 `invalid_argument`。

### 3.5 限制

| 限制 | 默认 | 行为 |
|---|---|---|
| `max_header` | 16 KiB | 超限断开/拒绝 |
| `max_body` | 1 MiB | `resource_exhausted` |
| `max_stream` | 32 条并发流 | `resource_exhausted` |
| `max_inflight` | 每连接 64 | `resource_exhausted` |
| `max_stream_buffer` | 每流 1 MiB | 暂停读/拒绝；见 §10 |

大对象走 stream，或交给 files/images 的 capability；不要塞进单个 body。

---

## 4. Ref 与寻址

### 4.1 Ref

```jsonc
{
  "kind": "human | agent | service | system | work | capability",
  "id": "portal | agent-01J... | memory | work-01J... | cap-01J...",
  "authority": "owner | runtime | none | delegated", // 仅 from
  "tenant": "tenant-a" // reserved，v2.1 不得依赖
}
```

### 4.2 来源 `from.kind`

| kind | 含义 | 默认 authority |
|---|---|---|
| `human` | 人类（经入口） | `owner` |
| `agent` | agent 运行时 | `none` |
| `service` | 服务自身 | `none` |
| `system` | 系统内部 | `none` |

### 4.3 目标 `to.kind`

| kind | 解析 | v2.1 |
|---|---|---|
| `service` | settings-center 名册 / 本地卡片 | supported |
| `agent` | memory actor/work 表 | supported（同 owner） |
| `work` | memory work 行 → agent/owner | supported（同 owner） |
| `capability` | capability handle | **not designed**；返回 `not_supported` |

### 4.4 校验

- 接收方必须校验 `to.id == 自己`；不匹配回 `invalid_argument`，不得“帮忙转交”。
- `to.kind=service` 时，`to.id` 必须是本服务名。
- `to.kind=agent/work` 只接受 reasoner/memory 解析后的目标；解析失败 `not_found`。
- `from` 在 v2.1 没有认证，是自称；authority 校验是可信内网内的校验，不是安全边界（见 §13）。

---

## 5. 握手、服务卡、控制面

### 5.1 握手

连接后第一帧：

```jsonc
call method=hello
body: {
  "protocol": "spv2.1",
  "actor": { "kind": "service", "id": "memory" },
  "capabilities": ["msgpack", "json", "stream"],
  "auth": null
}
```

回复就是普通 `reply`，带服务卡：

```jsonc
reply reply_to=<hello.id> status=ok
body: { "card": { ... } }
```

**没有 `welcome` method。**

### 5.2 服务卡

```jsonc
{
  "protocol": "spv2.1",
  "name": "memory",
  "actor": { "kind": "service", "id": "memory" },
  "version": "0.1.0",
  "description": "记忆服务",
  "capabilities": ["msgpack", "json", "stream"],
  "methods": [
    {
      "name": "context",
      "shape": "call",
      "version": 1,
      "tool": false,
      "idempotent": true,
      "idempotency_ttl_ms": 3600000,
      "streaming": false,
      "long_running": false,
      "timeout_ms": 30000,
      "description": "同步组装上下文",
      "input": { "type": "object", "properties": { }, "required": [ ] },
      "output": { "type": "object", "properties": { }, "required": [ ] }
    }
  ],
  "limits": { "max_body": 1048576, "max_stream": 32 },
  "observability": { "health": true, "metrics": true, "format": "spv2.metrics.v1" }
}
```

规则：

- `tool:true` 才暴露给模型；一 method 一 tool；工具名 `service.method`。
- `shape`：`call` 或 `event`；流式是响应形态，用 `streaming:true`。
- `timeout_ms`：该 method 的上限；实际取 `min(work 剩余 deadline, timeout_ms)`。
- `idempotent`：决定能否重试；重试必须带 `idempotency_key`。
- `long_running:true`：返回 `accepted` + `work`，后续用 `event` 推进。
- 大 schema 用 `schema_ref`，卡里只放引用。
- `instances` 是 enterprise profile；v2.1 不返回、不依赖。

### 5.3 控制面

设置中心是普通服务，只是 method 特殊：

| method | 方向 | 作用 |
|---|---|---|
| `hello` | 任意 | peer 握手，返回卡片 |
| `register` | service → center | 提交卡片，返回 roster |
| `heartbeat` | service → center | 报活；`status=bye` 表示退出 |
| `roster` | center → service | 名册事件（全量 + revision） |
| `settings` | center → service | 设置下发/回传 |
| `report` | service → center | 错误/告警事件 |

- 名册只放 `name / actor / addr / version / methods 摘要 / capabilities`；完整 schema 按需拉。
- 设置中心不做业务转发，不存指标时间序列。
- `register` 失败必须回明确错误；不得静默降级。

---

## 6. Work 模型

### 6.1 Work 对象

```jsonc
{
  "id": "01J...",
  "parent": "01J...",          // 可选；child work
  "root": "01J...",
  "class": "request | thread | spawn",
  "owner": { "kind": "human", "id": "owner" },
  "agent": { "kind": "agent", "id": "reasoner" },
  "entry": { "kind": "service", "id": "frontend" },
  "goal": "改门户配色",
  "status": "open | running | waiting | stalled | done | failed | cancelled | orphaned",
  "lease": { ... },
  "participants": [
    { "actor": { "kind": "agent", "id": "worker-1" }, "role": "member", "status": "active" }
  ],
  "policy": {
    "cross_owner": false,
    "private": false,
    "publish": false
  },
  "created_at": "RFC3339",
  "updated_at": "RFC3339",
  "closed_at": "RFC3339"
}
```

### 6.2 Work 与 call

- **Work**：用户消息、A2A thread、spawn 出来的 agent。有 owner/lease/状态。
- **Call**：work 内的一次 method 调用（shell.run / files.read / memory.context）。**不新建 work**，只带同一个 `work`。
- **Child Work**：A2A thread、spawn。带 `parent`，有自己的 owner/agent/lease。
- 工具调用不会把 work 表撑爆；work 树只记录真正的“意图分支”。

### 6.3 状态机

```text
open ──▶ running ──▶ waiting ──▶ running ──▶ done
  │         │            │
  │         ├──▶ failed  ├──▶ stalled ──▶ running（renew/resume）
  │         └──▶ cancelled
  └──▶ cancelled
```

| 状态 | 含义 | 允许转移 |
|---|---|---|
| `open` | 已创建，未开始 | `running` / `cancelled` / `stalled` |
| `running` | 执行中 | `waiting` / `done` / `failed` / `cancelled` / `stalled` |
| `waiting` | 等 participant / 工具 / 人类 | `running` / `cancelled` / `stalled` / `orphaned` |
| `stalled` | 无进展 / wait 环 / 乒乓 / lease 耗尽 | `running`（renew/resume）/ `cancelled` |
| `done` | 终态，成功 | — |
| `failed` | 终态，失败 | — |
| `cancelled` | 终态，取消 | — |
| `orphaned` | owner 消失/被顶掉 | `cancelled` / `done` |

非法转移回 `conflict`。`done` 前必须检查：无 pending child、无未完成 call、无未清 lease、无未处理 event。

### 6.4 角色

| 角色 | 说明 | 权限 |
|---|---|---|
| `owner` | 人类 actor | `renew` / `cancel` / `deliver` |
| `agent` | 执行 work 的 agent | `deliver`（作为 runtime）/ 执行 call |
| `entry` | 人类入口服务 | 接收 `deliver`，投递人类消息 |
| `participant` | owner 邀请的 agent/human | `message` / `event`；不能 `deliver`、不能拉新人 |

### 6.5 Authority 矩阵

| 操作 | 校验方 | 规则 | 失败 |
|---|---|---|---|
| `deliver` | entry | `from == work.agent && authority=runtime`，或 `from == work.owner && authority=owner` | `permission_denied` |
| `message`（human） | reasoner | `from.kind=human`，入口 allowlist，`authority=owner` | `permission_denied` |
| `message`（agent） | reasoner | `from.kind=agent`，`authority=none`，目标必须是同 owner 的 work/participant | `permission_denied` |
| `context` | memory | `from.kind=agent/service`，work 的 agent/owner 匹配 | `permission_denied` |
| `cancel` / `renew` | reasoner / memory | owner 或 runtime | `permission_denied` |
| 工具 call | reasoner | `from.kind=agent`，`authority=none` | 不涉及 authority |
| `health` / `metrics` | 任意 | 无 | — |

**v2.1 限制**：没有认证，`from` 是自称；上表只能在可信内网内执行。不要把它当公网安全边界。

### 6.6 Lease 账本

```jsonc
{
  "deadline_at": "2026-11-05T21:00:00Z",
  "max_tool_calls": 50,
  "max_tokens": 100000,
  "max_approvals": 5,
  "max_children": 8,
  "max_depth": 4
}
```

- **memory 是唯一账本**：`works` 行存 `deadline_at / tools_left / tokens_left / approvals_left / children_left / depth_left`。
- `lease_charge(work, delta)` 原子扣减；`lease_release(work, delta)` 归还；`lease_get(work)` 读快照。
- reasoner 按轮批量预留（例如 10 次工具 / 10k token），结束时结算；批量用完再预留。
- 工具不直接扣账；reasoner 根据实际结果扣；审批在发起前扣 `approvals_left`。
- 耗尽：`resource_exhausted` + work `stalled`；owner 可 `renew`，不是静默杀。
- 默认：10 分钟 / 50 次工具 / 100k token / 5 次审批 / 8 个子 work / 深度 4。

### 6.7 Stall 与清理

**执行者**：memory 的 `work_sweep`（默认 30s 一次）。reasoner 可以上报进度，但不拥有 sweep。

`sweep` 做：

1. `deadline_at < now` 且非终态 → `stalled`。
2. `progress_hash` 连续 `stall_after_ms` 不变 → `stalled`。
3. `work_messages` 等待图有环 → `stalled`。
4. owner/agent 离线超过 TTL → `orphaned`。
5. `work_messages` 超过 TTL → `expired`。
6. 终态 work 超过 `retention_ms` → 归档/删除；`messages` 按保留策略清理。

`sweep` 只做维护，不派活；不构成中央调度器。

### 6.8 Work 表（只在 memory）

```sql
works(
  id TEXT PRIMARY KEY,
  parent TEXT,
  root TEXT NOT NULL,
  class TEXT NOT NULL,
  owner_kind TEXT NOT NULL, owner_id TEXT NOT NULL, owner_tenant TEXT,
  agent_kind TEXT NOT NULL, agent_id TEXT NOT NULL,
  entry_kind TEXT NOT NULL, entry_id TEXT NOT NULL,
  goal TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  deadline_at TEXT,
  tools_left INTEGER NOT NULL DEFAULT 0,
  tokens_left INTEGER NOT NULL DEFAULT 0,
  approvals_left INTEGER NOT NULL DEFAULT 0,
  children_left INTEGER NOT NULL DEFAULT 0,
  depth_left INTEGER NOT NULL DEFAULT 0,
  progress_hash TEXT NOT NULL DEFAULT '',
  progress_at TEXT,
  policy_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at TEXT
);

work_participants(
  work_id TEXT NOT NULL,
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  role TEXT NOT NULL,
  status TEXT NOT NULL,
  joined_at TEXT NOT NULL,
  PRIMARY KEY(work_id, actor_kind, actor_id)
);

work_messages(
  id TEXT PRIMARY KEY,
  work_id TEXT NOT NULL,
  from_kind TEXT NOT NULL, from_id TEXT NOT NULL,
  to_kind TEXT NOT NULL, to_id TEXT NOT NULL,
  method TEXT NOT NULL,
  body_ref INTEGER,            -- messages.id；正文只存一份
  state TEXT NOT NULL,         -- pending|claimed|delivered|failed|expired
  lease_until TEXT,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
```

- `messages` 表加 `work_id`；A2A 正文只存 `messages` 一份，`work_messages.body_ref` 只存指针。
- reasoner 只缓存 work 行；工具不建 work 表；entry 只认 `deliver` 的 `conversation`。
- `memory.context` 首次调用时创建 work 行；重复调用幂等。
- `work_get` / `work_open` / `work_update` / `work_close` / `lease_charge` / `lease_release` 是 memory 内部 method，**不进协议 method 表**。

---

## 7. 核心 Methods

| method | 方向 | 说明 | tool |
|---|---|---|---|
| `message` | entry/agent → reasoner | 统一的人类/agent 消息 | true（reasoner） |
| `context` | reasoner → memory | 同步组装上下文 | false |
| `deliver` | reasoner → entry | 只有 owner/runtime 能调 | false |
| `cancel` | owner/runtime/caller | 取消 work / stream | false |
| `renew` | owner/runtime | 续 lease | false |
| `health` | any | 存活/就绪/版本 | false |
| `metrics` | any | 指标快照 | false |

控制面 method 见 §5.3；memory 内部 method 见 §6.8。

### 7.1 `message`

```jsonc
call method=message
body: {
  "text": "帮我看一下这个 CSS 变量",
  "sender": { ... },          // human 才有
  "intent": "ask | reply | inform",
  "reply_to": "01J...",       // agent 回复才有
  "conversation": "web:private:owner" // human 入口才有
}
```

- human：`from.kind=human`、`authority=owner`；reasoner 走人类路径。
- agent：`from.kind=agent`、`authority=none`；只能发给同 owner 的 work/agent；participant 只能 reply。
- 没有 `user_message` / `agent_message` 两套。

### 7.2 `context`

```jsonc
call method=context work=w1
body: {
  "messages": [ ... ],
  "owner": { ... },
  "conversation": "web:...",
  "task_name": "..."
}
→ reply body: {
  "messages": [ ... ],
  "work": { ... },
  "thread": { ... }          // A2A 时
}
```

- memory 负责 ensure work、查历史、组装记忆、返回上下文。
- reasoner 拿到结果后才开黑板、调 LLM；memory 失败 → 不调 LLM。
- 没有 `recall` 的 accepted-only，也没有 `resume` 回调。
- 返回体有上限；超限截断/分页，并在 `warnings` 里说明。

### 7.3 `deliver`

```jsonc
call method=deliver work=w1
body: {
  "conversation": "web:private:owner",
  "text": "最终答复",
  "final": true
}
```

- 只有 `work.agent + authority=runtime` 或 `work.owner + authority=owner`。
- participant 调 `deliver` → `permission_denied`。
- entry 若没有 work 行，可调 memory 内部 `work_get` 解析 `work -> conversation`；这是有界缓存/查询，不是第二张 work 表。

### 7.4 `cancel`

```jsonc
call method=cancel work=w1
body: { "reason": "用户停手", "stream": "s1" } // stream 可选
```

- Work 标 `cancelled`；在飞工具 **best-effort**：可能完成，也可能返回 `late_result`。
- 不承诺强杀；需要在 tool 卡里声明是否支持取消。
- 取消后的结果作为 `event` 回来，不再改变终态。

### 7.5 `renew`

```jsonc
call method=renew work=w1
body: { "lease": { ... } }
```

- 只有 owner/runtime。
- 与 lease 耗尽的竞争由 memory 原子处理：谁先成功谁生效；失败回 `conflict`。
- 续租不得把 `deadline_at` 改到过去。

### 7.6 `health`

```jsonc
reply body: {
  "status": "ok | degraded | fail",
  "version": "0.1.0",
  "uptime_ms": 123456,
  "checks": [
    { "name": "center", "status": "ok", "latency_ms": 1 },
    { "name": "db", "status": "ok", "latency_ms": 0 }
  ]
}
```

- HTTP 侧：`/healthz`（liveness）、`/readyz`（readiness）。
- `degraded` 不等于死；不要因为依赖慢就判 fail。

### 7.7 `metrics`

```jsonc
reply body: {
  "service": "reasoner",
  "version": "0.1.0",
  "timestamp": "RFC3339",
  "metrics": [
    {
      "name": "sump_requests_total",
      "type": "counter",
      "help": "Total requests",
      "unit": "1",
      "labels": { "method": "message", "code": "ok" },
      "value": 42
    },
    {
      "name": "sump_request_duration_seconds",
      "type": "histogram",
      "help": "Request latency",
      "unit": "s",
      "labels": { "method": "message" },
      "count": 42,
      "sum": 3.14,
      "buckets": [ { "le": 0.005, "count": 10 }, { "le": 0.01, "count": 20 } ]
    }
  ]
}
```

- HTTP 侧：`/metrics`（OpenMetrics）。
- 指标名 `sump_<域>_<名>`；label 只放低基数维度；不放 `work` / `actor` / `tenant`。
- 每个服务必须实现最小指标集（见 §12.3）。

### 7.8 长任务

- method 卡 `long_running:true`；`call` 返回 `accepted` + `work`。
- 后续进度用 `event`；终态用 `reply` 或 `stream end`。
- 不需要 `async:true` 标志；work + lease 就是异步语义。

### 7.9 工具生成

- `tool:true` 的 method → 一个 LLM tool；名字 `service.method`；参数用 `input` schema。
- 链上方法（`deliver` / `context` / `cancel` / `renew`）`tool:false`。
- `message` 是唯一 `tool:true` 的链入口；reasoner 工具路径强制 `from.kind=agent`、`authority=none`。
- 没有中央 reserved 列表；`tool` 标记是唯一闸门。

---

## 8. 错误

```jsonc
{
  "code": "deadline_exceeded",
  "message": "memory.context 超过 30s",
  "retryable": true,
  "details": {
    "method": "context",
    "work": "01J...",
    "elapsed_ms": 30012,
    "type": "https://sump.dev/errors/deadline"
  }
}
```

固定 code：

| code | 含义 | retryable |
|---|---|---|
| `invalid_argument` | 参数/帧不合法 | false |
| `not_found` | method/target/work 不存在 | false |
| `conflict` | 状态转移冲突 / 并发冲突 | false |
| `deadline_exceeded` | 超时 | true |
| `cancelled` | 调用方取消 | false |
| `permission_denied` | authority/角色不允许 | false |
| `resource_exhausted` | 队列/流/lease 满 | true |
| `unavailable` | 目标不可达 | true |
| `internal` | 内部错误 | true |
| `stalled` | work 停滞 | false |
| `not_supported` | 未设计/未实现 | false |

规则：

- 成功 = `status:"ok"`，不另设 `ok:true`。
- `status` 与 `error` 互斥。
- 错误必须有 `code`；`message` 给人看；`details` 给机器看。
- 不得用字符串当错误码。
- 未知 method → `not_found`；未知字段按 §14 忽略。

---

## 9. 幂等、重试、deadline

### 9.1 幂等

- method 卡声明 `idempotent:true`；重试必须带 `idempotency_key`。
- 服务按 `(from.kind, from.id, method, idempotency_key)` 去重；TTL 由 `idempotency_ttl_ms` 声明（默认 1h）。
- 非幂等 method 不得自动重试。
- 重复请求返回第一次的结果（或 `conflict` + 原结果引用），不得重复副作用。

### 9.2 重试

- 只重试：`retryable:true` + `idempotent:true` + 有 `idempotency_key`。
- 拨号失败可重试；送达后的超时不重试（可能已执行）。
- 重试退避由调用方实现；协议只定义语义，不定义调度器。

### 9.3 deadline

- `timeout_ms` 是 method 上限；work `deadline_at` 是总上限。
- 实际执行取 `min(work 剩余, timeout_ms)`。
- 到点必须回 `deadline_exceeded`；不得挂住。
- 超时后的结果作为 `late_result` event 回来，不再改变终态。

---

## 10. 流式与背压

- 请求是 `call`；响应是多个 `stream` 帧。

```jsonc
stream reply_to=<call.id> work=w1
stream: { "id": "s1", "seq": 1, "end": false }
body: "你"

stream reply_to=<call.id> work=w1
stream: { "id": "s1", "seq": 2, "end": false }
body: "好"

stream reply_to=<call.id> work=w1
stream: { "id": "s1", "seq": 3, "end": true }
status: "ok"
```

- `stream.id` 由发送方生成；同一 `reply_to` 下唯一。
- `seq` 从 1 递增；接收方按 `seq` 排序；缺失/重复记 `sump_stream_gap_total`。
- `end=true` 才带 `status` / `error`；中间块不带。
- 背压：每流 `max_stream_buffer`（默认 1 MiB）；超限暂停读或回 `resource_exhausted`。
- v2.1 **不做 credit window**；只做有界 buffer + 超限错误。明确写出来，不假装有完整流控。
- 取消：`call method=cancel`，带 `work` / `reply_to`；发送方停止发送，接收方清理。

---

## 11. A2A：work 子树

- A2A thread = `work.class=thread`；有 owner、participants、lease。
- owner 创建 thread；participant 由 owner 邀请。
- `message` 到 `to.kind=agent/work` 时，memory 写 `work_messages`；reasoner 唤醒目标。
- 只有 owner/runtime 能 `deliver` 给人类；participant 只能 `message`。

### 11.1 队列语义

```text
pending ──claim──▶ claimed ──ack──▶ delivered
   ▲                  │
   └──── nack ────────┘
   │
   ├── attempts > max_attempts ──▶ failed
   └── TTL 过期 ──▶ expired
```

- `claim`：reasoner 取一条，设 `lease_until`；一次一条（同 work 可并发受 `max_inflight` 限制）。
- `ack`：`delivered`；正文已写入接收方 `messages`。
- `nack`：回 `pending`，`attempts++`；超过上限 `failed`，通知 owner。
- `expired`：TTL 到；不再投递。
- 重复投递按 `work_messages.id` 幂等；接收方见过就 ack。

### 11.2 角色

- owner：创建 thread、邀请 participant、收尾、deliver。
- participant：只能 message/event；不能邀请、不能 deliver。
- 邀请：owner 发 `message` 到新 participant 的 work，并在 body 带 `thread=<work>`；memory 写 participant 行。
- participant 离开：`message` `intent=leave`；memory 把 participant 标 `left`；不再投递。

### 11.3 Spawn / 跨 owner

- `spawn`：`work.class=spawn`；v2.1 **not designed**；调用返回 `not_supported`。
- 跨 owner：`policy.cross_owner=false`；v2.1 一律 `permission_denied`。
- `max_children` / `max_depth` 只预留给 thread；v2.1 不实现 spawn 语义。
- `capability`：v2.1 只留字段；`to.kind=capability` 返回 `not_supported`。

---

## 12. 可观测性

### 12.1 health / metrics

- 每个服务必须实现 `health` / `metrics`；HTTP 侧 `/healthz`、`/readyz`、`/metrics`。
- 二者 `tool:false`；模型不需要看运维指标。
- `metrics` 返回快照；采集器/门户按名册逐个拉；设置中心不做指标中转、不存时间序列。

### 12.2 指标格式

- 名字：`sump_<域>_<名>`
- 类型：`counter | gauge | histogram`
- 单位：`1 | s | bytes | count`
- label：低基数，白名单 `service / method / status / code / model / outcome`
- 禁止：`work` / `actor` / `tenant` / 原始错误文本 / 用户 id

### 12.3 最小指标集

| 域 | 指标 |
|---|---|
| 传输 | `sump_requests_total`、`sump_request_duration_seconds`、`sump_inflight_requests`、`sump_errors_total`、`sump_request_bytes`、`sump_response_bytes` |
| 连接 | `sump_reconnects_total`、`sump_connection_state`、`sump_heartbeat_age_seconds` |
| 队列 | `sump_queue_depth`、`sump_dropped_total{reason}` |
| work | `sump_work_active{status}`、`sump_work_stalled_total{reason}`、`sump_lease_exhausted_total` |
| A2A | `sump_a2a_messages_total{state}`、`sump_a2a_turns_total{outcome}` |
| LLM | `sump_llm_tokens_total{model,kind}`、`sump_llm_requests_total{model,outcome}`、`sump_llm_latency_seconds` |
| 工具 | `sump_tool_calls_total{tool,outcome}`、`sump_tool_duration_seconds` |
| 审批 | `sump_approval_decisions_total{decision}`、`sump_approval_wait_seconds` |

### 12.4 责任

- **骨架**：自动埋传输/连接/队列/错误指标；每个 Go 服务默认有。
- **服务**：埋自己的域指标（LLM、工具、审批、A2A）。
- **测试**：每个服务必须通过“最小指标集”契约测试；缺一个就失败。

### 12.5 日志与 trace

- 结构化日志：`work` / `id` / `method` / `from` / `to` / `duration_ms` / `status` / `error_code` / `bytes`。
- `work` 树 = trace；`id` / `reply_to` = span 关系。
- 日志不打印 body 全文；敏感字段脱敏。
- 指标是聚合，日志是单条；同一事实不两处定义。

---

## 13. 安全

- **v2.1 信任边界**：可信内网；`from` 是自称；没有认证。不要把端口暴露到不可信网络。
- **authority 只来自 work 角色或 capability**；不来自消息正文。
- **agent 的 message 永远 `authority=none`**；只有 work.agent 执行 deliver / cancel / renew 时使用 `authority=runtime`，接收方必须按 work 行校验；runtime 不能用于 message，也不能授权工具或代替用户审批。
- **participant 永远不能 `deliver`**。
- **capability**：v2.1 只留字段；未实现前 `authority=delegated` 一律 `not_supported`。
- **审批**：请求带 `work` / `from` / `authority`；人类看到“谁让谁跑的”；agent 不能批量预授权。
- **限制**：body/stream/queue/lease/approval 上限；超限 `resource_exhausted`。
- **企业版**：认证、tenant、RBAC、instances、drain/retire 属于 `enterprise` profile；v2.1 not designed。

---

## 14. Schema 与演进

- schema：JSON Schema 子集（`type / properties / required / enum / items / description`）；大 schema 用 `schema_ref`。
- `method.version` 独立演进；服务卡 `version` 是服务版本。
- 兼容规则：
  - 只加可选字段；不改字段类型/语义；不删 required。
  - 未知字段忽略；未知 method → `not_found`。
  - 未知 event：记 `sump_unknown_event_total`，不静默丢，也不回错。
  - 字段名不复用；`reserved` 先占位，不实现。
- 工具生成：`tool:true` → 一 method 一 tool；输入用 `input` schema；输出用 `output` schema；描述来自 `description`。
- CI：schema 校验、契约测试、字段名唯一性 lint、术语表 lint。

---

## 15. 迁移 v1 → SPv2.1

| v1 | SPv2.1 |
|---|---|
| `jump.action` | `call.method` |
| `response` | `reply` |
| `boss` | `work.owner`（memory work 表） |
| `trace` | `work` / `reply_to` |
| `user_message` | `message`（human） |
| `agent_message`（未实现） | `message`（agent） |
| `recall` / `resume` | `context`（同步） |
| `report_result` | `event` / `reply` |
| `deliver` | `deliver` |
| `async:true` | `long_running:true` + `event` |
| `provides` | `methods` + schema |

- **迁移方式（本仓库现状）**：**不做 v1 适配器**。所有服务直接改为只暴露 `/spv2`；v1 信封、`jump` 与 `protocol.Client` 已从代码中删除，原计划的适配器脚手架未落地。
- **切片**：frontend → embedding → settings-center → service 骨架 → timer/images/files → qq/shell/mcp/batch/approval → reasoner → protocol；已全部完成。

---

## 16. 示例

### 16.1 人类消息

```text
1. entry → reasoner
   call work=w1 method=message
   from=human:portal authority=owner
   to=service:reasoner
   body={text:"改一下门户配色", conversation:"web:private:owner"}

2. reasoner → memory
   call work=w1 method=context
   from=agent:reasoner authority=none
   to=service:memory
   body={messages:[...], owner:{...}, conversation:"web:..."}
   → reply body={messages:[...], work:{...}}

3. reasoner → shell
   call work=w1 method=run
   from=agent:reasoner authority=none
   to=service:shell
   body={command:"..."}
   → reply/event body={...}

4. reasoner → entry
   call work=w1 method=deliver
   from=agent:reasoner authority=runtime
   to=service:frontend
   body={conversation:"web:...", text:"...", final:true}
```

### 16.2 A2A

```text
1. A → reasoner
   call work=w2 method=message
   from=agent:A authority=none
   to=agent:B
   body={text:"帮我看下这段 CSS", intent:"ask"}

2. reasoner → B
   call work=w2 method=message
   from=agent:A authority=none
   to=agent:B
   body={...}

3. B → A
   reply work=w2 body={text:"原因是...", intent:"reply"}

4. A → entry
   call work=w1 method=deliver
   from=agent:reasoner authority=runtime
   body={conversation:"web:...", text:"..."}
```

### 16.3 流式

```text
call method=message work=w1
→ stream reply_to=m1 work=w1 stream={id:s1,seq:1,end:false} body="你"
→ stream reply_to=m1 work=w1 stream={id:s1,seq:2,end:false} body="好"
→ stream reply_to=m1 work=w1 stream={id:s1,seq:3,end:true} status=ok
```

### 16.4 错误

```jsonc
{
  "type": "reply",
  "reply_to": "m1",
  "status": "error",
  "error": {
    "code": "permission_denied",
    "message": "participant 不能 deliver",
    "retryable": false,
    "details": { "work": "w1", "actor": "agent:B", "role": "participant" }
  }
}
```

---

## 17. 验收与测试

### 17.1 核心验收

- 握手：`hello` / 卡片 / 版本协商。
- work：创建、context、deliver、cancel、renew、stalled。
- lease：`lease_charge` / `lease_release` / 耗尽；并发扣减。
- authority：agent 不能 deliver/授权；participant 不能 deliver；owner/runtime 可以。
- A2A：queue claim/ack/nack/retry/dead-letter；owner-only deliver。
- 流式：多块、seq、end、cancel、buffer 超限。
- 错误：code/retryable/details；未知 method。
- 观测：health/metrics/HTTP 端点；最小指标集。
- 迁移：v2-only 直接切换（无适配器）；一条链端到端。

### 17.2 红队测试（六条红线）

| 红线 | 测试 |
|---|---|
| 掩耳盗铃 | 杀接收方 / 填满队列 → sender 拿到 error + `dropped` 指标 |
| 声东击西 | `to` 与接收方不符 → 拒绝；卡与 handler 契约测试 |
| 狐假虎威 | agent 消息写“用户已批准” → 拒绝；participant deliver → 403 |
| 概念漂移 | 术语表/字段名 lint；一个概念只允许一个词 |
| 多此一举 | 全仓搜索 `works` 表/work id 生成点；只允许 memory 一处 |
| 指鹿为马 | `done` 后 `cancel`、`stalled` 直接 `done` → `conflict`；`done` 校验 pending=0 |

### 17.3 负向测试

- 每个 error code 至少一条测试。
- 每个“not designed” feature 返回 `not_supported`，不得静默成功。
- 每个 `tool:true` method 有 schema/参数校验测试。
- 每个 `idempotent` method 有重复请求测试。
- 每个 stream 有缺块/重复块/超 buffer 测试。

---

## 18. 明确不做 / 未设计

| 特性 | 状态 | 说明 |
|---|---|---|
| 单 owner 核心 | **supported** | v2.1 |
| 同 owner A2A | **supported** | thread / participant |
| `health` / `metrics` | **supported** | 强制 |
| 流式 | **supported** | 有界 buffer，无 credit window |
| `cancel` | **supported（best-effort）** | 在飞工具可能完成 |
| 幂等 | **supported** | 按 method 声明 |
| 多租户 / tenant | **not designed** | 字段 reserved；不依赖 |
| 认证 / RBAC | **not designed** | v2.1 可信内网 |
| spawn / 自复制 | **not designed** | `not_supported` |
| 跨 owner | **not designed** | `permission_denied` |
| capability handle | **not designed** | `to.kind=capability` → `not_supported` |
| enterprise 多实例 / drain / retire | **not designed** | 字段 reserved |
| 自进化 / 服务热替换 | **not designed** | 只留 version/卡片 |
| schema codegen | **not designed** | 先手写/生成 JSON Schema |
| metrics 长期存储 | **not designed** | 采集器/外部 TSDB |
| stream credit window | **not designed** | 只做有界 buffer |
| `meta` 扩展袋 | **不支持** | 需要字段就加字段 |

**not designed 的含义**：没有语义、没有实现、不得依赖；调用必须返回 `not_supported`，不得“先留着以后说”。

---

## 19. 实现顺序与 v3 触发

### 19.1 顺序

1. **P0 核心**：帧/Ref/card/hello；memory work 表 + lease 账本 + work_messages；`message` / `context` / `deliver` / `health` / `metrics`；authority 矩阵；单测。
2. **P1 工具与 A2A**：工具 call；A2A queue claim/ack/nack；stream；cancel；idempotency；契约测试。
3. **P2 观测**：骨架指标、最小指标集、门户快照；负向/红队测试。
4. **P3 迁移**：v2-only 直接切换——不做 v1 适配器；服务逐个改为只暴露 `/spv2`，全部迁完后删除 protocol 的 v1 传输。
5. **Profiles**：等真实场景；不提前实现。

### 19.2 v3 触发

以下任一发生，才升级 v3：

- 必须改核心 8 个必填字段或 7 个核心 method 的语义；
- 必须改 work/lease/authority 的语义；
- 某个 profile 必须进入核心（例如 tenant/auth 成为默认）；
- 适配器复杂度/事故类型证明 v2.1 边界不成立；
- 需要改帧边界/安全模型。

否则，v2.x 只做加法：新 method、新可选字段、新 profile、新指标。

---

## 附录 A：六条红线

1. **无静默终态**：call 必须有 reply/stream end；event 不是终态；drop 必计数；error 必有 code。
2. **无未校验的 `to`/`from`/`authority`**：`to` 必须匹配接收方；`from` 只是声明；authority 必须由 work 行 + capability 校验。
3. **无第二份真相**：一个 work 表（memory）、一个 id（`work`）、一个 error shape、一个发现路径（`hello`/卡）。
4. **无未版本化的语义**：字段名不复用；schema 有版本；改名 = 破坏性变更。
5. **无不可派生的重复存储**：能算出来的不存；存了就必须能重建。
6. **无非法状态转移**：`status`/`error` 互斥；`done` 必须无 pending child/call/lease；指标标签必须与状态一致。

---

## 附录 B：许愿式编程审计

| 许愿特征 | SPv2.1 处理 |
|---|---|
| 只写能力不写 owner | §6.7 / §6.8 明确 sweep、work 表、lease 账本 owner |
| 只写 happy path | §8 / §9 / §10 写超时、取消、重试、重复、背压 |
| “以后支持”没边界 | §18 标 `not designed`，调用 `not_supported` |
| 名词代替机制 | work/lease/authority 都有状态机/表/矩阵 |
| 没有状态机 | §6.3 明确状态与非法转移 |
| 没有资源上限 | §3.5 / §6.6 / §10 有上限 |
| 没有幂等 | §9.1 明确 scope/TTL |
| 没有权限 | §6.5 / §13 有矩阵与限制 |
| 没有观测 | §12 有最小指标集与契约测试 |
| 接口只有名字 | §5.2 method 有 schema/shape/tool/idempotent |
| 测试留以后 | §17 有红队/负向/契约测试 |
| 用“灵活”掩盖取舍 | §18 明确 supported / not designed |
| 一词多义 | §2 命名基线 |
| 假设环境良性 | §13 明确可信内网、无认证 |
| 把复杂度推给调用方 | §6.6 / §6.8 / §10 定义账本、队列、背压 |
| 用未来为现在背书 | §18 不实现 profile；§19 定义 v3 触发 |
| 伪抽象 | 删除 operation/provenance/meta 等中间层 |
| 没有版本/迁移 | §14 / §15 |
| 没有数据生命周期 | §6.7 retention/sweep |
| 预留=已设计 | §18 明确 not designed |

---

*SPv2.1 草案 v0.1。核心冻结；profile 未设计。*