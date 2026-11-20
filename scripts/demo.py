#!/usr/bin/env python3
"""现场字幕/时间码索引演示：生成确定性夹具 -> 建索引 -> 注入损坏 -> 续建验证。

退出码与 CLI 契约一致：0 证据达标，1 证据不足，2 参数/环境错误。
"""
from __future__ import annotations

import argparse
import sys
import time
from fractions import Fraction
from pathlib import Path

# 允许从源码树直接运行（scripts/ 不在包路径内）
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from fieldindex.cli import EXIT_EVIDENCE, EXIT_OK, EXIT_USAGE  # noqa: E402
from fieldindex.fixtures import build_fixtures  # noqa: E402
from fieldindex.media import probe_media  # noqa: E402
from fieldindex.pipeline import Pipeline, PipelineConfig  # noqa: E402
from fieldindex.repair import RepairConfig  # noqa: E402


def _run_pipeline(bundle, index_path, resume: bool):
    sub = bundle.subtitles.read_bytes()
    tc = bundle.timecodes.read_bytes()
    media = probe_media(bundle.media)
    cfg = PipelineConfig(
        frame_rate=bundle.rate,
        gop=bundle.gop,
        drift_limit=Fraction(1, 4),
        subtitle_source=bundle.subtitles.name,
        timecode_source=bundle.timecodes.name,
    )
    blobs = {
        bundle.subtitles.name: sub,
        bundle.timecodes.name: tc,
    }
    pipeline = Pipeline(
        index_path, cfg, RepairConfig(drift_limit=cfg.drift_limit)
    )
    return pipeline.build(sub, tc, blobs, media, resume=resume)


def _print_result(tag, result):
    ok, total = result.verified
    print(f"[{tag}]")
    print(f"  segments={len(result.segments)}")
    print(f"  roundtrip_rate={float(result.roundtrip_rate):.4f}")
    print(f"  keyframe_hit_rate={float(result.keyframe_hit_rate):.3f}")
    print(f"  sha256_verified={ok}/{total}")
    print(f"  recovered={result.recovered}")
    print(f"  errors={len(result.errors)}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="fieldindex 验收演示")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--frames", type=int, default=1200)
    parser.add_argument("--gop", type=int, default=100)
    parser.add_argument("--workdir", default=None)
    parser.add_argument("--corrupt-tail", action="store_true")
    parser.add_argument("--partial-index", action="store_true")
    args = parser.parse_args(argv)

    root = Path(__file__).resolve().parents[1]
    workdir = Path(args.workdir) if args.workdir else root / ".tmp" / "demo"
    try:
        workdir.mkdir(parents=True, exist_ok=True)
        started = time.perf_counter()
        bundle = build_fixtures(
            workdir, seed=args.seed, frames=args.frames, gop=args.gop,
            corrupt_tail=args.corrupt_tail,
            partial_index=args.partial_index,
        )
        index_path = workdir / "index.jsonl"
        if index_path.exists():
            index_path.unlink()

        first = _run_pipeline(bundle, index_path, resume=False)
        _print_result("first-build", first)
        first_hashes = [s.sha256 for s in first.segments]

        # 复跑：相同输入必须得到相同分段边界与哈希
        rerun_path = workdir / "index_rerun.jsonl"
        rerun = _run_pipeline(bundle, rerun_path, resume=False)
        rerun_hashes = [s.sha256 for s in rerun.segments]
        deterministic = (
            first_hashes == rerun_hashes
            and [(s.start, s.end) for s in first.segments]
            == [(s.start, s.end) for s in rerun.segments]
        )
        print(f"  deterministic_rerun={'yes' if deterministic else 'NO'}")

        # 中断续建：截断索引尾部后恢复，证据须与首跑一致
        if args.partial_index:
            raw = index_path.read_text().splitlines(keepends=True)
            cut = next(
                (i for i, line in enumerate(raw)
                 if '"segment_id": 7' in line),
                len(raw),
            )
            index_path.write_text("".join(raw[:cut]))
            resumed = _run_pipeline(bundle, index_path, resume=True)
            _print_result("resume-rebuild", resumed)
            resumed_hashes = [s.sha256 for s in resumed.segments]
            resume_match = resumed_hashes == first_hashes
            print(f"  resume_hash_match={'yes' if resume_match else 'NO'}")
        else:
            resumed = first
            resume_match = True

        elapsed = time.perf_counter() - started
        print(f"elapsed_seconds={elapsed:.3f}")
    except OSError as exc:
        print(f"USAGE-ERROR: {exc}", file=sys.stderr)
        return EXIT_USAGE

    ok, total = resumed.verified
    checks = [
        float(resumed.roundtrip_rate) >= 0.99,
        float(resumed.keyframe_hit_rate) >= 1.0,
        len(resumed.segments) == bundle.segment_count,
        ok == total == bundle.segment_count,
        resumed.recovered >= 2,
        len(resumed.errors) >= 2,
        deterministic and resume_match,
    ]
    if not all(checks):
        print("EVIDENCE-FAIL: 验收阈值未全部满足", file=sys.stderr)
        return EXIT_EVIDENCE
    print("ACCEPTANCE=PASS")
    return EXIT_OK


if __name__ == "__main__":
    raise SystemExit(main())
