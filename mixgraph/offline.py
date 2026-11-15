"""Blockwise offline rendering and project analysis."""

from __future__ import annotations

import json
from pathlib import Path

import soundfile as sf

from .errors import ManifestError
from .processor import MixProcessor
from .project import Project, track_automation


def _check_block_size(block_size: int) -> int:
    if isinstance(block_size, bool) or not isinstance(block_size, int) or block_size <= 0:
        raise ManifestError("block size must be a positive integer", "block")
    return block_size


def _iter_spans(total: int, block_size: int):
    for start in range(0, total, block_size):
        yield start, min(block_size, total - start)


def stabilize_wav_header(path: str | Path) -> None:
    """Clear libsndfile's wall-clock timestamp from an optional PEAK chunk."""
    target = Path(path)
    file_size = target.stat().st_size
    with target.open("r+b") as handle:
        header = handle.read(12)
        if len(header) < 12 or header[:4] not in {b"RIFF", b"RIFX"}:
            return
        position = 12
        while position + 8 <= file_size:
            handle.seek(position)
            chunk_header = handle.read(8)
            if len(chunk_header) < 8:
                break
            chunk_id = chunk_header[:4]
            size = int.from_bytes(chunk_header[4:8], "little")
            payload = position + 8
            if chunk_id == b"PEAK" and size >= 8:
                handle.seek(payload + 4)
                handle.write(b"\x00" * 8)
            position = payload + size + (size & 1)


def _render_summary(processor: MixProcessor, output: str, block_size: int) -> dict:
    output_meter = processor.meters[processor.output_node].report()
    return {
        "output": output,
        "sample_rate": processor.project.sample_rate,
        "channels": processor.project.channels,
        "frames": processor.project.duration_frames,
        "block_size": block_size,
        **output_meter,
    }


def render_offline(
    project: Project, out_path: str | Path, block_size: int | None = None
) -> dict:
    block_size = _check_block_size(block_size or project.block_size)
    path = Path(out_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    processor = MixProcessor(project)
    try:
        with sf.SoundFile(
            str(path),
            "w",
            samplerate=project.sample_rate,
            channels=project.channels,
            subtype="FLOAT",
        ) as handle:
            for start, count in _iter_spans(project.duration_frames, block_size):
                handle.write(processor.read_block(start, count))
    finally:
        processor.close()
    stabilize_wav_header(path)
    return _render_summary(processor, str(path), block_size)


def analyze_project(project: Project, block_size: int | None = None) -> dict:
    block_size = _check_block_size(block_size or project.block_size)
    processor = MixProcessor(project)
    try:
        for start, count in _iter_spans(project.duration_frames, block_size):
            processor.read_block(start, count)
        tracks = []
        for track in project.tracks:
            meter = processor.meters[track["gain_node"]].report()
            curve = track_automation(project, track)
            base_db = float(project.graph.nodes[track["gain_node"]].params.get("gain_db", 0.0))
            ratio = processor.track_ratio(track)
            tracks.append(
                {
                    "id": track["id"],
                    "active": track["active"],
                    "peak_dbfs": meter["peak_dbfs"],
                    "rms_dbfs": meter["rms_dbfs"],
                    "clip_samples": meter["clip_samples"],
                    "clip_segments": meter["clip_segments"],
                    "first_clip_sample": meter["first_clip_sample"],
                    "resample_ratio": ratio.as_dict(),
                    "resample_ratio_text": str(ratio),
                    "automation_samples": curve.block_boundary_db(
                        project.duration_frames,
                        block_size,
                        project.sample_rate,
                        base_db,
                    ),
                }
            )
        buses = []
        for node in project.graph.find_by_type("bus"):
            buses.append({"id": node.id, **processor.meters[node.id].report()})
        output_meter = processor.meters[project.output_node].report()
        master_bus_meter = processor.meters[project.master_bus].report()
        return {
            "project": project.name,
            "sample_rate": project.sample_rate,
            "channels": project.channels,
            "duration_sec": project.duration,
            "block_size": block_size,
            "tracks": tracks,
            "buses": buses,
            "master_bus": {
                "id": project.master_bus,
                **master_bus_meter,
            },
            "master_peak_dbfs": output_meter["peak_dbfs"],
            "master_rms_dbfs": output_meter["rms_dbfs"],
            "clip_total": output_meter["clip_samples"],
            "clip_segments": output_meter["clip_segments"],
            "first_clip_sample": output_meter["first_clip_sample"],
        }
    finally:
        processor.close()


def write_report(report: dict, path: str | Path) -> Path:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(
        json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    return target

