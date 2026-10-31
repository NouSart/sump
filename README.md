# SUMP v2

[![CI](https://github.com/NouSart/sump/actions/workflows/ci.yml/badge.svg)](https://github.com/NouSart/sump/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)

> **v0.0.1-beta.1** · Apache-2.0 · *SUMP is a service-mesh style agent runtime: every capability is an independent process with its own database and address, and the agent loop lives only in the network.*

SUMP 是「服务的互联网」式智能体系统：每个能力是**独立进程**（独立数据库、任意语言），按地址互相访问；智能体循环不存在于任何服务内部，而是 LLM 服务在网络上的自我跳转链。

## 三条路径

| 我想…… | 去哪 |
| --- | --- |
| 先把它跑起来 | [docs/quickstart.md](./docs/quickstart.md)（一条命令起全栈，门户 <http://127.0.0.1:9501/>） |
| 写一个自己的服务 | [docs/guides/add-a-service.md](./docs/guides/add-a-service.md) |
| 改协议 / 核心 | [docs/concepts/protocol.md](./docs/concepts/protocol.md) + [AGENTS.md](./AGENTS.md)（红线） |
| 理解它为什么长这样 | [docs/concepts/architecture.md](./docs/concepts/architecture.md) |
| 知道下一步做什么 | [docs/concepts/roadmap.md](./docs/concepts/roadmap.md) + [TODO.md](./TODO.md) |

全部文档见 [docs/index.md](./docs/index.md)。

## 现在能用它做什么

- **门户对话**：回复按 Markdown 渲染，思考 / 终端命令 / 终端输出画成步骤时间线；侧栏是「会话 → 任务」目录；设置页按服务分页、每项按类型出控件
- **工具**：文件（含 Excel / Word / PPT / PDF 读写）、命令执行（危险命令默认拦住，等用户点头）、图片、定时、批处理（一次扇出、按需唤醒）、MCP 双向翻译
- **记忆**：会话记录（对话 + 过程轨迹）+ 长期记忆 + 经验网；语义召回（embedding），embedding 缺席时 bigram 兜底
- **设置全链路**：普通项 / 秘密项（谁消费谁持有）/ 只读项，门户直接改，服务侧变化实时回传
- **可替换**：服务之间只共享协议；同名同语义的实现可以换语言重写

服务清单、端口与数据落点见 [docs/concepts/architecture.md](./docs/concepts/architecture.md)。

## 已知限制（上线时如实告知）

| # | 限制 | 影响 / 现状 |
| - | ---- | ----------- |
| 1 | 门户没有文件上传入口 | 模型能 `files.save`（给 URL 入库）→ `read`，但「用户本地上传一个文件」还没有入口；临时替代：给 URL，或用 shell 把文件放到 `data/files` 后手工入库 |
| 2 | `files.fetch` / 大图 `images.fetch` 回的是 base64 | 刻意不把 `files.fetch` 暴露给模型（会把上下文撑爆）；`images.fetch` 已暴露（图片索引的用途就是转 base64），大图会很长 |
| 3 | 语义召回需要模型文件 | 没有 `data/models/bge-small-zh-v1.5/` 时以 fake 后端启动，召回退化为 bigram |
| 4 | qq 入口需要外部 NapCat | 不起 NapCat 就只有门户一个入口 |
| 5 | 审批第二层只有「批准 / 拒绝」 | 解释（LLM 解读命令）与富通知未做；用户批准后 30 秒内必须执行（排队过期即作废） |
| 6 | 门户只有一个会话 | 会话身份固定为 `web:private:owner`，还没有多会话列表；任务板块的「会话 → 任务」目录目前只挂这一个 |
| 7 | 审批档位对 PowerShell 不生效 | `auto_approve` 只在 bash 侧被读取（`engine.Tier()` 的唯一调用点），面板上改档位对 PowerShell 命令没有效果 —— [#31](https://github.com/NouSart/sump/issues/31) |
| 8 | 每条命令的审批解析约 276ms | 每次评估都起一个 `pwsh` 子进程做 AST 解析（p50 276ms / p95 293ms）；它落在每条命令的必经路径上 —— [#33](https://github.com/NouSart/sump/issues/33) |

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/index.md](./docs/index.md) | 文档地图：我想做什么 → 读哪篇 |
| [docs/quickstart.md](./docs/quickstart.md) | 快速开始：依赖、一键启动、端口、测试、长期运行 |
| [docs/concepts/architecture.md](./docs/concepts/architecture.md) | 架构：服务清单、一次对话怎么流动、数据归属、刻意不做的事 |
| [docs/concepts/agent-loop.md](./docs/concepts/agent-loop.md) | 运行时语义：工具异步、会话记录、任务、经验网、复盘 |
| [docs/concepts/protocol.md](./docs/concepts/protocol.md) | 协议契约：独立性约束、生命周期、帧与信封、动作与暴露、异步补报、链机制、命名基线 |
| [docs/concepts/approval.md](./docs/concepts/approval.md) | 命令审批：一条命令怎么走完、裁决包、限额、用户裁决、边界 |
| [docs/concepts/roadmap.md](./docs/concepts/roadmap.md) | 路线与失败条件：三步走、现在在哪、什么情况下该停 |
| [docs/guides/add-a-service.md](./docs/guides/add-a-service.md) | 写一个服务：名片字段、三类设置项与下发、服务自报、测试要求 |
| [docs/guides/mcp-connect.md](./docs/guides/mcp-connect.md) | MCP 远程入口、私有 CA、不执行工具的接入检查 |
| [AGENTS.md](./AGENTS.md) | 给 AI 代理与新贡献者的红线与常用命令 |
| [CONTRIBUTING.md](./CONTRIBUTING.md) | 参与开发：环境、测试自检、提交流程、风格 |
| [TODO.md](./TODO.md) | 唯一待办源 |

## 历史

- 按批次的进度清单：[docs/archive/progress-log.md](./docs/archive/progress-log.md)
- v1 → v2 重构设计（原文归档）：[docs/archive/v1-to-v2.md](./docs/archive/v1-to-v2.md)
- MCP 验收快照（2026-10）：[docs/archive/mcp-acceptance-2026-10.md](./docs/archive/mcp-acceptance-2026-10.md)
- v1 源码与历史：[sump-v1](https://github.com/QAQ-awa-QAQ/sump-v1)

## 许可

[Apache-2.0](./LICENSE)。提交 PR 即表示同意以同一许可授权你的贡献（inbound = outbound，无需签 CLA）。