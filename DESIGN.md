# SUMP v2 · 重构设计

> 状态：草稿（填写中）
> 来源：`../1`（SUMP v0.2.0 实现，参考 `docs/SUMP_architecture.md`）
> 目标：本目录（`2`）

---

## 1. 背景：1 走了哪些弯路

- **根本架构问题**：1 从根本架构上就有问题，导致严重竞态（具体场景待补充）。
- **v2 对策**：服务之间彻底隔离——独立进程、独立运行、独立数据库，用网络通信取代进程内共享状态。

## 2. 设计目标与非目标

**核心理念：模仿互联网。** 一句话：v2 是一个“服务的互联网”——服务之间不共享任何进程内状态，只在网络上互相访问。

- **服务互相独立、互相访问**：每个服务是独立进程，有自己的地址，可以被其他服务按地址访问。
- **WebSocket 通信**：服务之间的一切往来都通过 WS 消息传递。
- **语言不统一**：各服务自由选择语言——当前分配（性能优先）：Go 扛全部逻辑，embedding 用 Python，前端 TS。
- **独立数据库、独立运行**：各服务自带存储，可单独部署、单独重启。
- **智能体循环可自由组装**：循环不驻留任何服务内部——每一轮都是一次独立的消息处理，轮次间以 LLM 服务的**自我跳转**（`step`）衔接，每一步都可插入 / 替换 / 观察；任务窗口内的上下文与在跑工具由内存**黑板**持有（窗口 = 任务在跑期，不落盘、不跨重启）。

**非目标**：（待补充）

## 3. 总体架构

### 互联网类比

| 互联网 | SUMP v2 |
| ------ | ------- |
| 网站 | 服务（独立进程 / 独立数据库 / 任意语言） |
| 网址 | 服务地址（`ws://host:port/...`） |
| 浏览器 + 网址栏 | LLM 服务发起的一次“访问” |
| 网页间跳转 | 智能体循环的每一步 |
| 网民 | LLM 服务（决定下一个访问哪里） |

### 设置中心（注册 · 全局信息 · 设置与调度）

- **服务注册**：各服务访问设置中心并发送报文，内容包括：自己的位置（地址）、可设置项、功能简介、接受什么输入、有什么输出。
- **全局信息分发（类似 DNS）**：
  - **推送**：有新服务注册时，设置中心向所有服务推送最新**全量**清单（`roster` 事件）；
  - **拉取**：服务可随时访问设置中心获取最新全量清单（`jump` 的 `roster` 动作）——推送丢失 / 服务重启后的自愈手段。
- **设置与调度**：各服务接受设置中心的设置与调度。调度**不是中央交换机**，而是“设置各个服务的行为、设计工作流”。
  - **设置存取（已实现）**：服务注册时声明的 `settings` 由设置中心保管——`list_settings` 查询、`set_setting` 写覆盖值、`reset_setting` 清除覆盖；生效值 = 覆盖值 ?? 服务声明的预设值；覆盖值落盘 `data/settings.json`。**下发（已实现·注册即对齐）**：`register` 响应发出后，中心异步把该服务**全部生效值**逐键以 `configure` 下发（有界重试；服务明确拒绝不重试），set/reset 成功后单键下发；**受理回执不等待送达**，结果（ok=true / ok=false + 原因）以 `settings` 事件广播（`{cause, service, results}`），面板据此自动刷新与提示。服务侧接收由骨架统一实现（`HandleSetting`：声明 / 校验 / 应用；注册成功后异步拉取一次作兜底，与下发幂等）。设置动作与 `configure` 都不在 `provides` 声明 `tool`（不作为 LLM 工具暴露）。中心自身也声明并应用自己的设置项（`sweep_ms` / `heartbeat_fallback_ms`，本地应用、不经网络）。
  - **离线判定（已实现）**：服务在名片中声明心跳间隔（`heartbeat_ms`）；中心周期扫描，超过 2× 心跳未见即判离线——摘除名册并广播；退出前的 `bye` 心跳立即摘除；心跳晚到可从墓碑复归。断开自愈在服务侧（`service/` 骨架：指数退避重连 + 重新注册 + 注册去重）。
- **错误上报（已实现·留接口）**：服务经既有中心连接 `report` 一条错误——中心记日志并交给 `OnReport` 挂点（frontend / 落盘 / 富日志后续接入——前端面板已在，上报视图按计划随 v2）。**不含链路编号**（暂无链路记录者）；**防刷在上报方**：退避 1s 起逐档翻倍、封顶 30s，静默 60s 重置；中心视为**永远在线**——不排队、不回补。当前接入点：骨架 `Fire` 未送达（kind=fire）。
- **前端面板（frontend，已实现 v1）**：拓扑 `浏览器 ——HTTP/JSON + SSE——▶ frontend(:9501) ——单条 WS（注册 / 心跳 / 名册广播 / 动作代转）——▶ 设置中心`。面板本体是服务节点（可在名册中看到自己），但**不持业务状态**：每次请求把 HTTP 翻成中心跳转、把回执翻回 JSON；名册与连接状态经 SSE 实时推送。界面（v2 版式，多轮评审后定稿）：**悬浮胶囊 + 表格化**——①页面=表格：主列元素同一左轴，设置五列全对齐（键 / 生效值 / 默认值 / 状态 / 操作）；②唯一主体=服务名（clamp 56–96px），其余要素降为注释层；③边栏=悬浮胶囊（白底、细边、微浮起、20px 圆角、内容高不通高、sticky）；④色彩克制：白 #FAFAF9 / 近黑 #101418 / 灰，唯一蓝 #005B99（绿仅 7px 状态点）；字体 Fira Sans / Fira Code（@fontsource 本地打包、离线可用）。版式样张在 `frontend/design/`（specimen.html + specimen.css；字体资产为拷贝，不入库）。**评审记录**：暗色（skill Modern Dark）、服务身份色池、通高栏、12 栏大字号色场均经试验后否决——定稿以样张为准。**有意偏差**：首个连接失败不退出（面板的持续退避重连优先于“引导错误立刻可见”）；`-web` 可指定静态目录。v1 范围：设置查看 / 修改 / 重置 + 名册 / 连接状态 / 设置结果事件驱动实时（无刷新按钮）；错误上报视图留待 v2。
- 服务间的实际访问**不经设置中心中转**。

```mermaid
flowchart LR
    C[设置中心<br/>注册 · 全局信息 · 设置与调度]
    A[服务 A] <-->|注册 / 全局信息| C
    B[服务 B] <-->|注册 / 全局信息| C
    A <-->|直接互访| B
```

### 智能体循环：自我跳转 + boss 直送

**循环不实现在任何服务内部**，而是网络上的跳转链：每一轮是独立的消息处理（收到 → 处理 → 发出下一跳 → 结束），服务不驻留循环状态；跨步状态原则上随消息携带（唯一例外：reasoner 任务窗口黑板，见下）。

- **boss（老板）= “面向谁工作”**：每跳自带其 boss（结果归属该对象），**逐跳自定、不强制沿用上游、但不可丢失**——如：qq 发起任务是任务 boss、**必须收到结果**；过程中的工具为 LLM 工作（工具那跳的 boss 是 LLM）；LLM 产出结果后**交还它的 boss（qq）**
- 链终点“判定结束”后**直接向 boss 送达结果**（jump 到其约定的 `deliver` 动作）——**不沿链逆向回传**，每步“发出即完”，没有长挂起的链
- LLM 服务是**单步推理器**：收到一次输入（`resume` / `step` / 工具结果），调一次 LLM，产出“下一步动作”
- **“继续推理” = LLM 服务对自己的跳转**（`step`，信纸只带会话号——上下文在黑板里）——与其他跳转完全同构
- **工具是异步的**：调用即刻回执（占住 API 的 `tool_calls` 配对位），执行在后台；结果以 `[工具结果]` 消息注入黑板并按通知模式唤醒下一轮；`wait` 是内置工具（阻塞等齐）；**收尾门禁**保证“结果没到齐不得收尾”
- 局部等待仅限“工具调用的一跳响应”（拿返回值）与 `wait`；链条推进全靠自跳

```mermaid
flowchart LR
    B["boss（发起者）"] -->|"user_message（boss=自己）"| L["LLM 服务 (Go)<br/>单步推理 + 任务黑板"]
    L <-->|"工具调用 + [工具结果] 注入（异步）"| T[工具服务]
    L <-->|审批| P[审批服务]
    L <-->|"记忆往返（recall/resume·任务入口）"| M[记忆服务]
    L -.->|"自我跳转：下一轮（会话号）"| L
    L -->|"结束：deliver 直达"| B
```

### 记忆往返：完全启动

LLM 服务不自己持有记忆——**每次调用 LLM API 之前，都要先把任务上下文托管给记忆服务**：

- 收到任务入口（`user_message`）时只做“半启动”：把上下文（含原 boss）以 `recall` 交给记忆服务，本轮结束（发出即完）；任务内的后续轮次由黑板延续，不再重复经记忆；
- 记忆服务查询会话历史、组装记忆——**注入什么记忆、上下文最终长什么样，全由记忆服务决定**（LLM 服务只负责把组装结果交给 LLM API）；
- 记忆服务把组装后的上下文以 `resume` 发回；LLM 服务**只在收到来自记忆服务的消息（`from=memory`）时才“完全启动”——这是调用 LLM API 的唯一入口**（其他来源的 `resume` 一律拒绝）；
- 信封 `boss`：记忆为 llm 工作（请求与回复的 boss 均为 llm）；原任务 boss 随上下文托管、原样带回，交付时仍直达原 boss。

```mermaid
sequenceDiagram
    participant B as boss（发起者）
    participant L as LLM 服务
    participant M as 记忆服务
    B->>L: user_message（boss=发起者）
    Note over L: 半启动：托管上下文后本轮结束
    L-->>M: recall（上下文 + 原 boss）
    Note over M: 查历史 / 组装记忆
    M->>L: resume（from=memory：注入记忆后的上下文）
    Note over L: 完全启动：调 LLM API
    L-->>B: （工具异步派发 / 自跳推进下一轮；结束 deliver 直达）
```

### 图片链路（qq → images → llm）

- qq 收到图片消息：把图片 URL 交给 images 服务保存（`save` → `{id}`），`user_message` 携带图片 id（消息 `ImageIDs`）；
- llm 在“完全启动”后、调 API 前：对**最近一条带图消息**的 id 逐个 `fetch`（images 返回 base64），组装为 OpenAI 多模态内容（`image_url`）内联；
- 更早的图片引用不内联，序列化时转文本占位 `[图片]`（继承 v1 行为：仅最近一条内联）；
- images 独立库（SQLite 元数据 + 文件目录），单图上限 32MiB（对齐 DeepSeek 图片限制）。

### 文件链路（qq → files → llm 凭 id 自取）

- qq 收到文件消息：把文件 URL 与文件名交给 files 服务保存（`save` → `{id}`），`user_message` 携带文件 id（`Files` 字段）并在文本里附加占位 `[文件: 名, id=...]`——占位必须带 id：文件不是自动内联的，是模型凭 id 自取；
- `read` 在 `provides` 里声明 `tool: true`，经名册自动成为 LLM 工具（`files__read`），**reasoner 零改动**（同 `remember` 先例）：文本类返回截断文本（`read_max_chars` 默认 4000，`truncated` 显式标记“没给全”）；`.xlsx` 转 TSV、`.docx` 提取正文；二进制 / 旧版 Office 明确拒绝——PDF 抽取与更复杂的 Word 编辑属后续批次，届时只升级 files 内部；
- `fetch`（id → base64 全量）与图片语义同构，v0 链上无消费者，为对称与后续检索/解析服务预留；
- 文件 id 寿命与图片 id 一致：任务窗口内随消息链（`Message.FileIDs`），落库后只剩占位文本（`StorePayload` 只存文本）；
- files 独立库（SQLite 元数据 + 文件目录），单文件上限默认 100MiB（`-max-mb` 可调；文件无模型侧限制）。

**Excel 读写（M9，已实现）**：`read` 的类型判定从"文本类"放宽为"可文本化"——`.xlsx` 经 excelize 逐 sheet 转 TSV（`# 工作表: 名` + 制表符分列），走同一 `read_max_chars` 截断（行边界，不切断整行）；`.xls`（OLE2）明确拒绝并引导另存。新动作 `write`（`tool: true`）：`{id, sheet, edits:[{cell, value}]}` 单元格级编辑，校验链 = id 存在 → 必须 .xlsx → sheet 存在（报错附可用表清单）→ 坐标全量预检（不过则一格不写）→ 写回库内权威副本（`Overwrite` 就地更新，尺寸入台账）；全局写锁串行化 read-modify-write。值 v0 统一字符串；旧格式 / 公式重算 / 样式编辑 / 从零创建均属后续。

**Word 读写（M10，已实现）**：`.docx`（OpenXML）被 `read` 识别为可文本化，按正文段落提取（段落边界截断）；`.doc`（OLE2）明确拒绝并引导另存。新动作 `edit_word`（`tool: true`）：`{id, operations:[{op, old, new}]}`，v0 仅 `op="replace"`，对 `word/document.xml` 做纯文本占位符替换（XML 安全转义，换行/制表符映射为 `<w:br/>` / `<w:tab/>`），预检查所有 `old` 存在后统一写回权威副本；同一个 `old` 出现多次则**全替换**（结果里的 `replaced` 是实际替换处数）；未知 `op` 报错。后续可通过新增 `op` 类型扩展页眉/页脚/段落/表格/样式编辑，无需改动作名。

**就地覆盖的取舍（明写在案）**：`write` / `edit_word` 直接覆盖库内的**唯一权威副本**，**原字节不留**（无副本、无版本、无回收站）——这是有意的：改文件是显式动作（谁改的可从链路日志查），留副本等于把"文件服务"变成"文件版本库"，那是另一个服务的边界。代价说清楚：模型一次调用就能改掉主人的原文件且**不可回退**；要原件请在改动前 `fetch` 一份。想反过来（留副本/记 hash）再议。

**解压封顶（压缩炸弹防线）**：`.xlsx` / `.docx` 都是 zip，文件本体受 `max_mb` 管，但**解压后**的大小不受它管（1MiB 的包能解出 GB 级）。两道防线：zip 目录里的实体大小预检（声明超限即拒）+ 读取时 `io.LimitReader` 兜住"头部撒谎"的包；xlsx 侧把同一上限传给 excelize 的 `UnzipSizeLimit`。上限 256MiB（远大于 `read_max_chars` 默认 4000 字符的实际需要）。

### 通信与性能约定

- 服务间 WS **不做纯长连接**：默认短连接、空闲即断；访问频繁则连接保留时长上涨，冷下来回落——随实际访问频率自适应增减。
- **连接时长可配置**：每个服务可自定义自己的默认连接时长（可作为“可设置项”，由设置中心统一下发）。
- **流式直通**：LLM token 流从 reasoner 一路透传到 QQ / 浏览器，中间不攒批。
- **并行跳转**：一轮内多个跳转并行发出（goroutine），汇合后继续推理。

### 协议格式

**序列化：MessagePack 为主，JSON 保留作调试开关。**

| 方案 | 性能 | 可读性 | 开发负担 | 取舍 |
| ---- | ---- | ------ | -------- | ---- |
| JSON | 基准 | ✅ | 最低 | 调试模式（文本帧） |
| **MessagePack** | 体积小 30–50%、解析快 2–5× | ❌（需工具解码） | 低（与 JSON 同构，无 IDL） | **主选** |
| protobuf / FlatBuffers | 最快之一 | ❌ | 高（schema + 代码生成） | 不选 |
| 自研二进制 | 理论最优 | ❌ | 最高 | 不选 |

- 不选 protobuf 的原因：agent 消息形态多变，强 schema 维护成本大于收益；未来瓶颈链路可单独换格式。
- 落地库：Go `vmihailenco/msgpack`（或 `msgp`）；Python `msgspec`（同库支持 MessagePack + JSON）；TS `msgpackr`。

**帧分层**

- 控制消息 → 二进制帧（MessagePack）；调试模式 → 文本帧（JSON）。
- 大二进制（图片 / 音频）→ 独立二进制帧，不塞进控制消息；超大走分帧。
- LLM token 流 → 专用流帧直通，不套信封。

**信封**（所有控制消息一层壳）

```jsonc
{
  "v": 1,                        // 协议版本
  "id": "01J...",                // 消息 id（请求 → 响应配对）
  "trace": "01J...",             // 链路 id（同一条用户请求的全链共享）
  "boss": "qq",                  // 老板：本跳“面向谁工作”（结果归属；不强制沿用上游）
  "type": "register",            // 消息类型 = 动作（register / jump / ...）
  "from": "reasoner",
  "to": "settings-center",
  "payload": { }                 // 该动作的数据
}
```

#### 消息类型（第一批）

> 信封统一 `v / id / trace / type / from / to / payload`；以下只列 `type` 与 `payload`。

**① `register`**（服务 → 设置中心，请求）——启动即注册：

```jsonc
{
  "v": 1, "id": "01J...", "trace": "01J...", "type": "register",
  "from": "reasoner", "to": "settings-center",
  "payload": {
    "name": "reasoner",
    "addr": "ws://127.0.0.1:9101/ws",        // 自己的位置
    "description": "LLM 单步推理：决定下一步跳转（工具 / 自己 / 审批 / 回复）",
    "provides": [                             // 接受什么输入、有什么输出
      { "action": "user_message", "input": "用户消息（文本/图片）", "output": "流式回复" }
    ],
    "settings": [                             // 可设置项
      { "key": "tool.notify_mode", "default": "each" }
    ]
  }
}
```

**② `response`**（通用，所有请求的回复）——`id` 与请求配对：

```jsonc
{
  "type": "response",
  "payload": { "ok": true, "data": { "roster": [ /* 当前全局清单 */ ], "revision": 3 }, "error": null }
}
```

**③ `roster`**（设置中心 → 各服务，事件）——全局信息分发：

```jsonc
{
  "type": "roster",
  "payload": { "services": [ /* 各服务名片的公开部分 */ ], "revision": 4 }
}
```

**④ `jump`**（任意服务 → 任意服务，请求）——唯一的“访问”动作（用户消息、工具调用都是 jump）：

```jsonc
{
  "type": "jump",
  "payload": { "action": "run", "input": { "command": "ls -la" } }
}
```

**⑤ `heartbeat`**（服务 → 设置中心，事件）——报活：

```jsonc
{ "type": "heartbeat", "payload": { "status": "ok" } }
```

**已定约束**

- 引导只靠配置：设置中心地址 + 自己的名字/地址；其余全靠 `register` 响应与 `roster`（这是“地址发现”的全部）
- `register` 响应直接带当前全局 roster，新服务立刻可互访
- action 命名在服务内唯一（`to` 已指明服务），无需全局前缀
- `provides` 的 input/output 用宽松描述，不做正式 JSON Schema
- `jump` 的目标**可以是自己**：LLM 每一轮推理 = 一次自我跳转（循环的下一次）
- 信封 `trace`：同一条用户请求的全链共享（用户消息 → N 次自跳 → 工具 → 回复），调试 / 审计靠它
- 信封 `boss`：“面向谁工作”的结果归属；每跳自定、**不强制转发、不可丢失**；任务发起者（boss）必须收到最终结果（由链终点以 `deliver` 交付；boss 为空时仅记日志）
- 流式回复：专用流帧带关联 id 推回，`response` 收尾
- 心跳默认 15s；离线判定 = 2 倍间隔超时（已实现；`bye` 告别为立即摘除的优化），设置中心更新 roster 推送（无专门离线消息）
- 拉取全量清单复用 `jump`（`action: "roster"`），不新增消息类型

#### 任务链动作（reasoner）

- `user_message`（链入口）：payload `{text, conversation_id, images, files}`；**发起者必须设 `boss`**；受理后立即回执，最终结果稍后经 `deliver` 送达
- `step`（自跳继续）：payload `{conversation_id}`——上下文在黑板里（任务窗口）；**发出即完**（不等待）
- `deliver`（约定动作，**由 boss 实现**）：payload `{text}`；链终点判定结束直送 boss（交付方同步等回执）
- **工具命名**：`<service>__<action>`——由 roster 的 `provides` 自动生成；**暴露 = provides 里显式声明 `tool: true`**（未声明的动作对模型不可见）；`user_message` / `step` / `deliver` / `recall` / `resume` / `store` / `save` / `fetch` 属链机制动作，调试动作（如 reasoner 的 `echo` / `debug_jump`）同样不暴露（第二道闸）
- **工具异步模型（已实现）**：
  - 派发即回执：每个调用立刻回一条“已受理”`tool` 消息（占住配对位），执行在后台（单次超时默认 60s）；
  - 结果注入：完成后以 `[工具结果] …`（role=user）写入黑板；通知模式 `tool.notify_mode`（`each` 默认逐条唤醒 / `batch` 攒批），设置中心可覆盖（注册下发 + 拉取兜底）；
  - 内置工具 `wait`：阻塞等待全部工具完成；
  - 收尾门禁：仍有工具在跑、或本轮快照过期 → 拒绝收尾（暂存答复 + 注入 `【系统】` 提示）；
  - **任务窗口黑板**：最新消息链 + 在跑工具表（唯一副本）；同会话新消息取代旧任务（在跑结果丢弃）；窗口 = 任务在跑期，交付即释放
- **受理即回执与补报（已实现）**：`provides` 里 `async: true` 的动作，响应只表示已受理（带回 `item_id`）；
  后续进展由该服务以 `report_result` 补报给调用方（`{item_id, phase, text?, data?, final?}`，见 SPEC §6.5）——
  - 受理回执即该次工具调用的“结果位”，补报**不进入**在跑工具表：模型的收尾不被长命令扣住；
  - 补报到达时：任务在跑 → 注入黑板并唤醒；任务已收尾 → 开一轮“补报轮”（system + 补报），模型据此给 boss 补一句（结果不丢）；
  - `item_id` 对不上的补报忽略（不是错误）；`wait` 如实披露“已受理未回报”的条数，但不阻塞等待

#### 记忆动作（memory）

- `recall`（任务上下文托管）：payload `{context: {conversation_id, messages, boss}}`；受理后异步以 `resume` 回发
- `resume`（完全启动，memory → llm）：payload 同构 `{context}`；`messages` 为注入记忆后的上下文——llm 仅接受 `from=memory` 的这一跳来调用 LLM API
- `store`（对话落库）：payload `{conversation_id, role, content}`；幂等（与上一条完全相同则跳过）
- `remember`（长期记忆写入，**在 provides 里**——自动成为 LLM 工具 `memory__remember`，reasoner 零改动）：payload `{kind, content, priority?, conversation_id?}`
  - `priority>0` = 核心记忆（每次对话都注入，不看相关性）；同 `kind+content` 已有 active 条目时幂等跳过
- `forget`（软删，**不在 provides**——不给 LLM 删记忆的按钮；手动 / 直接跳转调用）：payload `{id}`；软删（`status='deleted'`，数据保留可恢复）
- **recall 组装 v0**：单块 `【记忆】`、替换保位；内部三节（空节省略）——
  1. `（最近对话）`：会话库最近 N 条（现有逻辑，去重裁剪不变）；
  2. `（核心记忆）`：`priority>0` 条目（上限 3 条 / 400 字，按优先级 + 新近排序）；
  3. `（相关记忆）`：bigram 子串打分（相邻二字近似分词；**实测 FTS5 trigram 对 1~2 字中文全打空，故不用**）——top 5 / 800 字，同分按 priority、新近。
- **检索位置**：**已实现 M8**——语义召回（embedding 服务，余弦 ≥0.45，阈值来自真模型实测分数带）+ bigram 兜底（服务缺席/超时 3s/解析失败 → 静默回落，不阻塞 recall）；条目向量存 memory 自己的 SQLite（BLOB，派生数据：懒补齐、可重建、换模型失效重算）。
  - 已定语义：**补齐部分失败的条目本轮不参与**（不回落 bigram；日志有记录）——“零可用向量时是否回落”**保持现状，之后决策**。
  - 已修（M9 复核）：Python 端 WS 端点的取消是**连接级**的——一条连接断开只取消它自己在飞的动作（此前跨连接连坐，会误伤并发的 embed 调用）。

**后续批次（占名）**

- `approval_request` / `approval_result`（reasoner ↔ qq/web）：审批挂起与裁决；**shell 的审批接缝已留**（`Approver` 接口 + fail-closed 默认），协议由审批服务定
- `configure`（设置中心 → 服务）：**已实现**（注册即全量下发 + 变更单键下发 + `settings` 事件回传；骨架统一承接）；“调度 = 设置行为、设计工作流”的完整形态待设计
- **后台任务托管**（shell 的第二形态：start / status / logs / kill）：受理即回执的天然形态，本批**未做**（只做前台 `run`）
- **输出有界引用**（PTC 式：大输出留句柄、按需分页取）：本批**未做**——shell 只按 `exec.max_output_kb` 截断并标注总数

### 服务清单（草案 · 语言已定）

> 从 1 的模块映射出的候选服务。**语言（性能优先）**：Go 扛全部逻辑，embedding 拆出用 Python，浏览器必须 TS。服务取舍仍在收尾。

| 候选服务 | 语言 | 职责 | 来自 1 的模块 | 备注 |
| -------- | ---- | ---- | ------------- | ---- |
| **reasoner** | Go | LLM 单步推理与跳转决策（轮次 = 事件驱动 + 自跳衔接；任务窗口黑板） | `agent.py`、`core/`（planner/executor）、`core/models/`、`evaluation/` | **第一批** |
| **settings-center**（设置中心） | Go | 服务注册 · 全局信息 · 设置 · 调度/工作流 | `settings.py`、`config.py` | **第一批** |
| sessions（会话） | Go | 会话历史与上下文持久化 | `api/session_manager.py`、`core/context.py` | 与接入服务的边界待定 |
| memory（记忆） | Go | 会话历史 + 上下文组装（recall/resume/store，完全启动链）；四层记忆/检索/巩固后续批次 | `memory/`（除 embedder）、记忆巩固工具、`core/sleep.py` | **已实现**（第二批） |
| embedding（推理） | Python | 文本 → 向量（onnxruntime + tokenizers，CLS pooling + L2 归一；fake 后端供测试/CI；零存储、零设置项） | `memory/embedder.py`（v1，fastembed） | **已实现**（M8） |
| security（安全审批） | Go | 规则 + LLM 分析 + 裁决；挂起/超时 | `security/` | |
| tools（工具） | Go | 工具注册与执行（Shell 等） | `tools/registry.py`、`tools/builtin/` | 是否按类再拆待定 |
| qq（QQ 接入） | Go | NapCat/OneBot 11 正向 WS；私聊（零信任）→ 任务链；deliver → 回发 | `plugins/builtin/napcat_plugin.py` | **已实现**（仅私聊；群聊/审批后续） |
| images（图片） | Go | 图片保存（URL/base64）+ 按需转 base64 发回（llm 取图内联） | （v1 图片直通部分） | **已实现** |
| files（文件） | Go | 文件保存（URL/base64）+ fetch 全量取回 + read 文本截取（`tool: true`，LLM 凭 id 自取）+ write 单元格级 Excel 编辑 | `assets.py`（存取部分） | **已实现**（含 Excel 读写；索引/检索与 PDF/Office 抽取后续） |
| **shell（命令执行）** | Go | 受理即回执的命令执行：审批接缝（未接入 = 一律未通过）+ 并行/队列/超时终止/输出截断；结果由 `report_result` 补报 | `tools/builtin/`（shell 部分） | **已实现**（审批协议等审批服务定；白名单策略不在本服务） |
| web（Web 接入） | Go | 浏览器 REST/SSE 接入（不带前端） | `api/` | UI 归 frontend 服务 |
| frontend（前端服务） | TS | 设置中心可视化面板：注册为服务节点（心跳 / 名册广播）；浏览器 HTTP JSON + SSE；设置查看 / 修改 / 重置（中心受理并下发） | `frontend/` | **已实现 v1**（错误上报视图随 v2） |
| assets（资产） | Go | 文件资产库、索引、检索 | `assets.py` | |
| skills（技能） | 并入 reasoner | 技能创建 / 加载 | `skills/` | |
| smart-home（智能家居） | Go | 家居控制后端 | `smart_home/` | 或并入 tools |
| search（搜索） | 无服务 | 搜索接入 | `tools/mcp/` + searxng | searxng 已是独立容器 |

> 决策注记：全 Go 的理由——并发模型 / 内存 / 长连接吞吐全面占优；v1 横竖要重写，无搬运成本优势。embedding 保留 Python 因 ONNX 生态（与 Rust 同引擎、速度等效）。

### 项目结构（目录约定）

- **一个文件夹 = 一个服务**：每个服务独立成文件夹。
- `protocol/` 与 `service/` 是两个**库**（非服务）：前者是协议实现，后者是服务骨架（注册 / 心跳 / 名册 / 断线自愈 / 优雅下线）。
- 每个服务**自带 `tests/`**：服务自身的测试跟着服务走。
- 根目录有**总 `tests/`**：跨服务的集成/端到端测试——拉起相关服务、走真实 WS 通信，验证“服务隔离 + 网络互访”（例：设置中心离线时，其余服务照常工作）。

```text
2/
├── <服务1>/         # 一个文件夹 = 一个服务
│   └── tests/       # 服务自带测试
├── <服务2>/
│   └── tests/
└── tests/           # 总测试（跨服务）
```

## 4. 数据与存储

- 各服务**独立建数据库**，数据归属到服务自身；服务之间不共享库表。
- memory：SQLite（`data/memory.db`）——`messages` 会话消息表 + `memories` 长期记忆表（v0：kind / content / priority / source / conversation_id / status 软删 / 时间戳）；**两表共享同一 DB 句柄**（同进程同库，跨表事务可行）。
- images：SQLite 元数据（`data/images.db`）+ 图片文件目录（`data/images/`）。
- files：SQLite 元数据（`data/files.db`，含 `filename` 列——文件与图片的结构差异）+ 文件目录（`data/files/`）。
- settings-center：`data/settings.json`——设置覆盖值（预设值由各服务声明，不落盘）。
- （细化待定：其余服务存什么、跨服务数据如何流转）

## 5. 从 1 的取舍清单

<!-- 逐模块过一遍 1 的实现，确定去向；填表时机：服务清单确定后 -->

| 模块 | 保留 | 重写 | 丢弃 | 理由 |
| ---- | ---- | ---- | ---- | ---- |
|      |      |      |      |      |

## 6. 实施路线

- **第一批**：reasoner + 设置中心（纯后端，不带前端）
- **第二批**：memory（记忆服务）——recall / resume / store 与完全启动链
- **第三批**：qq + images（第一个真实入口）——QQ 私聊 → 任务链 → 回发；图片存取链路

## 7. 待决问题

- 跳转的超时与失败、连接保活的自适应阈值与公式（“具体再看”）。
- 记忆缺席/失败：当前为“硬门”（链停在原地、日志可见，无重试）；重试 / 降级策略待细化。
- qq 后续批次：群聊（@ 必回 / 自主插话 / 群聊记录）、审批推送与数字裁决。
- 图片：清理策略（保留期/容量）、去重；主动发送图片（表情包）能力。
- 文件：PDF 抽取（read 升级；Excel/Word 已 M9/M10 实现）、清理策略（保留期/容量）、去重；主动向用户发文件（deliver 文件）。
- 设置键命名（待改进，非违规）：files 的 `read_max_chars` 是裸键（与 `max_mb`/`owner`/`sweep_ms` 同族），与 SPEC §13 的 `<域>.<名>` 格式不完全一致。若统一为 `read.max_chars`：属"改名"，须按 §13 登记替代名与迁移说明；改名最佳时机是 M8 合并前（键未发布、迁移负担为零），合并后再改则需真实迁移流程。
- 服务清单收尾：tools 是否再拆（sessions 已并入 memory）。
- 设置 / 管理类动作是否暴露为 LLM 工具：当前靠“不写进 `provides`”规避；倾向改为 `provides` 显式声明才暴露——reasoner 的硬编码排除表覆盖不到第三方服务（见 [SPEC.md](./SPEC.md) §10）。
- 链级失败策略：拨号失败已安全重试（+0.5s/+2s）；“送达后失败”需幂等键才能重试；失败上报 boss（降级回复）待定。
