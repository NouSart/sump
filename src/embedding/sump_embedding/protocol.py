"""SUMP 的 ULID 形态 id 生成（Python 侧）。

协议与载荷都由 SPv2.1（Go `src/spv2`、`src/protocol`）定义；
本文件只保留跨语言共用的 id 生成，不再携带任何 v1 信封结构。
"""

from __future__ import annotations

import secrets
import time

_ID_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def new_id() -> str:
    """生成 ULID 形态的消息 id（26 字符：48 位毫秒时间 + 80 位随机）。"""
    t = int(time.time() * 1000)
    time_part = ["0"] * 10
    for i in range(9, -1, -1):
        time_part[i] = _ID_ALPHABET[t % 32]
        t //= 32
    suffix = "".join(_ID_ALPHABET[b % 32] for b in secrets.token_bytes(16))
    return "".join(time_part) + suffix
