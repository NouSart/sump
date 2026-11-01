# AGENTS.md

> 给在本仓库工作的 AI 代理（以及想快速对齐的人）。
> **读者**：AI 代理 / 新贡献者 ｜ **读完能做**：知道什么绝对不能碰、命令怎么跑、去哪查细节 ｜ **最后校验**：2026-10-30

## 这是什么

SUMP 是「服务的互联网」式智能体系统：每个能力是一个**独立进程 + 独立数据库 + 按地址可达的 WS 服务**，智能体循环不存在于任何服务内部，而是 LLM 服务在网络上的自我跳转链。定位与路线见 [docs/concepts/architecture.md](docs/concepts/architecture.md)。

## 三条硬规则（架构红线）

1. **扩展不需要修改任何其他服务**：新增或替换服务，不得要求改动别人。
2. **唯一共享物是协议**：不共享进程内状态；不 import 其他服务的包；不直接读写其他服务的库或文件。
3. **能力可替换**：只要服务名与动作语义一致，任何语言都能顶替。

> 判断一个改动是否合规的最快问法：**它需要别的服务配合修改吗？** 需要，就是设计走偏了。

## 不可违反的技术约定

- 信封 `v: 1`；**动作只增不改**，删改 = 破坏性变更。
- **未知字段必须忽略**；**未知动作必须回 `{ok:false, error}`**，不得静默丢弃。
- 工具暴露必须显式：`provides` 里 `tool: true` 才给模型看；管理 / 调试动作不暴露。
- 每跳的 `boss`（结果归属）逐跳自定、**不可丢失**；入口服务必须把 boss 设成自己。
- 长耗时动作走 `async: true` + `report_result` 补报，不让调用方等。
- 派生数据（缓存 / 向量 / 索引）必须可丢弃、可重建；权威数据只有一份。
- **会话库私有、共享库只放提炼物**：`data/memory.db`（对话 / 任务）只有当事会话能读；跨会话共享的库（`data/experience.db`）**不得**出现原文、工具输出、路径或账号——靠表结构和载荷保证，不靠调用方自觉。
- 测试不得依赖外网：用假 LLM / 假对端；真模型只在显式开关下跑。
- 新动作要有测试（含错误路径）；不允许为了变绿而改弱测试。

细节：[docs/concepts/protocol.md](docs/concepts/protocol.md)（契约）、[docs/guides/add-a-service.md](docs/guides/add-a-service.md)（写服务）、[docs/concepts/agent-loop.md](docs/concepts/agent-loop.md)（运行时语义）。

## 常用命令

```powershell
.\scripts\run-local.ps1                 # 起全栈（门户 http://127.0.0.1:9501/）
.\scripts\reload-sump.ps1 -DryRun       # 改完代码：先看会杀掉谁，再去掉 -DryRun 重载
cd <模块>; go test ./... -count=1       # 单模块测试
cd tests; go test ./... -count=1        # 跨服务真进程端到端（约 20 秒）
gofmt -l .                              # 提交前必须无输出
```

改动 `frontend` 另跑 `cd frontend; npm test`；改动 `embedding` 另跑 `cd embedding; .\.venv\Scripts\python.exe -m pytest tests -q`。完整环境要求与自检清单见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 改代码的规矩

- 小步提交：一个提交只做一件事；信息用「类型: 中文描述」（`feat:` / `fix:` / `docs:` / `refactor:` / `test:` / `chore:`）。
- 协议变更（`protocol/`）、新动作、新消息类型：**先商量再动手**，且只做加法。
- 不顺手重构：改动范围收敛在目标内；发现无关问题提 issue，别顺路改。
- Go 以 `gofmt` 为准；注释与文档用中文；错误信息指明对象、可读。

## 改文档的规矩

- **一个事实只有一个家**：写在这里就别在别处复制，别处只放链接。
- **承诺必须有测试或验收脚本**，否则只能写进 [TODO.md](TODO.md)。
- 不建空文档；没有实现就不写「怎么做」，只写「未做」。
- 文档地图见 [docs/index.md](docs/index.md)。
