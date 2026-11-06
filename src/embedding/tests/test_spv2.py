"""SPv2.1 原生端点集成测试：真 WS + 假中心（/spv2）+ FakeBackend。

覆盖 /spv2 的 embed 成功路径（reply 配对 / vectors / dim / model）、unknown method
（not_found）、坏 body / 错版本 / 空 body / 非 map（invalid_argument）、hello/health，
以及原生端点的连接隔离（一条连接断开只取消它自己在飞的动作）。
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import msgspec
import websockets

from sump_embedding import spv2
from sump_embedding.backends import FakeBackend
from sump_embedding.service import Config, Service

TIMEOUT = 15.0
log = logging.getLogger("embedding-spv2-test")

CLIENT = spv2.Ref(kind="service", id="probe", authority=spv2.AUTHORITY_NONE)


class StubCenter:
    """最小原生假中心：register call 回名册；心跳 / bye 收下即可（本文件不测自愈）。"""

    def __init__(self) -> None:
        self._server: websockets.Server | None = None
        self.url = ""
        self.registrations = 0

    async def start(self) -> str:
        self._server = await websockets.serve(self._handler, "127.0.0.1", 0, max_size=None)
        port = self._server.sockets[0].getsockname()[1]
        self.url = f"ws://127.0.0.1:{port}/spv2"
        return self.url

    async def _handler(self, ws: websockets.ServerConnection) -> None:
        try:
            async for raw in ws:
                frame = spv2.decode(raw)
                if frame.type == spv2.TYPE_CALL and frame.method == "register":
                    self.registrations += 1
                    roster = {"services": [], "revision": 1}
                    await ws.send(
                        spv2.encode(spv2.new_reply(frame, spv2.service_ref("settings-center"), roster))
                    )
        except websockets.ConnectionClosed:
            pass

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None


@asynccontextmanager
async def running_service(**over: object) -> AsyncIterator[Service]:
    center = StubCenter()
    url = await center.start()
    svc = Service(
        Config(addr="127.0.0.1:0", center=url, heartbeat_ms=200, **over),  # type: ignore[arg-type]
        FakeBackend(dim=32),
        log,
    )
    await svc.start()
    try:
        yield svc
    finally:
        await svc.stop()
        await center.stop()


async def call_spv2(
    addr: str,
    method: str,
    body: object = None,
    *,
    frame: spv2.Frame | None = None,
) -> tuple[spv2.Frame, spv2.Frame]:
    """以第三方身份发一跳原生 call，返回 (call, reply)。"""
    async with websockets.connect(f"ws://{addr}/spv2", max_size=None) as ws:
        if frame is None:
            frame = spv2.new_call(
                work="w-test",
                from_=CLIENT,
                to=spv2.service_ref("embedding"),
                method=method,
                body=body,
            )
        await ws.send(spv2.encode(frame))
        return frame, spv2.decode(await asyncio.wait_for(ws.recv(), TIMEOUT))


def make_call(method: str, body: object = None) -> spv2.Frame:
    return spv2.new_call(
        work="w-test",
        from_=CLIENT,
        to=spv2.service_ref("embedding"),
        method=method,
        body=body,
    )


def run(scenario) -> None:
    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


# ---------- 帧编码 ----------


def test_frame_wire_shape() -> None:
    """钉住线路形态：MessagePack map、v=spv2.1、from/to 是 {kind,id}、body 是 bin。"""
    frame = make_call("embed", {"texts": ["苹果"]})
    raw = msgspec.msgpack.decode(spv2.encode(frame))
    assert isinstance(raw, dict)
    assert raw["v"] == "spv2.1" and raw["type"] == "call"
    assert raw["from"] == {"kind": "service", "id": "probe", "authority": "none"}
    assert raw["to"] == {"kind": "service", "id": "embedding", "authority": "none"}
    assert isinstance(raw["body"], bytes)
    assert msgspec.msgpack.decode(raw["body"]) == {"texts": ["苹果"]}

    back = spv2.decode(spv2.encode(frame))
    assert back.id == frame.id and back.work == "w-test" and back.method == "embed"
    assert back.body == frame.body


# ---------- /spv2 成功路径 ----------


def test_embed_native_reply() -> None:
    async def scenario() -> None:
        async with running_service() as svc:
            call, reply = await call_spv2(svc.addr, "embed", {"texts": ["苹果", "爱吃苹果"]})
            assert reply.type == spv2.TYPE_REPLY
            assert reply.reply_to == call.id
            assert reply.work == "w-test"
            assert reply.status == spv2.STATUS_OK
            assert reply.from_.kind == "service" and reply.from_.id == "embedding"
            data = spv2.decode_body(reply.body)
            assert isinstance(data, dict)
            assert data["dim"] == 32 and data["model"] == "fake-bigram-v1"
            assert len(data["vectors"]) == 2
            for vector in data["vectors"]:
                assert abs(sum(x * x for x in vector) ** 0.5 - 1.0) < 1e-9
            # query 标志走同一条原生路径
            _, reply2 = await call_spv2(svc.addr, "embed", {"texts": ["苹果"], "query": True})
            assert reply2.status == spv2.STATUS_OK

    run(scenario)


# ---------- /spv2 错误路径 ----------


def test_unknown_method_not_found() -> None:
    async def scenario() -> None:
        async with running_service() as svc:
            _, reply = await call_spv2(svc.addr, "no_such_method")
            assert reply.type == spv2.TYPE_REPLY
            assert reply.status == spv2.STATUS_ERROR
            assert reply.error is not None
            assert reply.error.code == spv2.CODE_NOT_FOUND
            assert "no_such_method" in reply.error.message
            assert reply.body == b""  # 失败 reply 不带 body

    run(scenario)


def test_invalid_argument_paths() -> None:
    async def scenario() -> None:
        async with running_service() as svc:
            # body 不是 MessagePack
            bad_body = make_call("embed", None)
            bad_body.body = b"\xc1"
            # body 是 MessagePack 但不是对象（action_embed 拒绝）
            not_map = make_call("embed", ["not", "a", "map"])
            # 帧版本不对
            bad_version = make_call("embed", {"texts": ["苹果"]})
            bad_version.v = "1"
            # 空 body
            empty_body = make_call("embed", None)
            # content_type 不支持
            bad_type = make_call("embed", {"texts": ["苹果"]})
            bad_type.content_type = "application/json"

            for frame in (bad_body, not_map, bad_version, empty_body, bad_type):
                _, reply = await call_spv2(svc.addr, "embed", frame=frame)
                assert reply.status == spv2.STATUS_ERROR, frame
                assert reply.error is not None
                assert reply.error.code == spv2.CODE_INVALID_ARGUMENT, (frame, reply.error)

    run(scenario)


# ---------- /spv2 最小 hello / health ----------


def test_hello_and_health() -> None:
    async def scenario() -> None:
        async with running_service() as svc:
            _, reply = await call_spv2(
                svc.addr,
                "hello",
                {"protocol": "spv2.1", "actor": CLIENT.wire(), "capabilities": ["msgpack"]},
            )
            assert reply.status == spv2.STATUS_OK
            card = spv2.decode_body(reply.body)["card"]
            assert card["protocol"] == "spv2.1"
            assert card["name"] == "embedding"
            assert card["actor"] == {"kind": "service", "id": "embedding", "authority": "none"}
            names = [m["name"] for m in card["methods"]]
            embed = next(m for m in card["methods"] if m["name"] == "embed")
            assert embed["shape"] == "call"
            assert embed["input"]["required"] == ["texts"]
            assert embed["output"]["required"] == ["vectors", "dim", "model"]
            assert "hello" in names and "health" in names
            assert "metrics" not in names  # 不伪造 metrics

            _, hreply = await call_spv2(svc.addr, "health")
            assert hreply.status == spv2.STATUS_OK
            health = spv2.decode_body(hreply.body)
            assert health["status"] == "ok"
            assert health["version"] == "0.1.0"
            assert isinstance(health["uptime_ms"], int)

    run(scenario)


# ---------- 原生端点连接隔离 ----------


class Harness:
    """动作替身：slow 停在闸门上等测试放行，并记录自己是否被取消。"""

    def __init__(self) -> None:
        self.gate = asyncio.Event()
        self.started = asyncio.Event()
        self.cancelled = asyncio.Event()

    async def slow(self, _body: bytes) -> object:
        self.started.set()
        try:
            await self.gate.wait()
        except asyncio.CancelledError:
            self.cancelled.set()
            raise
        return {"slow": True}

    async def work(self, _body: bytes) -> object:
        return {"work": True}


async def start_native_endpoint(h: Harness) -> tuple[websockets.Server, str]:
    ep = spv2.NativeEndpoint(
        name="probe",
        methods=[
            spv2.MethodSpec(name="slow", handler=h.slow),
            spv2.MethodSpec(name="work", handler=h.work),
        ],
        action_timeout=10.0,
        log=log,
        version="test",
    )
    server = await websockets.serve(ep.handle_conn, "127.0.0.1", 0, max_size=None)
    port = server.sockets[0].getsockname()[1]
    return server, f"ws://127.0.0.1:{port}/spv2"


async def send_native_call(ws: websockets.ClientConnection, method: str) -> None:
    frame = spv2.new_call(
        work=spv2.new_id(),
        from_=CLIENT,
        to=spv2.service_ref("probe"),
        method=method,
    )
    await ws.send(spv2.encode(frame))


def test_closing_one_native_connection_does_not_cancel_other_inflight() -> None:
    """A 收到回执后关闭连接（= 调用方每次短连接）→ B 在飞的动作继续跑完并拿到响应。"""

    async def scenario() -> None:
        h = Harness()
        server, url = await start_native_endpoint(h)
        try:
            conn_b = await websockets.connect(url)
            await send_native_call(conn_b, "slow")
            await asyncio.wait_for(h.started.wait(), 2.0)

            conn_a = await websockets.connect(url)
            await send_native_call(conn_a, "work")
            reply = spv2.decode(await asyncio.wait_for(conn_a.recv(), 2.0))
            assert reply.status == spv2.STATUS_OK, reply.error
            await conn_a.close()
            await asyncio.sleep(0.2)

            assert not h.cancelled.is_set(), "B 在飞的动作被另一条连接的断开连坐取消了"
            h.gate.set()
            reply_b = spv2.decode(await asyncio.wait_for(conn_b.recv(), 3.0))
            assert reply_b.status == spv2.STATUS_OK, reply_b.error
            assert spv2.decode_body(reply_b.body) == {"slow": True}
            await conn_b.close()
        finally:
            server.close()
            await server.wait_closed()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_closing_native_connection_cancels_its_own_inflight() -> None:
    """连接断开 → 它自己在飞的动作被取消（这半边行为要有，不能因为修 bug 一起丢了）。"""

    async def scenario() -> None:
        h = Harness()
        server, url = await start_native_endpoint(h)
        try:
            conn = await websockets.connect(url)
            await send_native_call(conn, "slow")
            await asyncio.wait_for(h.started.wait(), 2.0)
            await conn.close()
            await asyncio.wait_for(h.cancelled.wait(), 2.0)
        finally:
            server.close()
            await server.wait_closed()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))
