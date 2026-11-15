"""Convert detector states into snapped, regularized segments and silences."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from . import frames
from .detector import DetectorParams, refine_boundary
from .noise import Thresholds
from .wavio import WavReader


@dataclass(frozen=True)
class Segment:
    start_sample: int
    end_sample: int
    peak_dbfs: float


@dataclass(frozen=True)
class Silence:
    start_sample: int
    end_sample: int


@dataclass
class Segmentation:
    segments: list[Segment]
    silences: list[Silence]
    dropped_segments: int


def _state_runs(states: np.ndarray) -> list[tuple[int, int]]:
    if states.size == 0:
        return []
    padded = np.concatenate(([False], states, [False]))
    changes = np.flatnonzero(padded[1:] != padded[:-1])
    return [(int(changes[index]), int(changes[index + 1])) for index in range(0, changes.size, 2)]


def build_segmentation(
    reader: WavReader,
    analysis: frames.FrameAnalysis,
    states: np.ndarray,
    thresholds: Thresholds,
    params: DetectorParams,
) -> Segmentation:
    total = analysis.total_samples
    intervals: list[tuple[int, int]] = []
    for first, stop in _state_runs(states):
        start = int(analysis.starts[first])
        end = min(int(analysis.starts[stop - 1]) + analysis.frame_length, total)
        start = refine_boundary(
            reader,
            start,
            threshold_dbfs=thresholds.enter_dbfs,
            frame_length=analysis.frame_length,
            is_start=True,
        )
        end = refine_boundary(
            reader,
            end,
            threshold_dbfs=thresholds.exit_dbfs,
            frame_length=analysis.frame_length,
            is_start=False,
        )
        if stop == states.size:
            end = total
        if end > start:
            intervals.append((start, end))

    min_silence = int(round(analysis.sample_rate * params.min_sil_ms / 1000.0))
    merged: list[list[int]] = []
    for start, end in intervals:
        if merged and start - merged[-1][1] < min_silence:
            merged[-1][1] = max(merged[-1][1], end)
        else:
            merged.append([start, end])

    min_segment = int(round(analysis.sample_rate * params.min_seg_ms / 1000.0))
    kept: list[tuple[int, int]] = []
    dropped = 0
    for start, end in merged:
        if end - start < min_segment:
            dropped += 1
        elif end > start:
            kept.append((start, end))

    pad = int(round(analysis.sample_rate * params.pad_ms / 1000.0))
    kept = [(max(0, start - pad), min(total, end + pad)) for start, end in kept]
    non_overlapping: list[tuple[int, int]] = []
    for start, end in kept:
        if non_overlapping and start <= non_overlapping[-1][1]:
            non_overlapping[-1] = (non_overlapping[-1][0], max(non_overlapping[-1][1], end))
        else:
            non_overlapping.append((start, end))

    segments: list[Segment] = []
    for start, end in non_overlapping:
        peak = 0.0
        for chunk in reader.iter_range(start, end):
            if chunk.size:
                peak = max(peak, float(np.max(np.abs(chunk))))
        segments.append(
            Segment(start_sample=start, end_sample=end, peak_dbfs=float(frames.linear_to_dbfs(peak)))
        )

    silences: list[Silence] = []
    cursor = 0
    for segment in segments:
        if segment.start_sample > cursor:
            silences.append(Silence(cursor, segment.start_sample))
        cursor = max(cursor, segment.end_sample)
    if cursor < total:
        silences.append(Silence(cursor, total))
    if not segments and total > 0:
        silences = [Silence(0, total)]
    return Segmentation(segments=segments, silences=silences, dropped_segments=dropped)

