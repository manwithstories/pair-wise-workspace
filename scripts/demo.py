#!/usr/bin/env python3
"""验收演示：生成夹具并运行成功/慢消费者/损坏/续跑场景，打印 JSON 行。

用法：
    .venv/bin/python scripts/demo.py --generate-fixtures --dir .local-fixtures
    .venv/bin/python scripts/demo.py --dir .local-fixtures          # 跑全部场景
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys

# 允许从源码树直接运行（未安装时）
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from mediapipe import fixtures  # noqa: E402
from mediapipe.demux import Demuxer  # noqa: E402
from mediapipe.errors import MediaError  # noqa: E402

PYTHON = sys.executable
FRAMES = 300  # 约 600 包 / 约 1MB，贴合验收数量级


def emit(payload: dict) -> None:
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True), flush=True)


def generate_fixtures(directory: str) -> dict[str, str]:
    os.makedirs(directory, exist_ok=True)
    paths = {
        "sample_mp4": os.path.join(directory, "sample.mp4"),
        "sample_mov": os.path.join(directory, "sample.mov"),
        "truncated": os.path.join(directory, "truncated.mp4"),
        "corrupt_box": os.path.join(directory, "corrupt_box.mp4"),
        "corrupt_packet": os.path.join(directory, "corrupt_packet.mp4"),
        "missing_index": os.path.join(directory, "missing_index.mp4"),
    }
    fixtures.generate_sample(paths["sample_mp4"], frames=FRAMES)
    shutil.copyfile(paths["sample_mp4"], paths["sample_mov"])
    specs = {
        "truncated": fixtures.corrupt_truncated(
            paths["sample_mp4"], paths["truncated"]
        ),
        "corrupt_box": fixtures.corrupt_box(
            paths["sample_mp4"], paths["corrupt_box"]
        ),
        "corrupt_packet": fixtures.corrupt_packet(
            paths["sample_mp4"], paths["corrupt_packet"]
        ),
        "missing_index": fixtures.corrupt_missing_index(
            paths["sample_mp4"], paths["missing_index"]
        ),
    }
    for name, spec in specs.items():
        emit(
            {
                "event": "fixture_corruption",
                "name": name,
                "kind": spec.kind,
                "exact_offset": spec.exact_offset,
                "offset_range": [spec.offset_start, spec.offset_end],
                "note": spec.note,
            }
        )
    for name, path in paths.items():
        emit(
            {
                "event": "fixture",
                "name": name,
                "path": path,
                "bytes": os.path.getsize(path),
            }
        )
    return paths


def run_cli(args: list[str]) -> int:
    proc = subprocess.run(
        [PYTHON, "-m", "mediapipe.cli", *args], capture_output=True, text=True
    )
    for line in proc.stdout.splitlines():
        print(line, flush=True)
    if proc.stderr.strip():
        emit({"event": "stderr", "text": proc.stderr.strip()[:500]})
    return proc.returncode


def demo_scenarios(directory: str) -> None:
    sample = os.path.join(directory, "sample.mov")
    # 1) 成功 + 慢消费者 + 回读验证
    emit({"event": "scenario", "name": "success_slow_consumer"})
    code = run_cli(
        [
            "remux", "--input", sample,
            "--output", "/tmp/remux.mov",
            "--checkpoint", "/tmp/remux.ckpt",
            "--queue-bytes", "1048576",
            "--slow-consumer-ms", "5",
            "--verify",
        ]
    )
    emit({"event": "scenario_exit", "name": "success_slow_consumer", "exit_code": code})

    # 2) 截断损坏（期望退出码 3）
    emit({"event": "scenario", "name": "truncated"})
    code = run_cli(
        [
            "remux", "--input", os.path.join(directory, "truncated.mp4"),
            "--output", "/tmp/bad.mp4",
            "--checkpoint", "/tmp/bad.ckpt",
            "--verify",
        ]
    )
    emit({"event": "scenario_exit", "name": "truncated", "exit_code": code})

    # 3) 中断后续跑：用很小检查点间隔制造中断点，再重复同一成功命令
    emit({"event": "scenario", "name": "resume"})
    ckpt = "/tmp/resume.ckpt"
    out = "/tmp/resume.mov"
    if os.path.exists(ckpt):
        os.remove(ckpt)
    code1 = run_cli(
        [
            "remux", "--input", sample, "--output", out,
            "--checkpoint", ckpt, "--checkpoint-every", "25",
            "--queue-bytes", "1048576", "--verify",
        ]
    )
    code2 = run_cli(
        [
            "remux", "--input", sample, "--output", out,
            "--checkpoint", ckpt, "--checkpoint-every", "25",
            "--queue-bytes", "1048576", "--verify",
        ]
    )
    emit(
        {
            "event": "scenario_exit",
            "name": "resume",
            "first_exit_code": code1,
            "resume_exit_code": code2,
        }
    )


def main() -> int:
    parser = argparse.ArgumentParser(description="mediapipe 验收演示")
    parser.add_argument("--generate-fixtures", action="store_true")
    parser.add_argument("--dir", default=".local-fixtures")
    parser.add_argument("--scenarios", action="store_true",
                        help="生成后立即运行全部场景")
    args = parser.parse_args()
    if args.generate_fixtures:
        generate_fixtures(args.dir)
    if args.scenarios or not args.generate_fixtures:
        if not os.path.isdir(args.dir):
            generate_fixtures(args.dir)
        demo_scenarios(args.dir)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
