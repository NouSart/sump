# spv2 · SPv2.1 参考实现

> **读者**：要基于 SPv2.1 写服务，或评估迁移差距的人 ｜ **读完能做**：用本包起 WS 服务与客户端，分清哪些是已实现机制、哪些只是协议字段 ｜ **最后校验**：2026-11-06

**这是 SPv2.1 的参考实现，不是生产服务迁移完成版。**
核心协议、术语、错误码与验收标准见 [docs/concepts/spv2.md](../../docs/concepts/spv2.md)；本文只写实现视角，不复述 spec。

模块路径 `github.com/NouSart/sump/spv2`，是一个独立 Go 包：核心包不 import 本仓库其他服务，只依赖 `gorilla/websocket`、`oklog/ulid`、`vmihailenco/msgpack`，SQLite 实现另依赖 `modernc.org/sqlite`。

## 包结构

| 文件 | 内容 |
|---|---|
| `frame.go` | `Frame` / `Ref` / `Stream` / `Error`、常量、构造器、`Normalize`、`DeadlineFromLease` |
| `id.go` | `NewID`：时间有序的 ULID |
| `codec.go` | `Marshal` / `Unmarshal`（MsgPack）、body 编解码 |
| `validate.go` | `Frame.Validate`、`ValidateTo`、来源 authority 校验 |
| `authority.go` | `AuthorizeDeliver` / `AuthorizeControl` / `AuthorizeMessage`、`IsParticipant` |
| `errors.go` | 标准错误码、`ErrCode`、`IsCode` |
| `card.go` | `Method` / `Card` / `Limits` / `Observability` 与卡片校验 |
| `work.go` | `Work` / `Lease` / `Participant`、状态与转移、默认租约 |
| `store.go` | `WorkStore` 接口、`MemStore`、队列消息、sweep |
| `sqlitestore.go` | `SQLiteStore` / `NewSQLiteStore`：`WorkStore` 的 SQLite 实现 |
| `client.go` | `Client`：拨号、call、stream、hello、事件回调 |
| `server.go` | `Server` / `Handler` / `Call`，内置 hello / health / metrics |
| `http.go` | `Server.Mux`：`/spv2`、`/healthz`、`/readyz`、`/metrics`（OpenMetrics） |
| `metrics.go` | `Registry`、`Metric` / `Bucket` 与快照 |
| `idempotency.go` | `IdempotencyCache`：按卡片声明缓存 unary reply |
| `ref/` | 参考链路：entry → reasoner → memory → tool，四个服务走真实 WS；含 `ref_test.go` |
| `*_test.go` | 帧、校验、卡片、指标、store、WS 传输测试 |

## API

### Frame 与 Ref

- `Frame` 对应线上唯一单元：`v` / `id` / `type` / `work` / `from` / `to`，加上可选的 `method` / `reply_to` / `content_type` / `schema` / `idempotency_key` / `stream` / `status` / `error` / `body`。
- `NewCall` / `NewReply` / `NewErrorReply` / `NewEvent` / `NewStreamFrame` / `NewErrorStreamFrame` 负责生成帧并填默认值；`Normalize` 补版本、id、内容类型和 authority。
- `Marshal` / `Unmarshal` 走 MsgPack；`EncodeBody` / `DecodeBody` 处理 body。`DecodeBody` 按 `content_type` 解到目标：MsgPack/JSON 用通用解码，`text/plain` 只接受 `*string`，`application/octet-stream` 只接受 `*[]byte`。
- **结构体必须同时带 `msgpack:` 与 `json:` 标签**：msgpack 是默认编解码器，而 msgpack/v5 在缺 `msgpack:` 时退回 Go 字段名——只写 `json:"tool_calls"` 会让线上键名变成 `ToolCalls`，与 JSON 契约不一致。`msgpack_tags_test.go` 直接断言线上键名，不测 Go 结构体之间的自反往返（那种写法漏标签也照样绿）。
- `Frame.Validate` 检查版本、四种 type、work/from/to、`content_type` 白名单（msgpack/json/text/plain/octet-stream）、reply/stream 的目标是调用方，以及各 type 的排他字段（call 不携带 `reply_to` / `status` / `error` / `stream` 等）。
- **authority 帧级规则**：human 只能 `owner` / `none`；agent 允许 `none` / `runtime`（`delegated` 返回 `not_supported`）；service/system 只能 `none`。因此 agent 可以携带 `runtime` 发 deliver，但能否执行仍由 work 角色校验决定。
- `Ref` 有 `kind` / `id` / `authority` / `tenant`；`HumanRef`、`AgentRef`、`ServiceRef`、`SystemRef`、`WorkRef`、`CapabilityRef` 给默认 authority，`WithAuthority` 返回副本。`tenant` 仅为保留字段。
- `ValidateTo` 只在 `to.kind=service` 时要求目标就是本服务；`capability` 目标直接返回 `not_supported`。

### 生产骨架（src/service）怎么映射帧头

说清楚现状，免得被当成 bug 去「修」：

- 出站 `Service.Call(ctx, to, action, payload, trace, boss)` 与 `Fire` 把 **trace 当作帧头 `work`**（trace 为空时现场生成一个 ULID）——SPv2.1 里 work 是唯一的工作名词，骨架没有第二个 id 可放。
- 帧头 **`from` 是发起这一跳的服务自己**（逐跳自定）：跨服务看到的调用方身份来自 `from`，不是任何入参。
- **`boss` 不上线**：SPv2.1 帧头没有 `boss` 字段，`callSPv2` 也从不读它。生产链路在载荷里另带 boss，所以这是遗留参数、不是数据丢失。要彻底去掉它得改每个服务的签名，那属于协议讨论（AGENTS.md「协议变更先商量」），本轮不动；`src/service/spv2_trace_work_test.go` 把上面三条钉成了断言（含「整帧上线后线上不出现 boss」）。

### Card 与 Method

- `Method` 描述一个可调用能力：`name` / `shape` / `version` / `tool` / `idempotent` / `idempotency_ttl_ms` / `streaming` / `long_running` / `timeout_ms` / `description` / `input` / `output`。
- `Card.Validate` 校验协议版本、服务名与 actor、method 不重名、`shape` 只能是 call/event；`tool:true` 的 method 必须有 input schema。
- `Card.Method(name)` 按名字取 method；`Method.ToolName(service)` 生成 `service.method`。工具是否真的暴露给模型，由使用卡片的服务决定，本包不做生成。
- `Limits` 和 `Observability` 只是卡片声明；当前运行时不会按卡片里的 `limits` 自动限流。

### Work 与 Lease

- `Work` 包含 id、parent/root、class（request/thread/spawn）、owner/agent/entry、goal、status、lease、participants、policy 与时间戳。
- `CanTransition` / `IsTerminal` 实现工作状态机；`Work.Validate` 校验必填项与 class/status 合法性。
- `Lease` 是 deadline + 五类预算；`DefaultLease` 给 10 分钟、50 次工具、100k token、5 次审批、8 个子 work、深度 4。
- `Charge` / `Remaining` 是扣减与余额结构；`DeadlineFromLease` 解析 RFC3339 deadline。

### Authority

- `AuthorizeDeliver` / `AuthorizeControl` 只放行 work owner（`authority=owner`）或 work.agent（`authority=runtime`）。核心现在允许 agent 帧携带 `runtime`，就是为 deliver / cancel / renew 这条路径准备的。
- `AuthorizeMessage` 放行 owner，或 `authority=none` 的 work agent / participant；agent 带 `runtime` 不能走 message（`ref` 把它映射为 `permission_denied`）。`IsParticipant` 忽略已 `left` 的参与者。
- 这些是给业务 handler 调用的辅助函数，server 不会自动按 work 角色拦截 method。

### Store

- `WorkStore` 是 memory 侧的 work/lease/queue 账本接口；实现有 `MemStore` 和 `SQLiteStore`。参考链路与传输测试用 `MemStore`，SQLite 有自己的测试。
- **work**：`EnsureWork` 幂等创建、`GetWork`、`UpdateStatus`（走状态机；转 `done` 前检查 pending/claimed 消息与未终结子 work）、`AddParticipant`。
- **renew**：`Renew` 只上调预算、把 deadline 往后延，不减少已用余额；终态 work 拒绝续租。
- **lease**：`Charge` 原子检查 deadline 与余额，不足报 `ErrLeaseExhausted`；`Release` 归还但不超过初始上限；`Remaining` 读快照。
- **queue**：`PostMessage`、`ClaimMessage`（按创建时间取最早的 pending，设置 `lease_until` 并 `attempts++`）、`AckMessage`、`NackMessage`（超过 3 次转 failed）、`QueueDepth`。
- **sweep**：`Sweep` 把超 deadline 的非终态 work 标 `stalled`，把过期 claim 退回 pending（超过重试次数则 failed），把超过 24h 的 pending 标 `expired`；需要调用方自行定时触发，包内没有后台循环。
- **SQLite**：`NewSQLiteStore(path)` 打开/创建库、设置 `busy_timeout` 并建表；单连接串行化写入。`sqlitestore_test.go` 覆盖持久化、并发扣减、队列、renew 与 sweep；参考链路默认仍用 `MemStore`，生产 memory 未接入。

### Client

- `Dial` 建立 WS 连接；`WithClientMaxMessageBytes`、`WithClientWriteTimeout`、`WithOnEvent` 配选项。
- `Call` 发送 call 并等对应 reply；错误 reply 直接把 `*Error` 作为 error 返回。`CallDecode` 是「调用 + 解 body」的快捷方式。
- `Stream` 发送 call 并返回流式帧通道，收到 `end=true` 或普通 reply 后关闭；`Send` 用于单向帧；`Hello` 发送 hello 并取回 `Card`。
- `OnEvent` 接收没有挂起请求的 event/stream/reply；`Close` / `Done` 处理关闭。

### Server

- `NewServer(card, ...)` 先校验卡片；默认注册 `hello` / `health` / `metrics`，其余 method 用 `Handle` 注册。
- `Handler()` 返回挂在 `/spv2` 上的 HTTP handler；`Mux()` 返回完整 mux：`/spv2`、`/healthz`、`/readyz`、`/metrics`（OpenMetrics 文本）。
- 选项有 `WithMaxMessageBytes`（默认 4 MiB）、`WithActionTimeout`（默认 30s）、`WithMaxInflight`（默认 64）、`WithMetrics`、`WithIdempotency`。
- 收到 call 后做 `Normalize` / `Validate` / `ValidateTo`；未知 method 回 `not_found`，并发超限回 `resource_exhausted`，每个 call 在独立 goroutine 中执行。
- `Handler` 拿到的 `*Call` 提供 `Reply` / `ReplyError` / `SendEvent` / `SendStream` / `SendStreamError` / `Replied` / `WorkID`；handler 没回应时 server 会补错误 reply。
- `hello` 校验请求协议版本并返回服务卡；`health` 返回 status/version/uptime；`metrics` 返回当前快照。event 交给同名 handler 处理，不产生 reply。

### HTTP 观测

- `Server.Mux()` 一次挂好 `/spv2`、`/healthz`、`/readyz`、`/metrics`。
- `/healthz` 与 `/readyz` 都返回 JSON 的 `HealthResponse`；当前没有依赖探针，固定 `status=ok`（所以 `readyz` 还不具备“依赖不可用就不接流量”的语义）。
- `/metrics` 把 `Registry.Snapshot()` 渲染成 OpenMetrics 文本（counter/gauge/histogram，含 `_bucket` / `_sum` / `_count`）。

### 幂等

- `NewIdempotencyCache(ttl)` 创建缓存（默认 1h），用 `WithIdempotency` 挂到 server。
- 只有卡片声明 `idempotent:true` 且请求带 `idempotency_key` 的 unary call 才缓存；key 由 `from.kind` / `from.id` / `method` / key 组成。
- 命中时复用第一次的 body/status/error，重写 `id` / `reply_to` / `to` 后发回；stream 不缓存，缓存在进程内。


### 参考链路 ref/

- `ref` 是协议可运行性的参考，不是生产实现：`entry`、`reasoner`、`memory`、`tool` 四个服务都用 `spv2.Server`，服务间用真实 WS。
- `memory` 落在 `MemStore`，提供 `context` / `work_get` / `cancel` / `lease_charge` / `message_post` / `message_claim` / `message_ack`。
- `reasoner.message` 走固定流程：先校验 message（agent 必须是同 work 的 agent/participant，且 `authority=none`）→ `memory.context` → `tool.echo` → `entry.deliver`；不接 LLM，不做 planner。
- `entry.deliver` 在配置了 `MemoryURL` 时先 `work_get` 并调 `AuthorizeDeliver`；为空则跳过校验（参考实现的简化，生产入口不能省）。
- `ref` 目前仍保留 `runtimeRefFor` 桥：把 work.agent 的 `authority=none` 在本地提升为 `runtime` 再交给 `Authorize*`。核心现在已允许 agent 携带 runtime，后续可以删掉这个桥。
- `ref/ref_test.go` 覆盖真实 WS 链路、message 拒绝路径、entry deliver 授权、memory.cancel 与队列/lease。

### Metrics

- `Registry` 支持 `Counter`、`Gauge`、`AddGauge`、`Observe`、`Snapshot`；相同名字 + 标签聚合为一条序列，快照按名字和标签排序。
- 直方图默认桶为 0.005 / 0.01 / 0.05 / 0.1 / 0.5 / 1 / 5 秒。
- server 自动埋 `sump_requests_total`（method/code）、`sump_request_duration_seconds`（method）、`sump_inflight_requests`（method）；命名、单位和标签纪律由调用方负责，Registry 不强制。

## 最小示例

```go
srv, err := spv2.NewServer(card)
if err != nil {
    return err
}
srv.Handle("echo", func(ctx context.Context, call *spv2.Call) error {
    var in struct{ Text string }
    if err := call.Frame.DecodeBody(&in); err != nil {
        return spv2.ErrCode(spv2.CodeInvalidArgument, "body 无效", false, nil)
    }
    return call.Reply(map[string]string{"text": in.Text})
})
http.ListenAndServe(":9000", srv.Mux())

client, err := spv2.Dial(ctx, "ws://127.0.0.1:9000/spv2")
if err != nil {
    return err
}
defer client.Close()
call, err := spv2.NewCall("w1", spv2.HumanRef("portal"), spv2.ServiceRef("echo"), "echo", map[string]string{"text": "hi"})
if err != nil {
    return err
}
var out map[string]string
if err := client.CallDecode(ctx, call, &out); err != nil {
    return err
}
```

## 实现状态与边界

阶段、范围、文件地图与待办以 [IMPLEMENTATION.md](IMPLEMENTATION.md) 为准；README 只记录当前代码事实。

- 已落地：帧/卡片/校验、call/reply/event/stream、WS client/server、hello/health/metrics、HTTP `/healthz` `/readyz` `/metrics`、`MemStore`（work/lease/queue/sweep/renew）、可选幂等缓存、`ref/` 参考链路。
- SQLite：`sqlitestore.go` 有 `SQLiteStore` / `NewSQLiteStore` 和 `sqlitestore_test.go`，测试已通过；但参考链路与默认测试仍用 `MemStore`，生产 memory 尚未接入，不把参考实现当成迁移完成。
- 未接入：auth/tenant/capability、spawn/cross-owner；`cancel` 只在 `ref/memory` 有 handler，`renew` 只有 Store 方法，生产 method 未接。
- 测试缺口：没有 reconnect/heartbeat、stream 缺口/重复、专门的并发压测；`go test -race` 已通过。
- 迁移顺序：frontend → reasoner → memory → tools；v3 触发条件见 [spec §19](../../docs/concepts/spv2.md#19-实现顺序与-v3-触发)。

## 运行与测试

```powershell
cd src/spv2
gofmt -l .            # 应无输出
go test ./... -count=1
```

测试全部在本机内存/httptest 和 SQLite 临时库范围内跑，不依赖网络或外部服务。

## 协议与计划链接

- 协议：[`docs/concepts/spv2.md`](../../docs/concepts/spv2.md)
- 实现阶段/待办：[`src/spv2/IMPLEMENTATION.md`](IMPLEMENTATION.md)
