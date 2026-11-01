# 文档地图

> **读者**：第一次进仓库、想知道「这件事该看哪篇」的人 ｜ **读完能做**：直达那一篇，不再逐个翻 ｜ **最后校验**：2026-10-30

## 按「我想做什么」找

| 我想…… | 读这篇 |
| --- | --- |
| 先知道这是什么 | [../README.md](../README.md) |
| 在本机把它跑起来 | [quickstart.md](quickstart.md) |
| 写一个自己的服务 / 接入一个能力 | [guides/add-a-service.md](guides/add-a-service.md) |
| 改协议、加动作、判断改动是否合规 | [concepts/protocol.md](concepts/protocol.md) |
| 改审批规则、查「命令为什么没执行」 | [concepts/approval.md](concepts/approval.md) |
| 理解架构为什么长这样 | [concepts/architecture.md](concepts/architecture.md) |
| 理解一次对话怎么流动、状态落在哪 | [concepts/agent-loop.md](concepts/agent-loop.md) |
| 知道现在做到哪、下一步该证明什么 | [concepts/roadmap.md](concepts/roadmap.md) |
| 把 MCP 接到远程 / 先做接入检查 | [guides/mcp-connect.md](guides/mcp-connect.md) |
| 查某个服务的动作与设置项 | 服务页：[services/](services/)（images / qq / reasoner / settings-center / approval）；其余看服务目录的 README：[batch](../batch/README.md) / [embedding](../embedding/README.md) / [files](../files/README.md) / [frontend](../frontend/README.md) / [mcp](../mcp/README.md) / [memnet](../memory/memnet/README.md) / [shell](../shell/README.md) / [timer](../timer/README.md) |
| 交 PR、跑测试、看提交规矩 | [../CONTRIBUTING.md](../CONTRIBUTING.md) |
| 我是 AI 代理，先看红线 | [../AGENTS.md](../AGENTS.md) |
| 接手团队待办 | [../TODO.md](../TODO.md) |
| 查历史（旧设计、进度、验收快照） | [archive/](archive/) |
| 查实验结论（LLM 判官 vs 规则等） | [../experiments/](../experiments/) |
| 评测 harness 本身（不是模型） | [../evals/README.md](../evals/README.md) |

## 每篇的定位

| 文档 | 读者 | 状态 | 最后校验 |
| --- | --- | --- | --- |
| [quickstart.md](quickstart.md) | 想跑起来的人 | 有效 | 2026-10-30 |
| [concepts/architecture.md](concepts/architecture.md) | 想理解设计的人 | 有效 | 2026-10-30 |
| [concepts/agent-loop.md](concepts/agent-loop.md) | 改 reasoner / memory 的人 | 有效 | 2026-10-30 |
| [concepts/protocol.md](concepts/protocol.md) | 写服务 / 改协议的人 | 有效（`v: 1`） | 2026-10-30 |
| [concepts/approval.md](concepts/approval.md) | 改审批规则的人 | 有效（解释与富通知未实现） | 2026-10-30 |
| [concepts/roadmap.md](concepts/roadmap.md) | 想知道边界与顺序的人 | 有效 | 2026-10-30 |
| [guides/add-a-service.md](guides/add-a-service.md) | 加新服务的人 | 有效 | 2026-10-30 |
| [guides/mcp-connect.md](guides/mcp-connect.md) | 接远程 MCP 的人 | 有效 | 2026-10-30 |
| [services/](services/) | 查某个服务的动作 / 设置项 / 数据 / 依赖 / 怎么测 | 有效 | 2026-11-01 |
| [archive/v1-to-v2.md](archive/v1-to-v2.md) | 想追溯取舍的人 | 历史，勿据此开发 | 2026-10-30 |
| [archive/progress-log.md](archive/progress-log.md) | 想查批次进度的人 | 历史 | 2026-10-30 |
| [archive/mcp-acceptance-2026-10.md](archive/mcp-acceptance-2026-10.md) | 想查验收范围的人 | 历史快照 | 2026-10-30 |
| [../evals/README.md](../evals/README.md) | 想给编排层打分的人 | 有效（首版，含已知红项） | 2026-11-01 |

## 文档的规矩

1. **一个事实只有一个家**：写在一处，别处只放链接，不复制段落。
2. **每篇开头四行**：读者 / 读完能做 / 状态 / 最后校验日期。
3. **承诺必须有测试或验收脚本**，否则只能写进 [TODO.md](../TODO.md)。
4. **不建空文档**：没有实现就不写「怎么做」，只写「未做」。
5. 链接与目录一致性由 `tests/docs_test.go` 守着：`cd tests; go test -run 'TestDocs' ./...`
