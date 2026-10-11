# SUMP v2

[![CI](https://github.com/QAQ-awa-QAQ/sump/actions/workflows/ci.yml/badge.svg)](https://github.com/QAQ-awa-QAQ/sump/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)

> 状态：M6 完成（长期记忆 v0：remember / forget + bigram 召回 + 核心注入） · 设计文档：[DESIGN.md](./DESIGN.md)

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
- **语言分工（性能优先）**：Go 扛全部逻辑，Python 只做 embedding 推理，TS 只做前端（后续的独立前端服务）
- **智能体循环 = 跳转式调度**：每轮推理决定是访问工具服务、重新访问自己，还是发起审批
- **异步工具模型**：工具调用即时回执、结果以 `[工具结果]` 注入并唤醒下一轮（通知模式 `each`/`batch` 可设）；内置 `wait` 工具；**收尾门禁**保证结果到齐才交付；进行中任务的上下文由 reasoner 内存**黑板**持有（窗口 = 任务在跑期）
- **长期记忆 v0**：`remember`（声明在 `provides` 里，自动成为 LLM 工具——加能力零 reasoner 改动）/ `forget` 软删；召回 = bigram 打分（1~2 字中文可命中）+ 核心注入（priority）；recall 组装为「最近对话 / 核心记忆 / 相关记忆」三节
- **记忆激活（完全启动）**：LLM 调 API 前先把任务上下文托管给记忆服务；只有收到 `from=memory` 的 `resume` 才“完全启动”——注入什么记忆由记忆服务决定
- **图片链路**：QQ 图片存入 images 服务（独立进程/独立库）；llm 推理前自动取“最近一条消息”的图转 base64 内联给模型（更早的消息转文本占位）

## 本地运行

```powershell
# 构建并启动五个服务（settings-center :9000 + memory :9201 + images :9401 + qq :9301 + reasoner :9101）
.\scripts\run-local.ps1
```

- 自定义地址：`.\scripts\run-local.ps1 -CenterAddr 127.0.0.1:9000 -MemoryAddr 127.0.0.1:9201 -ImageAddr 127.0.0.1:9401 -QQAddr 127.0.0.1:9301 -ReasonerAddr 127.0.0.1:9101`
- 接 QQ（NapCat 正向 WS，仅私聊）：`.\scripts\run-local.ps1 -Owner 2271917353 -NapCatURL ws://192.168.11.196:3001 -NapCatToken <token>`——连不上会自动重试，不影响其余服务
- 数据落在 `data/`（`memory.db` / `images.db` / `images/` / `settings.json`，不入库）
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
- [x] 设置存取（设置中心）：服务声明的可设置项可查询 / 覆盖 / 重置，覆盖值落盘 `data/settings.json`（下发到服务待做）
- [x] 服务骨架库 `service/` 与离线判定：断线自愈（重连 + 重注册 + 注册去重）、优雅下线（bye）、中心心跳超时摘除与复归
- [x] 异步工具模型（M5）：派发即回执 / `[工具结果]` 注入 / `wait` / 收尾门禁 / 任务窗口黑板；工具结果通知模式 `tool.notify_mode`（each 默认 / batch；设置中心覆盖 + 启动拉取）
- [x] 长期记忆 v0（M6）：`remember`（provides → 自动成为 LLM 工具）/ `forget` 软删；bigram 召回（1~2 字中文可命中）+ 核心注入（priority）；recall 组装三节（最近对话 / 核心 / 相关）
- [x] 工具暴露显式声明（`provides` 里 `tool: true`）+ 设置下发（`configure`）：中心 set/reset 后推送生效值，reasoner 实时应用（启动拉取兜底）
- [ ] 下一步：embedding 语义召回 / 巩固线（sleep 提炼）、群聊（@ 必回 / 自主插话）、审批链路

## 文档

| 文档 | 内容 |
| ---- | ---- |
| [DESIGN.md](./DESIGN.md) | 重构设计：背景 / 架构 / 目录约定 / 实施路线 / 待决问题 |
| [SPEC.md](./SPEC.md) | 服务规范（草案）：写一个新服务要满足什么（独立性 / 最小实现 / 动作词汇 / 工具暴露 / 兼容规则 / 待定事项） |
| [CONTRIBUTING.md](./CONTRIBUTING.md) | 参与开发指南：环境 / 测试 / 提交规范 / 三条硬规则 |
