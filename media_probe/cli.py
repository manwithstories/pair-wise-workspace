"""Terminal entry point: fixtures / scan / repair subcommands.

Exit codes: 0 = all checks passed; 2 = over-threshold jumps remain after
repair; 3 = file undecodable or video stream missing/unsupported.
"""

from __future__ import annotations

import argparse
import sys

from . import export_ts, fixtures_gen, framewalk, probe as probe_mod
from . import reorder, repair as repair_mod, report, stats

EXIT_OK = 0
EXIT_JUMPS = 2
EXIT_UNDECODABLE = 3


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="media_probe",
        description="Frame-level timestamp audit and repair planner (H.264/HEVC).",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_fixtures = sub.add_parser("fixtures", help="generate sample clips with defects")
    p_fixtures.add_argument("--out", default="fixtures", help="output directory")

    for name in ("scan", "repair"):
        p = sub.add_parser(name, help=f"{name} a media file")
        p.add_argument("--input", required=True, help="media file path")
        p.add_argument(
            "--jump-threshold",
            type=float,
            default=1.5,
            help="jump threshold in nominal frame intervals (default: 1.5)",
        )
        p.add_argument("--json", help="write the JSON manifest to this path")
    sub.choices["repair"].add_argument(
        "--export", help="write repaired timestamps to a CSV sidecar"
    )
    return parser


def _fallback_interval(probe: probe_mod.ProbeResult) -> int | None:
    if probe.nominal_fps and probe.time_base:
        return max(1, round(1.0 / (probe.nominal_fps * float(probe.time_base))))
    return None


def run_pipeline(input_path: str, jump_threshold: float, mode: str):
    """Shared scan/repair pipeline; returns (manifest, repair_result, exit_code)."""
    probe = probe_mod.probe(input_path)
    walk = framewalk.walk(
        input_path,
        probe.stream_index,
        duration_sec=probe.duration_sec,
        time_base=probe.time_base,
    )
    if not walk.frames:
        raise probe_mod.ProbeError("no decodable video frames")
    ordered = reorder.display_order(walk.frames)
    repair = repair_mod.repair_timeline(
        ordered,
        jump_threshold=jump_threshold,
        time_base=probe.time_base,
        fallback_interval=_fallback_interval(probe),
    )
    type_counts = stats.frame_type_counts(walk.frames)
    gops = stats.gop_lengths(repair.rows)
    rate = stats.rate_analysis(repair.rows, probe.time_base)
    manifest = report.build_manifest(
        mode, probe, walk, repair, type_counts, gops, rate
    )
    exit_code = EXIT_JUMPS if repair.jumps else EXIT_OK
    return manifest, repair, exit_code


def _cmd_scan_or_repair(args, mode: str) -> int:
    try:
        manifest, repair, exit_code = run_pipeline(args.input, args.jump_threshold, mode)
    except probe_mod.ProbeError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_UNDECODABLE
    if mode == "repair" and args.export:
        export_ts.export_csv(repair, args.export)
        manifest["exported_csv"] = args.export
    if args.json:
        report.write_json(manifest, args.json)
    report.print_summary(manifest)
    for warning in manifest["warnings"]:
        print(f"warning: {warning}", file=sys.stderr)
    return exit_code


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.command == "fixtures":
        written = fixtures_gen.generate_all(args.out)
        print(f"generated {len(written)} fixtures under {args.out.rstrip('/')}/")
        for path in written:
            print(f"  {path}")
        return EXIT_OK
    return _cmd_scan_or_repair(args, args.command)


if __name__ == "__main__":
    sys.exit(main())
