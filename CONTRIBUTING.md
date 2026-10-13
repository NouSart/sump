# 参与开发

感谢愿意一起做 SUMP！本文是操作版指南——**规范本身在 [SPEC.md](./SPEC.md)，动手前请先通读**（一页，不多）。

## 你会得到什么

- 一个真实的"服务的互联网"式智能体系统：多进程、独立库、全链路真进程测试
- 服务内完全自治：你负责的服务，怎么实现你说了算
- 一对一答疑与 code review（作者本人）

## 环境要求

- Go 1.26+（`go version` 确认）
- Git；文本文件使用 **LF** 换行（仓库已带 `.gitattributes`，克隆后自动生效；Windows 用户无需额外设置）
- 可选：VS Code + Go 插件

## 快速开始

```powershell
git clone https://github.com/QAQ-awa-QAQ/sump.git
cd sump

# 跑某一个模块的测试（推荐先跑一遍确认环境正常）
cd protocol; go test ./... -count=1; cd ..
# 模块共 8 个：protocol / service / settings-center / reasoner / memory / images / qq / tests

# 前端服务（TS，Node ≥ 22.18）：类型检查 + 构建 + 测试
cd frontend; npm install; npm test; cd ..

# 跨服务真进程端到端（约 20 秒）
cd tests; go test ./... -count=1; cd ..

# 本地启动全部服务（含设置面板 http://127.0.0.1:9501/）
.\scripts\run-local.ps1
```

测试**不依赖外网**（LLM 用假客户端；真模型冒烟测试仅在显式开关下跑）。

## 提交前自检（三件事，缺一不可）

1. `gofmt -l .` 无输出（有输出就先 `gofmt -w` 再重跑）
2. 你改动涉及的模块测试全绿：`cd <模块>; go test ./... -count=1`
3. 脱离工作区也能构建：
   ```powershell
   cd <模块>; $env:GOWORK='off'; go build ./...
   ```

改动 `frontend`（TS）时另加：`cd frontend; npm test`（协议 / 中心链路 / HTTP API；`npm run build` 内含类型检查）。

CI（GitHub Actions）会替你跑全部内容；本地先过一遍能省一轮等待。

## 三条硬规则（架构红线）

1. **扩展不需要修改任何其他服务**：新增服务、替换服务，都不允许要求改动别人。
2. **唯一共享物是协议**：跨服务不共享进程内状态；不 import 其他服务的包；不直接读写其他服务的库或文件。
3. **能力可替换**：只要服务名与动作语义一致，任何语言都能顶替。

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
