"""Terminal batch processor for ETJ validation, resolution and scheduling."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Any, Sequence

from .conflict import scan_conflicts
from .convert import (
    FrameRateMismatchError,
    convert_etj,
    model_summary,
    read_etj,
    read_otio,
    write_etj,
    write_otio,
)
from .io_utils import DocumentIOError, ExchangeError, save_document
from .model import Timeline, ModelError, validate_model
from .resolve import ResolveError, resolve
from .schedule import build_schedule
from .timebase import TimebaseError, parse_rate

EXIT_OK = 0
EXIT_INVALID = 2
EXIT_UNRESOLVED = 3
EXIT_DRIFT = 4
EXIT_IO = 5


class CliFailure(Exception):
    def __init__(self, code: int, message: str, report: dict[str, Any] | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.report = report


def _write_report(path: str | None, data: dict[str, Any]) -> None:
    if path:
        save_document(path, data)


def _require_valid(timeline: Timeline) -> None:
    errors = validate_model(timeline)
    if errors:
        raise CliFailure(EXIT_INVALID, "; ".join(errors), {"errors": errors})


def _invalid_conflicts(timeline: Timeline) -> list[dict[str, Any]]:
    report = scan_conflicts(timeline, include_occlusions=False)
    return [
        conflict.as_dict()
        for conflict in report.conflicts
        if conflict.kind != "same_track_overlap"
    ]


def _out_version(path: str, requested: int | None = None) -> int:
    if requested in (1, 2):
        return requested
    return 1 if ".v1." in Path(path).name else 2


def _version_arg(value: str) -> int:
    normalized = value.lower().removeprefix("v")
    if normalized not in ("1", "2"):
        raise argparse.ArgumentTypeError("version must be 1, 2, v1 or v2")
    return int(normalized)


def _cmd_validate(args: argparse.Namespace) -> int:
    timeline = read_etj(args.input)
    _require_valid(timeline)
    report = scan_conflicts(timeline).as_dict()
    invalid = [item for item in report["invalid"]]
    report.update({"input": args.input, "valid": not invalid})
    _write_report(args.report, report)
    print(f"conflict_count={report['conflict_count']}")
    print(f"pairs={report['pair_count']}")
    print(f"track_count={report['track_count']}")
    print(f"segment_count={report['segment_count']}")
    print(f"occlusion_count={report['occlusion_count']}")
    print(f"audio_concurrency_peak={report['audio_concurrency_peak']}")
    if invalid:
        raise CliFailure(EXIT_INVALID, "invalid interval found", report)
    return EXIT_OK


def _cmd_resolve(args: argparse.Namespace) -> int:
    timeline = read_etj(args.input)
    _require_valid(timeline)
    invalid = _invalid_conflicts(timeline)
    if invalid:
        raise CliFailure(EXIT_INVALID, "input contains zero, negative or out-of-bounds intervals", {"invalid": invalid})
    policies = [item.strip() for item in args.policy.split(",") if item.strip()]
    before = scan_conflicts(timeline)
    result = resolve(timeline, policies, scope=args.scope)
    after = scan_conflicts(result.timeline)
    warnings = write_etj(result.timeline, args.out, _out_version(args.out))
    data = {
        **result.as_dict(),
        "policy": policies,
        "scope": args.scope,
        "input": args.input,
        "output": args.out,
        "conflict_count_before": before.conflict_count,
        "conflict_count_after": after.conflict_count,
        "warnings": warnings,
        "summary": model_summary(result.timeline),
    }
    _write_report(args.report, data)
    print(f"resolved_intervals={result.resolved_intervals}")
    print(f"unresolved_count={len(result.unresolved)}")
    print(f"total_duration_before_samples={result.before_samples}")
    print(f"total_duration_after_samples={result.after_samples}")
    print(f"drift_samples={result.drift_samples}")
    print(f"summary_digest={data['summary']['digest']}")
    if result.unresolved:
        return EXIT_UNRESOLVED
    if warnings or result.drift_samples > args.drift_threshold_samples:
        return EXIT_DRIFT
    return EXIT_OK


def _cmd_schedule(args: argparse.Namespace) -> int:
    timeline = read_etj(args.input)
    _require_valid(timeline)
    invalid = _invalid_conflicts(timeline)
    if invalid:
        raise CliFailure(EXIT_INVALID, "input contains zero, negative or out-of-bounds intervals", {"invalid": invalid})
    fps = parse_rate(args.fps) if args.fps else timeline.fps
    result = build_schedule(timeline, fps)
    data = {
        **result.report,
        "input": args.input,
        "summary": model_summary(timeline),
    }
    _write_report(args.report, data)
    print(f"schedule_frames={result.schedule_frames}")
    print(f"max_concurrent={result.report['max_concurrent']}")
    print(f"drift_samples={result.report['drift_samples']}")
    print(f"digest={result.digest}")
    if result.report["drift_samples"] > args.drift_threshold_samples:
        return EXIT_DRIFT
    return EXIT_OK


def _cmd_convert(args: argparse.Namespace) -> int:
    timeline = read_etj(args.input)
    _require_valid(timeline)
    warnings = write_etj(timeline, args.out, args.to)
    data = {
        "input": args.input,
        "output": args.out,
        "to_version": args.to,
        "warnings": warnings,
        "summary": model_summary(timeline),
    }
    _write_report(args.report, data)
    print(f"to_version={args.to}")
    print(f"warnings={len(warnings)}")
    print(f"summary_digest={data['summary']['digest']}")
    return EXIT_DRIFT if warnings else EXIT_OK


def _cmd_otio(args: argparse.Namespace) -> int:
    if args.export:
        timeline = read_etj(args.input)
        _require_valid(timeline)
        conflicts = scan_conflicts(timeline, include_occlusions=False).conflict_count
        if conflicts:
            raise CliFailure(EXIT_INVALID, "timeline must be conflict-free before OTIO export", {"conflict_count": conflicts})
        write_otio(timeline, args.export)
        try:
            reread = read_otio(args.export)
        except FrameRateMismatchError:
            raise
        except ExchangeError as exc:
            raise CliFailure(EXIT_IO, f"OTIO round-trip failed: {exc}") from exc
        summary = model_summary(timeline)
        roundtrip = model_summary(reread)
        match = summary["digest"] == roundtrip["digest"]
        data = {
            "input": args.input,
            "export": args.export,
            "summary": summary,
            "roundtrip_summary": roundtrip,
            "roundtrip_match": match,
        }
        _write_report(args.report, data)
        print(f"summary_digest={summary['digest']}")
        print(f"roundtrip_match={str(match).lower()}")
        if not match:
            raise CliFailure(EXIT_IO, "OTIO round-trip summary differs", data)
        return EXIT_OK
    if not args.out:
        raise CliFailure(EXIT_INVALID, "--export or --out is required")
    timeline = read_otio(args.input)
    _require_valid(timeline)
    write_etj(timeline, args.out, _out_version(args.out, 2))
    summary = model_summary(timeline)
    data = {"input": args.input, "output": args.out, "summary": summary}
    _write_report(args.report, data)
    print(f"summary_digest={summary['digest']}")
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m tlcore.cli")
    commands = parser.add_subparsers(dest="command", required=True)
    validate = commands.add_parser("validate", help="scan timeline conflicts")
    validate.add_argument("--in", dest="input", required=True)
    validate.add_argument("--report")
    validate.set_defaults(handler=_cmd_validate)
    resolver = commands.add_parser("resolve", help="apply conflict policies")
    resolver.add_argument("--in", dest="input", required=True)
    resolver.add_argument("--policy", default="ripple,trim")
    resolver.add_argument("--scope", choices=("cluster", "track"), default="cluster")
    resolver.add_argument("--out", required=True)
    resolver.add_argument("--report")
    resolver.add_argument("--drift-threshold-samples", type=int, default=48_000)
    resolver.set_defaults(handler=_cmd_resolve)
    scheduler = commands.add_parser("schedule", help="expand timeline by frame")
    scheduler.add_argument("--in", dest="input", required=True)
    scheduler.add_argument("--fps")
    scheduler.add_argument("--report")
    scheduler.add_argument("--drift-threshold-samples", type=int, default=48_000)
    scheduler.set_defaults(handler=_cmd_schedule)
    convert = commands.add_parser("convert", help="convert ETJ versions")
    convert.add_argument("--in", dest="input", required=True)
    convert.add_argument("--to", type=_version_arg, choices=(1, 2), required=True)
    convert.add_argument("--out", required=True)
    convert.add_argument("--report")
    convert.set_defaults(handler=_cmd_convert)
    otio = commands.add_parser("otio", help="export or import OTIO")
    otio.add_argument("--in", dest="input", required=True)
    otio.add_argument("--export")
    otio.add_argument("--out")
    otio.add_argument("--report")
    otio.set_defaults(handler=_cmd_otio)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.handler(args)
    except CliFailure as exc:
        if exc.report:
            try:
                _write_report(args.report, exc.report)
            except DocumentIOError:
                pass
        print(f"error={exc.message}", file=sys.stderr)
        return exc.code
    except DocumentIOError as exc:
        print(f"error={exc}", file=sys.stderr)
        return EXIT_IO
    except FrameRateMismatchError as exc:
        print(f"error={exc}", file=sys.stderr)
        return EXIT_INVALID
    except (ExchangeError, ModelError, ResolveError, TimebaseError, ValueError) as exc:
        print(f"error={exc}", file=sys.stderr)
        return EXIT_INVALID


if __name__ == "__main__":
    raise SystemExit(main())
