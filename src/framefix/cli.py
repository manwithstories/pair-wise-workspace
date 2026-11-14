"""Command-line interface for framefix."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from .errors import FramefixError, InputOpenError, NoVideoStreamError, UnsupportedCodecError
from .report import analyze_media, render_scan, scan_json
from .repair import repair_media
from .synth import PROFILES, synth_many

EXIT_OK = 0
EXIT_INPUT = 2
EXIT_PARTIAL = 3
EXIT_PIXEL_MISMATCH = 4


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="framefix",
        description="Inspect and repair H.264/H.265 MP4 packet timestamps without external tools.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    scan = subparsers.add_parser("scan", help="decode and report frame/timestamp health")
    scan.add_argument("input", help="input MP4 path")
    scan.add_argument("--json", action="store_true", help="emit machine-readable JSON")
    scan.add_argument("--max-jump-ms", type=float, default=250.0)
    scan.add_argument("--window", type=int, default=64, help="bounded reorder window")

    repair = subparsers.add_parser("repair", help="remux with corrected packet timestamps")
    repair.add_argument("input", help="input MP4 path")
    repair.add_argument("--out", required=True, help="repaired MP4 output path")
    repair.add_argument("--report", required=True, help="JSON report output path")
    repair.add_argument("--max-jump-ms", type=float, default=250.0)
    repair.add_argument("--window", type=int, default=64)

    synth = subparsers.add_parser("synth", help="generate deterministic synthetic fixtures")
    synth.add_argument("--out", default="fixtures", help="fixture output directory")
    synth.add_argument("--profiles", default="clean,bframes,broken")
    synth.add_argument("--codec", choices=("h264", "hevc"), default="h264")
    synth.add_argument("--frames", type=int, default=150)
    synth.add_argument("--width", type=int, default=320)
    synth.add_argument("--height", type=int, default=180)

    subparsers.add_parser("selfcheck", help="run built-in end-to-end and edge-case checks")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "scan":
            return _scan(args)
        if args.command == "repair":
            return _repair(args)
        if args.command == "synth":
            return _synth(args)
        if args.command == "selfcheck":
            from .selfcheck import run_selfcheck

            return run_selfcheck()
    except (InputOpenError, NoVideoStreamError, UnsupportedCodecError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_INPUT
    except (FramefixError, ValueError, OSError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_PARTIAL
    return EXIT_PARTIAL


def _scan(args: argparse.Namespace) -> int:
    if args.window < 1:
        raise ValueError("--window must be at least 1")
    result = analyze_media(
        args.input,
        max_jump_ms=args.max_jump_ms,
        window=args.window,
        capture_plan=False,
    )
    print(scan_json(result) if args.json else render_scan(result))
    return result.status_code


def _repair(args: argparse.Namespace) -> int:
    if args.window < 1:
        raise ValueError("--window must be at least 1")
    outcome = repair_media(
        args.input,
        args.out,
        args.report,
        max_jump_ms=args.max_jump_ms,
        window=args.window,
    )
    summary = {
        "fixed_entries": outcome.report["fixed_entries"],
        "reasons": outcome.report["reasons"],
        "pixel_digest_match": outcome.report["pixel_digest_match"],
        "passes": outcome.passes,
        "residual_entries": outcome.report["residual_entries"],
        "output_written": outcome.output_written,
        "report": str(Path(args.report)),
        "exit_code": outcome.exit_code,
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return outcome.exit_code


def _synth(args: argparse.Namespace) -> int:
    profiles = [item.strip() for item in args.profiles.split(",") if item.strip()]
    unknown = [item for item in profiles if item not in PROFILES]
    if unknown:
        raise ValueError(f"unknown synth profiles: {', '.join(unknown)}")
    if not profiles:
        raise ValueError("--profiles must not be empty")
    if args.frames < 1 or args.width < 16 or args.height < 16:
        raise ValueError("frames and dimensions must be positive")
    results = synth_many(
        args.out,
        profiles,
        codec=args.codec,
        frames=args.frames,
        width=args.width,
        height=args.height,
    )
    print(
        json.dumps(
            [
                {
                    "profile": item.profile,
                    "path": item.path,
                    "codec": item.codec,
                    "frames": item.frames,
                    "bytes": item.bytes,
                    "injected": item.injected,
                }
                for item in results
            ],
            ensure_ascii=False,
            indent=2,
        )
    )
    return EXIT_OK


if __name__ == "__main__":
    raise SystemExit(main())
