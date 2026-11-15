"""Command-line entry point for rendering, streaming, and audits."""

from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from mixer.automation import AutomationError
from mixer.engine import render_to_file
from mixer.graph import Graph, GraphError, load_graph
from mixer.io import (
    InputError,
    WriteError,
    resolve_workspace_path,
    validate_sample_rate,
)
from mixer.report import build_report, load_report, print_table, write_report
from mixer.stream import stream_to_file


EXIT_OK = 0
EXIT_USAGE = 2
EXIT_INPUT = 3
EXIT_GRAPH = 4
EXIT_MISMATCH = 5
EXIT_WRITE = 6
EXIT_CLIP = 7


class UsageError(ValueError):
    pass


def _workspace() -> Path:
    return Path.cwd().resolve()


def _input_path(value: str, workspace: Path, *, directory: bool = False) -> Path:
    path = resolve_workspace_path(value, workspace)
    exists = path.is_dir() if directory else path.is_file()
    if not exists:
        kind = "directory" if directory else "file"
        raise InputError(f"input {kind} not found: {value}")
    return path


def _output_path(value: str, workspace: Path) -> Path:
    path = Path(value)
    if not path.is_absolute():
        path = workspace / path
    return path.resolve(strict=False)


def _load_graph(path: Path) -> Graph:
    return load_graph(path)


def _master_rate(args: argparse.Namespace, graph: Graph) -> int:
    value = args.master_sr if getattr(args, "master_sr", None) is not None else graph.master_sr
    if value is None:
        value = 48_000
    return validate_sample_rate(value)


def _positive_block_size(value: int) -> int:
    if value <= 0:
        raise UsageError("block size must be positive")
    return int(value)


def _block_size(args: argparse.Namespace, graph: Graph, default: int) -> int:
    value = args.block_size
    if value is None:
        value = graph.block_size if graph.block_size is not None else default
    return _positive_block_size(value)


def _write_report(path: str | None, report: dict[str, Any], workspace: Path) -> None:
    if path is None:
        return
    target = _output_path(path, workspace)
    try:
        write_report(target, report)
    except OSError as exc:
        raise WriteError(str(exc)) from exc


def _has_clips(report: dict[str, Any]) -> bool:
    if int(report.get("bus", {}).get("clip_count", 0)) > 0:
        return True
    return any(int(track.get("clip_count", 0)) > 0 for track in report["tracks"])


def _finish_render(
    report: dict[str, Any], args: argparse.Namespace, workspace: Path
) -> int:
    print_table(report)
    _write_report(args.report, report, workspace)
    if args.fail_on_clip and _has_clips(report):
        print("error: clipping detected and --fail-on-clip is set", file=sys.stderr)
        return EXIT_CLIP
    return EXIT_OK


def _run_gen_fixtures(args: argparse.Namespace) -> int:
    if not math.isfinite(args.duration) or args.duration <= 0:
        raise UsageError("duration must be a positive finite number")
    from tests.gen_fixtures import generate_fixtures

    workspace = _workspace()
    out_dir = resolve_workspace_path(args.out_dir, workspace)
    try:
        result = generate_fixtures(out_dir, args.duration)
    except (OSError, RuntimeError) as exc:
        raise WriteError(f"cannot generate fixtures: {exc}") from exc
    print(json.dumps(result, indent=2))
    return EXIT_OK


def _run_render(args: argparse.Namespace) -> int:
    workspace = _workspace()
    graph_path = _input_path(args.graph, workspace)
    graph = _load_graph(graph_path)
    master_sr = _master_rate(args, graph)
    block_size = _block_size(args, graph, 1024)
    output_path = _output_path(args.out, workspace)
    result = render_to_file(
        graph,
        output_path,
        master_sr,
        block_size,
        workspace=workspace,
    )
    report = build_report(
        result.session,
        block_size=block_size,
        graph_path=graph_path,
        output_path=result.output_path,
        automation_step=args.automation_step,
    )
    return _finish_render(report, args, workspace)


def _run_stream(args: argparse.Namespace) -> int:
    workspace = _workspace()
    graph_path = _input_path(args.graph, workspace)
    graph = _load_graph(graph_path)
    master_sr = _master_rate(args, graph)
    block_size = _block_size(args, graph, 257)
    if not math.isfinite(args.seek) or args.seek < 0:
        raise UsageError("seek must be a finite non-negative number")
    output_path = _output_path(args.out, workspace)
    result = stream_to_file(
        graph,
        output_path,
        master_sr,
        block_size,
        workspace=workspace,
        seek_sec=args.seek,
    )
    report = build_report(
        result.session,
        block_size=block_size,
        graph_path=graph_path,
        output_path=result.output_path,
        automation_step=args.automation_step,
    )
    return _finish_render(report, args, workspace)


def _compare_files(a_path: Path, b_path: Path) -> dict[str, Any]:
    try:
        a_info = sf.info(str(a_path))
        b_info = sf.info(str(b_path))
    except (OSError, RuntimeError) as exc:
        raise InputError(f"cannot inspect comparison input: {exc}") from exc
    same_format = (
        int(a_info.samplerate) == int(b_info.samplerate)
        and int(a_info.channels) == int(b_info.channels)
    )
    same_length = int(a_info.frames) == int(b_info.frames)
    if not same_format:
        return {
            "identical": False,
            "max_abs_diff": None,
            "first_divergence": 0,
            "frames_a": int(a_info.frames),
            "frames_b": int(b_info.frames),
            "sample_rate_a": int(a_info.samplerate),
            "sample_rate_b": int(b_info.samplerate),
            "channels_a": int(a_info.channels),
            "channels_b": int(b_info.channels),
        }
    max_diff = 0.0
    first_divergence: int | None = None
    position = 0
    overlap = min(int(a_info.frames), int(b_info.frames))
    block_size = 8192
    try:
        with sf.SoundFile(str(a_path), "r") as a_file, sf.SoundFile(
            str(b_path), "r"
        ) as b_file:
            while position < overlap:
                count = min(block_size, overlap - position)
                a_block = a_file.read(count, dtype="float64", always_2d=True)
                b_block = b_file.read(count, dtype="float64", always_2d=True)
                difference = np.abs(a_block - b_block)
                block_max = float(np.max(difference, initial=0.0))
                max_diff = max(max_diff, block_max)
                unequal = np.any(a_block != b_block, axis=1)
                indices = np.flatnonzero(unequal)
                if indices.size and first_divergence is None:
                    first_divergence = position + int(indices[0])
                position += count
    except (OSError, RuntimeError) as exc:
        raise InputError(f"failed comparing audio: {exc}") from exc
    identical = same_length and first_divergence is None and max_diff == 0.0
    if not identical and first_divergence is None:
        first_divergence = overlap
    return {
        "identical": bool(identical),
        "max_abs_diff": float(max_diff),
        "first_divergence": first_divergence,
        "frames_a": int(a_info.frames),
        "frames_b": int(b_info.frames),
    }


def _run_compare(args: argparse.Namespace) -> int:
    workspace = _workspace()
    a_path = _input_path(args.a, workspace)
    b_path = _input_path(args.b, workspace)
    result = _compare_files(a_path, b_path)
    print(json.dumps(result, indent=2, sort_keys=True))
    return EXIT_OK if result["identical"] else EXIT_MISMATCH


def _run_report(args: argparse.Namespace) -> int:
    workspace = _workspace()
    path = _input_path(args.report, workspace)
    try:
        report = load_report(path)
    except (OSError, json.JSONDecodeError) as exc:
        raise InputError(f"cannot read report {path}: {exc}") from exc
    print_table(report)
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m mixer.cli",
        description="Chunked mix rendering and delivery audits",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    fixtures = subparsers.add_parser("gen-fixtures", help="generate audio fixtures")
    fixtures.add_argument("--out-dir", default="fixtures")
    fixtures.add_argument("--duration", type=float, default=6.0)
    fixtures.set_defaults(handler=_run_gen_fixtures)

    for name, handler, default_block in (
        ("render", _run_render, 1024),
        ("stream", _run_stream, 257),
    ):
        command = subparsers.add_parser(name)
        command.add_argument("--graph", required=True)
        command.add_argument("--out", required=True)
        command.add_argument("--master-sr", type=int, default=None)
        command.add_argument("--block-size", type=int, default=None)
        command.add_argument("--report", default=None)
        command.add_argument("--automation-step", type=float, default=0.01)
        command.add_argument("--fail-on-clip", action="store_true")
        if name == "stream":
            command.add_argument("--seek", type=float, default=0.0)
        command.set_defaults(handler=handler)

    compare = subparsers.add_parser("compare", help="compare two rendered files")
    compare.add_argument("--a", required=True)
    compare.add_argument("--b", required=True)
    compare.set_defaults(handler=_run_compare)

    report = subparsers.add_parser("report", help="print an existing JSON report")
    report.add_argument("--report", required=True)
    report.set_defaults(handler=_run_report)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if getattr(args, "automation_step", 0.01) <= 0 or not math.isfinite(
        getattr(args, "automation_step", 0.01)
    ):
        parser.error("--automation-step must be positive and finite")
    try:
        return int(args.handler(args))
    except UsageError as exc:
        print(f"error: invalid parameter: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except GraphError as exc:
        print(f"error: graph validation failed: {exc}", file=sys.stderr)
        return EXIT_GRAPH
    except (InputError, AutomationError, ValueError) as exc:
        print(f"error: invalid input: {exc}", file=sys.stderr)
        return EXIT_INPUT
    except WriteError as exc:
        print(f"error: write failed: {exc}", file=sys.stderr)
        return EXIT_WRITE


if __name__ == "__main__":
    raise SystemExit(main())
