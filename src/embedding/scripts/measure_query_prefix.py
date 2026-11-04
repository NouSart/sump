"""A/B 实测：query 侧指令前缀对检索排序的影响（真模型，本地运行）。

用法（仓库根）：
    embedding/.venv/Scripts/python.exe embedding/scripts/measure_query_prefix.py
    embedding/.venv/Scripts/python.exe embedding/scripts/measure_query_prefix.py -model-dir data/models/bge-small-zh-v1.5

做法：小评估集（记忆条目的真实形态：短查询 → 条目文本），两种模式（无前缀 / 官方指令前缀）
各自 embed 查询与全部候选，按余弦排序，报告 top-1 / top-3 命中率、MRR 与分数分布。
结论（含数字）写入 embedding/README.md，并据此确定服务的默认 query 前缀。
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sump_embedding.backends import BGE_QUERY_PREFIX, OnnxBackend  # noqa: E402

# （查询，应命中的条目文本）。候选池 = 全部条目文本（含各查询的正例与其他干扰项）。
PAIRS: list[tuple[str, str]] = [
    ("苹果", "爱吃苹果，尤其是脆的"),
    ("生日", "我的生日是 3 月 12 日"),
    ("作息", "习惯凌晨一点睡，早上八点起"),
    ("猫", "养了一只叫年糕的橘猫"),
    ("咖啡", "喝咖啡不加糖，喜欢美式"),
    ("前端", "前端用 React 和 Vite 构建"),
    ("服务器", "家庭服务器是一台旧的 NUC"),
    ("过敏", "对青霉素过敏，用药需避开"),
    ("地址", "收件地址在杭州市西湖区"),
    ("模型", "喜欢用 DeepSeek 的模型做代码生成"),
    ("天气", "今天天气很好，适合散步"),
    ("跳绳", "每天跳绳一千个作为锻炼"),
]

# 干扰项（补充候选池，提高检索难度）。
DISTRACTORS: list[str] = [
    "周末打算去爬山",
    "最近在读《人类简史》",
    "常去的面馆在巷子口",
    "地铁 2 号线早高峰很挤",
    "想学一门乐器，在看吉他",
    "公司楼下新开了一家奶茶店",
    "手机套餐下个月到期",
    "枕头有点高，睡得不舒服",
    "计划暑假去云南看雪山",
    "跑步鞋该换了",
]


def rank_of(scores: list[float], idx: int) -> int:
    """返回 idx 的排名（1-based）。"""
    better = sum(1 for s in scores if s > scores[idx])
    return better + 1


def dot(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b))


def measure(mode: str, model_dir: str) -> dict:
    backend = OnnxBackend(model_dir, query_prefix=BGE_QUERY_PREFIX if mode == "prefix" else "")
    corpus = [text for _, text in PAIRS] + DISTRACTORS
    doc_vecs = backend.embed(corpus)
    hits1 = hits3 = 0
    mrr = 0.0
    pos_scores: list[float] = []
    neg_scores: list[float] = []
    print(f"\n== 模式: {mode}（query 前缀 {'开' if mode == 'prefix' else '关'}）==")
    for query, relevant in PAIRS:
        qv = backend.embed([query], query=True)[0]
        scores = [dot(qv, dv) for dv in doc_vecs]
        idx = corpus.index(relevant)
        rank = rank_of(scores, idx)
        hits1 += 1 if rank == 1 else 0
        hits3 += 1 if rank <= 3 else 0
        mrr += 1.0 / rank
        pos_scores.append(scores[idx])
        neg_scores.extend(s for i, s in enumerate(scores) if i != idx)
        print(f"  {query!r:>10} → 命中排名 {rank:>2}（正例分 {scores[idx]:.4f}，最高干扰 {max((s for i, s in enumerate(scores) if i != idx), default=0):.4f}）")
    n = len(PAIRS)
    result = {
        "mode": mode,
        "top1": hits1 / n,
        "top3": hits3 / n,
        "mrr": mrr / n,
        "pos_mean": sum(pos_scores) / len(pos_scores),
        "pos_min": min(pos_scores),
        "neg_mean": sum(neg_scores) / len(neg_scores),
        "neg_p95": sorted(neg_scores)[int(len(neg_scores) * 0.95)],
    }
    print(
        f"  —— top1 {result['top1']:.0%} / top3 {result['top3']:.0%} / MRR {result['mrr']:.3f}"
        f"；正例均分 {result['pos_mean']:.3f}（最低 {result['pos_min']:.3f}）"
        f"；干扰均分 {result['neg_mean']:.3f}（p95 {result['neg_p95']:.3f}）"
    )
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("-model-dir", "--model-dir", default="data/models/bge-small-zh-v1.5")
    args = parser.parse_args()
    print(f"模型目录: {args.model_dir}（评估集 {len(PAIRS)} 查询 / 候选池 {len(PAIRS) + len(DISTRACTORS)} 条）")
    off = measure("no-prefix", args.model_dir)
    on = measure("prefix", args.model_dir)
    winner = "prefix" if on["mrr"] > off["mrr"] else "no-prefix"
    print(f"\n== 结论：MRR 更高者 = {winner}（no-prefix {off['mrr']:.3f} vs prefix {on['mrr']:.3f}）==")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
