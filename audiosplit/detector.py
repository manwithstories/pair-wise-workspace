"""The single dialogue detector: noise adaptation, hysteresis, and boundaries."""

from __future__ import annotations

from dataclasses import asdict, dataclass

import numpy as np

from . import frames, noise
from .errors import AudioError, UsageError
from .wavio import WavReader


@dataclass(frozen=True)
class DetectorParams:
    frame_ms: float = 20.0
    hop_ms: float = 10.0
    enter_db: float = 6.0
    exit_db: float = 3.0
    min_seg_ms: float = 120.0
    min_sil_ms: float = 250.0
    pad_ms: float = 40.0
    hysteresis_frames: int = 2

    def validate(self) -> None:
        if self.frame_ms <= 0 or self.hop_ms <= 0 or self.hop_ms > self.frame_ms:
            raise UsageError("invalid_parameters", "frame/hop must satisfy 0 < hop <= frame")
        if self.exit_db < 0 or self.enter_db <= self.exit_db:
            raise UsageError("invalid_parameters", "enter-db must be greater than exit-db >= 0")
        if self.min_seg_ms < 0 or self.min_sil_ms < 0 or self.pad_ms < 0:
            raise UsageError("invalid_parameters", "duration parameters cannot be negative")
        if self.hysteresis_frames < 1:
            raise UsageError("invalid_parameters", "hysteresis frames must be at least one")

    def as_dict(self) -> dict:
        return asdict(self)


@dataclass
class Detection:
    analysis: frames.FrameAnalysis
    thresholds: noise.Thresholds
    states: np.ndarray
    segments: list
    silences: list
    dropped_segments: int


def detect_states(
    rms_dbfs: np.ndarray, enter_dbfs: float, exit_dbfs: float, frames_required: int
) -> np.ndarray:
    states = np.zeros(rms_dbfs.size, dtype=bool)
    active = False
    run = 0
    for index, level in enumerate(rms_dbfs):
        if not active:
            run = run + 1 if level >= enter_dbfs else 0
            if run >= frames_required:
                states[max(0, index - frames_required + 1) : index + 1] = True
                active = True
                run = 0
        else:
            states[index] = True
            run = run + 1 if level <= exit_dbfs else 0
            if run >= frames_required:
                states[max(0, index - frames_required + 1) : index + 1] = False
                active = False
                run = 0
    return states


def _rms_envelope(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    window = max(3, int(round(sample_rate * 0.001)))
    half = window // 2
    squared = np.square(samples.astype(np.float64, copy=False))
    prefix = np.concatenate(([0.0], np.cumsum(squared)))
    positions = np.arange(samples.size)
    low = np.clip(positions - half, 0, samples.size)
    high = np.clip(positions + half + 1, 0, samples.size)
    return np.sqrt((prefix[high] - prefix[low]) / np.maximum(high - low, 1))


def _nearest_zero_crossing(samples: np.ndarray, index: int, radius: int) -> int:
    low = max(0, index - radius)
    high = min(samples.size - 1, index + radius)
    if high <= low:
        return index
    section = samples[low : high + 1]
    signs = np.signbit(section)
    crossing = np.flatnonzero((signs[:-1] != signs[1:]) | (section[:-1] == 0.0))
    if crossing.size == 0:
        return index
    candidates = np.unique(np.concatenate((crossing, crossing + 1))) + low
    candidates = candidates[(candidates >= low) & (candidates <= high)]
    return int(candidates[np.argmin(np.abs(candidates - index))])


def refine_boundary(
    reader: WavReader,
    approximate: int,
    *,
    threshold_dbfs: float,
    frame_length: int,
    is_start: bool,
) -> int:
    total = reader.header.total_samples
    if is_start:
        begin = max(0, approximate - frame_length)
        end = min(total, approximate + 2 * frame_length)
    else:
        begin = max(0, approximate - 2 * frame_length)
        end = min(total, approximate + frame_length)
    if end <= begin:
        return max(0, min(approximate, total))
    samples = reader.read_range(begin, end)
    if samples.size == 0:
        return max(0, min(approximate, total))
    envelope = _rms_envelope(samples, reader.header.sample_rate)
    threshold = 10.0 ** (threshold_dbfs / 20.0)
    above = envelope >= threshold
    padded = np.concatenate(([False], above, [False]))
    changes = np.flatnonzero(padded[1:] != padded[:-1])
    sustain = max(1, int(round(reader.header.sample_rate * 0.002)))
    runs = [
        (int(changes[pos]), int(changes[pos + 1]))
        for pos in range(0, changes.size, 2)
        if changes[pos + 1] - changes[pos] >= sustain
    ]
    if runs:
        local = runs[0][0] if is_start else runs[-1][1] - 1
    else:
        local = max(0, min(approximate - begin, samples.size - 1))
    radius = max(1, int(round(reader.header.sample_rate * 0.001)))
    local = _nearest_zero_crossing(samples, local, radius)
    result = begin + local
    if not is_start:
        result += 1
    return max(0, min(result, total))


def detect(reader: WavReader, params: DetectorParams):
    params.validate()
    if reader.header.total_samples <= 1:
        raise AudioError(
            "audio_too_short",
            "empty and single-sample WAV files are not segmentable",
            path=str(reader.path),
        )
    analysis = frames.analyze_reader(reader, params.frame_ms, params.hop_ms)
    floor = noise.estimate_noise_floor(analysis.rms_dbfs, analysis.zcr, params.enter_db)
    thresholds = noise.build_thresholds(floor, params.enter_db, params.exit_db)
    states = detect_states(
        analysis.rms_dbfs,
        thresholds.enter_dbfs,
        thresholds.exit_dbfs,
        params.hysteresis_frames,
    )
    from .segmenter import build_segmentation

    segmentation = build_segmentation(reader, analysis, states, thresholds, params)
    return Detection(
        analysis=analysis,
        thresholds=thresholds,
        states=states,
        segments=segmentation.segments,
        silences=segmentation.silences,
        dropped_segments=segmentation.dropped_segments,
    )

