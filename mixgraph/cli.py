"""Terminal entry point for mixgraph."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path
import sys

import numpy as np
import soundfile as sf

from .errors import ClipLimitError, ManifestError, MixgraphError, ParityError
from .meters import meter_wav
from .offline import analyze_project, render_offline, write_report
from .project import load_project
from .stream import render_stream


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m mixgraph.cli")
    parser.add_argument("--version", action="version", version="mixgraph 0.1.0")
    commands = parser.add_subparsers(dest="command", required=True)

    generate = commands.add_parser("gen-fixtures", help="generate deterministic fixtures")
    generate.add_argument("--out", default="fixtures")
    generate.add_argument("--seed", type=int, default=7)

    inspect = commands.add_parser("inspect", help="analyze a project blockwise")
    inspect.add_argument("--project", required=True)
    inspect.add_argument("--json", dest="json_path")
    inspect.add_argument("--block", type=int)

    render = commands.add_parser("render", help="render a project to WAV")
    render.add_argument("--project", required=True)
    render.add_argument("--mode", choices=("offline", "stream"), required=True)
    render.add_argument("--block", type=int)
    render.add_argument("--out", required=True)

    compare = commands.add_parser("compare", help="compare two WAV renders")
    compare.add_argument("--a", required=True)
    compare.add_argument("--b", required=True)
    compare.add_argument("--tol", type=float, default=1e-9)
    compare.add_argument("--max-clips", type=int, default=0)
    return parser


def _positive(value: int | None, name: str, default: int) -> int:
    result = default if value is None else value
    if result <= 0:
        raise ManifestError(f"{name} must be a positive integer", name)
    return result


def _cmd_generate(args) -> int:
    from fixtures.gen import generate

    if args.seed < 0:
        raise ManifestError("seed must be a non-negative integer", "seed")
    manifest, files = generate(args.out, args.seed)
    print(f"fixtures: {Path(manifest).parent}")
    print(f"assets: {len(files)}")
    print(f"manifest: {manifest}")
    return 0


def _cmd_inspect(args) -> int:
    project = load_project(args.project)
    block_size = _positive(args.block, "block", project.block_size)
    report = analyze_project(project, block_size)
    if args.json_path:
        target = write_report(report, args.json_path)
        print(f"report: {target}")
    else:
        print(json.dumps(report, indent=2, ensure_ascii=False))
    for track in report["tracks"]:
        print(
            f"track {track['id']}: peak={track['peak_dbfs']:.3f} dBFS "
            f"rms={track['rms_dbfs']:.3f} dBFS clips={track['clip_samples']}"
        )
    print(f"master_peak_dbfs: {report['master_peak_dbfs']:.6f}")
    print(f"clip_total: {report['clip_total']}")
    if report["clip_total"] > project.max_clip_samples:
        raise ClipLimitError(
            f"{report['clip_total']} clipped samples exceed limit "
            f"{project.max_clip_samples}",
            "clip_total",
        )
    return 0


def _cmd_render(args) -> int:
    project = load_project(args.project)
    default_block = project.block_size if args.mode == "offline" else 97
    block_size = _positive(args.block, "block", default_block)
    if args.mode == "offline":
        summary = render_offline(project, args.out, block_size)
    else:
        summary = render_stream(project, args.out, block_size)
    print(f"rendered: {summary['output']}")
    print(f"mode: {args.mode} block={block_size} frames={summary['frames']}")
    print(f"peak_dbfs: {summary['peak_dbfs']:.6f}")
    print(f"clip_total: {summary['clip_samples']}")
    if summary["clip_samples"] > project.max_clip_samples:
        raise ClipLimitError(
            f"{summary['clip_samples']} clipped samples exceed limit "
            f"{project.max_clip_samples}",
            "clip_total",
        )
    return 0


def _max_abs_diff(path_a: str, path_b: str) -> tuple[float, int, int, int]:
    try:
        a_handle = sf.SoundFile(path_a, "r")
    except (OSError, RuntimeError) as exc:
        raise ManifestError(str(exc), "a") from exc
    try:
        b_handle = sf.SoundFile(path_b, "r")
    except (OSError, RuntimeError) as exc:
        a_handle.close()
        raise ManifestError(str(exc), "b") from exc
    with a_handle as a, b_handle as b:
        if (a.samplerate, a.channels, a.frames) != (b.samplerate, b.channels, b.frames):
            raise ParityError(
                "WAV metadata differs: "
                f"a={a.samplerate}Hz/{a.channels}ch/{a.frames}f, "
                f"b={b.samplerate}Hz/{b.channels}ch/{b.frames}f",
                "compare",
            )
        maximum = 0.0
        while True:
            block_a = a.read(4096, dtype="float64", always_2d=True)
            block_b = b.read(4096, dtype="float64", always_2d=True)
            if block_a.shape[0] == 0 or block_b.shape[0] == 0:
                break
            difference = np.abs(block_a - block_b)
            if difference.size:
                maximum = max(maximum, float(np.max(difference)))
        return maximum, a.samplerate, a.channels, a.frames


def _cmd_compare(args) -> int:
    if not math.isfinite(args.tol) or args.tol < 0.0:
        raise ManifestError("tolerance must be a non-negative finite number", "tol")
    if args.max_clips < 0:
        raise ManifestError("max-clips must be non-negative", "max-clips")
    maximum, _, _, _ = _max_abs_diff(args.a, args.b)
    clip_a = meter_wav(args.a).clip_samples
    clip_b = meter_wav(args.b).clip_samples
    clip_total = max(clip_a, clip_b)
    parity_ok = maximum <= args.tol
    print(f"max_abs_diff: {maximum:.15g}")
    print(f"clip_total: {clip_total}")
    print(f"parity_ok: {'true' if parity_ok else 'false'}")
    if not parity_ok:
        return ParityError.exit_code
    if clip_total > args.max_clips:
        return ClipLimitError.exit_code
    return 0


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "gen-fixtures":
            return _cmd_generate(args)
        if args.command == "inspect":
            return _cmd_inspect(args)
        if args.command == "render":
            return _cmd_render(args)
        if args.command == "compare":
            return _cmd_compare(args)
        raise MixgraphError(f"unknown command {args.command!r}", "command")
    except MixgraphError as exc:
        print(f"error: {exc.render()}", file=sys.stderr)
        return exc.exit_code
    except Exception as exc:  # pragma: no cover - terminal safety net
        print(f"unexpected error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

