"""命令行：demo / verify。退出码 0/2/3/4 见 errors.py。"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .errors import (
    EXIT_OK,
    CheckpointError,
    RelayError,
)
from .manifest import verify_manifest
from .pipeline import PipelineConfig, PipelineInterrupted, run_pipeline
from .simulate import FAULT_CASES
from .timebase import TimeBase
from .timeline import Clip, Timeline

DEMO_CASES = sorted(FAULT_CASES) + [
    "invalid-cut",
    "truncated-output",
    "interrupted-resume",
]


def _print_json(payload: dict, stream=sys.stdout) -> None:
    print(json.dumps(payload, indent=2, sort_keys=True), file=stream)


def _run_demo(args: argparse.Namespace) -> int:
    case = args.case
    out_dir = Path(args.out)
    tb = TimeBase()

    if case == "invalid-cut":
        # 非法剪辑点：剪辑越过时间线末端，应在校验期被拒绝
        bad = Timeline(tb=tb, duration=tb.to_ticks(4.0), clips=[
            Clip(track=0, start=0, duration=tb.to_ticks(5.0), source_id="x"),
        ])
        bad.validate()
        return EXIT_OK  # 不可达

    fault = FAULT_CASES.get(case, FAULT_CASES["mixed"])
    config = PipelineConfig(
        out_dir=out_dir,
        task_id=f"demo-{case}",
        seed=args.seed,
        fault=fault,
        force_rebuild=args.force_rebuild,
        audio_offset_ticks=tb.to_ticks(2.0 / 24),
    )

    interrupted_note = None
    if case in ("mixed", "interrupted-resume"):
        # 先跑一部分后模拟中断，再续跑复用已提交块
        first = PipelineConfig(**{**config.__dict__,
                                  "interrupt_after_chunks": 2})
        try:
            run_pipeline(first)
        except PipelineInterrupted as exc:
            interrupted_note = str(exc)

    result = run_pipeline(config)
    stats = dict(result.stats)
    if interrupted_note:
        stats["resume_note"] = interrupted_note

    if case == "truncated-output":
        # 截断首个已提交块，复核应报告首个损坏块并返回非零
        victim = out_dir / f"chunk-{config.task_id}-0000.npy"
        data = victim.read_bytes()
        victim.write_bytes(data[: len(data) // 2])
        try:
            verify_manifest(result.manifest_path)
        except CheckpointError as exc:
            stats["corrupt_blocks"] = exc.conflict_blocks
            stats["error"] = str(exc)
            _print_json(stats)
            return exc.exit_code

    verify_manifest(result.manifest_path)  # 指标超限抛 MetricsExceededError
    _print_json(stats)
    return EXIT_OK


def _run_verify(args: argparse.Namespace) -> int:
    stats = verify_manifest(Path(args.manifest))
    _print_json(stats)
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="relay_timeline",
        description="区域直播分发的可恢复离线时间线重建",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="生成样例并跑通故障链路")
    demo.add_argument("--seed", type=int, default=41)
    demo.add_argument("--out", required=True)
    demo.add_argument("--case", default="mixed", choices=DEMO_CASES)
    demo.add_argument("--force-rebuild", action="store_true")
    demo.set_defaults(func=_run_demo)

    verify = sub.add_parser("verify", help="复核清单与验收指标")
    verify.add_argument("--manifest", required=True)
    verify.set_defaults(func=_run_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except RelayError as exc:
        _print_json(
            {"error": str(exc), "exit_code": exc.exit_code},
            stream=sys.stderr,
        )
        return exc.exit_code


if __name__ == "__main__":
    sys.exit(main())
