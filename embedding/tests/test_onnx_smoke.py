"""真模型冒烟：仅在 SUMP_EMBED_MODEL_DIR 指定模型目录时运行（本地 / 显式开关）。

CI 不跑（不引入 94MB 模型；测试不得依赖外网/大件）。这里锚定最小的语义事实：
维度一致、向量归一、短查询对相关条目打分明显高于无关条目（与 README 实测分数带一致）。
"""

from __future__ import annotations

import math
import os
from pathlib import Path

import pytest

from sump_embedding.backends import OnnxBackend

# 相对路径按仓库根解析（无论从 embedding/ 还是仓库根运行）。
_REPO_ROOT = Path(__file__).resolve().parents[2]
MODEL_DIR = os.environ.get("SUMP_EMBED_MODEL_DIR", "")
if MODEL_DIR and not os.path.isabs(MODEL_DIR):
    MODEL_DIR = str((_REPO_ROOT / MODEL_DIR).resolve())
pytestmark = pytest.mark.skipif(
    not MODEL_DIR,
    reason="未设置 SUMP_EMBED_MODEL_DIR——真模型冒烟仅在显式开关下跑（CI 不跑）",
)


def dot(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b))


def test_real_model_semantics() -> None:
    b = OnnxBackend(MODEL_DIR, query_prefix="")
    assert b.dim > 0
    q = b.embed(["苹果"], query=True)[0]
    assert abs(math.sqrt(sum(x * x for x in q)) - 1.0) < 1e-6

    related = b.embed(["爱吃苹果，尤其是脆的"])[0]
    unrelated = b.embed(["今天天气很好，适合散步"])[0]
    assert dot(q, related) > dot(q, unrelated)
    assert dot(q, related) >= 0.45  # README 实测：正例 ≥0.49
    assert dot(q, unrelated) <= 0.45  # 干扰 p95 ≤0.41

    # 同文本自相似 = 1（确定性）
    again = b.embed(["苹果"])[0]
    assert abs(dot(again, again) - 1.0) < 1e-6
