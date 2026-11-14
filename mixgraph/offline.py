"""Whole-render offline path built on the shared block mix engine."""
from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .assets import AssetError
from .graph import MixGraph
from .streaming import MixEngine


def render_offline(
    graph: MixGraph,
    project_dir: str | Path,
    output_path: str | Path,
    chunk_size: int = 4096,
) -> dict[str, Any]:
    if chunk_size <= 0:
        raise ValueError("offline chunk size must be positive")
    output = Path(output_path)
    output.parent.mkdir(parents=True, exist_ok=True)
    engine = MixEngine(graph, project_dir)
    try:
        with sf.SoundFile(
            str(output),
            mode="w",
            samplerate=graph.sample_rate,
            channels=graph.channels,
            subtype="FLOAT",
            format="WAV",
        ) as handle:
            while engine.position < engine.total_frames:
                frames = min(chunk_size, engine.total_frames - engine.position)
                handle.write(engine.process(frames).T)
        engine.finish()
        return engine.metrics()
    except (RuntimeError, OSError) as exc:
        engine.close()
        raise AssetError(f"cannot write offline WAV '{output}': {exc}") from exc


def compare_paths(
    graph: MixGraph,
    project_dir: str | Path,
    offline_block: int = 4096,
    stream_block: int = 256,
) -> tuple[float, int]:
    if offline_block <= 0 or stream_block <= 0:
        raise ValueError("comparison block sizes must be positive")
    offline_engine = MixEngine(graph, project_dir)
    stream_engine = MixEngine(graph, project_dir)
    maximum = 0.0
    compared = 0
    try:
        if offline_engine.total_frames != stream_engine.total_frames:
            return float("inf"), 0
        while offline_engine.position < offline_engine.total_frames:
            remaining = offline_engine.total_frames - offline_engine.position
            offline_frames = min(offline_block, remaining)
            offline_audio = offline_engine.process(offline_frames)
            parts: list[np.ndarray] = []
            needed = offline_frames
            while needed:
                stream_frames = min(stream_block, needed)
                parts.append(stream_engine.process(stream_frames))
                needed -= stream_frames
            stream_audio = np.concatenate(parts, axis=1)
            difference = float(np.max(np.abs(offline_audio - stream_audio)))
            maximum = max(maximum, difference)
            compared += offline_frames
        return maximum, compared
    finally:
        offline_engine.close()
        stream_engine.close()
