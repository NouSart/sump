"""Mutation-driven edge cases for the Python embedding service."""

from __future__ import annotations

import asyncio
import logging

import pytest

from sump_embedding import spv2
from sump_embedding.backends import FakeBackend
from sump_embedding.center import CenterLink, center_spv2_url
from sump_embedding.service import Config, Service, split_addr

LOG = logging.getLogger("mutation-gaps")


def test_error_retryable_only_true_bool() -> None:
    assert spv2.Error.parse({"code": "x", "retryable": "true"}).retryable is False
    assert spv2.Error.parse({"code": "x", "retryable": 1}).retryable is False
    assert spv2.Error.parse({"code": "x", "retryable": True}).retryable is True


def test_error_details_empty_is_omitted() -> None:
    assert "details" not in spv2.Error(code="x", details={}).wire()
    assert spv2.Error(code="x", details={"a": 1}).wire()["details"] == {"a": 1}


def test_frame_wire_defaults_and_empty_body() -> None:
    frame = spv2.Frame(v="", type=spv2.TYPE_REPLY)
    assert frame.wire()["v"] == spv2.PROTOCOL_VERSION
    assert "body" not in frame.wire()
    assert spv2.encode_body(None) == b""


def test_ref_parse_dict_ok_and_non_dict_rejected() -> None:
    assert spv2.Ref.parse({}).id == ""
    with pytest.raises(spv2.FrameError):
        spv2.Ref.parse([])


def test_frame_body_must_be_bin() -> None:
    with pytest.raises(spv2.FrameError):
        spv2.Frame.parse({"body": []})
    assert spv2.Frame.parse({"body": b"x"}).body == b"x"


def test_roster_skips_non_dict_cards() -> None:
    link = CenterLink(
        name="embedding",
        center_url="ws://127.0.0.1:1/ws",
        self_url="ws://127.0.0.1:2/ws",
        description="x",
        provides=[],
        settings=[],
        heartbeat_ms=1000,
        log=LOG,
    )
    link._set_roster({"services": [None, {"name": "ok"}, {"id": "missing"}], "revision": 2})
    assert list(link.roster) == ["ok"]


def test_center_spv2_url_rules() -> None:
    assert center_spv2_url("ws://h:1/ws") == "ws://h:1/spv2"
    assert center_spv2_url("ws://h:1/spv2") == "ws://h:1/spv2"
    assert center_spv2_url("ws://h:1") == "ws://h:1/spv2"


def test_split_addr_rejects_bad_parts() -> None:
    assert split_addr("127.0.0.1:1234") == ("127.0.0.1", 1234)
    with pytest.raises(ValueError):
        split_addr(":1234")
    with pytest.raises(ValueError):
        split_addr("127.0.0.1:notaport")


def _endpoint(*, timeout: float = 1.0, methods: list[spv2.MethodSpec] | None = None) -> spv2.NativeEndpoint:
    return spv2.NativeEndpoint(
        name="embedding", methods=methods or [], action_timeout=timeout, log=LOG, version="test"
    )


async def _call(ep: spv2.NativeEndpoint, call: spv2.Frame) -> spv2.Frame:
    sent: list[spv2.Frame] = []

    async def send(frame: spv2.Frame) -> None:
        sent.append(frame)

    await ep._handle_call(send, call)
    assert len(sent) == 1
    return sent[0]


def test_endpoint_unknown_method_not_found() -> None:
    ep = _endpoint()

    async def run() -> None:
        call = spv2.new_call(
            work="w", from_=spv2.service_ref("caller"), to=spv2.service_ref("embedding"), method="nope"
        )
        reply = await _call(ep, call)
        assert reply.status == spv2.STATUS_ERROR
        assert reply.error is not None and reply.error.code == spv2.CODE_NOT_FOUND

    asyncio.run(run())


def test_endpoint_tool_false_in_card() -> None:
    async def noop(_body: bytes) -> object:
        return {}

    ep = _endpoint(methods=[spv2.MethodSpec(name="embed", handler=noop, tool=False)])
    method = next(m for m in ep.card["methods"] if m["name"] == "embed")
    assert method["tool"] is False


def test_endpoint_validation_rejects_bad_calls() -> None:
    ep = _endpoint()
    valid = spv2.new_call(
        work="w", from_=spv2.service_ref("caller"), to=spv2.service_ref("embedding"), method="embed"
    )

    async def run() -> None:
        for mutate in (
            lambda c: setattr(c, "v", "spv2.0"),
            lambda c: setattr(c, "id", ""),
            lambda c: setattr(c, "method", ""),
            lambda c: setattr(c.from_, "id", ""),
            lambda c: setattr(c, "to", spv2.Ref(kind="service", id="other")),
            lambda c: setattr(c, "content_type", "text/plain"),
        ):
            call = spv2.Frame(**{**valid.__dict__, "from_": spv2.Ref(**valid.from_.__dict__), "to": spv2.Ref(**valid.to.__dict__), "body": valid.body})
            mutate(call)
            reply = await _call(ep, call)
            assert reply.status == spv2.STATUS_ERROR
            assert reply.error is not None and reply.error.code == spv2.CODE_INVALID_ARGUMENT

    asyncio.run(run())


def test_endpoint_timeout_is_retryable() -> None:
    async def slow(_body: bytes) -> object:
        await asyncio.sleep(0.05)
        return {}

    ep = _endpoint(timeout=0.01, methods=[spv2.MethodSpec(name="slow", handler=slow)])

    async def run() -> None:
        call = spv2.new_call(
            work="w", from_=spv2.service_ref("caller"), to=spv2.service_ref("embedding"), method="slow"
        )
        reply = await _call(ep, call)
        assert reply.error is not None
        assert reply.error.code == spv2.CODE_DEADLINE_EXCEEDED
        assert reply.error.retryable is True

    asyncio.run(run())


def test_embed_exact_max_batch_and_query_flag() -> None:
    class RecordingBackend(FakeBackend):
        def __init__(self) -> None:
            super().__init__(dim=8)
            self.queries: list[bool] = []

        def embed(self, texts: list[str], query: bool = False) -> list[list[float]]:
            self.queries.append(query)
            return super().embed(texts, query)

    backend = RecordingBackend()
    svc = Service(Config(name="embedding", addr="127.0.0.1:0", max_batch=2), backend, LOG)

    async def run() -> None:
        await svc.action_embed({"texts": ["a", "b"], "query": True})
        assert backend.queries == [True]
        with pytest.raises(ValueError):
            await svc.action_embed({"texts": ["a", "b", "c"]})

    asyncio.run(run())


def test_request_spv2_rejects_non_reply_frame() -> None:
    link = CenterLink(
        name="embedding",
        center_url="ws://127.0.0.1:1/ws",
        self_url="ws://127.0.0.1:2/ws",
        description="x",
        provides=[],
        settings=[],
        heartbeat_ms=1000,
        log=LOG,
    )
    frame = spv2.new_call(
        work="w", from_=spv2.service_ref("embedding"), to=spv2.service_ref("settings-center"), method="register"
    )

    class FakeWS:
        async def send(self, _data: bytes) -> None:
            for fut in link._pending.values():
                fut.set_result(spv2.Frame(type=spv2.TYPE_EVENT))

    async def run() -> None:
        with pytest.raises(RuntimeError, match="期待 reply"):
            await link._request_spv2(FakeWS(), frame, 1.0)

    asyncio.run(run())


def test_service_routes_spv2_path_with_query() -> None:
    class FakeRequest:
        def __init__(self, path: str) -> None:
            self.path = path

    class FakeWS:
        def __init__(self, path: str) -> None:
            self.request = FakeRequest(path)

    svc = Service(Config(name="embedding", addr="127.0.0.1:0"), FakeBackend(), LOG)
    calls: list[str] = []

    async def native(_ws: object) -> None:
        calls.append("native")

    async def v1(_ws: object) -> None:
        calls.append("v1")

    svc.native.handle_conn = native  # type: ignore[method-assign]
    svc.endpoint.handle_conn = v1  # type: ignore[method-assign]

    async def run() -> None:
        await svc.handle_conn(FakeWS("/spv2?probe=1"))  # type: ignore[arg-type]
        await svc.handle_conn(FakeWS("/ws"))  # type: ignore[arg-type]

    asyncio.run(run())
    assert calls == ["native", "v1"]
