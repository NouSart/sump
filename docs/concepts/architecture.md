# 架构：为什么是「服务的互联网」

> **读者**：想理解这套设计为什么长成这样、而不是一个大单体的人 ｜ **读完能做**：判断一个改动是否在架构方向上、找到该改哪个服务 ｜ **最后校验**：2026-10-30

## 1. 一句话

SUMP 是「服务的互联网」：每个服务是独立进程、独立数据库、按地址可达的 WS 端点；**智能体循环不存在于任何服务内部**，而是 LLM 服务在网络上的自我跳转链。

## 2. 世界里只有两样基础设施

- **协议**（怎么说话）——见 [protocol.md](protocol.md)
- **设置中心**（谁在哪、有什么设置）——不是中央交换机，**不做流量中转**

除此之外没有任何特权服务：LLM 推理、记忆、图片、QQ / 门户接入、浏览器前端、工具、搜索、审批、智能家居……**全部是服务**，独立进程 / 独立库 / 可单独替换。

由此得到三条硬规则（正文见 [AGENTS.md](../../AGENTS.md)）：扩展不需要修改别人、唯一共享物是协议、能力可替换。

## 3. 服务清单

| 服务 | 语言 | 一句话职责 |
| --- | --- | --- |
| settings-center | Go | 维护「服务名 → 名片」注册表；注册返回名册、新注册广播名册；保管设置并提供存取动作 |
| reasoner | Go | 单步推理器：任务入口经 memory 托管、收到 resume 才调 LLM；工具异步派发（即时回执 → 结果注入 → 唤醒），收尾门禁保护，结果 deliver 直达 boss |
| memory | Go | 会话消息的唯一保管者；recall 取最近历史 + 长期记忆组装为记忆块后 resume 回调用方；remember 写入 / forget 软删；任务与经验网也在这里 |
| images | Go | save（URL/base64 落盘）返回 id；fetch（id → base64） |
| files | Go | save / fetch / read（文本截断、xlsx 转 TSV、docx/pptx 提取正文）/ write（单元格级编辑）/ edit_word / edit_ppt |
| shell | Go | 受理即回执的命令执行（无队列，只有防爆熔断；超时终止 + 输出截断）；审批接缝（未接入 = 一律未通过）；结果由 `report_result` 补报 |
| approval | Go | 终端命令审批：PowerShell AST + 不可变规则快照 + 执行摘要绑定 + fail-closed；第二层用户裁决 |
| timer | Go | 计时器：受理「N 毫秒后唤醒我」（即时回执）→ 到点把 `report_result` 跳回调用方；按 boss 分组推倒计时 |
| batch | Go | 批处理：一次声明 N 项工作 + 并发度 + 唤醒时机；暂停可恢复、停止是终态；目标动作须显式声明 `Batchable` |
| mcp | Go | 双向协议翻译：显式映射导出获准的原生能力、导入外部 MCP 工具；只登记自身，不自动传播下属能力 |
| qq | Go | 把主人私聊消息（图先存 images）打成 `user_message` 发给 reasoner；实现 deliver 回发 |
| frontend | TS | 用户交互门户（与 qq 同类）＋ 设置面板：提供 `deliver`（推给浏览器），自己跳 `user_message`；浏览器 HTTP JSON + SSE；单用户 + Host/Origin 栅栏 |
| embedding | Python | 文本 → 向量（onnxruntime + tokenizers，CLS pooling + L2 归一）；动作 `embed`；fake 后端供测试与 CI |

两个 Go **库**（不是服务）：`protocol/`（协议与载荷类型）、`service/`（服务骨架：注册 / 心跳 / 名册 / 重连 / 告别 / 设置接收）。根 `tests/` 是跨服务真进程端到端，不属于任何服务。

端口一览见 [../quickstart.md](../quickstart.md)。

## 4. 一次对话怎么流动

```text
门户/QQ ──user_message(boss=自己)──▶ reasoner
reasoner ──recall──▶ memory            （上下文托管出去）
memory ──resume(from=memory)──▶ reasoner   （只有收到它才真正调模型）
reasoner ──jump──▶ 工具服务（shell / files / images / timer / batch / …）
工具服务 ──report_result──▶ 调用方       （长耗时的都走异步补报）
reasoner ──deliver──▶ boss             （终点直达，不沿链逆向回传）
```

三条容易记错的点：

- **boss 不可丢**：入口服务必须把 boss 设成自己，否则回答照样生成但**回不来**（reasoner 只按 boss 交付）。
- **记忆是真相**：`progress` 只是「别等最后才看见」，丢一条不影响历史。
- **发出即完**：一步 = 收到 → 处理 → 发出下一跳 → 结束；`wait` 是唯一的阻塞点。

运行时语义（工具异步、会话记录、任务、经验网、复盘）见 [agent-loop.md](agent-loop.md)。

## 5. 数据归属

每个服务只读写**自己的**库与目录；跨服务的一切都走消息。派生数据（缓存 / 向量 / 索引）必须可丢弃、可重建——**权威数据只有一份**。

| 服务 | 落点 | 内容 |
| --- | --- | --- |
| memory | `data/memory.db` | `messages`（对话与轨迹）、`tasks`、`conversations`、经验网表 |
| images | `data/images.db` + `data/images/` | 元数据 + 图片文件 |
| files | `data/files.db` + `data/files/` | 元数据（含 `filename`）+ 文件 |
| settings-center | `data/settings.json` | 设置覆盖值（预设值由各服务声明，不落盘） |
| batch | `data/batch.db` | 任务与工作项（重启后能给出明确归宿） |
| 各服务 | `data/<名>-secret.json` | 秘密项的明文文件（目录 0700、文件 0600、原子替换） |

`data/` 不入库；删掉它等于清空记忆、文件与设置。

## 6. 与 v1 的关系

- 本仓库是 **v2 主线**，承载当前全部开发。
- **v1 已停止开发**：单进程 Python 单体，进程内共享状态 + 并发竞态严重；源码与版本历史归档在 [sump-v1](https://github.com/QAQ-awa-QAQ/sump-v1)。
- 重构的核心取舍：用「服务隔离 + 网络通信 + 无驻留循环」从根上消除共享状态问题——代价是每次跨服务交互都要过网络与序列化。
- 走过哪些弯路、当时怎么取舍的，见 [../archive/v1-to-v2.md](../archive/v1-to-v2.md)。

## 7. 这个架构刻意不做的事

- **不做中央调度**：中心只管名册与设置，不转发业务调用（否则它就是单点与新特权服务）。
- **不做进程内共享**：需要共享的东西要么进协议，要么归某一个服务所有。
- **不做万能插件宿主**：一个能力 = 一个服务，语言自由，可被同名同语义的实现替换。
- **不提前做生态层**：认证、市场、多租户属于后续阶段，见 [roadmap.md](roadmap.md)。
