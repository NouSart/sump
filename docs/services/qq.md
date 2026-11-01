# qq 服务

> **读者**：接 QQ 入口、调群聊策略、排查「消息没回」的人 ｜ **读完能做**：知道私聊 / 群聊各自怎么走、来源标注写在哪 ｜ **状态**：有效 ｜ **最后校验**：2026-11-01

把 QQ 消息变成任务链的入口，也是链终点：回答最终从它发回 QQ。**只连主人的私聊**，群聊则按策略把
**每个人的**消息都送进上下文并标清「谁说的、在哪说的、多少人看得见」。

## 职责

- 私聊只认主人（`owner`），其余礼貌拒绝且不触发任务。
- 群聊任何人都能进来：入口只负责把事实标清楚（`protocol.Sender` → `Label()`），说不说、能不能外发由模型判断。
- 群消息三档策略：`observe` 只记录 / `mention` 仅被 @ 才推理 / `judge`（默认）每条都进上下文。
- 首次见到会话时给会话起一个人话标题（`conversation_open`），失败不重试、不挡消息。
- 实现 `deliver`：把任务结果发回原会话（私聊用 `user_id`、群聊用 `group_id`）。

## 端口

默认 `127.0.0.1:9301`（`scripts/run-local.ps1` 的 `-QQAddr`）。另外**主动**连 NapCat 的正向 WS
（`napcat.url`，默认 `ws://127.0.0.1:3000`），连不上会一直重试，不影响别的服务。

## 动作

| 动作 | 输入 | 输出 |
| --- | --- | --- |
| `deliver` | `{text, conversation_id}` | `{}`（已发回 QQ） |

入口侧（QQ → 链）不是动作，是内部流程：`user_message` 发给 reasoner（带 `sender` 标注）。

## 设置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `owner` | 空 | 主人 QQ 号；空 = 谁的私聊都不理 |
| `napcat.url` | `ws://127.0.0.1:3000` | NapCat 正向 WS 地址（热改即重连） |
| `napcat.token` | 空 | 访问令牌（**秘密项**：值只在本服务手里，中心不回显） |
| `group.mode` | `judge` | 群聊策略：观察 / 仅@ / 判断 |
| `reasoner` / `images` / `files` / `memory` | 各自服务名 | 消息交给谁推理、附件存哪、只记录时写进哪 |
| `name` / `addr` / `center` | 启动参数 | 只读项 |

## 数据

- **不落盘**：会话记录在 memory，图片在 images，文件在 files。
- 秘密文件（`-secret`，默认 `data/qq-secret.json`）只存 `napcat.token`。
- 进程内缓存：群资料（群名 / 人数）10 分钟、取不到时短缓存 1 分钟；已起过名的会话（每进程一次）。

## 依赖

| 目标 | 动作 | 什么时候 |
| --- | --- | --- |
| `napcat`（外部网关） | `send_msg` / `get_group_info` | 回消息、取群资料 |
| `images` | `save` | 消息里带图 |
| `files` | `save` | 消息里带文件 |
| `reasoner` | `user_message` | 该推理的消息（等受理回执） |
| `memory` | `store` / `conversation_open` | 被策略拦下的消息（只记录）、给会话起名 |

## 怎么测

```powershell
cd qq; go test ./... -count=1
```

- `tests/group_test.go`：来源标注字段、三档策略、群 `deliver`、群资料缓存、自己消息不回灌。
- `tests/titles_test.go`：会话标题（群名 / 昵称兜底 / 群名未知不起名 / 只起一次 / 失败不重试 / 不挡消息）。
- `tests/quiet_store_test.go`：只记录落库失败要有信号（累计计数 + `report`）。
- 跨服务真进程链路见根目录 `tests/qq_e2e_test.go`、`tests/qq_group_e2e_test.go`、`tests/file_e2e_test.go`。

## 已知限制

群聊授权只到「策略档位」，没有按群的授权白名单——见 [README 限制 4.1](../../README.md#已知限制上线时如实告知)。
