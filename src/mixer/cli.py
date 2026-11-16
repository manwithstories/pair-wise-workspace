"""Command line surface: generate, render, stream, inspect, compare.

Exit codes: 0 success, 2 argument/input error, 3 fixture format error,
4 offline/streaming mismatch. Every command prints a JSON document to stdout
and a short human summary to stderr, so automation can parse either.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .engine import OfflineEngine, StreamingEngine, MixSession, validate_block
from .errors import (
    EXIT_ARGUMENT,
    EXIT_FORMAT,
    EXIT_MISMATCH,
    EXIT_OK,
    MixerError,
    argument_error,
    format_error,
)
from .graph import build_default_graph
from .io import read_master, summarize
from .model import Project, parse_project


# --------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------
def load_project(path: str | Path, block: int | None = None) -> Project:
    from .io import SourceFile  # local import keeps --help cheap

    project_path = Path(path)
    if not project_path.exists():
        raise argument_error("project_not_found", f"project file not found: {project_path}")
    try:
        raw = json.loads(project_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise argument_error("invalid_project_json", f"cannot parse {project_path.name}: {exc}")
    project = parse_project(raw, base_dir=project_path.parent, default_block=block or 1024)
    if block is not None:
        project.block_size = block
    # Validate every source up front so a bad file is reported by track name
    # before any rendering work happens.
    for track in project.tracks:
        with SourceFile(track.file, track.name, track.layout):
            pass
    return project


def automation_summary(project: Project, block_size: int | None = None) -> dict[str, Any]:
    """Point counts, sampled-edge counts and block-boundary continuity."""
    points = {track.name: len(track.automation) for track in project.tracks}
    gaps: dict[str, float] = {}
    sampled: dict[str, int] = {}
    try:
        with MixSession(project) as session:
            if session.timeline_frames() > 0:
                for size in (256, 1024):
                    session.reset()
                    measured = session.check_continuity(size)
                    sampled[str(size)] = session.automation_sample_count(size)
                    for name, value in measured.items():
                        gaps[f"{name}@{size}"] = round(value, 8)
                sampled["offline"] = session.automation_sample_count(None)
    except MixerError:
        pass
    return {
        "points": points,
        "total": sum(points.values()),
        "sampled_points": sampled,
        "boundary_jump_db": gaps,
    }


def _emit(payload: dict[str, Any], human: str, code: int = EXIT_OK) -> int:
    print(json.dumps(payload, indent=2, sort_keys=False))
    print(human, file=sys.stderr)
    return code


def _error(exc: MixerError) -> int:
    payload = {"ok": False, "error": exc.to_dict()}
    print(json.dumps(payload, indent=2))
    print(f"error[{exc.code}]: {exc}", file=sys.stderr)
    return exc.exit_code


def _db(value: float) -> float | None:
    if value is None or not math.isfinite(value):
        return None
    return round(float(value), 4)


# --------------------------------------------------------------------------
# commands
# --------------------------------------------------------------------------
def cmd_generate(args: argparse.Namespace) -> dict[str, Any]:
    import importlib

    try:
        module = importlib.import_module("fixtures.generate_samples")
    except ModuleNotFoundError:
        raise argument_error(
            "fixtures_missing",
            "fixtures/generate_samples.py is not importable; run from the repo root",
        )
    out_dir = Path(args.out)
    tracks = module.build_project(out_dir, seed=args.seed)
    return {
        "ok": True,
        "command": "generate",
        "out": str(out_dir),
        "seed": args.seed,
        "tracks": [t["name"] for t in tracks],
        "files": sorted(p.name for p in out_dir.glob("*.wav")),
    }


def cmd_render(args: argparse.Namespace) -> dict[str, Any]:
    block = validate_block(args.block, streaming=False)
    project = load_project(args.input, block=block)
    graph = build_default_graph(project, project.bus_map(), project.bus_gains())
    session = MixSession(project, graph, block_size=block)
    try:
        result = session.render(block_size=args.block if args.mode == "stream" else None)
        summary = summarize(_write(args.output, result.audio, session.rate))
        topology = session.summary.to_dict()
        continuity = {
            name: round(value, 8) for name, value in session.check_continuity(block).items()
        }
    finally:
        session.close()
    return {
        "ok": True,
        "command": "render",
        "mode": "offline",
        "tracks": len(project.tracks),
        "target_rate": project.target_rate,
        "frames": int(result.audio.shape[0]),
        "block_size": block,
        "automation": automation_summary(project),
        "topology": topology,
        "continuity_db": continuity,
        "limiter": result.limiter,
        "track_stats": [s.to_dict() for s in result.stats],
        "output": summary,
    }


def cmd_stream(args: argparse.Namespace) -> dict[str, Any]:
    block = validate_block(args.block, streaming=True)
    project = load_project(args.input, block=block)
    engine = StreamingEngine(project, block_size=block)
    result = engine.render()
    summary = summarize(_write(args.output, result.audio, 48000))
    with MixSession(project, block_size=block) as session:
        continuity = {n: round(v, 8) for n, v in session.check_continuity(block).items()}
        topology = session.summary.to_dict()
        plan = engine.plan(session.timeline_frames())
    return {
        "ok": True,
        "command": "stream",
        "mode": "stream",
        "tracks": len(project.tracks),
        "target_rate": project.target_rate,
        "frames": int(result.audio.shape[0]),
        "block_size": block,
        "blocks": len(plan),
        "tail_block": plan[-1].frames if plan else 0,
        "automation": automation_summary(project),
        "topology": topology,
        "continuity_db": continuity,
        "limiter": result.limiter,
        "track_stats": [s.to_dict() for s in result.stats],
        "output": summary,
    }


def cmd_inspect(args: argparse.Namespace) -> dict[str, Any]:
    target = Path(args.path)
    if target.suffix.lower() == ".json":
        project = load_project(target)
        return {
            "ok": True,
            "command": "inspect",
            "kind": "project",
            "tracks": [
                {
                    "name": t.name,
                    "file": Path(t.file).name,
                    "layout": t.layout.name,
                    "channels": t.layout.channels,
                    "start_time": t.start_time,
                    "gain_db": t.gain_db,
                    "automation_points": len(t.automation),
                    "silences": [[s.start, s.end] for s in t.silences],
                    "clips": len(t.clips),
                }
                for t in project.tracks
            ],
            "automation": automation_summary(project),
        }
    return {
        "ok": True,
        "command": "inspect",
        "kind": "audio",
        "audio": summarize(target),
    }


def compare_masters(
    offline_path: Path,
    stream_path: Path,
    project: Project | None,
    max_error: float,
) -> dict[str, Any]:
    offline, off_rate = read_master(offline_path)
    stream, str_rate = read_master(stream_path)
    if off_rate != str_rate:
        raise MixerError(
            "rate_mismatch",
            f"offline is {off_rate} Hz but stream is {str_rate} Hz",
            EXIT_MISMATCH,
            details={"offline_rate": off_rate, "stream_rate": str_rate},
        )
    frames = min(offline.shape[0], stream.shape[0])
    if frames:
        diff = np.abs(offline[:frames] - stream[:frames])
        max_abs = float(diff.max())
        mse = float(np.mean(np.square(diff)))
    else:
        max_abs = 0.0
        mse = 0.0
    frame_delta = int(abs(offline.shape[0] - stream.shape[0]))

    def meter(audio: np.ndarray) -> dict[str, Any]:
        peak = float(np.abs(audio).max()) if audio.size else 0.0
        rms = float(np.sqrt(np.mean(np.square(audio)))) if audio.size else 0.0
        return {
            "frames": int(audio.shape[0]),
            "peak_dbfs": _db(20 * math.log10(peak)) if peak > 0 else None,
            "rms_dbfs": _db(20 * math.log10(rms)) if rms > 0 else None,
            "clip_count": int(np.count_nonzero(np.abs(audio) >= 0.999)),
        }

    automation = {"points": {}, "total": 0} if project is None else automation_summary(project)
    track_stats: list[dict[str, Any]] = []
    if project is not None:
        with MixSession(project) as session:
            session.timeline_frames()
            track_stats = [s.to_dict() for s in session.track_statistics()]
            # Sample counts for each path: offline evaluates 2 edges per track,
            # streaming 2 per block, so density differences are explicit.
            counts = {
                "offline": session.automation_sample_count(None),
                "stream@256": session.automation_sample_count(256),
            }
        automation.setdefault("sampled_points", {}).update(counts)

    matched = max_abs <= max_error and frame_delta == 0
    payload = {
        "ok": matched,
        "command": "compare",
        "max_error": max_error,
        "frames_offline": int(offline.shape[0]),
        "frames_stream": int(stream.shape[0]),
        "frame_delta": frame_delta,
        "max_abs_error": max_abs,
        "mse": mse,
        "automation": automation,
        "offline": meter(offline),
        "stream": meter(stream),
        "track_stats": track_stats,
        "verdict": "match" if matched else "mismatch",
    }
    if not matched:
        raise MixerError(
            "render_mismatch",
            f"verdict=mismatch: offline and stream differ beyond {max_error} "
            f"(max_abs={max_abs:.3e}, frames {offline.shape[0]} vs {stream.shape[0]})",
            EXIT_MISMATCH,
            details=payload,
        )
    return payload


def cmd_compare(args: argparse.Namespace) -> dict[str, Any]:
    project = load_project(args.automation) if args.automation else None
    return compare_masters(Path(args.offline), Path(args.stream), project, args.max_error)


def _write(path: str | Path, audio: np.ndarray, rate: int) -> str:
    from .io import write_master

    write_master(path, audio, rate)
    return str(path)


# --------------------------------------------------------------------------
# parser
# --------------------------------------------------------------------------
def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m src.mixer.cli",
        description="Offline/streaming mix master (48 kHz stereo, shared graph)",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="write sample fixtures and project.json")
    gen.add_argument("--out", required=True, help="fixture output directory")
    gen.add_argument("--seed", type=int, default=17)
    gen.set_defaults(func=cmd_generate)

    render = sub.add_parser("render", help="offline render of the full timeline")
    render.add_argument("--input", required=True, help="project JSON")
    render.add_argument("--output", required=True, help="master WAV path")
    render.add_argument("--rate", type=int, default=48000, choices=[48000])
    render.add_argument("--block", type=int, default=1024)
    render.add_argument("--mode", choices=["offline"], default="offline", help=argparse.SUPPRESS)
    render.set_defaults(func=cmd_render)

    stream = sub.add_parser("stream", help="blockwise streaming render")
    stream.add_argument("--input", required=True, help="project JSON")
    stream.add_argument("--output", required=True, help="master WAV path")
    stream.add_argument("--rate", type=int, default=48000, choices=[48000])
    stream.add_argument("--block", type=int, default=256, choices=[256, 1024])
    stream.set_defaults(func=cmd_stream)

    inspect = sub.add_parser("inspect", help="summarise a project JSON or WAV file")
    inspect.add_argument("--path", required=True)
    inspect.set_defaults(func=cmd_inspect)

    compare = sub.add_parser("compare", help="compare offline and stream masters")
    compare.add_argument("--offline", required=True)
    compare.add_argument("--stream", required=True)
    compare.add_argument("--automation", help="project JSON for automation counts")
    compare.add_argument("--max-error", type=float, default=1e-5)
    compare.set_defaults(func=cmd_compare)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        payload = args.func(args)
    except MixerError as exc:
        return _error(exc)
    except (OSError, ValueError) as exc:
        return _error(argument_error("runtime_error", str(exc)))
    return _emit(payload, _human_line(payload))


def _human_line(payload: dict[str, Any]) -> str:
    command = payload.get("command")
    topology = payload.get("topology")
    if topology and command in ("render", "stream"):
        layers = " | ".join(" ".join(layer) for layer in topology["layers"])
        graph_line = (
            f"graph: {topology['track_nodes']} tracks + {topology['bus_nodes']} buses "
            f"-> master ({topology['edges']} edges)\n  layers: {layers}"
        )
    else:
        graph_line = None
    if command == "generate":
        return f"generated {len(payload['tracks'])} tracks in {payload['out']}"
    if command in ("render", "stream"):
        limiter = payload.get("limiter", {})
        summary = (
            f"{command}: tracks={payload['tracks']} target_rate={payload['target_rate']} "
            f"frames={payload['frames']} peak_dbfs={limiter.get('peak_out_dbfs')} "
            f"rms_dbfs={limiter.get('rms_out_dbfs')} clip_count={limiter.get('clip_count')} "
            f"blocks={payload.get('blocks', 1)}"
        )
        return f"{graph_line}\n{summary}" if graph_line else summary
    if command == "compare":
        return (
            f"compare: max_abs_error={payload['max_abs_error']:.3e} "
            f"frames={payload['frames_offline']}/{payload['frames_stream']} "
            f"verdict={payload['verdict']}"
        )
    return f"{command}: ok"


if __name__ == "__main__":
    sys.exit(main())
