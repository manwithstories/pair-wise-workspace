"""Terminal entry point: ``demo`` and ``resume`` subcommands.

``demo`` generates fixtures, runs the pipeline for ``--interrupt-after`` nodes
and stops with a consistent checkpoint.  ``resume`` continues from that
checkpoint, verifies the assembled output and prints the JSON report.

Exit codes are the contract:

* ``0`` - every check passed,
* ``2`` - input was damaged, but policy recovered and the report says so,
* other non-zero - bad parameters, dependency graph failure or output check
  failure.

The five acceptance fields are always present in the JSON report and are also
printed as a human-readable block on stdout so a bare terminal run is usable.
"""

from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

from .errors import (
    EXIT_GRAPH,
    EXIT_OK,
    EXIT_OUTPUT,
    EXIT_RECOVERED,
    EXIT_USAGE,
    MediaError,
    ParameterError,
)
from .cli_report import (
    _build_report,
    _exit_for,
    _final_exit,
    _print_error,
    _print_headline,
    _verify_output,
)
from .pipeline import Pipeline
from .pipeline_types import Interrupted, PipelineSettings, RunResult
from .report import Report
from .testdata import generate_fixtures
from .timeline import Timeline

__all__ = ["main", "build_parser"]


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mediacore", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="generate fixtures and run the pipeline")
    demo.add_argument("--generate", action="store_true", help="regenerate fixtures")
    demo.add_argument("--workdir", default=".demo")
    demo.add_argument("--fixtures", default=None, help="fixture dir (default: <workdir>/fixtures)")
    demo.add_argument("--timeline", default=None, help="timeline JSON")
    demo.add_argument("--sample-rate", type=int, default=48_000)
    demo.add_argument("--layout", choices=("mono", "stereo"), default="stereo")
    demo.add_argument("--target-lufs", type=float, default=-16.0)
    demo.add_argument("--interrupt-after", type=int, default=None)
    demo.add_argument("--force", action="store_true", help="ignore existing checkpoint")

    resume = sub.add_parser("resume", help="continue from the checkpoint and verify")
    resume.add_argument("--workdir", default=".demo")
    resume.add_argument("--fixtures", default=None)
    resume.add_argument("--timeline", default=None)
    resume.add_argument("--sample-rate", type=int, default=48_000)
    resume.add_argument("--layout", choices=("mono", "stereo"), default="stereo")
    resume.add_argument("--target-lufs", type=float, default=-16.0)
    resume.add_argument("--verify", action="store_true")
    resume.add_argument("--interrupt-after", type=int, default=None)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "demo":
            return _demo(args)
        return _resume(args)
    except ParameterError as exc:
        _print_error(exc)
        return EXIT_USAGE
    except MediaError as exc:
        _print_error(exc)
        return _exit_for(exc)
    except Exception as exc:  # pragma: no cover - unexpected
        print(f"internal error: {type(exc).__name__}: {exc}", file=sys.stderr)
        return EXIT_USAGE


def _demo(args: argparse.Namespace) -> int:
    workdir = Path(args.workdir).resolve()
    workdir.mkdir(parents=True, exist_ok=True)
    fixtures_dir = Path(args.fixtures).resolve() if args.fixtures else workdir / "fixtures"
    settings = PipelineSettings(
        sample_rate=args.sample_rate,
        layout=args.layout,
        target_lufs=args.target_lufs,
        workdir=str(workdir),
    )

    generate_started = time.perf_counter()
    fixtures = generate_fixtures(
        fixtures_dir,
        sample_rate=args.sample_rate,
        layout=args.layout,
        force=args.generate,
    )
    generate_seconds = time.perf_counter() - generate_started

    timeline_path = Path(args.timeline) if args.timeline else fixtures["timeline_clean"]
    timeline = Timeline.load(timeline_path)

    pipeline = Pipeline(timeline, settings, stop_after=args.interrupt_after)
    result = pipeline.run()

    report = _build_report(
        command="demo",
        timeline=timeline,
        result=result,
        settings=settings,
        workdir=workdir,
    )
    report.phase_timings["generate"] = generate_seconds
    if result.interrupted:
        code = EXIT_OK
    else:
        code = _final_exit(report)
    report.exit_code = code
    report.write(workdir / "report.json")
    _print_headline(report, workdir)
    if result.interrupted:
        # A controlled stop is not a failure; the resume command continues.
        print(f"interrupted after {result.rendered_nodes} nodes; run `resume` next")
    return code


def _resume(args: argparse.Namespace) -> int:
    workdir = Path(args.workdir).resolve()
    if not workdir.exists():
        raise ParameterError("workdir does not exist; run demo first", workdir=str(workdir))
    fixtures_dir = Path(args.fixtures).resolve() if args.fixtures else workdir / "fixtures"
    settings = PipelineSettings(
        sample_rate=args.sample_rate,
        layout=args.layout,
        target_lufs=args.target_lufs,
        workdir=str(workdir),
    )
    timeline_path = Path(args.timeline) if args.timeline else fixtures_dir / "timeline_clean.json"
    timeline = Timeline.load(timeline_path)

    pipeline = Pipeline(timeline, settings, stop_after=args.interrupt_after)
    result = pipeline.run()

    report = _build_report(
        command="resume",
        timeline=timeline,
        result=result,
        settings=settings,
        workdir=workdir,
    )

    if args.verify and not result.interrupted:
        _verify_output(workdir, result, report, timeline, settings)

    code = EXIT_OK if result.interrupted else _final_exit(report)
    report.exit_code = code
    report.write(workdir / "report.json")
    _print_headline(report, workdir)
    if result.interrupted:
        print(f"interrupted after {result.rendered_nodes} nodes")
    return code


















if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
