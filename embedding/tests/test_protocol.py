"""协议层测试：解码 Go 侧产出的字节（跨语言锚点）+ 编码往返与 omitempty 语义。"""

import base64

from fixtures import (  # type: ignore[import-not-found]
    GO_HEARTBEAT,
    GO_REGISTER,
    GO_RESPONSE,
    GO_ROSTER,
    GO_SETTINGS,
)
from sump_embedding.protocol import (
    PROTOCOL_VERSION,
    TYPE_HEARTBEAT,
    TYPE_REGISTER,
    TYPE_RESPONSE,
    TYPE_ROSTER,
    TYPE_SETTINGS,
    decode,
    encode,
    new_envelope,
    new_id,
    new_response,
)


def b64(s: str) -> bytes:
    return base64.b64decode(s)


def test_decode_go_register() -> None:
    env = decode(b64(GO_REGISTER))
    assert env["v"] == 1
    assert env["type"] == TYPE_REGISTER
    assert env["from"] == "svc-a"
    assert env["to"] == "settings-center"
    assert env["trace"] == "TRACE-1"
    assert env["id"].startswith("01FIXREGISTER"), env["id"]
    p = env["payload"]
    assert p["name"] == "svc-a"
    assert p["addr"] == "ws://127.0.0.1:9999/ws"
    assert p["description"] == "夹具服务"
    assert p["provides"][0]["action"] == "ping"
    assert p["provides"][0]["output"] == "pong"
    assert p["provides"][0]["tool"] is True
    assert p["settings"][0]["key"] == "demo.setting"
    assert p["settings"][0]["default"] == "5m"
    assert p["heartbeat_ms"] == 15000


def test_decode_go_roster() -> None:
    env = decode(b64(GO_ROSTER))
    assert env["type"] == TYPE_ROSTER
    assert env["from"] == "settings-center"
    r = env["payload"]
    assert r["revision"] == 3
    assert r["services"][0]["name"] == "svc-a"
    assert r["services"][0]["addr"] == "ws://127.0.0.1:9999/ws"
    assert r["services"][0]["heartbeat_ms"] == 15000


def test_decode_go_response() -> None:
    env = decode(b64(GO_RESPONSE))
    assert env["type"] == TYPE_RESPONSE
    assert env["to"] == "svc-a"
    assert env["trace"] == "TRACE-1"  # NewResponse 沿用请求 trace
    assert env["payload"]["ok"] is True
    sr = env["payload"]["data"]
    assert sr["services"][0]["service"] == "svc-a"
    assert sr["services"][0]["settings"][0]["value"] == "10m"
    assert sr["services"][0]["settings"][0]["overridden"] is True


def test_decode_go_heartbeat() -> None:
    env = decode(b64(GO_HEARTBEAT))
    assert env["type"] == TYPE_HEARTBEAT
    assert env["payload"]["status"] == "ok"


def test_decode_go_settings_event() -> None:
    env = decode(b64(GO_SETTINGS))
    assert env["type"] == TYPE_SETTINGS
    p = env["payload"]
    assert p["cause"] == "set_setting"
    assert p["service"] == "svc-a"
    assert p["results"][0]["key"] == "tool.notify_mode"
    assert p["results"][0]["value"] == "batch"
    assert p["results"][0]["ok"] is True
    assert p["results"][1]["ok"] is False
    assert "未送达" in p["results"][1]["error"]


def test_envelope_roundtrip() -> None:
    env = new_envelope(
        "jump",
        "embedding",
        "memory",
        "",
        {"action": "embed", "input": {"texts": ["苹果"], "query": True}},
    )
    back = decode(encode(env))
    assert back["v"] == PROTOCOL_VERSION
    assert back["id"] == env["id"]
    assert back["trace"]  # 空 trace 自动生成
    assert back["type"] == "jump"
    assert back["payload"]["action"] == "embed"
    assert back["payload"]["input"]["texts"] == ["苹果"]


def test_envelope_omitempty() -> None:
    import msgspec

    env = new_envelope("heartbeat", "a", "b", "T", None)
    raw = msgspec.msgpack.decode(encode(env))  # 线路键（未见过的键不出现）
    assert "payload" not in raw
    assert "boss" not in raw
    assert raw["trace"] == "T"
    # decode() 归一化视图仍给出空值（调用方不必判键存在）
    norm = decode(encode(env))
    assert norm["payload"] is None and norm["boss"] == ""


def test_new_response_pairs_request() -> None:
    req = new_envelope("jump", "caller", "embedding", "TRACE-9", {"action": "embed"})
    resp = new_response(req, "embedding", {"ok": True})
    assert resp["id"] == req["id"]
    assert resp["trace"] == "TRACE-9"
    assert resp["to"] == "caller"
    assert resp["type"] == TYPE_RESPONSE


def test_new_id_shape() -> None:
    a, b = new_id(), new_id()
    assert len(a) == 26 and len(b) == 26
    assert a != b
    assert all(c in "0123456789ABCDEFGHJKMNPQRSTVWXYZ" for c in a)
