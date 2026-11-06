# 交接包：删除全部 v1 兼容层，全面转 v2

> 给接手的编码代理：这是一次**破坏性大改**，不是小修补。
> 先读完整包，再动手；不要合并、不要推 main、不要开 PR，除非用户明确说可以。

## 0. 一句话

把 SUMP 从“v1 为主 + v2 兼容双栈”改成 **v2-only**：运行时不再有 v1 `/ws`、v1 信封、v1 fallback、v1 adapter，也不再保留 `SUMP_SPV2` 开关。

## 1. 工作区与基线

- 仓库：`D:\agent\SUMP\2`
- 分支：`feat/v2-only`
- HEAD：`8dacc8a refactor: 删除 v1 适配器`
- 父提交：`b5739d6`（`feat/spv2.1-final`，已通过 #94 CI）
- 已删：`src/spv2/adapter/`（v1↔v2 适配器）
- 工作区：干净；该分支**未推送**，没有对应 PR
- 注意：`feat/spv2.1-final` / PR #94 是另一条线；不要在它上面继续删 v1，先在这个分支做

## 2. 目标状态（验收定义）

完成后应满足：

- 所有服务只暴露 `/spv2`；不存在 v1 `/ws` 业务端点。
- 运行时不再出现 `protocol.Envelope` / `protocol.JumpPayload` / `protocol.TypeJump` / v1 `protocol.Client`。
- 不存在 `SUMP_SPV2` 环境变量或 `Config.SPV2` 开关；没有 fallback。
- 不存在 `src/spv2/adapter`。
- 前端/embedding/settings-center 的 v1 client/endpoint 全部删除。
- 所有测试改为直接打 `/spv2`；v1 测试删除或重写，**不能靠 skip 变绿**。
- `run-local.ps1` 不再设置 `SUMP_SPV2`。
- 全仓库 `go test`、`go vet`、`GOWORK=off go build`、frontend、embedding 全绿。

## 3. 现状盘点（已确认）

| 区域 | 现状 |
|---|---|
| `src/service` | 707 行；v1 中心连接、`/ws`、`s.Handle` action 桥、`Call` v1 fallback、`spv2On` 开关 |
| 各服务 | 约 64 处 `s.Handle(...)` v1 注册；native `HandleNative` 已并存 |
| `settings-center` | 同时有 v1 `/ws` serverConn 和 native `/spv2` |
| frontend | v1 `CenterLink`/`attachJumpEndpoint` 与 native `SPv2Session`/`createSPv2WSS` 并存 |
| embedding | v1 `JumpEndpoint`/`CenterLink` 与 native `NativeEndpoint`/native CenterLink 并存 |
| `src/protocol` | v1 Envelope/Client/JumpPayload + 被业务使用的 payload 类型（如 `Provide`/`Timer*`/`RegisterPayload`） |
| 测试 | 大量 v1 `protocol.Client`/`serve` 测试 |

## 4. 推荐工作顺序（每一步可独立提交）

### P0 冻结与安全

1. 确认在 `feat/v2-only`，工作区干净。
2. 新建备份分支：`git branch backup/v2-only-before-removal`。
3. 不要 push、不要开 PR；每步小提交。

### P1 `src/service` 去 v1（最关键）

- 删除 `Config.SPV2`、`spv2On`、`SUMP_SPV2` 读取；服务启动即 native。
- 删除 `/ws` 的 mux 注册、`handleWS`、v1 `onCenterEvent`、v1 中心 `connectAndRegister`/`heartbeatLoop` 分支。
- 删除 `s.Handle` 与 `s.actions` 旧 action 表；只保留 `HandleNative`。
- 删除 `Service.Call` 的 v1 `callOnce` 与 fallback；只走 `callSPv2`。
- `report.go` 只保留 native report。
- `settings.go` 的 `actionConfigure` v1 wrapper 删除；保留原生 `configure`。
- `spv2_bridge.go` 删除 legacy `s.actions` 循环；只注册 `s.native`。
- 保留 `spv2_center.go`（native 控制面客户端）和 `spv2_configure.go`。
- 删除 `service/tests` 里 v1 中心/WS 测试，改成 native 测试。

### P2 逐个服务去 v1

顺序建议：timer → images → files → qq → shell → mcp → batch → approval → memory → reasoner。

每个服务：

1. 删除 `s.Handle("...", s.actionX)` 注册。
2. 删除 v1 `actionX(ctx, env, in)` wrapper；共享业务函数已存在（`sleep`/`save`/`recall` 等）。
3. 删除只被 wrapper 使用的 `protocol.Envelope`/`JumpPayload` import。
4. 保留 `Provides`（native card 仍用它生成方法描述）；但不再注册 v1 action。
5. 该服务的 v1 测试改为 `spv2.Client`/`/spv2`；删除纯 v1 兼容测试。
6. 跑该模块 `go test ./...`；此时不应再需要 `SUMP_SPV2`。

### P3 settings-center

- 删除 `server/server.go` 的 v1 `/ws`、`serverConn`、v1 `handleRegister/heartbeat/jump`、v1 `upgrade`。
- 保留 native `/spv2`、`native.go`、`push_native.go`。
- `hub.Conn` 的 v1 `Send(protocol.Envelope)` 若只服务 v1，删除；保留 `EventConn` 和 native conn。
- 删除 v1 测试（register/configure/offline 等），重写成 `/spv2` 控制面测试。

### P4 frontend

- 删除 `attachJumpEndpoint`、`dialOnce`、v1 `CenterLink` 分支、v1 `onMessage`、v1 `registerV1`、v1 `heartbeat`。
- 只保留 `SPv2Session` / `dialSPv2Once` / `createSPv2WSS` / native `CenterLink`。
- 删除 `stub-center.ts`（v1）或改成 native；保留 `stub-native-center.ts`。
- 测试全部改成 native；删掉 v1 回退用例。

### P5 embedding

- 删除 `sump_embedding/server.py` 的 `JumpEndpoint`（v1）。
- 删除 `CenterLink` 的 v1 `_connect_and_register_v1`/`register_envelope`/`heartbeat_envelope`/v1 读循环；只保留 native。
- `service.py` 的 `handle_conn` 只接受 `/spv2`；其它路径 404。
- 删除 v1 测试；保留 native + mutation gap 测试。

### P6 `src/protocol` 的 v1 transport

**先分清两类东西：**

- v1 transport：`Envelope`、`Client`、`Dial*`、`TypeJump`、`JumpPayload`、v1 server 相关 → 删除。
- 业务 DTO：`Provide`、`ServiceCard`、`RegisterPayload`、`Timer*`、`Image*`、`Settings*` 等仍被 native handler 使用 → **不能直接删**。

建议：

1. 把仍被使用的 payload/DTO 移到 `src/spv2/dto`（或保留 `src/protocol/payloads.go` 但只留数据定义，删掉 client/envelope）。
2. 更新所有 import。
3. 删除 v1 client/envelope 测试。
4. 删除 `src/protocol` 的 v1 transport 文件，只保留 DTO 或整体迁移后删除空包。

### P7 文档/启动

- `run-local.ps1` 删除 `$env:SUMP_SPV2='1'` 和注释。
- 更新 `README`/`IMPLEMENTATION`/`AGENTS` 中“v1 兼容边缘”表述。
- 删除 `src/spv2/README` 的 v1 适配器章节（已删）。

## 5. 验收命令（必须全跑）

```powershell
# 1. 不允许残留 v1 transport / 开关 / adapter
rg -n "protocol\.Envelope|protocol\.JumpPayload|protocol\.TypeJump|SUMP_SPV2|spv2/adapter|Handle\(\"" src testing
# 允许的例外必须逐条解释；不能有 /ws 业务路由
Select-String -Path src,testing -Pattern '"/ws"|/ws' -Recurse

# 2. 所有 Go 模块
$mods = @('src/protocol','src/service','src/settings-center','src/reasoner','src/memory','src/images','src/files','src/qq','src/shell','src/timer','src/batch','src/approval','src/mcp')
foreach ($m in $mods) { Push-Location $m; go test ./... -count=1; Pop-Location }

# 3. e2e
cd testing/tests; go test ./... -count=1 -timeout 600s

# 4. 静态与独立构建
gofmt -l (git ls-files '*.go')
foreach ($m in $mods) { Push-Location $m; go vet ./...; GOWORK=off go build ./...; Pop-Location }

# 5. frontend / embedding
cd src/frontend; npm run typecheck; npm test
cd src/embedding; python -m pytest tests -q
```

## 6. 硬约束

- 只在 `feat/v2-only` 上改；不碰 `main`、不 push、不开/合 PR，除非用户明确允许。
- **小步提交**，每个提交一个服务/一层；作者用：
  `XR <85773844+QAQ-awa-QAQ@users.noreply.github.com>`
  不要再用 `agent <agent@local>`。
- 不削弱测试：删除 v1 测试前先确认对应行为已有 native 测试；不能靠 skip/空断言变绿。
- 保留业务语义和 native method 名称/body；只删 transport/兼容层，不改业务规则。
- 不引入新依赖；不联网跑测试。
- 同一个问题修 5 次仍不过：保留现状，报告失败命令和错误，不要无限重试。

## 7. 已知陷阱

- `protocol.*Payload` 被 native handler 使用：删 `src/protocol` 前先迁移 DTO，否则大面积编译失败。
- `settings-center` 的 hub `Conn` 和 v1 `serverConn` 与 native `EventConn` 耦合，先拆接口再删实现。
- frontend 的 `attachJumpEndpoint` 同时承担 `/ws` 和 `/spv2` upgrade 路由；删 v1 时把 `/spv2` 路由和信任栅栏保留到 native 入口。
- embedding 的 `websockets.serve` 用 `handle_conn` 分流；删 v1 后 `/ws` 要明确 404，不要误落到 native。
- `GOWORK=off` 的 go.mod/go.sum 在 import 变化后要重新 tidy/build；CI 会查。
- 不要用 Squash 合并；用户要求保留小提交记录。

## 8. 交付

- 分支：`feat/v2-only`
- 每个服务/层的提交（作者 XR）
- `reports/v2-only-removal.md`：改了哪些文件、删除的 v1 面、验收命令与结果、剩余例外
- 不要合并；等用户 review。
