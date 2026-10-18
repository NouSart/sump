"""协议层测试：跨语言锚点（解 Go 产出的字节 + 本侧编码回锚夹具）+ 编码往返与 omitempty 语义。"""

import base64
import logging

import msgspec

from fixtures import (  # type: ignore[import-not-found]
    GO_HEARTBEAT,
    GO_REGISTER,
    GO_RESPONSE,
    GO_ROSTER,
    GO_SETTINGS,
)
from sump_embedding.center import CenterLink
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


# ---------- 反向锚定：Python 编码 → 必须与同一批夹具一致 ----------
#
# 这批夹具在本文件里此前只被「解码」（单向）。实测盲区：把 center.py 注册载荷的
# heartbeat_ms 改成 hb_ms 后，本文件全部用例 + 根 tests/ 的跨语言真进程 e2e 照样全绿——
# 中心把服务声明的心跳间隔静默读成 0，退回全局默认（settings-center/hub/hub.go 的 Sweep），
# 声明间隔偏长的服务会被误判离线。夹具模块原先声称「反向锚定由根 e2e 承担」，实测不成立。
#
# 用例刻意走**真实生产代码**（CenterLink 的组包方法），而不是在测试里另抄一份线路字典——
# 后者只能证明「测试自己抄对了」，对生产代码的漂移毫无约束力。
#
# 为什么不逐字节相等：Go 把 int64 编成定长 d3（heartbeat_ms=15000 → d3 00…3a98），
# msgspec 编成最窄的 cd 3a98。整数字宽不影响互解（实测其余字节逐位相同），
# 故锚定「键名 + 取值 + 嵌套结构」——漂移会伤到的正是这一层。


def _center_link() -> CenterLink:
    """与 Go 夹具同输入的 CenterLink（只用构造参数，不建连接）。"""
    return CenterLink(
        name="svc-a",
        center_url="ws://127.0.0.1:9999/ws",
        self_url="ws://127.0.0.1:9999/ws",
        description="夹具服务",
        provides=[{"action": "ping", "output": "pong", "tool": True}],
        settings=[{"key": "demo.setting", "default": "5m"}],
        heartbeat_ms=15000,
        log=logging.getLogger("test"),
    )


def _assert_matches_fixture(env: dict, want_b64: str) -> None:
    """断言本侧编出的线路视图与夹具一致（键名、取值、嵌套结构）。"""
    got = msgspec.msgpack.decode(encode(env))
    want = msgspec.msgpack.decode(b64(want_b64))
    assert got == want, "Python 编码与固化夹具不一致（线路契约已漂移）"


def test_register_envelope_matches_go_fixture() -> None:
    """注册这一跳：键名漂移（如 heartbeat_ms → hb_ms）会被中心静默读成零值。"""
    env = _center_link().register_envelope()
    # 生产上 id 与 trace 都由 new_id() 生成；这里按夹具固定，好比对。
    env["id"] = "01FIXREGISTER0000000000000"
    env["trace"] = "TRACE-1"
    _assert_matches_fixture(env, GO_REGISTER)


def test_heartbeat_envelope_matches_go_fixture() -> None:
    env = _center_link().heartbeat_envelope("ok")
    env["id"] = "01FIXHEARTBEAT000000000000"
    env["trace"] = "TRACE-3"
    _assert_matches_fixture(env, GO_HEARTBEAT)
