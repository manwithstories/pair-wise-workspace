"""Terminal interface for fieldcut.

Exit codes (per the delivery contract):
    0 - success, including runs whose damaged input was fully recovered
    1 - invalid arguments / unreadable input (usage error)
    2 - unrecovered media error, alignment over tolerance, or a manifest that
        fails cross-representation validation

All acceptance numbers are printed directly; no media player is required.
"""

from __future__ import annotations

import argparse
import json
import os
import resource
import sys

from .errors import UsageError
from .manifest import first_inconsistency, max_boundary_divergence_ms
from .orchestrator import representation_summaries, run_job

MEMORY_LIMIT_MB = 256


def _peak_memory_mb() -> float:
    """Resident-set high-water mark for this process (macOS reports bytes)."""
    usage = resource.getrusage(resource.RUSAGE_SELF)
    raw = usage.ru_maxrss
    # macOS ru_maxrss is bytes; Linux is kilobytes. Detect by magnitude.
    mb = raw / (1024.0 * 1024.0) if raw > 10_000_000 else raw / 1024.0
    return round(mb, 2)


def _parse_bitrates(token: str) -> list[int]:
    try:
        values = [int(part) for part in token.split(",") if part.strip()]
    except ValueError as exc:
        raise UsageError(f"invalid bitrate list {token!r}") from exc
    if not values or any(v <= 0 for v in values):
        raise UsageError("bitrates must be a non-empty list of positive ints")
    return values


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="fieldcut",
        description="Align multi-camera field recordings and cut a "
                    "multi-bitrate segmented delivery ladder.")
    sub = parser.add_subparsers(dest="command", required=True)

    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--output", required=True,
                        help="output directory (sources are never modified)")
    common.add_argument("--bitrates", default="800000,1600000",
                        help="comma-separated target bitrates in bps")
    common.add_argument("--segment-seconds", type=float, default=2.0)
    common.add_argument("--queue-capacity", type=int, default=8,
                        help="fixed per-stage queue capacity (frames)")
    common.add_argument("--json", action="store_true",
                        help="emit the full report as JSON on stdout")

    demo = sub.add_parser("demo", parents=[common],
                          help="generate synthetic cameras and run the pipeline")
    demo.add_argument("--duration", type=float, required=True)
    demo.add_argument("--cameras", type=int, required=True)
    demo.add_argument("--inject", default="normal",
                      help="comma-separated: drift,wrap,vfr,truncated,bad_packet")

    process = sub.add_parser(
        "process", parents=[common],
        help="process existing MP4/MOV files (--input f1.mp4 f2.mp4 ...)")
    process.add_argument("--input", nargs="+", required=True)
    return parser


def _run_demo(args) -> dict:
    from .synthdata import CameraSpec, generate_camera, parse_defects, reference_rgb
    if args.cameras < 1:
        raise UsageError("--cameras must be >= 1")
    if args.duration <= 0:
        raise UsageError("--duration must be positive")
    try:
        defects = parse_defects(args.inject)
    except ValueError as exc:
        raise UsageError(str(exc)) from exc
    source_dir = os.path.join(args.output, "sources")
    cameras = []
    for i in range(args.cameras):
        name = f"cam{i + 1}"
        # Distribute requested defects across cameras deterministically so a
        # multi-camera demo always includes at least one healthy reference.
        cam_defects = ("normal",) if i == 0 else defects
        spec = CameraSpec(name, args.duration, cam_defects, seed=1000 + i)
        info = generate_camera(spec, source_dir)
        cameras.append((name, info["path"]))
    return _execute(cameras, args, reference_rgb)


def _run_process(args) -> dict:
    cameras = []
    for path in args.input:
        if not os.path.isfile(path):
            raise UsageError(f"input file not readable: {path}")
        cameras.append((os.path.splitext(os.path.basename(path))[0], path))
    # No synthetic generator for real media: PSNR is measured against the
    # decoded source frames of each segment instead.
    return _execute(cameras, args, None)


def _execute(cameras, args, reference_frame_fn) -> dict:
    bitrates = _parse_bitrates(args.bitrates)
    result = run_job(
        cameras, args.output, bitrates, reference_frame_fn,
        target_seconds=args.segment_seconds,
        queue_capacity=args.queue_capacity)
    summaries = representation_summaries(result)
    alignment_ms = max_boundary_divergence_ms(result.manifest)
    damaged = [
        {"segment": s.index,
         "start_ms": round(s.start_ticks / 90.0, 2),
         "end_ms": round(s.end_ticks / 90.0, 2)}
        for s in result.plan.segments if s.damaged]
    return {
        "segments": len(result.plan.segments),
        "representations": len(result.representations),
        "recovered_segments": result.recovered_segments,
        "unrecovered_count": len(result.log.unrecovered),
        "damaged_ranges": damaged,
        "manifest_alignment_ms": alignment_ms,
        "first_manifest_issue": first_inconsistency(result.manifest),
        "queue_peak_frames": result.queue_peak,
        "peak_memory_mb": _peak_memory_mb(),
        "representations_detail": summaries,
        "audio_continuous": {a.camera: a.continuous for a in result.audio},
    }


def _print_human(report: dict) -> None:
    print(f"segments={report['segments']}")
    print(f"representations={report['representations']}")
    print(f"recovered_segments={report['recovered_segments']}")
    print(f"unrecovered_count={report['unrecovered_count']}")
    print(f"manifest_alignment_ms={report['manifest_alignment_ms']}")
    print(f"peak_memory_mb={report['peak_memory_mb']}")
    if report["damaged_ranges"]:
        first = report["damaged_ranges"][0]
        print(f"damaged_ranges={json.dumps(report['damaged_ranges'])}")
        print(f"first_damaged_range_start_ms={first['start_ms']}")
    for detail in report["representations_detail"]:
        tag = f"{detail['camera']}/r{detail['representation']}"
        print(f"[{tag}] bitrate_target_bps={detail['bitrate_target_bps']} "
              f"bitrate_actual_bps={detail['bitrate_actual_bps']} "
              f"bitrate_error_pct={detail['bitrate_error_pct']} "
              f"bitrate_peak_bps={detail['bitrate_peak_bps']} "
              f"psnr_db={detail['psnr_db']}")


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        report = _run_demo(args) if args.command == "demo" else _run_process(args)
    except UsageError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:  # pipeline/IO failures -> exit 2
        print(f"error: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 2

    if args.json:
        print(json.dumps(report, indent=2, ensure_ascii=False))
    else:
        _print_human(report)

    if report["first_manifest_issue"] is not None:
        issue = report["first_manifest_issue"]
        print(f"manifest validation failed: {issue}", file=sys.stderr)
        return 2
    if report["unrecovered_count"] > 0:
        print(f"unrecovered errors: {report['unrecovered_count']}",
              file=sys.stderr)
        return 2
    if report["peak_memory_mb"] > MEMORY_LIMIT_MB:
        print(f"memory limit exceeded: {report['peak_memory_mb']} MiB",
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
