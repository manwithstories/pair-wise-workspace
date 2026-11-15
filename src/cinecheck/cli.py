"""Command line entry point: analyze / repair / fixtures subcommands.

Exit codes:
    0 - no anomalies detected
    2 - anomalies detected and repaired
    3 - unrecoverable input (truncated / no decodable frames)
    4 - invalid arguments
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from . import __version__
from .export import render_json, render_summary, write_json
from .fixtures import generate_fixtures
from .pipeline import (
    EXIT_UNRECOVERABLE,
    EXIT_USAGE,
    run_pipeline,
    write_repair_outputs,
)
from .reorder import DEFAULT_REORDER_DEPTH
from .timeline import DEFAULT_TOLERANCE_MS


def _positive_float(value: str) -> float:
    try:
        parsed = float(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"not a number: {value!r}") from exc
    if parsed <= 0:
        raise argparse.ArgumentTypeError(f"must be positive, got {parsed}")
    return parsed


class _CinecheckParser(argparse.ArgumentParser):
    """ArgumentParser that reports usage problems with our exit code 4."""

    def error(self, message: str) -> None:
        self.print_usage(sys.stderr)
        self.exit(EXIT_USAGE, f"{self.prog}: error: {message}\n")


def build_parser() -> argparse.ArgumentParser:
    parser = _CinecheckParser(
        prog="cinecheck",
        description="H.264 frame inspection and PTS repair (terminal only).",
    )
    parser.add_argument("--version", action="version",
                        version=f"cinecheck {__version__}")
    sub = parser.add_subparsers(dest="command", required=True)

    def add_common(sp: argparse.ArgumentParser) -> None:
        sp.add_argument("source", type=Path, help="input video file (read-only)")
        sp.add_argument("--json", type=Path, default=None,
                        help="write the full JSON report to this path")
        sp.add_argument("--fps-override", type=_positive_float, default=None,
                        help="declared fps when stream metadata is wrong")
        sp.add_argument("--tolerance-ms", type=_positive_float,
                        default=DEFAULT_TOLERANCE_MS,
                        help="jitter tolerance in milliseconds (default 1.0)")
        sp.add_argument("--reorder-depth", type=int,
                        default=DEFAULT_REORDER_DEPTH,
                        help="bounded reorder window, clamped to 1..32")

    analyze = sub.add_parser("analyze", help="inspect a clip and print a summary")
    add_common(analyze)

    repair = sub.add_parser("repair", help="repair the timeline and emit CSV/JSON")
    add_common(repair)
    repair.add_argument("--csv", type=Path, default=None,
                        help="per-frame repaired pts CSV path")

    fixtures = sub.add_parser("fixtures", help="generate synthetic sample clips")
    fixtures.add_argument("--out", type=Path, required=True,
                          help="output directory for the four .mp4 fixtures")
    fixtures.add_argument("--seed", type=int, default=7,
                          help="deterministic RNG seed (default 7)")
    return parser


def _validate(args: argparse.Namespace) -> str | None:
    if args.command in ("analyze", "repair"):
        if not args.source.exists() or not args.source.is_file():
            return f"input file not found: {args.source}"
        if not 1 <= args.reorder_depth <= DEFAULT_REORDER_DEPTH:
            return (f"--reorder-depth must be within 1.."
                    f"{DEFAULT_REORDER_DEPTH}, got {args.reorder_depth}")
    if args.command == "fixtures":
        if args.seed < 0:
            return f"--seed must be non-negative, got {args.seed}"
    return None


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    error = _validate(args)
    if error is not None:
        parser.error(error)

    if args.command == "fixtures":
        return _run_fixtures(args)
    return _run_inspection(args)


def _run_fixtures(args: argparse.Namespace) -> int:
    paths = generate_fixtures(args.out, seed=args.seed)
    for name in sorted(paths):
        print(f"{name}={paths[name]}")
    return 0


def _run_inspection(args: argparse.Namespace) -> int:
    try:
        result = run_pipeline(
            args.source,
            fps_override=args.fps_override,
            tolerance_ms=args.tolerance_ms,
            reorder_depth=args.reorder_depth,
            include_repair=args.command == "repair",
        )
    except Exception as exc:  # noqa: BLE001 - surface as unrecoverable
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_UNRECOVERABLE

    if args.command == "analyze" and args.json is not None:
        write_json(result.payload, args.json)

    if args.command == "repair":
        rows = write_repair_outputs(
            result, getattr(args, "csv", None), args.json
        )
        if args.csv is not None:
            print(f"csv_rows={rows} csv={args.csv}")

    print(render_summary(result.payload))
    if args.json is not None:
        print(f"json={args.json}")
    return result.exit_code


if __name__ == "__main__":
    raise SystemExit(main())
