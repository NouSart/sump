# memory 域拆分目标：门面 + session / experience 服务 + context 模块

> **读者**：要评审或实施 memory 拆分的人 ｜ **读完能做**：判断拆分边界、协议归属与验收方式 ｜ **状态**：**已实施第一阶段（三进程角色化）；源码包拆分未做**，见 §13 ｜ **最后校验**：2026-11-19

## 1. 目标

把当前单个 `memory` 服务拆成**一个对外门面 + 两个数据权威服务 + 一个拼装模块**：

```text
settings-center
      │ 全局名册只有一张卡：memory
      ▼
memory 门面（服务）
  ├─ context 模块（拼装）
  ├─ 路由 / 卡片聚合 / 设置聚合 / health / metrics
  ├─ session 客户端 ──SPv2──▶ session 服务（私有会话库）
  └─ experience 客户端 ─SPv2──▶ experience 服务（共享经验库）
```

一句话：**对外只暴露 `memory`；对内谁拥有哪份数据、谁负责拼装，必须一眼看清。**

## 2. 非目标

- 不是把 `context` 也拆成第三个内部服务；它没有自己的数据。
- 不是给 memory 再造一个全局“分 DNS”；内部地址先静态配置，服务数量真正变多再谈私有 registry。
- 不是让门面持有会话/经验数据或做业务判断。
- 不是改变 `memory.*` 对外 method 名与模型工具名（`memory__remember` 等保持不变）。

## 3. 三个功能的形态

| 功能 | 形态 | 理由 |
| --- | --- | --- |
| `session` | **独立服务** | 私有数据权威：conversation / task / message / work；有自己的 DB、生命周期、故障语义 |
| `experience` | **独立服务** | 共享数据权威：longterm / memnet / 共享摘要 / 可见性过滤；原文不离开本服务 |
| `context` | **`memory` 门面内模块** | 无数据，只做“取基线 + 取经验块 + 插入/裁剪”；拆成服务只会多一跳、多一套失败组合 |
| `memory` 门面 | **独立服务** | 唯一对外卡片、路由、设置/健康/指标聚合、错误收口 |

## 4. 数据权威

| 数据 | 权威服务 | 说明 |
| --- | --- | --- |
| `data/session.db` 的 conversations / messages / tasks / work | `session` | 私有；只有 session 能读写 |
| `data/experience.db` 的长期记忆、经验网、共享摘要 | `experience` | 跨会话；只有 experience 能读写 |
| 拼装结果 `ContextResult` | 不落库 | 由 `context` 模块按请求现算 |

约束：

- 两个服务各自打开自己的 DB；门面与另一个服务都拿不到对方的 DB 句柄。
- 不设跨库事务；跨服务用幂等用例与明确错误码。
- `work` 账本归 `session`，因为 `context` 需要在同一处 ensure work 并取 task/history。

## 5. 对外协议：只有 `memory.*`

settings-center 与调用方只看见 `memory`。门面按 method 路由：

| 对外 method | 去向 |
| --- | --- |
| `memory.context` | 门面内 `context` 模块 |
| `memory.store` / `history` / `conversation_*` / `task_*` / `work_get` | `session` |
| `memory.remember` / `forget` / `dig` / `lineheads` / `publish_session` / `consolidate` | `experience` |
| `memory.configure`（设置下发） | 门面按设置键归属转发 |

门面启动时必须检查：

- 两个内部服务声明了同一个 method → 启动失败；
- 两个内部服务声明了同一个设置键 → 启动失败；
- 模块 `provides` 的 `tool` 标记冲突 → 启动失败；
- 对外卡片必须等于所有内部卡片的聚合。

## 6. 内部协议：直接使用 SPv2

门面与 `session` / `experience` 之间**不定义另一套 RPC**，内部调用就是普通 SPv2 帧：同一套 `Frame`、`/spv2`、msgpack、错误码、超时与指标。

```text
call to=session method=context_base work=w1
  body {conversation, messages, owner?, task_name?}
reply {task, work, messages}          // ensure work + 取当前任务历史

call to=experience method=inject work=w1
  body {viewer, origin, query, work, budget}
reply {block}                         // 已按查看者过滤的记忆块
```

约定：

- 帧头 `from` **原样保留外部调用方**（门面透传，不改成自己的名字）；`work` 与外部请求同一个工作号。
  这样 `session` 的 work 授权仍按真实调用方判断，也符合“结果归属逐跳不可丢失”；
- 内部 method 名不带服务前缀，服务由 `to` 指定；
- body 用 `src/protocol` 的 DTO，不建内部私有协议；
- 内部服务不注册到 settings-center，但仍然暴露 `/spv2`；
- 门面的 Go 接口（`SessionClient` / `ExperienceClient`）只是依赖注入与测试替身，**不是线上协议**，实现由 SPv2 客户端完成。

`context` 模块只做：

1. 通过 SPv2 调 `session.context_base` 拿 task/work/history；
2. 通过 SPv2 调 `experience.inject` 拿过滤后的记忆块；
3. 把块插入消息链、按预算裁剪；
4. 返回 `ContextResult{messages, work, task}`。

它**不**做：会话写入、经验写入、可见性判断、embedding 调用。

## 7. 可见性边界

- 共享库的读取过滤归 `experience`；`context` 只接收过滤后的块。
- 来源会话：`viewer == origin` 时可见原文；其他会话按机密路径规则替换为 `[机密文件]`。
- 没有单一来源的聚合条目：默认按“非来源”处理，宁可保守。
- 审批（如需要）绑定 `work + entry/文件摘要 + viewer + TTL`；审批不等于允许写入共享库原文。
- 机密文件路径是否允许原文落盘，属于独立决策；本方案只规定“读取时按查看者过滤”。

## 8. 设置与发现

- settings-center 只向 `memory` 下发设置；门面按设置键归属，通过 SPv2 `configure` 转发给 session / experience。
- `session` / `experience` **不注册到 settings-center**，全局名册只有 `memory`。
- 门面聚合 health / metrics 也走 SPv2 调内部 `health` / `metrics`，不直接引用内部包。
- 内部地址先由 memory 配置静态给出；内部服务达到 3 个以上、且需要独立扩缩容时，再考虑私有 `memory-registry`（只 `resolve`，不代理数据，不注册全局，协议仍是 SPv2）。
- 门面不做进程守护；启动顺序与重启由 run-local / systemd / 容器负责。

## 9. 失败语义

第一版保持硬门，不静默降级：

| 失败 | 对外表现 |
| --- | --- |
| `session` 不可用 | `memory.context` 失败，链停 |
| `experience` 不可用 | `memory.context` 失败，链停（降级策略另行决策） |
| `memory` 门面不可用 | 整个 memory 不可用；接受单点，靠重启/健康检查 |
| 内部 method 超时 | 门面映射为统一错误码，不泄露内部服务名 |

## 10. 源码布局

目标布局（**尚未完成**；当前实现见 §13）：

```text
src/memory/
  README.md                 # 领域地图：数据权威 / method / 设置 / 失败语义（已建）
  facade/                   # main + router + context 模块
    main.go
    router/
    context/                # 拼装模块（不出进程）+ 依赖接口
    clients/                # 基于 spv2.Client 的 session/experience 客户端
  session/                  # 独立 go.mod + main.go
    server/
    store/
  experience/               # 独立 go.mod + main.go
    server/
    longterm/
    memnet/
    shared/
    redact/
    audit/
    repair/
```

规则：

- `session` / `experience` 互不 import，也不 import `facade`；
- 门面不 import 两个服务的内部包，只通过 `/spv2` 与 `src/protocol` DTO 通信；
- 共享物只有 `src/protocol` / `src/spv2` / `src/service`；
- `context` 只依赖 `SessionClient` / `ExperienceClient` 接口，不直接拿 DB；生产实现一定是 SPv2 客户端，测试替身才允许进程内；
- 依赖方向由架构测试（import 规则）钉住，不靠自觉。

## 11. 实施阶段与验收

实施可以分阶段，但**架构约束从第一阶段就生效**：

1. **门面骨架**：`memory` 卡片、路由表、设置/health/metrics 聚合；session / experience 从第一阶段就是独立进程，门面只通过 SPv2 调它们。
2. **纵切一条链**：`memory.context` → `session.context_base` → `experience.inject` → 拼装。
3. **迁移 method**：按归属表搬，`memory.*` 不变。
4. **补可见性过滤与故障注入**。
5. **补契约测试**：同一套 SPv2 契约分别打真 session / experience 与假 peer，禁止走进程内快捷路径。

验收：

- 全局名册只有 `memory` 一张卡；
- `memory.context` / `store` / `remember` / `forget` 纵切通过；
- 把 `experience` 换成假 peer，session 与门面零改动；
- 同一 method/设置键冲突时门面启动失败；
- session 挂、experience 挂的错误语义符合第 9 节；
- 模型工具名仍为 `memory__*`。

## 12. 被拒绝的方案

- **现在就把 context 做成独立服务**：无数据权威，多一跳，失败组合变多；留 Go 接口即可。
- **内部走进程内函数调用、只把外部做成服务**：会得到“本地一套、线上另一套”，架构验证不到真实边界；内部协议统一用 SPv2。
- **memory 下设分 DNS**：两个内部服务不值得；静态配置即可，超过 3 个再谈私有 registry。
- **session / experience 注册到全局名册**：会把内部拓扑暴露给所有服务，违背“只暴露 memory”。
- **门面持有数据或业务规则**：会变成新的“总管”，拆分失去意义。
- **共享内部库**：会让两个服务重新耦合；共享物只能是协议。

## 13. 实施记录（2026-11-19）

已落地的是**运行时边界**，不是最终源码布局：

- `src/memory` 仍是**单个 Go 模块、单个实现包**；`memory.exe -role session|experience|facade` 启动三个独立进程。
- `service` 骨架新增 `Standalone`（不连设置中心）、`Peers`（静态内部地址）、`NativeFilter`（名片只发布本域 method）。
- 角色只决定暴露哪些 method / 设置项：`session` 持有会话库，`experience` 持有共享库，`facade` 不打开数据库、只做路由与 `context` 拼装。
- 门面启动自检：两个内部服务必须可达、method 不得重叠、每个域内 method 必须有人声明；缺失或冲突直接启动失败。`/healthz` 带 session / experience 探针。
- `context` 两跳：`session.context_base`（任务 / work / 历史）+ `experience.inject`（核心 / 相关 / 线头 / 其他会话），门面本地 `compose.Inject`。
- 整理池跨域：`experience.consolidate` 通过 `session.staged_pending|mark|retry` 读写，不直接打开会话库。
- 设置下发：门面保留 13 个键，按归属转发 `configure`；只读项由门面展示。内部服务用 `settings_schema` 报自己的键，门面启动时检查重复。Session / experience 只声明本域键（不再声明 name / addr / center）。
- 测试：`tests/facade_roles_test.go` 覆盖跨进程读写与设置转发；`tests/role_cards_test.go` 钉住名片隔离与内部 method 不外泄；`tests/facade_start_test.go` 钉住启动硬门；`service/tests/standalone_test.go` 钉住独立模式与静态对端。

仍未做（已进 TODO）：

- 把实现包拆成 `facade/` / `session/` / `experience/` 三个 Go 包（乃至独立 `go.mod`），并用 import 规则测试钉住。当前靠 `Role` 与 `NativeFilter` 在运行时隔离，编译期仍可互相 import。
- 门面 metrics 聚合；`conversation_get` 补 `publish_session` 的会话标题；读取侧可见性过滤（`inject.viewer` 目前只是占位）。

