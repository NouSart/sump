"""fake 后端测试：词项切分 / 确定性 / 归一化 / 排序可预期（共享词项越多越相似）。"""

import math

from sump_embedding.backends import FakeBackend, terms


def dot(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b))


def test_terms() -> None:
    assert terms("爱吃苹果") == ["爱吃", "吃苹", "苹果"]
    assert terms("hello world") == ["hello", "world"]
    assert terms("苹果apple") == ["苹果", "apple"]
    assert terms("猫") == ["猫"]
    assert terms("a") == []  # 单字符拉丁不成词
    assert terms("你好，世界！") == ["你好", "世界"]


def test_deterministic_and_normalized() -> None:
    b1, b2 = FakeBackend(dim=64), FakeBackend(dim=64)
    v1 = b1.embed(["爱吃苹果"])[0]
    v2 = b2.embed(["爱吃苹果"])[0]
    assert v1 == v2  # 跨实例确定性（测试与 CI 可复现）
    assert abs(math.sqrt(sum(x * x for x in v1)) - 1.0) < 1e-9
    assert b1.name == "fake-bigram-v1" and b1.dim == 64


def test_similarity_follows_term_overlap() -> None:
    b = FakeBackend(dim=64)
    q = b.embed(["苹果"], query=True)[0]
    related = b.embed(["爱吃苹果，尤其是脆的"])[0]
    unrelated = b.embed(["今天天气很好"])[0]
    assert dot(q, q) == 1.0
    assert dot(q, related) > dot(q, unrelated)
    assert dot(q, unrelated) == 0.0  # 无共享词项 → 正交


def test_empty_text_zero_vector() -> None:
    b = FakeBackend(dim=16)
    v = b.embed([""])[0]
    assert v == [0.0] * 16
    assert dot(v, b.embed(["苹果"])[0]) == 0.0
