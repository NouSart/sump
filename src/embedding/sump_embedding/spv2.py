"""SPv2.1（SUMP Protocol v2.1）帧编解码与原生端点 /spv2。

线协议只有一个帧：header + 不透明 body。与 Go 侧 spv2/ 逐字段对齐：

- 帧是 MessagePack map，键名即线路键名：v / id / type / work / from / to / method /
  reply_to / content_type / status / error / body；
- body 是 bin（不透明字节；本服务的 body 内容是 MessagePack 编码的对象）；
- 空字段不出现（对齐 Go 的 omitempty；work 例外：Go 结构体无 omitempty，恒出现）。

原生端点只处理 type=call：method 命中时解 body 调动作实现，回 type=reply
（status=ok，body=msgpack(结果)）；未知 method 回 not_found；帧 / body 解析失败回
invalid_argument。metrics 未实现也不伪造：未知 method 一律 not_found。
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field

import msgspec
import websockets

from .protocol import new_id

# ---------- 常量（与 Go 侧 spv2/frame.go、errors.go 对齐） ----------

PROTOCOL_VERSION = "spv2.1"

TYPE_CALL = "call"
TYPE_REPLY = "reply"
TYPE_EVENT = "event"
TYPE_STREAM = "stream"

STATUS_OK = "ok"
STATUS_ERROR = "error"

CONTENT_TYPE_MSGPACK = "application/msgpack"

AUTHORITY_NONE = "none"

CODE_INVALID_ARGUMENT = "invalid_argument"
CODE_NOT_FOUND = "not_found"
CODE_DEADLINE_EXCEEDED = "deadline_exceeded"
CODE_INTERNAL = "internal"


class FrameError(Exception):
    """带 SPv2 错误码的解析 / 处理失败（端点据此回 error reply）。"""

    def __init__(self, code: str, message: str, retryable: bool = False) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.retryable = retryable


def _text(value: object) -> str:
    return value if isinstance(value, str) else ""


# ---------- 帧模型 ----------


@dataclass
class Ref:
    """通用引用：来源或目标。"""

    kind: str = ""
    id: str = ""
    authority: str = ""
    tenant: str = ""

    def wire(self) -> dict:
        out = {"kind": self.kind, "id": self.id}
        if self.authority:
            out["authority"] = self.authority
        if self.tenant:
            out["tenant"] = self.tenant
        return out

    @classmethod
    def parse(cls, raw: object) -> "Ref":
        if raw is None:
            return cls()
        if not isinstance(raw, dict):
            raise FrameError(CODE_INVALID_ARGUMENT, "from/to 必须是 {kind,id} 对象")
        return cls(
            kind=_text(raw.get("kind")),
            id=_text(raw.get("id")),
            authority=_text(raw.get("authority")),
            tenant=_text(raw.get("tenant")),
        )


def service_ref(name: str) -> Ref:
    """服务自身的引用（authority=none：Go 侧 service 只接受 none）。"""
    return Ref(kind="service", id=name, authority=AUTHORITY_NONE)


@dataclass
class Error:
    """结构化错误。"""

    code: str = ""
    message: str = ""
    retryable: bool = False
    details: dict | None = None

    def wire(self) -> dict:
        out = {"code": self.code, "message": self.message, "retryable": self.retryable}
        if self.details:
            out["details"] = self.details
        return out

    @classmethod
    def parse(cls, raw: object) -> "Error | None":
        if raw is None:
            return None
        if not isinstance(raw, dict):
            raise FrameError(CODE_INVALID_ARGUMENT, "error 必须是 {code,message,retryable} 对象")
        details = raw.get("details")
        return cls(
            code=_text(raw.get("code")),
            message=_text(raw.get("message")),
            retryable=raw.get("retryable") is True,
            details=details if isinstance(details, dict) else None,
        )


@dataclass
class Frame:
    """SPv2.1 的唯一线上单元。字段名与 Go 的 Frame 一一对应（from 用 from_ 避开关键字）。"""

    v: str = PROTOCOL_VERSION
    id: str = ""
    type: str = ""
    work: str = ""
    from_: Ref = field(default_factory=Ref)
    to: Ref = field(default_factory=Ref)
    method: str = ""
    reply_to: str = ""
    content_type: str = ""
    status: str = ""
    error: Error | None = None
    body: bytes = b""

    def wire(self) -> dict:
        """帧 → 线上 map（空字段不出现，对齐 Go 的 omitempty）。"""
        out: dict = {
            "v": self.v or PROTOCOL_VERSION,
            "id": self.id,
            "type": self.type,
            "work": self.work,
            "from": self.from_.wire(),
            "to": self.to.wire(),
        }
        if self.method:
            out["method"] = self.method
        if self.reply_to:
            out["reply_to"] = self.reply_to
        if self.content_type:
            out["content_type"] = self.content_type
        if self.status:
            out["status"] = self.status
        if self.error is not None:
            out["error"] = self.error.wire()
        if self.body:
            out["body"] = self.body
        return out

    @classmethod
    def parse(cls, raw: object) -> "Frame":
        if not isinstance(raw, dict):
            raise FrameError(CODE_INVALID_ARGUMENT, "帧不是 map")
        body = raw.get("body", b"")
        if body is None:
            body = b""
        if not isinstance(body, (bytes, bytearray, memoryview)):
            raise FrameError(CODE_INVALID_ARGUMENT, "body 必须是 bin（MessagePack bytes）")
        return cls(
            v=_text(raw.get("v")) or PROTOCOL_VERSION,
            id=_text(raw.get("id")),
            type=_text(raw.get("type")),
            work=_text(raw.get("work")),
            from_=Ref.parse(raw.get("from")),
            to=Ref.parse(raw.get("to")),
            method=_text(raw.get("method")),
            reply_to=_text(raw.get("reply_to")),
            content_type=_text(raw.get("content_type")),
            status=_text(raw.get("status")),
            error=Error.parse(raw.get("error")),
            body=bytes(body),
        )


# ---------- 编解码 ----------


def encode(frame: Frame) -> bytes:
    """帧 → MessagePack 字节（WS 二进制帧内容）。"""
    return msgspec.msgpack.encode(frame.wire())


def decode(data: bytes | bytearray | memoryview) -> Frame:
    """MessagePack 字节 → 帧；结构/类型不合法时抛 FrameError(invalid_argument)。"""
    try:
        raw = msgspec.msgpack.decode(bytes(data))
    except Exception as err:  # msgspec 的 DecodeError 可能随版本变化，这里按「坏帧」统一处理
        raise FrameError(CODE_INVALID_ARGUMENT, f"帧不是合法 MessagePack: {err}") from err
    return Frame.parse(raw)


def encode_body(value: object) -> bytes:
    """业务对象 → body 字节（None = 空 body）。"""
    if value is None:
        return b""
    return msgspec.msgpack.encode(value)


def decode_body(body: bytes) -> object:
    """body 字节 → 业务对象；空 body / 非 MessagePack 抛 FrameError(invalid_argument)。"""
    if not body:
        raise FrameError(CODE_INVALID_ARGUMENT, "body 为空：需要 MessagePack 编码的对象")
    try:
        return msgspec.msgpack.decode(body)
    except Exception as err:
        raise FrameError(CODE_INVALID_ARGUMENT, f"body 不是合法 MessagePack: {err}") from err


# ---------- 帧构造 ----------


def new_call(*, work: str, from_: Ref, to: Ref, method: str, body: object = None) -> Frame:
    return Frame(
        v=PROTOCOL_VERSION,
        id=new_id(),
        type=TYPE_CALL,
        work=work,
        from_=from_,
        to=to,
        method=method,
        content_type=CONTENT_TYPE_MSGPACK,
        body=encode_body(body),
    )


def new_reply(call: Frame, from_: Ref, body: object) -> Frame:
    """成功 reply：沿用 work / 调用方引用，reply_to 配对 call.id。"""
    return Frame(
        v=PROTOCOL_VERSION,
        id=new_id(),
        type=TYPE_REPLY,
        work=call.work,
        from_=from_,
        to=call.from_,
        reply_to=call.id,
        status=STATUS_OK,
        content_type=CONTENT_TYPE_MSGPACK,
        body=encode_body(body),
    )


def new_error_reply(
    call: Frame,
    from_: Ref,
    code: str,
    message: str,
    retryable: bool = False,
    details: dict | None = None,
) -> Frame:
    """失败 reply：不带 body，错误在 error 字段（对齐 Go NewErrorReply）。"""
    return Frame(
        v=PROTOCOL_VERSION,
        id=new_id(),
        type=TYPE_REPLY,
        work=call.work,
        from_=from_,
        to=call.from_,
        reply_to=call.id,
        status=STATUS_ERROR,
        content_type=CONTENT_TYPE_MSGPACK,
        error=Error(code=code, message=message, retryable=retryable, details=details),
    )


# ---------- 原生端点 ----------

NativeHandler = Callable[[bytes], Awaitable[object]]


@dataclass
class MethodSpec:
    """一个原生 method：handler 收到原始 body，返回业务对象（端点负责编码）。"""

    name: str
    handler: NativeHandler
    tool: bool = False
    description: str = ""
    input: dict = field(default_factory=dict)
    output: dict = field(default_factory=dict)


class NativeEndpoint:
    """原生 /spv2 端点：只处理 call，按 method 分派；断连只取消本连接在飞的调用。"""

    def __init__(
        self,
        *,
        name: str,
        methods: list[MethodSpec],
        action_timeout: float,
        log: logging.Logger,
        version: str = "",
        description: str = "",
    ) -> None:
        self.name = name
        self.actor = service_ref(name)
        self.action_timeout = action_timeout
        self.log = log
        self.version = version
        self.description = description
        self.started = time.monotonic()

        self._specs: dict[str, MethodSpec] = {}
        for spec in methods:
            self._specs[spec.name] = spec
        # hello / health 是 SPv2 骨架内置（Go 侧 spv2.NewServer 同样自动注册）；
        # metrics 不实现——不伪造指标，未知名一律 not_found。
        self._specs.setdefault(
            "hello", MethodSpec(name="hello", handler=self._hello, description="取服务卡（SPv2.1 握手）")
        )
        self._specs.setdefault(
            "health", MethodSpec(name="health", handler=self._health, description="健康检查（不伪造 metrics）")
        )
        self.card = self._build_card()

    # ---------- 连接生命周期 ----------

    async def handle_conn(self, ws: websockets.ServerConnection) -> None:
        lock = asyncio.Lock()
        tasks: set[asyncio.Task] = set()

        async def send(frame: Frame) -> None:
            async with lock:  # websockets 允许并发 send，串行化更省心
                await ws.send(encode(frame))

        try:
            async for raw in ws:
                try:
                    frame = decode(raw)
                except FrameError as err:
                    # 帧坏了，连 id/from 都取不到，无法构造 reply；丢弃并记日志。
                    self.log.warning("SPv2 坏帧（丢弃）: %s", err.message)
                    continue
                except Exception as err:
                    self.log.warning("SPv2 坏帧（丢弃）: %s", err)
                    continue
                if frame.type != TYPE_CALL:
                    self.log.debug("SPv2 忽略非 call 帧: %s", frame.type)
                    continue
                task = asyncio.create_task(self._handle_call(send, frame))
                tasks.add(task)
                task.add_done_callback(tasks.discard)
        except websockets.ConnectionClosed:
            pass
        finally:
            for task in list(tasks):
                task.cancel()
            if tasks:
                await asyncio.gather(*tasks, return_exceptions=True)

    async def _handle_call(self, send: Callable[[Frame], Awaitable[None]], call: Frame) -> None:
        try:
            self._validate_call(call)
            spec = self._specs.get(call.method)
            if spec is None:
                raise FrameError(CODE_NOT_FOUND, f"unknown method: {call.method}")
            result = await asyncio.wait_for(spec.handler(call.body), self.action_timeout)
        except FrameError as err:
            await send(new_error_reply(call, self.actor, err.code, err.message, err.retryable))
            return
        except asyncio.TimeoutError:
            await send(
                new_error_reply(call, self.actor, CODE_DEADLINE_EXCEEDED, f"{call.method}: 处理超时", True)
            )
            return
        except ValueError as err:  # 动作参数不合法：属于调用方问题
            await send(new_error_reply(call, self.actor, CODE_INVALID_ARGUMENT, str(err) or err.__class__.__name__))
            return
        except Exception as err:
            self.log.warning("%s 处理失败: %s", call.method, err)
            await send(new_error_reply(call, self.actor, CODE_INTERNAL, str(err) or err.__class__.__name__))
            return
        await send(new_reply(call, self.actor, result))

    def _validate_call(self, call: Frame) -> None:
        if call.v != PROTOCOL_VERSION:
            raise FrameError(CODE_INVALID_ARGUMENT, f"v 必须是 {PROTOCOL_VERSION}，收到 {call.v!r}")
        if not call.id:
            raise FrameError(CODE_INVALID_ARGUMENT, "call.id 必填")
        if not call.method:
            raise FrameError(CODE_INVALID_ARGUMENT, "call.method 必填")
        if not call.from_.id:
            raise FrameError(CODE_INVALID_ARGUMENT, "call.from 必填（{kind,id}）")
        if call.to.kind == "service" and call.to.id and call.to.id != self.name:
            raise FrameError(CODE_INVALID_ARGUMENT, f"消息目标 {call.to.id} 不是本服务 {self.name}")
        if call.content_type not in ("", CONTENT_TYPE_MSGPACK):
            raise FrameError(CODE_INVALID_ARGUMENT, f"不支持的 content_type: {call.content_type}")

    # ---------- 内置 method ----------

    async def _hello(self, body: bytes) -> object:
        if body:
            request = decode_body(body)
            if not isinstance(request, dict):
                raise FrameError(CODE_INVALID_ARGUMENT, "hello: body 必须是 {protocol, actor} 对象")
            protocol = request.get("protocol")
            if isinstance(protocol, str) and protocol and protocol != PROTOCOL_VERSION:
                raise FrameError(CODE_INVALID_ARGUMENT, f"protocol 不匹配: {protocol}")
        return {"card": self.card}

    async def _health(self, _body: bytes) -> object:
        return {
            "status": "ok",
            "version": self.version,
            "uptime_ms": int((time.monotonic() - self.started) * 1000),
        }

    def _build_card(self) -> dict:
        methods = []
        for spec in self._specs.values():
            methods.append(
                {
                    "name": spec.name,
                    "shape": "call",
                    "version": 1,
                    "tool": spec.tool,
                    "idempotent": False,
                    "streaming": False,
                    "long_running": False,
                    "timeout_ms": int(self.action_timeout * 1000),
                    "description": spec.description,
                    "input": spec.input or {"type": "object"},
                    "output": spec.output or {"type": "object"},
                }
            )
        return {
            "protocol": PROTOCOL_VERSION,
            "name": self.name,
            "actor": self.actor.wire(),
            "version": self.version,
            "description": self.description,
            "capabilities": ["msgpack"],
            "methods": methods,
            "limits": {"max_body": 0, "max_stream": 0},
            # health 真有；metrics 没实现，就如实写 false（不伪造）。
            "observability": {"health": True, "metrics": False},
        }
