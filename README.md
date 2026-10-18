# SUMP v2

[![CI](https://github.com/QAQ-awa-QAQ/sump/actions/workflows/ci.yml/badge.svg)](https://github.com/QAQ-awa-QAQ/sump/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)

> 状态：M9 完成（**shell 命令执行服务**：受理即回执 + 审批接缝 + `report_result` 补报）· 长期记忆与 embedding 语义召回（M8，Python 服务）· 文件服务（含 Excel 读写）· 前端服务 v1（设置中心可视化面板） · 设计文档：[DESIGN.md](./DESIGN.md)

SUMP v2 是一个“服务的互联网”式的智能体系统。每个服务独立运行（独立进程、独立数据库、任意语言），通过 WebSocket 按地址互相访问；智能体循环由 LLM 服务在服务网络之上自由跳转、组装。

## 与 v1 的关系

- 本仓库（`sump`）是 **v2 主线**，承载当前全部开发。
- **v1 已停止开发**：完整源码与版本历史归档于 [sump-v1](https://github.com/QAQ-awa-QAQ/sump-v1)（main + v0.1.0 ~ v1.0 全部 tag）；本地开发目录为 `../1`，其 origin 已指向该归档仓库。
- 重构原因：v1 是单进程 Python 单体，进程内共享状态 + 并发触发严重竞态；v2 用“服务隔离 + 网络通信 + 无驻留循环”从根上消除共享状态问题。

## 设计要点

- **一个文件夹 = 一个服务**；各服务自带 `tests/`，根目录有总 `tests/`（跨服务端到端）
- **WebSocket 通信**：默认短连接、按访问频率自适应保活；服务间点对点直连
- **设置中心**：注册 · 全局信息 · 设置与调度——不是中央交换机，不做流量中转
- **服务骨架库（`service/`）**：注册 / 心跳 / 名册 / 断线自愈（重连 + 重注册）/ 优雅下线（bye）统一实现——服务只写业务动作
- **语言分工（性能优先）**：Go 扛全部逻辑，Python 只做 embedding 推理，TS 做前端（`frontend` 服务：设置中心可视化面板）
- **前端服务（frontend）**：设置中心的浏览器面板——TS 服务节点（注册 / 心跳 / 名册广播）+ 浏览器 HTTP JSON + SSE；只做视图层，不持业务状态；界面走瑞典极简（系统无衬线字体、亚麻白底、零阴影）
- **智能体循环 = 跳转式调度**：每轮推理决定是访问工具服务、重新访问自己，还是发起审批
- **异步工具模型**：工具调用即时回执、结果以 `[工具结果]` 注入并唤醒下一轮（通知模式 `each`/`batch` 可设）；内置 `wait` 工具；**收尾门禁**保证结果到齐才交付；进行中任务的上下文由 reasoner 内存**黑板**持有（窗口 = 任务在跑期）
- **长期记忆**：`remember`（声明在 `provides` 里，自动成为 LLM 工具——加能力零 reasoner 改动）/ `forget` 软删；召回 = **语义召回（embedding 服务，余弦 ≥0.45）+ bigram 兜底（服务缺席/超时静默回落）** + 核心注入（priority）；recall 组装为「最近对话 / 核心记忆 / 相关记忆」三节；条目向量是派生数据（懒补齐、可重建）
- **记忆激活（完全启动）**：LLM 调 API 前先把任务上下文托管给记忆服务；只有收到 `from=memory` 的 `resume` 才“完全启动”——注入什么记忆由记忆服务决定
- **图片链路**：QQ 图片存入 images 服务（独立进程/独立库）；llm 推理前自动取“最近一条消息”的图转 base64 内联给模型（更早的消息转文本占位）
- **文件链路**：QQ 文件存入 files 服务（独立进程/独立库）；链上只传 id + 占位文本（`[文件: 名, id=...]`），`read` 声明 `tool: true` 自动成为 LLM 工具（`files__read`）——模型凭 id 自取文本（截断；二进制明确拒绝；xlsx 转 TSV、docx/pptx 提取正文），`write` 支持单元格级 Excel 编辑，`edit_word` 支持 .docx 纯文本替换，`edit_ppt` 支持 .pptx 纯文本替换，reasoner 零改动
- **受理即回执与补报（`async` → `report_result`）**：耗时不可预期的动作（如跑命令）响应只表示已受理（带回 `item_id`），后续进展由该服务补报回调用方；reasoner 收到补报时——任务在跑就注入同一条链、已收尾就开“补报轮”补一句给用户（结果不丢）；`item_id` 对不上的补报忽略；`wait` 如实披露“已受理未回报”的条数但不阻塞等待

## 本地运行

```powershell
# 构建并启动九个服务（settings-center :9000 + memory :9201 + embedding :9601 + images :9401 + files :9801 + qq :9301 + shell :9701 + reasoner :9101 + frontend :9501）
.\scripts\run-local.ps1
```

- 自定义地址：`.\scripts\run-local.ps1 -CenterAddr 127.0.0.1:9000 -MemoryAddr 127.0.0.1:9201 -EmbeddingAddr 127.0.0.1:9601 -ImageAddr 127.0.0.1:9401 -FilesAddr 127.0.0.1:9801 -QQAddr 127.0.0.1:9301 -ShellAddr 127.0.0.1:9701 -ReasonerAddr 127.0.0.1:9101 -FrontendAddr 127.0.0.1:9501`
- 设置面板：<http://127.0.0.1:9501/>——查看各服务声明的设置项、修改（中心受理即回执，不等待服务送达）/ 重置；名册、连接状态与设置结果（行状态：已覆盖·待确认 / 已覆盖·未送达；提示：已生效 / 未生效 + 原因）事件驱动实时刷新（SSE，无刷新按钮）；首次运行会自动 `npm install` + `npm run build`（需要 Node ≥ 22.18，推荐 24）
- 前端单独开发：`cd frontend; npm run build`（构建）/ `npm test`（协议 + 链路 + API + 面板组件测试）
- 接 QQ（NapCat 正向 WS，仅私聊）：`.\scripts\run-local.ps1 -Owner 2271917353 -NapCatURL ws://192.168.11.196:3001 -NapCatToken <token>`——连不上会自动重试，不影响其余服务
- shell（命令执行，工作目录根 = 仓库根）：**审批未接入时一律“未通过”**——命令不会被真正执行（fail-closed）；审批协议落地后由审批服务裁决。边界：工作目录越界即拒（含软链解析）、执行超时后终止整棵进程树、单流输出超上限截断并标注总数、并行上限与待执行队列上限（满即拒绝执行）。细则与设置项：[shell/README.md](./shell/README.md)
- embedding 与语义召回（需要 Python ≥ 3.12，首次运行脚本自动建 venv 装依赖）：模型不入库——把 `model*.onnx + tokenizer.json + config.json`（bge-small-zh-v1.5 量级）放进 `data/models/bge-small-zh-v1.5/`；**没有模型时脚本以 fake 后端启动（显著警告，语义召回不可用），放置后重启即启用**。细则与实测记录：[embedding/README.md](./embedding/README.md)
- 数据落在 `data/`（`memory.db` / `images.db` / `images/` / `files.db` / `files/` / `settings.json`，不入库）
- 接入真 LLM：`$env:DEEPSEEK_API_KEY='sk-...'; .\scripts\run-local.ps1`（或显式 `-LlmKey/-LlmModel/-LlmBase`）
- 真 LLM 冒烟测试：`$env:SUMP_LIVE_LLM='1'; $env:DEEPSEEK_API_KEY='sk-...'; cd reasoner; go test ./tests/ -run TestLiveDeepSeek -v`（平时自动跳过）
- `Ctrl+C` 停止（脚本会清理全部子进程）
- 跨服务端到端测试：`cd tests; go test ./... -count=1`

## 进度

- [x] 重构设计（[DESIGN.md](./DESIGN.md)）
- [x] 第一批骨架：reasoner + 设置中心（注册 / 名册 / 跨服务跳转 / 自我跳转）
- [x] 单步推理链（M2）：LLM 决策 / 工具跳转 / 自跳 step / deliver 直达 boss——含假 LLM 端到端与真 DeepSeek 冒烟
- [x] 记忆服务（M3）：会话历史 + 上下文组装；llm 经记忆激活的“完全启动”链（recall → resume）——含全链测试与真进程端到端
- [x] QQ 私聊接入 + 图片服务（M4）：NapCat/OneBot 11 → 任务链 → deliver 回发；图片存入 images、推理前取图内联（含全链真进程 e2e）
- [x] 设置存取（设置中心）：服务声明的可设置项可查询 / 覆盖 / 重置，覆盖值落盘 `data/settings.json`
- [x] 服务骨架库 `service/` 与离线判定：断线自愈（重连 + 重注册 + 注册去重）、优雅下线（bye）、中心心跳超时摘除与复归
- [x] 异步工具模型（M5）：派发即回执 / `[工具结果]` 注入 / `wait` / 收尾门禁 / 任务窗口黑板；工具结果通知模式 `tool.notify_mode`（each 默认 / batch；设置中心覆盖 + 启动拉取）
- [x] 长期记忆（M6）：`remember`（provides → 自动成为 LLM 工具）/ `forget` 软删；核心注入（priority）；recall 组装三节（最近对话 / 核心 / 相关）
- [x] 工具暴露显式声明（`provides` 里 `tool: true`）+ 设置下发（`configure`）：中心 set/reset 后推送生效值，reasoner 实时应用（启动拉取兜底）
- [x] 设置热链（M7）：注册即全量下发（受理回执不等待送达）｜`settings` 事件回传结果（ok=true / ok=false + 原因）｜骨架通用接收（`HandleSetting` 声明 + 校验 + 应用；注册后异步拉取兜底）｜设置项接入全部服务（reasoner 3 / memory 5 / images 1 / qq 1 / 中心 2）｜面板事件驱动自动刷新（无刷新按钮）
- [x] 前端服务 v1（`frontend`）：设置中心可视化面板——TS 服务节点（注册 / 心跳 / 名册）+ HTTP JSON + SSE；设置查看 / 修改（configure 下发）/ 重置；瑞典极简界面（React/Vite + 系统无衬线字体）；协议以 Go 产出夹具做跨语言字节级锚定
- [x] 文件服务（含 Excel/Word/PPT 读写）：`files` 服务（save/fetch/read/write/edit_word/edit_ppt，独立进程/独立库）；QQ 私聊文件消息 → 存库 → 占位带 id 上链；`read` 声明 `tool: true` 自动成为 LLM 工具（文本截断 / 二进制明确拒绝 / xlsx 转 TSV / docx/pptx 提取正文）；`write` 单元格级编辑 / `edit_word` .docx 纯文本替换 / `edit_ppt` .pptx 纯文本替换——含服务级测试、qq 接缝测试与全链真进程 e2e
- [x] embedding 服务（M8，Python）：文本 → 向量（onnxruntime + tokenizers 本地推理；fake 后端供测试与 CI）；协议 / 注册自愈 / 双后端
- [x] 语义召回（M8）：memory 接入 embedding——向量列（派生、懒补齐）+ 余弦 ≥0.45 阈值 + **bigram 兜底**（服务缺席/超时静默回落）；跨语言真进程 e2e；query 前缀 A/B 实测（no-prefix 胜出）
- [x] shell 命令执行服务（M9）：受理即回执（审批与执行不占响应）｜审批接缝（`Approver` 接口；未接入 = 一律未通过）｜结果以 `report_result` 补报（reasoner：在跑则注入、已收尾则开补报轮）｜护栏：工作目录根（越界即拒）+ 执行超时（终止整棵进程树）+ 输出上限（截断标注）+ 并行/队列上限（满即拒）｜8 个设置项（`exec.*` / `approval.*`）
- [ ] 下一步：巩固线（sleep 提炼）、群聊（@ 必回 / 自主插话）、审批链路（审批服务定协议 → 接入 shell 的 `Approver`）、PDF/Office 抽取（files.read 升级）、shell 后续（后台任务托管 / 输出有界引用 / 文件工具）

## 文档

| 文档 | 内容 |
| ---- | ---- |
| [DESIGN.md](./DESIGN.md) | 重构设计：背景 / 架构 / 目录约定 / 实施路线 / 待决问题 |
| [SPEC.md](./SPEC.md) | 服务规范（草案）：写一个新服务要满足什么（独立性 / 最小实现 / 动作词汇 / 工具暴露 / 异步补报 / 兼容规则 / 待定事项） |
| [CONTRIBUTING.md](./CONTRIBUTING.md) | 参与开发指南：环境 / 测试 / 提交规范 / 三条硬规则 |
| [shell/README.md](./shell/README.md) | shell 服务：边界、审批接缝、设置项、补报时序 |
| [embedding/README.md](./embedding/README.md) | embedding 服务：模型放置、A/B 实测与阈值来源 |
| [files/README.md](./files/README.md) | files 服务：文件存取、Excel 读写、设置项、错误处理 |
