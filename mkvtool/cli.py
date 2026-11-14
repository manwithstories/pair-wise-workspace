"""Terminal entry point for fixture generation, probing, remux, and verify."""

from __future__ import annotations

import argparse
import json
import os
import sys

from mkvtool.ebml.tree import parse_ebml
from mkvtool.errors import (
    InputParseError,
    MkvToolError,
    TimelineError,
    UnsupportedTrackError,
    VerificationError,
)
from mkvtool.fixtures import generate_fixtures
from mkvtool.mkv.demux import demux_parsed
from mkvtool.mkv.timeline import build_timeline
from mkvtool.mp4.inspect import inspect_mp4_file
from mkvtool.mp4.muxer import mux_timeline
from mkvtool.report.remux_report import build_remux_report, verification_result
from mkvtool.report.track_map import build_track_map
from mkvtool.report.tree_summary import build_tree_summary


def _write_json(path: str | None, payload: dict) -> None:
    if not path:
        return
    parent = os.path.dirname(os.path.abspath(path))
    os.makedirs(parent, exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=False)


def _print_json(payload: dict) -> None:
    print(json.dumps(payload, indent=2, ensure_ascii=False))


def _read(path: str) -> bytes:
    try:
        with open(path, "rb") as handle:
            return handle.read()
    except OSError as exc:
        raise InputParseError(f"cannot read {path}: {exc}") from exc


def command_gen_fixtures(args: argparse.Namespace) -> int:
    fixtures = generate_fixtures(args.out, args.cases)
    print(f"generated {len(fixtures)} fixtures in {args.out}")
    for fixture in fixtures:
        print(f"  {fixture.options.name}: {len(fixture.data)} bytes")
    return 0


def command_probe(args: argparse.Namespace) -> int:
    parse_result = parse_ebml(_read(args.input))
    demux = demux_parsed(parse_result)
    summary = build_tree_summary(parse_result, args.input)
    track_map = build_track_map(demux)
    payload = {
        **summary,
        "track_map": track_map,
        "cues_present": demux.cues_element_present,
        "cue_entries": len(demux.cues),
        "demux_issues": demux.issues,
    }
    _write_json(args.json, payload)
    _print_json(payload)
    return 0


def command_remux(args: argparse.Namespace) -> int:
    input_bytes = _read(args.input)
    parse_result = parse_ebml(input_bytes)
    demux = demux_parsed(parse_result)
    if not any(track.supported for track in demux.tracks):
        raise UnsupportedTrackError("no supported video/audio track can be written as MP4")
    timeline = build_timeline(
        demux,
        gap_threshold_ms=args.gap_ms,
        fill_gaps=args.fill_gaps,
        shift_negative=not args.no_shift,
    )
    mux = mux_timeline(timeline)
    os.makedirs(os.path.dirname(os.path.abspath(args.output)), exist_ok=True)
    with open(args.output, "wb") as handle:
        handle.write(mux.data)
    report = build_remux_report(
        timeline, mux, input_path=args.input, output_path=args.output
    )
    self_check = verification_result(inspect_mp4_file(args.output), report)
    report["self_verification"] = self_check
    _write_json(args.report, report)
    summary = {
        "input": args.input,
        "output": args.output,
        "tracks": report["tracks"],
        "video_frames": report["video_frames"],
        "audio_frames": report["audio_frames"],
        "first_dts_ms": report["first_dts_ms"],
        "rewritten_first_dts_ms": report["rewritten_first_dts_ms"],
        "last_pts_ms": report["last_pts_ms"],
        "keyframes": report["keyframes"],
        "index": report["index"],
        "skipped_tracks": report["source"]["skipped_tracks"],
        "report": args.report,
    }
    _print_json(summary)
    if not all(self_check[key] for key in (
        "frame_count_match", "dts_monotonic", "index_matches", "timestamps_match"
    )):
        raise VerificationError("self-verification failed after muxing")
    return 0


def command_verify(args: argparse.Namespace) -> int:
    try:
        with open(args.report, encoding="utf-8") as handle:
            report = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise VerificationError(f"cannot read report {args.report}: {exc}") from exc
    mp4_info = inspect_mp4_file(args.input)
    result = verification_result(mp4_info, report)
    result["file"] = args.input
    result["report"] = args.report
    _print_json(result)
    required = ("frame_count_match", "dts_monotonic", "index_matches", "timestamps_match")
    if not all(result[key] for key in required):
        raise VerificationError("MP4 verification mismatch")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mkvtool", description="Matroska to MP4 remux toolkit")
    subparsers = parser.add_subparsers(dest="command", required=True)
    gen = subparsers.add_parser("gen-fixtures", help="generate deterministic sample files")
    gen.add_argument("--out", required=True, help="fixture output directory")
    gen.add_argument("--cases", type=int, default=7, choices=range(0, 8), metavar="N")
    gen.set_defaults(func=command_gen_fixtures)
    probe = subparsers.add_parser("probe", help="summarize the EBML tree and track map")
    probe.add_argument("input")
    probe.add_argument("--json", metavar="PATH", help="write JSON summary")
    probe.set_defaults(func=command_probe)
    remux = subparsers.add_parser("remux", help="repair timestamps and write MP4")
    remux.add_argument("input")
    remux.add_argument("-o", "--output", required=True)
    remux.add_argument("--report", metavar="PATH")
    remux.add_argument("--gap-ms", type=float, default=1000.0)
    remux.add_argument("--fill-gaps", action="store_true")
    remux.add_argument("--no-shift", action="store_true")
    remux.set_defaults(func=command_remux)
    verify = subparsers.add_parser("verify", help="reparse MP4 and compare with report")
    verify.add_argument("input")
    verify.add_argument("--report", required=True)
    verify.set_defaults(func=command_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except MkvToolError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return exc.exit_code
    except (OSError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
