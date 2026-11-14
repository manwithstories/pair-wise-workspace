"""Sample-accurate boundary refinement around detector frame intervals."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .detector import DetectionResult, FrameSegment
from .framing import Envelope, to_db
from .riff import WavReader


FADE_THRESHOLD_DB = 3.0
MONOTONIC_TOLERANCE_DB = 1.0
DEFAULT_MAX_FADE_MS = 600
DEFAULT_MERGE_GAP_MS = 250


@dataclass(frozen=True)
class CutSegment:
    start_sample: int
    end_sample: int
    fade_in_ms: int
    fade_out_ms: int

    @property
    def sample_count(self) -> int:
        return max(0, self.end_sample - self.start_sample)


def _frame_levels(samples: np.ndarray, frame_samples: int) -> np.ndarray:
    if samples.shape[0] == 0:
        return np.empty(0, dtype=np.float32)
    clean = samples.astype(np.float32, copy=True)
    step = max(1, int(frame_samples))
    count = int(np.ceil(clean.shape[0] / step))
    padded = np.zeros((count * step, clean.shape[1]), dtype=np.float32)
    padded[: clean.shape[0]] = clean
    frames = padded.reshape(count, step, clean.shape[1])
    frames -= np.mean(frames, axis=1, keepdims=True, dtype=np.float64).astype(np.float32)
    power = np.mean(frames.astype(np.float64) ** 2, axis=1).max(axis=1)
    return to_db(np.sqrt(power).astype(np.float32))


def _local_floor(envelope: Envelope, center_frame: int, direction: int) -> float:
    if envelope.rms.size == 0:
        return -120.0
    center = min(max(0, int(center_frame)), envelope.rms.size - 1)
    if direction < 0:
        start, end = max(0, center - 100), center + 1
    else:
        start, end = center, min(envelope.rms.size, center + 101)
    power = envelope.rms[start:end].astype(np.float64) ** 2
    value = float(np.percentile(power, 15))
    return max(-120.0, 10.0 * np.log10(max(value, 1e-12)))


def _expand_in(
    reader: WavReader,
    envelope: Envelope,
    detection: DetectionResult,
    segment: FrameSegment,
    max_fade_ms: int,
) -> int:
    total = envelope.total_samples
    detected = min(total, segment.start_frame * envelope.frame_samples)
    frame = envelope.frame_samples
    frame_count = max(0, max_fade_ms // 10)
    first_frame = max(0, segment.start_frame - frame_count)
    base = first_frame * frame
    sample_count = min(total - base, detected - base + frame)
    levels = _frame_levels(reader.read_frames(base, sample_count), frame)
    if levels.size == 0:
        return detected
    floor = _local_floor(envelope, segment.start_frame, -1)
    threshold = floor + FADE_THRESHOLD_DB
    step = frame
    start_index = min(levels.size - 1, max(0, (detected - base) // step))
    anchor = start_index
    for index in range(start_index, levels.size):
        if levels[index] >= threshold:
            anchor = index
            break
    first = anchor
    while first > 0:
        candidate = levels[first - 1]
        current = levels[first]
        if candidate < threshold or candidate > current + MONOTONIC_TOLERANCE_DB:
            break
        first -= 1
    return min(detected, max(base, base + first * step))


def _expand_out(
    reader: WavReader,
    envelope: Envelope,
    detection: DetectionResult,
    segment: FrameSegment,
    max_fade_ms: int,
) -> int:
    total = envelope.total_samples
    detected = min(total, segment.end_frame * envelope.frame_samples)
    frame = envelope.frame_samples
    prefix = frame
    base = max(0, detected - prefix)
    forward = min(total - detected, max(0, max_fade_ms // 10) * frame)
    levels = _frame_levels(reader.read_frames(base, detected - base + forward), frame)
    if levels.size == 0:
        return detected
    floor = _local_floor(envelope, max(0, segment.end_frame - 1), 1)
    threshold = floor + FADE_THRESHOLD_DB
    step = frame
    anchor = min(levels.size - 1, max(0, (detected - base) // step - 1))
    while anchor > 0 and levels[anchor] < threshold:
        anchor -= 1
    last = anchor
    while last + 1 < levels.size:
        candidate = levels[last + 1]
        current = levels[last]
        if candidate < threshold or candidate > current + MONOTONIC_TOLERANCE_DB:
            break
        last += 1
    expanded = base + (last + 1) * step
    return min(total, max(detected, expanded))


def refine_segments(
    path: str,
    envelope: Envelope,
    detection: DetectionResult,
    max_fade_ms: int = DEFAULT_MAX_FADE_MS,
    merge_gap_ms: int = DEFAULT_MERGE_GAP_MS,
) -> list[CutSegment]:
    """Expand fades from raw samples and merge adjacent short gaps."""

    max_fade_ms = max(0, int(max_fade_ms))
    merge_gap_samples = max(0, int(round(merge_gap_ms * envelope.sample_rate / 1000.0)))
    total = envelope.total_samples
    refined: list[CutSegment] = []
    with WavReader(path) as reader:
        for frame_segment in detection.segments:
            start = max(0, frame_segment.start_frame * envelope.frame_samples)
            end = min(total, frame_segment.end_frame * envelope.frame_samples)
            if end <= start:
                continue
            expanded_start = _expand_in(reader, envelope, detection, frame_segment, max_fade_ms)
            expanded_end = _expand_out(reader, envelope, detection, frame_segment, max_fade_ms)
            fade_in = int(round((start - expanded_start) * 1000.0 / envelope.sample_rate))
            fade_out = int(round((expanded_end - end) * 1000.0 / envelope.sample_rate))
            refined.append(
                CutSegment(
                    start_sample=expanded_start,
                    end_sample=max(start + 1, expanded_end),
                    fade_in_ms=min(max_fade_ms, max(0, fade_in)),
                    fade_out_ms=min(max_fade_ms, max(0, fade_out)),
                )
            )
    if not refined:
        return []
    merged: list[CutSegment] = [refined[0]]
    for current in refined[1:]:
        previous = merged[-1]
        gap = current.start_sample - previous.end_sample
        if gap <= merge_gap_samples:
            merged[-1] = CutSegment(
                start_sample=previous.start_sample,
                end_sample=max(previous.end_sample, current.end_sample),
                fade_in_ms=previous.fade_in_ms,
                fade_out_ms=current.fade_out_ms,
            )
        else:
            merged.append(current)
    return merged
