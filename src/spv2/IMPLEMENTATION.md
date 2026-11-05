# SPv2.1 实现计划与状态

> 读者：要接手 SPv2.1 实现的人。
> 状态：协议核心 + 参考链路 + SQLite WorkStore 已实现；生产服务迁移未开始。
> 规格：[docs/concepts/spv2.md](../../docs/concepts/spv2.md)。

## 1. 范围

本模块是 SPv2.1 的参考实现，证明协议可运行、可测试：

- 帧编解码/校验、Ref、Card、Method、Work、Lease、Error、Metrics；
- WS client/server：call / reply / event / stream；
- hello / health / metrics；HTTP /healthz / readyz / metrics；
- WorkStore：内存实现 + SQLite 实现（work/lease/queue/sweep/renew）；
- 幂等缓存、authority 辅助、content_type 解码、错误终态 stream；
- 参考链路：entry → reasoner → memory.context → tool → deliver；
- v1 → SPv2.1 适配器。

不包含：生产 reasoner/memory/frontend 的替换、认证/tenant/capability、spawn/跨 owner、企业多实例。它们按 spec §18 标 not designed。

## 2. 文件地图

| 文件 | 作用 |
|---|---|
| frame.go | Frame/Ref/Stream/Error 与构造函数 |
| card.go | 服务卡、method、limits、observability |
| work.go | Work、Lease、状态机、默认租约 |
| validate.go | 帧校验、authority、目标校验 |
| authority.go | deliver/control/message 的角色授权 |
| codec.go | msgpack/JSON/text/binary body 编解码 |
| metrics.go | counter/gauge/histogram 注册表 |
| client.go / server.go | WS 传输与 call/stream/event |
| http.go | /ws /healthz /readyz /metrics |
| idempotency.go | 可选的 unary reply 幂等缓存 |
| store.go | WorkStore 接口 + MemStore |
| sqlitestore.go | WorkStore 的 SQLite 实现 |
| adapter/v1.go | v1 jump/response ↔ SPv2 call/reply |
| ref/ | 参考链路与 e2e |
| *_test.go | 契约/传输/存储/参考链路测试 |

## 3. 阶段

### P0 核心（已完成）

- 帧/Ref/Card/Work/Lease/Error/Metrics；
- MemStore / SQLiteStore：work、lease、queue、sweep、renew；
- Client/Server：call/reply/event/stream、hello/health/metrics；
- HTTP 观测端点；幂等缓存；content_type；错误终态 stream；
- 基础测试：frame/card/validate/store/http/metrics/idempotency/transport。

### P1 参考链路（已完成）

- memory：context / work_get / lease_charge / message_* / cancel / renew；
- reasoner：message → context → tool → deliver（runtime authority）；
- entry：deliver，先查 memory work 行再 AuthorizeDeliver；
- tool：echo；
- e2e：真实 WS，不接 LLM。

### P2 适配（已完成）

- v1 adapter：jump/response ↔ call/reply；
- event/stream 明确不支持，不静默降级。

### P3 生产接入（未开始）

- memory：用 SQLiteStore 落 works/work_participants/work_messages；
- reasoner：用 message 替换 user_message，用 context 替换 recall/resume；
- frontend/qq：用 SPv2 client，保留 deliver；
- tools：method 卡 + schema，去掉中央动作表；
- settings-center：卡片/名册/版本协商。

## 4. 验证

在 src/spv2 下运行：go test ./... -count=1；go test -race ./... -count=1；gofmt -l .；go vet ./...。

生产迁移后追加：一条链 e2e、v1 适配器对照、最小指标集契约、红队测试。

## 5. 已知未做

- 生产 reasoner/memory/frontend 迁移；
- auth/tenant/capability；
- spawn/cross-owner；
- stream credit window；
- schema codegen；
- 参考链路的入口 allowlist（ref 只做 authority 形状校验）。

## 6. v3 触发

与 spec §19 一致：改核心 8 个必填字段、7 个核心 method 语义、work/lease/authority 语义、安全模型，或 profile 必须进核心。否则只做加法。
