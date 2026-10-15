"""SUMP 协议（Python 侧）：信封 + MessagePack 编解码。

与 Go 侧 protocol/ 逐字段对齐——键名即线路键名（snake_case），
空字段与 Go 的 omitempty 对齐（不出现键）。
语义锚定：tests/test_protocol.py 用 Go 产出的字节做解码断言；
反向锚定由根 tests/ 的跨语言真进程 e2e（Go 服务 ↔ 本服务）承担。
"""

from __future__ import annotations

import secrets
import time

import msgspec

PROTOCOL_VERSION = 1

TYPE_REGISTER = "register"
TYPE_RESPONSE = "response"
TYPE_ROSTER = "roster"
TYPE_JUMP = "jump"
TYPE_HEARTBEAT = "heartbeat"
TYPE_SETTINGS = "settings"

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


def new_envelope(
    typ: str,
    from_: str,
    to: str,
    trace: str = "",
    payload: object | None = None,
    boss: str = "",
) -> dict:
    """构造一条消息；trace 为空时自动生成（与 Go NewEnvelope 一致）。"""
    env: dict = {"v": PROTOCOL_VERSION, "id": new_id()}
    env["trace"] = trace or new_id()
    if boss:
        env["boss"] = boss
    env["type"] = typ
    env["from"] = from_
    env["to"] = to
    if payload is not None:
        env["payload"] = payload
    return env


def new_response(req: dict, from_: str, payload: dict) -> dict:
    """构造对 req 的响应：沿用请求的 id（配对）与 trace（同链）。"""
    env = new_envelope(TYPE_RESPONSE, from_, str(req.get("from", "")), str(req.get("trace", "") or ""), payload)
    env["id"] = str(req.get("id", ""))
    return env


def encode(env: dict) -> bytes:
    """信封 → MessagePack 字节（WS 二进制帧内容）。"""
    return msgspec.msgpack.encode(env)


def decode(data: bytes | bytearray | memoryview) -> dict:
    """MessagePack 字节 → 信封（字段缺失有默认值；payload 为嵌套值）。"""
    raw = msgspec.msgpack.decode(bytes(data))
    if not isinstance(raw, dict):
        raise ValueError("信封不是 map")
    return {
        "v": raw.get("v", 0) if isinstance(raw.get("v"), int) else 0,
        "id": raw.get("id", "") if isinstance(raw.get("id"), str) else "",
        "trace": raw.get("trace", "") if isinstance(raw.get("trace"), str) else "",
        "boss": raw.get("boss", "") if isinstance(raw.get("boss"), str) else "",
        "type": raw.get("type", "") if isinstance(raw.get("type"), str) else "",
        "from": raw.get("from", "") if isinstance(raw.get("from"), str) else "",
        "to": raw.get("to", "") if isinstance(raw.get("to"), str) else "",
        "payload": raw.get("payload"),
    }
