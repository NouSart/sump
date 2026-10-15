"""Service：把「中心连接（注册 / 心跳 / 自愈）」与「WS 端点（jump 动作）」组装成一个服务。

动作表只有 `embed`：批量文本 → 向量（L2 归一化）。推理走线程池（onnxruntime 是同步
CPU 调用），慢动作不阻塞事件循环；并发推理安全（ONNX Runtime 会话与 tokenizers 均线程安全）。
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field

import websockets

from .backends import Backend
from .center import CenterLink
from .server import JumpEndpoint

DEFAULT_MAX_BATCH = 32
DEFAULT_ACTION_TIMEOUT = 60.0


@dataclass
class Config:
    name: str = "embedding"
    addr: str = "127.0.0.1:9601"
    center: str = "ws://127.0.0.1:9000/ws"
    heartbeat_ms: int = 15000
    max_batch: int = DEFAULT_MAX_BATCH
    action_timeout: float = DEFAULT_ACTION_TIMEOUT
    provides: list[dict] = field(
        default_factory=lambda: [
            {"action": "embed", "input": "{texts:[…], query?}", "output": "{vectors, dim, model}（L2 归一化）"},
        ]
    )
    description: str = "文本 → 向量（本地 ONNX 推理，bge-small-zh-v1.5 量级）：供记忆语义召回使用"


def split_addr(addr: str) -> tuple[str, int]:
    host, _, port = addr.rpartition(":")
    if not host or not port.isdigit():
        raise ValueError(f"非法监听地址: {addr}（应为 host:port）")
    return host, int(port)


class Service:
    def __init__(self, cfg: Config, backend: Backend, log: logging.Logger) -> None:
        self.cfg = cfg
        self.backend = backend
        self.log = log
        self.addr = cfg.addr  # 实际监听地址（绑定后含真实端口）
        self._server: websockets.Server | None = None
        self._watch: asyncio.Task | None = None
        self.endpoint = JumpEndpoint(
            name=cfg.name,
            actions={"embed": self.action_embed},
            action_timeout=cfg.action_timeout,
            log=log,
        )
        self.link: CenterLink | None = None

    async def start(self) -> None:
        host, port = split_addr(self.cfg.addr)
        self._server = await websockets.serve(self.endpoint.handle_conn, host, port, max_size=None)
        sockname = self._server.sockets[0].getsockname()
        self.addr = f"{sockname[0]}:{sockname[1]}"  # 支持 :0 随机端口（测试）
        self.log.info("%s 监听 ws://%s/ws", self.cfg.name, self.addr)
        self.log.info(
            "后端: %s（%s, dim=%d）",
            type(self.backend).__name__,
            self.backend.name,
            self.backend.dim,
        )
        self.link = CenterLink(
            name=self.cfg.name,
            center_url=self.cfg.center,
            self_url=f"ws://{self.addr}/ws",
            description=self.cfg.description,
            provides=self.cfg.provides,
            settings=[],  # 零设置项：只声明真正消费的
            heartbeat_ms=self.cfg.heartbeat_ms,
            log=self.log,
        )
        await self.link.start()  # 首次注册：失败即抛错（引导错误立刻可见）
        self._watch = asyncio.create_task(self.link.watch())

    async def stop(self) -> None:
        if self.link is not None:
            await self.link.stop()  # 尽力 bye（中心立即摘除名册）并关闭连接
        if self._watch is not None:
            self._watch.cancel()
            await asyncio.gather(self._watch, return_exceptions=True)
            self._watch = None
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None

    # ---------- 动作 ----------

    async def action_embed(self, input_: object) -> object:
        if not isinstance(input_, dict):
            raise ValueError("embed: 输入必须是对象 {texts:[…]}")
        texts = input_.get("texts")
        if not isinstance(texts, list) or not texts:
            raise ValueError("embed: texts 必填（非空字符串数组）")
        if any(not isinstance(t, str) for t in texts):
            raise ValueError("embed: texts 必须全为字符串")
        if len(texts) > self.cfg.max_batch:
            raise ValueError(f"embed: 批次超过上限 {self.cfg.max_batch}")
        query = bool(input_.get("query", False))
        vectors = await asyncio.to_thread(self.backend.embed, texts, query)
        return {"vectors": vectors, "dim": self.backend.dim, "model": self.backend.name}
