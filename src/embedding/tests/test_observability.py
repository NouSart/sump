"""HTTP 观测面（spec §12）：/healthz /readyz /metrics 必须真实存在，指标不伪造。"""

from __future__ import annotations

import asyncio
import urllib.error
import urllib.request

from test_service import StubCenter, TIMEOUT, call, make_service


def _http_get(addr: str, path: str) -> tuple[int, bytes, str]:
    url = f"http://{addr}{path}"
    try:
        with urllib.request.urlopen(url, timeout=5) as resp:
            return resp.status, resp.read(), resp.headers.get("Content-Type", "")
    except urllib.error.HTTPError as err:
        return err.code, err.read(), err.headers.get("Content-Type", "")


def test_http_observability_endpoints() -> None:
    async def scenario() -> None:
        center = StubCenter()
        url = await center.start()
        svc = make_service(url)
        await svc.start()
        try:
            status, body, ctype = await asyncio.to_thread(_http_get, svc.addr, "/healthz")
            assert status == 200 and body == b"ok\n", (status, body)
            assert "text/plain" in ctype

            status, body, _ = await asyncio.to_thread(_http_get, svc.addr, "/readyz")
            assert status == 200 and b'"status":"ok"' in body, (status, body)

            # 打一次 embed，指标必须反映真实次数（不是常数 0）。
            reply = await call(svc.addr, "embed", {"texts": ["苹果", "香蕉"]})
            assert reply.status == "ok"
            status, body, ctype = await asyncio.to_thread(_http_get, svc.addr, "/metrics")
            assert status == 200 and "openmetrics" in ctype, (status, ctype)
            text = body.decode()
            assert "# EOF" in text
            assert "sump_embed_requests_total 1" in text
            assert "sump_embed_texts_total 2" in text

            # 非观测路径仍然是 404，不能误落原生端点。
            status, _, _ = await asyncio.to_thread(_http_get, svc.addr, "/ws")
            assert status == 404
        finally:
            await svc.stop()
        await center.stop()

    asyncio.run(asyncio.wait_for(scenario(), TIMEOUT))