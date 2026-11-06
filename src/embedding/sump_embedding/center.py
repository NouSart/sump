"""到设置中心的连接（服务节点一侧）：注册 / 心跳 / 断线自愈（重连 + 重注册）/ 名册广播。

与 Go 侧 service/ 骨架同语义（本服务是 Python，等价实现其中该子集）：
- 首次注册同步完成；失败即抛错（引导配置错误要立刻可见——不像前端面板那样持续等待）；
- SUMP_SPV2=1 时优先走设置中心原生 /spv2：register 是 call、心跳是 event、roster / settings
  是中心下发的 event；原生接入失败（拨号失败 / 注册被拒 / 回复不合法）自动回退 v1；
- 读循环在后台任务里跑（connect_and_register 不阻塞在连接上）；
- watch() 负责自愈：会话结束 → 1s 快速重连；拨号/注册失败 → 指数退避（1s → 30s 上限）；
- 心跳写失败即断开连接（触发自愈）；关停前发一条 status="bye" 的心跳。
"""

from __future__ import annotations

import asyncio
import logging
import os

import websockets

from . import spv2
from .protocol import (
    TYPE_HEARTBEAT,
    TYPE_REGISTER,
    TYPE_RESPONSE,
    TYPE_ROSTER,
    TYPE_SETTINGS,
    decode,
    encode,
    new_envelope,
    new_id,
)

REGISTER_TIMEOUT = 10.0
MAX_BACKOFF = 30.0

# 传输方式：v1 是兼容边缘，spv2 是原生控制面（SUMP_SPV2=1 时优先）。
TRANSPORT_V1 = "v1"
TRANSPORT_SPV2 = "spv2"

CENTER_NAME = "settings-center"
ENV_SPV2 = "SUMP_SPV2"


def spv2_enabled() -> bool:
    """是否优先走原生 /spv2（环境开关；默认关，保持既有 v1 行为）。"""
    return os.environ.get(ENV_SPV2, "").strip() == "1"


def center_spv2_url(center_url: str) -> str:
    """v1 中心地址 → 原生 /spv2 地址（与 Go 侧 service.spv2Addr 同规则）。"""
    if center_url.endswith("/spv2"):
        return center_url
    if center_url.endswith("/ws"):
        return center_url[: -len("/ws")] + "/spv2"
    return center_url.rstrip("/") + "/spv2"


class CenterLink:
    def __init__(
        self,
        *,
        name: str,
        center_url: str,
        self_url: str,
        description: str,
        provides: list[dict],
        settings: list[dict],
        heartbeat_ms: int,
        log: logging.Logger,
    ) -> None:
        self.name = name
        self.center_url = center_url
        self.self_url = self_url
        self.description = description
        self.provides = provides
        self.settings = settings
        self.heartbeat_ms = heartbeat_ms
        self.log = log
        self.spv2_enabled = spv2_enabled()

        self.roster: dict[str, dict] = {}
        self.registered = False

        self._ws: websockets.ClientConnection | None = None
        self._session_task: asyncio.Task | None = None
        self._pending: dict[str, asyncio.Future] = {}
        self._closed = False
        self._mode: str | None = None
        self._actor = spv2.service_ref(name)
        self._center = spv2.service_ref(CENTER_NAME)

    # ---------- 对外 ----------

    async def start(self) -> None:
        """首次注册（同步等待注册结果）：失败即抛错，由调用方决定退出。"""
        await self._connect_and_register()

    async def watch(self) -> None:
        """断线自愈：会话结束/失败后重连并重新注册（成功过 → 1s 快连；失败 → 指数退避）。"""
        backoff = 1.0
        while not self._closed:
            task = self._session_task
            if task is None:
                try:
                    await self._connect_and_register()
                    backoff = 1.0
                    continue
                except asyncio.CancelledError:
                    raise
                except Exception as err:
                    self.log.warning("重连设置中心失败: %s（%.0fs 后重试）", err, backoff)
                    await asyncio.sleep(backoff)
                    backoff = min(backoff * 2, MAX_BACKOFF)
                    continue
            try:
                await task  # 等会话结束（读循环直到连接断开）
            except asyncio.CancelledError:
                raise
            except Exception as err:
                self.log.warning("中心会话异常结束: %s", err)
            self._session_task = None
            if self._closed:
                return
            self.log.info("与设置中心连接断开，1s 后重连")
            await asyncio.sleep(1.0)

    async def stop(self) -> None:
        """优雅下线：尽力发 bye（中心立即摘除名册）后关闭连接与会话。"""
        self._closed = True
        ws = self._ws
        if ws is not None:
            try:
                if self._mode == TRANSPORT_SPV2:
                    await ws.send(spv2.encode(self.heartbeat_event("bye")))
                else:
                    await ws.send(encode(self.heartbeat_envelope("bye")))
            except Exception:
                pass  # 尽力
        task = self._session_task
        if task is not None:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            self._session_task = None
        if ws is not None:
            try:
                await ws.close()
            except Exception:
                pass

    # ---------- 组包（线路键名即契约） ----------

    def register_payload(self) -> dict:
        """注册名片（RegisterPayload）：v1 的 payload 与原生 register 的 body 是同一份。"""
        return {
            "name": self.name,
            "addr": self.self_url,
            "title": "向量",
            "description": self.description,
            "provides": self.provides,
            "settings": self.settings,
            "heartbeat_ms": self.heartbeat_ms,
        }

    def register_envelope(self) -> dict:
        """注册这封消息；线路键名即契约（tests/test_protocol.py 对固化夹具做锚定）。"""
        return new_envelope(TYPE_REGISTER, self.name, CENTER_NAME, "", self.register_payload())

    def heartbeat_envelope(self, status: str) -> dict:
        """心跳这封消息；status 是线路键名，同上锚定。"""
        return new_envelope(TYPE_HEARTBEAT, self.name, CENTER_NAME, "", {"status": status})

    def register_frame(self) -> spv2.Frame:
        """原生注册帧：body 与 v1 register 的 payload 相同（对齐 Go 侧 connectAndRegisterSPv2）。"""
        return spv2.new_call(
            work=new_id(),
            from_=self._actor,
            to=self._center,
            method="register",
            body=self.register_payload(),
        )

    def heartbeat_event(self, status: str) -> spv2.Frame:
        """原生心跳事件；status 是线路键名（"ok" / "bye"）。"""
        return spv2.new_event(
            work=new_id(),
            from_=self._actor,
            to=self._center,
            method="heartbeat",
            body={"status": status},
        )

    # ---------- 连接 + 注册 ----------

    async def _connect_and_register(self) -> None:
        """连接 + 注册：SPv2 开关打开时优先原生 /spv2，原生接入失败回退 v1。"""
        if self.spv2_enabled:
            try:
                await self._connect_and_register_spv2()
                return
            except asyncio.CancelledError:
                raise
            except Exception as err:
                self.log.warning("SPv2 原生接入设置中心失败，回退 v1: %s", err)
        await self._connect_and_register_v1()

    async def _connect_and_register_spv2(self) -> None:
        """原生接入：register call 同步等回复；成功后在后台跑会话（读循环 + 心跳）。"""
        ws = await websockets.connect(center_spv2_url(self.center_url), max_size=None)
        self._mode = TRANSPORT_SPV2
        self._ws = ws
        # 先起读循环：register 的 reply 要由它按 reply_to 派发——必须先于注册请求存在。
        self._session_task = asyncio.create_task(self._run_session(ws))
        try:
            roster = await self._request_spv2(ws, self.register_frame(), REGISTER_TIMEOUT)
            if not isinstance(roster, dict):
                raise RuntimeError("SPv2 register 回复不是名册对象")
            self._set_roster(roster)
            self.registered = True
            self.log.info(
                "已接入设置中心（SPv2 原生，名册 %d 个服务, rev %s）",
                len(self.roster),
                roster.get("revision", "?"),
            )
        except Exception:
            await self._abort_session()
            raise

    async def _connect_and_register_v1(self) -> None:
        """v1 接入（兼容边缘）：连接 + 注册；成功后在后台跑会话（读循环 + 心跳）。"""
        ws = await websockets.connect(self.center_url, max_size=None)
        self._mode = TRANSPORT_V1
        self._ws = ws
        # 先起读循环：注册响应要由它按 id 派发——必须先于注册请求存在。
        self._session_task = asyncio.create_task(self._run_session(ws))
        try:
            env = self.register_envelope()
            resp = await self._request(ws, env, REGISTER_TIMEOUT)
            if not resp.get("ok"):
                raise RuntimeError(f"注册被拒: {resp.get('error', '')}")
            roster = resp.get("data")
            if isinstance(roster, dict):
                self._set_roster(roster)
            self.registered = True
            self.log.info(
                "已接入设置中心（名册 %d 个服务, rev %s）",
                len(self.roster),
                roster.get("revision") if isinstance(roster, dict) else "?",
            )
        except Exception:
            await self._abort_session()
            raise

    async def _abort_session(self) -> None:
        """注册失败时收尾当前会话：取消读循环（其 finally 负责关连接与清状态）。"""
        task = self._session_task
        self._session_task = None
        if task is not None:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)

    # ---------- 会话 ----------

    async def _run_session(self, ws: websockets.ClientConnection) -> None:
        """读循环 + 心跳，直到连接断开；负责最后的清理与在途请求失败。"""
        native = self._mode == TRANSPORT_SPV2
        heartbeat = asyncio.create_task(
            self._heartbeat_loop_spv2(ws) if native else self._heartbeat_loop(ws)
        )
        try:
            if native:
                await self._read_loop_spv2(ws)
            else:
                await self._read_loop(ws)
        finally:
            heartbeat.cancel()
            await asyncio.gather(heartbeat, return_exceptions=True)
            self.registered = False
            if self._ws is ws:
                self._ws = None
            self._reject_pending("与设置中心连接断开")
            try:
                await ws.close()
            except Exception:
                pass

    # ---------- v1 请求 / 读循环 / 心跳 ----------

    async def _request(self, ws: websockets.ClientConnection, env: dict, timeout: float) -> dict:
        fut: asyncio.Future = asyncio.get_running_loop().create_future()
        self._pending[str(env["id"])] = fut
        try:
            await ws.send(encode(env))
            payload = await asyncio.wait_for(fut, timeout)
            return payload if isinstance(payload, dict) else {}
        finally:
            self._pending.pop(str(env["id"]), None)

    async def _read_loop(self, ws: websockets.ClientConnection) -> None:
        async for raw in ws:
            try:
                env = decode(raw)
            except Exception as err:
                self.log.warning("坏帧: %s", err)
                continue
            if env["type"] == TYPE_RESPONSE:
                fut = self._pending.pop(env["id"], None)
                if fut is not None and not fut.done():
                    fut.set_result(env["payload"])
                continue
            if env["type"] == TYPE_ROSTER:
                payload = env["payload"]
                if isinstance(payload, dict):
                    self._set_roster(payload)
                continue
            if env["type"] == TYPE_SETTINGS:
                continue  # 已知事件：设置下发结果（本服务零设置项，静默忽略）
            self.log.warning("未知事件: %s", env["type"])

    async def _heartbeat_loop(self, ws: websockets.ClientConnection) -> None:
        try:
            while True:
                await asyncio.sleep(self.heartbeat_ms / 1000)
                env = self.heartbeat_envelope("ok")
                await ws.send(encode(env))
        except asyncio.CancelledError:
            raise
        except Exception as err:
            self.log.warning("心跳发送失败（断开以自愈）: %s", err)
            try:
                await ws.close()
            except Exception:
                pass

    # ---------- SPv2 请求 / 读循环 / 心跳 ----------

    async def _request_spv2(self, ws: websockets.ClientConnection, frame: spv2.Frame, timeout: float) -> object:
        """发一条原生 call 并等 reply；失败抛 RuntimeError（调用方据此回退 v1）。"""
        fut: asyncio.Future = asyncio.get_running_loop().create_future()
        self._pending[frame.id] = fut
        try:
            await ws.send(spv2.encode(frame))
            reply = await asyncio.wait_for(fut, timeout)
        finally:
            self._pending.pop(frame.id, None)
        if not isinstance(reply, spv2.Frame) or reply.type != spv2.TYPE_REPLY:
            raise RuntimeError("SPv2 register: 期待 reply 帧")
        if reply.status != spv2.STATUS_OK:
            err = reply.error
            detail = f"{err.code}: {err.message}" if err is not None else repr(reply.status)
            raise RuntimeError(f"SPv2 register 被拒: {detail}")
        if not reply.body:
            raise RuntimeError("SPv2 register 回复缺少 body")
        return spv2.decode_body(reply.body)

    async def _read_loop_spv2(self, ws: websockets.ClientConnection) -> None:
        async for raw in ws:
            try:
                frame = spv2.decode(raw)
            except spv2.FrameError as err:
                self.log.warning("SPv2 坏帧: %s", err.message)
                continue
            except Exception as err:
                self.log.warning("SPv2 坏帧: %s", err)
                continue
            if frame.type == spv2.TYPE_REPLY:
                fut = self._pending.pop(frame.reply_to, None)
                if fut is not None and not fut.done():
                    fut.set_result(frame)
                continue
            if frame.type == spv2.TYPE_EVENT:
                self._handle_spv2_event(frame)
                continue
            self.log.warning("SPv2 未知帧类型: %s", frame.type)

    def _handle_spv2_event(self, frame: spv2.Frame) -> None:
        """中心下发的原生事件：roster 更新名册；settings 已知但本服务零设置项。"""
        if frame.method == "roster":
            try:
                payload = spv2.decode_body(frame.body)
            except spv2.FrameError as err:
                self.log.warning("SPv2 roster 事件解析失败: %s", err.message)
                return
            if isinstance(payload, dict):
                self._set_roster(payload)
            return
        if frame.method == "settings":
            return  # 已知事件：设置下发结果（本服务零设置项，静默忽略）
        self.log.warning("SPv2 未知事件: %s", frame.method)

    async def _heartbeat_loop_spv2(self, ws: websockets.ClientConnection) -> None:
        try:
            while True:
                await asyncio.sleep(self.heartbeat_ms / 1000)
                await ws.send(spv2.encode(self.heartbeat_event("ok")))
        except asyncio.CancelledError:
            raise
        except Exception as err:
            self.log.warning("心跳发送失败（断开以自愈）: %s", err)
            try:
                await ws.close()
            except Exception:
                pass

    # ---------- 名册 / 在途 ----------

    def _set_roster(self, payload: dict) -> None:
        services = payload.get("services")
        roster: dict[str, dict] = {}
        if isinstance(services, list):
            for card in services:
                if isinstance(card, dict) and card.get("name"):
                    roster[str(card["name"])] = card
        self.roster = roster
        self.log.info("roster 更新: %d 个服务 (rev %s)", len(roster), payload.get("revision", "?"))

    def _reject_pending(self, reason: str) -> None:
        for fut in self._pending.values():
            if not fut.done():
                fut.set_exception(RuntimeError(reason))
        self._pending.clear()
