"""Command-line interface for fixtures, checks, rendering, and OTIO export."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from .conflicts import by_kind, conflict_dicts, scan_conflicts
from .fixtures import FIXTURE_SEED, baseline, generate_fixtures
from .model import InvariantError, ModelError, Timeline
from .otio_bridge import OTIOBridgeError, duration_report, write_otio
from .render import render_timeline
from .resolve import NonConvergenceError, ResolveError, resolve
from .tle import TLEError, read_tle
from .timebase import TimebaseError, parse_rate


class UsageError(ValueError):
    pass


class Parser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        raise UsageError(message)


def _write_json(path: str | Path | None, data: dict[str, Any]) -> None:
    if path is None:
        return
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(
        json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )


def _emit(data: dict[str, Any]) -> None:
    print(json.dumps(data, ensure_ascii=False, indent=2))


def _intervals(timeline: Timeline) -> tuple[list[str], dict[str, list[list[int]]]]:
    merged_by_track: dict[str, list[list[int]]] = {}
    text: list[str] = []
    for track in timeline.tracks:
        merged: list[list[int]] = []
        for clip in track.ordered_clips():
            if clip.duration_ns <= 0:
                continue
            if merged and clip.start_ns <= merged[-1][1]:
                merged[-1][1] = max(merged[-1][1], clip.end_ns)
            else:
                merged.append([clip.start_ns, clip.end_ns])
        merged_by_track[track.id] = merged
        text.extend(f"{track.id}:[{item[0]},{item[1]})" for item in merged)
    return text, merged_by_track


def _check(args: argparse.Namespace) -> tuple[dict[str, Any], int]:
    timeline = read_tle(args.input)
    initial = scan_conflicts(timeline)
    result = resolve(timeline, args.policy, max_iterations=args.max_iterations)
    intervals, intervals_by_track = _intervals(result.timeline)
    resolved_count = len(initial) - len(result.unresolved)
    report = {
        "input": str(args.input),
        "policy": args.policy,
        "conflicts": conflict_dicts(initial),
        "by_kind": by_kind(initial),
        "resolved": resolved_count,
        "resolved_count": resolved_count,
        "resolutions": [item.to_dict() for item in result.resolutions],
        "unresolved": conflict_dicts(result.unresolved),
        "unresolved_count": len(result.unresolved),
        "iterations": result.iterations,
        "fixed_point": result.fixed_point,
        "duration_declared_ns": result.timeline.declared_duration_ns,
        "duration_computed_ns": result.timeline.computed_duration_ns,
        "deviation_ns": result.timeline.deviation_ns,
        "resolved_intervals": intervals,
        "resolved_intervals_by_track": intervals_by_track,
    }
    _write_json(args.report, report)
    return report, 2 if result.unresolved else 0


def _render(args: argparse.Namespace) -> tuple[dict[str, Any], int]:
    fps = parse_rate(args.fps)
    timeline = read_tle(args.input)
    result = resolve(timeline, args.policy, max_iterations=args.max_iterations)
    rendered = render_timeline(result.timeline, fps)
    report = {
        "input": str(args.input),
        "policy": args.policy,
        **rendered.summary(),
        "unresolved_count": len(result.unresolved),
        "duration_computed_ns": result.timeline.computed_duration_ns,
    }
    if args.hash:
        destination = Path(args.hash)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(rendered.sha256 + "\n", encoding="utf-8")
    _write_json(args.report, report)
    return report, 2 if result.unresolved else 0


def _export(args: argparse.Namespace) -> tuple[dict[str, Any], int]:
    timeline = read_tle(args.input)
    write_otio(timeline, args.otio)
    report = duration_report(timeline, path=args.otio)
    report["input"] = str(args.input)
    report["otio"] = str(args.otio)
    _write_json(args.report, report)
    return report, 0 if report["deviation_ns"] == 0 else 4


def _fixtures(args: argparse.Namespace) -> tuple[dict[str, Any], int]:
    paths = generate_fixtures(args.out, args.seed)
    report = {
        "seed": args.seed,
        "out": str(args.out),
        "files": {name: str(path) for name, path in paths.items()},
        "baseline": baseline(args.seed),
    }
    return report, 0


def build_parser() -> Parser:
    parser = Parser(prog="tlcore", description="Short-form timeline inspector")
    subparsers = parser.add_subparsers(dest="command", required=True)

    fixtures_parser = subparsers.add_parser("fixtures", help="generate deterministic sample data")
    fixtures_parser.add_argument("--seed", type=int, default=FIXTURE_SEED)
    fixtures_parser.add_argument("--out", required=True)
    fixtures_parser.set_defaults(handler=_fixtures)

    check_parser = subparsers.add_parser("check", help="detect and resolve timeline conflicts")
    check_parser.add_argument("--input", required=True)
    check_parser.add_argument("--policy", choices=("ripple", "trim", "drop"), default="ripple")
    check_parser.add_argument("--report")
    check_parser.add_argument("--max-iterations", type=int, default=10_000)
    check_parser.set_defaults(handler=_check)

    render_parser = subparsers.add_parser("render", help="build the frame-layer routing table")
    render_parser.add_argument("--input", required=True)
    render_parser.add_argument("--policy", choices=("ripple", "trim", "drop"), default="ripple")
    render_parser.add_argument("--fps", default="25")
    render_parser.add_argument("--hash")
    render_parser.add_argument("--report")
    render_parser.add_argument("--max-iterations", type=int, default=10_000)
    render_parser.set_defaults(handler=_render)

    export_parser = subparsers.add_parser("export", help="write an OTIO timeline")
    export_parser.add_argument("--input", required=True)
    export_parser.add_argument("--otio", required=True)
    export_parser.add_argument("--report")
    export_parser.set_defaults(handler=_export)
    return parser


def _exit_for_error(exc: Exception) -> int:
    if isinstance(exc, (InvariantError, ResolveError, OTIOBridgeError)):
        return 4
    if isinstance(exc, (TLEError, TimebaseError, ModelError, UsageError, OSError)):
        return 3
    return 4


def _error_payload(exc: Exception) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "ok": False,
        "error": type(exc).__name__,
        "message": str(exc),
    }
    if isinstance(exc, TLEError):
        payload["source"] = exc.source
        if exc.clip_id is not None:
            payload["clip_id"] = exc.clip_id
    if isinstance(exc, NonConvergenceError):
        payload["iterations"] = exc.iterations
        payload["unresolved_count"] = len(exc.remaining)
    return payload


def main(argv: Sequence[str] | None = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        data, code = args.handler(args)
        _emit({"ok": code == 0, **data})
        return code
    except SystemExit:
        raise
    except Exception as exc:
        payload = _error_payload(exc)
        print(json.dumps(payload, ensure_ascii=False, indent=2), file=sys.stderr)
        return _exit_for_error(exc)


if __name__ == "__main__":
    raise SystemExit(main())
