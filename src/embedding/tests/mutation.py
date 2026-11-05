from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
CENTER = ["tests/test_center_spv2.py"]
SPV2 = ["tests/test_spv2.py", "tests/test_endpoint.py"]
SERVICE = ["tests/test_service.py", "tests/test_spv2.py", "tests/test_endpoint.py"]
GAP = ["tests/test_mutation_gaps.py"]
MUTANTS = [
    {"id": "PY01", "file": "center.py", "old": 'return os.environ.get(ENV_SPV2, "").strip() == "1"', "new": 'return os.environ.get(ENV_SPV2, "").strip() != "1"', "tests": CENTER},
    {"id": "PY02", "file": "center.py", "old": 'if center_url.endswith("/spv2"):', "new": 'if not center_url.endswith("/spv2"):', "tests": CENTER},
    {"id": "PY03", "file": "center.py", "old": 'if center_url.endswith("/ws"):', "new": 'if not center_url.endswith("/ws"):', "tests": CENTER},
    {"id": "PY04", "file": "center.py", "old": 'return center_url.rstrip("/") + "/spv2"', "new": 'return center_url + "/spv2"', "tests": CENTER},
    {"id": "PY05", "file": "center.py", "old": "if self.spv2_enabled:", "new": "if not self.spv2_enabled:", "tests": CENTER},
    {"id": "PY06", "file": "center.py", "old": "if not isinstance(roster, dict):", "new": "if isinstance(roster, dict):", "tests": CENTER},
    {"id": "PY07", "file": "center.py", "old": 'if not resp.get("ok"):', "new": 'if resp.get("ok"):', "tests": CENTER},
    {"id": "PY08", "file": "center.py", "old": "if isinstance(roster, dict):", "new": "if not isinstance(roster, dict):", "tests": CENTER},
    {"id": "PY09", "file": "center.py", "old": "native = self._mode == TRANSPORT_SPV2", "new": "native = self._mode != TRANSPORT_SPV2", "tests": CENTER},
    {"id": "PY10", "file": "center.py", "old": "if not isinstance(reply, spv2.Frame) or reply.type != spv2.TYPE_REPLY:", "new": "if not isinstance(reply, spv2.Frame) and reply.type != spv2.TYPE_REPLY:", "tests": CENTER},
    {"id": "PY11", "file": "center.py", "old": "if reply.status != spv2.STATUS_OK:", "new": "if reply.status == spv2.STATUS_OK:", "tests": CENTER},
    {"id": "PY12", "file": "center.py", "old": "if not reply.body:", "new": "if reply.body:", "tests": CENTER},
    {"id": "PY13", "file": "center.py", "old": 'if frame.method == "roster":', "new": 'if frame.method != "roster":', "tests": CENTER},
    {"id": "PY14", "file": "center.py", "old": "if isinstance(services, list):", "new": "if not isinstance(services, list):", "tests": CENTER},
    {"id": "PY15", "file": "center.py", "old": 'if isinstance(card, dict) and card.get("name"):', "new": 'if isinstance(card, dict) or card.get("name"):', "tests": CENTER},
    {"id": "PY16", "file": "spv2.py", "old": "if self.authority:", "new": "if not self.authority:", "tests": SPV2},
    {"id": "PY17", "file": "spv2.py", "old": 'if raw is None:\n            return cls()\n        if not isinstance(raw, dict):\n            raise FrameError(CODE_INVALID_ARGUMENT, "from/to 必须是 {kind,id} 对象")', "new": 'if raw is not None:\n            return cls()\n        if not isinstance(raw, dict):\n            raise FrameError(CODE_INVALID_ARGUMENT, "from/to 必须是 {kind,id} 对象")', "tests": SPV2},
    {"id": "PY18", "file": "spv2.py", "old": 'if not isinstance(raw, dict):\n            raise FrameError(CODE_INVALID_ARGUMENT, "from/to 必须是 {kind,id} 对象")', "new": 'if isinstance(raw, dict):\n            raise FrameError(CODE_INVALID_ARGUMENT, "from/to 必须是 {kind,id} 对象")', "tests": SPV2},
    {"id": "PY19", "file": "spv2.py", "old": "retryable=raw.get(\"retryable\") is True,", "new": "retryable=raw.get(\"retryable\") is not True,", "tests": SPV2},
    {"id": "PY20", "file": "spv2.py", "old": "if self.details:", "new": "if not self.details:", "tests": SPV2},
    {"id": "PY21", "file": "spv2.py", "old": '"v": self.v or PROTOCOL_VERSION,', "new": '"v": self.v and PROTOCOL_VERSION,', "tests": SPV2},
    {"id": "PY22", "file": "spv2.py", "old": "if self.body:", "new": "if not self.body:", "tests": SPV2},
    {"id": "PY23", "file": "spv2.py", "old": "if body is None:", "new": "if body is not None:", "tests": SPV2},
    {"id": "PY24", "file": "spv2.py", "old": "if not isinstance(body, (bytes, bytearray, memoryview)):", "new": "if isinstance(body, (bytes, bytearray, memoryview)):", "tests": SPV2},
    {"id": "PY25", "file": "spv2.py", "old": "if not body:", "new": "if body:", "tests": SPV2},
    {"id": "PY26", "file": "spv2.py", "old": "status=STATUS_OK,", "new": "status=STATUS_ERROR,", "tests": SPV2},
    {"id": "PY27", "file": "spv2.py", "old": "status=STATUS_ERROR,", "new": "status=STATUS_OK,", "tests": SPV2},
    {"id": "PY28", "file": "spv2.py", "old": "if spec is None:", "new": "if spec is not None:", "tests": SPV2},
    {"id": "PY29", "file": "spv2.py", "old": 'CODE_DEADLINE_EXCEEDED, f"{call.method}: 处理超时", True', "new": 'CODE_DEADLINE_EXCEEDED, f"{call.method}: 处理超时", False', "tests": SPV2},
    {"id": "PY30", "file": "spv2.py", "old": "if call.v != PROTOCOL_VERSION:", "new": "if call.v == PROTOCOL_VERSION:", "tests": SPV2},
    {"id": "PY31", "file": "spv2.py", "old": "if not call.id:", "new": "if call.id:", "tests": SPV2},
    {"id": "PY32", "file": "spv2.py", "old": 'if call.to.kind == "service" and call.to.id and call.to.id != self.name:', "new": 'if call.to.kind == "service" or call.to.id or call.to.id != self.name:', "tests": SPV2},
    {"id": "PY33", "file": "spv2.py", "old": 'if call.content_type not in ("", CONTENT_TYPE_MSGPACK):', "new": 'if call.content_type in ("", CONTENT_TYPE_MSGPACK):', "tests": SPV2},
    {"id": "PY34", "file": "spv2.py", "old": "if isinstance(protocol, str) and protocol and protocol != PROTOCOL_VERSION:", "new": "if isinstance(protocol, str) or protocol or protocol != PROTOCOL_VERSION:", "tests": SPV2},
    {"id": "PY35", "file": "spv2.py", "old": '"tool": spec.tool,', "new": '"tool": True,', "tests": SPV2},
    {"id": "PY36", "file": "service.py", "old": "if not host or not port.isdigit():", "new": "if not host and not port.isdigit():", "tests": SERVICE},
    {"id": "PY37", "file": "service.py", "old": 'if path.split("?", 1)[0] == "/spv2":', "new": 'if path.split("?", 1)[0] != "/spv2":', "tests": SERVICE},
    {"id": "PY38", "file": "service.py", "old": 'if path.split("?", 1)[0] == "/spv2":', "new": 'if path == "/spv2":', "tests": SERVICE},
    {"id": "PY39", "file": "service.py", "old": "if not isinstance(input_, dict):", "new": "if isinstance(input_, dict):", "tests": SERVICE},
    {"id": "PY40", "file": "service.py", "old": "if not isinstance(texts, list) or not texts:", "new": "if not isinstance(texts, list) and not texts:", "tests": SERVICE},
    {"id": "PY41", "file": "service.py", "old": "if any(not isinstance(t, str) for t in texts):", "new": "if any(isinstance(t, str) for t in texts):", "tests": SERVICE},
    {"id": "PY42", "file": "service.py", "old": "if len(texts) > self.cfg.max_batch:", "new": "if len(texts) >= self.cfg.max_batch:", "tests": SERVICE},
    {"id": "PY43", "file": "service.py", "old": 'query = bool(input_.get("query", False))', "new": 'query = not bool(input_.get("query", False))', "tests": SERVICE},
]


def run_tests(cwd: Path, tests: list[str], timeout: float) -> tuple[str, str]:
    env = os.environ.copy()
    env["PYTHONPATH"] = str(cwd) + os.pathsep + env.get("PYTHONPATH", "")
    env.pop("SUMP_SPV2", None)
    try:
        proc = subprocess.run(
            [sys.executable, "-m", "pytest", "-q", *tests],
            cwd=cwd,
            env=env,
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
        return ("lived" if proc.returncode == 0 else "killed"), (proc.stdout + proc.stderr)[-16000:]
    except subprocess.TimeoutExpired as err:
        output = (err.stdout or "") + (err.stderr or "")
        if isinstance(output, bytes):
            output = output.decode(errors="replace")
        return "timeout", output[-16000:]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--only", help="逗号分隔的 mutant id")
    parser.add_argument("--timeout", type=float, default=10.0)
    parser.add_argument("--output")
    parser.add_argument("--verbose", action="store_true")
    args = parser.parse_args()
    only = args.only.split(",") if args.only else None
    selected = [m for m in MUTANTS if only is None or m["id"] in only]
    if not selected or (only is not None and len(selected) != len(only)):
        raise SystemExit("未知 mutant id")

    results = []
    with tempfile.TemporaryDirectory(prefix="sump-embedding-mutation-") as temp_name:
        temp = Path(temp_name)
        shutil.copytree(ROOT / "sump_embedding", temp / "sump_embedding", ignore=shutil.ignore_patterns("__pycache__"))
        shutil.copytree(ROOT / "tests", temp / "tests", ignore=shutil.ignore_patterns("__pycache__"))
        shutil.copy2(ROOT / "pyproject.toml", temp / "pyproject.toml")
        baseline_tests = list(dict.fromkeys(list(GAP) + [test for mutant in selected for test in mutant["tests"]]))
        status, log = run_tests(temp, baseline_tests, args.timeout)
        if status != "lived":
            print(log, file=sys.stderr)
            raise SystemExit("未变异基线未通过，mutation 结果无效")
        print(f"baseline: pass ({', '.join(baseline_tests)})")
        for mutant in selected:
            path = temp / "sump_embedding" / mutant["file"]
            original = path.read_text(encoding="utf-8")
            if original.count(mutant["old"]) != 1:
                raise SystemExit(f"{mutant['id']}: 变异锚点不唯一或不存在")
            try:
                path.write_text(original.replace(mutant["old"], mutant["new"]), encoding="utf-8")
                status, log = run_tests(temp, list(GAP) + list(mutant["tests"]), args.timeout)
                results.append({**{k: mutant[k] for k in ("id", "file", "old", "new")}, "status": status})
                print(f"{mutant['id']} {status} {mutant['file']}", flush=True)
                if status == "lived" and args.verbose:
                    print(log)
            finally:
                path.write_text(original, encoding="utf-8")

    summary = {
        "total": len(results),
        "killed": sum(r["status"] == "killed" for r in results),
        "lived": sum(r["status"] == "lived" for r in results),
        "timeout": sum(r["status"] == "timeout" for r in results),
    }
    print(f"summary: {json.dumps(summary)}")
    if args.output:
        Path(args.output).write_text(json.dumps({"summary": summary, "results": results}, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return 1 if len(results) != len(selected) or summary["lived"] + summary["timeout"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
