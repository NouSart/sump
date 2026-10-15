"""服务端 WS 端点：接受任意服务的 jump 访问（对齐 Go 骨架语义）。

- 非 jump 消息：回执 `unsupported message type: …`，连接保持；
- jump：`{action, input}` 分派到动作表；未知名回 `unknown action: …`；
- 每个 jump 的处理并发进行（慢动作不阻塞读循环）；响应按请求 id 配对。
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable

import websockets

from .protocol import TYPE_JUMP, decode, encode, new_response

Action = Callable[[object], Awaitable[object]]


class JumpEndpoint:
    """一个 WS 端点：把 jump 分派给动作表，其余消息按协议回绝。"""

    def __init__(
        self,
        *,
        name: str,
        actions: dict[str, Action],
        action_timeout: float,
        log: logging.Logger,
    ) -> None:
        self.name = name
        self.actions = actions
        self.action_timeout = action_timeout
        self.log = log

    async def handle_conn(self, ws: websockets.ServerConnection) -> None:
        lock = asyncio.Lock()
        # 任务集合是**连接级**的：一条连接断开只取消它自己在飞的动作，不得连坐其它连接
        # （调用方各自短连接、各自等自己的响应；连坐会让无辜的调用方只能超时）。
        tasks: set[asyncio.Task] = set()

        async def send(env: dict) -> None:
            async with lock:  # websockets 允许并发 send，但显式串行化更省心
                await ws.send(encode(env))

        try:
            async for raw in ws:
                try:
                    env = decode(raw)
                except Exception as err:
                    self.log.warning("坏帧: %s", err)
                    continue
                if env["type"] != TYPE_JUMP:
                    await send(new_response(env, self.name, {"ok": False, "error": f"unsupported message type: {env['type']}"}))
                    continue
                task = asyncio.create_task(self._handle_jump(send, env))
                tasks.add(task)
                task.add_done_callback(tasks.discard)
        except websockets.ConnectionClosed:
            pass
        finally:
            for task in list(tasks):
                task.cancel()
            if tasks:
                await asyncio.gather(*tasks, return_exceptions=True)

    async def _handle_jump(self, send: Callable[[dict], Awaitable[None]], env: dict) -> None:
        payload = env["payload"]
        if not isinstance(payload, dict):
            await send(new_response(env, self.name, {"ok": False, "error": "bad jump payload"}))
            return
        action = payload.get("action")
        if not isinstance(action, str) or not action:
            await send(new_response(env, self.name, {"ok": False, "error": "bad jump payload"}))
            return
        handler = self.actions.get(action)
        if handler is None:
            await send(new_response(env, self.name, {"ok": False, "error": f"unknown action: {action}"}))
            return
        try:
            output = await asyncio.wait_for(handler(payload.get("input")), self.action_timeout)
        except asyncio.TimeoutError:
            await send(new_response(env, self.name, {"ok": False, "error": f"{action}: 处理超时"}))
            return
        except Exception as err:
            await send(new_response(env, self.name, {"ok": False, "error": str(err) or err.__class__.__name__}))
            return
        resp_payload: dict = {"ok": True}
        if output is not None:
            resp_payload["data"] = output
        await send(new_response(env, self.name, resp_payload))
