# evals —— 评测 harness 本身

> **读者**：要回答「这次改动让编排层变好还是变坏、贵了多少、稳不稳」的人 ｜ **读完能做**：跑一次评测、加一条任务、看懂报告、判断结论能不能信 ｜ **状态**：可用（首版，6 个任务集 15 条任务）｜ **最后校验**：2026-11-01

## 1. 这里评的不是模型

被测对象是**编排层**：任务入口 → 记忆托管 → 单步推理 → 工具派发（异步）→ 结果注入 → 收尾门禁 → 交付。
模型被**冻结**：`-llm-base` 指向本模块自带的假 OpenAI 服务，模型行为由任务自带的剧本给出。
所以「分数变了」只能是 harness 变了，不是模型变聪明了。

这与 `tests/`（跨服务真进程 e2e）的分工：

| | `tests/` | `evals/` |
| --- | --- | --- |
| 回答 | 有没有坏 | 变好还是变坏、贵了多少、稳不稳 |
| 判定 | 过程断言（某条消息出现了） | **终态断言**（世界变成什么样） |
| 重复 | 单次 | k 次，报 `pass^k` |
| 指标 | 无 | 跳数 / 耗时 / token / 方差 / 契约违规 |

## 2. 跑起来

```powershell
cd evals
go test ./... -count=1                       # 评测器自己的测试（不需要起服务）
go run ./cmd/run -list                       # 看有哪些任务集与任务
go run ./cmd/run -suite smoke -trials 3      # 跑一次（k=3）
go run ./cmd/run -suite smoke -trials 3 -save-baseline          # 把这次存成 data/evals/baseline.json
go run ./cmd/run -suite smoke -trials 3 -baseline baseline.json # 与基线对比（给 Δ 与噪声带判定）
go run ./cmd/run -suite shell -only shell-001 -v
go run ./cmd/run -suite orchestration -set reasoner.tool.notify_mode=batch -profile batch   # 消融：只改一个旋钮
```

产物落在 `data/evals/<run_id>/`（已在 `.gitignore` 的 `data/` 里）：
| 文件 | 内容 |
| --- | --- |
| `report.md` | 给人看的报告（总览 + 逐任务 + 失败原因 + 与基线对比 + 怎么读） |
| `results.csv` | 给机器比对的逐 trial 结果 |
| `manifest.json` | 这次 run 的快照：commit、数据集 sha256、全部旋钮、模型、trials、主机、起止 |
| `summary.json` | 聚合结果（下一次 run 拿它当基线；`report.md` 是渲染视图，别去解析它） |
| `traces/<task>-t<k>.json` | 证据包：LLM 请求留痕 + 完整轨迹（含 reasoning / tool_call / tool_result） |

服务二进制缓存在 `data/evals/bin/`，按**源码内容指纹**判断要不要重编（`-rebuild` 可强制）。
为什么不每次编：这台机器同时跑着一堆进程，连编五个服务会把换页文件顶满
（真实报错：`The paging file is too small for this operation to complete`）。

## 3. 任务集

| 任务集 | 起哪些服务 | 测什么 |
| --- | --- | --- |
| `smoke` | 设置中心 / 记忆 / reasoner | 记忆工具闭环；**负向对照**（只说不做必须被判失败） |
| `gate` | 同上 | 收尾门禁（没判定就催）、工具暴露闸门（未声明的动作调不动） |
| `orchestration` | 同上 | 并行工具、`wait` 工具、结果注入后的下一跳 |
| `shell` | + approval + shell | 真命令写文件、审批拒绝时文件不变、只读命令 |
| `fault` | 设置中心 / 记忆 / reasoner | LLM 故障：500 / 429 / 空 choices / 截断 JSON / 非 JSON / 慢响应 |
| `nodep` | **故意不起 memory** | 依赖缺席：链停在入口，且**没有告知入口方**（当前是红的，见 §7） |

「起哪些服务、钉死哪些设置」写在 `cmd/run/main.go` 的 `suitePresets` 里——那是跑法，不是数据。
`cmd/run` 的测试会检查「每个数据集文件都必须登记跑法」，反之亦然。

## 4. 任务怎么写

`dataset/<suite>.jsonl`，一行一条：

```json
{
  "id": "shell-001",
  "category": "shell",
  "prompt": "在工作区创建文件 a1.txt，内容写 hello。",
  "setup": {"files": {"keep.txt": "keepme"}},
  "budget": {"max_hops": 16, "max_wall_ms": 90000},
  "expect": [{"file_contains": {"path": "a1.txt", "text": "hello"}}],
  "script": {"steps": [
    {"name": "write", "when": {"call_at_most": 1},
     "reply": {"tool_calls": [{"id": "c1", "name": "shell__run",
               "arguments": "{\"command\":\"Set-Content a1.txt hello\"}"}]}},
    {"name": "judge", "when": {"contains": "命令执行完成"},
     "reply": {"tool_calls": [{"id": "c2", "name": "task_status",
               "arguments": "{\"status\":\"done\"}"}]}},
    {"name": "answer", "when": {"contains": "判定为"}, "reply": {"content": "已创建 a1.txt。"}}
  ]}
}
```

- **`when`** 是匹配条件，全满足才命中：`tool_results_at_least`（看到几个工具结果）、
  `contains`（任意消息包含某段文本）、`call_at_least` / `call_at_most`（第几次调用）。
  按内容匹配而不是按序号，是因为工具完成时机不确定——按序号写剧本，改一次 harness 就得重写。
- **`expect`** 是终态断言，一条任务里**每个元素只写一个字段**：
  `file_exists` / `file_contains` / `file_absent` / `memory_has` / `deliver_contains` /
  `delivered` / `failure_notified` / `no_tool_failure` / `reminders_at_least` /
  `gate_rejects_at_least` / `hops_at_most`。
- **`budget`**：`max_hops` 与 `max_wall_ms`，任一超出即判失败，原因写明是哪一项。
- **`must_fail: true`**：负向对照（见 §6）。
- 文件路径一律相对工作区根（`<临时目录>/work`），不写绝对路径——否则换机器必红。

任务集文件**不要带 BOM**（PowerShell 的 `Set-Content -Encoding UTF8` 会写 BOM，加载会报
`invalid character 'ï'`；用 `[System.IO.File]::WriteAllText(path, text, (New-Object System.Text.UTF8Encoding($false)))`）。

## 5. 指标口径（缺一条就不算评完）

| 指标 | 定义 | 为什么必须有 |
| --- | --- | --- |
| 单次成功率 | 通过的 trial / 全部 trial | 最容易被误当作结论的那个数 |
| **`pass^k`** | k 次 trial **全过**的任务占比 | 一次侥幸不能算过；τ-bench 的口径 |
| 跳数 | 一次任务内 LLM 调用次数（含催办轮） | harness 有没有白跑 |
| 耗时 | `user_message` → 交付（或预算耗尽） | 编排的等待与串行程度 |
| token | 假 LLM 回报的 usage（剧本没给则估算，口径与 reasoner 一致） | 成本 |
| 标准差 | 同配置 k 次的跳数/耗时/token 波动 | **harness 改动的成本方差常大于换模型** |
| 闸门拒绝 / 催办轮 / 契约违规 | 工具被闸门挡住次数 / 收尾门禁催了几轮 / 交付前未判定等 | 编排层自己出错 |

**噪声带**：`-baseline` 对比时，跳数/耗时/token 的"噪声带"取两次 run 标准差里较大的那个；
`|差异| ≤ 噪声带` 就判为「未超出噪声带」——**不能说改动有效**。只有 `pass^k` 是比例指标，
只要有变化就算显著（它本身已是聚合值）。k=1 时没有波动数据，报告只给差值并明说"无法判断是否显著"。

标定流程：`-save-baseline` 存一次公认的好成绩 → 改代码 → `-baseline baseline.json` 对比 →
报告里出现"超出噪声带"才谈改进。换任务集 / 换 k / 换数据集内容会直接判「不可比」。

## 6. 许愿式编程的六种样子，以及本模块怎么堵

「许愿式」= 用一句听起来合理的话代替证据。下面每条都是**本仓库真实会遇到**的形态，
右边是代码里对应的防线（都能在 `*_test.go` 里点到）。

| # | 形态 | 长什么样 | 本模块的防线 |
| --- | --- | --- | --- |
| 1 | **掩耳盗铃**（只看自述） | 「模型说做完了，所以做完了」；或失败也能算过 | 判定只看**终态断言**；`task_status done` 只是被测对象的自述，不作数（`runner/evaluate`）。失败必须写 `fail_reason`，报告固定列出（`report/TestMarkdownCarriesTheNumbersThatMatter`） |
| 2 | **声东击西**（换指标） | 只报「成功率 100%」，不谈跳数、成本、方差 | 报告强制同时给 `pass^k`、跳数、耗时、token、方差、契约违规（`metrics/TestAggregatePassK`、`TestAggregatePerTaskMeans`） |
| 3 | **狐假虎威**（借来的权威） | 故障没真注入，却宣称「扛住了故障」；或拿默认策略恰好放行当作审批通过 | `UnmatchedFaults()`：声明了故障却一次没命中 → 该 trial 判 `invalid`（`fakeopenai/TestUnmatchedFaults`）。`shell` 任务集**钉死** policy 文件与解释器（`cmd/run/TestShellPresetPinsPolicy`） |
| 4 | **概念漂移**（偷换对象） | 把「harness 评测」讲成「模型评测」，读者据此比较模型 | 报告首行声明模型被冻结；剧本随任务走、不随模型走；`shell` 固定 `powershell`（不自动探测 bash/pwsh/cmd）；`nodep` 用「不给 memory」而不是 stub 假扮缺席 |
| 5 | **多此一举**（仪式性动作） | 为凑数加断言、同一件事在两个任务集里各测一遍、为绿而改弱断言 | 每个断言字段都对应一条真实承诺（`dataset/TestValidateAcceptsEveryAssertion` 反向守住「新断言要登记」）；`smoke`/`fault` 分工明确，故障类只在 `fault`；负向对照单独成类，不与能力混算 |
| 6 | **只说不做**（假阳性） | 模型一句「已经创建好了」，评测器就盖章 | `must_fail` 负向对照：模型只说不做时**必须被判失败**，判成功即「负向对照失效」（`runner/TestFinishNegativeControl`、任务 `smoke-negctl`，报告里标 🛡 并单独成节） |

另外三条同源的坑，也一并堵了：

| 坑 | 防线 |
| --- | --- |
| **空集合算满分**（0 条任务 = 0 条失败） | 空任务集加载即报错（`dataset/TestLoadRejectsEmptyFile`）；空结果也会产出报告，不自称通过（`report/TestEmptySummaryStillWrites`） |
| **环境偷偷换**（策略/解释器/端口随机器变） | 设置项用 `<服务>.<项>` 显式下发（`runner/applySettings`）；端口进程内去重（`runner/TestFreePortUnique`）；`manifest.json` 记录 commit + 数据集 sha256 + 全部旋钮 + 主机 |
| **拿旧二进制测新代码** | 二进制按**源码内容指纹**缓存，内容变了必然重编（`runner/TestSourceFingerprintChangesWithCode`） |

## 7. 已知局限（现在是红的，别当成绿的）

1. **`nodep` 是红的，而且是真问题**：记忆服务缺席时链停在入口，入口方什么通知都收不到。
   这条任务的断言是 `failure_notified: true`，当前必红——它记的是 [TODO.md](../TODO.md) 里
   「记忆缺席 / 失败：当前是硬门」这条待决，不是评测器的 bug。**不要为了变绿删掉这条断言。**
2. **没有沙箱**：`shell` 在临时工作目录里跑真命令，破坏性任务（`sandbox: true`）在加载时被**拒绝**，
   而不是偷偷跑在本机。
3. **只跑单机串行**：并发与跨机（roadmap 里跨机本身还没验证）不在首版范围。
4. **没有真模型**：目前是「剧本化假 LLM」。真模型的录制/回放（`--record` / `--replay`）未实现——
   在实现之前，本模块**不能**用来比较模型。
5. **Windows 上钉死 `powershell`**：跨机对比前必须显式固定解释器，否则同一任务结果不可比。

## 8. 评测器自己的测试

```powershell
cd evals
go test ./... -count=1            # 含两条真进程用例（约 4 秒，有二进制缓存）
go test ./... -count=1 -short     # 只跑不依赖真进程的部分
go test ./... -count=1 -cover     # 当前：dataset 96.8% / metrics 92.7% / fakeopenai 88.9%
                                  #       report 82.1% / cmd/run 80.7% / runner 58.9%
```

`runner` 剩下的未覆盖部分是子进程生命周期（超时杀掉、异常退出），由 `TestStackRoundTrip`
与 `cmd/run` 的 `TestRunEndToEnd` 用真进程走过；其余纯函数（断言求值、负向对照、
端口分配、源码指纹）都有直测。

### 异变测试（确认这些测试真守得住）

覆盖率高只说明"代码被跑到过"，不说明"写错了会被发现"。所以还要做异变测试：
对判定代码逐个植入"看起来合理但语义错了"的改动，跑测试；测试失败 = 异变被杀死。

```powershell
cd evals
go run ./scripts/mutation              # 全部（含真进程包，约 1 分钟）
go run ./scripts/mutation -skip-slow   # 只跑纯逻辑包（约 10 秒）
go run ./scripts/mutation -v           # 附上每个异变的测试输出
```

当前 **25 条异变，全部被杀死**（`README §6` 的每种许愿式形态都有对应异变）。
脚本退出码非 0 表示有存活异变——那说明测试太弱，要补断言，**不许改弱异变**。

注意：CI 的模块清单里**没有** `evals`（刻意的：它要起真进程、跑真命令，不该拖慢主流水线；
`gofmt` 检查覆盖它）。改动本模块后请手工跑一次 `go test ./... && go run ./scripts/mutation`。

被异变测试逼出来的四处改动（都是真缺陷，不是仪式）：

1. `splitDeliveries` / `contractViolations` / `splitSettingKey` / `claimPort` 从流程里拆出来：
   原先它们藏在需要真进程的路径里，测不到，异变能存活。
2. 故障断言原来写成"响应含 `"role":"assist`"`——正常响应也含，恒真；改成**必须解析失败**。
3. 报告断言原来只查出现 "pass^k" 三个字，抓不到"把 pass^k 印成单次成功率"；改成连数值一起断言。
4. 对照任务原先没有报告断言，补了「🛡 + 单独成节 + 原因」。

## 9. 边界

- 不加协议动作、不改信封：`usage` 走假 LLM 的响应，轨迹直接读记忆服务现有的 `history`。
- 不动别的服务的代码；故障注入优先用「不启动某个服务」与模型侧剧本，不改被测进程。
- `evals` 是不注册、不提供动作的脚手架（`tests/docs_test.go` 的服务清单基线里已排除），
  但它在 `go.work` 里，`gofmt` / IDE 与主仓一致。
