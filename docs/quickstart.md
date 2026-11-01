# 快速开始

> **读者**：想在本机把 SUMP 跑起来的人 ｜ **读完能做**：起全栈、打开门户、跑通测试 ｜ **最后校验**：2026-10-30

## 0. 先看要求

| 依赖 | 版本 | 用来干什么 | 缺了会怎样 |
|---|---|---|---|
| Windows + PowerShell | PowerShell 5.1 或 pwsh | 启动脚本、审批服务 | 起不来 |
| Go | 1.26+ | 全部后端服务 | 起不来 |
| Node | ≥ 22.18（推荐 24） | 门户前端 | 起不来（首次运行会自动 `npm install` + `npm run build`） |
| Python | ≥ 3.12 | embedding 服务 | 能起来，但语义召回退化为 bigram 兜底 |
| DeepSeek 密钥 | — | 真正调用模型 | 服务能起，对话没法真的推理 |

## 1. 一条命令起全栈

```powershell
git clone https://github.com/NouSart/sump.git
cd sump
.\scripts\run-local.ps1
```

- 首次运行会自动装前端依赖、建 Python venv，需要几分钟；之后是秒级
- 门户：<http://127.0.0.1:9501/>
- `Ctrl+C` 停止（脚本会清理全部子进程）
- 数据落在 `data/`（`memory.db` 会话库 / `experience.db` 共享经验库 / `images.db` / `files.db` / `settings.json` / `images/` / `files/` / `models/`），不入库
- 首次升级会从旧 `memory.db` 里把长期记忆与经验网搬到 `experience.db`：**先复制一份 `memory.db.bak-<时间>`**，旧表只改名 `legacy_*`、不删字节

接真模型：

```powershell
$env:DEEPSEEK_API_KEY='sk-...'
.\scripts\run-local.ps1
```

## 2. 端口一览

| 服务 | 默认地址 | 对应启动参数 |
|---|---|---|
| settings-center | 127.0.0.1:9000 | `-CenterAddr` |
| reasoner | 127.0.0.1:9101 | `-ReasonerAddr` |
| memory | 127.0.0.1:9201 | `-MemoryAddr` |
| qq | 127.0.0.1:9301 | `-QQAddr` |
| images | 127.0.0.1:9401 | `-ImageAddr` |
| frontend（门户） | 127.0.0.1:9501 | `-FrontendAddr` |
| embedding | 127.0.0.1:9601 | `-EmbeddingAddr` |
| shell | 127.0.0.1:9701 | `-ShellAddr` |
| timer | 127.0.0.1:9801 | `-TimerAddr` |
| files | 127.0.0.1:9802 | `-FilesAddr` |
| approval | 127.0.0.1:9901 | `-ApprovalAddr` |
| mcp（内部 WS） | 127.0.0.1:9902 | `-MCPAddr` |
| mcp（外部 HTTP） | 127.0.0.1:9903 | `-MCPHTTPAddr` |
| batch | 127.0.0.1:9951 | `-BatchAddr` |

改端口示例：

```powershell
.\scripts\run-local.ps1 -CenterAddr 127.0.0.1:9001 -FrontendAddr 127.0.0.1:9502
```

## 3. 跑测试

测试不依赖外网（LLM 用假客户端，真模型冒烟测试要显式开关）。

```powershell
# 单个模块（推荐先跑一遍确认环境正常）
cd protocol; go test ./... -count=1; cd ..

# 跨服务真进程端到端（约 20 秒）
cd tests; go test ./... -count=1; cd ..

# 前端（类型检查 + 构建 + 测试）
cd frontend; npm install; npm test; cd ..

# embedding（Python）
cd embedding
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install -e ".[onnx,dev]"
.\.venv\Scripts\python.exe -m pytest tests -q
cd ..
```

## 4. 长期运行

`run-local.ps1` 是前台开发脚本，`Ctrl+C` 就全停、日志只在控制台。要长期挂机用守护脚本：

```powershell
.\scripts\serve.ps1                  # 前台守护：日志落 logs\sump-<日期>.log，退出即退避重拉
.\scripts\serve.ps1 -Once            # 只跑一次（排查用）
.\scripts\serve.ps1 -InstallTask     # 注册登录自启（无管理员权限时自动回落到「启动文件夹」）
.\scripts\serve.ps1 -UninstallTask   # 取消自启
```

守护只负责「拉起来」和「记日志」：单个服务崩溃由服务骨架自己重连中心，整栈退出才由守护重拉。

## 5. 常见的三件事

**改完代码想整体换掉整栈**

```powershell
.\scripts\reload-sump.ps1 -DryRun    # 先看会杀掉哪些进程
.\scripts\reload-sump.ps1            # 按依赖逆序停干净，再重新拉起并等端口就绪
```

从 SUMP 自己的对话里（shell 服务）触发时必须加 `-Detach`，否则重载会在杀掉 `shell.exe` 的瞬间把发起命令一起带走。

**端口被占用**

脚本会直接报错并指出残留进程（常见是 `shell.exe` / `sump-approval.exe`）：先跑一次 `reload-sump.ps1`，或换端口。

**接 QQ / 接 MCP**

```powershell
.\scripts\run-local.ps1 -Owner <你的QQ> -NapCatURL ws://<napcat地址>:3000 -NapCatToken <token>
```

QQ 连不上会自动重试，不影响其余服务；不填 `-Owner` 则拒绝所有 QQ 私聊。MCP 未设置入口 token 时会拒绝所有外部请求，配置方法见 [mcp/README.md](../mcp/README.md)。

## 6. 已知限制

上线前必须如实告知的限制清单在 [README.md](../README.md#已知限制上线时如实告知)，含「门户没有文件上传入口」「语义召回需要模型文件」「门户只有一个会话」等。
