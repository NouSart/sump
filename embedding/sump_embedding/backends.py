"""向量后端：fake（确定性特征哈希）/ onnx（真模型，本地推理）。

- FakeBackend：把文本切成词项（CJK 相邻二字 / 拉丁数字整词），做稳定哈希特征映射——
  文本共享词项越多、余弦越高。确定性跨进程（blake2b），不依赖模型文件与网络；
  供测试与 CI 跑全链，**不用于生产语义**。
- OnnxBackend：onnxruntime + HuggingFace tokenizers 直接加载本地模型目录；
  bge 系列取 CLS pooling + L2 归一化（与 v1 经 fastembed 的行为一致）。
"""

from __future__ import annotations

import hashlib
import math
import os
import re
from typing import Protocol

# bge 中文模型卡的检索建议：query 侧加指令、候选侧不加（A/B 实测见 scripts/measure_query_prefix.py）。
BGE_QUERY_PREFIX = "为这个句子生成表示以用于检索相关文章："

_HAN = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")


def terms(text: str) -> list[str]:
    """把文本切成词项（去重、保序）：CJK 取相邻二字（孤立单字取该字），拉丁/数字取整词（小写）。"""
    out: list[str] = []
    seen: set[str] = set()

    def emit(t: str) -> None:
        if t and t not in seen:
            seen.add(t)
            out.append(t)

    def flush_han(chars: list[str]) -> None:
        if len(chars) == 1:
            emit(chars[0])
        else:
            for i in range(len(chars) - 1):
                emit(chars[i] + chars[i + 1])
        chars.clear()

    def flush_word(chars: list[str]) -> None:
        if len(chars) >= 2:
            emit("".join(chars).lower())
        chars.clear()

    han: list[str] = []
    word: list[str] = []
    for ch in text:
        if _HAN.match(ch):
            flush_word(word)
            han.append(ch)
        elif ch.isalnum():
            flush_han(han)
            word.append(ch)
        else:
            flush_han(han)
            flush_word(word)
    flush_han(han)
    flush_word(word)
    return out


class Backend(Protocol):
    """向量后端接口：name 作为向量模型标识随结果返回（memory 据此判定失效）。"""

    name: str
    dim: int

    def embed(self, texts: list[str], query: bool = False) -> list[list[float]]: ...


class FakeBackend:
    """确定性伪向量：字符/词项 bigram 特征哈希（测试与 CI 用；语义无关，但排序可预期）。"""

    def __init__(self, dim: int = 384, name: str = "fake-bigram-v1") -> None:
        self.dim = dim
        self.name = name

    def embed(self, texts: list[str], query: bool = False) -> list[list[float]]:
        del query  # fake 不区分查询侧（确定性优先）
        out: list[list[float]] = []
        for text in texts:
            vec = [0.0] * self.dim
            for term in terms(text):
                digest = hashlib.blake2b(term.encode("utf-8"), digest_size=8).digest()
                idx = int.from_bytes(digest[:4], "little") % self.dim
                vec[idx] += 1.0
            norm = math.sqrt(sum(x * x for x in vec)) or 1.0
            out.append([x / norm for x in vec])
        return out


class OnnxBackend:
    """真模型后端：onnxruntime + tokenizers（延迟导入——fake 模式不需要 onnx extra）。"""

    def __init__(
        self,
        model_dir: str,
        model_file: str = "",
        query_prefix: str = BGE_QUERY_PREFIX,
        name: str = "",
    ) -> None:
        try:
            import numpy as np
            import onnxruntime
            from tokenizers import Tokenizer
        except ImportError as err:  # pragma: no cover - 环境缺失时给清晰指引
            raise RuntimeError(
                f"onnx 后端依赖缺失（{err}）；请安装 onnx extra：pip install -e \".[onnx]\""
            ) from err

        path = self._find_model_file(model_dir, model_file)
        self._np = np
        self.session = onnxruntime.InferenceSession(path, providers=["CPUExecutionProvider"])
        self._input_names = {i.name for i in self.session.get_inputs()}
        outputs = self.session.get_outputs()
        if not outputs:
            raise RuntimeError(f"模型没有输出：{path}")
        last_dim = outputs[0].shape[-1]
        self.tokenizer = Tokenizer.from_file(os.path.join(model_dir, "tokenizer.json"))
        self.tokenizer.enable_truncation(max_length=512)
        self.tokenizer.enable_padding()
        self.query_prefix = query_prefix
        self.name = name or os.path.basename(os.path.normpath(model_dir))
        self.dim = int(last_dim) if isinstance(last_dim, int) else 384

    @staticmethod
    def _find_model_file(model_dir: str, model_file: str) -> str:
        if model_file:
            path = os.path.join(model_dir, model_file)
            if not os.path.isfile(path):
                raise RuntimeError(f"模型文件不存在：{path}")
            return path
        for candidate in ("model.onnx", "model_optimized.onnx"):
            path = os.path.join(model_dir, candidate)
            if os.path.isfile(path):
                return path
        raise RuntimeError(
            f"模型目录 {model_dir} 下未找到 model.onnx / model_optimized.onnx"
            "（放置：model*.onnx + tokenizer.json + config.json）"
        )

    def embed(self, texts: list[str], query: bool = False) -> list[list[float]]:
        np = self._np
        if query and self.query_prefix:
            texts = [self.query_prefix + t for t in texts]
        encodings = self.tokenizer.encode_batch(texts)
        feeds: dict[str, object] = {
            "input_ids": np.array([e.ids for e in encodings], dtype=np.int64),
            "attention_mask": np.array([e.attention_mask for e in encodings], dtype=np.int64),
        }
        if "token_type_ids" in self._input_names:
            feeds["token_type_ids"] = np.array([e.type_ids for e in encodings], dtype=np.int64)
        hidden = self.session.run(None, feeds)[0]  # (batch, seq, dim)
        pooled = np.asarray(hidden)[:, 0]  # CLS pooling（bge 系列）
        norms = np.linalg.norm(pooled, axis=1, keepdims=True)
        norms[norms == 0] = 1e-12
        normalized = pooled / norms
        return [[float(x) for x in row] for row in normalized]
