# 已集成的命令处理与审批 v1

2026-10-16。本轮总第 2 阶段／2.1：第一层实现与联调。

本改动加入独立的 `approval` 服务，并接通已有 shell 和 reasoner 的命令处理反馈。

## 最快运行方式：三个服务的小循环

先安装 Go 1.26+ 与 PowerShell，在仓库根目录运行 `build.ps1` 构建 `bin` 中的程序。最小链路无需模型密钥、QQ 或前端依赖。构建完成后，在仓库根目录打开 PowerShell：

```powershell
.\start-command-approval.ps1
```

出现 `READY: ws://127.0.0.1:19000/ws` 后保持窗口开启，在第二个 PowerShell 窗口进入本目录：

```powershell
.\submit-command.ps1 -CommandText "Write-Output hello"
.\submit-command.ps1 -CommandText "Get-Location"
.\submit-command.ps1 -CommandText "Remove-Item demo.txt"
.\submit-command.ps1 -CommandText 'Write-Output $env:PATH'
```

第一、二条通过后真实执行；第三条拒绝；第四条因动态展开进入 review，本次不执行。目录、原因、阶段和执行输出会显示在提交窗口。

启动器只运行设置中心（19000）、approval（19801）、shell（19701）。工作目录为 `command-work/`，设置和日志在 `command-runtime/`，停止按 Ctrl+C。三个端口均可通过启动参数修改；若改中心端口，提交时同步指定 `-Center ws://127.0.0.1:新端口/ws`。这个观察客户端不模拟模型思考，是真实工具链的命令行入口。

停止启动器会停止三个服务；沿用原 shell 的退出语义，已经启动的命令子进程不保证同时终止。因此维护者扩展为长时间操作后，需要另外处理运行任务的停止与恢复。

若系统执行策略阻止 `.ps1`，可在此次启动进程使用 `powershell -NoProfile -ExecutionPolicy Bypass -File .\start-command-approval.ps1`；这不会修改系统执行策略。提交也可直接使用 `bin/sump-approval.exe send -center ws://127.0.0.1:19000/ws -command "Get-Location"`。

只分析、不执行则用：

```powershell
.\inspect-command.ps1 -CommandText "Write-Output hello"
```

## 接到原有完整 SUMP

`scripts/run-local.ps1` 已加入 approval 的构建和启动，shell 显式使用同一 PowerShell、设置 `approval.service=approval`。按原项目 README 配置 Go、Node、Python、模型端点等环境后运行该脚本，沿原有入口使用 `shell.run` 即可。QQ 只是其中一种应用入口。

完整脚本仍保留上游的前端构建和 embedding 依赖安装逻辑。**本次实测范围是命令服务链和真实 reasoner + 假 LLM；未用真实模型密钥、QQ 登录或 embedding 模型执行整套业务。** 最小启动器已单独实测。

如果已有服务正在运行，可以单独启动 `bin/sump-approval.exe serve -center ...`，将 shell 的 `approval.service` 设置为 `approval`，并确保 `exec.shell` 与 approval 选中的 PowerShell 是同一文件。既有中心覆盖值优先于启动默认值；出现 `RUNTIME_MISMATCH` 时应统一这两个配置。

## 一条命令怎样走完

1. LLM 或客户端调用 `shell.run`；shell 校验目录和大小，立即返回 `item_id`。
2. shell 固定本次的命令、目录、解释器、参数、环境、超时和输出限额，再请求 `approval.approval_request`。
3. approval 校验上下文，用原生 AST 提取结构，按不可变规则快照返回一份裁决，附模型反馈。
4. shell 核验任务、摘要和有效期。仅 allow 可以进入执行队列；deny、review、服务失联或响应不完整都停止本次命令。
5. shell 发送 approved 的同时可执行命令；approved 的投递尝试结束后再发送 done/failed；拒绝则发送 final rejected。补报含结构化裁决，以及 reasoner 可直接读到的原因、命中规则与未知结构。
6. 原 reasoner 把补报加入当前模型上下文，或在任务收尾后启动补报轮；用户输出沿原有 deliver 通道返回。

approval 不绕过工具再额外直发同一条模型反馈，因此不会因双通道重复触发模型。第二层“并行解释请求＋用户通知”尚未启用，接口位于 `approval/review/ports.go`。

## 本次加入／修改的内容

| 位置 | 变化 |
| --- | --- |
| `approval/` | 新增解析、规则、裁决、服务适配、CLI 和第二层接口 |
| `protocol/approval.go`、`execution_profile.go` | 新增消息契约、摘要、固定 PowerShell 环境约定 |
| `protocol/payloads.go` | shell 补报增加可选 `approval` 字段，旧字段保留 |
| `shell/server/remote_approval.go` | 接通网络审批、固定执行描述、校验响应和 TTL |
| `shell/server/server.go` | 允许设置审批服务；限制待审批数量；按顺序投递反馈；透传裁决 |
| `shell/server/exec.go` | 真实执行使用获准描述和环境快照 |
| `shell/server/approval.go`、`shell/main.go` | 更新接缝说明，保留原 Approver 注入接口和未接入时拒绝行为 |
| `shell/tests`、`protocol/*test.go` | 兼容性与执行绑定回归验证 |
| `approval-integration/` | 真实中心、工具、审批、reasoner 的离线联调，假 LLM 不调用 API |
| `go.work`、启动与构建脚本 | 注册新模块，提供最小和完整启动路径 |

reasoner、memory、QQ 等服务的业务源码没有改动。`approval-integration` 只在测试里导入多个服务实现，生产服务之间仍走 WS + MessagePack。

## 当前能力和边界

默认支持 PowerShell 简单命令：位置、时间、当前目录列表、静态文本输出。复杂语法、未知命令及未覆盖参数返回 review。规则可以在 `approval/policy/default.json` 或启动 `-policy` 文件中维护，运行时也可下发 `approval.policy.json`。更改内嵌默认文件后需重新构建；已有中心覆盖值不会自动被新默认替换。

新增 allow 名称等于维护者显式信任该命令在限定参数形态下的行为。不能为了“少拦截”直接加入任意脚本解释器或任意程序；更广泛业务需要参数／行为规则和测试一起扩展。

这不是沙箱。工作目录约束限制 `work_dir`，不代表所有文件操作被关在目录内；摘要不冻结磁盘文件、环境外部状态，也不是签名。仅按原项目的可信本机／内网服务边界部署，未增加网络身份认证或传输大帧防护，不适合直接暴露公网。

30 秒许可从 shell 发请求前计时；排队过期就停止，需要新提交。运行中规则更新不追溯撤销已返回的短期许可。反馈和中心故障上报沿现有内存通道，没有落盘重放或恰好一次保证；网络不明时不重发执行。不要把“发送已受理”当作命令已执行成功。

## 构建和测试

```powershell
.\build.ps1 -GoExe go
.\test-approval.ps1 -GoExe go
```

需要 Go 1.26+ 和 PowerShell。测试包含对临时目录的无害执行；危险示例只分析或明确拒绝。源码结构和完整协议见 [协议说明](COMMAND-APPROVAL-PROTOCOL.md)，验证范围和环境见本 PR 描述；本机已通过所列模块测试与最小进程联调，GitHub CI 结果以 Checks 为准。
