# 许愿式编程审计（2026-11-15）

> 范围：整个仓库（17 个 Go 模块 + 根 e2e/evals/approval-integration + 前端 TS + embedding Python + 文档/守卫）。
> 标准：`docs/concepts/spv2.md` 为准；本审计只认“有实现 + 有测试/守卫 + 有证据”的落地。

## 1. 什么是“许愿式编程”（本仓库口径）

许愿式编程 = 用语言承诺替代可验证事实。具体检查以下 9 个特征：

| # | 特征 | 本仓库的防呆控制 |
|---|---|---|
| W1 | 注释/文档/报告宣称已实现，代码没有或已漂移 | 文档守卫 `TestDocs`；红队“概念漂移”扫描帧字段、spec 键名、概念文档旧词汇；本审计逐条核对 |
| W2 | 声明即完成：`provides`/settings/health 声明了但没有 handler/消费者 | `TestProvidesMatchHandle`（声明 ↔ 注册双向对账）；settings 行为级测试；health 探针测试 |
| W3 | 只测替身：mock/stub 断言通过，真实链路未跑 | 根 e2e 起真进程（settings-center/memory/reasoner/qq/shell/…）；approval-integration 真 reasoner；门户/qq 全链 e2e |
| W4 | 弱断言：测试只断言“没报错/非空” | 静态扫描 `*_test.go` 是否含失败断言（仅 4 个文件无断言：2 个 probe + 2 个 helper 专用文件，已人工确认非测试）；全量测试 + 异变 |
| W5 | 错误静默吞掉：`_ =` / 只 log 没有可观测信号 | 指标审计（每个 `sump_*` 至少被测试引用，29/29）；A2A claim 失败指标、work 推进指标均补测试；关键 `_ =` 逐一分类（close/rollback 可忽略，状态写失败改为 log） |
| W6 | TODO/“待定”当完成 | 扫描 `TODO/FIXME/未做/待定`：均落在明确的能力边界（旧版 Office/OCR、幂等键待定等），不是“已宣称完成”；审计报告 §9.4 旧快照已标注历史 |
| W7 | 兼容层/旧词汇残留（做了两套却宣称一套） | `src` 中 `user_message`/`recall`/`resume` method token 为 0；红队“多此一举”扫描死灰复燃；`recall.*` 设置键改名旧键被拒 |
| W8 | 指标/日志宣称但无人验证 | 见 W5；指标 label 白名单审计（无未白名单 label） |
| W9 | 测试跳过关键路径 | `t.Skip` 全量清点：全部是环境/显式开关（无 Git Bash、无 PowerShell、无前端构建、`-short`、真实 LLM 开关），没有产品路径被静默跳过 |

## 2. 本轮发现与处置

| 编号 | 发现 | 性质 | 处置/证据 |
|---|---|---|---|
| F1 | `docs/concepts/architecture.md`、`protocol.md`、`agent-loop.md`、`docs/services/reasoner.md`、`docs/services/qq.md` 仍写 `user_message`/`recall`/`resume` 与“半启动/记忆往返”旧流程 | 文档概念漂移（W1/W7） | 5 篇文档按当前实现改写为 `message`/`context` 同步链；红队“概念漂移”新增 `docs/concepts` + `docs/services` 旧词汇扫描（`spv2.md` 除外），防复发 |
| F2 | `protocol.MessagePayload.Stream` 注释仍称“非 token 级” | 注释漂移（W1） | 改为“token 增量 + 终态，需 LLM 流式支持；Fake/DeepSeek 均已实现” |
| F3 | `sump_work_transitions_total` 用 `to` 标签，但 label 白名单没有 `to` → 标签被**静默丢弃**，所有推进混成一条；且只有 cancel 路径记录，work_update 不记 | 指标名不副实（W5/W8） | 标签改为白名单内 `status`；`work_update` 也记录；同包测试 + 真实链路测试（context→work_update done 断言指标） |
| F4 | `sump_a2a_claim_failures_total`、`sump_stream_cancelled_total`、`sump_work_transitions_total` 三个生产指标没有任何测试引用 | 指标未验证（W8） | 分别补指标断言（reasoner claim 失败注入、spv2 cancel、memory 真实调用）；指标引用审计从 3 缺失 → **0 缺失（29/29）** |
| F5 | batch 恢复路径 `_ = SetCounts(...)` / `_ = SetPhase(...)`：写库失败无日志 | 静默失败（W5） | 改为检查并 logger.Printf（不改变业务语义，只让失败可见） |
| F6 | 审计报告 §9.4“本轮未完成”与最终 §10 结论矛盾 | 历史快照未标注（W1/W6） | 标注为 r17 历史快照，指向 §9.3c/§10 |
| F7 | 3 个非白名单指标 label 的静态检查曾误报（脚本把普通 map 当指标 label）；精确扫描后为 0 | 审计脚本本身要防误报 | 精确扫描（只看 `sump_*` 附近的 `map[string]string`）结果：无未白名单 label |

## 3. 测试面覆盖（本轮全量）

| 测试面 | 命令 | 结果 |
|---|---|---|
| 全量 Go（17 模块） | 全量脚本 `go test ./... -count=1` ×17 | **17/17 绿**（282.1s，含根 e2e 141.4s；随后文档/测试扫描改动再次复跑，见 `go-tests-audit-2.log`） |
| 根跨服务真进程 e2e | `cd testing/tests; go test ./... -count=1` | 141.4s 绿（含 `TestProvidesMatchHandle`、红队组、门户/qq/approval 真进程链） |
| 前端 | `cd src/frontend; npm test`（node + vitest）与 `npm run build` | 本轮复跑：node 90/90、vitest 153/153、build 绿 |
| embedding Python | `.venv\Scripts\python.exe -m pytest tests -q -rs` | 49 passed, 1 skipped（skip = 未设 `SUMP_EMBED_MODEL_DIR` 的 onnx 冒烟，opt-in） |
| 文档守卫 | `cd testing/tests; go test -run TestDocs -count=1 .` | 绿（本轮） |
| 提供/声明对账 | `testing/tests` 的 `TestProvidesMatchHandle` | 绿（全量 e2e 内） |
| 异变 | `cd testing/evals; go run ./scripts/guardmutation` | **107/107 杀死，0 存活，0 无效**（`gm-audit.log`） |

## 4. 明确边界（不是许愿，是写下来的不做）

- `tenant`/`capability`/`delegated`、`spawn`/`drain`/`retire`：按 spec §18 返回 `not_supported`（有测试）。
- `src/spv2` 的 work/lease 参考模型不替换生产 memory 的会话库/黑板（参考实现定位，非“已接入生产”）。
- A2A 跨进程多 agent 并发消费受 spawn 未设计限制：当前是单 reasoner 拓扑下的入队→下次开任务 claim/注入→ack 闭环。
- 幂等键缺失时“送达后不重试”是显式取舍（`service.Call` 注释），不是静默重试。
- PDF OCR/旧版 Office/加密 PDF：明确 `not supported` 报错，不做“看起来能读”的兜底。

## 5. 结论

本轮未发现“只靠嘴完成”的功能项；发现并修掉 7 处声明与事实不一致（概念文档 3 篇、注释 1 处、指标 2 类问题、静默失败 1 处、历史快照 1 处）。
剩余可改进：设置项“声明 ↔ 消费者”还没有像 provides 那样的机械对账测试（现有行为级测试覆盖主要预算项）；列入后续候选，不冒充已完成。
### 3.1 覆盖率取样（`go test ./... -cover`，包内测试口径）

| 包 | 覆盖率 |
|---|---|
| protocol / fetchsafe | 87.5% / 89.5% |
| spv2 / spv2/ref | 82.1% / 73.3% |
| memory/store；memory/shared；memory/redact | 58.1%；79.3%；90.8% |
| reasoner/llm；reasoner/server（包内测试口径） | 71.4%；54.3% |
| service | 48.7% |
| qq/napcat；qq/server（包内测试口径） | 55.5%；11.1% |

> 说明（避免拿数字自我欺骗）：`memory/server`、`reasoner/server`、`qq/server`、`service` 的多数测试在**同模块的 `tests` 包**或**真进程 e2e** 里，`go test -cover` 不会把外部测试包的覆盖计入包内百分比；这些服务的真实覆盖证据是本表下方的根 e2e 与 approval-integration（真进程/真协议）。包内百分比低≠未测，但也不能当作“已充分覆盖”的证明。