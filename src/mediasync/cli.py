"""Terminal entry point: ``mediate sync demo`` and ``mediasync verify``.

Exit codes follow the acceptance contract:

* ``0`` – every gate passed,
* ``2`` – invalid input, parameters or a malformed manifest,
* ``3`` – unrecoverable intervals were found,
* ``4`` – the manifest or synchronization metrics failed their gate.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from .errors import (
    EXIT_CODES,
    EXIT_INVALID_INPUT,
    EXIT_OK,
    EXIT_QUALITY_GATE,
    EXIT_UNRECOVERABLE,
    MediaSyncError,
    QualityGateError,
    UnrecoverableError,
)
from .pipeline import PipelineConfig, PipelineResult, run_pipeline
from .report import (
    format_summary,
    load_manifest,
    verify_manifest,
    verify_summary,
)


def _positive_int(text: str) -> int:
    try:
        value = int(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"expected an integer, got {text!r}") from exc
    if value <= 0:
        raise argparse.ArgumentTypeError("value must be positive")
    return value


def _non_negative_int(text: str) -> int:
    try:
        value = int(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"expected an integer, got {text!r}") from exc
    if value < 0:
        raise argparse.ArgumentTypeError("value cannot be negative")
    return value


def _unit_interval(text: str) -> float:
    try:
        value = float(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"expected a number, got {text!r}") from exc
    if not 0.0 <= value < 1.0:
        raise argparse.ArgumentTypeError("value must be within [0, 1)")
    return value


def _positive_float(text: str) -> float:
    try:
        value = float(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"expected a number, got {text!r}") from exc
    if value <= 0:
        raise argparse.ArgumentTypeError("value must be positive")
    return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mediasync",
        description=(
            "Deterministic multi-track A/V ingestion, recovery, synchronization "
            "and manifest verification."
        ),
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    demo = subparsers.add_parser(
        "demo", help="generate fixtures, run the pipeline and write a manifest"
    )
    demo.add_argument("--quiet", action="store_true", help="suppress progress lines")
    demo.add_argument("--seed", type=int, default=17, help="random seed")
    demo.add_argument("--loss", type=_unit_interval, default=0.08)
    demo.add_argument("--jitter-ms", type=_non_negative_int, default=45)
    demo.add_argument("--target-bps", type=_positive_int, default=4_000_000)
    demo.add_argument("--out", type=Path, required=True, help="manifest output path")
    demo.add_argument(
        "--codec", choices=("h264", "hevc", "av1"), default="h264",
        help="raw elementary stream codec",
    )
    demo.add_argument("--frames", type=_positive_int, default=150)
    demo.add_argument("--fps", default="30", help="frame rate, e.g. 30 or 30000/1001")
    demo.add_argument("--tracks", type=_positive_int, default=2)
    demo.add_argument("--sample-rate", type=_positive_int, default=48_000)
    demo.add_argument("--audio-seconds", type=_positive_float, default=None)
    demo.add_argument("--max-error-ms", type=_positive_float, default=20.0)
    demo.add_argument(
        "--workdir", type=Path, default=None,
        help="keep generated fixtures in this directory instead of a temp dir",
    )

    verify = subparsers.add_parser(
        "verify", help="re-check a manifest written by demo"
    )
    verify.add_argument("--quiet", action="store_true", help="suppress progress lines")
    verify.add_argument("manifest", type=Path)

    inspect = subparsers.add_parser(
        "inspect", help="print a short JSON digest of a manifest"
    )
    inspect.add_argument("manifest", type=Path)
    return parser


def _emit(lines: Sequence[str], quiet: bool) -> None:
    if quiet:
        return
    for line in lines:
        print(line)


def _demo(args: argparse.Namespace) -> int:
    config = PipelineConfig(
        seed=args.seed,
        codec=args.codec,
        frames=args.frames,
        fps=args.fps,
        target_bps=args.target_bps,
        loss=args.loss,
        jitter_ms=args.jitter_ms,
        audio_seconds=args.audio_seconds,
        sample_rate=args.sample_rate,
        max_tracks=args.tracks,
        max_error_ms=args.max_error_ms,
    )
    result = run_pipeline(config, workdir=args.workdir, keep_workdir=bool(args.workdir))
    path = result.manifest.write(args.out)
    payload = result.manifest.to_dict()
    _emit(format_summary(result.manifest), args.quiet)
    _emit([f"manifest={path}"], args.quiet)
    failures = verify_manifest(payload)
    if failures:
        for failure in failures:
            print(f"gate_failure={failure}", file=sys.stderr)
        if result.manifest.unrecoverable > 0:
            raise UnrecoverableError(
                f"{result.manifest.unrecoverable} packets could not be recovered",
                [gap for gap in result.report.gaps],
            )
        raise QualityGateError("; ".join(failures))
    if result.manifest.unrecoverable > 0:
        raise UnrecoverableError(
            f"{result.manifest.unrecoverable} packets could not be recovered",
            [gap for gap in result.report.gaps],
        )
    return EXIT_OK


def _verify(args: argparse.Namespace) -> int:
    payload = load_manifest(args.manifest)
    failures = verify_manifest(payload)
    _emit(verify_summary(payload, failures), args.quiet)
    if failures:
        for failure in failures:
            print(f"gate_failure={failure}", file=sys.stderr)
        transport = payload.get("transport", {})
        if isinstance(transport, dict) and int(transport.get("unrecoverable", 0)) > 0:
            return EXIT_UNRECOVERABLE
        return EXIT_QUALITY_GATE
    transport = payload.get("transport", {})
    if isinstance(transport, dict) and int(transport.get("unrecoverable", 0)) > 0:
        return EXIT_UNRECOVERABLE
    return EXIT_OK


def _inspect(args: argparse.Namespace) -> int:
    payload = load_manifest(args.manifest)
    failures = verify_manifest(payload)
    print(
        json.dumps(
            {
                "status": payload.get("status"),
                "verified": not failures,
                "bitrate": payload.get("bitrate"),
                "tracks": [
                    {
                        "track_id": track.get("track_id"),
                        "status": track.get("status"),
                        "max_error_ms": track.get("max_error_ms"),
                    }
                    for track in payload.get("tracks", [])
                ],
                "transport": payload.get("transport"),
            },
            indent=2,
            sort_keys=True,
        )
    )
    return EXIT_OK if not failures else EXIT_QUALITY_GATE


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:  # argparse already printed a message
        return int(exc.code) if isinstance(exc.code, int) else EXIT_INVALID_INPUT
    try:
        if args.command == "demo":
            return _demo(args)
        if args.command == "verify":
            return _verify(args)
        if args.command == "inspect":
            return _inspect(args)
        parser.error(f"unknown command {args.command!r}")
    except MediaSyncError as exc:
        print(f"error[{exc.category}]: {exc}", file=sys.stderr)
        if isinstance(exc, UnrecoverableError):
            for gap in exc.intervals[:16]:
                print(f"  gap {gap.to_dict()}", file=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:  # pragma: no cover - interactive only
        print("interrupted", file=sys.stderr)
        return EXIT_INVALID_INPUT
    return EXIT_INVALID_INPUT


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
