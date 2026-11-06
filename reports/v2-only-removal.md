# v2-only 迁移验收报告

> 目标：SUMP 从「v1 主链路 + v2 兼容」收敛为**只保留 SPv2.1**——所有服务只暴露 `/spv2`，
> 删除 v1 信封 / `jump` / `protocol.Client` / `SUMP_SPV2` 开关与回退，全部测试直接打 `/spv2`。
> 分支：`feat/v2-only`（本地，未 push / 未开 PR / 未合 main）｜完成日期：2026-11-06

## 1. 结论

- v1 传输面已清零：`src/`、`testing/`、`scripts/` 中无 `protocol.Envelope`、`protocol.JumpPayload`、
  `protocol.TypeJump`、`protocol.TypeRegister/TypeResponse/TypeHeartbeat`、`protocol.ResponsePayload`、
  `protocol.NewEnvelope/NewResponse/Dial/Client`、`SUMP_SPV2`、`src/spv2/adapter`、业务 `/ws`（OneBot 除外）。
- 全部 Go 模块、跨服务 e2e、frontend、embedding 测试通过；三套异变测试 0 存活。
- 迁移提交自 `8dacc8a` 起算（含 1 个合并提交），全部作者 `XR <85773844+QAQ-awa-QAQ@users.noreply.github.com>`，
  每个提交（含合并提交）都带 `Co-authored-by: qaq_awa_qaq <2271917353@qq.com>`。

## 2. 各层改动

| 层 | 改动 |
| --- | --- |
| `src/service` | 骨架只挂 `/spv2`；`spv2Addr` 不再把 `/ws` 改写成 `/spv2`；测试走原生 call/event |
| `src/settings-center` | 控制面只保留原生 `register`（call）/`heartbeat`（event）/`roster`/`settings`/`ping`/设置方法；删除 v1 分派 |
| 8 个业务服务 | timer、images、files、qq、shell、mcp、batch、approval 删除 `s.Handle("动作")` 与 v1 包装，只保留原生 method；业务函数与语义不变 |
| `src/reasoner` | 9 个 v1 入口删除；boss 改为帧头 `from`；删除 v2 下不可达的「向入口补 [交付失败]」路径 |
| `src/memory` | 业务层测试用本地 `callResult` 转述原生 call，不再依赖 v1 `ResponsePayload` |
| `src/protocol` | 删除 `Envelope`、`Client`、`Dial`、`Marshal/Unmarshal`、`TypeRegister/Response/Jump/Heartbeat`、`ResponsePayload`、`JumpPayload`、`ProtocolVersion` 与 v1 夹具测试；保留业务 DTO、`NewID`、`EncodePayload`/`DecodeRaw`（`codec.go`）、事件 method 常量；`go.mod` 去掉 gorilla/websocket |
| `src/spv2` | `Mux()` / `HTTPToWS` 统一挂 `/spv2`；README 删除适配器描述 |
| `src/frontend` | 中心链路只走原生 register call / heartbeat event / roster·settings event；删除 v1 信封、`SUMP_SPV2`、`/ws` 入站；测试桩改真 `/spv2` |
| `src/embedding` | 删除 `server.py`（v1 JumpEndpoint）与 `test_endpoint.py`；只放行 `/spv2`，其余路径 404；`protocol.py` 只剩 `new_id`，夹具改为 Frame body |
| `scripts` | `run-local.ps1` 删除 `SUMP_SPV2`，12 处 `-center` 改 `/spv2`；命令审批脚本同步 |
| 跨服务测试 | `testing/tests` 13 个既有文件 + 原生辅助文件、`testing/approval-integration`、`testing/evals` 的观察者/boss/门户/设置替身全部改原生帧 |
| 文档 | README、AGENTS、protocol/architecture/approval/agent-loop、add-a-service、mcp-connect、各服务 README 改为 SPv2.1；历史 handoff / archive 不动 |

## 3. 语义变化（必须明说的部分）

1. **`boss` 字段消失**：SPv2.1 的结果归属方 = 帧头 `from`。入口服务把自己写进 `from`；
   qq / frontend / approval / timer / batch 均按此迁移。evals 的假入口从 `from=eval,boss=eval-boss`
   改为 `from=eval-boss`。
2. **删除 `TestDeliveryFailureNoticeE2E`**：它验证的是 v1「boss 指错、失败补报回入口」；
   v2 中 entry 恒等于 boss，该分支不可达。同时删掉 reasoner 的入口表与补报路径（否则是假承诺）。
   交付失败仍按「送达后不重试」停住并记日志；失败路径由新增的原生用例
   `TestDeliverFailureKeepsTaskRetryableE2E` 覆盖（注入一次 deliver 错误 → 不记录成功交付 → 后续消息仍能交付）。
3. **业务 DTO 中的 `boss` 字段保留**（`TaskContext.Boss`、`StorePayload.Boss`、`TimerStatePayload.Boss`）：
   它们是载荷内的业务概念（会话入口 / 计时归属），不是帧头字段；改名会破坏已有数据与设置。
4. **OneBot `/ws` 保留**：qq 测试桩的 `/ws` 是 NapCat/OneBot 自己的协议，不是 SUMP 端点。

## 4. 验收矩阵（全部实跑）

| 项目 | 命令 | 结果 |
| --- | --- | --- |
| gofmt | `gofmt -l $(git ls-files '*.go')` | 无输出 |
| Go 模块（14 个 src） | `GOWORK=off go build/vet/test ./...` | 全绿 |
| approval-integration | `GOWORK=off go build/vet/test ./...` | ok 4.5s |
| evals | `GOWORK=off go build/vet` + `go test ./...`（runner 需 workspace 编译子进程） | 全绿；runner 1.9s（构建缓存命中） |
| 跨服务 e2e | `cd testing/tests && go test ./... -count=1 -timeout 900s` | ok 93.8s（含门户真进程与交付失败用例，前端 dist 已构建） |
| frontend | `npm run typecheck` + `npm test` | node 86 pass / web 153 pass |
| embedding | `pytest tests -q` | 48 passed, 1 skipped（ONNX 冒烟需模型目录） |
| frontend 异变 | `node tests/mutation.mjs` | 31 个：killed 26 / timeout 5 / **lived 0** |
| embedding 异变 | `python tests/mutation.py` | 43 个：killed 43 / lived 0 / timeout 0 |
| evals 异变 | `go run ./scripts/mutation` | 25 个：killed 25 / lived 0 |

残余静态检查：

```text
grep -rn "protocol\.Envelope|protocol\.JumpPayload|protocol\.TypeJump|SUMP_SPV2" --include=*.go --include=*.ts --include=*.py src testing scripts
# 无输出
```

## 5. 验收例外（逐条）

任务书的验收正则命中以下位置，均为有意保留：

1. `Handle("`：全部是 `*spv2.Server.Handle`（原生 method 注册表）或 `mux.Handle("/spv2")`，
   不是 v1 `service.Service.Handle` 动作桥（该 API 已删除）。代表位置：
   `src/settings-center/server/native.go`、`src/spv2/server.go`、`src/spv2/ref/*`、
   `testing/evals/runner/stack.go` 的假 boss、各服务测试里的中心/对端桩。
2. `/ws`：
   - qq 测试桩的 NapCat/OneBot `/ws`——外部协议，不是 SUMP 端点；
   - embedding 对 `/ws` 返回 404 的断言与 docstring——负向测试；
   - frontend / embedding 异变脚本里“把 `/spv2` 改成 `/ws`”的变异体——测漂移的输入；
   - embedding 冻结 Go 夹具里的 `addr` 值——历史字节，不是路由；
   - 文档里“不存在 `/ws` 业务端点”的否定表述。
3. 业务载荷字段名 `boss`：仍存在于 `TaskContext` / `StorePayload` / `TimerStatePayload`，
   表示会话入口 / 计时归属，不是帧头字段；SPv2 帧没有 `boss`。
4. `docs/archive/` 与历史 handoff 保留旧协议描述：历史记录，不作为现行契约。
5. `testing/tests` 的门户用例在缺少前端 `dist/` 时跳过（既有环境跳过）；本次验收先构建，
   实际执行；embedding 的 ONNX 冒烟同理（无模型目录）。

## 5. 已知边界与未做

- frontend 异变有 5 个 timeout（TS16/22/23/24/25）：变异体会让 reply/event 路径挂起，
  测试以超时暴露，而非断言失败；没有存活变异。
- `testing/evals/runner` 的 `TestStackRoundTrip` 会在子进程里 `go build` 服务，
  因此**测试**需要 go.work；`GOWORK=off` 下模块本身 `build`/`vet` 通过。这是测试脚手架的既有约束。
- 门户 e2e 在缺少 `src/frontend/dist/server` 时跳过（既有行为）；本次验收先 `npm run build`，因此实跑。
- `dist/` 构建产物、`third_party/` 与历史 handoff 文档未纳入改动；`reports/` 只新增本文件。
- 未 push、未开 PR、未合 main。
