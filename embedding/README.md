# embedding —— SUMP v2 向量服务（文本 → 向量）

独立进程 / 独立语言（Python）/ 无存储：注册 · 心跳 · 名册 · 断线自愈 · 优雅下线走设置中心连接（与 Go 侧
`service/` 骨架同语义）；对外只提供一个动作 **`embed`**——批量文本 → 向量（L2 归一化），供 memory 的语义召回使用。

```
memory(:9201) ──jump embed──▶ embedding(:9601) ──onnx──▶ data/models/bge-small-zh-v1.5
```

## 命令

```powershell
# 一次性环境（仓库根）
cd embedding
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install -e ".[onnx,dev]"     # onnx=numpy/onnxruntime/tokenizers；dev=pytest

# 启动（仓库根；run-local.ps1 已自动纳入）
embedding\.venv\Scripts\python.exe -m sump_embedding -addr 127.0.0.1:9601 -center ws://127.0.0.1:9000/ws
# 测试/CI（无模型依赖）：-backend fake                     # 确定性伪向量（字符 bigram 特征哈希）
```

参数（Go 风格单横线）：`-name` `-addr` `-center` `-backend onnx|fake` `-model-dir` `-model-file`
`-query-prefix` `-heartbeat-ms` `-max-batch` `-fake-dim`。

## 模型（不入库）

放置到 `data/models/bge-small-zh-v1.5/`（`data/` 已被 gitignore）：`model*.onnx` + `tokenizer.json` + `config.json`
（bge-small-zh-v1.5 量级，隐藏维 **512**；服务自动探测维度）。本机可从 v1 归档目录拷贝：

```powershell
Copy-Item 'D:\agent\SUMP\1\data\models\models--Qdrant--bge-small-zh-v1.5\snapshots\<snapshot>\*' data\models\bge-small-zh-v1.5\ -Force
```

## 实测记录（2026-10-15，bge-small-zh-v1.5 / 本地 CPU）

**query 侧指令前缀 A/B**（脚本 `scripts/measure_query_prefix.py`：12 条中文短查询 × 22 条候选）：
官方建议给 query 加「为这个句子生成表示以用于检索相关文章：」前缀。实测：

| 模式 | top-1 | top-3 | MRR | 正例均分（最低） | 干扰均分（p95） |
| ---- | ----- | ----- | --- | ---------------- | --------------- |
| **no-prefix（采用）** | **100%** | 100% | **1.000** | 0.646（0.495） | 0.288（0.407） |
| prefix（官方建议） | 92% | 100% | 0.958 | 0.532（0.355） | 0.229（0.334） |

结论：**v1.5 在短查询检索上不加前缀更好**（前缀把短查询向量拉进了指令语义区，失手 1 条；
与 v1 经 fastembed 的实际行为一致——默认不加）。前缀保留为参数 `-query-prefix`，可随时复测/切换。

**分数带（供 memory 侧定阈）**：正例 ≥0.49、干扰 p95 ≤0.41 → memory 语义召回最小余弦分取 **0.45**。

## 结构

- `sump_embedding/protocol.py`：协议层（信封 + MessagePack）——键名与 Go `protocol/` 对齐，`tests/test_protocol.py` 用 Go 产出字节锚定；
- `sump_embedding/center.py`：中心连接（注册 / 心跳 / 断线自愈 / 名册广播；首次注册失败即退出——引导错误立刻可见）；
- `sump_embedding/server.py`：WS 端点（jump 分派 / 未知动作回错 / 慢动作并发）；
- `sump_embedding/backends.py`：fake（确定性特征哈希，测试与 CI）/ onnx（onnxruntime + tokenizers，CLS pooling + L2 归一）；
- `tests/`：pytest（协议锚定 / fake 后端 / 真 WS 集成）；真模型冒烟在 `SUMP_EMBED_MODEL_DIR` 指定目录时才跑。
