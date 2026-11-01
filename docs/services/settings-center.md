# settings-center 服务

> **读者**：想知道「服务怎么被发现、设置怎么下发」的人 ｜ **读完能做**：知道名册谁维护、设置项从哪来、离线怎么判 ｜ **状态**：有效 ｜ **最后校验**：2026-11-01

**中心**：名册（DNS 式发现）+ 设置项登记与下发 + 服务健康（心跳扫描）。它不转发业务流量——
各服务拿到地址后**直连**，中心只当地址簿。

## 职责

- 收注册（`register`）与心跳，维护全量名册，变更时向所有连接广播新名册。
- 汇总每个服务自报的设置项（含秘密项的「有没有值」，**值永不上线**），接受改 / 重置并下发。
- 判离线：按 `sweep_ms` 扫描，未声明心跳的服务用 `heartbeat_fallback_ms` 兜底。
- 收 `report`（服务自报的异常，例如跳转未送达）。

## 端口

默认 `127.0.0.1:9000`（`scripts/run-local.ps1` 的 `-CenterAddr`）。所有服务都连它，**先起它**。

## 动作

| 动作 | 输入 | 输出 |
| --- | --- | --- |
| `ping` | — | `pong`（连通性测试） |
| `roster` | — | 最新全量服务清单（`{services, revision}`） |
| `report` | `{kind?, message}` | 回执（中心记日志并交给消费挂点） |

骨架注册的通用设置动作也由它应答：`list_settings` / `set_setting` / `reset_setting` / `report_setting` /
`configure`（`configure` 是中心 → 服务的下发方向）。

> 中心**不走** `service` 骨架的 `Handle`，自己实现动作分发——`tests/provides_gate_test.go` 对这类模块
> 用弱检查（声明过的动作名必须在源码里出现），见 `tests/provides_gate_test.go` 的 `ownDispatcher`。

## 设置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `sweep_ms` | 5000 | 离线扫描间隔（毫秒） |
| `heartbeat_fallback_ms` | 15000 | 未声明心跳的服务判离线时的兜底间隔 |
| `addr` / `settings` | 启动参数 | 只读项：监听地址、设置清单路径 |

## 数据

- **设置清单文件**（`-settings`，默认 `data/settings.json`）：各服务设置项的当前值（秘密项只存「有没有」）。
  这是权威副本，重启后据此恢复下发。
- 名册只在内存（服务重启会重新注册，不需要持久化）。

## 依赖

- 不主动调业务服务；只在设置变更时向目标服务发 `configure`。

## 怎么测

```powershell
cd settings-center; go test ./... -count=1
```

`tests/` 里覆盖：注册与名册广播、设置项增删改与下发、秘密项不回显、离线扫描与兜底心跳、
`report` 消费挂点。跨服务拉起顺序见根目录 `tests/e2e_test.go`。
