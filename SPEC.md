# SUMP 服务规范（草案 v0.1）

> **读者**：要给 SUMP 写一个新服务（= 一个插件）的人，不限语言。
> **状态**：草案——标注【待定】的条目尚未拍板，实现前先讨论。
> **理念**：**一切皆服务**（对应 DeepSeek Harness 的“一切皆插件”）。
> **相关**：[DESIGN.md](./DESIGN.md)（内部重构设计）、[README.md](./README.md)（项目总览）

---

## 0. 一句话

SUMP 是“服务的互联网”：每个服务是一个独立进程、独立数据库、按地址可达的 WS 端点；
**智能体循环不存在于任何服务内部**，而是 LLM 服务在网络上的自我跳转链。

---

## 1. 核心理念：一切皆服务（一切皆插件）

- **内核极小**：世界里只有两样“基础设施”——**协议**（怎么说话）与**设置中心**（谁在哪、有什么设置）。没有特权服务、没有中央交换机。
- **一切能力皆插件**：LLM 推理、记忆、图片、QQ/微信接入、浏览器前端、工具、搜索、审批、智能家居……全部是服务，**独立进程 / 独立库 / 可单独替换**。
- 由此得到三条硬规则：
  1. **扩展不需要修改**：新增一个服务、或替换掉一个现有服务，**不得要求改动任何其他服务的代码**。
  2. **唯一共享物是协议**：服务之间不共享进程内状态、不共享库表；跨服务的一切都走消息。
  3. **能力可替换**：只要服务名与动作语义一致，任何语言、任何实现的同名服务都能顶替（例：换一个 `memory` 实现，其余服务无需感知）。

> 判断一个改动是否“合规”的最快问法：**它需要别的服务配合修改吗？** 需要，就是设计走偏了。

---

## 2. 独立性硬约束

1. **独立进程**：可单独启动 / 停止 / 重启；除“设置中心地址”这一条引导配置外，不依赖任何其他服务处于运行状态。
2. **独立存储**：只读写自己的库与目录；**不得**直接访问其他服务的库表或文件。派生数据（缓存 / 向量 / 索引）必须可丢弃、可重建——**权威数据只有一份**，且在它的归属服务里。
3. **只经网络访问**：按地址点对点直连；不经过任何中转。
4. **不驻留链状态**：跨步状态原则上随消息携带；唯一例外是 reasoner 的**任务窗口黑板**——进行中任务的最新消息链与在跑工具表保存在进程内（窗口 = 任务在跑期：交付 / 被取代即释放；不落盘、不跨重启；见 §6.4）。
5. **语言自由**：任何语言都可实现。`protocol/`（Go 包）是本规范的**参考实现之一**，不是规范本身。

---

## 3. 引导与生命周期

引导只靠配置：**自己的名字、自己的监听地址、设置中心地址**（其余是服务自己的业务配置）。

```
启动 → 监听 WS → 向设置中心 register（首次注册同步完成；中心不可达则启动失败）
     → 每 15s 发 heartbeat（间隔随注册名片声明 heartbeat_ms，中心据此判定离线）
     → 订阅 roster 事件（名册有更新时替换本地通讯录）
     → 受理 jump（按动作分派 → 回 response）
     → 退出前发一条 status="bye" 的心跳（中心立即摘除名册）
运行中断线：指数退避（1s → … → 30s 上限）重连并重新注册（名片无变化不广播——重连去重）
离线判定（中心侧）：2 × 声明心跳未见 → 摘除名册并广播；心跳晚到可从墓碑复归
```

- 服务名：小写字母开头的短名（`memory` / `images` / `qq` / `reasoner`）。
- 地址：`ws://host:port/ws`。
- 重复注册必须幂等（同一个名字重注册 = 更新名片）。

---

## 4. 传输、帧与信封

- **传输**：WebSocket；**编码**：MessagePack（二进制帧）。JSON 保留作未来调试开关。
- **访问模型**：默认短连接、用完即走；只有“工具调用那一跳”等响应（其余发出即完）。

**信封**（所有控制消息一层壳）：

```jsonc
{
  "v": 1,                 // 协议版本
  "id": "01J...",         // 消息 id（请求 ↔ 响应配对）
  "trace": "01J...",      // 链路 id（同一条用户请求的全链共享）
  "boss": "qq",           // 本跳“面向谁工作”（结果归属；逐跳自定、不可丢失）
  "type": "jump",         // 消息类型 = 动作
  "from": "qq",
  "to": "reasoner",
  "payload": { }          // 该动作的数据
}
```

---

## 5. 消息类型

| type | 方向 | 语义 |
| ---- | ---- | ---- |
| `register` | 服务 → 设置中心 | 注册（附名片）；**响应里带全量名册** |
| `response` | 任何请求的回复 | `{ok, data, error}`，`id` 与请求配对 |
| `roster` | 设置中心 → 各服务 | 全局清单推送（事件） |
| `heartbeat` | 服务 → 设置中心 | 报活（默认 15s） |
| `jump` | 任何服务 → 任何服务 | **唯一的“访问”动作**；目标可以是自己（自跳 = 循环的下一步） |
| `settings` | 设置中心 → 各服务 | 设置变更与下发结果（事件） |

拉取名册复用 `jump`（`action: "roster"`）；设置中心另提供 `ping`（连通性）、`report`（错误上报，见 §7）与设置动作（见 §7）。

---

## 6. 动作词汇与工具暴露

### 6.1 保留动作名（链机制，语义不可改）

`user_message`（链入口）/ `step`（自跳继续）/ `deliver`（终点直达 boss）/
`recall` `resume` `store`（记忆往返）/ `save` `fetch`（图片/文件链）/ `report_result`（异步受理的补报，§6.5）。

这些是**协议级词汇**：即便某个服务提供了同名动作，也**不作为 LLM 工具**暴露。
另有**骨架保留动作** `configure`（设置下发专用，由 `service/` 骨架统一承接，见 §7）——同样不进 `provides`、不作 LLM 工具。

### 6.2 服务自定义动作

在服务内唯一即可（`to` 已指明服务，不需要全局前缀）。命名：小写 + 下划线。

### 6.3 工具名映射与暴露规则

LLM 工具名 = **`<service>__<action>`**（由 reasoner 依据名册 `provides` 生成）。

**暴露 = 显式声明（已实现）**：动作要在 `provides` 里声明 **`tool: true`** 才暴露给 LLM；
未声明的动作只可被“直接跳转”访问，对模型不可见。另有两道硬闸（即便声明也不暴露）：

1. 链机制保留动作（§6.1 的 `user_message` / `step` / `deliver` / `recall` / `resume` / `store` / `save` / `fetch`）；
2. 调试 / 测试动作（如 reasoner 的 `echo` / `debug_jump`）。

两闸同时作用于**执行面**：模型若仍指名调用未提供（或保留动作）的工具，
reasoner 在执行前直接拒绝（错误作为工具结果回给模型——不会发生真实跳转）。

> 为什么必须显式声明：第三方服务的动作无法被 reasoner 的硬编码表覆盖，必须由服务自己表态。

**工具并发**：同一会话内的工具调用并发执行；reasoner 侧默认单会话上限 10（可配置，-1 = 不限制），超限排队。同一服务的多个调用可并发重复进行，不做去重。

**给新服务的建议**：管理 / 调试类动作（改配置、清库、发号施令型代理）**默认不要暴露**给 LLM；
能力类动作（查、算、取、发）才是给模型的。

### 6.4 工具异步模型（reasoner 侧，已实现）

- **派发即回执**：LLM 发起工具调用后，立即为每个调用记一条“已受理”`tool` 回执（占住 OpenAI 协议要求的 `tool_calls` 配对位），执行转入后台（单次超时默认 60s）。
- **结果注入**：工具完成后，结果作为 `[工具结果] <名>（job=<id>）` 消息（role=user）注入黑板；
  - 通知模式 = 设置项 **`tool.notify_mode`**：`each`（默认，逐条唤醒）/ `batch`（攒批唤醒，等最后一个结果）；设置中心可覆盖（reasoner 启动时拉取生效）。
- **内置工具 `wait`**：阻塞等待本会话全部工具任务完成（上限 5 分钟）；结果未到齐、不能收尾时使用。
- **收尾门禁**：最终答复被接受的前提 = 没有工具在跑，且本轮快照未过期；否则拒绝收尾（暂存模型的答复 + 注入 `【系统】` 提示），等下一轮重新收尾。
- **黑板（任务窗口）**：进行中任务的最新消息链 + 在跑工具表存于 reasoner 进程内（唯一副本）；任务被同会话新消息取代时旧任务关闭、在跑结果丢弃（日志可见）。窗口 = 任务在跑期，交付即释放——不落盘、不跨重启（这是 §2.4 的唯一例外）。
- **轮次衔接**：`step`（自跳）payload 只带 `conversation_id`（上下文在黑板里）；轮次由事件驱动（派发 / 结果注入 / 等待 / 收尾门禁），不驻留循环。

### 6.5 受理即回执与补报（`async`，已实现）

有些动作的耗时不可预期（跑一条命令、跑一次训练）——让调用方等结果既会撞上工具超时，也违反“边界”：等待不该跨服务传递。

**声明**：`provides` 里 `async: true` 表示该动作**受理即回执**——响应只表示“已受理”，并给一个句柄：

```jsonc
// 调用方 → 服务（jump）
{ "action": "run", "input": { "command": "ls -la" } }
// 服务的响应（只表示已受理）
{ "ok": true, "data": { "item_id": "01J…", "phase": "queued" } }
```

**补报**：后续每条进展都是服务 → 调用方的一次独立 `jump`（`action: "report_result"`，`to` = 原先的调用方）：

```jsonc
{ "item_id": "01J…", "phase": "approved", "text": "审批通过，已开始执行\n$ ls -la", "final": false }
{ "item_id": "01J…", "phase": "done", "text": "命令执行完成（退出码 0，耗时 12ms）…", "final": true,
  "data": { "exit_code": 0, "stdout": "…", "stderr": "", "duration_ms": 12 } }
```

- `item_id`：受理回执里的句柄（调用方据此把补报配回自己的记账）；补报的 `phase` 与 `data` 内容由服务自定；
  受理回执里的 `phase` 与补报同一词汇（受理就是第一个阶段；shell 当前：`queued`）；
  `text` 是可直接阅读的正文（调用方据此注入上下文 / 呈现）；`final: true` = 该次受理的最后一条。
- **零阻塞**：受理回执不等审批、不等执行；补报各自独立一跳；调用方离线 = 补报发不出去（服务只记日志，不排队补发）。
- 调用方缺失句柄对应关系时的补报（重复 / 服务重启过）**忽略即可**，不是错误。
- **reasoner 侧行为**：受理回执即该次工具调用的结果位（配对已闭合），补报不进入任务的“在跑工具”表——
  模型的收尾不该被一条长命令扣住；补报到达时：任务在跑 → 注入黑板并唤醒；任务已收尾 → 以补报开一轮“补报轮”
  （结果不丢，模型据此给 boss 补一句，boss 为空则记日志丢弃）。内置工具 `wait` 会如实披露“已受理未回报”的条数，但不阻塞等待它们。
- **与“工具超时”的关系**：异步动作的受理是瞬时动作，单次工具超时（默认 60s）不再约束实际执行时长（由服务自己的执行超时管）。

---

## 7. 名片：`provides` 与 `settings`

```jsonc
{
  "name": "images",
  "addr": "ws://127.0.0.1:9401/ws",
  "description": "图片服务：保存图片（URL/base64），按需转 base64 发回调用方",
  "provides": [                                  // 我对外提供哪些动作（"tool": true 才作为 LLM 工具暴露；"async": true = 受理即回执，见 §6.5）
    { "action": "save",  "input": "{url} 或 {data(base64), mime}", "output": "{id, mime, size}" },
    { "action": "fetch", "input": "{id}", "output": "{id, mime, data(base64), size}" }
  ],
  "settings": [                                  // 我可被设置哪些项（预设值 = 本服务启动引导值；只声明真正消费的项）
    { "key": "max_mb", "default": "32",                                      // 普通项：值由中心保管
      "label": "单张图片大小上限", "kind": "number", "unit": "MB" },          // 人话元数据（可选）：怎么显示、用什么控件
    { "key": "llm.key", "secret": true, "set": false },                      // 秘密项：值由服务自己持有
    { "key": "addr", "default": "127.0.0.1:9101", "readonly": true }         // 只读项：启动参数带起来的
  ]
}
```

- `provides[].input/output` 是**给人看的宽松描述**，不是 JSON Schema。
  精确的字段形状由协议包里的载荷类型定义（`protocol/payloads.go`）。
- `settings[]` 的**人话元数据**全部可选——服务不给，面板就退回"键名 + 文本框"：

| 字段 | 用途 |
| ---- | ---- |
| `label` | 中文名。面板优先取语言包 `setting.<service>.<key>.label`，再回落这里 |
| `hint` | 一句话说明：改它会发生什么、留空意味着什么 |
| `kind` | 控件类型：`bool` / `enum` / `number` / `text` / `textarea` / `service` / `optional-service`（缺省 `text`） |
| `options` | `enum` 的候选**原值**；标签走语言包 `setting.<service>.<key>.opt.<value>`，缺了就显示原值 |
| `unit` | `number` 的单位**符号**：`ms` / `MB` / `KB` / `chars` / `count`。面板翻成人话并在值旁给读法（`300000 ms → ≈ 5 分钟`）；服务收到的仍是原始数值 |

`service` 表示值是一个服务名：面板从名册里列候选，但**名册只是候选，校验权仍在服务**
（名册里没有的名字照样提交，服务自己拒）。`optional-service` 表示该项可以"没有"
（清空 = 关闭这项能力），面板画成「开关 + 选择」，关掉提交空串、打开回落声明预设。
- `settings[]` 声明“可设置项 + 预设值”（线路字段名沿用 `default`；预设值 = 服务自己给的引导值，可在重启后变化）。设置中心保管并提供动作：

| 动作（jump 到 `settings-center`） | 输入 | 输出 |
| ---- | ---- | ---- |
| `list_settings` | `{service?}`（可选过滤） | `{services:[{service, settings:[{key,value,default,overridden,secret?,readonly?,set?}]}]}` |
| `set_setting` | `{service, key, value}` | 被改动服务的设置（同上结构） |
| `reset_setting` | `{service, key}` | 同上（回落预设值；秘密项 = 让服务清空） |
| `report` | `{kind?, message}` | `{status:"ok"}`（中心记日志并交给消费挂点） |

- 生效值 = **覆盖值 ?? 服务声明的预设值**；覆盖值落盘于设置中心（`data/settings.json`）。
- 校验两级：中心只校验“服务已注册、设置项已被声明”；**值是否合法由服务自己校验**——
  `configure` 被服务拒绝即保持旧值（中心把拒绝理由记入结果事件）。
- **下发（注册即对齐 + 变更单键）**：中心在 `register` 响应发出后**异步**（不阻塞注册、不等待送达）
  把该服务的普通项生效值逐键以 `configure` 跳转下发（有界重试 +0.5s、+2s；服务明确拒绝不重试）；
  `set_setting` / `reset_setting` 成功后单键下发。**受理回执只表示“已落盘受理”**，送达结果
  （含失败原因）以 `settings` 事件广播：`{cause, service, results:[{key, value?, ok, error?}]}`
  （cause = `register` / `set_setting` / `reset_setting`）——面板据此自动刷新与提示。
- **服务侧接收（骨架通用）**：Go 骨架 `service/` 提供 `HandleSetting(键, 预设, 校验+应用回调)`——
  声明进名片、统一承接 `configure`、注册成功后**异步拉取一次**作兜底（只应用覆盖项；与下发幂等）；
  另暴露 `RefreshSettings()` 手动拉取。预设值由**服务自己决定**（库不规定来源）。
  非 Go 服务照此实现；**服务之间不新增任何设置同步通道**（设置只走服务 ↔ 中心）。
- **离线一致性**：服务离线期间的变更**不排队补发**——其下次注册时全量对齐（幂等收敛）。

### 7.1 三类设置项：普通 / 秘密 / 只读

- **普通项**：值由中心保管（覆盖值落盘中心），注册时全量对齐下发。
- **秘密项**（`secret: true`，如 API key、token）：值**不进中心**——中心不落盘、不回显
  （`list_settings` 里 `value` 恒空），只在面板提交那一次把值**转发**给服务；由**用它的服务自己**
  持久化（骨架 `HandleSecret` + `Config.SecretFile`，默认 `data/<name>-secret.json`；`"-"` = 不落盘），
  服务 `Start()` 时自己加载并应用。名片里 `set` 只说明「服务侧当前有没有值」（面板据此显示「已设置」，
  值本身永不上线）；**注册对齐不含秘密项**（中心手里没有值，推个空过去只会把服务那份抹掉）；
  `reset_setting` = 推空值过去让服务自己清。一句话：**谁消费谁持有**。
- **只读项**（`readonly: true`，含 `-name` / `-addr` / `-center` 这类引导三要素，以及各服务的存储身份
  `db` / `dir` / 中心的 `settings`、门户的 `web`）：进面板只读展示
  （值 = 启动值，放在 `default`），`set_setting` / `reset_setting` 一律明确拒绝——**改它要重启**
  （重启按钮另做）。**路径类参数默认只做只读**：运行中换库/换目录等于让在途读写换个落点，不做。
- **面板语义**：秘密项显示「已设置 / 未设置」+ 覆盖（密码框）+ 清空；只读项显示启动值、无修改入口。
- **安全边界（说清楚，不假装）**：秘密文件是**明文**（本机单用户场景，等同 `.env`；写时目录 0700、
  文件 0600、原子替换）；面板提交那一次的值走的是中心 ↔ 服务之间既有的本机 WS 通道（与其它协议流量同级）。
  中心侧全程不留值：不落盘、不回显、日志掩码、`settings` 事件不带值。
- 不做 `env:` 之类二级引用写法：**不叠机制**，要换值就在面板覆盖一次。

### 7.2 服务自报：服务侧的值自己变了

控制方向只有一条（中心 → 服务 `configure`）；反方向的 `report_setting` **只报事实**：

| 动作（jump 到 `settings-center`） | 输入 | 输出 |
| ---- | ---- | ---- |
| `report_setting` | `{service, key, value, set?, seq}` | 被改动服务的设置（同上结构） |

- 服务侧的值因自身原因变化时（自适应 / 规范化 / 程序内部改写），服务主动报一条——骨架 API
  `AnnounceSetting(键)`：异步发出（**不阻塞**服务）、失败不排队（下次注册名片对齐收敛）。
- 中心**不改**自己手里的覆盖值，只把这条记进「服务侧实际值」那一列：面板同时看得见
  **中心口径的生效值**（`value`）与**服务侧实际值**（`actual`），不一致时给一个「服务侧已变」标记。
  （`report_setting` 的下发成功那一步等价于服务侧已确认：此时两列相等，不报标记。）
- **秘密项 `value` 恒空**：只回 `set`（服务侧有没有值），值永远不上线。
- `seq` 服务内单调递增：中心丢弃比已记下更小的旧报（回传与补报的到达顺序都不保证）。
- 服务侧实际值是**运行时态**：不落盘（中心重启后为空，等服务下次回传或注册对齐）。

### 7.3 会话记录：对话 + 过程轨迹（已实现）

记忆服务（memory）保管的不只是"用户说了什么 / 智能体答了什么"：

| `kind` | 内容 | 谁写 |
| ---- | ---- | ---- |
| `user` / `assistant` | 对话本身 | reasoner（链入口 / 交付成功后） |
| `reasoning` | 模型这一轮的思考（`reasoning_content`） | reasoner（每轮有思考就记一条） |
| `tool_call` | 工具调用（shell 类工具直接给出 `command`） | reasoner（派发时，**执行前**落库） |
| `tool_result` | 工具输出（异步工具取**终态补报**的文本，不是"已排队"回执） | reasoner（结果入账时） |

- **只有 `user` / `assistant` 进 LLM 上下文**（`recall` 的 `Recent` 过滤）：思考回灌纯浪费，
  工具结果缺少配对的 `tool_calls` 会被上游直接拒。轨迹是**给人看**的。
- **`seq` 由写入方给**（微秒时间戳，跨重启仍递增）：`Fire` 是 goroutine，落地顺序不保证，
  `seq` 保证"命令在输出之前"这种真实先后；门户也用它去重。**不能超过 2^53**——
  它要经 JSON 到浏览器，纳秒级会被 JS 的 number 抹平，相邻记录可能撞成同一个数。
- 单条上限 8000 字符，超出标 `meta.truncated = true` + `full_chars`（终端输出上限是 256KB，原样存会撑爆库和界面）。
- 对话消息仍幂等去重（连续完全相同 = 重复交付）；**事件类记录不去重**——连跑两条一样的命令是两件事。
- 老库平滑升级：`kind` / `meta` / `seq` 三列按需 `ALTER TABLE`，老数据 `kind` 按 `role` 推断、`seq` 用主键回填。

| 动作（jump 到 `memory`） | 输入 | 输出 |
| ---- | ---- | ---- |
| `store` | `{conversation_id, boss?, role, content, kind?, meta?, seq?, task_id?}` | `{}`（尽力落库；失败只记日志） |
| `history` | `{conversation_id, limit?, after_seq?}` | `{conversation_id, messages:[{id, seq, role, kind, content, meta, at}]}` |

- `after_seq` 是**增量拉取**：只回序号更大的记录（门户重连/回填不必每次全量重来）。
- **实况推送**：reasoner 在落库的同时，若入口方（boss）在 `provides` 里声明了 `progress`，
  就推一条 `{conversation_id, seq, kind, content, meta, at}`——界面进行中就能看见思考与命令，
  不必等最终答复。**没声明的不推**（QQ 之类），否则对方只会回 unknown action。
  记忆是真相，`progress` 只是"别等最后才看见"：丢一条不影响历史。

### 7.4 任务：agent 在任务里工作（已实现）

任务是**一段有名字、有始有终的工作**，会话记录挂在任务下；历史列表的标题就是任务名。

| 动作（jump 到 `memory`） | 输入 | 输出 |
| ---- | ---- | ---- |
| `task_current` | `{conversation_id}` | `{task?}`（最近活跃的 active 任务；没有就是空） |
| `task_open` | `{conversation_id, name}` | `{task}` |
| `task_rename` | `{task_id, name}` | `{task}` |
| `task_finish` | `{task_id, name?}` | `{task}`（status=done，之后只读） |
| `task_resume` | `{task_id}` | `{task, messages[]}`（把未完结任务顶成当前，记录一并带走） |
| `task_list` | `{conversation_id, limit?}` | `{tasks[]}`（最近活跃在前） |

- **没有任务必须创建**：`recall` 里先 `EnsureTask`——当前没有 active 任务就以这次用户消息命名建一个，
  随 `resume` 带回 reasoner。拿不到任务，这一轮不该开工。
- **一个会话最多一个 active 任务**：靠 SQLite 部分唯一索引
  `CREATE UNIQUE INDEX ... ON tasks(conversation_id) WHERE status='active'` 保证；
  并发下建不上的那一方回头读现有的那个（任务不会分叉）。
- **完成判定权在 reasoner**：内置工具 `task_status{status: done|continue, title?, note?}`，
  收尾门禁要求交付前必须调用一次（没判就催一轮，最多两次；两次都不判按 continue 记账）。
  `done` → `task_finish`（顺带用 `title` 把任务名改准）；`continue` → `task_rename`（任务保持进行中）。
- **完结 = 不再续**：已完结任务不接受继续（`task_resume` 明确拒绝）。但它的记录还在会话里：
  门户按**任务分节**连续展示整段会话，侧栏是"会话 → 任务"的目录，点任务只是跳到那一段（不切换视图、不过滤）。
  **未完结 = 可恢复**：它的记录都还在（`task_id` 一列），点开就回到那一段继续。
- **记录归属**：`store` 带 `task_id`；没带时——`user` 记录会触发 `EnsureTask`，
  其他记录（答复/轨迹）挂到**最近一个任务**上，绝不拿它们的内容当任务名。
- **LLM 上下文按任务取**：`recall` 只取当前任务的对话消息（`Recent(..., taskID)`），换任务即换上下文。
- **老数据**：迁移时每个会话补一个已完结的「历史记录」任务，旧消息全部挂上去（不丢历史）。
- 任务现在**只存事实**（名字/状态/归属），不解释语义。要独立成服务时，把 `tasks` 表与这六个动作整体搬走即可。

#### 会话本身也是一行（`conversations`）

任务是"会话里的一段工作"，**会话**是比它更外一层的东西：能起名字、能分入口、能归档。
没有这张表，历史记录页只能靠 `GROUP BY conversation_id` 现凑——起不了名，多会话也无从谈起。

| 动作（jump 到 `memory`） | 输入 | 输出 |
| ---- | ---- | ---- |
| `conversation_list` | `{limit?, include_archived?}` | `{conversations:[{id, title, entry, status, created_at, updated_at}]}` |
| `conversation_rename` | `{id, title}`（空串 = 清掉名字） | `{conversation}` |

- **只存元信息**：`title` / `entry` / `status` / 两个时间。消息数、任务数、最后一句说什么全部现算——
  冗余字段迟早和真相打架。
- **写入即建行**：`store` 落库成功后自动 upsert（`Record` 内部），谁都不用显式建会话。
  只有落库**成功**才推 `updated_at`，否则列表里会出现"刚活跃过"的空会话。
- **`entry` 取 boss 不取 `env.From`**：记录是 reasoner 代写的，真正"从哪扇门进来"的是它替谁办事
  （`frontend` / `qq`）。只在空时填，后来的不覆盖——第一次从哪进来就是哪。
- **`title` 为空是真的空**，不造假名字：界面自己回退一个占位。起名是件明确的事（`conversation_rename`），
  不靠"顺带"。
- **老库回填**：从 `messages` / `tasks` 里已有的 `conversation_id` 聚合出会话行（首末时间为界），
  升级后历史记录页立刻能用。
- 两个动作都**不进 `provides`**：界面导航不是模型该自己做的决策。

### 7.5 经验网：把做过的事变成能"抓线头"的经验（已实现）

会话记录是**流水**（按时间、给人看）；经验网是**联想**（按语义、给模型用）。两者永不混用：
历史记录页只读流水的表，经验网只给 recall 与 `dig`。

实现是一个**独立 Go 包** `memory/memnet`：零依赖本仓库其他包、只吃一个 `*sql.DB`，
表与阈值都是它自己的——将来整块搬走即成独立仓库 / 独立服务。细则见
[memory/memnet/README.md](./memory/memnet/README.md)。

| 动作（jump 到 `memory`） | 输入 | 输出 |
| ---- | ---- | ---- |
| `note` | `{content, conversation_id?, task_id?, durability?}` | `{id, node_ids, duplicate, skipped}` |
| `dig`（`tool: true`） | `{entry_id?, node_id?, limit?}` | `{notes[], total}` |
| `lineheads` | `{query, limit?}` | `{heads:[{node_id, text, count, score, task_id, members}]}` |
| `prune` | `{}` | `{entries, links}` |

- **一条经验 = 一句话**（总结 / 用了什么技术 / 踩了什么坑，各是一条），带 `task_id` 指回当时的完整存档。
- **树只有一层**：偏好 / 事实 / 做法 / 坑 / 取舍五个大类，把种子句的向量当定义，**永不随成员重算**
  （否则五个大类会彼此趋同）。第二层不落库——持久化二级枝会被"成员均值"带跑（质心融化，实测可见）。
- **网在归属上**：一条经验可同时挂多个大类（`mem_links`，带粗细），`MaxLinks` 控制上限。
- **线头是派生的**：`recall` 里先全库按相关度捞，再把候选**现算**成簇，每簇一行
  `[N 条 · 分数] 代表句（id=…，来自 task-…）`。糊了下次换 query 重新聚，不会永久长歪。
- **比较前必减语料均值**：短句向量各向异性，原始余弦的同义 / 无关几乎重叠（间隔 +0.068），
  中心化后 +0.216——这是阈值能用的前提。均值是派生缓存，不进库。
- **遗忘曲线只作用在条目上**：`S = S₀ × growth^(hits-1)`、`R = exp(-Δt/S)`，排序用 `cos × R`。
  复现靠"又被观察到"，**不靠被召回**（否则形成回音室）。`core` 不衰减，`transient` 不进网。
- **剪枝是软的**：`R` 跌破线只标 `pruned`（不删行、不删向量、留着强度），重新遇到能复活。
- **整理时机不做死在服务里**：`note` 是纯动作，谁调都行（收尾后 / 定时 / 门户手动）。

### 7.6 复盘与单步推理：模型调用只有一个出口（已实现）

**所有非任务型的模型调用都走 llm 服务**（reasoner 本身就是那个服务）：
别的服务不配自己的 LLM 客户端、不散落 Key，一律 jump 到 `think`。

| 动作（jump 到 `reasoner`） | 输入 | 输出 |
| ---- | ---- | ---- |
| `think` | `{prompt, system?, model?, max_tokens?}` | `{text}` |

- **一次补全，来去干净**：不带工具、不开任务、不进黑板、不写记忆。与任务链零交集。
- **不进 `provides`**：不给 LLM "让自己再想一次"的按钮——那是无限自我循环的起点。
- `model` 非空即覆盖默认模型（"最小不思考"= 指一个非推理模型）；`max_tokens` 硬夹到 4096；
  提示词超 20 万字符直接拒（一个误拼的循环能把账单打飞）。

**复盘（交付后写经验）**：任务交付 boss **之后**在后台跑一次，把这一段做过的事变成经验网的条目
（`note`）。老板已经拿到答复，复盘成败都不影响他。

- **何时**：交付成功且任务没被新消息取代（被打断的半截工作不是可复用经验，是噪音）。
- **谁写**：llm 服务自己（本节的 `think` 同一条路，只是预置了提示词）。
- **写什么**：做法 / 坑 / 事实 / 偏好 / 取舍，一条一句话、独立可读；**允许 0 条**（宁缺勿滥），上限 3 条。
- **耐用度不由模型定**：`kind=偏好` → `core`（不衰减），其余 `durable`。让模型自己挑，它会全标 core，
  网就再也不遗忘了。
- **不设"太短就不复盘"的门槛**：最短的最值钱——"以后都用中文回复"只有十个字，却必须记住。
  长度门槛会精准筛掉这类经验；宁可让模型看一眼再自己说"没有值得记的"。
- **开关**：设置项 `reflect.on`（启动值 `-reflect`，默认开；关掉后已写进网的不会删）。

**长任务怎么塞进一次复盘请求**（借用 opencode 与 codex 的同一套形状，2026-10 版）：

1. 不把原始消息链丢给总结模型，先序列化成带角色前缀的流水（`[用户]` / `[助手]` / `[工具调用]` / `[工具输出]`）。
2. **工具输出是体积来源，先砍**：单条截到 1250 字（opencode `TOOL_OUTPUT_MAX_CHARS = 1_250`）。
3. **最近的逐字保留，早的骨架化**：最近 8k token 原样留（opencode 现在默认 15k，它要能让下一个人接着干，
   我们只要看清最近这一段）；更早的用户消息留 500 字（用户说的话是需求本身），助手留 300 字，
   工具输出留 200 字——**不能全丢**，报错原文是"坑"的唯一证据。
4. **切分点对齐到轮次**（opencode 的 `recentStart`）：一轮要么整轮逐字、要么整轮骨架化，
   绝不出现"工具调用留着、它的输出被丢了"这种半截状态。
5. **硬上限 40k token**：还超就从最老的丢，并在开头写明 `[更早的 N 条记录已省略]`——不假装看到了全部。

与两家的差别：他们对超出的部分**再调一次模型做摘要**（opencode 现在还有锚定更新
`buildPrompt(update)`、`SHRINK_STEPS` 在被"太长"拒绝后按 0.7/0.5/0.35 缩预算、
用 provider 回报的 usage 当锚点的 measured+estimated 估算），我们暂时只做"截断 + 骨架化"，
一次复盘只花一次模型调用。等真有任务长到 40k token 都装不下，再补"二次摘要"那一层。



- **错误上报（已实现·留接口）**：服务把错误上报给中心（`report`）——中心记一条日志并交给 `OnReport`
  消费挂点（前端 / 落盘 / 富日志后续接入此处）。**不含链路编号**（暂无链路记录者，记了无处可查）；
  **防刷在上报方**：退避 1s 起逐档翻倍、封顶 30s，静默 60s 重置，被节流即丢弃；中心视为**永远在线**，
  不做补发队列——断连期间的上报直接丢弃（中心侧不排队、不回补、不落盘）。

### 7.3 MCP 服务设置

`mcp` 复用既有三类设置机制，不新增中心动作或配置存储。能力映射由本服务读取 `data/mcp.json`；外部连接和工具不登记为独立的原生服务，导入权限与对外发布权限分别配置。

| 键 | 类型 | 含义 |
| --- | --- | --- |
| `http.token` | 秘密 | MCP HTTP 入口的 Bearer token；通过 `HandleSecret` 保存在服务自己的秘密文件 |
| `name` / `addr` / `center` | 只读 | 服务名、内部 WS 监听地址、中心地址 |
| `http.addr` | 只读 | 独立 MCP HTTP 监听地址 |
| `config.path` | 只读 | 本服务配置文件路径；exports 经完整校验后热更新；路径、remotes、imports 与证书信任修改需重启 |

---

## 8. 链机制要点（写“参与链”的服务必读）

- **boss（结果归属）**：每跳自带其 boss，逐跳自定、**不可丢失**；任务发起者必须收到最终结果。
  入口服务必须把 boss 设成**自己**（qq 与门户都如此）：少了它，回答照样生成，但**回不来**——
  reasoner 只按 boss 交付，boss 认错就是静默丢弃。交付**失败**时 reasoner 会向会话入口
  （`user_message` 的信封 `from`）补一条 `[交付失败] …`（"最后失败了再补一条给 A"）。
- **trace**：同一条用户请求全链共享，用于调试 / 审计。
- **deliver**：链终点判定结束后**直达 boss**（不沿链逆向回传）。发起任务的接入服务（如 qq）必须实现 `deliver`。
- **完全启动（记忆往返）**：任务入口先 `recall` 给记忆服务托管上下文；**只有收到 `from=memory` 的 `resume`** 才调用 LLM API。任务内的各轮由黑板延续——不再重复经记忆（上下文已在线程里）。
- **工具结果入账**：结果到达 → 注入黑板 → 按通知模式唤醒下一轮（`each` 逐条 / `batch` 攒批，见 §6.4）。
  **工具参数的数字一律按整数归一**（LLM 的 JSON 数字是 float64，直接进 msgpack 会让服务侧的整数字段解码失败）。
- **发出即完**：一步 = 收到 → 处理 → 发出下一跳 → 结束；没有长挂起的链（`wait` 是唯一的阻塞点）。
- **失败处理（现状）**：拨号失败（连接未建立、消息可证明未送达）自动安全重试（+0.5s、+2s）；
  送达后的失败不重试——可能已被处理，重试需要幂等键（待定）。工具调用失败会作为结果消息注入（错误文本交回模型决断）；
  单次工具超时（默认 60s）后按失败入账；任务被同会话新消息取代时，在跑结果丢弃（日志可见）；
  交付 boss 失败按“送达后不重试”原则停住（日志可见；失败上报 boss 待定）。

---

## 9. 测试要求

- 服务自带 `tests/`：起真实服务 + 真实 WS，覆盖自己的动作（含错误路径）。
- 根 `tests/`：跨服务**真进程**端到端（新增链路时补一条）。
- 测试**不得依赖外网**：用假 LLM（OpenAI 兼容替身）/ 假对端；真模型只在显式开关下跑冒烟。
- 测试里构造消息建议**手写字段**（当“只读规范的第三方”），而不是复用生产结构体——这样契约漂移会立刻被测试抓住。

---

## 10. 兼容与版本

- 信封 `v` 是协议版本（当前 `1`）。
- **动作只增不改**：已有动作的语义与载荷字段只做加法；删改 = 破坏性变更。
- **未知字段必须忽略**（解码时不得因多字段报错）；**未知动作必须回 `{ok:false, error}`**，不得静默丢弃。
- 【待定】`protocol/` 是否对外发布版本（当前为仓库内本地模块，`v0.0.0` + `replace`）。

---

## 11. 待定事项（写服务前先看这里）

| # | 事项 | 现状 | 倾向 |
| - | ---- | ---- | ---- |
| 1 | LLM 工具暴露规则 | **已实现**：`provides` 里 `tool: true` 显式声明才暴露 + 保留/调试动作硬排除（第二道闸） | — |
| 2 | 离线判定 | **已实现**：2× 心跳超时 + 扫描摘除 + 告别立即摘除 + 心跳复归 | — |
| 3 | 连接自愈 | **已实现**（`service/` 骨架）：指数退避重连 + 重新注册 + 注册去重 | — |
| 4 | 设置热链 | **已实现 M7**：注册即全量下发（异步、不等待）+ set/reset 单键下发 + `settings` 事件回传结果；骨架通用接收（`HandleSetting` + 注册后拉取兜底）；各服务设置项（reasoner 3 / memory 5 / images 1 / qq 1 / 中心自身 2） | “调度 = 设置行为、设计工作流”的完整形态 |
| 5 | 连接时长自适应 | 全短连接（注释已标注） | 热保留 / 冷回落，参数可配置（实现前不声明设置项） |
| 6 | 审批链路 | **已实现第一层**：PowerShell AST 规则引擎 + 执行摘要绑定 + fail-closed；shell 通过 `approval_request` 取得裁决，并沿 `report_result` 反馈；**第二层已实现**：白名单外挂起 → 推门户 → 用户裁决（`approval_pending` / `approval_decide` / `approval_list`，TTL 默认 10 分钟）| 解释（LLM 解读）与富通知未做；`approval_result` 占名未采用 |
| 6.1 | 命令执行的护栏（shell） | **已实现**：工作目录根 + 越界即拒（含软链解析）、执行超时、单流输出上限、**无队列**（并行/串行归调用方；只有防爆熔断，超过上限同步拒绝）；命令规则已由 approval 第一层实现，用户审批策略**未做** | 护栏归审批服务与设置项演进，服务只报事实 |
| 7 | 服务骨架复用 | **已实现**：`service/` 薄骨架库（注册/心跳/名册/自愈/告别），四服务共用 | — |
| 8 | 链级失败策略 | 拨号失败已安全重试；送达后失败不重试（无幂等键）；工具超时 / 任务取代的结果丢弃已定义 | 幂等键；失败上报 boss（降级回复） |
| 9 | 工具结果通知模式 | **已实现**：设置项 `tool.notify_mode`（each 默认 / batch）+ 注册下发 + 拉取兜底 + `-tool-notify` | — |
| 10 | 长期记忆 | **已实现**：`remember`/`forget`（软删）+ 核心注入（priority）+ **语义召回（embedding 服务，余弦 ≥0.45）+ bigram 兜底**；`remember` 在 provides 里（自动暴露为 LLM 工具），`forget` 不在 | 自动提炼与巩固线（sleep）；去重 / 冲突 |
| 11 | 错误上报（服务 → 中心） | **已实现·留接口**：`report` 动作 + 中心日志 + `OnReport` 挂点；上报方退避防刷（1s 起翻倍、封顶 30s）；无队列（中心视为永远在线）；当前接入点：骨架 Fire 未送达 | 前端 / 落盘 / 富日志接入挂点；更多上报点按需接入 |

---

## 12. 参考实现

| 服务 | 语言 | 职责（一句话） |
| ---- | ---- | ---- |
| settings-center | Go | 维护“服务名 → 名片”注册表；注册返回名册、新注册广播名册；保管设置并提供存取动作 |
| reasoner | Go | 单步推理器：任务入口经 memory 托管、收到 resume 才调 LLM；工具异步派发（即时回执 → 结果注入 → 唤醒），收尾门禁保护，结果 deliver 直达 boss |
| memory | Go | 会话消息的唯一保管者；recall 取最近历史 + 长期记忆（核心注入 + bigram 召回）组装为记忆块后 resume 回调用方；remember 写入（幂等）/ forget 软删 |
| images | Go | save（URL/base64 落盘）返回 id；fetch（id → base64） |
| files | Go | save（URL/base64 + filename 落盘）返回 id；fetch（id → base64）；read（id → 截断文本 / .xlsx 转 TSV / .docx/.pptx 提取正文，`tool: true`）；write（`{id, sheet, edits:[{cell, value}]}` 单元格编辑，`tool: true`——就地修改 .xlsx 权威副本）；edit_word（`{id, operations:[{op, old, new}]}` 纯文本替换，`tool: true`——就地修改 .docx 正文）；edit_ppt（`{id, operations:[{op, old, new}]}` 纯文本替换，`tool: true`——就地修改 .pptx 幻灯片文本） |
| shell | Go | 受理即回执的命令执行（无队列：并行/串行归调用方，只有防爆熔断；超时终止 + 输出截断）；审批接缝（未接入 = 一律未通过）；结果由 `report_result` 补报 |
| approval | Go | 终端命令审批第一层：PowerShell AST + 不可变规则快照 + 执行摘要绑定；第二层仅接口 |
| timer | Go | 计时器：受理「N 毫秒后唤醒我」（受理即回执，不阻塞调用方）→ 到点把 `report_result` 跳回调用方；在计时器按 boss 分组推给门户（倒计时） |
| mcp | Go | 双向协议翻译：通过显式映射导出获准的原生能力、导入外部 MCP 工具；只登记自身，不自动传播下属能力或重新导出已导入工具 |
| batch | Go | 批处理：一次声明 N 项工作 + 并发度 + 唤醒时机；受理即回执，按 key / 首次失败 / 终态推至多三条；暂停可恢复、停止是终态；目标动作须显式声明 `Batchable` |
| embedding | Python | 文本 → 向量（onnxruntime + tokenizers，CLS pooling + L2 归一）；动作 `embed`；fake 后端供测试与 CI |
| qq | Go | 把主人私聊消息（图先存 images）打成 user_message 发给 reasoner；实现 deliver 回发私聊 |
| frontend | TS | 用户交互门户（与 qq 同类）＋ 设置中心面板：对外提供 `deliver`（推给浏览器），自己跳 `user_message` 给 reasoner；浏览器 HTTP JSON + SSE；**单用户** + 浏览器信任栅栏（Host 挡 DNS rebinding、Origin 挡跨站） |
| tests | Go | 跨服务真进程端到端（不属于任何服务） |

> 非 Go 实现只需遵循本规范；`protocol/`（协议实现）与 `service/`（服务骨架：注册 / 心跳 / 重连 / 告别 / 设置）是 Go 侧的便利库，不是准入条件。

### 12.1 本地开发端口

| 服务 | 默认端口 | 协议 |
| --- | --- | --- |
| settings-center / reasoner / memory / qq / images | 9000 / 9101 / 9201 / 9301 / 9401 | WS `/ws` |
| frontend | 9501 | HTTP 面板与内部 WS |
| embedding / shell / timer / files / approval | 9601 / 9701 / 9801 / 9802 / 9901 | WS `/ws` |
| mcp | 9902 | 内部 WS `/ws`，常规注册与结果补报 |
| mcp | 9903 | MCP Streamable HTTP `/mcp`，独立鉴权，默认回环监听 |

地址可由启动参数调整；同一 `mcp` 进程持有两个监听地址，不是两个 SUMP 服务。端口分配与 `scripts/run-local.ps1` 一致。

---

## 13. 命名基线（防漂移）

**名字只增不改；改名 = 破坏性变更，须在本表登记替代名与迁移说明。** 新增任何名字前先对照本表：

| 类别 | 规则 | 现有值 |
| ---- | ---- | ------ |
| 消息类型（`type`） | 裸名词；请求 / 响应 / 事件方向固定（事件型只由中心广播） | `register` `response` `roster` `jump` `heartbeat`；事件：`roster` `settings` |
| 动作（`jump.action`） | 小写下划线、动词_名词；保留动作见 §6.1 | `ping` `roster` `report` `list_settings` `set_setting` `reset_setting` `report_setting` `embed` `run` `report_result` `approval_request` `approval_pending` `approval_decide` `approval_list` `capabilities`；骨架保留：`configure`；timer：`sleep` `cancel` `list`；memory：`remember` `forget`；files：`read` `write` `edit_word` `edit_ppt` |
| 载荷字段 | snake_case；可选即省略（omitempty）；跨服务新字段先在本表落一行 | `key` `value` `default` `overridden` `cause` `service` `results` `ok` `error` `heartbeat_ms`；设置项三类标记：`secret` `readonly` `set`；服务自报：`actual` `reported`；文件链：`filename` `files` `file_ids` `truncated` `sheet` `cell` `edits` `updated` `operations` `op` `old` `new` `replaced`；embedding：`texts` `query` `vectors` `dim` `model`；异步受理与补报：`item_id` `phase` `text` `data` `final`；shell：`command` `work_dir` `exit_code` `stdout` `stderr` `stdout_bytes` `stderr_bytes` `timed_out` `truncated` `duration_ms`；timer：`ms` `note` `fire_at` `seq` `timers` `caller` `status` |
| 审批载荷字段 | snake_case；结构由 `protocol/approval.go` 定义 | `schema_version` `plan` `decision` `decision_id` `plan_digest` `policy_version` `reason_code` `valid_for_ms` `llm_feedback` `summary` `unknowns` `rule_ids`；补报新增 `approval`；第二层：`requested_by` `created_ms` `expires_ms` `note` |
| exec 域执行描述字段 | 当前 `plan` 是命令域专用结构；每个字段只保留一个语义 | `version` `command` `work_dir` `shell_path` `args` `dialect` `platform` `profile` `timeout_ms` `max_output_bytes`；`dialect` 只表示 shell 方言，`profile` 只表示执行环境约定 |
| 域判别字段（将来） | 新增第二个执行域时定义独立字段；当前协议尚未发送，不复用 `dialect` | 建议 `kind`，取值如 `exec` / `fs` / `http`；其他域各自声明执行字段组，裁决包、摘要绑定、有效期、fail-closed 共用 |
| 审批枚举 | 取值由 approval 服务定义，见 [审批协议](./COMMAND-APPROVAL-PROTOCOL.md) | `decision`：`allow` / `deny` / `review`；`reason_code`：`RULE_ALLOW` `POLICY_DENY` `DYNAMIC_BEHAVIOR` `NO_MATCHING_RULE` `PARSE_ERROR` `UNSUPPORTED_DIALECT` `CONTEXT_UNKNOWN` `RUNTIME_MISMATCH` `LIMIT_EXCEEDED`，第二层用户裁决：`USER_APPROVED` `USER_DENIED` |
| 服务名 | 小写短名 | `settings-center` `reasoner` `memory` `images` `files` `qq` `shell` `timer` `batch` `mcp` `frontend` `embedding` `approval` |
| 设置键 | `<域>.<名>` 小写 + 下划线；由服务自己声明 | `tool.notify_mode` `tool.concurrency` `tool.timeout_ms` `batch.concurrency` `batch.max_concurrency` `batch.max_inflight` `batch.max_items` `batch.item_timeout_ms` `batch.preview_bytes` `batch.job_ttl_ms` `batch.resume_on_start` `recall.history_limit` `recall.core_limit` `recall.core_max_chars` `recall.rel_limit` `recall.rel_max_chars` `recall.embedding` `max_mb` `read_max_chars` `owner` `sweep_ms` `heartbeat_fallback_ms` `exec.shell` `exec.root` `exec.timeout_ms` `exec.max_output_kb` `exec.max_running` `approval.service` `approval.timeout_ms` `policy.json` `max_ms` `max_pending` `portal` `llm.base` `llm.model` `llm.key` `memory` `images` `name` `addr` `center` `napcat.url` `napcat.token` `reasoner` `db` `dir` `settings` `web` |
| 名片字段（`provides[]`） | 小写下划线；跨语言一致 | `action` `input` `output` `tool` `async` `batchable` |

审批设置键补充：`policy.json`（approval 声明的规则快照配置）。`approval_result` 没有注册为动作；审批结果作为 `approval_request` 的响应返回，并由 shell 的 `report_result` 承载后续反馈。

MCP 设置键补充：`http.token` 与 `remote.<id>.token`（秘密）、`http.addr` / `config.path`（只读），见 §7.3。MCP 的方法名、连接与工具目录属于 `mcp` 的外部协议，不新增 SUMP 信封字段。外部请求进入 SUMP 时，由 `mcp` 生成独立 trace 并作为结果归属方（`from=mcp`、`boss=mcp`）；反向调用沿用原始 SUMP 请求的响应关联。受理回执不得当作最终执行结果。

MCP 导入动作的 `provides[].batchable` 为 `false`；导入授权不包含 batch 代理执行授权。batch 对此返回 `BATCH_NOT_BATCHABLE`，直接调用及其并发不受影响。

MCP 同步导入结果：`phase` 为 `completed` / `failed`，`is_error` 表示远端工具是否明确失败，`content` 保留 MCP 内容块，`structured_content` 保留已校验的结构化结果；工具明确失败仍是已成功翻译的响应，`response.ok=true`。传输或契约失败使用 `response.ok=false`，不可据此认定远端没有执行。详见 [MCP 服务](./mcp/README.md)。

MCP 异步导入由绑定配置 `async:true` 固定声明 `Provide.Async`。先返回 `{item_id,phase:"accepted"}`，再沿原来源、trace、boss 发送现有 `report_result`；`text` 是有界可读投影，`data` 保留翻译结果，`final:true` 表示本地观察结束。派发后失去结果时 `phase=outcome_unknown`，不能解释为操作未执行或已停止。`cancel_import` 输入仅 `item_id`，校验原调用服务身份；返回 `status=observation_cancelled/unknown` 和 `remote_outcome=unknown`，只取消本地等待。失败数据可带 `execution_outcome=not_submitted/unknown`，不新增信封字段或全局调度状态。

异步调用方在派发前登记待回执，补报必须以来源服务、trace、item_id 共同关联。早到缓冲有界，确定终态仅交付一次。MCP 对外的 `sump://jobs/{job_id}` 观察资源归 HTTP 认证主体所有，不依赖传输会话；它是模块自己的临时记录，不是原生任务表或 MCP Tasks。
