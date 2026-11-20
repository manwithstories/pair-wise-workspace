"""Terminal-only command line entry point.

Subcommands:

* ``demo``    - generate deterministic HLS/DASH fixtures and run the pipeline
* ``verify``  - re-verify a previously produced output directory

Exit codes: 0 acceptance passed, 2 input/timeline validation failure,
3 unrecoverable processing or write error.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .config import DEFAULT_TARGET_BITRATE, POLICY_GAP_FILL, Limits
from .errors import ErrorCode, FatalError, RecoverableError
from .fixtures import FixtureSpec, build_fixture
from .pipeline import PipelineConfig, run_pipeline

SUMMARY_TEMPLATE = (
    "segments={segments} frames={frames} audio_rms={audio_rms} "
    "bitrate_target_bps={bitrate_target_bps} bitrate_actual_bps="
    "{bitrate_actual_bps} bitrate_error_pct={bitrate_error_pct} "
    "quality_psnr_db={quality_psnr_db} manifest_alignment_ms="
    "{manifest_alignment_ms} peak_memory_mb={peak_memory_mb} "
    "frame_hashes={frame_hashes}"
)


def _summary_line(stats: dict) -> str:
    return SUMMARY_TEMPLATE.format(
        segments=stats["segments"],
        frames=stats["frames"],
        audio_rms=stats["audio_rms"],
        bitrate_target_bps=stats["bitrate_target_bps"],
        bitrate_actual_bps=stats["bitrate_actual_bps"],
        bitrate_error_pct=stats["bitrate_error_pct"],
        quality_psnr_db=stats["quality_psnr_db"],
        manifest_alignment_ms=stats["manifest_alignment_ms"],
        peak_memory_mb=stats["peak_memory_mb"],
        frame_hashes=stats["frame_hashes"],
    )


def _run_one(fmt: str, out_root: Path, args, limits: Limits) -> int:
    media_dir = out_root / fmt
    spec = FixtureSpec(
        out_dir=media_dir,
        fmt=fmt,
        duration=args.duration,
        target_bitrate=args.target_bitrate,
        drift_ppm=args.drift_ppm,
        wrap_at=args.wrap_at,
        vfr=args.vfr,
        fault=args.fault,
    )
    fixture = build_fixture(spec)
    config = PipelineConfig(
        queue_size=args.queue_size,
        drift_ppm_limit=args.drift_limit,
        policy=args.policy,
        abort_after=args.abort_after,
    )
    try:
        result = run_pipeline(fixture.manifest, media_dir / "run",
                              config, fixture.scenario, limits)
    except RecoverableError as exc:
        if exc.code is ErrorCode.WRITE_INTERRUPTED and args.abort_after:
            print(f"[crash-test] {exc.message}", file=sys.stderr)
            result = run_pipeline(fixture.manifest, media_dir / "run",
                                  PipelineConfig(queue_size=args.queue_size,
                                                 drift_ppm_limit=args.drift_limit,
                                                 policy=args.policy),
                                  fixture.scenario, limits)
        elif exc.is_fatal:
            print(f"FATAL: {exc.message}", file=sys.stderr)
            return 3
        else:
            print(f"RECOVERABLE: {exc.message}", file=sys.stderr)
            return 2
    stats = result.summary
    print(f"[{fmt}] " + _summary_line(stats))
    if result.errors.records:
        counts = result.errors.counts()
        first = result.errors.records[0]
        print(f"[{fmt}] first_bad_segment={first.segment} "
              f"first_code={first.code} policy={args.policy} "
              f"recoverable={counts['recoverable']} fatal={counts['fatal']}")
        for gap in stats["gaps"]:
            print(f"[{fmt}] gap segment={gap['segment']} "
                  f"range_s=({float(gap['start_s']):.2f},"
                  f"{float(gap['end_s']):.2f}) policy={gap['policy']} "
                  f"filled_frames={gap['frames_emitted']}")
    return _acceptance(fmt, stats, args)


def _acceptance(fmt: str, stats: dict, args) -> int:
    failures: list[str] = []
    if not (250 <= stats["frames"] <= 400):
        failures.append(f"frames {stats['frames']} outside 250-400")
    if stats["bitrate_error_pct"] >= 8.0:
        failures.append(
            f"bitrate error {stats['bitrate_error_pct']}% >= 8%")
    if stats["manifest_alignment_ms"] >= 40.0:
        failures.append(
            f"alignment {stats['manifest_alignment_ms']}ms >= 40ms")
    import math
    if not math.isfinite(stats["quality_psnr_db"]):
        failures.append("PSNR not finite")
    if not math.isfinite(stats["audio_rms"]):
        failures.append("RMS not finite")
    if stats["frame_hashes"] != stats["frames"]:
        failures.append("hash count != frame count")
    if failures:
        for fail in failures:
            print(f"[{fmt}] ACCEPTANCE FAIL: {fail}", file=sys.stderr)
        return 2
    print(f"[{fmt}] acceptance=PASS")
    return 0


def _cmd_demo(args) -> int:
    limits = Limits.from_env()
    out_root = Path(args.out)
    out_root.mkdir(parents=True, exist_ok=True)
    code = 0
    for fmt in args.format.split(","):
        code = max(code, _run_one(fmt.strip(), out_root, args, limits))
    return code


def _cmd_verify(args) -> int:
    out_root = Path(args.input)
    candidates = [p for p in out_root.iterdir() if p.is_dir()
                  and (p / "run" / "report.json").exists()]
    if not candidates:
        print(f"no run outputs found under {out_root}", file=sys.stderr)
        return 2
    worst = 0
    for media_dir in candidates:
        stats = json.loads(
            (media_dir / "run" / "report.json").read_text(encoding="utf-8"))
        if args.target_bitrate and stats["bitrate_target_bps"] == 0:
            stats["bitrate_target_bps"] = args.target_bitrate
            actual = stats["bitrate_actual_bps"]
            stats["bitrate_error_pct"] = round(
                abs(actual - args.target_bitrate) / args.target_bitrate * 100, 3)
        print(f"[{media_dir.name}] " + _summary_line(stats))
        worst = max(worst, _acceptance(media_dir.name, stats, args))
    return worst


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="relay_timeline")
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="generate fixtures and run pipeline")
    demo.add_argument("--format", default="hls,dash")
    demo.add_argument("--duration", type=float, default=12.0)
    demo.add_argument("--drift-ppm", type=float, default=0.0,
                      dest="drift_ppm")
    demo.add_argument("--wrap-at", type=int, default=1 << 33,
                      dest="wrap_at")
    demo.add_argument("--vfr", action="store_true")
    demo.add_argument("--queue-size", type=int, default=8,
                      dest="queue_size")
    demo.add_argument("--out", required=True)
    demo.add_argument("--fault", choices=["truncated", "zero", "missing"],
                      default=None)
    demo.add_argument("--policy", default=POLICY_GAP_FILL,
                      choices=["fail", "gap-fill", "skip"])
    demo.add_argument("--target-bitrate", type=int,
                      default=DEFAULT_TARGET_BITRATE, dest="target_bitrate")
    demo.add_argument("--drift-limit", type=float, default=100.0,
                      dest="drift_limit")
    demo.add_argument("--abort-after", type=int, default=None,
                      dest="abort_after")
    demo.set_defaults(func=_cmd_demo)

    verify = sub.add_parser("verify", help="verify an existing output dir")
    verify.add_argument("--input", required=True)
    verify.add_argument("--target-bitrate", type=int,
                        default=DEFAULT_TARGET_BITRATE, dest="target_bitrate")
    verify.set_defaults(func=_cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.func(args))
    except FatalError as exc:
        print(f"FATAL [{exc.code.value}]: {exc.message}", file=sys.stderr)
        return 3 if exc.code not in (
            ErrorCode.PATH_TRAVERSAL, ErrorCode.MANIFEST_PARSE,
            ErrorCode.LIMIT_EXCEEDED) else 2
    except RecoverableError as exc:
        print(f"ERROR [{exc.code.value}]: {exc.message}", file=sys.stderr)
        return 2
    except FileNotFoundError as exc:
        print(f"FATAL: {exc}", file=sys.stderr)
        return 3


if __name__ == "__main__":
    sys.exit(main())
