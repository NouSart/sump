# frontend —— SUMP v2 前端服务（设置中心可视化面板）

TS 服务节点 + 浏览器面板：把设置中心的状态与设置可视化，写操作经中心受理（**受理回执不等待服务送达**；服务确认结果经 `settings` 事件实时回显）。

```
浏览器 ──HTTP/JSON + SSE──▶ frontend(:9501) ──单条 WS（注册 / 心跳 / 名册 / 动作代转）──▶ settings-center(:9000)
```

- 面板本体注册为服务节点（在名册中可见自己）；**不持业务状态**：每次请求现翻成中心跳转，回执翻回 JSON。
- v1 范围：服务名册 + 设置查看 / 修改（中心受理并下发 `configure`）/ 重置；名册、连接状态与设置结果事件驱动实时刷新（SSE，**无刷新按钮**）；设置行状态：默认 / 已覆盖 / 已覆盖·待确认（本会话写入等待服务确认）/ 已覆盖·未送达（悬浮看原因）。错误上报视图留待 v2。

## 命令

```powershell
npm install          # 安装依赖（Node ≥ 22.18）
npm run build        # 类型检查（tsc ×3，含测试）+ 服务端编译 + vite 构建（dist/server + dist/web）
npm test             # 全部测试 = test:node + test:web
npm run test:node    # 协议夹具 / 中心链路 / HTTP API（node --test，内置 TS 类型剥离）
npm run test:web     # 面板组件测试（vitest + jsdom + Testing Library，逻辑层）
npm start            # 启动（需先 build）：node dist/server/index.js -addr 127.0.0.1:9501 -center ws://127.0.0.1:9000/ws
```

常用参数：`-addr`（HTTP/WS 监听）、`-center`（设置中心地址）、`-name`（服务名，默认 frontend）、`-web`（静态目录，默认 `dist/web`）、`-heartbeat-ms`。

## 结构

- `src/server/protocol.ts`：协议层（信封 + MessagePack）——与 Go 侧逐字段对齐，`tests/protocol.test.ts` 用 Go 产出字节做锚定；
- `src/server/center.ts`：中心连接（注册 / 心跳 / 断线自愈 / 名册广播 / 调用的 `RemoteError` 区分）；
- `src/server/service.ts`：自己的 WS 端点（v1 未提供动作，一律 `unknown action` 回执——注册的地址必须真实可达）；
- `src/server/api.ts`：HTTP 层（`/api/overview`、`/api/settings/set|reset`、`/api/events` SSE——事件：`status` / `roster` / `settings`、静态托管）；
- `src/web/`：React 面板（v2 版式：悬浮胶囊边栏 + 96px 服务名主体 + 设置五列表格；唯一蓝强调；Fira Sans / Fira Code 本地打包）。
- `tests/`：`*.test.ts` = 服务端（node --test）；`tests/web/` = 面板组件测试（vitest + jsdom，
  `harness.ts` 提供假 EventSource 与 fetch 路由桩——SSE/fetch 在边界处替换，App 代码零改动；`tsconfig.test.json` 让测试进类型检查）。
- `design/`：版式样张（specimen.html + specimen.css，纯静态、供版式评审；经 `:19531` 查看需先拷到 `dist/web/`）。

## 设计说明（节选）

- 样式基线取自 ui-ux-pro-max 技能库的 Minimal/Swiss 企业工具条目（库内无 “Swedish” 词条；配色为瑞典色组合：瑞典蓝 #005B99 / 芥末黄 / 法伦红）。
- 有意偏差：首个连接设置中心失败不退出（持续退避重连），与其余服务“引导错误立刻可见”不同——面板优先可用性。
