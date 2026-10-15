"""服务集成测试：真 WS + 假中心——注册 / 名册 / 心跳 / 断线自愈 / bye / embed 全路径与错误路径。

不依赖模型与网络：后端用 FakeBackend（确定性）；假中心是进程内 websockets 服务端。
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Callable

import pytest
import websockets

from sump_embedding.backends import FakeBackend
from sump_embedding.protocol import (
    TYPE_HEARTBEAT,
    TYPE_REGISTER,
    TYPE_ROSTER,
    TYPE_SETTINGS,
    decode,
    encode,
    new_envelope,
    new_response,
)
from sump_embedding.service import Config, Service

TIMEOUT = 15.0
log = logging.getLogger("embedding-test")


# ---------- 假中心 ----------


class StubCenter:
    def __init__(self) -> None:
        self.registrations: list[dict] = []
        self.heartbeats: list[str] = []
        self.conns: set = set()
        self.roster: dict = {
            "services": [{"name": "settings-center", "addr": "ws://127.0.0.1:1/ws"}],
            "revision": 1,
        }
        self._server: websockets.Server | None = None
        self.url = ""

    async def start(self) -> str:
        self._server = await websockets.serve(self._handler, "127.0.0.1", 0, max_size=None)
        port = self._server.sockets[0].getsockname()[1]
        self.url = f"ws://127.0.0.1:{port}/ws"
        return self.url

    async def _handler(self, ws: websockets.ServerConnection) -> None:
        self.conns.add(ws)
        try:
            async for raw in ws:
                env = decode(raw)
                if env["type"] == TYPE_REGISTER:
                    self.registrations.append(env["payload"])
                    await ws.send(encode(new_response(env, "settings-center", {"ok": True, "data": self.roster})))
                elif env["type"] == TYPE_HEARTBEAT:
                    payload = env["payload"] if isinstance(env["payload"], dict) else {}
                    self.heartbeats.append(str(payload.get("status", "ok")))
        finally:
            self.conns.discard(ws)

    async def drop_all(self) -> None:
        for ws in list(self.conns):
            await ws.close()

    async def broadcast(self, typ: str, payload: object) -> None:
        for ws in list(self.conns):
            await ws.send(encode(new_envelope(typ, "settings-center", "", "", payload)))

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


async def jump(addr: str, action: str | None, input_: object = None, typ: str = "jump") -> dict:
    """以第三方身份对服务发起一跳（或任意类型消息），返回响应 payload。"""
    async with websockets.connect(f"ws://{addr}/ws", max_size=None) as ws:
        payload = None if action is None else {"action": action, "input": input_}
        env = new_envelope(typ, "probe", "embedding", "T-PROBE", payload)
        await ws.send(encode(env))
        raw = await asyncio.wait_for(ws.recv(), TIMEOUT)
        return decode(raw)["payload"]


def dot(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b))


# ---------- 用例 ----------


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
            assert reg["addr"] == f"ws://{svc.addr}/ws"
            assert reg["heartbeat_ms"] == 100
            assert reg["settings"] == []
            assert reg["provides"][0]["action"] == "embed"
            # 名册（注册响应带回）已到账
            assert "settings-center" in svc.link.roster
            # 心跳按间隔发出
            await wait_until(lambda: len(center.heartbeats) >= 2, 5, "心跳 ≥2")

            # embed：批量 + 归一化 + 模型标识
            rp = await jump(svc.addr, "embed", {"texts": ["苹果", "爱吃苹果"]})
            assert rp["ok"] is True, rp
            data = rp["data"]
            assert data["dim"] == 32 and data["model"] == "fake-bigram-v1"
            assert len(data["vectors"]) == 2
            assert abs(sum(x * x for x in data["vectors"][0]) ** 0.5 - 1.0) < 1e-9
            # query 标志可用且不影响成功路径
            rp2 = await jump(svc.addr, "embed", {"texts": ["苹果"], "query": True})
            assert rp2["ok"] is True
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
            # 未知动作
            rp = await jump(svc.addr, "no_such_action")
            assert rp["ok"] is False and "unknown action" in rp["error"]
            # 非 jump 类型：回执 unsupported，连接保持（再来一跳仍可用）
            rp = await jump(svc.addr, None, typ="mystery_type")
            assert rp["ok"] is False and "unsupported message type: mystery_type" in rp["error"]
            rp = await jump(svc.addr, "embed", {"texts": ["苹果"]})
            assert rp["ok"] is True
            # jump 载荷缺 action
            rp = await jump(svc.addr, "")
            assert rp["ok"] is False and rp["error"] == "bad jump payload"
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
                (None, "输入必须是对象"),
                ({"texts": []}, "texts 必填"),
                ({"texts": "苹果"}, "texts 必填"),
                ({"texts": ["ok", 3]}, "必须全为字符串"),
                ({"texts": ["a", "b", "c", "d"]}, "批次超过上限 3"),
            ]
            for input_, want in cases:
                rp = await jump(svc.addr, "embed", input_)
                assert rp["ok"] is False, (input_, rp)
                assert want in rp["error"], (input_, rp)
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))


def test_bootstrap_failure_when_center_unreachable() -> None:
    async def scenario() -> None:
        svc = make_service("ws://127.0.0.1:1/ws", heartbeat_ms=100)
        with pytest.raises(Exception):
            await svc.start()

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
            rp = await jump(svc.addr, "embed", {"texts": ["苹果"]})
            assert rp["ok"] is True
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
            await center.broadcast(TYPE_SETTINGS, {"cause": "set_setting", "service": "x", "results": []})
            # roster 广播：名册更新
            await center.broadcast(
                TYPE_ROSTER,
                {
                    "services": [
                        {"name": "settings-center", "addr": "ws://127.0.0.1:1/ws"},
                        {"name": "memory", "addr": "ws://127.0.0.1:9201/ws"},
                    ],
                    "revision": 2,
                },
            )
            await wait_until(lambda: "memory" in svc.link.roster, 5, "名册更新")
            rp = await jump(svc.addr, "embed", {"texts": ["苹果"]})
            assert rp["ok"] is True
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))
