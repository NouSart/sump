"""到设置中心的连接（服务节点一侧）：注册 / 心跳 / 断线自愈（重连 + 重注册）/ 名册广播。

与 Go 侧 service/ 骨架同语义（本服务是 Python，等价实现其中该子集）：
- 首次注册同步完成；失败即抛错（引导配置错误要立刻可见——不像前端面板那样持续等待）；
- 读循环在后台任务里跑（connect_and_register 不阻塞在连接上）；
- watch() 负责自愈：会话结束 → 1s 快速重连；拨号/注册失败 → 指数退避（1s → 30s 上限）；
- 心跳写失败即断开连接（触发自愈）；关停前发一条 status="bye" 的心跳。
"""

from __future__ import annotations

import asyncio
import logging

import websockets

from .protocol import (
    TYPE_HEARTBEAT,
    TYPE_REGISTER,
    TYPE_RESPONSE,
    TYPE_ROSTER,
    TYPE_SETTINGS,
    decode,
    encode,
    new_envelope,
)

REGISTER_TIMEOUT = 10.0
MAX_BACKOFF = 30.0


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

        self.roster: dict[str, dict] = {}
        self.registered = False

        self._ws: websockets.ClientConnection | None = None
        self._session_task: asyncio.Task | None = None
        self._pending: dict[str, asyncio.Future] = {}
        self._closed = False

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
                env = new_envelope(TYPE_HEARTBEAT, self.name, "settings-center", "", {"status": "bye"})
                await ws.send(encode(env))
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

    # ---------- 内部 ----------

    async def _connect_and_register(self) -> None:
        """连接 + 注册；成功后在后台跑会话（读循环 + 心跳），本方法等待注册结果后返回。"""
        ws = await websockets.connect(self.center_url, max_size=None)
        self._ws = ws
        # 先起读循环：注册响应要由它按 id 派发——必须先于注册请求存在。
        self._session_task = asyncio.create_task(self._run_session(ws))
        try:
            env = new_envelope(
                TYPE_REGISTER,
                self.name,
                "settings-center",
                "",
                {
                    "name": self.name,
                    "addr": self.self_url,
                    "description": self.description,
                    "provides": self.provides,
                    "settings": self.settings,
                    "heartbeat_ms": self.heartbeat_ms,
                },
            )
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
            task = self._session_task
            self._session_task = None
            if task is not None:
                task.cancel()
                await asyncio.gather(task, return_exceptions=True)
            raise

    async def _run_session(self, ws: websockets.ClientConnection) -> None:
        """读循环 + 心跳，直到连接断开；负责最后的清理与在途请求失败。"""
        heartbeat = asyncio.create_task(self._heartbeat_loop(ws))
        try:
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
                env = new_envelope(TYPE_HEARTBEAT, self.name, "settings-center", "", {"status": "ok"})
                await ws.send(encode(env))
        except asyncio.CancelledError:
            raise
        except Exception as err:
            self.log.warning("心跳发送失败（断开以自愈）: %s", err)
            try:
                await ws.close()
            except Exception:
                pass

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
