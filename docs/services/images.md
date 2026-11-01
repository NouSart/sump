# images 服务

> **读者**：要接线图片能力、或排查「图为什么取不到」的人 ｜ **读完能做**：知道它提供什么、怎么调、数据在哪 ｜ **状态**：有效 ｜ **最后校验**：2026-11-01

图片的**权威副本与唯一存放处**：二进制落盘、元数据进 SQLite。别的服务不自己存图，也不缓存图。

## 职责

- 收图（URL 或 base64）并生成 id；id 一证两用：数据库主键 + 磁盘文件名。
- 按 id 给图：默认给**缩略图**（长边上限可调），要细节就按矩形取原图那一块；整份原图只给服务间调用。
- 上限由 `max_mb` 管；超限明确拒绝，不静默截断。

## 端口

默认 `127.0.0.1:9401`（`scripts/run-local.ps1` 的 `-ImageAddr`）。只开 WS，不提供 HTTP 业务。

## 动作

| 动作 | 输入 | 输出 | 谁用 |
| --- | --- | --- | --- |
| `save` | `{url}` 或 `{data(base64), mime}` | `{id, mime, size}` | LLM 工具（从 URL 收图）；入口服务（qq 收到图片） |
| `fetch` | `{id, x1?, y1?, x2?, y2?, max_side?}` | `{id, mime, data(base64), size, original_width, original_height, width, height, x1, y1, x2, y2, downscaled, data_bytes}` | LLM 工具（默认缩略图；给矩形则取该区域原图） |
| `fetch_full` | `{id}` | `{id, mime, data(base64), size}` | **仅服务间**：reasoner 把用户刚发的图内联给模型（不暴露给模型——原图会撑爆上下文） |

以 `provides`（`images/server/server.go`）为准，上表是同一份声明的摘要。

## 设置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `max_mb` | 32 | 单张图上限（MB） |
| `db` / `dir` / `name` / `addr` / `center` | 启动参数 | 只读项，进面板只为看得见 |

## 数据

- SQLite（`-db`）：`images(id, mime, path, size, created_at)`。
- 二进制目录（`-dir`）：文件名就是 id。
- 两者**必须一起备份**：只搬库会得到一堆指向空文件的记录。

## 依赖

- 不主动调任何服务（纯被调方）。
- 被 `reasoner`（`fetch_full` 内联图片）、`qq`（`save` 收图）、`frontend`（`save` 上传、`fetch` 看附件）调用。

## 怎么测

```powershell
cd images; go test ./... -count=1
```

- `tests/store_test.go`：两张图互不覆盖（固定文件名会让第二张顶掉第一张，而 `Load` 照旧成功）。
- `tests/service_test.go` / `settings_test.go`：动作面、错误路径、`max_mb` 热改。
- 跨服务链路（qq 收图 → 模型看图）见根目录 `tests/qq_e2e_test.go`。
