"""Terminal UI: analyze / batch / export / report / selftest subcommands."""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

from . import exporter, report, store, wavio
from .pipeline import InvariantError, persist_run, run_pipeline

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_INVARIANT = 3
EXIT_DB = 4


def _dump(obj, path=None, indent=None):
    text = json.dumps(obj, sort_keys=True, indent=indent)
    if path is not None:
        Path(path).write_text(text + "\n", encoding="utf-8")
    return text


def _detect_params(args):
    return {
        "frame_ms": args.frame_ms,
        "min_event_ms": args.min_event_ms,
        "min_silence_ms": args.min_silence_ms,
        "fade_guard_ms": args.fade_guard_ms,
    }


def cmd_analyze(args):
    path = wavio.safe_resolve(args.input)
    if not path.is_file():
        raise wavio.WavFormatError(f"input not found: {args.input}")
    params = _detect_params(args)
    result, segments, silences = run_pipeline(path, params)
    if args.db:
        result["run_id"] = persist_run(
            args.db, path, params, result, segments, silences)
    print(_dump(result, args.json))
    return EXIT_OK


def cmd_batch(args):
    root = wavio.safe_resolve(args.input_dir)
    if not root.is_dir():
        raise wavio.WavFormatError(f"not a directory: {args.input_dir}")
    params = _detect_params(args)
    files = sorted(p for p in root.rglob("*")
                   if p.is_file() and p.suffix.lower() == ".wav")
    entries, errors = [], []
    files_ok = files_failed = segments_total = silence_total = 0
    for path in files:
        try:
            path = wavio.safe_resolve(path, root)
            result, segments, silences = run_pipeline(path, params)
            entry = {"file": str(path),
                     "segment_count": result["segment_count"],
                     "silence_total_samples": result["silence_total_samples"]}
            if args.db:
                entry["run_id"] = persist_run(
                    args.db, path, params, result, segments, silences)
                if args.out_dir:
                    conn = store.open_db(args.db)
                    try:
                        exporter.export_run(
                            conn, entry["run_id"], args.out_dir,
                            pad_ms=args.pad_ms,
                            manifest_name=f"{path.stem}_manifest.json")
                    finally:
                        store.close_db(conn)
                run = dict(result, sample_rate=result["sample_rate"],
                           total_samples=result["total_samples"])
                entry["violations"] = report.check_compliance(
                    run, min_gap_ms=args.min_gap_ms,
                    max_gap_ms=args.max_gap_ms,
                    min_event_ms=args.min_event_ms,
                    margin_ms=args.margin_ms)
            files_ok += 1
            segments_total += result["segment_count"]
            silence_total += result["silence_total_samples"]
            entries.append(entry)
        except (wavio.WavFormatError, InvariantError) as exc:
            files_failed += 1
            errors.append({"file": str(path), "error": str(exc)})
    summary = {
        "files_ok": files_ok,
        "files_failed": files_failed,
        "segments_total": segments_total,
        "silence_total_samples": silence_total,
        "files": entries,
        "errors": errors,
    }
    print(_dump(summary, args.json))
    if args.strict and any(e.get("violations") for e in entries):
        return EXIT_INVARIANT
    return EXIT_OK


def cmd_report(args):
    conn = store.open_db(args.db)
    try:
        run = store.load_run(conn, args.run_id)
    finally:
        store.close_db(conn)
    if run is None:
        raise wavio.WavFormatError(f"run {args.run_id} not found")
    violations = report.check_compliance(
        run, min_gap_ms=args.min_gap_ms, max_gap_ms=args.max_gap_ms,
        min_event_ms=args.min_event_ms, margin_ms=args.margin_ms)
    if args.format == "json":
        print(_dump(report.format_json(run, violations), indent=2))
    else:
        print(report.format_text(run, violations))
    if args.strict and violations:
        return EXIT_INVARIANT
    return EXIT_OK


def cmd_export(args):
    conn = store.open_db(args.db)
    try:
        manifest = exporter.export_run(conn, args.run_id, args.out_dir,
                                       pad_ms=args.pad_ms)
    finally:
        store.close_db(conn)
    print(_dump({"run_id": args.run_id, "file_count": manifest["file_count"],
                 "out_dir": str(args.out_dir)}))
    return EXIT_OK


def cmd_selftest(args):
    fixtures = Path(args.fixtures)
    out_dir = Path(args.out_dir)
    fixtures.mkdir(parents=True, exist_ok=True)
    out_dir.mkdir(parents=True, exist_ok=True)
    script = Path(__file__).resolve().parent.parent / "scripts" / "make_fixtures.py"
    subprocess.run([sys.executable, str(script), "--out", str(fixtures)],
                   check=True)
    db = out_dir / "index.db"
    analyze_json = out_dir / "analyze.json"
    failures = []

    def check(name, ok):
        print(f"{'PASS' if ok else 'FAIL'} {name}")
        if not ok:
            failures.append(name)

    pack = fixtures / "pack_noisy_fade.wav"
    base = ["analyze", "--input", str(pack), "--db", str(db),
            "--frame-ms", "10", "--min-silence-ms", "180",
            "--fade-guard-ms", "30"]
    check("analyze", main(base + ["--json", str(analyze_json)]) == 0)
    repeat = out_dir / "analyze_repeat.json"
    check("analyze repeat", main(base + ["--json", str(repeat)]) == 0)
    check("determinism",
          analyze_json.read_bytes() == repeat.read_bytes())
    data = json.loads(analyze_json.read_text(encoding="utf-8"))
    check("segment count", 10 <= data["segment_count"] <= 60)
    ratio = data["silence_total_samples"] / data["total_samples"]
    check("silence ratio", 0.2 <= ratio <= 0.6)
    batch_json = out_dir / "batch.json"
    rc = main(["batch", "--input-dir", str(fixtures / "batch"),
               "--db", str(db), "--out-dir", str(out_dir / "segments"),
               "--json", str(batch_json), "--strict"])
    check("batch", rc == 0)
    batch = json.loads(batch_json.read_text(encoding="utf-8"))
    check("batch files", batch["files_failed"] == 0 and batch["files_ok"] >= 4)
    check("report", main(["report", "--db", str(db), "--run-id", "1",
                          "--format", "text"]) == 0)
    check("export", main(["export", "--db", str(db), "--run-id", "1",
                          "--out-dir", str(out_dir / "segments")]) == 0)
    manifest = json.loads(
        (out_dir / "segments" / "manifest.json").read_text(encoding="utf-8"))
    check("export count", manifest["file_count"] == data["segment_count"])
    if failures:
        print(f"SELFTEST FAIL: {', '.join(failures)}")
        return EXIT_INVARIANT
    print("SELFTEST PASS")
    return EXIT_OK


def build_parser():
    parser = argparse.ArgumentParser(
        prog="audio-slicer",
        description="Silence-based SFX event slicing, QC and export.")
    sub = parser.add_subparsers(dest="command", required=True)

    def detect_args(p):
        p.add_argument("--frame-ms", type=float, default=10.0)
        p.add_argument("--min-event-ms", type=float, default=150.0)
        p.add_argument("--min-silence-ms", type=float, default=200.0)
        p.add_argument("--fade-guard-ms", type=float, default=30.0)

    def compliance_args(p):
        p.add_argument("--min-gap-ms", type=float,
                       default=report.DEFAULT_MIN_GAP_MS)
        p.add_argument("--max-gap-ms", type=float,
                       default=report.DEFAULT_MAX_GAP_MS)
        p.add_argument("--margin-ms", type=float,
                       default=report.DEFAULT_MARGIN_MS)
        p.add_argument("--strict", action="store_true")

    p = sub.add_parser("analyze", help="analyze one WAV file")
    p.add_argument("--input", required=True)
    p.add_argument("--db")
    p.add_argument("--json")
    detect_args(p)
    p.set_defaults(func=cmd_analyze)

    p = sub.add_parser("batch", help="recursively process a directory")
    p.add_argument("--input-dir", required=True)
    p.add_argument("--db")
    p.add_argument("--out-dir")
    p.add_argument("--json")
    p.add_argument("--pad-ms", type=float, default=exporter.DEFAULT_PAD_MS)
    detect_args(p)
    compliance_args(p)
    p.set_defaults(func=cmd_batch)

    p = sub.add_parser("report", help="report a stored run")
    p.add_argument("--db", required=True)
    p.add_argument("--run-id", type=int, required=True)
    p.add_argument("--format", choices=("text", "json"), default="text")
    p.add_argument("--min-event-ms", type=float,
                   default=report.DEFAULT_MIN_EVENT_MS)
    compliance_args(p)
    p.set_defaults(func=cmd_report)

    p = sub.add_parser("export", help="export single-event WAVs of a run")
    p.add_argument("--db", required=True)
    p.add_argument("--run-id", type=int, required=True)
    p.add_argument("--out-dir", required=True)
    p.add_argument("--pad-ms", type=float, default=exporter.DEFAULT_PAD_MS)
    p.set_defaults(func=cmd_export)

    p = sub.add_parser("selftest", help="generate fixtures and verify pipeline")
    p.add_argument("--fixtures", required=True)
    p.add_argument("--out-dir", required=True)
    p.set_defaults(func=cmd_selftest)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except wavio.WavFormatError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except InvariantError as exc:
        print(f"invariant failure: {exc}", file=sys.stderr)
        return EXIT_INVARIANT
    except store.DatabaseError as exc:
        print(f"database error: {exc}", file=sys.stderr)
        return EXIT_DB
    except (OSError, KeyError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main())
