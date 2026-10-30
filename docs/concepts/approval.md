# 命令审批

> **读者**：要改审批规则、接入 shell、或排查「命令为什么没执行」的人 ｜ **读完能做**：读懂一份裁决包、加一条规则、判断边界在哪 ｜ **状态**：第一层已实现，第二层已实现，解释与富通知未实现 ｜ **最后校验**：2026-10-30

公共类型的权威定义在 `protocol/approval.go`；服务实现在 [approval/](../../approval)。

## 1. 它解决什么

危险命令**默认拦住**，等用户点头才执行——系统不代管决定。两道闸：

1. **第一层（规则引擎）**：按方言提取结构（PowerShell 原生 AST / POSIX shell 保守词法器）+ 不可变规则快照 + 执行摘要绑定，给不出 `allow` 就交第二层。
2. **第二层（用户裁决）**：命令挂起并推到门户，用户点批准 / 拒绝。

**fail-closed**：shell 没接通审批时，任何命令都按「未通过」处理，不会执行。

## 2. 一条命令怎样走完

1. LLM 或客户端调用 `shell.run`；shell 校验目录和大小，立即返回 `item_id`。
2. shell 固定本次的命令、目录、解释器、参数、环境、超时和输出限额，再请求 `approval.approval_request`。
3. approval 校验上下文，按方言提取结构（PowerShell 原生 AST / POSIX shell 保守词法器），按不可变规则快照返回一份裁决，附模型反馈。
4. shell 核验任务、摘要和有效期。仅 `allow` 可以进入执行队列；`deny`、`review`、服务失联或响应不完整都停止本次命令。
5. shell 发送 `approved` 的同时可执行命令；`approved` 的投递尝试结束后再发送 `done` / `failed`；拒绝则发送 `rejected`。补报含结构化裁决，以及 reasoner 可直接读到的原因、命中规则与未知结构。
6. reasoner 把补报加入当前模型上下文，或在任务收尾后启动补报轮；用户输出沿原有 deliver 通道返回。

approval 不会绕过工具再额外直发一条模型反馈，因此不会因双通道重复触发模型。

## 3. 服务与输入

服务名默认 `approval`，动作 `approval_request`。使用 `jump` 信封，载荷 MessagePack；`trace`、`boss` 沿用，业务关联用 `item_id`。**approval 不接受请求指定任意程序充当解析器。**

```json
{
  "schema_version": 1,
  "item_id": "本次工具任务的唯一句柄",
  "plan": {
    "version": 1,
    "command": "Write-Output hello",
    "work_dir": "C:\\work",
    "shell_path": "C:\\Program Files\\PowerShell\\7\\pwsh.exe",
    "args": ["-NoLogo", "-NoProfile", "-NonInteractive", "-Command"],
    "dialect": "powershell",
    "platform": "windows",
    "profile": "powershell-clean-v1",
    "timeout_ms": 300000,
    "max_output_bytes": 262144
  }
}
```

- `args` 不包含最后的 command；工具把命令作为一个独立启动参数交给解释器，不能重新拼出不同命令。
- 以下上下文校验针对 PowerShell；bash 请求改为校验 POSIX shell 形状、固定 `-c` 参数、工作目录与路径边界，不能套用 PowerShell 的环境约定。
- 执行路径必须与审批进程选定的可信 PowerShell 路径一致（Windows 大小写不敏感）。
- 工作目录由 shell 解析符号链接并验证是否在根目录内。
- **固定环境**：清除继承的 `PSMODULE*`、`POWERSHELL_*`、`PSEXECUTIONPOLICYPREFERENCE`，模块路径限定为解释器安装目录下 `Modules`，不加载用户 profile。其余环境在送审前由 shell 快照；不支持动态环境展开，所以不依赖变量取值做允许判断。这个约定依赖可信执行方和可信安装目录，**不是远程沙箱证明**。

## 4. 裁决包

成功处理时 `ResponsePayload.ok=true`，`data` 包含：

| 字段 | 含义 |
| --- | --- |
| `schema_version` | 固定 1 |
| `item_id` / `decision_id` | 原任务和本次裁决的关联标识 |
| `decision` | `allow` / `deny` / `review` 之一；**唯一裁决真值** |
| `plan_digest` | 绑定执行描述的 SHA-256 十六进制摘要 |
| `policy_version` | 配置版本字符串 + 原始配置内容哈希前 8 字节的十六进制 |
| `reason_code` | 稳定原因码 |
| `valid_for_ms` | `allow` 为 30000；其他结果为 0 |
| `llm_feedback.summary` | 给模型的原因说明 |
| `llm_feedback.rule_ids` | 可选，命中的允许规则 ID |
| `llm_feedback.unknowns` | 可选，未覆盖的结构 / 动态语义或解析错误 ID |

原因码：`RULE_ALLOW`、`POLICY_DENY`、`DYNAMIC_BEHAVIOR`、`NO_MATCHING_RULE`、`PARSE_ERROR`、`UNSUPPORTED_DIALECT`、`CONTEXT_UNKNOWN`、`RUNTIME_MISMATCH`、`LIMIT_EXCEEDED`；第二层另有 `USER_APPROVED` / `USER_DENIED`。

无效请求、内部解析故障、繁忙使用 `ok=false`。**deny / review 是正常业务结果，不算服务故障**。shell 只把经过 `ValidateFor` 的 `allow` 映射为执行许可。

**摘要编码**：依次写入 UTF-8 字符串，每项前写 uint64 大端字节长度——固定域 `sump-execution-plan-v1`、version 十进制、command、work_dir、shell_path、dialect、platform、profile、timeout_ms 十进制、max_output_bytes 十进制、参数个数十进制、每个 args 项。跨语言实现必须保持这个顺序，**不能对 JSON 文本直接哈希**。

## 5. 反馈路径

```text
终端工具 ──审批请求──> approval
终端工具 <─一份裁决包─ approval
终端工具 ──report_result──> reasoner ──模型上下文──> LLM
```

- `ShellEventData.approval` 保存完整裁决包；`AsyncResultPayload.text` 含可读原因、规则和未知结构（reasoner 读的是 `text`）。
- 先报 `approved`（非终态），结束报 `done` / `failed`（终态）；执行可与 `approved` 投递并行，执行槽在命令结束后释放，终态反馈等待 `approved` 投递尝试结束；拒绝 / review 报 `rejected`（终态）。
- 原始命令保留在工具反馈中；**approval 自身故障上报不含原始命令**。
- 工具在后台顺序投递，每次最多等 3 秒。只有明确 `status=ignored` 才按 50 / 100 / 200 / 400ms 退避重试（解决受理回执与补报的先后竞争）；网络错误不确定对端是否已接收，不盲目重发。超过期限上报 `shell_feedback`，执行流程仍按已有裁决继续，**不声称反馈已送达**。
- 调用 `shell.run` 两次会创建两个任务，**没有跨请求幂等键**；调用方遇到网络错误不得自动重提交有副作用的命令。

## 6. 限额与失败处理

- 单条命令 16 KiB；审批与 shell handler 在解码前限制业务载荷 64 KiB。
- PowerShell AST 节点或 bash 简单命令词元超过 4096、出现未知结构，不允许；bash 组合命令另受 16 KiB 命令长度上限约束。PowerShell 解析子进程 5 秒超时、输出最多 256 KiB；bash 词法器在进程内同步完成。
- approval 同时处理 8 条（`1–64` 可配）；满时返回 busy，**不建无限等待队列**。
- shell 待审批 / 投递进展的后台任务最多 64；执行并发与队列默认 8 / 32。
- 上述是业务层限额；公共传输层未增加 WebSocket ReadLimit 或身份认证。
- `allow` 的 30 秒**从工具请求开始计时**；执行前再次验证摘要、命令、目录和有效期。
- 配置按请求读取一次不可变快照；无效新配置不替换旧快照。**规则更新不撤销已返回但尚未过期的许可**。
- 解析器异常、内部 panic 通过服务骨架 `Report` 上报中心；取消的请求不误报。

## 7. 规则与方言扩展

默认规则在 `approval/policy/default.json`：规则包定义版本、禁止命令名、允许命令名与静态参数数量上限；**禁止规则优先**。规则可以在 `approval/policy/default.json` 或启动 `-policy` 指定的文件里维护，运行时也可下发 `approval.policy.json`；**改内嵌默认文件后必须重新构建**，而中心已有的覆盖值不会自动被新默认替换。别名只接受显式列入配置的名称。命令名不是自然语言敏感词，不翻译用户内容，也不按语言封禁字面参数。

首版只批准完整结构中的**单一静态命令**（PowerShell AST 与 bash 词元都必须全是字面量）。对参数带路径、写文件、网络访问、外部程序等业务，应增加相应事实提取与专门参数规则，而不是简单扩大命令名前缀允许范围。

- **新增 shell 方言（仍属 exec 域）**：实现 Parser，并同时定义执行 profile、路径身份和正反例测试；不能只改 `dialect` 字符串就套用 PowerShell 结果。当前已支持 `powershell` 与 `bash`；`bash` 方言族覆盖 bash / sh / dash / zsh / ksh（shell 传解释器名），只读白名单与边界见 [shell/README.md](../../shell/README.md)。
- **新增执行域（如 fs / http / 后台任务）**：先定义独立域判别字段（建议 `kind`），不让 `dialect` 兼任域名。各域声明自己的执行字段组、上下文校验、事实提取器和规则段；事实提取器注册在 approval 侧，**不能接受调用方自报「安全」结论**。裁决包、摘要绑定、有效期和 fail-closed 原则共用。当前 `plan` 的字段全部属于 exec 域；域判别字段只约定了扩展方向，尚未发送。

## 8. 第二层：用户裁决

第一层给不出 `allow` 时，命令**挂起**并推到门户，等用户点批准 / 拒绝。

| 方向 | 动作 | 载荷 |
| --- | --- | --- |
| approval → 门户 | `approval_pending` | `{item_id, command, work_dir, requested_by, reason_code, summary, unknowns[], rule_ids[], created_ms, expires_ms}` |
| 门户 → approval | `approval_decide` | `{item_id, decision: allow\|deny, note?}` |
| 门户 → approval | `approval_list` | `{}` → `{items: [...]}`（门户重启后拉回待审列表） |

- **只接受门户裁决**：`approval_decide` / `approval_list` 的 `from` 必须是 `-portal` 指定的服务名（默认 `frontend`），别的服务调一律拒。
- **挂起的是这条命令，不是审批服务**：每条 jump 独立 goroutine；并发槽只在解析 / 裁决期间占用，等待用户时释放。
- **TTL**（`-review-ttl`，默认 10 分钟）：到期按 `review` 原样返回（不执行），待审项消失。
- **门户不在名册**：不挂起，立即按 `review` 返回（不浪费调用方 10 分钟等一个不会来的裁决）。
- **队列满**（`-max-pending`，默认 32）：按 `review` 返回，不排队。
- 用户批准 → `decision=allow`、`reason_code=USER_APPROVED`、`valid_for_ms=30000`；拒绝 → `deny` + `USER_DENIED`。
- **许可期从「拿到裁决」起算**（shell 侧）：`valid_for_ms` 仍是 30 秒，但计时起点是收到裁决的时刻——否则用户审 5 分钟、许可早过期，等于白审。
- 门户重启后用 `approval_list` 恢复列表；approval 侧待审项只在内存里，重启即清空（挂起随之失效，调用方按未通过补报）。

**与「零阻塞」的关系**：阻塞被限制在**调用方自己那条链**——shell 的这条 job 在等裁决（受 `approval.timeout_ms` 限制，默认 10 分钟），审批服务、其它服务、其它命令都不受影响。门户慢或挂掉也不阻塞（推送是 Fire，不等回执）。

## 9. 边界（说清楚，不假装）

- **这不是沙箱**：工作目录约束限制 `work_dir`，不代表所有文件操作被关在目录内；摘要不冻结磁盘文件、环境外部状态，也不是签名。
- 仅按「可信本机 / 内网服务边界」部署；**未增加网络身份认证或传输大帧防护**，不适合直接暴露公网。
- 30 秒许可从 shell 发请求前计时；排队过期就停止，需要新提交。
- 反馈和中心故障上报沿现有内存通道，**没有落盘重放或恰好一次保证**；网络不明时不重发执行。
- 不要把「发送已受理」当作命令已执行成功。

## 10. 未实现

`approval/review/ports.go` 保留四个接口（解释与通知）：

1. `ExplainPort.RequestExplanation`：请求模型解释命令。
2. `UserNoticePort.NotifyPending`：初次通知用户服务。
3. `ExplanationReceiver.ReceiveExplanation`：接收模型回传。
4. `UserUpdatePort.UpdateExplanation`：转发解释并更新用户界面。

Context 携带原请求、裁决、trace、boss；Explanation 携带 item_id、执行摘要、目的、影响、未知项。它们目前没有生产实现或网络路由。**解释结果不自动成为执行许可**，LLM 或用户超时也不能隐式放行。

## 11. 运行与测试

```powershell
# 主线：run-local.ps1 会一并拉起 approval（127.0.0.1:9901）
.\scripts\run-local.ps1

# 独立演示：只跑 设置中心(19000) + approval(19801) + shell(19701)，不需要模型密钥
.\start-command-approval.ps1
.\submit-command.ps1 -CommandText "Write-Output hello"
.\inspect-command.ps1 -CommandText "Write-Output hello"   # 只分析、不执行

# 构建与测试
.\build.ps1 -GoExe go
.\test-approval.ps1 -GoExe go
```

- 工作目录 `command-work/`，设置与日志在 `command-runtime/`；停止按 `Ctrl+C`。
- 三个端口均可通过启动参数修改；改中心端口时提交要同步 `-Center ws://127.0.0.1:新端口/ws`。
- `shell` 的 `approval.service` 必须指向 `approval`；中心地址/名字对不上时 shell 会报 `RUNTIME_MISMATCH`，此时统一这两处配置即可（既有中心覆盖值优先于启动默认值）。
- 停止启动器会停止三个服务，但**已经启动的命令子进程不保证同时终止**——长时间操作的停止与恢复需另行处理。
- 若执行策略阻止 `.ps1`：`powershell -NoProfile -ExecutionPolicy Bypass -File .\start-command-approval.ps1`（不修改系统策略）。
