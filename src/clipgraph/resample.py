"""Fractional-delay sample-rate conversion with deterministic phase mapping."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Callable

import numpy as np

from .model import SourceSpec

SourceReader = Callable[[str, int, int], np.ndarray]
DEFAULT_TAPS = 32


def _sinc(x: np.ndarray) -> np.ndarray:
    out = np.ones_like(x, dtype=np.float64)
    nonzero = np.abs(x) > 1.0e-12
    out[nonzero] = np.sin(np.pi * x[nonzero]) / (np.pi * x[nonzero])
    return out


def _kernel(fraction: np.ndarray, taps: int, cutoff: float = 1.0) -> tuple[np.ndarray, np.ndarray]:
    half = taps // 2
    offsets = np.arange(-half + 1, half + 1, dtype=np.int64)
    distance = fraction[:, None] - offsets[None, :]
    window = np.blackman(taps + 1)[:taps][None, :]
    weights = cutoff * _sinc(cutoff * distance) * window
    weights /= np.sum(weights, axis=1, keepdims=True)
    return offsets, weights


def source_positions(source: SourceSpec, out_start: int, out_end: int, project_rate: int) -> np.ndarray:
    """Map absolute project frames to source sample positions."""
    frames = np.arange(out_start, out_end, dtype=np.float64)
    start_exact = source.start_sec * float(project_rate)
    return (frames - start_exact) * (float(source.sample_rate) / float(project_rate))


def phase_at(source: SourceSpec, out_frame: int, project_rate: int) -> float:
    """Return the retained fractional source phase at an output frame."""
    position = (out_frame - source.start_sec * project_rate) * source.sample_rate / project_rate
    return float(position - np.floor(position))


def resample_source(
    source: SourceSpec,
    out_start: int,
    out_end: int,
    project_rate: int,
    channels: int,
    reader: SourceReader,
    taps: int = DEFAULT_TAPS,
) -> np.ndarray:
    """Render one source interval using a zero-padded windowed-sinc kernel."""
    length = out_end - out_start
    if length <= 0:
        return np.empty((0, channels), dtype=np.float32)
    positions = source_positions(source, out_start, out_end, project_rate)
    half = taps // 2
    need_start = int(np.floor(float(np.min(positions)))) - half + 1
    need_end = int(np.ceil(float(np.max(positions)))) + half + 2
    read_start = max(0, need_start)
    output = np.zeros((length, channels), dtype=np.float64)
    if need_end <= 0 or need_end <= read_start:
        return output.astype(np.float32)
    data = np.asarray(reader(source.path, read_start, need_end - read_start), dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.shape[0] == 0:
        return output.astype(np.float32)
    if data.shape[1] != channels:
        raise ValueError(f"source reader returned {data.shape[1]} channels, expected {channels}")
    base = np.floor(positions).astype(np.int64)
    fraction = positions - base
    cutoff = min(1.0, float(project_rate) / float(source.sample_rate))
    offsets, weights = _kernel(fraction, taps, cutoff)
    indices = base[:, None] + offsets[None, :]
    local = indices - read_start
    valid = (local >= 0) & (local < data.shape[0])
    clipped = np.clip(local, 0, data.shape[0] - 1)
    weights = weights * valid
    for channel in range(channels):
        with np.errstate(invalid="ignore", over="ignore"):
            output[:, channel] = np.sum(data[clipped, channel] * weights, axis=1)
    output[positions < 0.0, :] = 0.0
    return output.astype(np.float32)


@dataclass(frozen=True)
class AlignmentInfo:
    source_id: str
    path: str
    source_rate: int
    project_rate: int
    start_sec: float
    start_exact_samples: float
    start_frame: int
    fractional_delay_samples: float
    rate_ratio: float
    max_alignment_error_samples: float

    def to_dict(self) -> dict[str, float | int | str]:
        return {
            "source_id": self.source_id,
            "path": self.path,
            "source_rate": self.source_rate,
            "project_rate": self.project_rate,
            "start_sec": self.start_sec,
            "start_exact_samples": self.start_exact_samples,
            "start_frame": self.start_frame,
            "fractional_delay_samples": self.fractional_delay_samples,
            "rate_ratio": self.rate_ratio,
            "max_alignment_error_samples": self.max_alignment_error_samples,
        }


def alignment_info(source_id: str, source: SourceSpec, project_rate: int) -> AlignmentInfo:
    exact = source.start_sec * project_rate
    start_frame = int(np.floor(exact))
    return AlignmentInfo(
        source_id=source_id,
        path=source.path,
        source_rate=source.sample_rate,
        project_rate=project_rate,
        start_sec=source.start_sec,
        start_exact_samples=exact,
        start_frame=start_frame,
        fractional_delay_samples=exact - start_frame,
        rate_ratio=source.sample_rate / project_rate,
        max_alignment_error_samples=0.0,
    )


def alignment_summary(graph) -> dict[str, object]:
    rows = [
        alignment_info(node.node_id, node.source, graph.sample_rate)
        for node in graph.nodes
        if node.kind == "source" and node.source is not None
    ]
    return {
        "max_alignment_error_samples": max((row.max_alignment_error_samples for row in rows), default=0.0),
        "max_fractional_delay_samples": max((row.fractional_delay_samples for row in rows), default=0.0),
        "sources": [row.to_dict() for row in rows],
    }
