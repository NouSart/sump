"""设置中心客户端的原生 SPv2.1 测试：register call / heartbeat event / roster & settings event / 断线重连。

假中心只提供 /spv2；不依赖外网与真模型：后端用 FakeBackend，中心是进程内 websockets 服务端。
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Callable

import pytest
import websockets
from websockets.datastructures import Headers
from websockets.http11 import Response

from sump_embedding import spv2
from sump_embedding.backends import FakeBackend
from sump_embedding.center import CenterLink, center_spv2_url
from sump_embedding.service import Config, Service

TIMEOUT = 15.0
log = logging.getLogger("embedding-center-spv2-test")

CENTER_REF = spv2.service_ref("settings-center")


# ---------- 假中心 ----------


class StubCenter:
    """假中心：只实现 /spv2 原生控制面；native=False 时模拟没有原生入口的中心。"""

    def __init__(self, *, native: bool = True, reject_register: bool = False) -> None:
        self.native = native
        self.reject_register = reject_register
        self.native_attempts = 0  # /spv2 握手次数（native=False 时返回 404）
        self.native_calls: list[spv2.Frame] = []
        self.native_events: list[spv2.Frame] = []
        self.native_registrations: list[dict] = []
        self.native_heartbeats: list[str] = []
        self.native_conns: set = set()
        self.roster: dict = {
            "services": [{"name": "settings-center", "addr": "ws://127.0.0.1:1/spv2"}],
            "revision": 1,
        }
        self._server: websockets.Server | None = None
        self.url = ""

    async def start(self) -> str:
        self._server = await websockets.serve(
            self._handler,
            "127.0.0.1",
            0,
            max_size=None,
            process_request=self._process_request,
        )
        port = self._server.sockets[0].getsockname()[1]
        self.url = f"ws://127.0.0.1:{port}/spv2"
        return self.url

    async def _process_request(self, _conn: object, request: object) -> Response | None:
        path = str(getattr(request, "path", "")).split("?", 1)[0]
        if path != "/spv2":
            return Response(404, "Not Found", Headers())
        self.native_attempts += 1
        if not self.native:
            return Response(404, "Not Found", Headers())  # 没有原生入口的中心
        return None

    async def _handler(self, ws: websockets.ServerConnection) -> None:
        self.native_conns.add(ws)
        try:
            async for raw in ws:
                frame = spv2.decode(raw)
                if frame.type == spv2.TYPE_CALL:
                    self.native_calls.append(frame)
                    if frame.method != "register":
                        continue
                    if self.reject_register:
                        await ws.send(
                            spv2.encode(
                                spv2.new_error_reply(
                                    frame, CENTER_REF, spv2.CODE_INVALID_ARGUMENT, "register: 拒绝（测试）"
                                )
                            )
                        )
                        continue
                    self.native_registrations.append(spv2.decode_body(frame.body))
                    await ws.send(spv2.encode(spv2.new_reply(frame, CENTER_REF, self.roster)))
                    continue
                if frame.type == spv2.TYPE_EVENT:
                    self.native_events.append(frame)
                    if frame.method == "heartbeat":
                        payload = spv2.decode_body(frame.body)
                        self.native_heartbeats.append(str(payload.get("status", "ok")))
        except websockets.ConnectionClosed:
            pass
        finally:
            self.native_conns.discard(ws)

    async def broadcast_native(self, method: str, payload: object) -> None:
        for ws in list(self.native_conns):
            frame = spv2.new_event(
                work=spv2.new_id(),
                from_=CENTER_REF,
                to=spv2.service_ref("embedding"),
                method=method,
                body=payload,
            )
            await ws.send(spv2.encode(frame))

    async def drop_native(self) -> None:
        for ws in list(self.native_conns):
            await ws.close()

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


def make_service(center_url: str, heartbeat_ms: int = 100) -> Service:
    return Service(
        Config(addr="127.0.0.1:0", center=center_url, heartbeat_ms=heartbeat_ms),
        FakeBackend(dim=32),
        log,
    )


async def call_spv2(addr: str, method: str, body: object) -> spv2.Frame:
    """以第三方身份对服务发起一跳原生 call，返回 reply 帧。"""
    async with websockets.connect(f"ws://{addr}/spv2", max_size=None) as ws:
        frame = spv2.new_call(
            work=spv2.new_id(),
            from_=spv2.service_ref("probe"),
            to=spv2.service_ref("embedding"),
            method=method,
            body=body,
        )
        await ws.send(spv2.encode(frame))
        return spv2.decode(await asyncio.wait_for(ws.recv(), TIMEOUT))


def run(scenario: Callable[[], object]) -> None:
    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT + 5))  # type: ignore[arg-type]


def _center_link() -> CenterLink:
    return CenterLink(
        name="svc-a",
        center_url="ws://127.0.0.1:9999/spv2",
        self_url="ws://127.0.0.1:9999/spv2",
        description="夹具服务",
        provides=[{"action": "ping", "output": "pong", "tool": True}],
        settings=[{"key": "demo.setting", "default": "5m"}],
        heartbeat_ms=15000,
        log=logging.getLogger("test"),
    )


# ---------- 地址映射 / 原生帧锚定 ----------


def test_center_spv2_url_mapping() -> None:
    assert center_spv2_url("ws://127.0.0.1:9000/spv2") == "ws://127.0.0.1:9000/spv2"
    assert center_spv2_url("ws://127.0.0.1:9000") == "ws://127.0.0.1:9000/spv2"
    assert center_spv2_url("ws://127.0.0.1:9000/") == "ws://127.0.0.1:9000/spv2"


def test_register_frame_and_heartbeat_event() -> None:
    """原生 register 的 body 与心跳 event 的 body 即线路契约。"""
    link = _center_link()
    frame = link.register_frame()
    assert frame.v == spv2.PROTOCOL_VERSION
    assert frame.type == spv2.TYPE_CALL and frame.method == "register"
    assert frame.id and frame.work and frame.content_type == spv2.CONTENT_TYPE_MSGPACK
    assert frame.from_ == spv2.service_ref("svc-a")
    assert frame.to == spv2.service_ref("settings-center")
    assert spv2.decode_body(frame.body) == link.register_payload()

    event = link.heartbeat_event("ok")
    assert event.type == spv2.TYPE_EVENT and event.method == "heartbeat"
    assert event.id and event.work
    assert spv2.decode_body(event.body) == {"status": "ok"}


# ---------- 原生控制面 ----------


def test_native_register_heartbeat_and_roster() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            # register 是 call；from / to / method 与 Go 侧一致
            assert center.native_attempts == 1
            assert center.native_calls, "没有收到原生 call"
            reg = center.native_calls[0]
            assert reg.type == spv2.TYPE_CALL and reg.method == "register"
            assert reg.from_.kind == "service" and reg.from_.id == "embedding"
            assert reg.to.kind == "service" and reg.to.id == "settings-center"
            assert reg.work and reg.content_type == spv2.CONTENT_TYPE_MSGPACK

            # body 即 RegisterPayload
            body = center.native_registrations[0]
            assert body["name"] == "embedding"
            assert body["addr"] == f"ws://{svc.addr}/spv2"
            assert body["heartbeat_ms"] == 100
            assert body["title"] == "向量"
            assert body["settings"] == []
            assert body["provides"][0]["action"] == "embed"

            # register 回复带回名册
            assert "settings-center" in svc.link.roster

            # 心跳是 event（不是 call），间隔到了就发
            await wait_until(lambda: len(center.native_heartbeats) >= 2, 5, "原生心跳 ≥2")
            hb = next(
                f
                for f in center.native_events
                if f.method == "heartbeat" and spv2.decode_body(f.body).get("status") == "ok"
            )
            assert hb.type == spv2.TYPE_EVENT
            assert hb.from_.id == "embedding" and hb.to.id == "settings-center"
            assert hb.work, "event.work 必填（对齐 Go Frame.Validate）"

            # roster 事件 → 名册更新
            await center.broadcast_native(
                "roster",
                {
                    "services": [
                        {"name": "settings-center", "addr": "ws://127.0.0.1:1/spv2"},
                        {"name": "memory", "addr": "ws://127.0.0.1:9201/spv2"},
                    ],
                    "revision": 2,
                },
            )
            await wait_until(lambda: "memory" in svc.link.roster, 5, "roster 事件更新")

            # settings 是已知事件：静默忽略，连接与动作不受影响
            await center.broadcast_native("settings", {"cause": "set_setting", "service": "x", "results": []})
            reply = await call_spv2(svc.addr, "embed", {"texts": ["苹果"]})
            assert reply.status == spv2.STATUS_OK
            assert spv2.decode_body(reply.body)["dim"] == 32
        finally:
            await svc.stop()
        # 关停：bye 走原生心跳事件（中心据此立即摘除名册）
        await wait_until(lambda: "bye" in center.native_heartbeats, 5, "bye")
        await center.stop()

    run(scenario)


def test_native_reconnect_reregisters() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            await wait_until(lambda: len(center.native_registrations) == 1, 5, "首次原生注册")
            # 中心断开 → 1s 后自动重连并重新走原生 register
            await center.drop_native()
            await wait_until(lambda: len(center.native_registrations) >= 2, 8, "原生重注册")
            await wait_until(lambda: svc.link.registered, 5, "registered 恢复")
            reply = await call_spv2(svc.addr, "embed", {"texts": ["苹果"]})
            assert reply.status == spv2.STATUS_OK
        finally:
            await svc.stop()
        await center.stop()

    run(scenario)


# ---------- 引导失败：没有原生入口 / 注册被拒 ----------


def test_start_fails_when_center_has_no_spv2() -> None:
    """中心没有 /spv2：握手 404，首次注册直接失败（不回退任何旧路径）。"""

    async def scenario() -> None:
        center = StubCenter(native=False)
        url = await center.start()
        svc = make_service(url)
        try:
            with pytest.raises(Exception):
                await svc.start()
            assert center.native_attempts == 1, "应尝试原生入口"
        finally:
            await svc.stop()
            await center.stop()

    run(scenario)


def test_start_fails_when_native_register_rejected() -> None:
    """/spv2 在但 register 被拒：首次注册直接失败，不回退任何旧路径。"""

    async def scenario() -> None:
        center = StubCenter(reject_register=True)
        url = await center.start()
        svc = make_service(url)
        try:
            with pytest.raises(RuntimeError, match="被拒"):
                await svc.start()
            assert center.native_calls and center.native_calls[0].method == "register"
            assert center.native_registrations == []
        finally:
            await svc.stop()
            await center.stop()

    run(scenario)
