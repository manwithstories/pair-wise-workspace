"""Command-line entry points and error-code mapping."""
from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
import sys
from typing import Any, Sequence

from .assets import AssetError, generate_fixtures
from .automation import AutomationError
from .graph import GraphError, MixGraph, parse_graph
from .offline import compare_paths, render_offline
from .report import ReportError, build_report, print_summary, write_report
from .resample import ResampleError
from .streaming import StreamingError, stream_project

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_ASSET = 3
EXIT_GRAPH = 4
EXIT_VERIFY = 5


class CliError(ValueError):
    """Raised for invalid command-line values."""


def _nonnegative_int(value: str) -> int:
    try:
        number = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("seed must be an integer") from exc
    if number < 0:
        raise argparse.ArgumentTypeError("seed must be non-negative")
    return number


def _positive_power_of_two(value: str) -> int:
    try:
        number = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("block must be an integer") from exc
    if number <= 0 or number & (number - 1):
        raise argparse.ArgumentTypeError("block must be a positive power of two")
    return number


def _project(path: str | Path) -> tuple[MixGraph, Path]:
    project_path = Path(path)
    if not project_path.is_file():
        raise AssetError(f"project file not found: '{project_path}'")
    try:
        raw = json.loads(project_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise CliError(f"invalid project JSON in '{project_path}': {exc}") from exc
    except OSError as exc:
        raise AssetError(f"cannot read project '{project_path}': {exc}") from exc
    return parse_graph(raw), project_path.parent


def _load(path: str | Path) -> tuple[MixGraph, Path]:
    return _project(path)


def _write_and_summarize(
    graph: MixGraph,
    project_dir: Path,
    report_path: str | Path,
    metrics: dict[str, Any],
) -> None:
    report = build_report(metrics)
    write_report(report_path, report)
    print_summary(report)


def command_gen_fixtures(args: argparse.Namespace) -> int:
    data = generate_fixtures(args.out, args.seed, args.project)
    print(
        f"generated {len(data['tracks'])} tracks, {len(data['buses'])} buses; "
        f"assets={Path(args.out)} project={Path(args.project)}"
    )
    return EXIT_OK


def command_render(args: argparse.Namespace) -> int:
    graph, project_dir = _load(args.project)
    metrics = render_offline(graph, project_dir, args.out)
    _write_and_summarize(graph, project_dir, args.report, metrics)
    print(f"wav={Path(args.out)} report={Path(args.report)}")
    return EXIT_OK


def command_stream(args: argparse.Namespace) -> int:
    graph, project_dir = _load(args.project)
    metrics = stream_project(
        graph,
        project_dir,
        args.block,
        args.out,
        print_metrics=args.print_metrics,
    )
    _write_and_summarize(graph, project_dir, args.report, metrics)
    print(f"wav={Path(args.out)} report={Path(args.report)} block={args.block}")
    return EXIT_OK


def _times(value: str) -> list[float]:
    parts = [part.strip() for part in value.split(",")]
    if not parts or any(not part for part in parts):
        raise CliError("--times must be a comma-separated list of numbers")
    try:
        times = [float(part) for part in parts]
    except ValueError as exc:
        raise CliError("--times must be a comma-separated list of numbers") from exc
    if any(not math.isfinite(time) or time < 0.0 for time in times):
        raise CliError("--times values must be finite and non-negative")
    return times


def command_curves(args: argparse.Namespace) -> int:
    graph, _ = _load(args.project)
    try:
        node = graph.node(args.node)
    except GraphError as exc:
        raise CliError(str(exc)) from exc
    if node.automation is None:
        raise CliError(f"node '{args.node}' has no automation curve")
    times = _times(args.times)
    gains = node.automation.gain_db_at(times)
    samples = [
        {"time": float(time), "gain_db": float(gain)}
        for time, gain in zip(times, gains)
    ]
    print(json.dumps(samples, ensure_ascii=False))
    return EXIT_OK


def command_verify(args: argparse.Namespace) -> int:
    if not math.isfinite(args.tol) or args.tol < 0.0:
        raise CliError("--tol must be a finite non-negative number")
    graph, project_dir = _load(args.project)
    maximum, _ = compare_paths(graph, project_dir)
    passed = maximum <= args.tol
    print(
        json.dumps(
            {
                "max_abs_diff": maximum,
                "pass": passed,
            },
            ensure_ascii=False,
        )
    )
    return EXIT_OK if passed else EXIT_VERIFY


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m mixgraph")
    subparsers = parser.add_subparsers(dest="command", required=True)

    fixtures = subparsers.add_parser("gen-fixtures")
    fixtures.add_argument("--out", required=True)
    fixtures.add_argument("--seed", type=_nonnegative_int, default=7)
    fixtures.add_argument("--project", required=True)
    fixtures.set_defaults(func=command_gen_fixtures)

    render = subparsers.add_parser("render")
    render.add_argument("--project", required=True)
    render.add_argument("--out", required=True)
    render.add_argument("--report", required=True)
    render.set_defaults(func=command_render)

    stream = subparsers.add_parser("stream")
    stream.add_argument("--project", required=True)
    stream.add_argument("--block", type=_positive_power_of_two, default=256)
    stream.add_argument("--out", required=True)
    stream.add_argument("--report", required=True)
    stream.add_argument(
        "--print-metrics", "--metrics-each-block", action="store_true", dest="print_metrics"
    )
    stream.set_defaults(func=command_stream)

    curves = subparsers.add_parser("curves")
    curves.add_argument("--project", required=True)
    curves.add_argument("--node", required=True)
    curves.add_argument("--times", required=True)
    curves.set_defaults(func=command_curves)

    verify = subparsers.add_parser("verify")
    verify.add_argument("--project", required=True)
    verify.add_argument("--tol", type=float, default=1e-6)
    verify.set_defaults(func=command_verify)
    return parser


def _report_error(kind: str, message: str) -> None:
    print(f"error[{kind}]: {message}", file=sys.stderr)


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:
        return int(exc.code) if isinstance(exc.code, int) else EXIT_USAGE
    try:
        return int(args.func(args))
    except CliError as exc:
        _report_error("parameter", str(exc))
        return EXIT_USAGE
    except StreamingError as exc:
        _report_error("parameter", str(exc))
        return EXIT_USAGE
    except GraphError as exc:
        _report_error("graph", str(exc))
        return EXIT_GRAPH
    except AutomationError as exc:
        _report_error("automation", str(exc))
        return EXIT_GRAPH
    except (AssetError, ReportError, ResampleError, OSError) as exc:
        _report_error("asset", str(exc))
        return EXIT_ASSET
    except KeyboardInterrupt:
        _report_error("interrupted", "operation cancelled")
        return 130
