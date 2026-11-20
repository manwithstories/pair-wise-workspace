"""验收适配入口：CLI 只负责参数解析、文件 IO 与阈值判定。

退出码约定：
- 0：验证通过，全部证据达到阈值；
- 1：证据未达阈值（roundtrip/命中率/复核失败等）；
- 2：参数或环境错误（路径越界、文件缺失、输入超限等）。
"""
from __future__ import annotations

import argparse
import sys
from fractions import Fraction
from pathlib import Path

from .index import SegmentIndex
from .model import FieldIndexError, resolve_within
from .pipeline import Pipeline, PipelineConfig
from .repair import RepairConfig
from .timecode import parse_rate

EXIT_OK = 0
EXIT_EVIDENCE = 1
EXIT_USAGE = 2


def _read(path: Path) -> bytes:
    try:
        return path.read_bytes()
    except OSError as exc:
        raise FieldIndexError(f"无法读取 {path}: {exc}") from exc


def _print_summary(result, index_path: Path) -> None:
    ok, total = result.verified
    print(f"index={index_path}")
    print(f"segments={len(result.segments)}")
    print(f"roundtrip_rate={float(result.roundtrip_rate):.4f}")
    print(f"keyframe_hit_rate={float(result.keyframe_hit_rate):.3f}")
    print(f"sha256_verified={ok}/{total}")
    print(f"recovered={result.recovered}")
    print(f"errors={len(result.errors)}")
    for err in result.errors:
        print(
            f"  - [{err.stage}] {err.source}@{err.start}..{err.end} "
            f"{err.reason} action={err.action} "
            f"recoverable={err.recoverable}"
        )


def _thresholds(args: argparse.Namespace) -> dict:
    return {
        "min_roundtrip": getattr(args, "min_roundtrip", 0.99),
        "min_keyframe": getattr(args, "min_keyframe", 1.0),
    }


def _evaluate(result, args: argparse.Namespace) -> int:
    th = _thresholds(args)
    ok, total = result.verified
    failures = []
    if float(result.roundtrip_rate) < th["min_roundtrip"]:
        failures.append(
            f"roundtrip_rate {float(result.roundtrip_rate):.4f} < {th['min_roundtrip']}"
        )
    if float(result.keyframe_hit_rate) < th["min_keyframe"]:
        failures.append(
            f"keyframe_hit_rate {float(result.keyframe_hit_rate):.3f} < {th['min_keyframe']}"
        )
    if ok != total or total == 0:
        failures.append(f"sha256 复核未全部通过: {ok}/{total}")
    if failures:
        for msg in failures:
            print(f"EVIDENCE-FAIL: {msg}", file=sys.stderr)
        return EXIT_EVIDENCE
    return EXIT_OK


def cmd_process(args: argparse.Namespace) -> int:
    try:
        workdir = Path(args.workdir).resolve()
        sub_path = resolve_within(workdir, args.subtitles)
        tc_path = resolve_within(workdir, args.timecodes)
        media_path = resolve_within(workdir, args.media)
        index_path = resolve_within(workdir, args.index)
        from .media import probe_media

        sub = _read(sub_path)
        tc = _read(tc_path)
        media = probe_media(media_path)
        rate = parse_rate(args.rate) if args.rate else media.frame_rate
        cfg = PipelineConfig(
            frame_rate=rate,
            gop=args.gop,
            drift_limit=Fraction(args.drift_limit).limit_denominator(10_000),
            subtitle_source=sub_path.name,
            timecode_source=tc_path.name,
        )
        blobs = {sub_path.name: sub, tc_path.name: tc}
        pipeline = Pipeline(index_path, cfg,
                            RepairConfig(drift_limit=cfg.drift_limit))
        result = pipeline.build(sub, tc, blobs, media, resume=not args.fresh)
    except (FieldIndexError, OSError, ValueError) as exc:
        print(f"USAGE-ERROR: {exc}", file=sys.stderr)
        return EXIT_USAGE
    _print_summary(result, index_path)
    return _evaluate(result, args)


def cmd_verify(args: argparse.Namespace) -> int:
    try:
        workdir = Path(args.workdir).resolve()
        index_path = resolve_within(workdir, args.index)
        blobs = {}
        for name in args.inputs:
            p = resolve_within(workdir, name)
            blobs[p.name] = _read(p)
        reader = SegmentIndex(index_path)
        reusable = reader.load_existing()
        if not reusable:
            print("EVIDENCE-FAIL: 索引为空或不可读", file=sys.stderr)
            return EXIT_EVIDENCE
        ok, total, failed = reader.verify(blobs)
    except (FieldIndexError, OSError, ValueError) as exc:
        print(f"USAGE-ERROR: {exc}", file=sys.stderr)
        return EXIT_USAGE
    print(f"sha256_verified={ok}/{total}")
    if failed:
        print(f"EVIDENCE-FAIL: 分段复核失败 {failed}", file=sys.stderr)
        return EXIT_EVIDENCE
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="fieldindex")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("process", help="解析/修复/建索引并验证")
    p.add_argument("--workdir", required=True)
    p.add_argument("--subtitles", required=True)
    p.add_argument("--timecodes", required=True)
    p.add_argument("--media", required=True)
    p.add_argument("--index", required=True)
    p.add_argument("--rate", default=None)
    p.add_argument("--gop", type=int, default=100)
    p.add_argument("--drift-limit", default="0.25")
    p.add_argument("--fresh", action="store_true")
    p.add_argument("--min-roundtrip", type=float, default=0.99)
    p.add_argument("--min-keyframe", type=float, default=1.0)
    p.set_defaults(func=cmd_process)

    v = sub.add_parser("verify", help="回读索引并复核 SHA-256")
    v.add_argument("--workdir", required=True)
    v.add_argument("--index", required=True)
    v.add_argument("--inputs", nargs="+", required=True)
    v.set_defaults(func=cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
