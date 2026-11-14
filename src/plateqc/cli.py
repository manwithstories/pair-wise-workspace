"""plateqc command line: report / normalize / selfcheck.

Exit codes: 0 ok, 2 bad arguments, 3 input not decodable, 4 strict-mode
unrepairable timestamps, 5 selfcheck output mismatch.
"""

from __future__ import annotations

import argparse
import os
import sys

from . import decoder, metrics, probe, remux, repair, reorder, report

EXIT_OK = 0
EXIT_ARGS = 2
EXIT_UNDECODABLE = 3
EXIT_STRICT = 4
EXIT_MISMATCH = 5


class StrictError(Exception):
    """Strict mode: timestamps cannot be repaired safely (exit code 4)."""


def run_pipeline(input_path: str, stream_index: int | None, strict: bool = False):
    """probe -> decode -> reorder -> repair -> metrics on one FrameTimeline."""
    container, stream, info = probe.probe(input_path, stream_index)
    try:
        decode = decoder.decode_frames(container, stream, info)
    finally:
        container.close()
    if decode.frames_decoded == 0:
        raise probe.ProbeError("no decodable video frames")
    ordered, reorder_warnings = reorder.reorder(
        decode.records, repair.nominal_interval(info)
    )
    timeline = repair.build_timeline(decode, ordered, info, reorder_warnings)
    summary = metrics.summarize(timeline)
    if strict:
        if reorder.REORDER_OVERFLOW in timeline.warnings:
            raise StrictError("reorder window overflow under --strict")
        if timeline.duration_units < 0:
            raise StrictError("negative duration under --strict")
    return timeline, summary


def cmd_report(args) -> int:
    _, summary = run_pipeline(args.input, args.stream, args.strict)
    report.emit_report(summary, args.input, args.json)
    return EXIT_OK


def cmd_normalize(args) -> int:
    timeline, summary = run_pipeline(args.input, args.stream, args.strict)
    try:
        result = remux.normalize(args.input, args.output, timeline, args.stream)
    except OSError as exc:
        print(f"error: cannot write output: {exc}", file=sys.stderr)
        return EXIT_ARGS
    output = {
        "frames": result["frames"],
        "duration_ms": result["duration_ms"],
        "first_pts_ms": result["first_pts_ms"],
        "last_pts_ms": result["last_pts_ms"],
        "ts_repaired": summary["ts_repaired"],
        "max_jump_ms": summary["max_jump_ms"],
        "warnings": summary["warnings"],
    }
    table = report.render_table(
        f"plateqc normalize: {args.input} -> {args.output}",
        output,
        fields=list(output.keys()),
    )
    print(table)
    if args.json:
        report.write_json(output, args.json)
    return EXIT_OK


def cmd_selfcheck(args) -> int:
    os.makedirs(args.work, exist_ok=True)
    timeline, summary = run_pipeline(args.input, args.stream, args.strict)
    report.emit_report(summary, args.input)
    fixed = os.path.join(args.work, "plate_fixed.mp4")
    remux.normalize(args.input, fixed, timeline, args.stream)
    _, check = run_pipeline(fixed, None, False)
    report.emit_report(check, fixed)
    consistent = (
        check["ts_repaired"] == 0
        and check["frames_decoded"] == summary["frames_decoded"]
        and check["duration_ms"] == summary["duration_ms"]
    )
    print("selfcheck: PASS" if consistent else "selfcheck: FAIL")
    return EXIT_OK if consistent else EXIT_MISMATCH


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="plateqc",
        description="Off-line decode QC and timestamp repair for VFX plates.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    def _common(name, help_text):
        cmd = sub.add_parser(name, help=help_text)
        cmd.add_argument("--input", required=True, help="input plate file")
        cmd.add_argument("--stream", type=int, default=None, help="stream index")
        cmd.add_argument("--strict", action="store_true", help="strict mode")
        return cmd

    cmd = _common("report", "decode stats and timestamp QC report")
    cmd.add_argument("--json", default=None, help="write JSON report to path")
    cmd.set_defaults(func=cmd_report)

    cmd = _common("normalize", "remux with repaired timestamps (no re-encode)")
    cmd.add_argument("--output", required=True, help="output MP4 path")
    cmd.add_argument("--json", default=None, help="write JSON report to path")
    cmd.set_defaults(func=cmd_normalize)

    cmd = _common("selfcheck", "report, normalize, re-read and verify consistency")
    cmd.add_argument("--work", required=True, help="work directory for fixed file")
    cmd.set_defaults(func=cmd_selfcheck)
    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except probe.ProbeError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_UNDECODABLE
    except StrictError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_STRICT


if __name__ == "__main__":
    sys.exit(main())
