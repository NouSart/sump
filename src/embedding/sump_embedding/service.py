"""Service：把「中心连接（注册 / 心跳 / 自愈）」与「原生 WS 端点（/spv2 call）」组装成一个服务。

动作表只有 `embed`：批量文本 → 向量（L2 归一化）。推理走线程池（onnxruntime 是同步
CPU 调用），慢动作不阻塞事件循环；并发推理安全（ONNX Runtime 会话与 tokenizers 均线程安全）。
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field

import websockets
from websockets.datastructures import Headers
from websockets.http11 import Response

from . import __version__
from .backends import Backend
from .center import CenterLink
from .spv2 import MethodSpec, NativeEndpoint, decode_body

DEFAULT_MAX_BATCH = 32
DEFAULT_ACTION_TIMEOUT = 60.0
NATIVE_PATH = "/spv2"


@dataclass
class Config:
    name: str = "embedding"
    addr: str = "127.0.0.1:9601"
    center: str = "ws://127.0.0.1:9000/spv2"
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
        # 最小指标集（spec §12.3 的 embed 域）：只记低基数计数，不编造 duration 分桶。
        self.metrics = {"embed_requests_total": 0, "embed_texts_total": 0, "embed_errors_total": 0}
        self.started_at = time.monotonic()
        self._server: websockets.Server | None = None
        self._watch: asyncio.Task | None = None
        # 原生 SPv2.1 端点：/spv2 的 call 直接解 body 调 action_embed，不合成任何信封。
        self.native = NativeEndpoint(
            name=cfg.name,
            methods=[
                MethodSpec(
                    name="embed",
                    handler=self.native_embed,
                    description="批量文本 → 向量（L2 归一化）",
                    input={
                        "type": "object",
                        "properties": {
                            "texts": {"type": "array", "items": {"type": "string"}},
                            "query": {"type": "boolean"},
                        },
                        "required": ["texts"],
                    },
                    output={
                        "type": "object",
                        "properties": {
                            "vectors": {
                                "type": "array",
                                "items": {"type": "array", "items": {"type": "number"}},
                            },
                            "dim": {"type": "integer"},
                            "model": {"type": "string"},
                        },
                        "required": ["vectors", "dim", "model"],
                    },
                ),
            ],
            action_timeout=cfg.action_timeout,
            log=log,
            version=__version__,
            description=cfg.description,
        )
        self.link: CenterLink | None = None

    async def start(self) -> None:
        host, port = split_addr(self.cfg.addr)
        self._server = await websockets.serve(
            self.handle_conn,
            host,
            port,
            max_size=None,
            process_request=self._process_request,
        )
        sockname = self._server.sockets[0].getsockname()
        self.addr = f"{sockname[0]}:{sockname[1]}"  # 支持 :0 随机端口（测试）
        self.log.info("%s 监听 ws://%s/spv2（原生 SPv2.1）", self.cfg.name, self.addr)
        self.log.info(
            "后端: %s（%s, dim=%d）",
            type(self.backend).__name__,
            self.backend.name,
            self.backend.dim,
        )
        self.link = CenterLink(
            name=self.cfg.name,
            center_url=self.cfg.center,
            self_url=f"ws://{self.addr}/spv2",
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

    # ---------- 传输 ----------

    def _process_request(self, _conn: object, request: object) -> Response | None:
        """HTTP 侧：/healthz /readyz /metrics 是本服务的观测面，/spv2 交给原生端点；
        其余路径（尤其 /ws）明确回 404，绝不落到原生端点。"""
        path = str(getattr(request, "path", "")).split("?", 1)[0]
        if path == NATIVE_PATH:
            return None
        if path == "/healthz":
            return Response(200, "OK", Headers([("Content-Type", "text/plain; charset=utf-8")]), b"ok\n")
        if path == "/readyz":
            ready = self.link is not None and self.link.registered
            body = b'{"status":"ok"}' if ready else b'{"status":"fail"}'
            return Response(
                200 if ready else 503,
                "OK" if ready else "Service Unavailable",
                Headers([("Content-Type", "application/json; charset=utf-8")]),
                body,
            )
        if path == "/metrics":
            return Response(
                200,
                "OK",
                Headers([("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")]),
                self._render_metrics().encode("utf-8"),
            )
        self.log.warning("拒绝非原生路径 %s（只接受 %s）", path or "/", NATIVE_PATH)
        return Response(404, "Not Found", Headers())

    def _render_metrics(self) -> str:
        """OpenMetrics 文本（含 # EOF）；指标名遵循 sump_<域>_<名>。"""
        lines = [
            "# HELP sump_embed_requests_total Embedding requests",
            "# TYPE sump_embed_requests_total counter",
            f"sump_embed_requests_total {self.metrics['embed_requests_total']}",
            "# HELP sump_embed_texts_total Embedded texts",
            "# TYPE sump_embed_texts_total counter",
            f"sump_embed_texts_total {self.metrics['embed_texts_total']}",
            "# HELP sump_embed_errors_total Embedding errors",
            "# TYPE sump_embed_errors_total counter",
            f"sump_embed_errors_total {self.metrics['embed_errors_total']}",
            "# HELP sump_uptime_seconds Process uptime",
            "# TYPE sump_uptime_seconds gauge",
            f"sump_uptime_seconds {time.monotonic() - self.started_at:.3f}",
            "# EOF",
        ]
        return "\n".join(lines) + "\n"

    async def handle_conn(self, ws: websockets.ServerConnection) -> None:
        """只接受原生 /spv2；其它路径（含 /ws）断开，不进入任何动作分派。"""
        request = getattr(ws, "request", None)
        path = getattr(request, "path", "") if request is not None else ""
        if path.split("?", 1)[0] != NATIVE_PATH:
            self.log.warning("关闭非原生路径连接: %s", path or "/")
            await ws.close(code=1008, reason=f"只接受 {NATIVE_PATH}")
            return
        await self.native.handle_conn(ws)

    # ---------- 动作 ----------

    async def native_embed(self, body: bytes) -> object:
        """原生 embed：body 是 msgpack 的 {texts, query?}；返回值由端点编码成 reply body。"""
        self.metrics["embed_requests_total"] += 1
        input_ = decode_body(body)
        try:
            out = await self.action_embed(input_)
        except Exception:
            self.metrics["embed_errors_total"] += 1
            raise
        texts = input_.get("texts") if isinstance(input_, dict) else None
        if isinstance(texts, list):
            self.metrics["embed_texts_total"] += len(texts)
        return out

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
