"""Command-line interface for audioscan."""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
import tempfile
from pathlib import Path
from typing import Any

from .batch import AnalysisOptions, analyze_file, run_batch
from .ledger import (
    LedgerBusyError,
    connect,
    enqueue,
    init_db,
    set_config,
)
from .report import export_cuts, summary, write_reports
from .riff import RiffError
from .synth import generate_many


DEFAULT_DB = ".audioscan/audioscan.db"
DEFAULT_REPORT_DIR = ".audioscan/reports"


def _json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, separators=(",", ":")))


def _analysis_options(args: argparse.Namespace) -> AnalysisOptions:
    return AnalysisOptions(
        noise_margin_db=float(args.noise_margin),
        hangover_ms=int(args.hangover_ms),
        max_fade_ms=int(args.max_fade_ms),
        merge_gap_ms=int(args.merge_gap_ms),
    )


def _add_detection_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--noise-margin", type=float, default=12.0)
    parser.add_argument("--hangover-ms", type=int, default=250)
    parser.add_argument("--max-fade-ms", type=int, default=600)
    parser.add_argument("--merge-gap-ms", type=int, default=250)


def _db_connection(path: str):
    conn = connect(path)
    init_db(conn)
    return conn


def _cmd_synth(args: argparse.Namespace) -> int:
    generated = generate_many(args.out, args.files, args.seed, args.duration_scale)
    _json({"files": len(generated), "out": os.fspath(Path(args.out)), "seed": args.seed})
    return 0


def _cmd_analyze(args: argparse.Namespace) -> int:
    try:
        result = analyze_file(args.path, _analysis_options(args))
    except RiffError:
        _json({"error": "riff_invalid"})
        return 3
    except OSError:
        _json({"error": "riff_invalid"})
        return 3
    _json(result)
    return 0


def _cmd_batch_enqueue(args: argparse.Namespace) -> int:
    conn = _db_connection(args.db)
    try:
        result = enqueue(conn, args.path)
    finally:
        conn.close()
    _json(
        {
            "scanned": result.scanned,
            "queued": result.queued,
            "skipped": result.skipped,
        }
    )
    return 0


def _cmd_batch_run(args: argparse.Namespace) -> int:
    options = _analysis_options(args)
    result = run_batch(args.db, args.report_out, args.workers, options)
    conn = _db_connection(args.db)
    try:
        for key, value in {
            "noise_margin_db": options.noise_margin_db,
            "hangover_ms": options.hangover_ms,
            "max_fade_ms": options.max_fade_ms,
            "merge_gap_ms": options.merge_gap_ms,
        }.items():
            set_config(conn, key, str(value))
    finally:
        conn.close()
    _json(
        {
            **result.summary,
            "processed": result.done,
            "run_failed": result.failed,
            "interrupted": result.interrupted,
        }
    )
    return 5 if result.failed or result.interrupted else 0


def _cmd_export(args: argparse.Namespace) -> int:
    conn = _db_connection(args.db)
    try:
        path = export_cuts(conn, args.out, args.format)
        count = conn.execute(
            "SELECT COUNT(*) AS n FROM segments s JOIN jobs j ON j.file_id=s.file_id WHERE j.status='done'"
        ).fetchone()["n"]
    finally:
        conn.close()
    _json({"out": os.fspath(path), "format": args.format, "rows": int(count)})
    return 0


def _cmd_report(args: argparse.Namespace) -> int:
    conn = _db_connection(args.db)
    try:
        data = write_reports(conn, args.out)
        current = summary(conn)
    finally:
        conn.close()
    if args.summary:
        _json(current)
    else:
        _json({"out": os.fspath(Path(args.out)), **data})
    return 0


def _selfcheck_one(fixture, options: AnalysisOptions) -> tuple[bool, dict[str, Any]]:
    result = analyze_file(fixture.path, options)
    frame = max(1, int(round(fixture.sample_rate * 0.010)))
    silence_error = abs(int(result["silence_total_samples"]) - int(fixture.silence_samples))
    matched = int(result["segment_count"]) == fixture.segment_count and silence_error <= frame
    return matched, {
        "file": Path(fixture.path).name,
        "matched": matched,
        "segments": int(result["segment_count"]),
        "expected_segments": fixture.segment_count,
        "silence_error_samples": silence_error,
        "tolerance_samples": frame,
    }


def _cmd_selfcheck(args: argparse.Namespace) -> int:
    options = AnalysisOptions(
        noise_margin_db=12.0,
        hangover_ms=250,
        max_fade_ms=600,
        merge_gap_ms=250,
    )
    matched = 0
    details = []
    with tempfile.TemporaryDirectory(prefix="audioscan-selfcheck-") as temporary:
        fixtures = generate_many(temporary, args.files, args.seed)
        for fixture in fixtures:
            try:
                ok, detail = _selfcheck_one(fixture, options)
            except Exception as exc:
                ok, detail = False, {"file": Path(fixture.path).name, "error": str(exc)}
            matched += int(ok)
            details.append(detail)
    print(f"matched={matched}/{len(fixtures)}")
    if matched != len(fixtures):
        _json({"mismatches": [item for item in details if not item.get("matched")]})
        return 6
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="audioscan")
    subparsers = parser.add_subparsers(dest="command", required=True)

    synth = subparsers.add_parser("synth", help="generate deterministic WAV fixtures")
    synth.add_argument("--out", required=True)
    synth.add_argument("--files", type=int, default=12)
    synth.add_argument("--seed", type=int, default=7)
    synth.add_argument("--duration-scale", type=float, default=1.0)
    synth.set_defaults(handler=_cmd_synth)

    analyze = subparsers.add_parser("analyze", help="analyze one WAV")
    analyze.add_argument("path")
    analyze.add_argument("--json", action="store_true")
    _add_detection_options(analyze)
    analyze.set_defaults(handler=_cmd_analyze)

    batch = subparsers.add_parser("batch", help="enqueue or process a directory")
    batch_sub = batch.add_subparsers(dest="batch_command", required=True)
    enqueue_parser = batch_sub.add_parser("enqueue")
    enqueue_parser.add_argument("path")
    enqueue_parser.add_argument("--db", default=DEFAULT_DB)
    enqueue_parser.set_defaults(handler=_cmd_batch_enqueue)
    run_parser = batch_sub.add_parser("run")
    run_parser.add_argument("--db", default=DEFAULT_DB)
    run_parser.add_argument("--workers", type=int, default=4)
    run_parser.add_argument("--report-out", default=DEFAULT_REPORT_DIR)
    _add_detection_options(run_parser)
    run_parser.set_defaults(handler=_cmd_batch_run)

    export = subparsers.add_parser("export", help="export cut manifest")
    export.add_argument("--out", required=True)
    export.add_argument("--format", choices=("csv", "json"), default="csv")
    export.add_argument("--db", default=DEFAULT_DB)
    export.set_defaults(handler=_cmd_export)

    report = subparsers.add_parser("report", help="write and print reports")
    report.add_argument("--db", default=DEFAULT_DB)
    report.add_argument("--out", default=DEFAULT_REPORT_DIR)
    report.add_argument("--summary", action="store_true")
    report.add_argument("--json", action="store_true")
    report.set_defaults(handler=_cmd_report)

    selfcheck = subparsers.add_parser("selfcheck", help="compare detector against synthetic truth")
    selfcheck.add_argument("--seed", type=int, default=7)
    selfcheck.add_argument("--files", type=int, default=12)
    selfcheck.set_defaults(handler=_cmd_selfcheck)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if getattr(args, "files", 1) < 1:
            parser.error("--files must be positive")
        return int(args.handler(args))
    except LedgerBusyError:
        _json({"error": "ledger_busy"})
        return 4
    except (OSError, ValueError, sqlite3.Error) as exc:
        _json({"error": type(exc).__name__, "detail": str(exc)})
        return 1


if __name__ == "__main__":
    sys.exit(main())
