# Kimi 交接包：TS / Python 测试面补齐与异变测试

> 目标读者：Kimi（或另一个具备完整仓库访问权的编码代理）。
> 本包只包含 **frontend（TypeScript）** 与 **embedding（Python）**；Go 侧由当前代理继续，避免冲突。

## 0. 一句话任务

在独立 worktree / 分支上，为 frontend 与 embedding 补齐可量化的覆盖率与异变测试（mutation testing）；杀掉高价值 mutant，修复测试暴露的真实 bug，提交测试、脚本和一份结果报告。**不要改 Go 侧。**

## 1. 仓库与基线

- 仓库：`D:\agent\SUMP\2`
- 基线：`feat/spv2.1-native-settings` 的提交 `445680c`（“补 SPv2 边界面并修正幂等错误指标”）。
- 当前代理有未提交的 Go 改动（go.mod / go.sum / src/spv2 测试）；**不要基于工作区复制**，必须从 `445680c` 创建 worktree。
- 建议：

```powershell
cd D:\agent\SUMP\2
git worktree add ..\sump-kimi-ts-python 445680c
cd ..\sump-kimi-ts-python
git switch -c kimi/spv2-ts-python-mutation
```

如果 worktree 路径已存在，换一个目录名；不要删除当前代理的 worktree。

## 2. 背景：已经完成什么

- 全部业务服务的 Go `/spv2` handler 已原生化；frontend / embedding 的**业务入口 + 控制面**也已切到原生 `/spv2`。
- frontend 的 `CenterLink` 在 `SUMP_SPV2=1` 时走原生 `/spv2`（register / heartbeat / roster / settings / center.call）；失败回退 v1。
- embedding 同理；Python 侧有 `spv2.py` 帧编解码与 `/spv2` 端点。
- 现有测试：
  - frontend：`npm run typecheck` 通过；`npm test` = 82 node + 153 web 通过。
  - embedding：`.venv\Scripts\python.exe -m pytest tests -q` = 35 passed, 1 skipped。
- 已知缺口：
  - frontend 没有安装 `@vitest/coverage-v8`；Node 测试也没有覆盖率配置。
  - Python 没有安装 `pytest-cov` / `coverage` 的可用配置。
  - 两边都**没有跑过异变测试**；这是本任务的核心。

## 3. 范围（只改这些）

### frontend

- `src/frontend/src/server/center.ts`
- `src/frontend/src/server/spv2.ts`
- `src/frontend/src/server/service.ts`
- `src/frontend/src/server/portal.ts`
- `src/frontend/src/server/app.ts`
- `src/frontend/tests/`（含 `stub-center.ts`、`stub-native-center.ts`）
- `src/frontend/package.json`（仅测试/覆盖率依赖与脚本）

### embedding

- `src/embedding/sump_embedding/center.py`
- `src/embedding/sump_embedding/spv2.py`
- `src/embedding/sump_embedding/service.py`
- `src/embedding/sump_embedding/server.py`
- `src/embedding/sump_embedding/protocol.py`
- `src/embedding/tests/`
- `src/embedding/pyproject.toml`（仅测试/覆盖率配置，如需）

**禁止改**：`src/spv2`、`src/service`、`src/settings-center`、其它 Go 服务、`src/protocol`、`run-local.ps1`。

## 4. 具体任务

### A. 覆盖率（先做，作为 mutation 的输入）

1. frontend：
   - 优先用 Node 自带覆盖率（如果 Node 版本支持）：`node --experimental-test-coverage --test "tests/*.test.ts"`；
   - 或安装/使用 `c8` / `@vitest/coverage-v8`（只加 devDependency；不要在测试运行时联网）。
   - 至少覆盖 `src/server/center.ts`、`spv2.ts`、`service.ts`。
2. embedding：
   - 使用 `coverage.py`（`coverage run -m pytest tests -q; coverage report -m`）或 `pytest-cov`；
   - 至少覆盖 `center.py`、`spv2.py`、`service.py`、`protocol.py`。
3. 报告必须包含：命令、总覆盖率、关键文件覆盖率、未覆盖的关键分支。

### B. 异变测试（核心）

优先用成熟工具；装不上/不可用就写**最小、可复现**的自定义 mutation harness。

- TypeScript：优先 `@stryker-mutator/core` + `@stryker-mutator/vitest-runner`（或 node-test runner）；如果离线装不上，写脚本：
  - 复制源文件到临时目录/临时 worktree；
  - 做确定性文本/AST 变异：`===`↔`!==`、`&&`↔`||`、`true`↔`false`、`+1`↔`-1`、删除 `return` / `await` / `try` 边界；
  - 每次变异后跑相关测试；测试失败 = killed，全绿 = lived；
  - 恢复文件；**不得碰主工作区**。
- Python：优先 `mutmut`；装不上就写同样的脚本（`==`↔`!=`、`and`↔`or`、`True`↔`False`、`return` 删除、边界 `<`↔`<=`）。

最低要求：每个语言 **至少 30 个有效 mutant**，覆盖：

- frontend：`center.ts` 的原生/v1 分支、`spv2.ts` 的帧编解码/超时/错误映射、`service.ts` 的 `/spv2` 分流与信任栅栏。
- embedding：`center.py` 的原生/v1 分支、`spv2.py` 的帧编解码/错误、`service.py` 的路径分流。

禁止只做“等价 mutant”凑数；每个 survivor 必须给出：

- 补测试杀掉；或
- 证明等价（写明为什么行为不变）；或
- 明确标记为已知缺口 + 原因。

### C. 修真实 bug

如果 mutation/覆盖率暴露真实 bug：

- 先加一个失败测试；
- 再最小修复；
- 不削弱既有断言；
- 不改 v1 语义。

## 5. 必须验证的命令

```powershell
cd src/frontend
npm run typecheck
npm test

cd ..\embedding
.\.venv\Scripts\python.exe -m pytest tests -q
```

额外：

- `SUMP_SPV2=1` 与不设置两种环境都要跑；原生测试必须断言 native 特有行为，不能只断言“成功”。
- 不要依赖外网；测试用本机 `WebSocketServer` / `httptest` / 假后端。

## 6. 交付物

1. 新分支：`kimi/spv2-ts-python-mutation`（从 `445680c`）。
2. 测试与 mutation harness/脚本。
3. `reports/ts-python-mutation.md`：
   - 命令与版本；
   - 覆盖率数字；
   - mutant 总数、killed/lived/equivalent/timeout；
   - survivor 清单与处置；
   - 修复的 bug（文件:行、前后行为）；
   - 未完成项与原因。
4. 提交信息用中文 `test:` / `fix:` / `chore:`；一个提交只做一件事。

## 7. 验收标准

- [ ] frontend + embedding 全量测试通过；
- [ ] 至少 30 个有效 mutant/语言；
- [ ] mutation efficacy ≥ 80%（killed / (killed+lived)）；低于 80% 必须逐条解释；
- [ ] 每个 survivor 都有“杀掉/等价/已知缺口”结论；
- [ ] 覆盖率报告可复现；
- [ ] 没有改 Go 侧、没有改 v1 协议、没有削弱测试；
- [ ] 报告能让第三人只按命令复现。

## 8. 已知陷阱（避免许愿式编程）

- **不要把 v1 回退当原生通过**：原生测试要断言 native 特有错误码/卡片/帧形状；e2e 里的 fallback 日志不能证明原生路径。
- **不要只测 happy path**：错误码、超时、空 body、坏 msgpack、未注册、信任栅栏都要有失败断言。
- **不要用覆盖率代替质量**：覆盖率是输入，mutation 才是断言有效性的证据。
- **不要把条件 skip 算成 pass**：embedding 的 ONNX smoke 可能 skip；单独报告。
- **不要改生产行为来让 mutant 消失**；等价 mutant 要证明，不是删断言。
- **不要联网跑测试**；依赖只用本地已安装/显式 devDependency。
- 如果同一个 bug 修 5 次仍不过：保留现状，报告失败命令、错误、已尝试方案，不要无限重试。

## 9. 当前代理保留的部分（不要碰）

- Go `src/spv2` 的剩余 mutation survivor / timeout；
- `GOWORK=off` 独立模块构建修复；
- Go 控制面与设置下发；
- 最终全量 Go 测试与分支 PR。

完成后请把分支、提交、报告路径、未完成项发回；不要直接合并到 `main`。
