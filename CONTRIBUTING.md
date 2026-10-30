# 参与开发

感谢愿意一起做 SUMP！本文是操作版指南。**动手前先读两份**：[AGENTS.md](./AGENTS.md)（红线，一页）与 [docs/concepts/protocol.md](./docs/concepts/protocol.md)（协议契约）。写新服务看 [docs/guides/add-a-service.md](./docs/guides/add-a-service.md)，跑起来看 [docs/quickstart.md](./docs/quickstart.md)。

## 你会得到什么

- 一个真实的"服务的互联网"式智能体系统：多进程、独立库、全链路真进程测试
- 服务内完全自治：你负责的服务，怎么实现你说了算
- 一对一答疑与 code review（作者本人）

## 环境要求

- Go 1.26+（`go version` 确认）
- Python ≥ 3.12（embedding 服务与根 e2e；`python --version` 确认）
- Git；文本文件使用 **LF** 换行（仓库已带 `.gitattributes`，克隆后自动生效；Windows 用户无需额外设置）
- 可选：VS Code + Go 插件

## 快速开始

克隆、起全栈、跑测试的完整步骤见 [docs/quickstart.md](./docs/quickstart.md)。

单模块测试的通用形式：`cd <模块>; go test ./... -count=1`。Go 模块 15 个：protocol / service / settings-center / reasoner / memory / images / files / qq / shell / timer / batch / tests / approval / approval-integration / mcp；另有 embedding（Python，`cd embedding; python -m pytest tests -q`）与 frontend（TS，`cd frontend; npm test`）。

测试**不依赖外网**（LLM 用假客户端；真模型冒烟测试仅在显式开关下跑）。

## 提交前自检（三件事，缺一不可）

1. `gofmt -l .` 无输出（有输出就先 `gofmt -w` 再重跑）
2. 你改动涉及的模块测试全绿：`cd <模块>; go test ./... -count=1`
3. 脱离工作区也能构建：
   ```powershell
   cd <模块>; $env:GOWORK='off'; go build ./...
   ```

改动 `frontend`（TS）时另加：`cd frontend; npm test`（协议 / 中心链路 / HTTP API / 面板组件测试；`npm run build` 内含类型检查，含测试）。
改动 `embedding`（Python）时另加：`cd embedding; python -m pytest tests -q`（CI 只装核心依赖——真模型冒烟在本地以 `SUMP_EMBED_MODEL_DIR` 开启）。

CI（GitHub Actions）会跑各模块测试、根 e2e、前端与 embedding；Windows 专属分支（PowerShell 5.1 代码页、盘符路径等）由单独的 Windows 任务执行；`-race` 只覆盖并发敏感模块。本地先过一遍能省一轮等待。

## 三条硬规则（架构红线）

见 [AGENTS.md](./AGENTS.md)：① 扩展不需要修改任何其他服务；② 唯一共享物是协议；③ 能力可替换。

> 判断一个改动是否合规的最快问法：**它需要别的服务配合修改吗？** 需要，就是设计走偏了。

## 提交流程

1. **大改动先开 issue**：协议变更（`protocol/`）、新动作、新消息类型——先商量再动手。
   协议对已有内容**只做加法**，删改 = 破坏性变更。
2. **小步提交**：一个提交只做一件事；提交信息用"类型: 中文描述"
   （`feat:` / `fix:` / `docs:` / `refactor:` / `test:` / `chore:`），与现有历史保持一致。
3. **提 PR**：描述里写清楚"做了什么 / 为什么 / 怎么验证"；CI 通过 + review 后合并。
4. **测试要求**：新动作要有测试（含错误路径）；不允许为了让测试变绿而改弱测试。

## 风格

- Go 代码以 `gofmt` 为准；注释与文档用中文；错误信息指明对象、可读。
- 命名：动作小写 + 下划线（`list_settings`）；服务名用小写短名（`memory`）。
- 不顺手重构：改动范围收敛在目标内；发现无关问题提 issue，别顺路改。

## 许可证与贡献授权

本项目以 [Apache-2.0](./LICENSE) 开源。提交 PR 即表示你同意以 Apache-2.0 授权你的贡献（inbound = outbound，无需单独签署 CLA）。

## 联系

- 优先 GitHub Issue（公开可查、便于后来者检索）
- 其他方式见仓库主页的招募信息
