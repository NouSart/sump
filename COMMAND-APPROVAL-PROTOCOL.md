# 命令审批 v1 协议与维护约定

状态：第一层已实现，第二层仅接口。公共类型的权威定义在 `protocol/approval.go`。

## 服务与输入

服务名默认 `approval`，动作 `approval_request`。使用原 SUMP 的 `jump` 信封，载荷为 MessagePack；以下 JSON 仅用于阅读。信封 `trace`、`boss` 沿用，业务关联使用 `item_id`。approval 不接受请求指定一个任意程序来充当解析器。

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

`args` 不包含最后的 command。工具将命令作为一个独立启动参数交给指定解释器，不能重新拼出不同命令。执行路径必须与审批进程选定的可信 PowerShell 路径一致；Windows 大小写比较不敏感。工作目录由 shell 解析符号链接并验证是否在根目录内。

固定环境：清除继承的 `PSMODULE*`、`POWERSHELL_*`、`PSEXECUTIONPOLICYPREFERENCE`，模块路径限定为解释器安装目录下 `Modules`，不加载用户 profile。其余环境在送审前由 shell 快照；不支持动态环境展开，所以不依赖变量取值做允许判断。这个配置约定依赖可信执行方和可信安装目录，不是远程沙箱证明。

## 裁决包

成功处理时 `ResponsePayload.ok=true`，`data` 包含：

| 字段 | 含义 |
| --- | --- |
| `schema_version` | 固定 1 |
| `item_id` / `decision_id` | 原任务和本次裁决的关联标识 |
| `decision` | `allow`、`deny`、`review` 之一；这是唯一裁决真值 |
| `plan_digest` | 绑定执行描述的 SHA-256 十六进制摘要 |
| `policy_version` | 配置版本字符串＋原始配置内容哈希前 8 字节的十六进制表示 |
| `reason_code` | 稳定原因码 |
| `valid_for_ms` | allow 为 30000；其他结果为 0 |
| `llm_feedback.summary` | 给模型的原因说明 |
| `llm_feedback.rule_ids` | 可选，命中的允许规则 ID |
| `llm_feedback.unknowns` | 可选，未覆盖的 AST 结构或解析错误 ID |

原因码包括 `RULE_ALLOW`、`POLICY_DENY`、`DYNAMIC_BEHAVIOR`、`NO_MATCHING_RULE`、`PARSE_ERROR`、`UNSUPPORTED_DIALECT`、`CONTEXT_UNKNOWN`、`RUNTIME_MISMATCH`、`LIMIT_EXCEEDED`。

无效请求、内部解析故障、繁忙使用 `ok=false`。deny/review 是正常业务结果，不算服务故障。shell 只将经过 `ValidateFor` 的 allow 映射为执行许可。

摘要编码依次写入以下 UTF-8 字符串，每项前写 uint64 大端字节长度：固定域 `sump-execution-plan-v1`、version 十进制、command、work_dir、shell_path、dialect、platform、profile、timeout_ms 十进制、max_output_bytes 十进制、参数个数十进制、每个 args 项。跨语言实现必须保持这一顺序，不能对 JSON 文本直接哈希。

## 反馈路径

```
终端工具 ──审批请求──> approval
终端工具 <─一份裁决包─ approval
终端工具 ──report_result──> reasoner ──模型上下文──> LLM
```

`ShellEventData.approval` 保存完整裁决包；`AsyncResultPayload.text` 含可读原因、规则和未知结构，因为原 reasoner 读取 text。通过先报 approved（非终态），结束报 done/failed（终态）；执行可与 approved 投递并行，执行槽在命令结束后释放，终态反馈等待 approved 投递尝试结束；拒绝/review 报 rejected（终态）。原始命令保留在工具反馈中，approval 自身故障上报不含原始命令。

工具在后台顺序投递，每次最多等 3 秒。只有明确 `status=ignored` 才按 50、100、200、400ms 退避重试，用于解决受理回执与补报先后到达的竞争；网络错误不确定对端是否已接收，因此不盲目重发。超过期限上报 `shell_feedback`，执行流程仍按已有裁决继续，不声称反馈已送达。若业务要求“模型必须收到才执行”，应另定强交付协议，不改写当前语义。

调用 `shell.run` 两次会创建两个任务；没有跨请求幂等键。调用方遇到网络错误不得自动重提交具有副作用的命令。第二层人工批准时必须明确任务生命周期与重提规则。

## 限额与失败处理

- 单条命令 16 KiB；审批和 shell handler 在解码前限制业务载荷 64 KiB。
- AST 节点超过 4096 或出现未知结构，不允许；解析子进程 5 秒超时，输出最多 256 KiB。
- approval 同时处理 8 条，可由 Go Config 在 1–64 范围设置；满时返回 busy，不建无限等待队列。
- shell 待审批／投递进展的后台任务最多 64；执行并发和执行队列沿用原配置，默认 8／32。
- 上述是业务层限额；原公共传输层未增加 WebSocket ReadLimit 或身份认证。
- allow 的 30 秒从工具请求开始计时；执行前再次验证摘要、命令、目录和有效期。
- 配置按请求读取一次不可变快照；无效新配置不替换旧快照。规则更新不撤销已返回但尚未过期的许可。
- 解析器异常、内部 panic 通过服务骨架 Report 上报设置中心；取消的请求不误报，内部解析超时在外层请求仍有效时上报。

## 扩展规则与方言

默认规则在 `approval/policy/default.json`。规则包定义版本、禁止命令名、允许命令名与静态参数数量上限；禁止规则优先。别名只接受显式列入配置的名称。命令名不是自然语言敏感词，不翻译用户内容，也不按语言封禁字面参数。

首版只批准完整 AST 中的单一静态命令。对参数带路径、写文件、网络访问、外部程序等业务，应增加相应事实提取与专门参数规则，而不是简单扩大命令名前缀允许范围。

**新增 shell 方言（仍属于 exec 域）**：实现 Parser，并同时定义执行 profile、路径身份和正反例测试；不能只修改 `dialect` 字符串就套用 PowerShell 结果。

**新增执行域（例如 fs、http、后台任务）**：先定义独立域判别字段（建议 `kind`），不让 `dialect` 兼任域名。各域声明自己的执行字段组、上下文校验、事实提取器和规则段；事实提取器注册在 approval 侧，不能接受调用方自报“安全”结论。裁决包、摘要绑定、有效期和 fail-closed 原则共用。当前 `plan` 的 command/work_dir/shell_path/args/dialect/platform/profile/timeout_ms/max_output_bytes 都是 exec 域字段；本次只约定扩展方向，没有新增域判别字段或执行域实现，见 SPEC.md §13。

## 第二层：用户裁决（已实现）

第一层（规则引擎）给不出 `allow` 时，命令**挂起**并推到门户，等用户点批准 / 拒绝。

### 消息

| 方向 | 动作 | 载荷 |
| ---- | ---- | ---- |
| approval → 门户 | `approval_pending` | `{item_id, command, work_dir, requested_by, reason_code, summary, unknowns[], rule_ids[], created_ms, expires_ms}` |
| 门户 → approval | `approval_decide` | `{item_id, decision: allow\|deny, note?}` |
| 门户 → approval | `approval_list` | `{}` → `{items: [...]}`（门户重启后拉回待审列表） |

### 规则

- **只接受门户裁决**：`approval_decide` / `approval_list` 的 `from` 必须是 `-portal` 指定的服务名（默认 `frontend`），别的服务调一律拒。
- **挂起的是这条命令，不是审批服务**：每条 jump 独立 goroutine；并发槽只在解析/裁决期间占用，等待用户时释放。其它审批照常处理。
- **TTL**（`-review-ttl`，默认 10 分钟）：到期按 `review` 原样返回（调用方视为未通过，不执行），待审项消失。
- **门户不在名册**：不挂起，立即按 `review` 返回（不浪费调用方 10 分钟等一个不会来的裁决）。
- **队列满**（`-max-pending`，默认 32）：按 `review` 返回，不排队。
- 用户批准 → `decision=allow`、`reason_code=USER_APPROVED`、`valid_for_ms=30000`；拒绝 → `decision=deny`、`reason_code=USER_DENIED`。
- **许可期从"拿到裁决"起算**（shell 侧）：`valid_for_ms` 仍是 30 秒，但计时起点是收到裁决的时刻——否则用户审 5 分钟、许可早过期，等于白审。
- 门户重启后用 `approval_list` 恢复列表；`approval` 侧待审项只在内存里，重启即清空（挂起随之失效，调用方按未通过补报）。

### 与"零阻塞"的关系

阻塞被限制在**调用方自己那条链**：shell 的这条 job 在等裁决（受 `approval.timeout_ms` 限制，默认 10 分钟），
审批服务、其它服务、其它命令都不受影响。门户慢或挂掉也不阻塞——推送是 Fire（不等回执）。

## 第二层钩子（解释与通知，未实现）

`approval/review/ports.go` 中保留四个接口：

1. `ExplainPort.RequestExplanation`：请求模型解释命令。
2. `UserNoticePort.NotifyPending`：初次通知用户服务。
3. `ExplanationReceiver.ReceiveExplanation`：接收模型回传。
4. `UserUpdatePort.UpdateExplanation`：转发解释并更新用户。

Context 携带原请求、裁决、trace、boss；Explanation 携带 item_id、执行摘要、目的、影响、未知项。它们目前没有生产实现或网络路由。下一阶段先约定用户服务消息与状态持有者，再并行启动 1、2，收到 3 后执行 4。解释结果不自动成为执行许可，LLM 或用户超时也不能隐式放行。
