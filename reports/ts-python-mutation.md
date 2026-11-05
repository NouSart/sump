# TS / Python 覆盖率与异变测试报告

## 结论

| 语言 | Mutants | Killed | Lived | Timeout | Efficacy | Mutant coverage |
|---|---:|---:|---:|---:|---:|---:|
| TypeScript (frontend) | 40 | 30 | 3 | 7 | 90.9% | 82.5% |
| Python (embedding) | 43 | 37 | 0 | 6 | 100% | 86.0% |

两个语言都达到“≥30 mutant、efficacy ≥80%”的验收线。

## 覆盖率

Frontend（Node 内置覆盖率，`node --experimental-test-coverage --test "tests/*.test.ts"`）：

- all files：line 93.86%、branch 78.02%、funcs 90.69%
- center.ts 91.09 / 74.07 / 91.07
- spv2.ts 87.76 / 62.00 / 87.23
- service.ts 92.31 / 74.47 / 88.89
- api.ts 96.22 / 83.25 / 96.43

Embedding（`coverage.py`）：

- 总计 78%（874 statements / 188 miss）
- center.py 82%、spv2.py 90%、service.py 100%、protocol.py 95%、server.py 86%
- backends.py 61%（ONNX 分支未装真模型）；__main__.py 0%（进程入口未在单测内启动）

## 异变测试结果

TypeScript：40 mutants，Killed 30 / Lived 3 / Timeout 7。

- Lived：TS10（center payload 非对象夹具缺失）、TS26（event 空 body 分支）、TS29（conversation_id 非字符串组合）。
- Timeout：TS11、TS16、TS22、TS23、TS24、TS25、TS39；均为测试挂起，不冒充 killed。

Python：43 mutants，Killed 37 / Lived 0 / Timeout 6。

- Timeout：PY02、PY03、PY09、PY18、PY24、PY37；均为测试套件在变异下挂起。

## 本轮补的测试

TypeScript：

- 新增 tests/service.test.ts：parseTimerItems 非数组、合法项、单字段坏项。
- center.test.ts：环境变量默认原生、jumpTo 原生成功/RemoteError/回退 v1/缺名片、call 原生错误。
- spv2.test.ts：末尾斜杠、reply_to 不匹配、空错误码、空 body、createSPv2WSS timer/progress/report/not_found、信任栅栏、SPv2Session 空 body/事件/错误/关闭。
- stub-center.ts：增加 spv2Attempts，证明先试原生再回退。

Python：

- 新增 tests/test_mutation_gaps.py：Error retryable/details、Frame 默认/body 类型、Ref 解析、roster 非 dict、URL/split_addr、endpoint 校验/tool/超时、embed 批量与 query、非 reply 帧、/spv2 带 query 的路由。

## 未完成

- TS 3 个 lived、7 个 timeout；Python 6 个 timeout 尚未全部消除。
- 本 harness 是定向异变，不是全仓库全函数覆盖；每项均有可复现命令。
- 覆盖率未接 CI 阈值。

## 复现命令

```powershell
cd src/frontend
npm run typecheck
npm test
node --experimental-test-coverage --test "tests/*.test.ts"
node tests/mutation.mjs --output=ts-mutation.json

cd ..\embedding
python -m pytest tests -q
python -m coverage run --source=sump_embedding -m pytest tests -q
python -m coverage report -m
python tests/mutation.py --output=py-mutation.json
```
