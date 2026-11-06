"""服务集成测试：真 WS + 假中心（原生 /spv2）——注册 / 名册 / 心跳 / 断线自愈 / bye / embed 全路径。

不依赖模型与网络：后端用 FakeBackend（确定性）；假中心是进程内 websockets 服务端，只实现 /spv2。
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Callable

import pytest
import websockets
from websockets.exceptions import InvalidStatus

from sump_embedding import spv2
from sump_embedding.backends import FakeBackend
from sump_embedding.service import Config, Service

TIMEOUT = 15.0
log = logging.getLogger("embedding-test")

CENTER_REF = spv2.service_ref("settings-center")
CLIENT = spv2.service_ref("probe")


# ---------- 假中心 ----------


class StubCenter:
    """最小原生假中心：register call 回名册；心跳 / bye 收下即可。"""

    def __init__(self) -> None:
        self.registrations: list[dict] = []
        self.heartbeats: list[str] = []
        self.conns: set = set()
        self.roster: dict = {
            "services": [{"name": "settings-center", "addr": "ws://127.0.0.1:1/spv2"}],
            "revision": 1,
        }
        self._server: websockets.Server | None = None
        self.url = ""

    async def start(self) -> str:
        self._server = await websockets.serve(self._handler, "127.0.0.1", 0, max_size=None)
        port = self._server.sockets[0].getsockname()[1]
        self.url = f"ws://127.0.0.1:{port}/spv2"
        return self.url

    async def _handler(self, ws: websockets.ServerConnection) -> None:
        self.conns.add(ws)
        try:
            async for raw in ws:
                frame = spv2.decode(raw)
                if frame.type == spv2.TYPE_CALL and frame.method == "register":
                    self.registrations.append(spv2.decode_body(frame.body))
                    await ws.send(spv2.encode(spv2.new_reply(frame, CENTER_REF, self.roster)))
                elif frame.type == spv2.TYPE_EVENT and frame.method == "heartbeat":
                    payload = spv2.decode_body(frame.body)
                    self.heartbeats.append(str(payload.get("status", "ok")))
        except websockets.ConnectionClosed:
            pass
        finally:
            self.conns.discard(ws)

    async def drop_all(self) -> None:
        for ws in list(self.conns):
            await ws.close()

    async def broadcast(self, method: str, payload: object) -> None:
        for ws in list(self.conns):
            frame = spv2.new_event(
                work=spv2.new_id(),
                from_=CENTER_REF,
                to=spv2.service_ref("embedding"),
                method=method,
                body=payload,
            )
            await ws.send(spv2.encode(frame))

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None


# ---------- 辅助 ----------


async def wait_until(cond: Callable[[], bool], timeout: float, desc: str) -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < deadline:
        if cond():
            return
        await asyncio.sleep(0.02)
    raise AssertionError(f"等待超时: {desc}")


def make_service(center_url: str, heartbeat_ms: int = 100, **over: object) -> Service:
    cfg = Config(addr="127.0.0.1:0", center=center_url, heartbeat_ms=heartbeat_ms, **over)  # type: ignore[arg-type]
    return Service(cfg, FakeBackend(dim=32), log)


async def call(
    addr: str,
    method: str,
    body: object = None,
    *,
    frame: spv2.Frame | None = None,
) -> spv2.Frame:
    """以第三方身份对服务发起一跳原生 call，返回 reply 帧。"""
    async with websockets.connect(f"ws://{addr}/spv2", max_size=None) as ws:
        if frame is None:
            frame = spv2.new_call(
                work=spv2.new_id(),
                from_=CLIENT,
                to=spv2.service_ref("embedding"),
                method=method,
                body=body,
            )
        await ws.send(spv2.encode(frame))
        return spv2.decode(await asyncio.wait_for(ws.recv(), TIMEOUT))


def make_call(method: str, body: object = None) -> spv2.Frame:
    return spv2.new_call(
        work=spv2.new_id(),
        from_=CLIENT,
        to=spv2.service_ref("embedding"),
        method=method,
        body=body,
    )


def reply_data(reply: spv2.Frame) -> dict:
    assert reply.status == spv2.STATUS_OK, reply.error
    data = spv2.decode_body(reply.body)
    assert isinstance(data, dict)
    return data


def dot(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b))


# ---------- 用例 ----------


def test_default_center_is_native_spv2() -> None:
    """未显式配置时，服务入口与 Config 都默认连接原生 /spv2。"""
    from sump_embedding.__main__ import parse_args

    assert Config().center == "ws://127.0.0.1:9000/spv2"
    assert parse_args([]).center == "ws://127.0.0.1:9000/spv2"


def test_register_heartbeat_embed_bye() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            # 注册名片：名字 / 自述地址 / 心跳 / 零设置 / 唯一动作 embed
            reg = center.registrations[0]
            assert reg["name"] == "embedding"
            assert reg["addr"] == f"ws://{svc.addr}/spv2"
            assert reg["heartbeat_ms"] == 100
            assert reg["settings"] == []
            assert reg["provides"][0]["action"] == "embed"
            # 名册（register 回复带回）已到账
            assert "settings-center" in svc.link.roster
            # 心跳按间隔发出
            await wait_until(lambda: len(center.heartbeats) >= 2, 5, "心跳 ≥2")

            # embed：批量 + 归一化 + 模型标识
            data = reply_data(await call(svc.addr, "embed", {"texts": ["苹果", "爱吃苹果"]}))
            assert data["dim"] == 32 and data["model"] == "fake-bigram-v1"
            assert len(data["vectors"]) == 2
            assert abs(sum(x * x for x in data["vectors"][0]) ** 0.5 - 1.0) < 1e-9
            # query 标志可用且不影响成功路径
            data2 = reply_data(await call(svc.addr, "embed", {"texts": ["苹果"], "query": True}))
            assert data2["dim"] == 32
        finally:
            await svc.stop()
        # 关停：bye 已发出（中心据此立即摘除名册）
        assert "bye" in center.heartbeats
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_protocol_level_errors() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            # 未知 method → not_found
            reply = await call(svc.addr, "no_such_action")
            assert reply.status == spv2.STATUS_ERROR
            assert reply.error is not None and reply.error.code == spv2.CODE_NOT_FOUND
            assert "unknown method: no_such_action" in reply.error.message

            # 坏 body / 空 body / 错版本 / 错目标 / 错 content_type → invalid_argument
            bad_body = make_call("embed", {"texts": ["苹果"]})
            bad_body.body = b"\xc1"
            bad_version = make_call("embed", {"texts": ["苹果"]})
            bad_version.v = "1"
            bad_target = make_call("embed", {"texts": ["苹果"]})
            bad_target.to = spv2.service_ref("other")
            bad_type = make_call("embed", {"texts": ["苹果"]})
            bad_type.content_type = "application/json"
            for frame in (bad_body, make_call("embed", None), bad_version, bad_target, bad_type):
                reply = await call(svc.addr, "embed", frame=frame)
                assert reply.status == spv2.STATUS_ERROR, frame
                assert reply.error is not None
                assert reply.error.code == spv2.CODE_INVALID_ARGUMENT, (frame, reply.error)

            # 错误之后连接与会话照常可用
            assert reply_data(await call(svc.addr, "embed", {"texts": ["苹果"]}))["dim"] == 32
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_embed_validation_errors() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url, max_batch=3)
        await svc.start()
        try:
            cases = [
                (None, "body 为空"),
                ([], "输入必须是对象"),
                ({"texts": []}, "texts 必填"),
                ({"texts": "苹果"}, "texts 必填"),
                ({"texts": ["ok", 3]}, "必须全为字符串"),
                ({"texts": ["a", "b", "c", "d"]}, "批次超过上限 3"),
            ]
            for input_, want in cases:
                reply = await call(svc.addr, "embed", input_)
                assert reply.status == spv2.STATUS_ERROR, (input_, reply)
                assert reply.error is not None
                assert reply.error.code == spv2.CODE_INVALID_ARGUMENT, (input_, reply.error)
                assert want in reply.error.message, (input_, reply.error)
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_bootstrap_failure_when_center_unreachable() -> None:
    async def scenario() -> None:
        svc = make_service("ws://127.0.0.1:1/spv2", heartbeat_ms=100)
        with pytest.raises(Exception):
            await svc.start()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_non_native_path_returns_404() -> None:
    """只有 /spv2 会被接入；/ws 等路径在握手阶段明确 404，不能误落到原生端点。"""

    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            for path in ("/ws", "/", "/spv2/extra"):
                with pytest.raises(InvalidStatus) as excinfo:
                    async with websockets.connect(f"ws://{svc.addr}{path}", max_size=None):
                        pass
                assert excinfo.value.response.status_code == 404, path
            # /spv2 仍然正常
            assert reply_data(await call(svc.addr, "embed", {"texts": ["苹果"]}))["dim"] == 32
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_reconnect_after_center_drop() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            assert len(center.registrations) == 1
            # 中心侧断开全部连接 → 服务应自动重连并重新注册（1s 快连）
            await center.drop_all()
            await wait_until(lambda: len(center.registrations) >= 2, 8, "重新注册")
            await wait_until(lambda: svc.link.registered, 5, "registered 恢复")
            # 重连后功能照常
            assert reply_data(await call(svc.addr, "embed", {"texts": ["苹果"]}))["dim"] == 32
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT + 5))


def test_settings_event_ignored_and_roster_updated() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            # settings 是已知事件：静默忽略（不崩、不影响后续）
            await center.broadcast("settings", {"cause": "set_setting", "service": "x", "results": []})
            # roster 广播：名册更新
            await center.broadcast(
                "roster",
                {
                    "services": [
                        {"name": "settings-center", "addr": "ws://127.0.0.1:1/spv2"},
                        {"name": "memory", "addr": "ws://127.0.0.1:9201/spv2"},
                    ],
                    "revision": 2,
                },
            )
            await wait_until(lambda: "memory" in svc.link.roster, 5, "名册更新")
            assert reply_data(await call(svc.addr, "embed", {"texts": ["苹果"]}))["dim"] == 32
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))
