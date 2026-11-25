# memory 域

对外只有一张卡：`memory`。域内三个进程：

| 进程 | 角色 | 数据权威 | 对外 |
| --- | --- | --- | --- |
| `memory -role facade` | 门面 | 无数据库 | 注册设置中心；`context` + 全部 `memory.*` |
| `memory -role session` | 会话 | `data/memory.db`：conversation / message / task / work / 整理池 | 只监听 `/spv2`，不注册 |
| `memory -role experience` | 经验 | `data/experience.db`：长期记忆 / 经验网 / 共享摘要 | 只监听 `/spv2`，不注册 |

`-role` 必填；不指定直接拒绝启动，不再保留单进程兼容分支。

## 源码布局

```text
src/memory/
  session/          # 会话服务 + session/store
  experience/       # 经验服务 + shared/longterm/memnet/redact/audit/repair/compose
  facade/           # 门面 + context 拼装 + facade/compose
  tests/            # 三进程服务级用例
  main.go           # 按 -role 启动上面三个进程之一
```

三个实现包互不 import；`architecture_test.go` 用 `go list` 钉住边界。

## 职责

- `session`：`store` / `history` / `conversation_*` / `task_*` / `work_open` / `work_get` / `lease_*` / `work_update|sweep|cancel|renew` / `message_*`。
- `experience`：`remember` / `forget` / `consolidate` / `note` / `dig` / `lineheads` / `prune` / `publish_session` / `shared_sessions` / `shared_forget`。
- 门面 `context`：调 `session.context_base` 取四态基座（messages / task / work + 快照），completed 直通；其余调 `experience.inject` 取记忆四节，在基座上现注入；不碰数据库。
- 内部 method（不进门面名片）：`context_base`、`inject`、`settings_schema`、`staged_pending|mark|retry`。

## 设置

门面声明并转发 13 个键：

- `context.history_limit` → session
- `context.core_limit` / `core_max_chars` / `rel_limit` / `rel_max_chars` / `embedding` → experience
- `share.sessions_limit` / `share.digest_chars` → experience
- `db` / `shared_db` / `name` / `addr` / `center` → 门面只读展示

## 失败语义

- 门面启动时 session / experience 不可达、method 重叠或漏声明 → 启动失败。
- 运行中 `context` 任一内部调用失败 → 整个 `context` 失败，不静默降级。
- `session` / `experience` 独立模式不连接设置中心，依赖靠 `-peers` 静态解析；后上线依赖可 `SetPeer` 注入。
- 门面 metrics 聚合 session / experience 的指标；任一对端失败只跳过该对端，不影响健康探针。

## 运行与测试

```powershell
.\scripts\run-local.ps1            # 自动起 9201 门面 + 9311 session + 9312 experience
cd src/memory; go test ./... -count=1
cd testing/tests; go test ./... -count=1
```

设计边界与未完成项见 [docs/concepts/memory-target.md](../../docs/concepts/memory-target.md)。
