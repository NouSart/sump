# MCP 服务

> **读者**：要把 SUMP 的能力暴露给外部 MCP 应用、或把外部 MCP 工具接进 SUMP 的人 ｜ **读完能做**：配好能力映射、跑通双向调用与异步补报 ｜ **最后校验**：2026-10-30
>
> 远程入口 / 证书 / 接入检查见 [docs/guides/mcp-connect.md](../docs/guides/mcp-connect.md)；验收范围见 [docs/archive/mcp-acceptance-2026-10.md](../docs/archive/mcp-acceptance-2026-10.md)。

MCP 是一个独立的 SUMP 服务，提供双向协议翻译。设置中心只发现 `mcp` 本身；外部工具通过显式绑定导入，导入和对外发布相互独立。

支持同步与异步双向调用、HTTPS 远程接入和只读契约检查。协议基线是 MCP `2026-07-28` Stateless HTTP，使用官方 Go SDK `v1.8.0`。异步导入使用原生受理回执与补报；异步导出使用主体私有的临时观察资源。目前不提供 MCP Tasks、stdio、OAuth 或远程 shell。

## 架构

```text
外部 MCP Client ── HTTP /mcp + Bearer ──> Server Adapter
                                              │
                                     显式映射、输入检查、结果转换
                                              │
                                       SUMP files.read

SUMP 调用方 ── WS /ws + MessagePack ──> mcp 的具名动作
                                              │
                                  导入绑定、Schema 核对、结果转换
                                              │
                                        Client Adapter
                                              │
                                   获准的外部 MCP Server
```

业务请求按名册直接访问目标。MCP 不参与 Reasoner 规划，不添加全局调度器，也不继承外部服务的下属能力。

### 批处理与任务控制

MCP 导入动作的 `Provide.Batchable` 保持 `false`。显式导入只允许经该绑定调用，不自动授权 batch 代为调用；batch 对同步和异步导入均返回 `BATCH_NOT_BATCHABLE`，不会提交外部执行。这不影响多个直接调用并发运行。若以后需要批处理，须另外定义结果与取消的责任边界。

集成项目的 Reasoner 按来源服务、trace 与 item_id 关联补报，并保留原任务身份。派发时先登记，支持补报早于受理回执；最终结果去重。新版回执中的 `kind` 用于区分一次性任务与常驻任务。用户暂停期间仍接收结果，但不据此继续派发工具；恢复后消费已收到的结果。`user_hold` 控制推理推进，不等于取消远端执行。

| 文件 | 职责 |
| --- | --- |
| `main.go` | 参数与进程生命周期 |
| `server/http.go` | MCP HTTP、逐请求认证、Host/Origin、传输限制 |
| `server/server.go` | SUMP 注册、导出准入、调用关联、生命周期与错误上报 |
| `server/config.go` | 显式绑定配置与有效快照 |
| `server/bindings.go` | 原生能力的字段级翻译、严格 JSON、输出裁切 |
| `server/imports.go` | 外部连接、固定工具映射、Schema 核对、单次调用与结果转换 |
| `server/preflight.go` / `diagnostics.go` | 只读连接、认证与工具契约核对，不提交工具执行 |
| `server/async_imports.go` | 异步导入、取消本地等待、最终补报与有限收尾 |
| `server/async_exports.go` / `jobs.go` | 原生异步导出、主体私有观察、资源读取与计时取消 |

复用 `protocol`、`service`、MessagePack；JSON Schema 校验使用 SDK 已依赖的 `jsonschema-go`。没有新增数据库或前端。

## 启动

集成项目的 `scripts/run-local.ps1` 已包含 MCP。单独启动时先运行设置中心，在 `mcp/` 执行：

```powershell
go run . -addr 127.0.0.1:9902 -center ws://127.0.0.1:9000/ws -http-addr 127.0.0.1:9903 -config ../data/mcp.json -secret-file ../data/mcp-secret.json
```

默认 HTTP 入口为 `http://127.0.0.1:9903/mcp`，内部入口为 `ws://127.0.0.1:9902/ws`。所有相对路径以进程工作目录为准。配置缺失时不导入、不导出任何工具，外部认证仍强制要求。

通过项目**已有设置面板**选择 `mcp`：

- `http.token`：供外部客户端访问本服务的随机 Bearer 凭据，32–4096 字节，不含空白。
- `remote.<id>.token`：该外部服务器独立的 Bearer 凭据。只对启动配置中声明的 remote 注册此设置。
- 只读 `name`、`addr`、`center`、`http.addr`、`config.path`：显示启动参数。

秘密由公共服务库保存在服务自己的文件中，设置中心只展示是否已配置。入口 `http.token` 使用 `HandleSecretWithCommit`：先校验并保存，再发布凭据身份；保存失败时保留原凭据及其任务访问权。该文件不是加密保险箱；不要提交到仓库、放入普通 JSON 配置或作为工具参数。入站凭据不会转发给外部服务器。

## 显式配置

最小导出配置：

```json
{"exports":["files__read"]}
```

双向配置示例见 `config.example.json`。其中的 loopback 地址是需要替换的示例，`lookup` 是用于联调的工具契约，不是内置业务。输入与输出 Schema 必须与选定远端的声明一致。

`exports` 可热更新，每秒检查一次。未知工具、重复字段、无效配置保持上一份有效配置，并经设置中心上报本服务配置故障。

`imports` 和 `remotes` 是启动时的固定声明，修改需要重启；混合更新不会部分生效。运行中删除配置文件只撤销导出，已有导入仍使用启动声明；停止某个导入需修改配置并重启，或对要求凭据的 remote 清空秘密项。清空凭据不能撤回已发出的操作。

## 输入输出契约

### 外部 MCP → SUMP

| MCP 工具 | 原生目标 | 输入 | 成功结果 |
| --- | --- | --- | --- |
| `files__read` | `files.read` | `{"id":"已有文件ID","offset?":起始字符,"limit?":本次最多字符}` | `id`、`filename`、`text`、`truncated`、`total`、`next`、`warnings` |

只有 `exports` 包含工具且当前名册的目标动作是 `tool:true`、`async` 与绑定一致时才可见、可调用。`files__read` 是同步动作，`timer__sleep` 是异步动作。客户端不能指定内部 `from/to/boss`。参数必须是对象；拒绝重复键、未知字段、空 ID、负数 `offset`、非正 `limit`、非 UTF-8、过深嵌套及过长 ID。

`offset` 是字符（rune）偏移，`limit` 是本次最多返回的字符数且仍受 files 的 `read_max_chars` 上限约束；省略时从开头读。结果里 `total` 是全文长度，`next` 非 0 表示还有下一块，续读时把 `next` 当 `offset`。`warnings` 非空表示结果不完整（例如某张工作表损坏）；`truncated:true` 而 `next` 缺失或为 0 表示没有可续读的下一块。

入口在每个请求上验证凭据，不创建应用层会话。客户端按新协议发送请求元信息及 `MCP-Protocol-Version`、`Mcp-Method` 等必要头部。无需旧式初始化；不提供旧协议兼容通道。结果标为 `private`、TTL 0，HTTP 使用 `no-store`。

MCP 发出新的原生调用 ID/trace，以 `from=mcp,boss=mcp` 直接调用 files。返回 `CallToolResult`，结构化结果同时提供 JSON 文本。文件文本超限时按 UTF-8 边界裁切，设置 `truncated:true`，并把 `next` 改成“被裁掉的位置”（而不是上游原来的下一块），避免续读跳过中间正文；元数据不能裁切，超限时报错。原服务的 `truncated`、`total`、`warnings` 均保留；`next` 在上游未触发本地裁剪时原样保留。

参数或权限拒绝返回 `isError:true`；派发后超时或无法确认结果，明确返回结果未知，不自动重发。撤销在本地准入前生效；准入后的调用可能继续抵达原服务，不能据此宣称远端已停止。

### SUMP → 外部 MCP

配置中的每个 `imports.action` 注册为 `mcp` 自己的工具。`async` 省略或为 `false` 时采用同步调用；设为 `true` 时固定声明 `Provide.Async=true`，需要重启生效。同步调用例如：

```text
jump.to = mcp
jump.action = lookup
jump.input = MessagePack({"query":"天气工具"})
```

它固定指向配置的 remote/tool，调用方不能通过参数换地址、工具或凭据。工具目录只用于核对选定工具，不自动导入目录中的其他能力。

依次进行：输入 Schema 检查 → 获取目标专属凭据及其版本 → 连接并核对协议版本 → 分页读取目录并核对输入/输出 Schema → 再次核对取消及凭据状态 → 单次 `tools/call`。

凭据值与版本来自公共服务库的同一次读取。成功更换或清空凭据使此前准备、尚未通过最后派发检查的调用失效，即使随后改回原值；保存失败、重复保存当前值或更新其他 remote 的凭据不使本调用失效。只读预检使用同样的版本检查。通过最后检查表示本地准入，之后的轮换不能保证撤回已经准入或发送的操作。版本仅在本进程内有效，不增加持久任务状态。

每次调用持有独立 SDK 调用连接，共享 HTTP 连接池。协议回退、工具消失、Schema 变化、目录重复或分页超限时不调用工具。重定向、自动 OAuth 和工具重放均未启用。

成功翻译示例：

```json
{
  "phase":"completed",
  "is_error":false,
  "content":[{"type":"text","text":"found:天气工具"}],
  "structured_content":{"value":"found:天气工具"}
}
```

| 情况 | SUMP `response.ok` | 结果 |
| --- | --- | --- |
| 工具成功 | `true` | `phase=completed, is_error=false` |
| 远端工具明确报错 | `true` | `phase=failed, is_error=true`，保留错误内容 |
| 输入、认证、目录或契约不符 | `false` | 明确工具尚未提交 |
| 调用发出后丢失结果 | `false` | `MCP_OUTCOME_UNKNOWN`，不重试 |
| 要求交互补充输入 | `false` | `MCP_INPUT_REQUIRED`，不自动填充或续发 |
| 结果不符合声明或超限 | `false` | 明确执行可能已经发生 |

原生响应由骨架保留原请求的 ID、trace 与返回目标，不以 trace 单独作为调用标识。远端内容始终是工具数据；翻译器不会把它转成系统指令。

导入必须声明对象输入 Schema。`output_schema` 可省略，以接入只返回文本的标准工具；省略表示已审核的远端也不声明输出 Schema，并非忽略远端契约。若远端后来增加输出 Schema，原绑定仍拒绝漂移。配置声明输出 Schema 时，远端声明必须一致，成功结果必须包含符合该声明的结构化数据。任意 JSON 数字在 SDK 中经过 `float64`；超出安全整数范围 `±(2^53-1)` 的整数拒绝翻译。需要精确大整数时，接口应使用字符串。本实现不承诺任意精度十进制无损。

## 异步调用

### 导入：先受理，再向原调用方补报

在指定 import 中设置 `"async":true`。输入校验、凭据检查和容量预留成功后返回：

```json
{"item_id":"本次调用句柄","phase":"accepted"}
```

后台工作使用服务生命周期和 `-async-timeout`，不依赖已返回的入站请求 Context。目录和 Schema 核对仍在真正调用前完成；远端不可用、契约变化等可以在受理后以失败补报返回。工具调用最多派发一次。

原生最终补报沿原 `from/trace/boss` 返回，使用现有 `report_result`：

```text
item_id = 回执句柄
phase = completed / failed / outcome_unknown
text = 给模型读取的工具结果文字（最长 8 KiB 加截断提示）
data = 已校验的结构化翻译结果
final = true
```

`outcome_unknown` 表示派发后没有确认最终结果，`final:true` 结束的是本地观察，不代表远端已停。此时不重放工具，当前外部 HTTP 调用也没有晚到回调或自动恢复机制。补报接收方不可达时报告交付故障，不重新执行工具；没有持久补报队列。

调用方可向 MCP 的原生 `cancel_import` 提交 `{item_id}`。只有原 `from` 能取消对应本地等待，返回 `status=observation_cancelled` 或 `unknown`，并明确 `remote_outcome=unknown`。此控制动作没有作为模型工具或对外导出，不能据此声称支持可靠远端停止。

### 导出：计时器与私有结果资源

显式启用：

```json
{"exports":["files__read","timer__sleep","timer__cancel"]}
```

| 工具 | 输入 | 行为 |
| --- | --- | --- |
| `timer__sleep` | `{ms,note?}` | 100–600000 毫秒，备注不超过 200 个 Unicode 字符；返回 `job_id`、`resource_uri` 和观察快照 |
| `timer__cancel` | `{job_id}` | 只取消当前主体的已关联计时；不接受任意原生 item_id |

例如调用 `timer__sleep({ms:1000,note:"提醒"})` 后，使用标准 `resources/read` 读取返回的 `sump://jobs/<job_id>`。客户端需要支持资源读取；不自动发起模型轮次。手写 HTTP 时，资源读取的 `Mcp-Name` 头须填写完整 URI。

快照中 `state` 为 `reserved`、`accepted`、`unknown` 或 `final`；`phase` 是原生结果阶段（如 `done`、`canceled`），`final` 表示已有确定终态。资源请求与原工具请求可以是不同 HTTP 连接，身份归属不依赖 SDK 会话。

每次导出派发前分配唯一 trace 并预留记录，补报按来源服务、trace、回执 item_id 校验。回执前的消息有界暂存，收到回执后精确匹配；回执丢失进入 unknown 时仍允许该唯一来源/trace 的晚到报告建立结果。确定终态不被重复终态或后续进度覆盖。

计时取消必须通过 `timer__cancel` 的当前显式权限并核对 job 主体。原生返回 `canceled` 才确认取消；返回 `unknown` 不能推断已执行或已停止。取消使用独立的 4 个控制槽，查询不占业务执行槽，避免工作槽满时无法查询或取消。

默认保留最多 128 条观察（包含终态），每条含早到消息的编码预算 32 KiB，总预算 8 MiB，每条最多 8 个早到报告；这不是 Go 堆内存的精确值。文本裁切带 `truncated`，不能合法转换或超限的结构化数据明确拒绝。观察超时默认 2 分钟变为 unknown；记录从创建起 15 分钟后过期。过期释放本地额度而不取消原生任务，重启也不恢复记录。

可用 `-async-timeout`、`-delivery-timeout`、`-job-ttl`、`-max-jobs` 调整相应限制；`job-ttl` 必须大于 `async-timeout`。删除导出配置阻止新调用与控制，但原主体仍可读取已产生的观察；成功更换或撤销入口 token 后旧主体不能继续读取，替换凭据不会继承旧记录。成功轮换后再改回旧 token，也不会恢复旧任务权限；重复保存当前 token 则保留当前主体。

### 原生接收端

集成项目对 Reasoner 做了必要接缝修复：异步派发前登记待回执；以来源、trace、item_id 匹配；只为真实待回执调用缓存有界早到报告；确定终态去重。旧任务结果保留原任务归属，不注入新任务上下文。没有修改 Reasoner 的规划策略或引入通用多任务调度器。独立 MCP 模块接入其他调用方时，该调用方也必须正确处理早到补报。

## 范围、故障与限制

- 当前为单主人预配置 Bearer；共享凭据不代表多用户数据隔离。SUMP 内部仍是可信网络，`from` 只是路由身份。不要将内部 WS 直接暴露公网。
- 对外 HTTP 默认仅 loopback。非回环监听同时要求 `-allow-remote`、已配置的 token 与证书/私钥；服务直接提供 TLS 1.2+。使用反向代理时，明文后端必须留在 loopback。精确设置 `-allowed-hosts`。这仍不是完整公网部署方案：未提供 OAuth、多用户授权和持久恢复。默认拒绝带 Origin 的浏览器请求，不提供跨域前端。
- 外连只允许固定 HTTPS 地址；明文 HTTP 仅允许字面量 loopback IP。凭据按 remote 分离，禁止重定向，不使用隐式系统代理。没有通用 URL 代理或自动网络发现。
- 默认业务在途总量 8，每个 HTTP 凭据主体 / 内部调用者 2，每个外部目标 4。异步回执不会释放额度；导入持有到本地工作及有界补报结束，导出持有到确认终态或观察记录过期。满额直接拒绝。超时只结束本地等待，不证明远端操作结束；这些数值不是远端实际运行任务上限。
- 默认 HTTP 请求体 256 KiB、HTTP 在途 32、内部 WS 消息 1 MiB、导入输入及远端响应与翻译结果 128 KiB、同步等待 10 秒。配置 64 KiB，最多 16 个 remote / 64 个 import。响应大小限制作用在实际读取过程中。
- 工具拒绝、目标离线和业务错误反馈给调用方；配置损坏、协议适配失败和本服务监听故障使用原生 `Report`。同一个配置错误不每秒重复上报。
- 认证轮换不会强制回滚在途工作；HTTP 取消、服务退出也不等于远端执行已取消。不提供持久队列或 exactly-once 执行保证。

## 远程入口、证书与接入检查

非回环监听、TLS、私有 CA、`-check-remotes` 接入检查与两个运行场景见 [docs/guides/mcp-connect.md](../docs/guides/mcp-connect.md)。**未设 `http.token` 时拒绝所有外部请求。**

## 开发核验

```text
cd mcp
go test ./... -count=1
go vet ./...
cd ../tests
go test . -run '^TestMCP(Bidirectional|RemoteTLSPreparation)$' -count=1 -timeout 180s
```

单元测试包含独立 raw HTTP、官方 SDK 客户端、认证撤销、配置、读取限额、并发隔离、Schema 漂移、超时不重试、早到/迟到补报、私有资源、取消与退出。集成测试启动真实 settings-center/files/timer/mcp 进程，用独立进程的 HTTP 外部 fixture 验证同步和异步导入，并独立构造 JSON-RPC 验证文件导出、计时结果和取消。慢调用用屏障保持阻塞，先验证快调用完成，再放行慢调用；这不代表高负载性能基准。Reasoner 另有真实 WebSocket 工具在回执前补报的验证。HTTPS 集成测试在本机启动真实 TLS 服务进程并使用临时私有 CA，检查只读预检、双向调用和异步结果。CI 为 MCP 配置测试、静态检查和竞态检查，是否通过以当前提交的运行结果为准。完整范围与限制见 [验收契约](../docs/archive/mcp-acceptance-2026-10.md)。

参考：[MCP 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28)、[官方 Go SDK v1.8.0](https://github.com/modelcontextprotocol/go-sdk/tree/v1.8.0)、[JSON Schema 库](https://github.com/google/jsonschema-go)。具体依赖锁定在 `go.mod` / `go.sum`；保留各依赖原许可证。
