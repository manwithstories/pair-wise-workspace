"""Terminal entry point: ``demo``, ``process`` and ``verify`` subcommands.

Exit codes:

* ``0`` -- every acceptance threshold passed;
* ``1`` -- the batch ran but a threshold was missed;
* ``2`` -- bad arguments, limits or unreadable input paths;
* ``3`` -- unrecoverable media error (no asset could be processed at all).

Only the terminal is used: no window, browser, camera or microphone is opened.
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
import traceback
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Sequence

from . import __version__
from .errors import EXIT_MEDIA, EXIT_OK, EXIT_THRESHOLD, EXIT_USAGE, QCError, UsageError
from .fixtures import MARKERS, build_fixtures
from .pipeline import (
    PipelineOptions,
    iter_assets,
    process_asset,
)
from .report import STATUS_FAILED, BatchReport, load_report, verify_thresholds


@dataclass(slots=True)
class BatchPaths:
    workdir: Path
    inputs: Path
    outputs: Path
    cache: Path


def _paths(workdir: Path) -> BatchPaths:
    workdir = workdir.expanduser().resolve()
    return BatchPaths(
        workdir=workdir,
        inputs=workdir / "inputs",
        outputs=workdir / "outputs",
        cache=workdir / "cache",
    )


def _timestamp() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def run_batch(
    inputs: Path,
    workdir: Path,
    *,
    types: Sequence[str] | None = None,
    reuse: bool = True,
    timeout_s: float = 60.0,
    sync_markers: tuple[float, ...] = (),
    fps: float = 25.0,
    scenario: str = "process",
) -> BatchReport:
    paths = _paths(workdir)
    paths.outputs.mkdir(parents=True, exist_ok=True)
    paths.cache.mkdir(parents=True, exist_ok=True)
    if not inputs.exists():
        raise UsageError(f"input directory does not exist: {inputs}")
    assets = iter_assets(inputs)
    if types:
        allowed = {t.lower() for t in types}
        assets = [p for p in assets if p.suffix.lower().lstrip(".") in allowed]
    if not assets:
        # An empty batch is an operator error, not a failed QC run: exit 2 so
        # a scheduled job notices the wrong path immediately.
        raise UsageError(f"no matching MP4/MOV assets in {inputs}")

    options = PipelineOptions(
        workdir=paths.workdir,
        output_dir=paths.outputs,
        cache_dir=paths.cache,
        reuse=reuse,
        timeout_s=timeout_s,
        sync_markers=sync_markers,
        fps=fps,
    )
    started = _timestamp()
    results = []
    fatal = 0
    for asset in assets:
        outcome = process_asset(asset, options)
        results.append(outcome.result)
        if outcome.exit_fatal or outcome.result.status == STATUS_FAILED:
            fatal += 1
        _print_asset(outcome.result, reused=outcome.reused)
    return BatchReport(
        workdir=paths.workdir,
        results=results,
        started_at=started,
        finished_at=_timestamp(),
        fatal_assets=fatal,
        scenario=scenario,
    )


def _print_asset(result, *, reused: bool) -> None:
    status = result.status.upper()
    sync = "" if result.sync_error_ms is None else f" sync={result.sync_error_ms:.3f}ms"
    rate = "" if result.recovery_rate is None else f" recovery={result.recovery_rate:.3f}"
    match = (
        ""
        if result.remux_packet_match_rate is None
        else f" match={result.remux_packet_match_rate:.3f}"
    )
    cache = " [cache]" if reused else ""
    print(
        f"{status:9s} {result.asset.name:28s} {result.latency_ms:8.1f}ms"
        f"{rate}{match}{sync}{cache}"
    )
    for reason in result.reasons:
        print(f"          - {reason}")
    for error in result.errors:
        print(f"          ! {error.code}: {error}")


def _print_summary(report: BatchReport) -> None:
    summary = report.to_dict()["summary"]
    print("-" * 78)
    print(
        f"assets={summary['assets']} ok={summary['ok']} recovered={summary['recovered']} "
        f"refused={summary['refused']} failed={summary['failed']}"
    )
    print(
        f"sync_error_ms={_fmt(summary['sync_error_ms'])} "
        f"recovery_rate={_fmt(summary['recovery_rate'])} "
        f"p95_latency_ms={_fmt(summary['p95_latency_ms'])} "
        f"remux_packet_match_rate={_fmt(summary['remux_packet_match_rate'])} "
        f"damaged_samples={summary['damaged_samples']}"
    )
    for name, item in report.thresholds().items():
        if item.get("applicable") is False:
            print(f"  [SKIP] {name}: not applicable for this batch")
            continue
        mark = "PASS" if item["passed"] else "FAIL"
        print(
            f"  [{mark}] {name}: {_fmt(item['value'])} "
            f"(threshold {item['mode']} {item['threshold']})"
        )
    print(f"passed={report.all_passed()} exit_code={report.exit_code()}")


def _fmt(value) -> str:
    if value is None:
        return "n/a"
    if isinstance(value, float):
        return f"{value:.4f}".rstrip("0").rstrip(".")
    return str(value)


def cmd_demo(args: argparse.Namespace) -> int:
    workdir = Path(args.workdir).expanduser().resolve()
    workdir.mkdir(parents=True, exist_ok=True)
    if getattr(args, "fresh", False):
        for sub in ("inputs", "outputs", "cache"):
            target = workdir / sub
            if target.exists():
                shutil.rmtree(target)
    print(f"bcastqc {__version__} demo in {workdir}")
    fixtures = build_fixtures(workdir)
    print(f"generated {len(fixtures.as_dict())} fixtures in {workdir / 'inputs'}")
    for name, path in fixtures.as_dict().items():
        print(f"  {name:18s} {Path(path).name} ({Path(path).stat().st_size} bytes)")

    types = args.types if args.types else None
    report = run_batch(
        workdir / "inputs",
        workdir,
        types=types,
        reuse=not args.no_reuse,
        timeout_s=args.timeout,
        sync_markers=MARKERS,
        scenario="demo",
    )
    _print_summary(report)
    json_path = Path(args.json).expanduser().resolve()
    report.write(json_path)
    print(f"report: {json_path}")
    # The refused fixture is expected to be refused, not to fail the run.
    return report.exit_code()


def cmd_process(args: argparse.Namespace) -> int:
    inputs = Path(args.inputs).expanduser().resolve()
    workdir = Path(args.workdir).expanduser().resolve()
    report = run_batch(
        inputs,
        workdir,
        types=args.types,
        reuse=not args.no_reuse,
        timeout_s=args.timeout,
    )
    _print_summary(report)
    if args.json:
        json_path = Path(args.json).expanduser().resolve()
        report.write(json_path)
        print(f"report: {json_path}")
    return report.exit_code()


def cmd_verify(args: argparse.Namespace) -> int:
    path = Path(args.report).expanduser().resolve()
    if not path.exists():
        print(f"verify: report not found: {path}", file=sys.stderr)
        return EXIT_USAGE
    try:
        payload = load_report(path)
    except (OSError, ValueError) as exc:
        print(f"verify: cannot read report: {exc}", file=sys.stderr)
        return EXIT_USAGE
    passed, lines = verify_thresholds(payload)
    summary = payload.get("summary", {})
    print(f"verify {path}")
    print(
        f"assets={summary.get('assets')} damaged_samples={summary.get('damaged_samples')} "
        f"refused={summary.get('refused')} failed={summary.get('failed')}"
    )
    for line in lines:
        print(f"  {line}")
    print(f"passed={passed}")
    if payload.get("passed") != passed:
        print(
            "verify: report was written with a different verdict; "
            "re-evaluation takes precedence",
            file=sys.stderr,
        )
    return EXIT_OK if passed else EXIT_THRESHOLD


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="bcastqc",
        description="Lossless broadcast remux QC for MP4/MOV delivery batches.",
    )
    parser.add_argument("--version", action="version", version=f"bcastqc {__version__}")
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="generate fixtures, process them and write a report")
    demo.add_argument("--workdir", required=True, help="working directory for fixtures and outputs")
    demo.add_argument("--json", required=True, help="path of the JSON report to write")
    demo.add_argument("--fresh", action="store_true", help="remove previous inputs/outputs/cache first")
    demo.add_argument("--no-reuse", action="store_true", help="ignore the resume cache")
    demo.add_argument("--timeout", type=float, default=60.0, help="per-asset timeout in seconds")
    demo.add_argument("--types", nargs="*", help="only process these extensions (mp4, mov)")
    demo.set_defaults(func=cmd_demo)

    process = sub.add_parser("process", help="process an existing delivery directory")
    process.add_argument("--inputs", required=True, help="directory of MP4/MOV assets")
    process.add_argument("--workdir", required=True, help="where outputs and cache are written")
    process.add_argument("--json", help="path of the JSON report to write")
    process.add_argument("--no-reuse", action="store_true", help="ignore the resume cache")
    process.add_argument("--timeout", type=float, default=60.0, help="per-asset timeout in seconds")
    process.add_argument("--types", nargs="*", help="only process these extensions (mp4, mov)")
    process.set_defaults(func=cmd_process)

    verify = sub.add_parser("verify", help="re-check a written report against the thresholds")
    verify.add_argument("--report", required=True, help="JSON report produced by demo/process")
    verify.set_defaults(func=cmd_verify)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:  # argparse uses 2 for usage errors already
        return int(exc.code or 0)
    try:
        return int(args.func(args))
    except UsageError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return exc.exit_code
    except QCError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return EXIT_THRESHOLD
    except Exception:  # pragma: no cover - last-resort diagnostics
        traceback.print_exc()
        return EXIT_MEDIA


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
