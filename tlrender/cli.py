"""Command-line entry point: subcommands, exit codes and exception mapping."""
from __future__ import annotations

import argparse
import datetime as _dt
import json
import sys
from dataclasses import dataclass, field

from . import MAX_CUE_MS
from .compose import render_first_frame
from .digest import (
    build_records,
    compare_manifest,
    frame_sha256,
    load_manifest,
    write_manifest,
    write_pngs,
)
from .overlap import count_dropped, count_resolved, resolve_overlaps
from .report import json_lines, print_table, summary_line
from .tl_parse import (
    Cue,
    TimelineError,
    load_timeline,
)
from .transition import apply_transitions


@dataclass
class RunResult:
    cues: list[Cue] = field(default_factory=list)
    hashes: dict[str, str] = field(default_factory=dict)
    summary: dict = field(default_factory=dict)
    warnings: list = field(default_factory=list)
    manifest_path: str | None = None


def _merge(results) -> tuple[list[Cue], list, list]:
    cues: list[Cue] = []
    transitions = []
    warnings = []
    seen: set[str] = set()
    for result in results:
        warnings.extend(result.diagnostics)
        transitions.extend(result.transitions)
        for cue in result.cues:
            if cue.id in seen:
                raise TimelineError(f"duplicate cue id across inputs: {cue.id}")
            seen.add(cue.id)
            cues.append(cue)
    return cues, transitions, warnings


def run_pipeline(
    timeline_paths: list[str],
    out_dir: str,
    use_timestamp: bool = True,
) -> RunResult:
    parsed = [load_timeline(path) for path in timeline_paths]
    too_long = [
        diag
        for result in parsed
        for diag in result.diagnostics
        if diag.code == "cue_too_long"
    ]
    cues, transitions, warnings = _merge(parsed)

    cues = resolve_overlaps(cues)
    cues = apply_transitions(cues, transitions)
    cues.sort(key=lambda c: (c.track, c.final_start, c.id))

    frames = [render_first_frame(cue) for cue in cues]
    hashes = {frame.cue_id: frame_sha256(frame) for frame in frames}

    previous = load_manifest(out_dir)
    mismatch = compare_manifest(previous, hashes)
    write_pngs(frames, out_dir)

    timestamp = (
        _dt.datetime.now(_dt.timezone.utc).isoformat(timespec="seconds")
        if use_timestamp
        else None
    )
    summary = {
        "total": len(cues),
        "resolved": count_resolved(cues),
        "dropped": count_dropped(cues),
        "frames": len(frames),
        "hash_mismatch": mismatch,
        "warnings": len(warnings),
        "inputs": [str(path) for path in timeline_paths],
    }
    records = build_records(cues, hashes)
    manifest_path = write_manifest(out_dir, records, summary, timestamp)

    if too_long:
        raise _CueTooLongAfterWrite(too_long, RunResult(cues, hashes, summary, warnings, str(manifest_path)))
    return RunResult(cues, hashes, summary, warnings, str(manifest_path))


class _CueTooLongAfterWrite(TimelineError):
    exit_code = 2

    def __init__(self, diagnostics, partial: RunResult):
        self.diagnostics = diagnostics
        self.partial = partial
        details = "; ".join(
            f"{d.source}:{d.line}: {d.message}" for d in diagnostics
        )
        super().__init__(f"cue exceeds {MAX_CUE_MS} ms limit: {details}")


def _cmd_run(args: argparse.Namespace) -> int:
    paths = [args.timeline]
    if args.legacy:
        paths.append(args.legacy)
    try:
        result = run_pipeline(paths, args.out, use_timestamp=not args.no_timestamp)
    except _CueTooLongAfterWrite as exc:
        for diag in exc.diagnostics:
            print(f"warning: {diag.source}:{diag.line}: {diag.message}", file=sys.stderr)
        print(str(exc), file=sys.stderr)
        return exc.exit_code

    for warning in result.warnings:
        print(
            f"warning: {warning.source}:{warning.line}: {warning.code}: {warning.message}",
            file=sys.stderr,
        )
    if args.json:
        json_lines(result.cues, result.hashes)
    else:
        print_table(result.cues, result.hashes)
    summary_line(result.summary)
    if args.json_summary:
        print(json.dumps(result.summary, sort_keys=True))
    return 0


def _cmd_gen_fixtures(args: argparse.Namespace) -> int:
    from .fixtures_gen import generate_fixtures

    generated = generate_fixtures(args.dir)
    for path in generated:
        print(path)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="tlrender", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    run = sub.add_parser("run", help="resolve a timeline and render first frames")
    run.add_argument("--timeline", required=True, help="current v2 timeline (.tl)")
    run.add_argument("--legacy", help="legacy v1 timeline (.tl)")
    run.add_argument("--out", required=True, help="output directory")
    run.add_argument("--json", action="store_true", help="emit JSON lines")
    run.add_argument("--json-summary", action="store_true", help="also emit JSON summary")
    run.add_argument("--no-timestamp", action="store_true", help="omit manifest timestamp")
    run.set_defaults(func=_cmd_run)

    gen = sub.add_parser("gen-fixtures", help="generate sample timelines and expectations")
    gen.add_argument("--dir", default="fixtures")
    gen.set_defaults(func=_cmd_gen_fixtures)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except TimelineError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return getattr(exc, "exit_code", 1)
    except OSError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
