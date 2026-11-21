"""Terminal entry point for the archival media pipeline.

Subcommands:
* ``demo``  – generate the synthetic fixture set, run the pipeline twice to
  exercise checkpoint resume, emit a JSONL manifest and print the required
  acceptance metrics.
* ``run``   – process an existing directory of local MP4/MOV files.

The interface is terminal-only: all behavior is driven by arguments and
printed output, with no graphical or network dependency.  Exit codes:
0 = completed and metrics passed, 2 = input corruption recovered and
reported, 1 = fatal failure or a metric outside tolerance.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

from . import EXIT_FATAL, EXIT_OK, EXIT_RECOVERED
from .errors import FatalMediaError
from .index import MediaIndex
from .pipeline import Pipeline, PipelineConfig

# Metrics the acceptance contract requires on the terminal output.
METRIC_KEYS = (
    "target_kbps",
    "actual_kbps",
    "bitrate_delta_pct",
    "video_frames",
    "audio_rms",
    "silence_count",
    "duplicate_count",
    "manifest_alignment_ms",
    "checkpoint_resumed",
)


def _write_manifest(manifest_path: str, results) -> None:
    """Write deterministic, stable-order JSONL manifest rows."""
    rows: list[dict] = []
    for result in sorted(results, key=lambda r: r.metrics.source):
        header = {
            "record": "file",
            "source": os.path.basename(result.metrics.source),
            "metrics": result.metrics.to_dict(),
            "corrections": result.corrections,
        }
        rows.append(header)
        rows.extend(result.manifest_rows)
    with open(manifest_path, "w", encoding="utf-8") as handle:
        for row in rows:
            handle.write(json.dumps(row, sort_keys=True, ensure_ascii=False))
            handle.write("\n")


def _print_metrics(results) -> None:
    header = f"{'file':16s} " + " ".join(f"{k:>20s}" for k in METRIC_KEYS)
    print(header)
    for result in sorted(results, key=lambda r: r.metrics.source):
        m = result.metrics.to_dict()
        line = f"{os.path.basename(m['source']):16s} " + " ".join(
            f"{str(m[k]):>20s}" for k in METRIC_KEYS
        )
        print(line)


def _build_demo_fixtures(workdir: str, target_kbps: int):
    # Imported lazily so the package itself has no test-only import at runtime.
    fixtures_root = os.path.join(workdir, "fixtures")
    repo_root = Path(__file__).resolve().parents[2]
    tests_dir = str(repo_root / "tests")
    if tests_dir not in sys.path:
        sys.path.insert(0, tests_dir)
    from fixtures import build_fixtures  # type: ignore

    return build_fixtures(fixtures_root, target_kbps=target_kbps)


def cmd_demo(args: argparse.Namespace) -> int:
    workdir = Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    db_path = str(workdir / "archive_index.db")
    manifest_path = str(workdir / "manifest.jsonl")

    fixture_set = _build_demo_fixtures(str(workdir), args.target_kbps)
    wrap_periods = {
        case.name: case.wrap_period_ticks
        for case in fixture_set.cases
        if case.wrap_period_ticks
    }
    config = PipelineConfig(
        chunk_size=args.chunk_size,
        target_kbps=args.target_kbps,
    )

    last_results = None
    # Run twice; the second invocation resumes from committed checkpoints.
    for pass_no in (1, 2):
        with MediaIndex(db_path) as index:
            pipeline = Pipeline(config, index)
            try:
                results = pipeline.run_batch(
                    fixture_set.cases, wrap_periods=wrap_periods
                )
            except FatalMediaError as exc:
                print(f"FATAL: {exc.code}: {exc.message}", file=sys.stderr)
                return EXIT_FATAL
        last_results = results
        resumed = sum(1 for r in results if r.metrics.checkpoint_resumed)
        print(f"pass {pass_no}: {len(results)} files, {resumed} resumed from checkpoint")
        if not args.resume:
            break

    assert last_results is not None
    _write_manifest(manifest_path, last_results)
    _print_metrics(last_results)
    print(f"\nmanifest written to {manifest_path}")
    if hasattr(Pipeline, "_duplicate_pairs"):
        pass

    exit_code, failures = Pipeline.aggregate_exit(last_results)
    for failure in failures:
        print(f"METRIC FAILURE: {failure}", file=sys.stderr)
    status_label = {EXIT_OK: "ok", EXIT_RECOVERED: "recovered", EXIT_FATAL: "fatal"}
    print(f"exit_status={status_label[exit_code]} ({exit_code})")
    return exit_code


def cmd_run(args: argparse.Namespace) -> int:
    input_dir = Path(args.input_dir)
    if not input_dir.is_dir():
        print(f"FATAL: input directory not found: {input_dir}", file=sys.stderr)
        return EXIT_FATAL
    paths = sorted(
        str(p)
        for p in input_dir.glob("*")
        if p.suffix.lower() in (".mp4", ".mov")
    )
    if not paths:
        print("FATAL: no .mp4/.mov files found", file=sys.stderr)
        return EXIT_FATAL

    class _Case:
        def __init__(self, path: str) -> None:
            self.name = Path(path).stem
            self.path = path

    db_path = str(Path(args.workdir) / "archive_index.db")
    Path(args.workdir).mkdir(parents=True, exist_ok=True)
    config = PipelineConfig(chunk_size=args.chunk_size, target_kbps=args.target_kbps)
    with MediaIndex(db_path) as index:
        pipeline = Pipeline(config, index)
        try:
            results = pipeline.run_batch([_Case(p) for p in paths])
        except FatalMediaError as exc:
            print(f"FATAL: {exc.code}: {exc.message}", file=sys.stderr)
            return EXIT_FATAL
    manifest_path = str(Path(args.workdir) / "manifest.jsonl")
    _write_manifest(manifest_path, results)
    _print_metrics(results)
    exit_code, failures = Pipeline.aggregate_exit(results)
    for failure in failures:
        print(f"METRIC FAILURE: {failure}", file=sys.stderr)
    return exit_code


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="archive-media", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="generate fixtures and run the acceptance demo")
    demo.add_argument("--workdir", required=True)
    demo.add_argument("--chunk-size", type=int, default=4)
    demo.add_argument("--target-kbps", type=int, default=900)
    demo.add_argument("--resume", action="store_true", help="run twice to exercise resume")
    demo.set_defaults(func=cmd_demo)

    run = sub.add_parser("run", help="process a directory of local MP4/MOV files")
    run.add_argument("--input-dir", required=True)
    run.add_argument("--workdir", required=True)
    run.add_argument("--chunk-size", type=int, default=4)
    run.add_argument("--target-kbps", type=int, default=900)
    run.set_defaults(func=cmd_run)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
