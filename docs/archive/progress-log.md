# 档案：进度清单（原 README「进度」节）

> 这是按批次累积的完成记录，原样从 README 迁出，只为留存历史；**当前能力请看 [architecture.md](../concepts/architecture.md) 的服务清单与 [roadmap.md](../concepts/roadmap.md)**。
> 归档：2026-10-30

## 进度

- [x] 重构设计（过程归档：[v1-to-v2.md](v1-to-v2.md)）
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
- [x] 设置全链路（M10）：**秘密项**（`HandleSecret`：值不进中心——不落盘、不回显，只在提交那一次转发给服务，服务自己持久化 `data/<名>-secret.json` 并在启动时加载；名片只报「有没有值」；注册对齐不含它）｜**只读项**（`HandleReadOnly`：进面板展示启动值、改动明确拒绝）｜**服务自报**（`AnnounceSetting` → `report_setting`：服务侧的值自己变了实时回传，中心不改覆盖值，面板同时显示「中心口径」与「服务侧」两列，不一致给「服务侧已变」标记）｜面板三类渲染（秘密项密码框覆盖 / 清空；只读项无修改入口）｜**全部服务的启动参数进面板**：reasoner（`llm.*` / `memory` / `images` 热改 + `name`/`addr`/`center` 只读）· qq（`napcat.url` 热改即重连 / `napcat.token` 秘密 / `reasoner`·`images` 热改 + 只读三要素）· memory（`recall.embedding` 热改 + `db` 与三要素只读）· timer（`portal` 热改 + 三要素只读）· images / 中心自身（路径与三要素只读）· 门户（五个只读项，不发明 configure 机制）｜CI 的 frontend 任务跑门户全链路 e2e（需先构建前端产物）
- [x] 前端服务 v1（`frontend`）：设置中心可视化面板——TS 服务节点（注册 / 心跳 / 名册）+ HTTP JSON + SSE；设置查看 / 修改（configure 下发）/ 重置；协议以 Go 产出夹具做跨语言字节级锚定
- [x] 文件服务（含 Excel/Word/PPT 读写）：`files` 服务（save/fetch/read/write/edit_word/edit_ppt，独立进程/独立库）；QQ 私聊文件消息 → 存库 → 占位带 id 上链；`read` 声明 `tool: true` 自动成为 LLM 工具（文本截断 / 二进制明确拒绝 / xlsx 转 TSV / docx/pptx 提取正文）；`write` 单元格级编辑 / `edit_word` .docx 纯文本替换 / `edit_ppt` .pptx 纯文本替换——含服务级测试、qq 接缝测试与全链真进程 e2e
- [x] embedding 服务（M8，Python）：文本 → 向量（onnxruntime + tokenizers 本地推理；fake 后端供测试与 CI）；协议 / 注册自愈 / 双后端
- [x] 语义召回（M8）：memory 接入 embedding——向量列（派生、懒补齐）+ 余弦 ≥0.45 阈值 + **bigram 兜底**（服务缺席/超时静默回落）；跨语言真进程 e2e；query 前缀 A/B 实测（no-prefix 胜出）
- [x] shell 命令执行服务（M9）：受理即回执（审批与执行不占响应）｜审批接缝（`Approver` 接口；未接入 = 一律未通过）｜结果以 `report_result` 补报（reasoner：在跑则注入、已收尾则开补报轮）｜护栏：工作目录根（越界即拒）+ 执行超时（终止整棵进程树）+ 输出上限（截断标注）｜**无队列**：并行/串行由调用方决定（一次派发几条就并行几条），服务只做防爆熔断（超过 `exec.max_running` 同步拒绝，绝不返回"已排队"）｜8 个设置项（`exec.*` / `approval.*`）
- [x] 命令审批第一层（approval）：PowerShell AST 规则引擎｜执行摘要与短期许可绑定｜shell 审批及模型反馈
- [x] 会话记忆扩到轨迹：记录分 `kind`（user / assistant / reasoning / tool_call / tool_result）+ `meta` + `seq`；reasoner 抓深度思考、工具执行前记命令、异步工具终态补报记输出，门户经 `progress` SSE 实时画出来
- [x] 任务板块：`tasks` 表 + 六个 `task_*` 动作；一个会话最多一个进行中任务（SQLite 部分唯一索引兜底，并发建不上就回头读）；没有任务先建再开工；**完成判定权在 reasoner**（内置 `task_status`，交付前必须先判，最多催两轮，仍不判按 continue 记账）；完结即不再续，记录留在原处
- [x] 门户 v2：对话改成步骤时间线 + Markdown 渲染；侧栏「会话 → 任务」目录，点任务只做跳转；设置项说人话（`kind` / `options` / `unit` → 分段控件 / 服务下拉 / 开关 / 单位读法）；语言包（`locales/*.json`，含"全 x"测试包抓漏键）；整站配色跟本地时段走（17 个锚点插值底色 / 光斑 / 强调色，按亮度自动切深浅令牌）
- [x] 经验网（`memory/memnet`）：五个大类种子 + 多挂靠 + 遗忘曲线 + 软剪枝；比较前减语料均值（实测把同义/无关的间隔从 +0.068 拉到 +0.216）；线头在召回时**现算**成簇，第二层不落库（避开质心融化）；记忆服务暴露 `note` / `dig` / `lineheads` / `prune`
- [x] 会话表：`conversations` 一行一个会话（title / entry / status + 两个时间），消息数与任务数一律现算；写入即建行、老库自动回填；`conversation_list` / `conversation_rename`
- [x] 复盘与单步推理：**模型调用只有一个出口**（`think`：一次补全，不带工具、不开任务）；交付后在后台复盘写经验（做法 / 坑 / 事实 / 偏好 / 取舍，上限 3 条，允许 0 条）；长任务按 opencode / codex 的形状压流水（工具输出截 1250 字、最近 8k token 逐字留、更早骨架化、切分点对齐轮次、硬上限 40k）
- [ ] 下一步：巩固线（sleep 提炼）、群聊（@ 必回 / 自主插话）、审批第二层补强（LLM 解读命令 + 富通知）、门户多会话与文件上传入口、PDF/Office 抽取（files.read 升级）、shell 后续（后台任务托管 / 输出有界引用 / 文件工具）、经验簇的短标签（睡眠时模型写，用的时候没有就现算）
