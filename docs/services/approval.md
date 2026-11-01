# approval 服务

> **读者**：改审批规则、排查「命令为什么没执行」的人 ｜ **读完能做**：知道裁决从哪来、档位怎么影响结果、数据在哪 ｜ **状态**：有效（解释与富通知未实现） ｜ **最后校验**：2026-11-01

**命令审批**：shell 执行前把命令交来裁决。两层——规则的自动判定（第一层）+ 人的裁决（第二层）。
细节口径见 [../concepts/approval.md](../concepts/approval.md)。

## 职责

- 按方言（bash / PowerShell）解析命令并给出 `{decision, reason_code, llm_feedback, plan_digest, policy_version}`。
- 档位（`auto_approve`）决定自动放行的宽窄；拒绝名单在任何档位下优先。
- 第二层：把待审项攒起来给门户列表，接收 `allow` / `deny`。
- 暴露 `capabilities`：能自动分析的方言、其它方言的处理方式、第二层状态。

## 端口

默认 `127.0.0.1:9901`（`scripts/run-local.ps1` 的 `-ApprovalAddr`）。子命令 `serve`（`run-local` 已带）。

## 动作

| 动作 | 输入 | 输出 | 谁调 |
| --- | --- | --- | --- |
| `approval_request` | `{schema_version, item_id, plan}` | `{decision, reason_code, llm_feedback, plan_digest, policy_version}` | `shell`（每条命令） |
| `approval_pending` | — | `{items:[{item_id, command, work_dir, reason_code, summary, expires_ms}]}` | `frontend` |
| `approval_decide` | `{item_id, decision(allow\|deny), note?}` | `{status, item_id, decision}` | `frontend`（**仅门户**） |
| `approval_list` | — | 同上（列表形态） | `frontend` |
| `capabilities` | `{}` | `{dialects, other_dialects, review_*}` | 门户 / 调试 |

## 设置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `auto_approve` | `off` | 自动批准档位：关 / 低 / 中 / 工作区（**当前只对 bash 生效**，见限制 7） |
| `policy.json` | 空 | 声明的规则快照配置 |

## 数据

- **待审项只在内存**：排队过期即作废（批准后 30 秒内必须执行），不落盘。
- 规则与策略来自 `policy.json`（启动参数），由运维维护，不在库里。

## 依赖

- 被 `shell` 调用做裁决；被 `frontend` 调用看列表 / 下裁决。
- 不主动调业务服务；解析 PowerShell 时**起一个 `pwsh` 子进程**跑 AST（每条命令约 276ms，见限制 8）。

## 怎么测

```powershell
cd approval; go test ./... -count=1
cd approval-integration; go test ./... -count=1
```

- `tests/`：规则判定、档位、拒绝名单优先、第二层排队与过期、`capabilities`。
- `approval-integration`：与**真 shell** 的集成（裁决 → 执行 → 补报全链）。

## 已知限制

- 档位对 PowerShell 不生效（[#31](https://github.com/NouSart/sump/issues/31)）。
- 每条命令约 276ms 的子进程开销（[#33](https://github.com/NouSart/sump/issues/33)）。
- **解释与富通知未实现**：见 [README 限制 5](../../README.md#已知限制上线时如实告知)。
