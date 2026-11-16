"""Command-line entry point for scan, timeline, stats and verify."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from .container import MediaOpenError, inspect_container
from .frame_source import build_frame_stream
from .pixel_stats import collect_stats
from .report import (
    ReportError,
    json_text,
    scan_payload,
    stats_payload,
    terminal_summary,
    timeline_payload,
    verify_payload,
    write_report,
)
from .timestamps import TimestampRepairError


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="media_cli", description="Archive media inspection tools")
    subparsers = parser.add_subparsers(dest="command", required=True)
    for name, help_text in (
        ("scan", "summarize container and stream"),
        ("timeline", "emit frame indexes and repaired timeline"),
        ("stats", "decode pixels and compute per-frame statistics"),
        ("verify", "compare timeline and decoded paths"),
    ):
        command = subparsers.add_parser(name, help=help_text)
        command.add_argument("--input", "-i", required=True, help="input media file")
        command.add_argument("--stream", type=int, default=None, help="video stream index")
        command.add_argument("--json", action="store_true", help="write JSON to stdout")
        command.add_argument("--out", type=Path, default=None, help="write JSON or CSV output")
    return parser


def _emit(command: str, payload: dict[str, object], args, result=None) -> None:
    if args.json:
        print(json_text(payload))
    else:
        print(terminal_summary(command, payload))
    if args.out is not None:
        write_report(command, args.out, payload, result)


def _error(command: str, message: str, code: int, as_json: bool) -> int:
    if as_json:
        print(json.dumps({"error": message, "exit_code": code}, ensure_ascii=False))
    else:
        print(f"error: {message}", file=sys.stderr)
    return code


def _run_scan(args) -> int:
    info = inspect_container(args.input, args.stream)
    try:
        stream = build_frame_stream(args.input, args.stream)
    except TimestampRepairError:
        payload = scan_payload(
            info,
            frame_count=0,
            truncated=info.probe_truncated,
            timestamp_status="unrecoverable",
            warnings=["packets contain neither PTS nor DTS"],
        )
        _emit("scan", payload, args)
        return 4
    payload = scan_payload(
        info,
        frame_count=len(stream.frames),
        repaired_timestamps=stream.repair.repaired_timestamps,
        has_b_frames=stream.repair.has_b_frames,
        empty_stream=stream.repair.empty_stream,
        single_frame=stream.repair.single_frame,
        truncated=stream.truncated,
        timestamp_status="repaired" if stream.repair.repaired_timestamps else "ok",
        warnings=stream.warnings,
    )
    _emit("scan", payload, args)
    return 3 if stream.truncated else 0


def _run_timeline(args) -> int:
    stream = build_frame_stream(args.input, args.stream)
    _emit("timeline", timeline_payload(stream), args)
    return 3 if stream.truncated else 0


def _run_stats(args) -> int:
    result = collect_stats(args.input, args.stream)
    _emit("stats", stats_payload(result), args, result)
    return 3 if result.truncated else 0


def _run_verify(args) -> int:
    stream = build_frame_stream(args.input, args.stream)
    result = collect_stats(args.input, args.stream)
    payload = verify_payload(stream, result)
    _emit("verify", payload, args)
    if payload["truncated"]:
        return 3
    if not payload["frame_count_match"] or payload["timeline_mismatch"]:
        return 1
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "scan":
            return _run_scan(args)
        if args.command == "timeline":
            return _run_timeline(args)
        if args.command == "stats":
            return _run_stats(args)
        return _run_verify(args)
    except MediaOpenError as exc:
        return _error(args.command, str(exc), 2, args.json)
    except TimestampRepairError as exc:
        return _error(args.command, str(exc), 4, args.json)
    except ReportError as exc:
        return _error(args.command, str(exc), 2, args.json)
    except OSError as exc:
        return _error(args.command, f"I/O error: {exc}", 2, args.json)


if __name__ == "__main__":
    raise SystemExit(main())
