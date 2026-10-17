# frontend —— SUMP v2 用户交互门户（与 qq 同类）＋ 设置中心可视化面板

TS 服务节点 + 浏览器界面：既做**用户与智能体之间的门户**，也把设置中心的状态与设置可视化。

```
用户 ──HTTP/JSON + SSE──▶ frontend(:9501) ──单条 WS（注册 / 心跳 / 名册）──▶ settings-center(:9000)
                                       └──直连跳转（user_message / deliver）──▶ reasoner 等目标服务
```

## 门户：与 qq 同类

| 方向 | 机制 |
| --- | --- |
| **出站**（智能体 → 用户） | 对外**提供 `deliver`**（`{text, conversation_id}`）→ 回执"已送达面板" → 经 SSE `deliver` 事件推给浏览器。回执**不表示用户看过**（同 shell 的"受理即回执"） |
| **入站**（用户 → 智能体） | `POST /api/message {text}` → 直连跳转到 `-reasoner` 的 `user_message`（信封 **boss = 门户自己**，否则回答回不来）→ 只回**受理回执**；最终回答经 `deliver` 送回来；reasoner 交付失败时会补一条 `[交付失败] …` 走同一条 `deliver` 通路 |
| **身份** | 单用户：固定会话 id `web:private:owner`（多用户留待以后）；形状与 qq 的 `qq:private:<uid>` 一致 |
| **访问控制** | 浏览器信任栅栏（`src/server/portal.ts`）：Host 必须是回环 / 绑定地址 / `-trusted-hosts`（挡 DNS rebinding），带 Origin 时必须受信（挡跨站）。**HTTP 与 WS 端点都过这道栅栏**——服务之间的 WS 连接不带 Origin（放行），浏览器发起的握手必带（拦截），否则恶意页面能直连 `/ws` 往门户里塞假投递。栅栏只证明"请求来自受信地址"，**不建立身份** |

栅栏不是可选项：门户能驱动智能体，而智能体手里有 `shell`——门户就是一个"能执行命令的入口"。
只绑回环不够：恶意页面可把域名解析到 127.0.0.1（DNS rebinding），浏览器看它是**同源**因而不发预检，
但 Host 头写着攻击者的域名；Host 是重绑伪造不了的那个头，所以钉它。

- 面板本体注册为服务节点（在名册中可见自己）；**不持业务状态**：每次请求现翻成中心跳转，回执翻回 JSON。
- v1 范围：服务名册 + 设置查看 / 修改（中心受理并下发 `configure`）/ 重置；三类设置项各有各的界面——**普通项**（值 + 默认值 + 覆盖 / 重置两步确认）、**秘密项**（只显示「已设置 / 未设置」，覆盖走密码框、清空走两步确认，值不回显）、**只读项**（显示启动值 + `不可改`，没有修改入口）；服务自报的实际值另起一列，与中心口径不一致时给「服务侧已变」标记（悬浮看服务侧现值）；名册、连接状态与设置结果事件驱动实时刷新（SSE，**无刷新按钮**）；设置行状态：默认 / 已覆盖 / 已覆盖·待确认（本会话写入等待服务确认）/ 已覆盖·未送达（悬浮看原因）。错误上报视图留待 v2。

## 命令

```powershell
npm install          # 安装依赖（Node ≥ 22.18）
npm run build        # 类型检查（tsc ×3，含测试）+ 服务端编译 + vite 构建（dist/server + dist/web）
npm test             # 全部测试 = test:node + test:web
npm run test:node    # 协议夹具 / 中心链路 / HTTP API（node --test，内置 TS 类型剥离）
npm run test:web     # 面板组件测试（vitest + jsdom + Testing Library，逻辑层）
npm start            # 启动（需先 build）：node dist/server/index.js -addr 127.0.0.1:9501 -center ws://127.0.0.1:9000/ws
```

常用参数：`-addr`（HTTP/WS 监听）、`-center`（设置中心地址）、`-name`（服务名，默认 frontend）、`-web`（静态目录，默认 `dist/web`）、`-heartbeat-ms`、`-reasoner`（用户消息的跳转目标，默认 reasoner）、`-trusted-hosts`（额外放行的 Host，逗号分隔；留给反向代理 / 局域网域名）。

## 结构

- `src/server/protocol.ts`：协议层（信封 + MessagePack）——与 Go 侧逐字段对齐，`tests/protocol.test.ts` 用 Go 产出字节做锚定；
- `src/server/center.ts`：中心连接（注册 / 心跳 / 断线自愈 / 名册广播 / 调用的 `RemoteError` 区分）；
- `src/server/portal.ts`：门户的会话身份、投递分发（WS → SSE）与浏览器信任栅栏；
- `src/server/service.ts`：自己的 WS 端点（对外提供 `deliver`；其余动作一律 `unknown action` 回执——注册的地址必须真实可达）；
- `src/server/api.ts`：HTTP 层（`/api/overview`、`/api/message`、`/api/settings/set|reset`、`/api/events` SSE——事件：`status` / `roster` / `settings` / `deliver`、静态托管；每个请求先过信任栅栏）；
- `src/web/`：React 界面（版式：悬浮胶囊边栏；**主体是门户对话**，设置从侧栏底部一行进入、另起一页并自带服务导航 + 设置字段列表（纵排：键 + 状态标签 + 生效值 + 修改/重置，秘密项与只读项各有各的控件）；**视觉取自 DSH 设计系统**——见下；Fira Code 本地打包）。
- `tests/`：`*.test.ts` = 服务端（node --test）；`tests/web/` = 面板组件测试（vitest + jsdom，
  `harness.ts` 提供假 EventSource 与 fetch 路由桩——SSE/fetch 在边界处替换，App 代码零改动；`tsconfig.test.json` 让测试进类型检查）。
- `design/`：版式样张（specimen.html + specimen.css，纯静态、供版式评审；经 `:19531` 查看需先拷到 `dist/web/`）。

## 设计说明（节选）

- **本项目没有自己的视觉设计**：观感 = DSH（deepseek-harness）的设计系统。`styles.css` 整表 `@import` 上游 `ui-theme` 的样式表（令牌、字体栈、圆角阶梯、焦点环、滚动条），下游规则**直接用 `--dsw-alias-*` 令牌**，不自建色值。
- 组件取值照抄上游 `ui-primitives` 的 `.module.css`：输入框（`Input`、`settings-form/fields`）、按钮（`Button` 的 md/sm 与主/幽灵/描边变体）、状态点（`StateDot`）、分段开关（`SegmentedControl`）、设置字段（`settings-form`）；布局几何对齐上游 `ui-layout`（侧栏 280）。
- **外观开关**（侧栏底部）：只切 `body[data-ds-dark-theme]`——DSH 的暗色是另一套令牌，颜色全部自动跟随，我们不出第二套色。首选项存 `localStorage`，缺省跟随系统偏好。注意 `bg-overlay` 在暗色下是**遮罩**用的中灰，块底一律用 `interactive-bg-hover`。
- 注意令牌**作用域**：DSH 把 `--dsw-*` 定义在 `body` 上——自定义属性在「声明它的元素」上求值，所以别把这些令牌映射到 `:root`，否则整层引用会静默失效（属性变透明，CSS 不报错）。
- 有意偏差：① 占位符用 `label-tertiary` 而非上游的 `label-dimmed`（后者在白底上几乎看不见）；② 首个连接设置中心失败不退出（持续退避重连），与其余服务“引导错误立刻可见”不同——面板优先可用性。
