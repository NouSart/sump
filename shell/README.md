# shell：命令执行服务

代客执行命令的“代理商”：谁下单执行谁，自己不设判断（判断外包给审批），**受理即回执**，结果另行补报。

```text
run 受理（立即回执 {item_id, state:"queued"}）
  → 审批（接缝；未通过 → 补报 rejected，不执行）
  → 执行（并行上限 + 待执行队列上限；满即拒绝，不排队）
  → 补报 report_result（approved → done / failed）
```

端口默认 `:9701`；模块 `shell/`，入口 `shell/main.go`。

## 边界（护栏都在这一个服务里）

| 护栏 | 行为 | 设置键 |
| ---- | ---- | ---- |
| 工作目录 | 必须在**根目录内**（越界、不存在、不可访问 → 同步拒绝；软链先解析再判边界） | `exec.root` |
| 解释器 | 默认自动探测 `bash` → `pwsh` → `powershell` → `cmd`（Unix：`bash` → `sh`） | `exec.shell` |
| 执行超时 | 超时终止**整棵进程树**（Windows：`taskkill /T /F`；Unix：杀进程组），补报 `timed_out` + 退出码 `-1` | `exec.timeout_ms` |
| 输出上限 | 单流保前 N 字节，附“共多少字节”，**命令照常跑完**（不因截断中断） | `exec.max_output_kb` |
| 并行 | 同时执行的命令数上限 | `exec.concurrency` |
| 队列 | 已通过审批、等待执行的条数上限；满 → 立即拒绝（不排队等待） | `exec.queue_limit` |
| 审批 | 见下节；未接入 = 一律未通过 | `approval.service` / `approval.timeout_ms` |

**同步拒绝 vs 补报拒绝**：请求本身不合法（命令为空、工作目录越界）→ 同步 `{ok:false}`；
由不可控因素决定的（审批未通过、队列满、超时、shell 缺失）→ 受理后补报。

## 审批接缝（等审批服务定协议）

审批是 shell 自己的活儿，但“谁裁、按什么协议裁”由审批服务定义。代码里只剩**一个入口 + 一个出口**：

- 入口：[`shell/server/approval.go`](./server/approval.go) 的 `Approver` 接口——执行前调用，`nil` = 通过；
- 出口：未通过的原因**原文**进补报的 `error` 与 `text`。

当前实现是 `FailClosed`：**未接入审批就一律不执行**（`approval.service` 只接受空值，填别的会被设置校验拒绝并说明原因）。
审批协议落地后：实现 `Approver`（或经 `Config.Approver` 注入），其余代码不动。

## 补报时序（`report_result`）

| phase | 含义 | final |
| ---- | ---- | ---- |
| `approved` | 审批通过，已开始执行 / 已排队（文本里写明） | 否 |
| `rejected` | 未执行（审批未通过 / 队列已满） | 是 |
| `done` | 执行结束（含超时终止：看 `timed_out` / `exit_code`） | 是 |
| `failed` | 没能跑起来 / 没能跑完（如解释器缺失、进程启动失败） | 是 |

补报发往**发起跳转时的 `from`**（调用方）；调用方离线 = 发不出去，只记日志（不排队补发）。
`data` 字段带结构化结果（`command` / `exit_code` / `stdout` / `stderr` / `stdout_bytes` / `stderr_bytes` / `timed_out` / `truncated` / `duration_ms`）。

## 设置项一览

| 键 | 预设 | 说明 |
| ---- | ---- | ---- |
| `exec.root` | 进程当前目录（`run-local.ps1` 传仓库根） | 工作目录根 |
| `exec.shell` | 空（自动探测） | 指定解释器（需在 PATH 上可解析） |
| `exec.timeout_ms` | `300000` | 单条命令执行超时（≥1000） |
| `exec.max_output_kb` | `256` | 单流输出上限（KB，≥1） |
| `exec.concurrency` | `8` | 并行执行上限（≥1） |
| `exec.queue_limit` | `32` | 待执行队列上限（≥1） |
| `approval.service` | 空 | 审批服务名；**当前只接受空值**（协议未定） |
| `approval.timeout_ms` | `60000` | 等审批超时（≥1000；超时按未通过处理） |

## 测试

```powershell
cd shell; go test ./... -count=1
```

- 服务内（`shell/tests/`）：最小中心替身 + 真 shell 实例；**命令用真实子进程执行**——
  覆盖受理回执、fail-closed 拒绝（且确认真的没执行）、审批通过后的 `approved`/`done`、
  工作目录边界、队列满即拒、超时终止、输出截断、设置项校验与“改完即生效”。
- 根 `tests/shell_e2e_test.go`：真进程跨服务——名册里的 `async` 声明、受理回执不等执行、
  补报回到调用方、越界同步拒绝。
- 说明：fail-closed 下真进程 e2e 不会执行任何命令（这是有意的）；执行路径由服务内测试（注入审批实现）覆盖。

## 不做（本批）

后台任务托管（start / status / logs / kill）、输出有界引用（大输出留句柄按需取）、文件读写工具、
命令白名单策略、用户审批 UI——前两项见 [DESIGN.md](../DESIGN.md) 的后续批次；策略与 UI 归审批服务。
