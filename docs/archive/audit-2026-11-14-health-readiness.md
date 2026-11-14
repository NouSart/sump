# 审计快照：SPv2.1 /readyz 健康探针改动（2026-11-14）

> 读者：review 这次未提交改动、决定能不能说「就绪语义已落地」的人 ｜ 读完能做：知道哪些承诺有测试、哪些没有、下一步先补什么 ｜ 状态：审计快照，不修改产品代码 ｜ 最后校验：2026-11-14
>
> 被审计对象：工作区未提交改动（`src/spv2/health.go` 新增 + 6 个文件修改，见 §1）。
> 审计标准：[spv2.md](../concepts/spv2.md)（工作区现行版；判定只引其规范条文，其他文档与历史一律不可信，见下方「基准修订」）。
> 本报告是一次性历史快照，不是规范；结论以证据为准。

>
> **基准修订（2026-11-14，委托人指令）**：本报告此后的判定**只以 `docs/concepts/spv2.md` 的规范条文为准**。仓库其他文档（AGENTS.md / protocol.md / README / IMPLEMENTATION.md / CONTRIBUTING / TODO / 提交历史）**一律视为不可信线索**：可以记录其说法，但不得作为验收依据，也不得用来豁免或解释 spv2.md。§2.2 的 M6/M7 与「AGENTS.md 冲突」措辞降为附录观察；M5 改由 spv2.md §12.1 直接判定。

## 0. 工程口径（已知 / 设计输出 / deadline）

- **已知（基线）**：git HEAD = `61c4a79`；工作区有 6 个已修改文件 + 1 个新增文件 `src/spv2/health.go`。环境：Go 1.26.5 / Node 24.18 / Python 3.14 + `src/embedding/.venv`。
- **设计输出**：本报告 + 可复跑命令 + 异变结果；不改产品代码、不顺手重构（异变临时改源码后按字节恢复，见 §5 校验）。
- **deadline**：本次会话内收口；两个只读子审计（spec-audit / test-audit）已回收，不再等待、不再扩大范围。

## 1. 结论摘要

1. **全测试面本身是绿的**（17 个 Go 模块、根 e2e、frontend 153、embedding 48+1 skip、evals 自测），但 `src/spv2` 存在一条**真 flake**：首次全量跑失败 1 次，定向 `-count=100` 再失败 3 次（§3.2）。
2. **现有两套异变全杀伤**：evals `scripts/mutation` 25/25、`scripts/guardmutation` 37/37（本次复跑，0 存活、0 无效、0 过期）。
3. **新 health 改动的 7 个用例只守住了主路径**：我补了 14 条有效异变，**5 条被杀死、9 条存活**（§4）。存活项正好对应「文档承诺了但测试测不到」的形态：latency 实测、checks 省略、未知状态 fail-closed、fail 压过 degraded、探针 ctx、`/healthz` 不执行探针、多次注入叠加、panic 日志。
4. **生产不可达**：11 个生产服务既不注入探针（[spv2_bridge.go](../../src/service/spv2_bridge.go#L77) 只传 MaxMessageBytes/Logger），也不挂 `/healthz` `/readyz` `/metrics`（[service.go](../../src/service/service.go#L184-L186) 只挂 `/spv2`）；生产 `health` 永远 `ok`。spec §7.6 的新措辞虽限定「参考实现」，但 IMPLEMENTATION.md 删掉原来的「已知未做」却没有补上这条生产边界（§2.2）。
5. **按 spv2.md 标准还有几处与本次改动无关、但审计必须报的验收缺口**：§12.3 最小指标集只实现 4/23 且无 §12.4 契约测试；§3.5/§10 的 `max_stream`、`max_stream_buffer`、seq 缺口检测未实现；§17.3 三个错误码（`cancelled` / `unavailable` / `stalled`）无测试引用（§6）。

**现在不能说的话**（除非先补 §7 的 P0 项）：
- 「`/readyz` 就绪语义已落地」——只在参考实现里，且探针 hang 时端点会挂住；
- 「checks 省略已验证」「latency 已实测」「未知状态会 fail」——测试测不到；
- 「最小指标集已实现」「每个服务都有 HTTP 观测端点」——生产不成立。

## 2. 发现（按许愿式形态）

### 2.1 掩耳盗铃：断言看着在验，其实验不到

| # | 严重度 | 位置 | 问题 |
|---|---|---|---|
| M1 | 高 | [http_test.go](../../src/spv2/http_test.go#L118-L122) | `LatencyMS < 0` 对非负 int64 恒假；把 [health.go](../../src/spv2/health.go#L44) 的实测改成恒 0，7 个用例全绿（异变 H1 存活）。README 却说会「填充 `checks[].name/status/latency_ms`」（[README.md](../../src/spv2/README.md#L100)）。 |
| M2 | 高 | [http_test.go](../../src/spv2/http_test.go#L62-L64)、[:85-87](../../src/spv2/http_test.go#L85-L87) | 「无探针时 checks 省略」只断言解码后 `Checks == nil`；`"checks": null` 同样解码为 nil。去掉 `omitempty`（异变 H12）测试不红。实测当前 raw JSON 确实省略，但承诺没被守住。 |
| M3 | 中 | [http_test.go](../../src/spv2/http_test.go#L148-L170) | panic 用例用 `io.Discard` 藏日志，只断 fail+503；删掉 [health.go](../../src/spv2/health.go#L56) 的 panic 日志，测试不红（异变 H6c 存活）。 |
| M4 | 中 | [http_test.go](../../src/spv2/http_test.go#L67-L88) | `/healthz` 用例只断响应形状；在 healthz 里执行探针并丢弃结果（异变 H11）仍绿。而「纯存活检查」的关键恰是不碰依赖。 |

### 2.2 声东击西 / 狐假虎威：库能力代替产品能力，假探针证明真就绪

| # | 严重度 | 位置 | 问题 |
|---|---|---|---|
| M5 | 高 | [spv2_bridge.go](../../src/service/spv2_bridge.go#L77)、[service.go](../../src/service/service.go#L184-L186) | 生产骨架不传 `WithHealthChecks`，也不挂 `Mux()`：11 个生产服务的 `health` 恒 `ok`，`/readyz` 在生产根本不存在（全仓 `readyz` 只出现在 `src/spv2` 的实现与测试里）。spec §12.1 却写「每个服务必须实现 HTTP /healthz、/readyz、/metrics」（[spv2.md](../concepts/spv2.md#L756)），与 AGENTS.md「所有服务只暴露 /spv2」互相矛盾。 |
| M6 | 中 | [IMPLEMENTATION.md](../../src/spv2/IMPLEMENTATION.md#L20-L37)、[:46](../../src/spv2/IMPLEMENTATION.md#L46)、[:72-79](../../src/spv2/IMPLEMENTATION.md#L72-L79) | 文件地图没加 `health.go`；§5 删掉旧「/readyz 还没有就绪语义」一条，却没写「生产未接入探针 / 生产不挂 HTTP 观测面」。读者只能看到 P0「已完成」。 |
| M7 | 中 | [README.md](../../src/spv2/README.md#L165) | 库 README 的「已落地」清单把「含健康探针与就绪语义」写成事实，却不写适用范围——产品读者会读成生产能力。 |

### 2.3 概念漂移：契约里没定义的语义被测试钉成契约

| # | 严重度 | 位置 | 问题 |
|---|---|---|---|
| M8 | 中 | [health.go](../../src/spv2/health.go#L34-L43) | 未知 per-check 状态被静默改写成 `fail`；spec §7.6 只定义 overall 的 `ok/degraded/fail`，没定义 per-check 枚举与未知值策略。异变 H2b（未知当 ok、fail 仍 fail）存活——策略既无文字也无测试。 |
| M9 | 中 | [health.go](../../src/spv2/health.go#L26-L27)、[http_test.go](../../src/spv2/http_test.go#L167) | `check_%d` 是契约外发明的兜底名（spec 说探针自己给 name），测试把它钉死成 `check_1`；异变 H9b 杀死了，但这些「发明」现在有了回归绑定。 |
| M10 | 低 | [server.go](../../src/spv2/server.go#L454-L465) | `HealthCheck.LatencyMS` 带 `omitempty`：0ms 在线上省略；spec §7.6 示例里是 `latency_ms:0`。 |
| M11 | 低 | [http.go](../../src/spv2/http.go#L25) | `/healthz` 硬编码字符串 `"ok"`，不用 `StatusOK`。 |

### 2.4 多此一举 / 死代码

| # | 严重度 | 位置 | 问题 |
|---|---|---|---|
| M12 | 低 | [health.go](../../src/spv2/health.go#L51-L53)、[server.go](../../src/spv2/server.go#L90-L94) | `checker == nil` 分支不可达（构造函数已拒 nil）；异变 H5（改成 ok）存活，证明是死防御。 |
| M13 | 低 | [health.go](../../src/spv2/health.go#L20-L47) | 探针串行执行、无 per-probe deadline，也不看调用方 ctx 是否取消（异变 H4 把 ctx 换成 Background 存活）。实测阻塞探针会让 `/readyz` 超过客户端 300ms 超时仍不返回——spec §9.3「到点必须回 deadline_exceeded，不得挂住」对 health 不成立。 |
| M14 | 低 | [http_test.go](../../src/spv2/http_test.go#L54) 等 | 新用例 `http.Get` 无客户端超时；探针一旦 hang，包会拖到整包超时而不是给出指向根因的失败。 |

### 2.5 指鹿为马：测试/基建把未验证说成已验证

| # | 严重度 | 位置 | 问题 |
|---|---|---|---|
| M15 | 中 | [mutation/main.go](../../testing/evals/scripts/mutation/main.go#L94-L103)、[:126](../../testing/evals/scripts/mutation/main.go#L126) | evals 的 mutation 脚本把**任何非 0 退出**都记「杀死」，且测试没有 `-timeout`：编译失败、超时也会算「被测试抓住」。guardmutation 已把这类分出「无效」，两套口径不一致——「25 条全杀死」只能读成「至少非绿」。 |
| M16 | 中 | 同上 | 原地改真实源码 + defer 还原，没有锁；并行工作区里会覆盖别人的编辑，SIGKILL 下源码会停在变异态。本次跑在含未提交改动的工作区上，靠事后哈希 / `git diff` 复核（§5）兜底这个风险。 |
| M17 | 中 | [README.md](../../testing/evals/README.md#L157-L158) | evals README 的覆盖率数字标注「当前」，但最后校验是 2026-11-01，之后 evals 代码被重构；数字是旧快照，且 `-cover` 没有 CI 门禁。 |

## 3. 测试面覆盖（本次实测）

### 3.1 全量结果

| 面 | 命令 | 结果 |
|---|---|---|
| 17 个 Go 模块 | 逐模块 `go test ./... -count=1` | 全部 exit=0（共 269.1s；根 e2e `testing/tests` 120.1s） |
| 根 e2e | 含 Portal/DeliveryFailure/QQ/MCP/settings/shell/timer 等 25+ 用例 | 通过 |
| frontend | `npm test` + `npm run build`（typecheck） | 153 tests / 18 files 通过；build 通过 |
| embedding | `.venv` 下 `pytest tests -q` | 48 passed, 1 skipped |
| evals 自测 | `go test ./...` | 通过 |
| 静态 | `gofmt -l`（含未跟踪 `health.go`） | clean |
| race | `go test -race .`（src/spv2） | 通过，无 data race |

**但首轮全量跑不是全绿**：`TestServerUndecodableFrameCountedAndLogged` 失败 1 次（`server_isolation_test.go:214: 丢弃帧没有落日志: ""`）。

### 3.2 flake 根因（已复现）

- [server.go](../../src/spv2/server.go#L220-L227) 的 `countDropped` **先加指标、后写日志**；
- [server_isolation_test.go](../../src/spv2/server_isolation_test.go#L54-L68) 的 `waitReasonCounter` 看到计数就返回；
- 断言随即读日志（[:212-219](../../src/spv2/server_isolation_test.go#L212-L219)）→ 计数与日志之间的调度窗口被撞上就红。
- 证据：定向 `-count=100` 出现 3 次失败；其中一条失败打印的日志其实已经非空——因为 `Fatalf` 的参数在进入分支之后才求值，正好记录了这个竞态窗口（`if` 里读到空、报错时已写入）。
- 同类用 `waitReasonCounter` 的另外两个用例（invalid / max_body 记账）有同样风险。
- 修复二选一：`countDropped` 先 `logger.Printf` 再 `Counter`（保持「日志单条、指标聚合」语义不变）；或测试等到日志出现（不要只看指标）。

### 3.3 新 health 用例清单（7 个）

`http_test.go` 新增：无探针 ok、healthz 忽略 checks、ok+degraded→degraded、fail→503、panic→fail、WS health 用探针、nil 探针拒绝。**能证明主路径，不能证明 §4 的 9 个边界。**

## 4. 异变测试

### 4.1 既有两套（复跑）

- `cd testing/evals; go run ./scripts/mutation`：**25 条，杀死 25，存活 0，跳过 0**。
- `go run ./scripts/guardmutation`：**37 条，杀死 37，存活 0，无效 0，过期 0**。
- 注意 §2.5 M15/M16：数字成立但口径偏弱（非 0 即杀死、无 timeout、原地改源码）。

### 4.2 本次补的 health 异变（14 条有效：5 杀死 / 9 存活）

| # | 异变（改完仍编译、说得通） | 结果 | 说明 |
|---|---|---|---|
| H1 | `LatencyMS` 恒 0（仍非负） | **存活** | M1：没有任何断言检查实测值 |
| H2b | 未知 per-check 状态当 ok、fail 仍 fail | **存活** | M8：未知状态策略无测试 |
| H3 | degraded 无条件覆盖 fail | **存活** | 没有 fail+degraded 顺序用例 |
| H4 | 探针改收 `context.Background()` | **存活** | 无 ctx 传播/取消断言 |
| H5 | 不可达的 nil 分支改返回 ok | **存活** | 死代码（M12） |
| H6c | 删除 panic 日志 | **存活** | M3：日志无断言 |
| H7 | 无探针早退改 degraded | 杀死 | 已有用例守住 |
| H8 | panic 收成 ok | 杀死 | 已有用例守住 |
| H9b | 取消 `check_%d` 兜底名 | 杀死 | panic 用例守住（但把发明钉成契约，M9） |
| H10 | `/readyz` 对 fail 也回 200 | 杀死 | 已有用例守住 |
| H11 | `/healthz` 执行探针并丢弃结果 | **存活** | M4：healthz「不碰依赖」无断言 |
| H12 | checks 去掉 `omitempty`（无探针输出 `null`） | **存活** | M2：「省略」只测解码结果 |
| H13 | 多次 `WithHealthChecks` 覆盖而非叠加 | **存活** | 全部用例只注入一次 |
| H14 | WS `health` 忽略探针直接 ok | 杀死 | 已有用例守住 |

另：H2/H9/H6b 三条初版因分支合并/编译失败被排除并重写；其中 2 条「编译失败」若用仓库脚本会**被记成杀死**——正好是 M15 的现场例子。

### 4.3 动态确认（临时测试，跑完即删）

- 当前实现确实：无探针时 raw JSON 省略 `checks`；`/healthz` 执行探针 0 次；未知状态→fail；fail+degraded→fail；sleep 25ms 探针 latency=25ms。**行为本身基本正确，缺的是守它的测试。**
- 阻塞探针：`/readyz` 在客户端 300ms 超时前不返回（无内部 deadline）。

## 5. 审计纪律校验

- 异变全部临时改源码后按字节恢复：`health.go` / `http.go` / `server.go` SHA256 与改前一致；`git status` 与审计开始时一致（仍只有原来 6 个修改 + `health.go` 未跟踪 + 无关未跟踪文件）；`gofmt -l` clean。
- 未修改任何产品代码与现有测试；未削弱任何异变或断言。

## 6. 按 spv2.md 的验收缺口（与本次改动无关，但标准要求）

| # | 严重度 | 标准 | 现状与证据 |
|---|---|---|---|
| S1 | 高 | §12.3/§12.4 最小指标集（23 个）+ 每服务契约测试 | 全仓只实现 4/23（`sump_requests_total` / `request_duration_seconds` / `inflight_requests` / `dropped_total`），另 3 个名字不在清单；**没有任何最小指标集契约测试**（[spv2.md](../concepts/spv2.md#L770-L785)）。 |
| S2 | 高 | §12.1 每个服务 health/metrics + HTTP /healthz /readyz /metrics | 生产全部不挂（见 M5）；与 AGENTS.md「只暴露 /spv2」冲突，需要产品裁决，不能两头都说。 |
| S3 | 中 | §3.5/§10 流式上限：`max_stream`、`max_stream_buffer`、seq 缺口计数 | `MaxStream` 只声明不执行（[card.go](../../src/spv2/card.go#L30-L34)）；`sump_stream_gap_total` 只出现在 docs；stream 取消未实现。§17.1「buffer 超限」无测试。 |
| S4 | 中 | §17.3 每个 error code 至少一条负向测试 | `cancelled` / `unavailable` / `stalled` 在 `src/spv2` 测试里零引用；`deadline_exceeded`/`internal`/`resource_exhausted` 的 retryable 与 §8 表不一致（如 [store.go](../../src/spv2/store.go#L302)、[server.go](../../src/spv2/server.go#L163)、[:244](../../src/spv2/server.go#L244)）。 |
| S5 | 中 | §5.1 连接后第一帧 hello | `Dial` 不自动 hello，服务端也不要求首帧 hello；版本协商退化为逐帧校验（参考实现亦如此）。 |
| S6 | 中 | §17.2 红队六项 | 部分散落已有单测（unknown method、to 校验、authority、provides↔handler 契约），但没有成组验收；「works 表唯一」「done 后 cancel/stalled→conflict」「agent 消息冒充用户批准」没有端到端负向测试。 |
| S7 | 中 | §12.1 metrics 格式/标签 | `/metrics` 声明 OpenMetrics，实际是 Prometheus 0.0.4 文本且无 `# EOF`；直方图无 `+Inf` 桶；`method` 标签直接取自对端字符串，可被打爆基数（[server.go](../../src/spv2/server.go#L187)）。§12.2 白名单不含 `reason` 而 §12.3 又要求 `sump_dropped_total{reason}`——标准内部矛盾。 |
| S8 | 中 | §6/§7 work/lease/authority 在生产生效 | 生产没有 works 表、没有 work 级 authority：deliver/message/context 在生产是「自称 from + 业务字段」，spec §6.5 矩阵只能按参考实现验收；IMPLEMENTATION.md 已声明 work/lease 未接生产，但 spec §15「迁移已全部完成」的措辞会被读成整体完成。 |

## 7. 建议顺序

**P0（在说「完成」之前）**
1. 修 §3.2 flake（先日志后指标，或测试等日志）；把同类 3 个用例一起看。
2. 补 health 测试：latency 用 sleep 探针断言下限；checks 省略改断 raw JSON 无键；补「全 ok ⇒ ok」「fail+degraded ⇒ fail」「未知 status ⇒ fail」「多次 WithHealthChecks 叠加」「healthz 不执行探针（计数探针）」「panic 日志」「ctx 传播」。
3. 决定生产观测面：要么在 `src/service` 接探针并挂 Mux（并把 AGENTS.md 的「只暴露 /spv2」改成条件句），要么在 spec/IMPLEMENTATION 明写「HTTP 观测面仅参考实现、生产不提供」。二选一，不能含糊。

**P1**
4. 把 health 的关键异变固化进 `guardmutation`（至少 H1/H2b/H3/H11/H12/H13 六条），并按 guardmutation 口径分类，避免 M15 那类「编译失败=杀死」。
5. 给 mutation 脚本加 `-timeout`、把 `[build failed]` 归「无效」、与 guardmutation 口径对齐；加锁或文档化「不要并行跑异变」。
6. 补 §12.3 最小指标集契约测试（缺一个就红），再逐服务补指标。

**P2**
7. 文档同步：IMPLEMENTATION 文件地图加 `health.go`；§5 补「生产观测未接入」；README 的「已落地」加适用范围；evals README 的「当前覆盖率」加日期或删掉。
8. §6 表里的 S3/S4/S5/S6 按 §17.3 补负向测试后再宣称达标。

---


## 8. 基准修订后的判定矩阵（仅 spv2.md + 代码/测试事实）

> 口径：条款只引 `docs/concepts/spv2.md`；「证据」只写代码路径/测试名，不引其他文档。判定：符合 / 部分 / 不符 / 待证。

| # | spv2.md 条款 | 判定 | 代码/测试事实（只读核对 + 本次实测） |
|---|---|---|---|
| A1 | §3.3 未知 type 回 invalid_argument；§8 未知 method 回 not_found | 符合（参考实现） | `server.go` 未知 method 回 `CodeNotFound`；`validate.go` 拒未知 type；`transport_test.go`、`server_guards_test.go` 有测试。 |
| A2 | §3.5 max_header / max_body / max_stream / max_stream_buffer | **不符** | 只有 max_body 被执行并测试；`max_header`、`max_stream` 无执行点，`max_stream_buffer` 全仓无实现。 |
| A3 | §4.4 `to.id == 自己` 与 capability → not_supported | 符合（参考实现） | `validate.go` 的 `ValidateTo` 校验目标；capability 回 `CodeNotSupported`；有测试。 |
| A4 | §5.1 连接后第一帧 hello | **不符** | `client.go` 的 `Dial` 不自动 hello，`server.go` 不要求首帧 hello；版本协商退化为逐帧校验。 |
| A5 | §5.2 卡字段、method 的 shape/tool/input schema | 符合（参考实现） | `card.go` 校验卡（重复 method、非法 shape、tool 缺 schema 均拒）；`transport_test.go` 有正反例。 |
| A6 | §6 work 对象/状态机/§6.5 authority/§6.8 work 表只在 memory | **不符（生产）** | 参考实现有 WorkStore/MemStore/SQLiteStore 与 authority 辅助函数及测试；生产 memory 没有 works/work_messages 表，work 只当 trace 字符串。 |
| A7 | §7 核心 method：message / context / deliver / cancel / renew | **不符（生产）** | 参考链路实现并有 e2e；生产入口用的是 user_message / recall / resume 等另一套词汇，生产 reasoner/memory 不实现 §7 语义。 |
| A8 | §7.6 health：status ok/degraded/fail、checks 可选、探针、degraded 不等于死 | **部分** | 参考实现代码成立（health.go/http.go + 本次动态确认）；7 个新用例漏 9 个边界（§4.2 异变）。生产无探针注入，health 恒 ok（`src/service/spv2_bridge.go:77`）。 |
| A9 | §8 错误码固定表、retryable 与表一致、details 给机器 | **不符** | 码表齐全；`deadline_exceeded` / `internal` / `resource_exhausted` 实际回 `retryable=false`，与 §8 表不一致；生产 ReplyError 的 details 全 nil；§17.3 负向测试缺 cancelled / unavailable / stalled。 |
| A10 | §9.1/§9.2 幂等与重试门控 | **部分** | 参考实现幂等缓存按卡声明门控且有测试；生产没有任何 method 声明 `idempotent:true`，可重试族不存在。 |
| A11 | §9.3 deadline：到点回 deadline_exceeded、不得挂住 | **不符** | 服务端只把 ctx 交给 handler，没有 watchdog；探针/阻塞 handler 可越过 deadline（本次实测阻塞探针下 `/readyz` 超过 300ms 客户端超时仍不返回）。 |
| A12 | §10 流式：多块/seq/end/cancel/有界 buffer/seq 缺口计数 | **不符** | 多块、seq、end、错误终态有实现与测试；buffer 上限、`sump_stream_gap_total`、stream cancel 未实现。 |
| A13 | §11 A2A：claim/ack/nack/retry/dead-letter、owner-only deliver | **部分** | Store 层 claim/ack/nack 有 SQLite 测试；链上无 nack 出口、无邀请/participant 暴露、failed 不通知 owner（deferred）。 |
| A14 | §12.1 每服务 health/metrics + HTTP /healthz /readyz /metrics | **不符（生产）** | 参考实现 `Mux()` 有四个端点；生产骨架只挂 `/spv2`（`src/service/service.go:184-186`），11 个服务无 HTTP 观测端点。 |
| A15 | §12.3 最小指标集 + §12.4 每服务契约测试 | **不符** | 全仓只实现 §12.3 的 4/23（requests_total / request_duration_seconds / inflight_requests / dropped_total）；没有任何最小指标集契约测试。 |
| A16 | §13 authority 只来自 work 角色/capability；agent message 永远 none；participant 不能 deliver | **不符（生产）** | 参考实现有 authority 函数与单测；生产 deliver（frontend/qq）不读 from/authority/work，reasoner message 不校验 owner/participant。 |
| A17 | §17.2 红队六项 / §17.3 每错误码负向测试 | **不符** | 红队项只有零散单测（unknown method、to 校验、authority、provides↔handler 契约），无成组验收；负向测试缺 3 个错误码。 |
| A18 | §18 not designed 必须回 not_supported | **待证（部分）** | capability → not_supported 有实现与测试；spawn/cross-owner 在参考实现没有可调用入口，生产也没有对应 method，无可验证的拒绝路径。 |

### 8.1 按新基准，原报告哪些结论需要改写

- **保留（且不再依赖其他文档）**：M1–M5、M8–M16、S1–S4、S7；其中 M5 的判定依据改为 §12.1（不再提 AGENTS.md 冲突）；S2 同理。
- **降级为附录观察（不作判定）**：M6、M7（IMPLEMENTATION.md/README 的说法不可信，是否与实现一致只是仓库治理问题）；S5 的「hello 未强制」保留为 A4 代码事实，但不再引用 README/IMPLEMENTATION 的自我描述。
- **不变**：§3.2 flake、§4 异变结果、§3.1 测试面结果——这些是直接执行/改码得到的事实，与文档可信度无关。
- **对本次改动的净结论（仅按 spv2.md）**：参考实现的 §7.6 能力基本成立，但 §7.6/§12.1 要求的「探针 + 就绪语义」没有进入任何生产服务；7 个新测试未守住 9 个 §7.6 边界。**因此不能以“本次改动”声称 §7.6 或 §12.1 达标。**
---


## 9. 修复进度（第一轮，2026-11-14）

> 口径不变：只以 spv2.md 为准；以下每条都有代码 + 测试，异变验证见 §9.3。

### 9.1 已落地（代码 + 测试）

| spv2.md 条款 | 修复内容 | 测试 |
|---|---|---|
| §9.3 deadline | `runCall` 把 handler 放独立 goroutine，到期先回 `deadline_exceeded`（retryable=true）；`SendStream` 拒绝已回终态之后的 late chunk；`deadline_exceeded`/`resource_exhausted`/`internal` 的 retryable 与 §8 表对齐（store/sqlite 的 deadline 同样改 true） | `server_deadline_test.go`：超时回执、无视 ctx 的 handler 被兜住、late stream 被丢弃、retryable 断言 |
| §7.6 health | 新增 `WithHealthTimeout`（默认 5s），探针在 ctx 到期时按 fail 计，健康端点不再被挂住的依赖拖死；`latency_ms` 不再 omitempty；`/healthz` 用 `StatusOK` 常量 | `health_test.go`：探针实测 latency、raw JSON 缺 `checks` 键、fail 压过 degraded（两种顺序）、未知状态 fail-closed、多次 `WithHealthChecks` 叠加、healthz 不执行探针、panic 落日志、探针拿到带 deadline 的 ctx、挂住探针时 `/readyz` 在上限内 503 |
| §12.1 生产观测面 | `src/service` 的 `Start` 改为挂 `Mux()`：生产服务真实监听面提供 `/healthz` `/readyz` `/metrics`；新增 `Service.AddHealthCheck` 与骨架自带 `center` 探针（未连中心时 `/readyz`=503） | `src/service/tests/observability_test.go`：对真实 `Start` 的监听面断言三端点、`center`+注入探针都在跑 |
| §12.3/§12.4 骨架指标 | 传输域：`sump_request_bytes` / `sump_response_bytes` / `sump_errors_total`（含未知 method、reject、panic→internal、事件错误、缓存回放）；连接域：`sump_connection_state` / `sump_reconnects_total` / `sump_heartbeat_age_seconds`（heartbeat 循环每秒刷新） | 同一集成测试在真实请求+一条未知 method 之后，对 `/metrics` 断言 9 个指标名全部存在；`src/spv2` 既有 metrics 测试 |
| §12.4 契约测试 | 集成测试里把「骨架最小指标集」变成缺一个就红的断言，不再是文档承诺 | 同上 |
| §12.3 LLM/工具指标（生产 reasoner） | `llm.Client` 改为返回 `ChatResult{Message,Usage}`（DeepSeek 解析 provider usage、Fake 可配 Usage）；reasoner 所有 LLM 调用收敛到 `chatLLM` 统一记 `sump_llm_requests_total{model,outcome}` / `sump_llm_latency_seconds` / `sump_llm_tokens_total{model,kind}`；`runToolCall` 记 `sump_tool_calls_total{tool,outcome}` / `sump_tool_duration_seconds`；`service.Service.Metrics()` 公开注册表；指标白名单补 `tool` label | 新增 `reasoner/server/metrics_test.go`（纯函数直接喂 Registry）与 `reasoner/tests/metrics_test.go`（真实服务 + fake LLM：工具调用后从 `/metrics` 断言 LLM/工具系列）；`llm` 包补 `TestFakeReportsUsage` |
| §12.3 work/A2A/lease 指标（参考实现） | `ref/memory` 埋 `sump_work_active{status}`（状态计数归零显式写 0，不留旧值）、`sump_a2a_messages_total{state=pending/claimed/delivered}`、`sump_lease_exhausted_total`；指标白名单按 §12.3 具体表放行 `state`/`kind`（§12.2 通用白名单与 §12.3 的矛盾按更具体者执行） | 新增 `ref/metrics_test.go`：context 后 open=1、cancel 后 open=0/cancelled=1、A2A 三状态、lease 耗尽；另加 `TestMetricsSpecLabelsAllowed` 把白名单断言放回 metrics 所在包（此前 guard 会「存活」，因为断言长在 ref 包） |
| §5.1 hello 握手（Go/TS/Python 全部服务端 + 客户端） | `src/spv2.Client` 在第一次 `Call`/`Send`/`Stream` 前自动发 hello（target 取该帧 `to`），校验 `protocol` 与卡片服务名；服务端 `conn` 增加握手状态，**首帧不是 hello 的 call/event 一律拒绝**（call 回 `invalid_argument`，其它帧计数+日志，新增 `dropped{reason=no_hello}`）；`handleHello` 成功才标记完成 | 新增 `hello_test.go`：未握手的 call/event 被拒且不进 handler、握手后同连接恢复、客户端 Call/Send 首帧自动 hello；4 条异变全部杀死。**跨语言**：TS 前端 `dialSPv2Once`/`SPv2Session` 自动握手、`createSPv2WSS` 回服务卡；Python embedding 中心客户端先 hello、服务端已有 hello 处理；Go/TS/Python 客户端与服务端全部适配：Go 服务端与 TS `createSPv2WSS`、Python `NativeEndpoint` 均强制首帧 hello（未握手 call 回 invalid_argument，事件计数/日志）；前端 stub 与 Python 假中心、裸连接测试全部迁移为先 hello |
| §6/§6.8 生产 work 账本（第一片） | memory 库新增 `works`/`work_participants` 表与状态机（复用 `spv2.CanTransition`）；`recall` 按 `frame.work` 幂等登记 work（owner/agent/entry/lease 默认值，登记失败 fail-closed）；新增 §6.8 内部 method `work_get`/`lease_charge`/`lease_release`（不进 provides、不暴露给模型），租约扣减原子、过期→`deadline_exceeded`、预算不足→`resource_exhausted`、stalled→`stalled`、Release clamp 到初始上限；最小 authority：`owner`/`runtime` 或 work 的 agent/owner/entry ID，否则 `permission_denied` | 新增 `memory/store/work_test.go`（默认值/幂等/非法状态转移/扣减/归还/过期/stalled）与 `memory/tests/work_test.go`（真实 recall→work_get→charge→release→越权拒绝）；5 条异变全部杀死，清单 68→73 |
| §6.4/§6.3 reasoner 租约接线（第二片） | 每次真正执行远端工具前先向 memory 的 `lease_charge` 扣 `tool_calls=1`（扣不到就不执行，错误回给模型）；轮次边界经 `work_update` 推进 work：新任务取代旧任务→`cancelled`、交付完成→`done`、LLM 失败→`waiting`（可重试）；memory `work_get` 同时回 `remaining` 余额；recall 登记即 `running` | `reasoner/tests/lease_test.go`（扣一次租约再执行、扣租约失败工具 0 次执行且模型看到原文）、`reasoner/server/lease_test.go`（同包：失败不放行、boardClose 推 done/waiting）；根 e2e `TestInferChainE2E` 用固定 work id 在真 memory 账本核对 `done` + `tool_calls=49`；3 条异变全杀死 |
| §6.4 token 租约 + §17.2 done 前置条件 | 每次 LLM 调用成功后按 provider usage 扣 token 租约（total 缺省用 prompt+completion；usage=0 不凭空记账；扣不到→中止本轮并置 stalled）；`boardClose` 只有 pending=0 才标 `done`，否则 `waiting`；`sump_work_stalled_total{reason=deadline}` 只对真转移计数 | `reasoner/server/lease_test.go`（token 扣减/零 usage/失败上抛、有在飞工具不能 done）、`reasoner/tests/token_lease_test.go`（真服务替身：两轮 LLM 共扣 30 token）；2 条异变杀死，清单 78→**80 条**，`-only=src/reasoner` 实跑 **11/11 杀死** |
| §6.8 A2A 队列 + work cancel/renew | memory 新增 `work_messages` 表（正文只存 `messages` 一份、`body_ref` 指针）、`message_post/claim/ack/nack`（claim 按 `to_id` 过滤、租约过期可重领、nack 超上限进 `failed` dead-letter）、`work_cancel`（owner/runtime，终态回 conflict）、`work_renew`（owner/runtime，刷新 deadline/budget、stalled→running、禁止改到过去）；新增低基数指标 `sump_a2a_messages_total{state}`、`sump_work_transitions_total{to}` | `memory/store/a2a_test.go`（入队/领取/ack/nack 重排与 dead-letter/终态不可入队/renew 校验）、`memory/tests/a2a_test.go`（真实 recall 后走 post→claim→ack、越权 cancel 被拒、runtime cancel 成功、重复 cancel conflict）；4 条异变全部杀死，清单 80→**84 条** |
| §3.5 max_header + §12.3 审批指标 | `Limits.MaxHeader`（默认 16 KiB，可 `WithMaxHeader`/卡片覆盖）；服务端在 body 检查后按「header-only 序列化长度」拒绝超限帧（`resource_exhausted` + `dropped{reason=max_header}`），不等大 body 解完；审批域新增 `sump_approval_decisions_total{decision}` 与 `sump_approval_wait_seconds`（用户真正裁决时计数/观察，未 Start 的服务 no-op），label 白名单补 `decision` | `spv2/max_header_test.go`（默认值/超限拒绝/正常头不误伤）、`approval/server/native_test.go`（真服务 metrics 快照断言）；4 条异变杀死，清单 84→**88 条**，`-only=src/spv2`/`-only=src/approval` 实跑见 r20 结果 |
| §7.2 生产 `context`（memory 侧） | memory 新增核心 method `context`：同步返回组装后的 `{messages, work, task}`，work 在唯一登记处 ensure（重复调用幂等）、空 messages 明确报错；recall 与 context 共用抽出的 `assemble`，不再各写一份组装逻辑；程序化声明进 memory provides（tool:false） | `memory/tests/context_test.go`（messages/work/task 返回、work 幂等、空 messages 报错）；memory 全模块测试绿 |
| §7.2 reasoner 启动链切到 `context` | `user_message` 改为**同步**向 memory 要 context（`fetchContext`），拿到组装后的 messages/task 才开黑板；memory 失败直接回错误、**不调 LLM**；`resume` 原生 method 与 `startMemoryRound` 已删除（测试断言旧 method 必须 `not_found`）；`startTask` 抽出统一开任务逻辑 | reasoner/server 与 tests 全绿（含 resume 拒绝→已删除的断言迁移）、approval-integration 改走 user_message+context 后 5.2s 绿、根 e2e 145.8s 绿 |
| §12.3 frontend / embedding 观测面 | 前端门户新增 `/healthz`、`/readyz`（中心连接为 fail）、`/metrics`（OpenMetrics：requests_total{method,status}、request_duration 直方图、inflight gauge、errors_total，含 # EOF）；Python embedding HTTP 侧新增 `/healthz`、`/readyz`（注册状态）、`/metrics`（embed_requests/texts/errors_total + uptime，含 # EOF），指标只记真实计数 | 前端 node 87/87（含观测端点用例）、vitest 153/153；embedding 49 passed/1 skipped（test_observability）；dist 重建 |
| §7.1 `message` + §6.5 context authority | reasoner 新增 `message` 核心 method（tool:true）：human 必须 `authority=owner`（与 user_message 同源走 context 主链）、agent 必须 `authority=none` 且只能注入**进行中的任务**（无任务拒绝）、service/越权一律 `permission_denied`；memory `context` 增加来源校验：创建时要求 `from.kind=agent/service`（或 owner/runtime），已存在 work 必须由它的 agent/owner/entry 取，终态 work 回 `conflict` | reasoner/server/message_test.go（权限矩阵 + 注入形状）、reasoner/tests/message_test.go（human 走 context 主链）、memory/tests/context_test.go（越权 context/终态 context）；reasoner 全模块绿 |
| §12.3 生产 `queue_depth` | memory A2A 队列新增 `sump_queue_depth{state}`：post/claim/ack/nack 后按真实库计数刷新 pending/claimed/delivered/failed，纯函数形态 `recordQueueDepthTo` 便于同包单测；不含 work/actor 等高基数 label | `memory/tests/a2a_test.go`（post→pending 1、claim→pending 0/claimed 1）、`memory/server/work_test.go`（注册表快照断言）；新增 1 条异变（队列深度不再写注册表）并纳入 memory 子集 |
| §7.2 删除旧 recall/resume 链 | memory `recall` method / `nativeRecall` / `fireResume` / `RecallPayload` / `ResumePayload` 全部删除；memory provides 撤掉 recall；memory/tests 与 testing/tests 的 recall 探针全部迁移到 `context` 同步返回（stubCaller.contextUntilReady、e2e recallOnce/recallEventually 直接解 context 回执），根 e2e 的 recall→resume 观察者替身删除 | 全量 17 模块测试 + 根 e2e 绿（迁移后）；`recall.*` 设置键名仍保留（下一轮改名 context.*，属兼容名残留） |
| §10 流取消（transport cancel） | spv2 服务端拦截「method=cancel 且 body.stream 非空」的传输层取消：释放该连接上对应 stream.id 的账本并回 `{cancelled:n}`（服务自己的 work cancel 不受影响）；客户端新增 `CancelStream(ctx, from, to, work, streamID)`：发 cancel、解回执、关闭本地流通道（streamMu 统一保护 streams 的读/删/关，避免 readLoop 与取消并发 close/send panic）；指标 `sump_stream_cancelled_total{outcome=transport}` | `spv2/stream_cancel_test.go`：第一帧流→取消返回 1、本地通道关闭、重复取消返回 0；3 条异变（不释放账本、不拦截、不关通道）纳入 spv2 子集 |
| §6.5 deliver 归属校验 | service 骨架新增 `CallWithRef`（from 由调用方给）；reasoner 的 deliver（正常交付/失败通知/异步补报）统一用 `agent:<name> + authority=runtime`；前端门户 deliver 先经 memory `work_get` 解析 work 行，纯函数 `checkDeliverAuthority` 只放行 work.agent+runtime / work.owner+owner，未配置校验/查不到 work/来源不符一律 `permission_denied`（fail-closed）；门户作为 entry 允许读自己那条 work | 前端 node 90/90（含按 work 校验的允许/拒绝用例与纯函数矩阵）、vitest 153/153；reasoner/tests 断言 deliver 帧 from 确为 agent:reasoner/runtime；前端异变（放宽 agent 校验）被 2 条测试抓到；Go 侧新增 deliverRef 守卫 |
| §17.2 红队六条成组验收 | `testing/tests/redteam_test.go` 一组六个 t.Run：掩耳盗铃（坏帧 dropped{undecodable}+断连、handler 错误进 errors_total）、声东击西（to 不符 invalid_argument 且 handler 不执行）、狐假虎威（普通身份 cancel work → permission_denied）、概念漂移（帧字段 msgpack 键与 spec 字段表逐一对齐 + 文档键名存在）、多此一举（生产服务源码里 works/work_messages 建表只允许 memory 一处，参考实现 src/spv2 明确排除并注明理由）、指鹿为马（done→cancel conflict、stalled→done conflict） | 组内六个子用例全绿；红队组首次运行即发现"参考实现不算生产服务"的口径需写清（已在用例注释固定） |
| §17.3 负向测试 | 明确未设计 feature 一律 `not_supported`：tenant 字段、capability 目标、delegated authority（帧校验层），spawn/drain/retire method（服务端分发层，且进 errors_total）；memory 工具契约测试：3 个 tool:true 均有 Input/Output schema、remember 缺必填参数 `invalid_argument`、重复 remember 幂等（created=false 同 id）；其余 error code/stream 缺块重复超 buffer/idempotency 由既有 spv2/memory 测试覆盖 | `spv2/negative_spec_test.go`（tenant/capability/delegated/spawn-drain-retire）、`memory/tests/tool_contract_test.go`；新增 2 条守卫（tenant 静默忽略、spawn 退化 not_found）纳入 spv2 子集 |
| 设置键 `recall.*` → `context.*` | memory 6 个设置项全部改名（history_limit/core_limit/core_max_chars/rel_limit/rel_max_chars/embedding），非别名兼容：旧键下发直接报未知键；前端语言包键同步改名；docs/concepts/protocol.md 的设置键清单同步 | memory tests 断言声明里无 `recall.` 前缀且旧键被拒；前端 node/vitest 与全量 Go e2e 绿；新增 1 条守卫（键偷偷退回 recall.*） |
| §6.8 A2A 调用方 + 消费方（最小闭环） | `message` 的 agent 路径支持目标 `work` 字段：带目标 work 时经 memory `message_post` 入队并回 `queued:true`（不假注入）；该 work 下次开任务时 `startTask` 先 `message_claim` 取回、注入黑板、逐条 `message_ack`（claim 失败不阻塞主轮但计数 `sump_a2a_claim_failures_total`）；memory `work_messages` 读取视图带正文（body 只存 messages 一份，JOIN 取回） | `reasoner/tests/a2a_loop_test.go`（入队→下次开任务注入模型上下文→队列清零/无未 ack）、同包 `A2APostClaimAckHooks`/`MessageAgentWithTargetWorkQueues`/`StartTaskInjectsA2AQueue`；memory store 测试断言 claim 回执带正文；2 条异变守卫纳入 reasoner 子集 |
| §7.1 入口统一 message | reasoner 删除 `user_message` method/provides/handler；`message` 新增 service 入口路径（service + authority=none + sender 必填 → 人类主链，结果归属=入口服务）；前端门户与 qq 全部改发 `message`（带 conversation + sender + 附件 id），主链内部改名 humanMessage；e2e/evals/approval-integration 调用全部迁移 | reasoner/qq/前端测试全绿；根 e2e 全绿（门户/qq 真实链路走 message）；旧 method 已不可用（provides 不再声明） |
| §8/§17.3 错误语义 | 新增 `RetryableByCode`（§8 表的机器可读副本）；handler 返回 `context.Canceled/DeadlineExceeded` 分别映射 `cancelled/deadline_exceeded`（此前塌缩成 `internal`）；非 `*Error` 的 retryable 按 §8 表兜底；`Dial` 失败映射 `unavailable`（retryable=true，不再裸网络错误）；`MemStore`/`SQLiteStore` 对已 `stalled` 的 work 扣租约返回 `stalled` | 新增 `error_semantics_test.go`：11 码 retryable 表、拨号失败 unavailable、ctx 错误映射、stalled 扣租约（两 store）；`cancelled/unavailable/stalled` 从此都有真实路径负向测试 |
| §12.1/§12.2 指标格式与基数 | `/metrics` 改为 OpenMetrics（正确 content-type、直方图 `+Inf` 桶、`# EOF`）；`Registry` 强制 label 白名单（`work/actor/tenant` 丢弃，`reason` 按 §12.3 例外保留）；服务端 method label 收敛到卡片/已注册 handler，未知 method 记 `other`，不再把对端可控字符串带进指标序列 | 新增 `metrics_format_test.go`：OpenMetrics 结构、label 白名单、未知 method 基数有界；既有 unknown-event 用例改为断言「原名不得进 label」 |
| 测试基建 flake | ① evals `StartStack` 起栈失败后换端口重试（最多 3 次，保留最后一次错误）；`TestClaimPort` 改为每次取新端口号。② 二进制缓存加**跨进程编译锁**（独占锁文件 + 超时回收 + 临时文件原子 rename + stamp 后写），修掉 `go test ./...` 并行包同时 `go build` 写同一个 exe 的抢文件问题 | 新增 `buildlock_test.go`（串行性 + 崩溃遗留锁回收）；`runner -count=2` 绿；evals 全包连跑 3 次绿（修复前全量跑出现过 `TestStackRoundTrip` 与 `TestRunEndToEnd` 两次偶发失败） |
| §3.5/§10 流式有界 | 服务端按连接执行 `max_stream`（默认取卡片或 32）与 `max_stream_buffer`（默认 1 MiB），发送侧 `seq` 必须从 1 逐 1 递增；接收侧对 seq 缺口/重复记 `sump_stream_gap_total{outcome=missing|duplicate}`；去掉 client.go 里 `var _ = http.StatusOK` 死占位 | 新增 `stream_limits_test.go`：并发流上限、单流 buffer 上限、跳号被拒、客户端缺口/重复记账 |

### 9.2 顺带修掉的真缺陷

- `countDropped` 曾先记指标后写日志：`server_isolation_test.go` 看到计数就返回，存在调度窗口 → 真 flake。已改为先日志后指标；`-count=300` 复跑 0 失败（修复前 100 次跑出 3 次失败）。
- 测试清理顺序：挂住探针的用例原来先注册 `close(block)` 再注册 `ts.Close`，LIFO 会让 `ts.Close` 等死；已改成「先关探针再关 server」。

### 9.3 异变验证（本轮新增/复跑）

- 复跑审计发现的原 9 条存活异变：**全部被新测试杀死**（H1 latency 恒 0、H2b 未知状态当 ok、H3 degraded 覆盖 fail、H4 ctx 换 Background、H6c 删 panic 日志、H11 healthz 跑探针、H12 去掉 checks omitempty、H13 多次注入被覆盖、H5 死分支已直接删除）。
- 新增守卫异变也已杀死：D1 关 watchdog、S1 关 late-stream 守卫、R1 deadline retryable 改 false、L1 关 HealthTimeout、L2 probeHealth 不 select、P1 center 恒 ok、P2 healthz 写 degraded、P3 Start 退回只挂 `/spv2`。
- 方法提醒：本轮两次「测试挂整包」被误记成杀死（探针阻塞导致 go test 超时），已按「无效（挂住）」重判并修掉测试清理顺序后复跑；今后异变跑必须带 `-timeout` 并区分「编译失败/挂住」与「断言失败」。
- 流式守卫：4 条新异变（关 max_stream、关 max_stream_buffer、关 seq 递增、关客户端缺口记账）全部被 `stream_limits_test.go` 杀死，并已加入 `guardmutation` 持久清单（37→41 条），由清单同步守卫与实跑共同验证。
- 指标守卫：5 条新异变（关 label 白名单、关 method 收敛、去 EOF、去 +Inf、退回 Prometheus 文本）全部被 `metrics_format_test.go` 杀死，并加入 `guardmutation` 清单；清单现 **46 条，实跑 46/46 杀死（0 存活/0 无效/0 过期）**。
- 错误语义守卫：6 条新异变（deadline retryable 表、ctx 取消映射、retryable 兜底、拨号 unavailable、两个 store 的 stalled 扣租约）全部杀死，并加入清单；清单现 **52 条，实跑 52/52 杀死（0 存活/0 无效/0 过期）**（首轮曾有一条因替换缺结尾换行被判「无效」，补换行后复跑绿）。
- 测试基建：`TestRunEndToEnd` 在上一轮全量跑里也偶发失败过一次（3.24s，无根因信息），与 `TestStackRoundTrip` 同属「并行评测测试进程共享 `data/evals/bin`」。已给二进制缓存加跨进程编译锁 + 原子落盘，evals 全包连跑 3 次绿。
- 参考实现指标守卫：6 条新异变（work_active、a2a pending/claimed/delivered、lease_exhausted、state label 白名单）全部杀死；清单 52→58 条，本轮 `-only=src/spv2` 实跑 **29/29 杀死**（0 存活/0 无效）。其中「state label」首轮曾存活：断言写在 `ref` 包、守卫按文件在 `./` 包跑——已把断言补到 `metrics_format_test.go`（同一包）后复跑杀死。
- reasoner 指标守卫：6 条新异变（LLM 请求计数、prompt token、LLM 延迟、工具错误计数、Fake usage、tool label 白名单）全部得到可编译错法并杀死；清单 58→64 条。
- hello 握手守卫：4 条新异变（服务端不要求首帧 hello、客户端 Call 不握手、hello 成功不标记、客户端 Send 不握手）全部杀死；清单 64→**68 条**，本轮 `-only=src/spv2` 实跑 **34/34 杀死**（0 存活/0 无效）。
- work 账本守卫：5 条新异变（recall 不登记 work、非法状态转移不拦、控制权不校验、stalled 仍可扣、Release 不 clamp）全部得到可编译错法并杀死；清单 68→**73 条**，`-only=src/memory` 实跑 **25/25 杀死**（0 存活/0 无效；含 5 条 work 守卫，已补同包断言）。
- 租约接线守卫：3 条新异变（执行工具前不扣租约、扣租约失败仍放行、交付后不推进 work done）全部杀死；清单 73→**76 条**，`-only=src/reasoner` 实跑 **9/9 杀死**（0 存活/0 无效；异变目标已改到同包可测的 `chargeBeforeTool`/`updateWorkStatus`）。
- 跨语言验证：`src/frontend` 的 node 测试 86/86、vitest 153/153 绿（stub 中心/目标服务补了 hello 回执）；embedding pytest 48 passed/1 skipped（假中心补 hello，断言改为「首帧 hello、register 在其后」）。
- 注意记录一条方法教训：最初两条异变把变量删成未使用，属于「编译失败=无效」而不是杀死；已改成仍能编译的错法（计数 delta→0、只记成功），并补了同包纯函数测试（`recordLLMTo`/`recordToolTo`），避免断言长在 `./tests` 包导致守卫按 `./server` 跑时存活。
- flake 修复后复跑：`runner` 包 `-count=2` 绿；evals 全包重复跑绿。全量 17 模块的最终验证见 §9.3c。
- 修复后复跑既有两套：evals `mutation` **25/25 杀死**、`guardmutation` **37/37 杀死（0 无效、0 过期）**。
- `guardmutation` 有一条守卫片段因本轮 server.go 的「未知 method」代码改动而命中 0 次（`TestGuardsMatchSource` 当场报红），已把清单同步到新片段并复跑；这条正好证明「清单与源码同步守卫」在工作。

### 9.3b 本轮（流式）验证与残余 flake

- `guardmutation` 扩到 **41 条，实跑 41/41 杀死（0 存活/0 无效/0 过期）**；4 条流式守卫由 `stream_limits_test.go` 直接杀死。
- 全量 17 模块：16 个同轮绿（含根 e2e 112.8s）；`testing/evals` 的 `TestStackRoundTrip` 在全量负载下偶发失败 1 次——该模块单独重跑绿、`runner` 包连跑 5 次全绿。根因是 FreePort 探测与绑定之间的跨进程窗口（并行跑多个评测包时可能被抢端口）：`StartStack` 已改为失败换端口重试（3 次、保留最后错误），`TestClaimPort` 也修掉了 `-count>1` 时全局端口表不复位的隔离缺陷，`-count=2` 复跑绿。
- 顺带暴露测试自身问题：`go test ./... -count=2` 下 `TestClaimPort` 失败（命令行「首次认领应成功」）——runner 的进程内端口分配表跨重复运行不复位，属测试隔离缺陷，记录待修。
### 9.3c 本轮最终验证

- 全量 17 模块（r4，246.4s）：**17/17 绿**（含根 e2e 108.3s、`testing/evals` 11.8s——上一轮复现的 `TestStackRoundTrip` 本轮未再出现，起栈端口重试生效）。
- `guardmutation` 52/52 杀死；gofmt clean、改动模块 vet clean、文档守卫绿。
- 第三轮最终全量（r6）：**17/17 绿（232.3s）**；第四轮最终全量（r7）：**17/17 绿（279.7s，含根 e2e 131.2s、evals 14.4s）**；evals 全包在编译锁修复后连跑 3 次绿。
- 第四轮起 `guardmutation` 采用按文件定位包（`-only=src/spv2` 实跑 29/29）；新增的 ref 指标守卫与 state label 守卫全部杀死。
- **第五轮（r8）：17/17 绿（271.5s，含根 e2e 134s）**；`guardmutation` 清单 64 条，本轮按文件定位包实跑 `-only=src/reasoner` 6/6、`-only=src/spv2` 30/30 全杀死（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **第六轮（r12）：17/17 绿（268.4s，含根 e2e 124s）**——hello 强制后全链路恢复绿；`guardmutation` 清单 68 条，本轮 `-only=src/spv2` 实跑 **34/34 杀死**（含 4 条 hello 守卫，0 存活/0 无效）gofmt/vet/文档守卫绿。
- **第七轮（r13）：17/17 绿（248.1s，含根 e2e 116s）**——TS/Python 服务端也强制 hello 后，跨语言全链路仍绿；前端 node 86/86 + vitest 153/153、embedding 48 passed/1 skipped、前端 dist 重建后 e2e 通过。
- **第八轮（r14）：17/17 绿（270.7s，含根 e2e 136.5s）**——生产 work 账本第一片落地（works/participants/lease + recall 登记 + work_get/lease_charge/lease_release + 最小 authority）；`guardmutation` 清单 68→**73 条**，`-only=src/memory` 实跑 **25/25 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **第九轮（r16）：17/17 绿（258.6s，含根 e2e 132.3s，approval-integration 6.4s）**——reasoner 工具链扣租约 + work done/waiting/cancelled 接线落地；`guardmutation` 清单 73→**76 条**，`-only=src/reasoner` 实跑 **9/9 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **第十轮（r17）：17/17 绿（264.5s，含根 e2e 131s）**——memory work sweep（§6.9 第 1 条 deadline→stalled）+ `sump_work_stalled_total{reason=deadline}` + `work_sweep` 内部 method 落地；`guardmutation` 清单 76→**78 条**，`-only=src/memory` 实跑 **27/27 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。§17 验收盘点见 §10（work deliver/cancel/renew、生产 A2A、生产流式 message、token 租约、完整红队仍缺）。
- **续轮（r18）：17/17 绿（246s，含根 e2e 115.7s）**——§6.4 token 租约（LLM usage 计费、失败中止并 stalled）+ §17.2 done 前置条件（pending>0 不许 done）落地；`guardmutation` 清单 78→**80 条**，`-only=src/reasoner` 实跑 **11/11 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r19）：17/17 绿（275.8s，含根 e2e 134.4s）**——生产 A2A（work_messages 表 + post/claim/ack/nack dead-letter）+ work cancel/renew + 指标 `sump_a2a_messages_total{state}`/`sump_work_transitions_total{to}` 落地；`guardmutation` 清单 80→**84 条**，`-only=src/memory` 实跑 **31/31 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r20）：17/17 绿（258.1s，含根 e2e 116.9s）**——§3.5 `max_header`（默认 16 KiB、超限 `resource_exhausted`+dropped 计数）+ §12.3 审批指标（`sump_approval_decisions_total{decision}`、`sump_approval_wait_seconds`、白名单补 decision）落地；`guardmutation` 清单 84→**88 条**，`-only=src/spv2` **37/37**、`-only=src/approval` **2/2** 杀死（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r21）：17/17 绿（258.1s，含根 e2e 117.2s）**——memory 生产 `context` method（同步返回 messages/work/task、work 幂等、recall/context 共用 assemble、provides 声明）落地；`guardmutation` 清单仍 **88 条**，`-only=src/memory` 实跑 **31/31 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r23）：17/17 绿（279s，含根 e2e 145.8s，approval-integration 6.4s）**——reasoner 启动链切到同步 `context`，`resume`/`startMemoryRound` 删除（旧 method 调用断言 not_found）；`guardmutation` 清单仍 **88 条**，`-only=src/reasoner` 实跑 **11/11 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r24）：17/17 绿（240.2s，含根 e2e 113.4s）**——frontend 与 Python embedding 的 `/healthz`/`/readyz`/`/metrics`（OpenMetrics）观测面落地；前端 node 87/87、vitest 153/153、embedding 49 passed/1 skipped；两条观测计数器异变均被各自测试抓到；gofmt/vet/文档守卫绿。
- **续轮（r25）：17/17 绿（257.4s，含根 e2e 116.8s）**——reasoner `message`（human=owner / agent=none、agent 只能注入进行中任务）+ memory `context` 来源校验与终态冲突落地；`guardmutation` 清单 88→**91 条**，`-only=src/reasoner` 实跑 **14/14 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r26）：17/17 绿（273.1s，含根 e2e 131.7s）**——生产 `sump_queue_depth{state}`（A2A post/claim/ack/nack 后真实刷新）落地；`guardmutation` 清单 91→**92 条**，`-only=src/memory` 实跑 **32/32 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r27）：17/17 绿（279.3s，含根 e2e 135.3s）**——旧 `recall`/`resume` 链彻底删除（memory method/handler/types/provides），memory 与根 e2e 探针全部迁到同步 `context`；`guardmutation` 清单仍 **92 条**，`-only=src/memory` 实跑 **32/32 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r28）：17/17 绿（255.1s，含根 e2e 113s）**——§10 transport 级 stream cancel（服务端释放 ledger + `{cancelled:n}`、客户端 `CancelStream` 关本地通道、重复取消回 0）落地；`guardmutation` 清单 92→**95 条**，`-only=src/spv2` 实跑 **40/40 杀死**（0 存活/0 无效）；gofmt/vet/文档守卫绿。
- **续轮（r29）：17/17 绿（252.4s，含根 e2e 113.1s）**——§6.5 deliver 归属落地：service `CallWithRef` + reasoner deliver 用 agent/runtime + 前端 entry 经 memory work_get 校验（fail-closed）；前端 node 90/90、vitest 153/153；`guardmutation` 清单 95→**96 条**，`-only=src/reasoner` 实跑 **15/15 杀死**；前端放宽校验的异变被 2 条测试抓到；gofmt/vet/文档守卫绿。
- **续轮（r30）：17/17 绿（237.1s，含根 e2e 109.5s）**——qq entry 的 deliver 归属校验落地（经 memory work_get，纯函数 `deliverAuthorized` + 越权 permission_denied）；qq 测试替身补 memory work_get/自动注册；`guardmutation` 清单 96→**97 条**，`-only=src/qq` 实跑 **2/2 杀死**；gofmt/vet/文档守卫绿。
- **续轮（r31）：17/17 绿（283.1s，含根 e2e 116.5s）**——§17.2 红队六条成组验收落地（`testing/tests/redteam_test.go`，掩耳盗铃/声东击西/狐假虎威/概念漂移/多此一举/指鹿为马）；组内六子用例全绿；gofmt/vet/文档守卫绿。
- **续轮（r32）：17/17 绿（288.9s，含根 e2e 144.8s）**——§17.3 负向测试补齐：tenant/capability/delegated 帧级 `not_supported`、spawn/drain/retire 服务端 `not_supported`（含 errors_total）、memory 工具 schema/必填/幂等契约；`guardmutation` 清单 97→**99 条**，`-only=src/spv2` 实跑 **42/42 杀死**；gofmt/vet/文档守卫绿。
- **续轮（r33）：17/17 绿（278.1s，含根 e2e 143.8s）**——设置键 `recall.*`→`context.*` 全量改名（非别名兼容，旧键被拒），前端语言包/协议文档同步；前端 node 90/90、vitest 153/153；`guardmutation` 清单 99→**100 条**，`-only=src/memory` 实跑 **33/33 杀死**；gofmt/vet/文档守卫绿。
- **续轮（r34）：17/17 绿（255s，含根 e2e 125.1s）**——A2A 最小闭环：agent `message` 带目标 work 经 `message_post` 入队，该 work 下次开任务 `message_claim→注入→ack`；memory claim 回执带正文；`guardmutation` 清单 100→**102 条**，`-only=src/reasoner` 实跑 **17/17 杀死**，claim 守卫更新后直接异变验证仍被杀；gofmt/vet/文档守卫绿。
- **续轮（r35/r36）：17/17 绿（278.7s，含根 e2e 141.4s）**——§7.1 入口统一 message：reasoner 删除 user_message（method/provides/handler），message 新增 service 入口路径（authority=none + sender 必填 → 人类主链，结果归属=入口），前端/qq/e2e/evals/approval-integration 全部迁移；前端 node 90/90、vitest+build 绿；`guardmutation` 清单 102→**104 条**（2 条 message 入口守卫直接异变验证被杀）；gofmt/vet/文档守卫绿。
- **续轮（r37/r38）：17/17 绿（266.6s，含根 e2e 125.9s）**——清理 `protocol.UserMessagePayload` 死类型（payloads_test/batch 注释迁移），mcp 保留名单去掉已删 method，全仓 `user_message`/`UserMessagePayload` 归零；红队“多此一举”新增死灰复燃扫描（src 出现旧入口即失败）；gofmt/vet/文档守卫绿。
- **续轮（r39）：17/17 绿（300.9s，含根 e2e 150.5s）**——生产流式 message 落地（进度/工具/终态块，seq 逐 1、end=true、终态带最终答案；transport cancel 可取消）；`MessagePayload.stream` 显式开关；同轮新增守卫“流式终态不带最终答案”直接异变验证被杀（清单 **105 条**）；gofmt/vet/文档守卫绿。**边界**：不是 token 级——token 级需 LLM 流式接口，尚未做。
- **最终（r43）：全量守卫 106/106 杀死（0 存活/0 无效）**；全量 Go 17/17 绿（284.9s，含根 e2e 144s）；前端 node 90/90、vitest 153/153、build 绿；embedding pytest 49 passed/1 skipped；gofmt/vet/文档守卫绿。
- **续轮（r41/r42）：全量守卫实跑**——106 条守卫一次跑完，首轮 105/106：唯一存活“流式终态不带最终答案”是守卫落在仅被 ./tests 覆盖的代码上（跨包）；把终态提取抽成同包纯函数 `finalAssistantText` 并补同包测试后重跑 `-only=src/reasoner` **21/21 杀死**；全量套件 **17/17 绿**（284.9s，含根 e2e 144s）。
- **续轮（r40）：token 级流式落地**——`llm.StreamingClient` + `DeepSeekClient.ChatStream`（SSE content 增量/usage/tool_calls 拼装）+ `Fake.ChatStream`；`chatLLMStreamWithUsage`（指标/用量同口径）；`streamSink` 把 board 每轮 token 增量推给 message 流式答复，boardClose 关 sink、终态 end 带完整答案；新增测试（SSE 解析、Fake 分块、流式 message 增量拼接含答案）与守卫“token 增量不再回调”（清单 **106 条**，直接异变验证被杀）；gofmt/vet/文档守卫绿。全量套件复跑 **17/17 绿**（258.6s，含根 e2e 127.6s；前一次 approval-integration 偶发失败，单跑/重跑均绿）。

### 9.4 历史快照（r17 当时未完成项；后续轮次已逐条补齐，见 §9.3c 与 §10，勿当作现状）

- §6/§6.5/§6.8 剩余：works/participants/lease 账本、recall 登记、work_get/lease_charge/release/work_update、最小 authority、reasoner 工具前扣租约与 done/waiting/cancelled 推进已落地；仍缺 work_messages/A2A、sweep 调度、token 租约扣减（LLM usage）、完整 §6.5 矩阵与 deliver/context 的 work 校验。
- §7 核心 method 词汇：生产仍是 user_message/recall/resume，不是 message/context；需要定迁移与兼容边界（不做适配层，按 spec 只做加法）。
- §5.1：已完成（Go/TS/Python 三端服务端强制 + 客户端自动握手，raw 测试全部迁移）。
- §3.5/§10 剩余：`max_header`、stream cancel 未实现（`max_stream`/`max_stream_buffer`/seq 缺口与重复记账已落地，见 §9.1）。
- §12.3 各域指标：LLM / tool 本轮已在 reasoner 落地；仍缺 approval 决策指标、生产 memory 的 A2A/work 指标、队列深度；frontend/embedding 观测面亦未动。
- §17.2 红队成组验收仍未补；§17.3 的错误码负向测试本轮已补齐（cancelled/unavailable/stalled 见 §9.1）。
- frontend / embedding 两个非 Go 服务的 `/healthz` `/readyz` `/metrics` 与 hello 握手尚未对齐——§12.1 仍是「Go 服务已接入，非 Go 服务未接入」的部分状态。

---

### 证据路径（临时产物，不在仓库内）
- 全量测试日志：`%TEMP%/sump-audit/go-tests.log`
- 异变日志：`mutation.log`、`guardmutation.log`
- flake 复现：`flake.log`
- 自定义 health 异变与动态确认：本次会话输出（脚本临时改码后按 SHA256 恢复）

## 10. 最终验收盘点（对照 docs/concepts/spv2.md §17，按证据说话）

| §17.1 核心验收项 | 状态 | 证据 / 缺口 |
|---|---|---|
| 握手 hello/卡片/版本协商 | ✅ 已落地 | Go/TS/Python 三端服务端强制首帧 hello + 客户端自动握手；4 条异变杀死 |
| work 创建/context/stalled/cancel/renew | ✅ 已落地 | 登记/状态机/sweep/work_get/work_update/cancel/renew、memory `context` 同步链、reasoner deliver（前端/qq 两 entry 均按 work 校验来源）、旧 recall/resume/user_message 链已删除并有测试/守卫 |
| lease charge/release/耗尽/并发扣减 | ✅ 已落地 | 原子 SQL 租约、耗尽/stalled/deadline 错误码、工具前扣减、token 租约（LLM usage，含流式路径）、并发扣减与 claim actor 过滤测试 |
| authority 矩阵 | ✅ 已落地 | work 控制权、context 来源、message 的 human/service-entry/agent 权限、前端与 qq 两 entry 的 deliver 校验、A2A 入队/claim 归属链均有测试；tenant/capability/delegated 按 spec §18 明确 not_supported |
| A2A queue claim/ack/nack/retry/dead-letter | ✅ 生产最小闭环：memory 队列 + reasoner agent 消息入队/下次开任务 claim→注入→ack 已落地；跨进程多 agent 的并发消费仍按单 reasoner 拓扑（spawn 未设计） |
| 流式多块/seq/end/cancel/buffer | ✅ 已落地 | 缺块/重复/超 buffer/流上限测试；`max_header`；transport 级 stream cancel；生产 `message` 流式（token 级：LLM SSE → delta 帧 → end 终态，seq 严格递增、可分块取消） |
| 错误 code/retryable/details；未知 method | ✅ 已落地 | 错误码表、重试语义、未知 method 回 not_found、每个 code 有负向测试 |
| 观测 health/metrics/HTTP/最小指标集 | ✅ 已落地 | health/readyz、OpenMetrics、work/lease/stream/LLM/工具/连接/A2A/queue_depth/审批决策、frontend/embedding 观测面；A2A 调用方与 entry deliver 校验已落地 |
| 迁移 v2-only + 一条链 e2e | ✅ 已落地 | 无 v1 适配器；infer/portal/approval 等真进程 e2e 全绿（r17 见 §9.3c） |

| §17.2 红队 | 状态 |
|---|---|
| 掩耳盗铃（杀接收方/队列满→error+dropped） | ✅ 红队组：坏帧 dropped+断连、handler 错误指标、调用方可见错误 |
| 声东击西（to 不符拒绝、卡与 handler 契约） | ✅ 红队组：to 不符 invalid_argument 且 handler 不执行 |
| 狐假虎威（agent 冒充用户已批准、participant deliver） | ✅ 红队组：无 owner/runtime 控制 work 被拒；两 entry 的 deliver 越权均 permission_denied |
| 概念漂移（术语/字段 lint） | ✅ 红队组：帧 msgpack 键与 spec 字段表逐一对齐 + 文档键名存在；全仓旧入口 token 扫描为 0 |
| 多此一举（works 表/work id 生成点唯一） | ✅ 已落地：红队组扫描生产服务源码，works/work_messages 建表只允许 memory 一处 |
| 指鹿为马（done 后 cancel/stalled→done 冲突） | ✅ 已落地：红队组覆盖 done→cancel/stalled→done conflict；reasoner 侧 pending>0 不标 done 另有单测 |

**结论（不粉饰）**：§17.1 的帧/握手/错误/health/metrics/幂等/deadline/流式（含生产 message 的 token 级流式）、work/lease/authority/A2A/entry deliver、v2-only 迁移与端到端链路均已真实落地并有测试与异变证据；§17.2 红队六条成组验收全绿。**明确边界**：tenant/capability/delegated、spawn/drain/retire 按 spec §18 返回 `not_supported`；A2A 跨进程多 agent 并发消费受“spawn 未设计”限制，当前为单 reasoner 拓扑下的入队→下次开任务 claim/注入闭环。
