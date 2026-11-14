"""Command-line entry point and exit-code policy."""

from __future__ import annotations

import argparse
import sys
from fractions import Fraction
from pathlib import Path
from typing import Optional, Sequence

from . import __version__
from .decode import DecodeResult, decode_file, detect_nominal_fps
from .fixtures import make_fixtures
from .manifest import read_manifest, write_manifest
from .reorder import DEFAULT_REORDER_CAPACITY, reorder_frames
from .repair import repair_timestamps, ticks_to_ms
from .report import build_stats, json_text, terminal_summary, write_json
from .timeline import build_timeline


EXIT_OK = 0
EXIT_INPUT = 1
EXIT_DECODE = 2
EXIT_MISMATCH = 3
EXIT_UNREPAIRED = 4


class ArgumentParser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        self.print_usage(sys.stderr)
        self.exit(EXIT_INPUT, f"{self.prog}: error: {message}\n")


def positive_fraction(value: str) -> Fraction:
    try:
        fraction = Fraction(value)
    except (ValueError, ZeroDivisionError) as exc:
        raise argparse.ArgumentTypeError("must be a positive number or fraction") from exc
    if fraction <= 0:
        raise argparse.ArgumentTypeError("must be positive")
    return fraction


def nonnegative_int(value: str) -> int:
    try:
        number = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("must be an integer") from exc
    if number < 0:
        raise argparse.ArgumentTypeError("must be non-negative")
    return number


def positive_int(value: str) -> int:
    number = nonnegative_int(value)
    if number < 1:
        raise argparse.ArgumentTypeError("must be positive")
    return number


def _common_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--fps", type=positive_fraction, default=None,
                        help="nominal frame rate (default: stream rate, else 25)")
    parser.add_argument("--max-jump-ms", type=nonnegative_int, default=200,
                        help="maximum allowed timestamp jump in milliseconds")
    parser.add_argument("--max-decode-errors", type=nonnegative_int, default=0,
                        help="decode failures tolerated before exit code 2")
    parser.add_argument("--reorder-capacity", type=positive_int,
                        default=DEFAULT_REORDER_CAPACITY)


def _parser() -> argparse.ArgumentParser:
    parser = ArgumentParser(prog="frameaudit", description=__doc__)
    parser.add_argument("--version", action="version", version=__version__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    scan = subparsers.add_parser("scan", help="decode and audit timestamps")
    scan.add_argument("input", type=Path)
    scan.add_argument("--json-out", type=Path)
    _common_options(scan)

    repair = subparsers.add_parser("repair", help="repair and write a frame manifest")
    repair.add_argument("input", type=Path)
    repair.add_argument("--strategy", choices=("shift", "cfr"), required=True)
    repair.add_argument("--manifest", type=Path, required=True)
    repair.add_argument("--json-out", type=Path)
    _common_options(repair)

    verify = subparsers.add_parser("verify", help="cross-check a manifest against source")
    verify.add_argument("input", type=Path)
    verify.add_argument("--manifest", type=Path, required=True)
    verify.add_argument("--fps", type=positive_fraction, default=None)
    verify.add_argument("--max-jump-ms", type=nonnegative_int, default=200)
    verify.add_argument("--max-decode-errors", type=nonnegative_int, default=0)

    fixtures = subparsers.add_parser("make-fixtures", help="encode local demo fixtures")
    fixtures.add_argument("--out", type=Path, required=True)
    fixtures.add_argument("--fps", type=positive_fraction, default=Fraction(25, 1))
    fixtures.add_argument("--seconds", type=positive_fraction, default=Fraction(6, 1))
    fixtures.add_argument("--gop", type=positive_int, default=12)
    return parser


def _validate_input(path: Path) -> None:
    if not path.is_file():
        raise ValueError(f"input is not a readable file: {path}")
    try:
        with path.open("rb") as handle:
            handle.read(1)
    except OSError as exc:
        raise ValueError(f"input is not readable: {path}: {exc}") from exc


def _fps_text(fps: Fraction) -> str:
    return str(fps.numerator) if fps.denominator == 1 else f"{fps.numerator}/{fps.denominator}"


def _decode_frames(path: Path, args: argparse.Namespace) -> tuple[DecodeResult, Fraction]:
    requested = getattr(args, "fps", None)
    result = decode_file(path, nominal_fps=requested or Fraction(25, 1))
    fps = detect_nominal_fps(result, requested)
    return result, fps


def _run_scan(args: argparse.Namespace) -> int:
    _validate_input(args.input)
    decoded, fps = _decode_frames(args.input, args)
    if decoded.decode_errors > args.max_decode_errors:
        print(f"decode errors={decoded.decode_errors} exceed limit", file=sys.stderr)
        return EXIT_DECODE
    reordered = reorder_frames(decoded.frames, capacity=args.reorder_capacity)
    repair = repair_timestamps(
        reordered.frames,
        time_base=decoded.time_base,
        fps=fps,
        strategy="scan",
        max_jump_ms=args.max_jump_ms,
    )
    timeline = build_timeline(reordered.frames, repair, decoded.time_base)
    stats = _stats(decoded, reordered, repair, timeline, fps)
    print(terminal_summary(stats, title=f"scan: {args.input}"))
    if args.json_out:
        write_json(args.json_out, stats)
    return EXIT_OK


def _run_repair(args: argparse.Namespace) -> int:
    _validate_input(args.input)
    decoded, fps = _decode_frames(args.input, args)
    if decoded.decode_errors > args.max_decode_errors:
        print(f"decode errors={decoded.decode_errors} exceed limit", file=sys.stderr)
        return EXIT_DECODE
    reordered = reorder_frames(decoded.frames, capacity=args.reorder_capacity)
    repair = repair_timestamps(
        reordered.frames,
        time_base=decoded.time_base,
        fps=fps,
        strategy=args.strategy,
        max_jump_ms=args.max_jump_ms,
    )
    timeline = build_timeline(reordered.frames, repair, decoded.time_base)
    stats = _stats(decoded, reordered, repair, timeline, fps)
    write_manifest(args.manifest, timeline)
    print(terminal_summary(stats, title=f"repair[{args.strategy}]: {args.input}"))
    if args.json_out:
        write_json(args.json_out, stats)
    if not repair.monotonic or repair.residual_max_jump_ms > args.max_jump_ms:
        return EXIT_UNREPAIRED
    return EXIT_OK


def _manifest_verify(args: argparse.Namespace, decoded: DecodeResult) -> tuple[dict[str, object], bool]:
    rows = read_manifest(args.manifest)
    mismatched = abs(len(decoded.frames) - len(rows))
    by_decode_index = {frame.decode_index: frame for frame in decoded.frames}
    seen_decode_indices: set[int] = set()
    monotonic = True
    residual = 0
    previous: Optional[int] = None
    for expected_display, row in enumerate(rows):
        if row.get("display_index") != expected_display:
            mismatched += 1
        decode_index = row.get("decode_index")
        frame = by_decode_index.get(decode_index) if isinstance(decode_index, int) else None
        if isinstance(decode_index, int):
            if decode_index in seen_decode_indices:
                mismatched += 1
            seen_decode_indices.add(decode_index)
        if frame is None:
            mismatched += 1
        else:
            if row.get("pict_type") != frame.pict_type:
                mismatched += 1
            if bool(row.get("keyframe")) != frame.keyframe:
                mismatched += 1
        repaired_pts = row.get("repaired_pts")
        if not isinstance(repaired_pts, int):
            mismatched += 1
            monotonic = False
            continue
        if previous is not None:
            delta = repaired_pts - previous
            if delta <= 0:
                monotonic = False
            residual = max(residual, ticks_to_ms(delta, decoded.time_base))
        previous = repaired_pts
    mismatched += len(set(by_decode_index) - seen_decode_indices)
    payload: dict[str, object] = {
        "checked_frames": len(rows),
        "mismatched": mismatched,
        "monotonic": monotonic,
        "residual_max_jump_ms": residual,
    }
    return payload, (mismatched == 0 and monotonic and residual <= args.max_jump_ms)


def _run_verify(args: argparse.Namespace) -> int:
    _validate_input(args.input)
    if not args.manifest.is_file():
        raise ValueError(f"manifest is not a readable file: {args.manifest}")
    decoded, _fps = _decode_frames(args.input, args)
    if decoded.decode_errors > args.max_decode_errors:
        print(f"decode errors={decoded.decode_errors} exceed limit", file=sys.stderr)
        return EXIT_DECODE
    payload, consistent = _manifest_verify(args, decoded)
    print(json_text(payload))
    return EXIT_OK if consistent else EXIT_MISMATCH


def _stats(decoded: DecodeResult, reordered, repair, timeline, fps: Fraction) -> dict:
    return build_stats(decoded, reordered, repair, timeline, nominal_fps=_fps_text(fps))


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "scan":
            return _run_scan(args)
        if args.command == "repair":
            return _run_repair(args)
        if args.command == "verify":
            return _run_verify(args)
        if args.command == "make-fixtures":
            paths = make_fixtures(
                args.out,
                fps=args.fps,
                seconds=args.seconds,
                gop=args.gop,
            )
            for path in paths:
                print(path)
            return EXIT_OK
        raise ValueError(f"unknown command: {args.command}")
    except (ValueError, OSError, UnicodeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_INPUT


if __name__ == "__main__":
    raise SystemExit(main())
