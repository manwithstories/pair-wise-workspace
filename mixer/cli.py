"""Command-line interface. Every command writes exactly one JSON line to stdout."""

from __future__ import annotations

import argparse
import json
import os
from typing import Any, Sequence

import numpy as np
import soundfile as sf

from .engine import Executor
from .errors import (
    EXIT_RENDER,
    ManifestError,
    MixerError,
    MixerIOError,
    RenderError,
)
from .manifest import load_project
from .output import (
    cleanup,
    normalize_wav_metadata,
    prepare_parent,
    publish_pair,
    temp_file,
    write_json_file,
)


def _emit(payload: dict[str, Any]) -> None:
    print(json.dumps(payload, ensure_ascii=False, allow_nan=False, separators=(",", ":")))


def _summary(report: dict[str, Any], **extra: Any) -> dict[str, Any]:
    payload = dict(report)
    payload.update(extra)
    return payload


def command_validate(args: argparse.Namespace) -> int:
    project = load_project(args.manifest)
    graph = project.graph()
    _emit(
        {
            "ok": True,
            "sample_rate": project.sample_rate,
            "channels": project.channels,
            "duration_s": project.duration_s,
            "duration_frames": project.duration_frames,
            "tracks": len(project.tracks),
            "buses": len(project.buses),
            "nodes": len(graph.nodes),
            "warnings": project.warnings,
        }
    )
    return 0


def command_render(args: argparse.Namespace) -> int:
    if os.path.abspath(args.out) == os.path.abspath(args.report or ""):
        raise ManifestError("--out and --report must be different paths")
    prepare_parent(args.out)
    if args.report:
        prepare_parent(args.report)
    project = load_project(args.manifest)
    graph = project.graph()
    result = Executor(project, graph).run(project.duration_frames, collect_audio=True)
    assert result.audio is not None
    wav_temp = temp_file(args.out, ".wav")
    report_temp = temp_file(args.report, ".json") if args.report else None
    try:
        try:
            sf.write(wav_temp, result.audio.T, project.sample_rate, format="WAV", subtype="FLOAT")
        except Exception as exc:
            raise MixerIOError(f"cannot write WAV: {exc}", details={"path": args.out}) from exc
        normalize_wav_metadata(wav_temp)
        report = _summary(result.report, chunks=result.chunks)
        if report_temp:
            write_json_file(report_temp, report)
        publish_pair(wav_temp, args.out, report_temp, args.report)
    finally:
        cleanup(wav_temp, report_temp)
    _emit(report)
    return 0


def command_stream(args: argparse.Namespace) -> int:
    if args.chunk <= 0:
        raise ManifestError("--chunk must be positive")
    if args.out and args.report and os.path.abspath(args.out) == os.path.abspath(args.report):
        raise ManifestError("--out and --report must be different paths")
    if args.out:
        prepare_parent(args.out)
    if args.report:
        prepare_parent(args.report)
    project = load_project(args.manifest)
    graph = project.graph()
    wav_temp = temp_file(args.out, ".wav") if args.out else None
    report_temp = temp_file(args.report, ".json") if args.report else None
    writer: sf.SoundFile | None = None
    try:
        if wav_temp:
            try:
                writer = sf.SoundFile(
                    wav_temp,
                    mode="w",
                    samplerate=project.sample_rate,
                    channels=project.channels,
                    format="WAV",
                    subtype="FLOAT",
                )
            except Exception as exc:
                raise MixerIOError(f"cannot create WAV: {exc}", details={"path": args.out}) from exc

        def sink(block: np.ndarray) -> None:
            assert writer is not None
            writer.write(block.T)

        result = Executor(project, graph).run(
            args.chunk,
            collect_audio=False,
            sink=sink if writer is not None else None,
        )
        if writer is not None:
            writer.close()
            writer = None
            normalize_wav_metadata(wav_temp)
        report = _summary(result.report, chunks=result.chunks, streamed_frames=result.streamed_frames)
        if report_temp:
            write_json_file(report_temp, report)
        publish_pair(wav_temp, args.out, report_temp, args.report)
    finally:
        if writer is not None:
            writer.close()
        cleanup(wav_temp, report_temp)
    _emit(report)
    return 0


def command_parity(args: argparse.Namespace) -> int:
    if args.chunk <= 0:
        raise ManifestError("--chunk must be positive")
    if args.tolerance < 0.0 or not np.isfinite(args.tolerance):
        raise ManifestError("--tolerance must be finite and non-negative")
    project = load_project(args.manifest)
    graph = project.graph()
    offline = Executor(project, graph).run(project.duration_frames, collect_audio=True)
    reference = offline.audio
    assert reference is not None
    state = {"offset": 0, "max": 0.0, "mismatch": False}

    def compare(block: np.ndarray) -> None:
        start = state["offset"]
        stop = start + block.shape[1]
        if stop > reference.shape[1] or block.shape[0] != reference.shape[0]:
            state["mismatch"] = True
            return
        if block.size:
            difference = np.abs(reference[:, start:stop] - block)
            if not np.all(np.isfinite(difference)):
                state["mismatch"] = True
                return
            state["max"] = max(state["max"], float(np.max(difference)))
        state["offset"] = stop

    streamed = Executor(project, graph).run(args.chunk, collect_audio=False, sink=compare)
    if state["offset"] != reference.shape[1]:
        state["mismatch"] = True
    max_diff = float(state["max"])
    ok = not state["mismatch"] and max_diff <= args.tolerance
    payload = {
        "max_abs_diff": max_diff,
        "chunks": streamed.chunks,
        "ok": ok,
        "tolerance": args.tolerance,
        "samples": int(reference.shape[1]),
    }
    _emit(payload)
    return 0 if ok else EXIT_RENDER


def command_report(args: argparse.Namespace) -> int:
    try:
        with open(args.report, "r", encoding="utf-8") as handle:
            report = json.load(handle)
    except FileNotFoundError as exc:
        raise MixerIOError("report file does not exist", details={"path": args.report}) from exc
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise MixerIOError(f"cannot read report: {exc}", details={"path": args.report}) from exc
    if not isinstance(report, dict):
        raise MixerIOError("report root must be an object", details={"path": args.report})
    if args.track is None:
        _emit(report)
        return 0
    tracks = report.get("tracks")
    if not isinstance(tracks, list):
        raise MixerIOError("report does not contain tracks", details={"path": args.report})
    selected = next((item for item in tracks if item.get("id") == args.track), None)
    if selected is None:
        raise ManifestError(f"track {args.track!r} is not present in the report", details={"track": args.track})
    automation = next(
        (
            item
            for item in report.get("automation_tracks", [])
            if item.get("id") == args.track
        ),
        None,
    )
    _emit(
        {
            "track": selected,
            "automation": automation,
            "mix": report.get("mix"),
            "sample_rate": report.get("sample_rate"),
        }
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m mixer.cli", description="Deterministic offline mixer")
    subparsers = parser.add_subparsers(dest="command", required=True)
    validate = subparsers.add_parser("validate")
    validate.add_argument("manifest")
    validate.set_defaults(func=command_validate)
    render = subparsers.add_parser("render")
    render.add_argument("manifest")
    render.add_argument("--out", required=True)
    render.add_argument("--report")
    render.set_defaults(func=command_render)
    stream = subparsers.add_parser("stream")
    stream.add_argument("manifest")
    stream.add_argument("--chunk", type=int, default=4096)
    stream.add_argument("--out")
    stream.add_argument("--report")
    stream.set_defaults(func=command_stream)
    parity = subparsers.add_parser("parity")
    parity.add_argument("manifest")
    parity.add_argument("--chunk", type=int, default=4096)
    parity.add_argument("--tolerance", type=float, default=1e-6)
    parity.set_defaults(func=command_parity)
    report = subparsers.add_parser("report")
    report.add_argument("report")
    report.add_argument("--track")
    report.set_defaults(func=command_report)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except MixerError as exc:
        _emit(exc.payload())
        return exc.code
    except OSError as exc:
        error = MixerIOError(f"file operation failed: {exc}")
        _emit(error.payload())
        return error.code
    except Exception as exc:
        error = RenderError(f"unexpected failure: {type(exc).__name__}: {exc}")
        _emit(error.payload())
        return error.code


if __name__ == "__main__":
    raise SystemExit(main())
