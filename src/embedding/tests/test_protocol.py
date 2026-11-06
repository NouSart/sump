"""协议层测试：跨语言载荷锚点 + 原生 register/heartbeat 回锚。

Go 侧不再产出 v1 信封，夹具改为 Frame body（payload 字节）；断言内容不变。
"""

import base64
import logging

import msgspec

from fixtures import (  # type: ignore[import-not-found]
    GO_HEARTBEAT_BODY,
    GO_REGISTER_BODY,
    GO_ROSTER_BODY,
    GO_SETTINGS_BODY,
)
from sump_embedding import spv2
from sump_embedding.center import CenterLink
from sump_embedding.protocol import new_id


def b64(s: str) -> bytes:
    return base64.b64decode(s)


def body(fixture: str) -> dict:
    raw = msgspec.msgpack.decode(b64(fixture))
    assert isinstance(raw, dict)
    return raw


def test_decode_go_register_body() -> None:
    p = body(GO_REGISTER_BODY)
    assert p["name"] == "svc-a"
    assert p["addr"] == "ws://127.0.0.1:9999/ws"
    assert p["description"] == "夹具服务"
    assert p["provides"][0]["action"] == "ping"
    assert p["provides"][0]["output"] == "pong"
    assert p["provides"][0]["tool"] is True
    assert p["settings"][0]["key"] == "demo.setting"
    assert p["settings"][0]["default"] == "5m"
    assert p["heartbeat_ms"] == 15000


def test_decode_go_roster_body() -> None:
    r = body(GO_ROSTER_BODY)
    assert r["revision"] == 3
    assert r["services"][0]["name"] == "svc-a"
    assert r["services"][0]["addr"] == "ws://127.0.0.1:9999/ws"
    assert r["services"][0]["heartbeat_ms"] == 15000


def test_decode_go_heartbeat_body() -> None:
    assert body(GO_HEARTBEAT_BODY)["status"] == "ok"


def test_decode_go_settings_body() -> None:
    p = body(GO_SETTINGS_BODY)
    assert p["cause"] == "set_setting"
    assert p["service"] == "svc-a"
    assert p["results"][0]["key"] == "tool.notify_mode"
    assert p["results"][0]["value"] == "batch"
    assert p["results"][0]["ok"] is True
    assert p["results"][1]["ok"] is False
    assert "未送达" in p["results"][1]["error"]


def test_new_id_shape() -> None:
    a, b = new_id(), new_id()
    assert len(a) == 26 and len(b) == 26
    assert a != b
    assert all(c in "0123456789ABCDEFGHJKMNPQRSTVWXYZ" for c in a)


# ---------- 反向锚定：Python 原生帧 → 必须与同一批夹具的 body 一致 ----------
#
# 这批夹具在本文件里此前只被「解码」（单向）。实测盲区：把 center.py 注册载荷的
# heartbeat_ms 改成 hb_ms 后，本文件全部用例 + 根 tests/ 的跨语言真进程 e2e 照样全绿——
# 中心把服务声明的心跳间隔静默读成 0，退回全局默认（settings-center/hub/hub.go 的 Sweep），
# 声明间隔偏长的服务会被误判离线。夹具模块原先声称「反向锚定由根 e2e 承担」，实测不成立。
#
# 用例刻意走**真实生产代码**（CenterLink 的原生组包方法），而不是在测试里另抄一份线路字典——
# 后者只能证明「测试自己抄对了」，对生产代码的漂移毫无约束力。
#
# 为什么不逐字节相等：Go 把 int64 编成定长 d3（heartbeat_ms=15000 → d3 00…3a98），
# msgspec 编成最窄的 cd 3a98。整数字宽不影响互解（实测其余字节逐位相同），
# 故锚定「键名 + 取值 + 嵌套结构」——漂移会伤到的正是这一层。


def _fixture_center_link() -> CenterLink:
    """与 Go 夹具同输入的 CenterLink（只用构造参数，不建连接）。

    self_url 的 /ws 是 GO_REGISTER_BODY 夹具里那个服务登记的地址（夹具字节的一部分），
    不是本服务的路由；本服务只监听 /spv2。
    """
    return CenterLink(
        name="svc-a",
        center_url="ws://127.0.0.1:9999/spv2",
        self_url="ws://127.0.0.1:9999/ws",
        description="夹具服务",
        provides=[{"action": "ping", "output": "pong", "tool": True}],
        settings=[{"key": "demo.setting", "default": "5m"}],
        heartbeat_ms=15000,
        log=logging.getLogger("test"),
    )


def _native_body(frame: spv2.Frame) -> dict:
    out = spv2.decode_body(frame.body)
    assert isinstance(out, dict)
    return out


def test_register_frame_body_matches_go_fixture() -> None:
    """原生 register 的 body：键名漂移（如 heartbeat_ms → hb_ms）会被中心静默读成零值。"""
    frame = _fixture_center_link().register_frame()
    assert frame.type == spv2.TYPE_CALL and frame.method == "register"
    assert _native_body(frame) == body(GO_REGISTER_BODY)


def test_heartbeat_event_body_matches_go_fixture() -> None:
    event = _fixture_center_link().heartbeat_event("ok")
    assert event.type == spv2.TYPE_EVENT and event.method == "heartbeat"
    assert _native_body(event) == body(GO_HEARTBEAT_BODY)
