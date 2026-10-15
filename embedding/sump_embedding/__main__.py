"""embedding 服务入口：python -m sump_embedding [flags]（Go 风格单横线参数）。

用法（与其余服务一致的引导配置）：
    python -m sump_embedding -addr 127.0.0.1:9601 -center ws://127.0.0.1:9000/ws
                            -backend onnx -model-dir data/models/bge-small-zh-v1.5
    python -m sump_embedding -backend fake        # 测试/CI：确定性伪向量，无模型依赖
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import signal
import sys

from .backends import BGE_QUERY_PREFIX, FakeBackend, OnnxBackend
from .service import DEFAULT_MAX_BATCH, Config, Service

# query 侧指令前缀的默认值由实测裁决（scripts/measure_query_prefix.py；数字见 README）：
# v1.5 模型上 12 条短查询的 A/B：no-prefix MRR 1.000 / prefix 0.958（prefix 失手 1 条）——默认不加。
DEFAULT_QUERY_PREFIX = ""


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(prog="sump-embedding", description="SUMP v2 embedding 服务（文本 → 向量）")
    parser.add_argument("-name", "--name", default="embedding", help="服务名（注册用）")
    parser.add_argument("-addr", "--addr", default="127.0.0.1:9601", help="监听地址（host:port）")
    parser.add_argument("-center", "--center", default="ws://127.0.0.1:9000/ws", help="设置中心 WS 地址")
    parser.add_argument("-backend", "--backend", choices=["onnx", "fake"], default="onnx", help="向量后端")
    parser.add_argument("-model-dir", "--model-dir", default="data/models/bge-small-zh-v1.5", help="模型目录")
    parser.add_argument("-model-file", "--model-file", default="", help="onnx 文件名（默认自动探测）")
    parser.add_argument(
        "-query-prefix",
        "--query-prefix",
        default=DEFAULT_QUERY_PREFIX,
        help="query 侧指令前缀（默认空：实测优于官方指令，见 README）",
    )
    parser.add_argument("-heartbeat-ms", "--heartbeat-ms", type=int, default=15000, help="心跳间隔（毫秒）")
    parser.add_argument("-max-batch", "--max-batch", type=int, default=DEFAULT_MAX_BATCH, help="单次 embed 文本数上限")
    parser.add_argument("-fake-dim", "--fake-dim", type=int, default=384, help="fake 后端维度（对齐真模型，便于联调）")
    return parser.parse_args(argv)


def build_backend(args: argparse.Namespace) -> object:
    if args.backend == "fake":
        return FakeBackend(dim=args.fake_dim)
    return OnnxBackend(
        model_dir=args.model_dir,
        model_file=args.model_file,
        query_prefix=args.query_prefix,
    )


async def run(args: argparse.Namespace, log: logging.Logger) -> None:
    backend = build_backend(args)
    service = Service(
        Config(
            name=args.name,
            addr=args.addr,
            center=args.center,
            heartbeat_ms=args.heartbeat_ms,
            max_batch=args.max_batch,
        ),
        backend,
        log,
    )
    await service.start()
    if args.query_prefix:
        log.info("query 侧指令前缀: 已启用")

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()

    def on_signal(_signum: int, _frame: object) -> None:
        loop.call_soon_threadsafe(stop.set)

    for sig in (signal.SIGINT, getattr(signal, "SIGTERM", None)):
        if sig is not None:
            try:
                signal.signal(sig, on_signal)
            except ValueError:
                pass  # 非主线程（测试中不会走到这）

    await stop.wait()
    log.info("退出")
    await service.stop()


def main(argv: list[str] | None = None) -> int:
    args = parse_args(list(argv if argv is not None else sys.argv[1:]))
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [embedding] %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )
    log = logging.getLogger("embedding")
    # websockets 的连接生命周期日志太吵：只保留警告及以上。
    logging.getLogger("websockets").setLevel(logging.WARNING)
    try:
        asyncio.run(run(args, log))
    except KeyboardInterrupt:
        pass
    except Exception as err:
        log.error("启动失败: %s", err)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
