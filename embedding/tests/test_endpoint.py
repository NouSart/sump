"""WS 端点层测试：连接隔离——一条连接断开只取消**它自己**在飞的动作，不得连坐其它连接。

回归背景：任务集合曾是端点级（跨连接共享），于是 Go 侧短连接（拿到响应即关闭）会把此刻
别的连接在跑的 embed 一并取消——无辜的调用方只能等到超时，memory 静默回落 bigram。
"""

from __future__ import annotations

import asyncio
import logging

import websockets

from sump_embedding.protocol import decode, encode, new_envelope
from sump_embedding.server import JumpEndpoint

TIMEOUT = 15.0
log = logging.getLogger("endpoint-test")


class Harness:
    """动作替身：slow 停在闸门上等测试放行，并记录自己是否被取消。"""

    def __init__(self) -> None:
        self.gate = asyncio.Event()
        self.started = asyncio.Event()
        self.cancelled = asyncio.Event()

    async def slow(self, _input: object) -> object:
        self.started.set()
        try:
            await self.gate.wait()
        except asyncio.CancelledError:
            self.cancelled.set()
            raise
        return {"slow": True}

    async def work(self, _input: object) -> object:
        return {"work": True}


async def start_endpoint(h: Harness) -> tuple[websockets.Server, str]:
    ep = JumpEndpoint(name="probe", actions={"slow": h.slow, "work": h.work}, action_timeout=10.0, log=log)
    server = await websockets.serve(ep.handle_conn, "127.0.0.1", 0, max_size=None)
    port = server.sockets[0].getsockname()[1]
    return server, f"ws://127.0.0.1:{port}/ws"


async def send_jump(ws: websockets.ClientConnection, action: str) -> None:
    env = new_envelope("jump", "probe-client", "probe", "T-PROBE", {"action": action, "input": {}})
    await ws.send(encode(env))


def test_closing_one_connection_does_not_cancel_other_inflight() -> None:
    """A 收到回执后关闭连接（= Go 每次调用的实际行为）→ B 在飞的动作继续跑完并拿到响应。"""

    async def scenario() -> None:
        h = Harness()
        server, url = await start_endpoint(h)
        try:
            conn_b = await websockets.connect(url)
            await send_jump(conn_b, "slow")
            await asyncio.wait_for(h.started.wait(), 2.0)

            conn_a = await websockets.connect(url)
            await send_jump(conn_a, "work")
            rp = decode(await asyncio.wait_for(conn_a.recv(), 2.0))["payload"]
            assert rp["ok"] is True, rp
            await conn_a.close()
            await asyncio.sleep(0.2)

            assert not h.cancelled.is_set(), "B 在飞的动作被另一条连接的断开连坐取消了"
            h.gate.set()
            rp_b = decode(await asyncio.wait_for(conn_b.recv(), 3.0))["payload"]
            assert rp_b["ok"] is True and rp_b["data"] == {"slow": True}, rp_b
            await conn_b.close()
        finally:
            server.close()
            await server.wait_closed()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_closing_connection_cancels_its_own_inflight() -> None:
    """连接断开 → 它自己在飞的动作被取消（这半边行为要有，不能因为修 bug 一起丢了）。"""

    async def scenario() -> None:
        h = Harness()
        server, url = await start_endpoint(h)
        try:
            conn = await websockets.connect(url)
            await send_jump(conn, "slow")
            await asyncio.wait_for(h.started.wait(), 2.0)
            await conn.close()
            await asyncio.wait_for(h.cancelled.wait(), 2.0)
        finally:
            server.close()
            await server.wait_closed()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))
