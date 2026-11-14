"""Dual-threshold hysteresis and hangover speech detector."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .framing import Envelope, noise_floor_db


HIGH_NOISE_MARGIN_DB = 12.0
LOW_HYSTERESIS_DB = 3.0
ABSOLUTE_THRESHOLD_DB = -50.0
DEFAULT_HANGOVER_MS = 250
ALL_VOICED_MEDIAN_DB = -40.0


@dataclass(frozen=True)
class FrameSegment:
    start_frame: int
    end_frame: int


@dataclass
class DetectionResult:
    segments: list[FrameSegment]
    noise_floor: np.ndarray
    high_threshold: np.ndarray
    low_threshold: np.ndarray


def detect_frames(
    level_db: np.ndarray,
    high_threshold: np.ndarray | float,
    low_threshold: np.ndarray | float,
    hangover_frames: int,
) -> list[FrameSegment]:
    """Detect speech frames using high-to-enter and low-to-stay rules."""

    level = np.asarray(level_db, dtype=np.float64)
    high = np.broadcast_to(np.asarray(high_threshold, dtype=np.float64), level.shape)
    low = np.broadcast_to(np.asarray(low_threshold, dtype=np.float64), level.shape)
    keep = max(1, int(hangover_frames))
    segments: list[FrameSegment] = []
    active = False
    start = 0
    last_above_low = 0
    for index, value in enumerate(level):
        if not active:
            if value >= high[index]:
                active = True
                start = index
                last_above_low = index + 1
            continue
        if value >= low[index]:
            last_above_low = index + 1
        elif index - last_above_low >= keep:
            if last_above_low > start:
                segments.append(FrameSegment(start, last_above_low))
            active = False
    if active and last_above_low > start:
        segments.append(FrameSegment(start, last_above_low))
    return segments


def detect_envelope(
    envelope: Envelope,
    noise_margin_db: float = HIGH_NOISE_MARGIN_DB,
    hangover_ms: int = DEFAULT_HANGOVER_MS,
) -> DetectionResult:
    floor = noise_floor_db(envelope.rms, envelope.sample_rate, envelope.frame_samples)
    high = np.maximum(floor + float(noise_margin_db), ABSOLUTE_THRESHOLD_DB)
    low = high - LOW_HYSTERESIS_DB
    hangover_frames = int(round(hangover_ms / envelope.frame_ms))
    level = envelope.db()
    segments = detect_frames(level, high, low, hangover_frames)
    if not segments and level.size and float(np.median(level)) > ALL_VOICED_MEDIAN_DB:
        segments = [FrameSegment(0, int(level.size))]
    return DetectionResult(segments, floor, high, low)
