# batch 服务 · 协议设计

> 本文只定义**协议与语义**。工具怎么落地、未来接什么外部能力，不在本文范围。
> 复核通过才写码。

## 0. 一句话

一个动作，一次声明 N 项工作 + 并发度 + **唤醒时机**；受理即回执，跑完（或关键项跑完）
推一条汇总。**编排权在调用方**：batch 不排队、不重试、不替调用方做调度决定。

## 1. 协议类型（落点 `protocol/batch.go`）

信封沿用现有 `JumpPayload{action, input}`，下面是各动作的 input / 返回 data。

### 1.1 工作项

```go
// BatchItem 是一项待执行的工作。
type BatchItem struct {
	Input msgpack.RawMessage `msgpack:"input"`          // 原样透传给目标动作，batch 不解释内容
	Key   bool               `msgpack:"key,omitempty"`  // 关键项：全部关键项完成时触发 key 唤醒
	Tag   string             `msgpack:"tag,omitempty"`  // 可选标识，汇总与明细里原样带回
}
```

`Input` 用 `msgpack.RawMessage` 与 `JumpPayload.Input` 同类型：batch 只负责搬运，不解析。

### 1.2 唤醒策略

```go
// WakePolicy 决定"什么时候叫醒调用方"。每次任务至多触发一次。
//
// 只有两档：
//   all（默认）：全部项结束后唤醒（与终态补报重合，只发一条）
//   key        ：全部关键项结束后唤醒一次；全部项结束再发一条终态
type WakePolicy struct {
	Mode string `msgpack:"mode,omitempty"` // "" / "all" / "key"
}
```

### 1.3 发起

```go
type BatchRunPayload struct {
	Service     string      `msgpack:"service"`
	Action      string      `msgpack:"action"`
	Items       []BatchItem `msgpack:"items"`
	Concurrency int         `msgpack:"concurrency,omitempty"` // 0 = 服务默认（8）
	OnError     string      `msgpack:"on_error,omitempty"`    // ""/continue（默认） | stop
	TimeoutMS   int64       `msgpack:"timeout_ms,omitempty"`  // 单项超时（0 = 默认 60000）
	Kind        string      `msgpack:"kind,omitempty"`        // ""/once（默认） | resident（常驻，见 §3.11）
	Wake        WakePolicy  `msgpack:"wake,omitempty"`
}

// BatchRunResult 是受理回执。
//
// 字段名必须是 item_id：reasoner 的 trackAsync 只认这个键（reasoner/server/infer.go），
// 少一个字母，后续所有唤醒都会被当成"没有对应在途受理"而丢弃。
type BatchRunResult struct {
	ItemID string `msgpack:"item_id"` // 任务句柄（补报、状态、明细、取消都用它）
	Total  int    `msgpack:"total"`
	Phase  string `msgpack:"phase"` // "accepted"
	Kind   string `msgpack:"kind,omitempty"` // 回显：once / resident
}
```

### 1.4 状态

```go
type BatchStatusPayload struct {
	ItemID string `msgpack:"item_id"`
}

type BatchStatusResult struct {
	ItemID    string `msgpack:"item_id"`
	Service   string `msgpack:"service"`
	Action    string `msgpack:"action"`
	Phase     string `msgpack:"phase"` // accepted | running | done | failed | cancelled
	Total     int    `msgpack:"total"`
	Ok        int    `msgpack:"ok"`
	Failed    int    `msgpack:"failed"`
	Skipped   int    `msgpack:"skipped"`
	Pending   int    `msgpack:"pending"`   // 尚未开始 + 正在跑
	KeyTotal  int    `msgpack:"key_total"`
	KeyDone   int    `msgpack:"key_done"`
	Woken     bool   `msgpack:"woken"`     // key 唤醒是否已触发
	ElapsedMS int64  `msgpack:"elapsed_ms"`
}
```

### 1.5 明细（分页）

```go
type BatchResultPayload struct {
	ItemID string `msgpack:"item_id"`
	Offset int    `msgpack:"offset,omitempty"`
	Limit  int    `msgpack:"limit,omitempty"`  // 默认 20，上限 100
	Status string `msgpack:"status,omitempty"` // 只看一类：ok | failed | skipped | pending
}

type BatchItemResult struct {
	Seq     int    `msgpack:"seq"`
	Tag     string `msgpack:"tag,omitempty"`
	Key     bool   `msgpack:"key,omitempty"`
	Status  string `msgpack:"status"`            // ok | failed | skipped | pending | running
	Error   string `msgpack:"error,omitempty"`
	Preview string `msgpack:"preview,omitempty"` // 响应摘要（截断到 preview_bytes）
	Size    int    `msgpack:"size,omitempty"`    // 原始响应字节数（截断前）
	MS      int64  `msgpack:"ms,omitempty"`
}

type BatchResultResult struct {
	ItemID  string            `msgpack:"item_id"`
	Offset  int               `msgpack:"offset"`
	Limit   int               `msgpack:"limit"`
	Matched int               `msgpack:"matched"` // 满足 Status 过滤的总条数
	Items   []BatchItemResult `msgpack:"items"`
}
```

### 1.6 控制（暂停 / 恢复 / 停止）

三种操作合成一个动作：工具表少两条，模型也少犯"该用哪个"的错。

```go
type BatchControlPayload struct {
	Op     string `msgpack:"op"`                // pause | resume | cancel
	Scope  string `msgpack:"scope,omitempty"`   // ""/one（默认） | all
	ItemID string `msgpack:"item_id,omitempty"` // scope=one 时必填
}

type BatchControlResult struct {
	Op       string `msgpack:"op"`
	Scope    string `msgpack:"scope"`
	Affected int    `msgpack:"affected"` // 实际改变了状态的任务数
	Paused   int    `msgpack:"paused"`   // 操作后仍处于 paused 的任务数
	Running  int    `msgpack:"running"`
}
```

**幂等**：对已结束的任务 pause/resume/cancel 返回 `affected: 0`，**不报错**——
调用方从 `Running`/`Paused` 就能看出真实状态。只有"任务根本不存在或已过期"才报
`BATCH_JOB_UNKNOWN`。

### 1.7 队列视图

```go
// BatchListPayload 查任务队列（LLM 与前端共用同一个动作，用途不同）。
type BatchListPayload struct {
	Phase  string `msgpack:"phase,omitempty"`  // "" = 全部 | running | paused | done | failed | cancelled
	Limit  int    `msgpack:"limit,omitempty"`  // 默认 20，上限 100
	Caller string `msgpack:"caller,omitempty"` // 只看某个发起方
}

// BatchJobView 是队列里的一行。
type BatchJobView struct {
	ItemID    string `msgpack:"item_id"`
	Service   string `msgpack:"service"`
	Action    string `msgpack:"action"`
	Caller    string `msgpack:"caller"`
	Kind      string `msgpack:"kind,omitempty"` // ""/once | resident
	Phase     string `msgpack:"phase"`          // accepted | running | paused | done | failed | cancelled
	Total     int    `msgpack:"total"`
	Ok        int    `msgpack:"ok"`
	Failed    int    `msgpack:"failed"`
	Skipped   int    `msgpack:"skipped"`
	Running   int    `msgpack:"running"`
	Pending   int    `msgpack:"pending"`
	KeyTotal  int    `msgpack:"key_total"`
	KeyDone   int    `msgpack:"key_done"`
	Summary   string `msgpack:"summary,omitempty"` // 一句话摘要（失败原因归组；无失败时为空）
	ElapsedMS int64  `msgpack:"elapsed_ms"`
}

type BatchCounts struct {
	Running int `msgpack:"running"`
	Paused  int `msgpack:"paused"`
	Done    int `msgpack:"done"`
	Failed  int `msgpack:"failed"`
}

type BatchListResult struct {
	Jobs   []BatchJobView `msgpack:"jobs"`
	Counts BatchCounts    `msgpack:"counts"`
}
```

**"队列"的准确含义**：这里只有「已受理、在跑、已结束」三类任务，**没有「排队等待」的任务**——
超过全局上限是 `BATCH_BUSY` 拒绝，不是排队（§4 第 8 条）。

### 1.8 `Provide` 新字段（改 `protocol/payloads.go`）

```go
// Batchable 显式声明该动作可被 batch 批量调用（默认 false）。
// 与 Tool 同一套哲学：能力由服务自己表态，不由调度方猜。
Batchable bool `msgpack:"batchable,omitempty"`
```

### 1.9 `notes` 通道（通用，改 `protocol/payloads.go`）

```go
// Notes 是随用户消息一起送进模型的附注：**不是用户说的话**，是入口服务替系统转达的事实。
//
// 通用通道。批量任务的用户操作是第一个使用者；以后别的东西（定时器触发、
// 后台任务结果……）需要"随用户下一条消息一起送到"时，都放这里。
Notes []string `msgpack:"notes,omitempty"`
```

| 项 | 规矩 |
|---|---|
| 谁写 | **只有入口服务**（门户 / QQ）。服务之间不往里塞 |
| 每条自描述 | 自带时间与前缀（"12:19 用户暂停 …"）；渲染方只拼接、不解释 |
| 顺序 | 按时间正序，最新的一条离用户正文最近 |
| 上限 | 最多 20 条 / 合计 4 KiB；超出保留最新的，前面加一行"（更早的 N 条已省略）" |
| 送达即清 | 入口服务在消息投递成功后清空自己的队列 |
| 不放什么 | 模型自己的操作（它知道）、周期性进度（那是推送的活） |
| 渲染 | reasoner 拼成前缀块，首行必须写明**"这不是用户现在说的话"** |

## 2. 动作表
| 动作 | Tool | Async | Batchable | 说明 |
|---|---|---|---|---|
| `run` | ✅ | ✅ | ❌ | 发起一批 |
| `status` | ✅ | ❌ | ❌ | 查单个任务进度 |
| `result` | ✅ | ❌ | ❌ | 取明细（分页） |
| `list` | ✅ | ❌ | ❌ | 查任务队列（LLM 与前端共用） |
| `control` | ✅ | ❌ | ❌ | 暂停 / 恢复 / 停止（单个或全部） |

五个动作都进 `provides` 且 `Tool: true`。
batch 自身**不可被批量**（`Batchable: false`）。

## 3. 语义

### 3.1 受理即回执

`run` 的响应只表示"已受理"：校验通过、任务入库、开始派发，然后立即返回。
**不等任何一项完成。** 调用方的收尾不被批量任务扣住。

### 3.2 唤醒

两项时间的取舍决定了调用方什么时候能看到东西（下图假设无失败；有失败时多一条"首次失败"通知）：

```
wake = all                                  wake = key（封面、目录标 key）
──────────────────────────────────         ──────────────────────────────────
0s   run(300 项)                             0s   run(300 项)
0s   ← 回执 {item_id, total:300}             0s   ← 回执 {item_id, total:300}
…40 秒静默…                                   3s   ← 非终态补报：关键项已完成
40s  ← 终态补报（297 好 / 3 坏）                     （其余 297 项仍在跑）
                                             40s  ← 终态补报
```

**三个触发点，每次任务至多三条推送**：

| # | 触发 | phase | final | 用途 |
|---|---|---|---|---|
| 1 | 关键项全部结束（`mode=key`） | `progress` | 否 | "我要的东西好了" |
| 2 | **第一项失败** | `progress` | 否 | 带计数快照，供 LLM 决定继续还是取消 |
| 3 | 全部项结束 | `done`/`failed`/`cancelled` | 是 | 收尾 |

**`key` 的作用域仅限本任务内部**：它决定"这一个任务什么时候推一条进度"，
不参与跨任务的任何判断（例如次级停手的兜底只看"任务是否终态、是否常驻"，见 §3.10）。

硬规则：

1. **同一瞬间重合的触发合并成一条**：`all` 时只有终态；所有项都标 `key` 时也只有终态
2. 第 2 条只在 `on_error: continue` 下有效——`stop` 会立即结束，终态已包含失败
3. `key` 但**没有任何项标 `key`** → 拒绝受理（`BATCH_WAKE`），不静默降级
4. **唤醒不改执行**：除 `on_error: stop` 外，唤醒后其余项继续跑
5. 第 2 条只推**一次**：后 49 项再失败也不再叫（明细里都有，不能拿它当进度条）

### 3.3 阶段词汇

| phase | 含义 | final |
|---|---|---|
| `accepted` | 已受理，尚未开始派发 | — （这是回执里的值） |
| `running` | 正在跑 | 否 |
| `paused` | 暂停中：不再开始新项，在飞的会跑完 | 否 |
| `progress` | key 唤醒 / 首次失败（推送用的非终态词汇） | 否 |
| `done` | 全部项结束，无失败 | 是 |
| `failed` | 全部项结束，有失败（`on_error: stop` 提前结束时也算） | 是 |
| `cancelled` | 被 `control{op:"cancel"}` 结束 | 是 |

### 3.4 失败

- `on_error: continue`（默认）：单项失败只记该项，任务继续
- `on_error: stop`：第一项失败后，**未开始的项转 `skipped`**，在飞的跑完，任务以 `failed` 结束
- 终态补报里**按错误原文归组**（同一句错误只列一次，带项数与样例 tag），文本上限 2 KiB
- **不重试**：要重试就发一个新任务，只带失败项——重试策略归调用方

**首次失败**（非终态，只发一次，带四个计数供 LLM 决策）：

```
批量进度：50 项，完成 12，失败 1，在跑 8，待跑 29（item_id=b-7f3a）
首次失败：封面 — 文件不存在：data/封面.docx
继续就等终态；要停：batch__cancel {"item_id":"b-7f3a"}
```

**终态补报**：

```
批量完成：50 项，成功 47，失败 3，耗时 42s（item_id=b-7f3a）
失败集中在 1 类：
  · 文件不存在（3 项）：封面, 目录, 附录-1
明细：batch__result {"item_id":"b-7f3a","status":"failed"}
```

### 3.5 控制：暂停可恢复 / 停止是终态

| 操作 | 未开始的项 | 在飞的项 | 任务 phase | 可恢复？ |
|---|---|---|---|---|
| `pause` | 保持 `pending`，**不再开始** | 跑完 | `paused` | ✅ `resume` |
| `resume` | 继续开始 | — | `running` | — |
| `cancel` | 转 `skipped` | 跑完 | `cancelled` | ❌ 终态 |

**"可恢复"的准确含义**（这是 pause 与 cancel 的唯一区别，也是它存在的理由）：

- 已完成的项**不重跑**：`ok` / `failed` 原样保留，计数器不回退
- 未开始的项**保持 `pending`**：恢复后从断点继续，不是从头再来
- **进度不丢**：暂停 10 分钟再恢复，前 300 项仍然算完成

**状态机**：

```
accepted ──→ running ⇄ paused ──→ done | failed
    │            │
    └────────────┴──→ cancelled
```

三条硬规矩：

1. **在飞的项一律跑完**：pause 与 cancel 都不中途杀进程——半路终止会留下半写的文件
2. **pause 释放执行槽位**：暂停的任务不再占 `max_inflight` 名额，别的任务可以插进来
3. **`scope: all` 是全局的**：不按 caller 过滤，对 batch 里**所有**非终态任务批量执行；
   已结束的跳过（不计入 `affected`）。前端与模型看到的是同一个全局池

**代价要说清**：暂停一个有 8 项在跑的任务，最多还会再完成 8 项才真正静止。

### 3.6 重启恢复

任务状态在 SQLite 里，服务重启后必须有个明确归宿——否则 `running` 的任务会永远挂着。

| 重启时的状态 | 重启后 | 理由 |
|---|---|---|
| `accepted` / `running` 的任务 | → **`paused`**，日志记一条"因重启转暂停" | 不替调用方决定"把 50 个任务同时放起来" |
| 重启时正在跑的**项** | → `failed`，error = "服务重启中断，执行结果未知" | 不能假装成功，也不能擅自重跑（可能有副作用） |

恢复由调用方决定：`control {op: "resume"}`。
已 `ok` 的项不会重跑；中断的项停在 `failed`，要不要重发由调用方判断。

> 想要"重启后自动接着跑"，把设置项 `batch.resume_on_start` 改成 `true`。
> 默认关：自动放起 50 个任务比手动点一下更危险。

### 3.7 明细查询

- 只存**摘要**（`preview_bytes`，默认 2 KiB），完整响应不落库——需要就再调一次目标动作
- 分页：`limit` 默认 20、上限 100；`status` 可过滤
- 任务过期（`job_ttl_ms`）后查询 → `BATCH_JOB_UNKNOWN`

### 3.8 队列：两个消费者，两种频率

同一条队列，LLM 与前端页面的消费方式**必须分开**——否则页面一打开就把模型吵死。

| 消费者 | 拿什么 | 频率 | 通道 |
|---|---|---|---|
| LLM | 至多 3 条推送；要更多就主动查 | 稀疏 | `report_result` 推送 + `list`/`status`/`result` |
| 前端页面 | 全量任务列表 + 计数 | 密集（1s） | 前端服务端**轮询** `batch.list` → SSE 推浏览器 |

**前端不走推送通道**：batch 不广播、也不知道谁在看页面。前端按自己的节奏拉——
既符合"服务间只做工作数据流"，也让页面刷新频率与模型上下文彻底解耦。

轮询节奏由前端决定：有 `running` 任务时 1s；全部结束退到 5s；没有浏览器订阅就停。

### 3.9 两条控制路径：模型操作 vs 用户操作

| | 模型操作 | 用户操作 |
|---|---|---|
| 生效 | 立即 | 立即 |
| 谁发起 | LLM 调 `batch__control` | 点页面按钮 → 前端服务端 jump |
| 影响 batch | 指定任务 | 指定任务（`all` = 全局） |
| 影响 agent | **不影响**：它本来就在跑，拿到返回值继续 | **看碰到什么**：非常驻 → 停手（全部=一级，单个=次级）；只有常驻 → 不停手（见 §3.10） |
| 怎么回到模型 | 这次工具调用的返回值 | 下次输入的 notes 块 |
| 会不会唤醒 | 不额外唤醒 | **不唤醒，且不许开新动作** |

**为什么用户操作必须多一步**：模型自己暂停/停止时，它清楚上下文，可以接着做别的事。
用户操作不一样——用户不会无缘无故按暂停，**此时 agent 继续跳服务毫无意义**，
它不知道用户想要什么。

### 3.10 用户操作之后：停手分三级，看操作碰到了什么

**停不停手，取决于这次操作有没有碰到非常驻任务。**

| 这次操作影响到的任务 | agent | 通知送达 |
|---|---|---|
| 有非常驻任务，且是「全部」操作 | **一级停手**：立即停 | **只**在下一条用户消息 |
| 有非常驻任务，但是单个 / 部分操作 | **次级停手**：立即停 | 下一条用户消息 **或** 其他非常驻任务**全部**结束——谁先到 |
| **只有常驻任务** | **不停手** | 随下一次汇报 / 输入带出去，不着急，**无兜底** |

**为什么常驻任务不触发停手**：常驻任务是后台基础设施（挂着的后端、前端、监听进程），
用户随手暂停、重启它是**日常操作**，不代表"agent 做错了什么"。
这种事不值得让 agent 停手，消息迟一点到就行。

**次级停手的兜底怎么触发**：暂停发生时检查一次"还有没有别的**可执行**任务"；
之后每有一个任务结束再看一次。条件是**其他非常驻任务全部结束**（不是结束一个就算），
条件成立才在当前在飞动作结束之后唤醒 agent。

**兜底只看两件事**：任务是否**常驻**、任务是否**已终结**。
**不看 `key`**——`key` 只在单个任务内部决定"什么时候推一条进度"（§3.2），
与跨任务的兜底无关。一个任务里有几个关键项完成，不算这个任务结束。

**如果没有别的任务在跑**：兜底条件当场成立，agent 会在在飞动作结束时被唤醒
（而不是等到用户下次说话）。

**在飞的调用跑完不中断**（与"在飞的项跑完"同一口径）：
停手只保证"之后不再有新动作"。

### 3.11 常驻任务

```go
Kind string `msgpack:"kind,omitempty"` // ""/once（默认） | resident（常驻）
```

- **只影响唤醒聚合**：`resident` 不参与"其他非常驻任务是否全部结束"的判断
- **用户可随时操作**：暂停、恢复、停止常驻任务都不触发停手，通知随下次汇报送达
- 执行上没有任何区别：有限项、照样计数、照样能暂停/停止
- **v1 不提供"永不完结"的执行模型**。`resident` 是一个声明（"别等它"），
  不是循环 / 监听。真要做常驻（盯目录、轮询数据源），那是另一个执行模型，另开设计

`BatchRunResult` 与 `BatchJobView` 都回显 `Kind`，让下游能立刻区分。

### 3.12 跨服务协议：`reasoner.user_hold`

```go
// UserHoldPayload 是入口服务把"用户停手"这件事立刻告诉推理侧。
type UserHoldPayload struct {
	ConversationID string   `msgpack:"conversation_id"`
	Hold           string   `msgpack:"hold,omitempty"`         // ""/none | secondary | primary
	PausedItems    []string `msgpack:"paused_items,omitempty"` // 被暂停/停止的异步句柄
	Notes          []string `msgpack:"notes,omitempty"`        // 随之投递的附注（与 §1.9 同名同义）
}
```

`Hold` 由**前端**决定，因为它知道这次操作碰到了哪些任务（`BatchJobView.Kind`）：

| `hold` | 何时用 | agent 立即 | 解除 / 通知送达 |
|---|---|---|---|
| `primary` | 全部操作，且碰到非常驻任务 | 停手 | **只有**下一条用户消息 |
| `secondary` | 单个/部分操作，且碰到非常驻任务 | 停手 | 下一条用户消息 **或** 其他非常驻任务全部结束 |
| `none` | 只碰到常驻任务 | **不停手** | 随下一次汇报 / 输入带出去，无兜底 |

**为什么这个动作也要带 `Notes`**：次级兜底的唤醒**不是用户消息**，没有别的地方能携带文本。
放在这里，reasoner 两条路都能投递：

- 用户下一条消息到达 → 把 `Notes` 并进那条消息的 `notes`（同一通道）
- 兜底唤醒 → 把 `Notes` 作为这次唤醒的说明

**去重规矩**：交给 `user_hold` 的 `Notes`，入口服务**不再**重复放进消息的 `notes`。

`PausedItems` 里的句柄会被标成 paused，**不再算"可执行"**——否则那个被暂停的任务
永远不结束，次级停手的兜底永远触发不了。

次级停手的检查时机：`user_hold` 到达时一次，之后每个异步任务终结时再看一次。

**`user_hold` 不进 provides、不是模型工具**：它是入口服务用的控制面。

**代价**：硬挂起那一刻如果 LLM 正在生成，那一轮仍会送达（首字已出，收不回）。
挂起只保证"之后不再有新动作"。

**通知的形状**（前端组好）：

```
（系统提示：以下是你上次回复之后、用户在界面上直接做的操作，不是用户现在说的话。
这些操作都是用户主动发起的——不是失败，也不是系统异常，不要试图"修复"它们。）
- 12:19 用户暂停 任务 b-7f3a（files.convert，20/50 完成，8 项在跑）
  原因：用户操作。现在可以恢复（batch__control op=resume）。
- 12:20 用户停止 全部任务（影响 3 个任务，其中 1 个有在跑的项）
  原因：用户操作。被停止的未开始项已作废，**不会自动重跑**；要接着做就重新发起。
```

最后那句是必需的：否则 agent 看到"继续"会以为任务还完整，实际上被 cancel 的项已经作废。

**载体：已定为 §1.9 的 `notes` 通道**（`UserMessagePayload.Notes`）。
用户的话与系统附注在数据层分开；reasoner 渲染成前缀块。

**"全局暂停"不等于"冻结"**：它只停住**已存在**的任务；之后新建的任务照常跑。
如果要"从现在起什么都不许再启动"，那是另一个概念（服务级冻结开关），本协议不做。

## 4. 边界

| # | 规矩 | 错误码 |
|---|---|---|
| 1 | `items` 为空或超过 `max_items` | `BATCH_ITEMS` |
| 2 | 目标服务不在名册 | `BATCH_TARGET_UNKNOWN` |
| 3 | 目标动作未声明 `Batchable: true` | `BATCH_NOT_BATCHABLE` |
| 4 | 目标是 batch 自己（禁止套娃，深度 = 1） | `BATCH_NESTED` |
| 5 | `concurrency` 越界（<1 或 > `max_concurrency`） | `BATCH_CONCURRENCY` |
| 6 | `wake.mode` 不是 `all`/`key` | `BATCH_WAKE` |
| 7 | `wake.mode=key` 但没有项标 `key` | `BATCH_WAKE` |
| 8 | 全局在飞项数已达 `max_inflight` | `BATCH_BUSY`（**同步拒绝，不排队**） |
| 9 | 查询/控制不存在或已过期的任务 | `BATCH_JOB_UNKNOWN` |
| 10 | `op` 不是 pause/resume/cancel，或 `scope` 不是 one/all | `BATCH_OP` |
| 11 | `scope: one` 但没给 `item_id` | `BATCH_OP` |
| 12 | `on_error` 不是 `continue`/`stop` | `BATCH_ONERROR` |

**不排队的两层含义**：

- **任务级**：超全局上限 → 直接拒绝（同 shell 熔断）
- **任务内**：第 17 项等前 16 项 → 允许。这是调用方自己声明的 50 项在等自己的槽位，
  不是执行器替别人保管意图。边界没糊。

错误以 `ok:false` + `"CODE: 说明"` 返回（与 approval 的 reason code 同一形状）。

**恢复的资源语义**：暂停排空后归还该任务占的全局槽位（`max_inflight` 名额），恢复时重新申请；
申请不到就报 `BATCH_BUSY`，任务保持 `paused`——不会"假装在跑"。

## 5. 落盘（`data/batch.db`）

```sql
CREATE TABLE jobs (
  item_id     TEXT PRIMARY KEY,
  service     TEXT NOT NULL,
  action      TEXT NOT NULL,
  caller      TEXT NOT NULL,   -- 发起方服务名（企业向的隔离边界，现在只用于排查）
  boss        TEXT NOT NULL,   -- 会话入口（补报回到哪）
  trace       TEXT NOT NULL,
  phase       TEXT NOT NULL,
  total       INTEGER NOT NULL,
  ok          INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  skipped     INTEGER NOT NULL DEFAULT 0,
  key_total   INTEGER NOT NULL DEFAULT 0,
  key_done    INTEGER NOT NULL DEFAULT 0,
  woken       INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  started_at  INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  concurrency INTEGER NOT NULL DEFAULT 0,  -- 声明的单任务并发
  timeout_ms  INTEGER NOT NULL DEFAULT 0,  -- 声明的单项超时（0 = 用设置项）
  on_error    TEXT NOT NULL DEFAULT 'continue',
  wake_mode   TEXT NOT NULL DEFAULT 'all'
);

CREATE TABLE items (
  item_id  TEXT NOT NULL,
  seq      INTEGER NOT NULL,
  tag      TEXT NOT NULL DEFAULT '',
  is_key   INTEGER NOT NULL DEFAULT 0,
  status   TEXT NOT NULL,     -- pending | running | ok | failed | skipped
  input    BLOB,              -- 只在任务存活期间有值（重启恢复用），项一终结即置 NULL
  error    TEXT NOT NULL DEFAULT '',
  preview  BLOB NOT NULL DEFAULT x'',  -- 一行 JSON 文本（已把 msgpack 转成可读）
  size     INTEGER NOT NULL DEFAULT 0,
  ms       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (item_id, seq)
);
```

- 原始 `input` 只在**任务存活期**落库（重启可恢复的前提），项一终结（`ok`/`failed`/`skipped`/
  重启中断）立刻置 NULL；排查靠 `service`/`action`/`total` + 摘要
- `preview` 是**人/模型能读的 JSON 文本**，不是 msgpack 原始字节
- 执行策略（`concurrency`/`timeout_ms`/`on_error`/`wake_mode`）必须落库：
  重启恢复要照调用方当初声明的跑，不能悄悄换成默认值
- `job_ttl_ms` 到期删 `jobs` + `items`（只删终态任务）

## 6. 设置项

| 键 | 预设 | 说明 |
|---|---|---|
| `batch.concurrency` | `8` | 单任务并发默认值（调用方不写时用） |
| `batch.max_concurrency` | `64` | 单任务并发上限 |
| `batch.max_inflight` | `64` | 全局同时在跑的项数上限（熔断） |
| `batch.max_items` | `1000` | 单任务最多项数 |
| `batch.item_timeout_ms` | `60000` | 单项超时 |
| `batch.preview_bytes` | `2048` | 每项存多少响应摘要 |
| `batch.job_ttl_ms` | `3600000` | 任务可查询时长 |
| `batch.resume_on_start` | `false` | 重启后是否自动恢复被转暂停的任务（默认关：自动放起 50 个任务比手动点一下更危险） |

只读项：`name` / `addr` / `center` / `db`。

## 7. 推送载荷

复用**现有** `protocol.AsyncResultPayload`，`item_id` = 任务句柄：

| 时机 | phase | final | text |
|---|---|---|---|
| key 唤醒 | `progress` | false | 关键项完成数 + 仍在跑多少 |
| 首次失败 | `progress` | false | 共/完成/失败/在跑/待跑 + 首个错误原文 |
| 终态 | `done`/`failed`/`cancelled` | true | 汇总（见 §3.4） |

**reasoner 零改动**：`actionReportResult` 已支持非终态唤醒且不关闭登记。

## 8. 测试计划

| # | 用例 | 断言 |
|---|---|---|
| 1 | 契约 | 回执里是 `item_id`；reasoner 能登记上（打 log 验证） |
| 2 | 50 项全成功 | 全部 `ok`；终态 `done`；汇总数字对 |
| 3 | 失败归组 | 3 项同错 → 汇总里 **1 组 3 项**，不是 3 行 |
| 4 | `wake:key` | 共 **2 条**推送：key 完成时非终态 + 全部完成终态 |
| 5 | `wake:all` | 共 **1 条**推送 |
| 6 | 全部项都是 key | 共 **1 条**（重合） |
| 7 | `mode:key` 但无 key 项 | `BATCH_WAKE` |
| 8 | `on_error:stop` | 第一项失败后其余 `skipped`，终态 `failed` |
| 9 | 取消 | 未开始转 `skipped`，在飞跑完，终态 `cancelled` |
| 10 | 全局熔断 | 在飞满 64 → 新任务 `BATCH_BUSY`，**不等** |
| 11 | 套娃 | 目标 = batch → `BATCH_NESTED` |
| 12 | 未声明 Batchable | `BATCH_NOT_BATCHABLE` |
| 13 | TTL 过期 | 查询 → `BATCH_JOB_UNKNOWN` |
| 14 | **首次失败通知** | 50 项其中 3 项失败 → 推送共 **2 条**（首次失败 + 终态），且首次那条带"共/完成/失败/在跑/待跑"五个数 |
| 15 | 后续失败不再推 | 第 2、3 项也失败 → 仍只有 2 条推送 |
| 16 | `batch.list` | 过滤 `running`/`failed` 正确；`counts` 与实际一致 |
| 17 | 前端轮询不污染模型 | 连续 `list` 50 次 → 模型的推送条数不变（仍是 ≤3） |
| 18 | **暂停可恢复** | 20 项跑到 5 项时 pause → 停住；再等 2s 计数不变；resume → 跑完剩下的 15 项，**前 5 项不重跑** |
| 19 | 暂停释放槽位 | 全局上限 4 时暂停一个任务 → 新任务能进来（`BATCH_BUSY` 消失） |
| 20 | pause vs cancel | pause 后未开始项仍是 `pending`；cancel 后是 `skipped` |
| 21 | `scope: all` | 3 个任务（1 跑 1 暂停 1 完成）→ `pause_all` 的 `affected` = **1**（只有状态真变了那个）；暂停中的与已完成的一律不计（与 #22 同源） |
| 22 | 幂等 | 对已完成任务 pause/cancel → `affected: 0`，**不报错** |
| 23 | **重启恢复** | 跑到一半杀掉服务再起 → 任务 phase = `paused`；在飞的项变 `failed`（错误写明"重启中断"）；resume 后已 `ok` 的项不重跑 |
| 24 | **用户操作不唤醒模型** | 从"前端"调 `control{pause}` → 模型侧**零推送**（`report_result` 一条都没有） |
| 25 | **通知随下次输入** | 用户 pause 后再发一条消息 → 该消息带 notes 块，内容含时间/操作/任务/进度/**原因是用户操作** |
| 26 | **模型操作走返回值** | 模型调 `batch__control` → 工具返回 `affected`；无 notes 排队；**任务不挂起** |
| 27 | **`scope:all` 全局** | 两个不同 caller 的任务都能被 `pause_all` 停住 |
| 28 | **全部暂停 → 一级停手** | 用户 `pause_all` 后，模型不再派发任何新工具、不再开新轮次；**不会**被兜底唤醒 |
| 29 | **单个暂停 → 次级停手** | 用户 `pause` 单个非常驻任务 → 模型**同样停手**（不再派发新工具） |
| 30 | **兜底要等"全部"结束** | 3 个非常驻任务在跑，暂停 1 个 → 另两个**全部**结束后才唤醒；只结束 1 个时**不**唤醒 |
| 31 | **无其他任务时立刻兜底** | 暂停唯一在跑的非常驻任务 → 在飞动作结束后就被唤醒，不干等 |
| 32 | **常驻任务不挡兜底** | 有 `kind=resident` 任务在跑时，兜底唤醒仍然按时触发 |
| 32b | **动常驻任务不停手** | 暂停一个常驻任务 → `hold=none`，模型继续干活；通知随下次汇报送达 |
| 33 | **下次输入解除停手** | 一级停手后发一条消息 → 任务恢复运行，notes 随消息送达 |
| 34 | **取消的项不自动重跑** | 用户 `cancel` 后 agent 收到"继续" → 已 `skipped` 的项仍是 `skipped` |
| 35 | **别的话不误触发** | 用户只说话不暂停 → 不产生任何 note、不影响调度 |

**变异复核（至少两条，附证据）**

- 把 key 唤醒改成每项都推 → 用例 4 必须红
- 去掉套娃检查 → 用例 11 必须红
- 把回执字段改成 `job_id` → 用例 1 必须红
- 把首次失败通知改成每项失败都推 → 用例 15 必须红
- 让 `batch.list` 触发一次模型推送 → 用例 17 必须红

## 9. 登记清单

- [ ] `protocol/payloads.go` 加 `Provide.Batchable`
- [ ] `protocol/batch.go`（新）
- [ ] `go.work` 加 `./batch`
- [ ] `.github/workflows/ci.yml` **两处**模块循环都要加 `batch`（约第 48 行的测试循环、第 73 行的 vet/构建循环）
- [ ] `SPEC.md` §12 服务表 + §13 命名基线 + §7 设置项
- [ ] 端口 `127.0.0.1:9951`（已实测空闲）
- [ ] `scripts/run-local.ps1` 一键起（**保住 UTF-8 BOM**）
- [ ] `batch/README.md`
- [ ] 前端：任务队列页签（`frontend/src/server` 加 jobs 轮询 + SSE 通道，`frontend/src/web` 加面板）
- [ ] 前端：暂停/恢复/停止按钮（单个 + 全部）+ **待告知队列**（随下一条用户消息发出）
- [ ] 前端：用户 pause/stop 时**立刻**调 `reasoner.user_hold`
      （按碰到的任务决定 `hold`：非常驻+全部 → `primary`；非常驻+单个 → `secondary`；
      只有常驻 → `none`；并带 `paused_items`）
- [ ] reasoner：新增 `user_hold` 动作（不进 provides、不是模型工具）
- [ ] reasoner：`asyncJob` 记 `kind`（resident）与 `paused`，保底唤醒按"非暂停非常驻"判空
- [ ] 若采纳载体 A：`UserMessagePayload` 加 `notes []string`，reasoner 渲染成前缀块

## 10. 待复核的取舍

1. **不重试**：失败项留在结果里，重发新任务只带失败项
2. **不存原始 input**：隐私优先，排查靠 service/action/total
3. **只存摘要不存完整响应**：需要完整结果就再调一次目标动作
4. **暂停/停止都不中断在飞项**：避免留下半写的文件，代价是"最多再跑完 concurrency 项"
5. **`BATCH_BUSY` 同步拒绝**：与 shell 熔断同口径，不排队
6. **重启后默认转暂停、不自动恢复**：把"放不放大批任务"的决定权留给人/模型
7. **用户操作不唤醒模型**：通知攒到用户下次输入时一起发（见 §3.9）
8. **停手三级**：碰到非常驻任务才停手（全部=一级，单个=次级）；只碰常驻任务不停手
9. **常驻任务只影响唤醒聚合与停手判断**：v1 不提供永不完结的执行模型（见 §3.11）
