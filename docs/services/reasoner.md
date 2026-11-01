# reasoner 服务

> **读者**：改单步推理、工具派发、任务判定的人 ｜ **读完能做**：知道一轮推理从哪进、结果从哪出、状态落在哪 ｜ **状态**：有效 ｜ **最后校验**：2026-11-01

**单步推理器**：没有常驻循环，一次只决定「下一步做什么」。用户消息进来后先把上下文托管给 memory，
收到 `resume` 才真正调模型；工具调用即时回执、结果注入黑板再唤醒下一轮。

## 职责

- 把 `provides` 里 `tool: true` 的动作映射成 LLM 工具（唯一暴露判据）。
- 收尾门禁：交付前必须 `task_status` 判定；还有工具在跑 / 快照过期 → 拒绝收尾。
- 判定在**收尾那一刻**落库（`done` → `task_finish`，否则 `task_rename`）——提前落库会让下半程的工作
  挂在一个「已完成」的任务里。
- 交付到 boss（`deliver`）；交付失败补报给入口；后台复盘写经验网（`reflect.on`）。

## 端口

默认 `127.0.0.1:9101`（`scripts/run-local.ps1` 的 `-ReasonerAddr`）。

## 动作

| 动作 | 输入 | 输出 | 说明 |
| --- | --- | --- | --- |
| `user_message` | `{text, conversation_id?, images?, files?, sender?, notes?}` | `{status: accepted}` | 链入口；随后经 memory 的 `resume` 完全启动 |
| `resume` | `{context}` | `{status: accepted}` | **只接受 `from=memory`**（入站，不声明） |
| `report_result` | 补报载荷 | 回执 | 入站：异步工具的结果回灌（不声明） |
| `think` | `{prompt, system?, model?, max_tokens?}` | `{text}` | 一次补全：不带工具、不开任务；所有非任务型模型调用的统一出口 |
| `retry` | `{conversation_id}` | `{retried, conversation_id}` | 门户专用：对上一轮失败、任务还开着的会话再跑一轮 |
| `echo` / `debug_jump` | 任意 | 原样 / 目标响应 | 调试与测试（不暴露给模型） |

## 设置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `llm.base` / `llm.model` | `https://api.deepseek.com` / `deepseek-flash` | 模型端点与名字 |
| `llm.key` | 空 | **秘密项**：值只在本服务手里 |
| `tool.notify_mode` | `each` | 工具结果逐条唤醒 / `batch` 攒批 |
| `tool.concurrency` | 10 | 单会话并发工具上限（-1 = 不限） |
| `tool.timeout_ms` | 60000 | 单次工具超时 |
| `reflect.on` / `share.on` | `true` | 交付后复盘写经验；把摘要发布到跨会话目录 |
| `memory` / `images` | 服务名 | 记忆与取图目标 |
| `name` / `addr` / `center` | 启动参数 | 只读项 |

## 数据

**进程内**（不落盘，任务结束即释放）：黑板 `board[cid] = taskState`（消息链、在途工具、`gen`/`seen` 水位、
判定状态）；工具并发信号量、停手状态、入口记录。

持久状态全在 memory（会话记录 / 任务 / 经验网）。**重启即丢在跑的轮次**是设计要求：链上没有长挂起的进程。

## 依赖

| 目标 | 动作 | 什么时候 |
| --- | --- | --- |
| `memory` | `recall` / `store` / `task_finish` / `task_rename` / `note` / `publish_session` | 每轮入口托管、记录轨迹、判定落库、复盘 |
| `images` | `fetch_full` | 最近一条带图消息内联给模型 |
| boss（入口服务） | `deliver`（失败时向入口补 `[交付失败]`） | 收尾 |
| 名册里的任意服务 | 该服务 `tool: true` 的动作 | 模型决定调用时 |

## 怎么测

```powershell
cd reasoner; go test ./... -count=1
```

- `server/task_test.go`：`task_status` 判定（done / continue / 非法值 / 已关闭任务 / 标题截断）。
- `server/board_close_test.go`：收尾门禁四个分支（在跑工具 → 拒绝且停车；快照过期 → 拒绝不停车；
  没判定 → 催两次；没任务 → 不设门禁）。
- `tests/attribution_test.go`：来源标注进正文 / 落库 / 模型上下文，任务名不带标签；静默三路。
- `tests/judge_commit_test.go`：判定只在收尾落库（被判废的 done 不落库）。
- 其余：`tests/async*`（异步受理与补报）、`tests/chain_test.go`（多轮推进）、`tests/round_failure_test.go`（失败与重试）。
