# frontend —— SUMP v2 前端服务（设置中心可视化面板）

TS 服务节点 + 浏览器面板：把设置中心的状态与设置可视化，写操作经中心转发（`configure` 下发到服务）。

```
浏览器 ──HTTP/JSON + SSE──▶ frontend(:9501) ──单条 WS（注册 / 心跳 / 名册 / 动作代转）──▶ settings-center(:9000)
```

- 面板本体注册为服务节点（在名册中可见自己）；**不持业务状态**：每次请求现翻成中心跳转，回执翻回 JSON。
- v1 范围：服务名册 + 设置查看 / 修改（中心自动下发 `configure`）/ 重置；名册与连接状态实时（SSE）。错误上报视图留待 v2。

## 命令

```powershell
npm install          # 安装依赖（Node ≥ 22.18）
npm run build        # 类型检查（tsc ×2）+ 服务端编译 + vite 构建（dist/server + dist/web）
npm test             # 协议夹具 / 中心链路 / HTTP API 测试（node --test，内置 TS 类型剥离）
npm start            # 启动（需先 build）：node dist/server/index.js -addr 127.0.0.1:9501 -center ws://127.0.0.1:9000/ws
```

常用参数：`-addr`（HTTP/WS 监听）、`-center`（设置中心地址）、`-name`（服务名，默认 frontend）、`-web`（静态目录，默认 `dist/web`）、`-heartbeat-ms`。

## 结构

- `src/server/protocol.ts`：协议层（信封 + MessagePack）——与 Go 侧逐字段对齐，`tests/protocol.test.ts` 用 Go 产出字节做锚定；
- `src/server/center.ts`：中心连接（注册 / 心跳 / 断线自愈 / 名册广播 / 调用的 `RemoteError` 区分）；
- `src/server/service.ts`：自己的 WS 端点（v1 未提供动作，一律 `unknown action` 回执——注册的地址必须真实可达）；
- `src/server/api.ts`：HTTP 层（`/api/overview`、`/api/settings/set|reset`、`/api/events` SSE、静态托管）；
- `src/web/`：React 面板（瑞士国际风格 + 瑞典旗色：12 栏网格、平涂色场——服务身份色编号牌（蓝/黄/红/绿按名哈希）、选中行整行铺色、黄底“已覆盖”；Fira Sans / Fira Code（@fontsource 本地打包）；零圆角零阴影、结构性横线；无顶栏 / 底栏——左栏 200px + 右侧表格化设置）。

## 设计说明（节选）

- 样式基线取自 ui-ux-pro-max 技能库的 Minimal/Swiss 企业工具条目（库内无 “Swedish” 词条；配色为瑞典色组合：瑞典蓝 #005B99 / 芥末黄 / 法伦红）。
- 有意偏差：首个连接设置中心失败不退出（持续退避重连），与其余服务“引导错误立刻可见”不同——面板优先可用性。
