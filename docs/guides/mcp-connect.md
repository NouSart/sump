# 把 MCP 接到远程 / 先做接入检查

> **读者**：要把 MCP 从本机搬到跨设备、或接一个外部 MCP Server 的人 ｜ **读完能做**：配好 HTTPS 入口、信任私有 CA、在不执行工具的前提下验证契约 ｜ **最后校验**：2026-10-30

服务本身的职责、配置项与契约见 [mcp/README.md](../../mcp/README.md)。

## 1. 入口 HTTPS

证书使用 PEM 格式，证书的 SAN（允许使用的主机名 / IP）**必须包含客户端实际访问的地址**。证书链与私钥成对提供；启动时读取，替换后需重启。应用不安装系统证书、不生成防火墙规则。

```powershell
go run . -addr 127.0.0.1:9902 -center ws://127.0.0.1:9000/ws -http-addr 192.168.100.10:9903 -allow-remote -allowed-hosts 192.168.100.10:9903 -tls-cert ../private/server.pem -tls-key ../private/server-key.pem -config ../data/mcp.json -secret-file ../data/mcp-secret.json
```

- 上面的 IP 仅作示例，需换成实际受控网络地址。
- 先通过本机已有的设置面板给 MCP 设置 `http.token`，再以远程参数重启。
- 内部 WebSocket 与设置中心仍绑定回环；**跨设备流量只访问 MCP HTTPS 入口**。
- 私钥只保留在服务器上，勿提交或发给客户端。

## 2. 外连证书信任（私有 CA）

每个 remote 可以增加 `"ca_file":"../private/remote-ca.pem"`：

- 路径以进程工作目录为准，必须是 HTTPS。
- 配置后该目标**只**使用文件中的信任根；未配置时用系统默认根。
- 私有 CA 不加入系统证书库，也不会被其他 remote 继承。
- 仍检查有效期与主机名，**不提供跳过验证的开关**。
- 证书信任、remote 配置与 imports 均需重启生效。

## 3. 不执行工具的接入检查（推荐先跑这个）

```powershell
go run . -check-remotes -config ../data/mcp.json -secret-file ../data/mcp-secret.json -call-timeout 15s
```

- 不启动原生服务、不注册中心、不监听端口。
- 向显式配置的远端发出发现 / 目录读取请求，核对认证、当前协议版本与选定工具的输入输出 Schema，**不会发送 `tools/call`**。
- 完整检查共用 `-call-timeout` 时间预算，最多四项并发，按 action 排序输出 JSON。
- 退出码：`0` = 全部绑定核对成功；`1` = 至少一项失败或本地配置读不出。**没有 imports 会报错**，而不是误报通过。
- 输出只含 remote / action / tool、状态和固定错误信息，**不输出凭据**。部分 TLS / 网络失败统一归为连接失败，需结合证书与地址排查。

检查通过只说明**当时**可连接且契约匹配，不能证明业务执行权限、业务结果或后续网络稳定；正常调用时仍会重新核对权限和契约。检查失败后不要自动重试有副作用的业务调用。

## 4. 两个运行场景

**外部 MCP 应用读一个文件**：应用以文件 ID 调用 `files__read` → MCP 核对导出与当前名册 → 直接请求 files → files 读自己的数据后回包 → MCP 翻译成文本和结构化结果给原应用。**设置中心不转发文件正文。**

**SUMP 调外部工具**：SUMP 调 `lookup` → MCP 按绑定找到 `catalog/lookup`，用该目标凭据核对契约并调用。若另一个独立调用先完成，它先回给自己的请求，**不等待较慢的调用**。工具是否需要执行审批由执行端负责，MCP 不绕过其权限控制。
