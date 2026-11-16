"""The detection algorithm: noise floor, hysteresis, ramps, pulse handling.

Pipeline per file (never across files):

1. per-frame RMS in dB, lightly smoothed
2. estimate the background noise floor from a low percentile
3. two thresholds -- absolute ``threshold_db`` and ``noise_floor + margin``
4. hysteresis: enter on the high threshold, leave on the low one
5. ramp compensation: extend boundaries along a monotone energy trend so a
   slow fade is not mistaken for silence
6. bridge gaps shorter than both ``max_pulse_ms`` and ``min_silence_ms``
7. merge gaps shorter than ``min_silence_ms``
8. drop segments shorter than ``min_segment_ms``
9. pad boundaries, then derive silence as the exact complement

Because silence is the complement, segments and silences always partition
``[0, total_samples)`` and verification passes by construction.
"""

from __future__ import annotations

import math
from typing import Callable, Iterable, Iterator, Sequence

import numpy as np

from . import regions as reg
from . import signal as sig
from .model import DetectionConfig, DetectionResult, Segment, SilenceRegion
from .regions import Interval

BlockSource = Callable[[], Iterable[np.ndarray]]

__all__ = [
    "Interval",
    "detect_array",
    "detect_from_features",
    "detect_stream",
    "estimate_noise_floor",
    "compute_thresholds",
    "hysteresis_mask",
    "apply_ramp_compensation",
    "exact_region_stats",
]


def estimate_noise_floor(db: np.ndarray, percentile: float) -> float:
    """Background level from a low percentile of all analysis frames.

    Digital silence is included on purpose: when the quiet passages are true
    zeros the floor is the dB floor, the relative threshold falls away, and
    the absolute ``threshold_db`` governs.  That is the correct behaviour --
    real silence should never *raise* the bar for what counts as content.
    """

    if db.size == 0:
        return sig.DB_FLOOR
    floor = float(np.percentile(db, percentile))
    if not math.isfinite(floor):
        return sig.DB_FLOOR
    return max(sig.DB_FLOOR, floor)


def audible_floor(db: np.ndarray, percentile: float) -> float:
    """Low percentile ignoring digital zero frames.

    Used only as the lower bound of the ramp gate.  With a floor of -120 dB
    the gate would sink below ordinary room tone and every steady noise floor
    would look like a fade.  If nothing is audible, the dB floor is returned.
    """

    if db.size == 0:
        return sig.DB_FLOOR
    audible = db[db > sig.DB_FLOOR + 0.5]
    if audible.size == 0:
        return sig.DB_FLOOR
    return max(sig.DB_FLOOR, float(np.percentile(audible, percentile)))


def compute_thresholds(
    db: np.ndarray, config: DetectionConfig
) -> tuple[float, float, float]:
    """Return ``(noise_floor_db, enter_db, exit_db)``.

    The relative threshold is capped just below the loud end of the file so a
    recording with no real quiet passage cannot be suppressed wholesale.
    """

    noise_floor = estimate_noise_floor(db, config.noise_percentile)
    enter = max(config.threshold_db, noise_floor + config.relative_margin_db)
    if db.size:
        cap = float(np.percentile(db, 85)) - 1.0
        if cap > config.threshold_db:
            enter = min(enter, cap)
    exit_db = enter - config.hysteresis_db
    return noise_floor, enter, exit_db


def hysteresis_mask(db: np.ndarray, enter_db: float, exit_db: float) -> np.ndarray:
    """Frames above ``enter_db`` open a run; it closes only below ``exit_db``."""

    mask = np.zeros(db.size, dtype=bool)
    active = False
    start = 0
    for i, value in enumerate(db):
        if not active:
            if value >= enter_db:
                active = True
                start = i
        elif value < exit_db:
            mask[start:i] = True
            active = False
    if active:
        mask[start:] = True
    return mask


def apply_ramp_compensation(
    mask: np.ndarray,
    db: np.ndarray,
    enter_db: float,
    noise_floor: float,
    config: DetectionConfig,
    hop_ms: float,
) -> np.ndarray:
    """Reclaim fade-in/out frames that fall below the enter threshold.

    For each boundary we walk outward while the trend is monotone *toward*
    the segment (energy rising left-to-right on the left side, falling on the
    right side) and the frame is still clearly above the background noise.
    The walk is capped at ``ramp_window_ms`` and at ``ramp_max_db`` below the
    enter threshold, so a steady low-level floor is never swallowed.  Short
    excursions are ignored, which keeps isolated noise blips out.
    """

    if db.size == 0 or not np.any(mask):
        return mask
    max_frames = max(1, int(round(config.ramp_window_ms / max(hop_ms, 1e-9))))
    min_frames = 2
    min_rise_db = 3.0
    gate = max(noise_floor + config.ramp_margin_db, enter_db - config.ramp_max_db)
    tolerance = 0.5  # dB of wiggle allowed inside an otherwise monotone ramp
    out = mask.copy()
    for run in reg.runs_from_mask(mask):
        left = run.start
        steps = 0
        while left > 0 and steps < max_frames:
            k = left - 1
            if out[k] or db[k] < gate or db[k] > db[k + 1] + tolerance:
                break
            left = k
            steps += 1
        if steps >= min_frames and db[run.start] - db[left] >= min_rise_db:
            out[left : run.start] = True
        right = run.end
        steps = 0
        while right < db.size and steps < max_frames:
            k = right
            if out[k] or db[k] < gate or db[k] > db[k - 1] + tolerance:
                break
            right = k + 1
            steps += 1
        if steps >= min_frames and db[run.end - 1] - db[right - 1] >= min_rise_db:
            out[run.end : right] = True
    return out


def _ms_to_frames(ms: float, hop_ms: float) -> int:
    if ms <= 0 or hop_ms <= 0:
        return 0
    return int(math.ceil(ms / hop_ms))


def detect_from_features(
    features: sig.FrameFeatures, config: DetectionConfig
) -> tuple[list[Interval], float, float, float]:
    """Run the frame-domain stages; return sample intervals + thresholds."""

    hop_ms = features.grid.hop_ms
    db = features.rms_db()
    if db.size == 0:
        return [], sig.DB_FLOOR, config.threshold_db, config.threshold_db
    smoothed = sig.smooth_db(db, sig.smoothing_frames(config.smooth_ms, hop_ms))
    noise_floor, enter_db, exit_db = compute_thresholds(smoothed, config)
    ramp_floor = audible_floor(smoothed, config.noise_percentile)
    mask = hysteresis_mask(smoothed, enter_db, exit_db)
    mask = apply_ramp_compensation(
        mask, smoothed, enter_db, ramp_floor, config, hop_ms
    )
    silence_frames = _ms_to_frames(config.min_silence_ms, hop_ms)
    pulse_frames = _ms_to_frames(config.max_pulse_ms, hop_ms)
    mask = reg.bridge_pulses(mask, pulse_frames, silence_frames)
    runs = reg.runs_from_mask(mask)
    hop = features.grid.hop_samples
    total = features.total_samples
    # Convert to samples before merging/filtering so millisecond settings are
    # exact rather than rounded up to a whole hop.
    sample_runs = [
        Interval(r.start * hop, min(total, r.end * hop)) for r in runs
    ]
    min_gap = int(round(config.min_silence_ms / 1000.0 * features.grid.sample_rate))
    sample_runs = reg.merge_close_intervals(sample_runs, min_gap)
    min_length = int(
        round(config.min_segment_ms / 1000.0 * features.grid.sample_rate)
    )
    sample_runs = reg.drop_short_intervals(sample_runs, min_length)
    if not sample_runs:
        return [], noise_floor, enter_db, exit_db

    intervals: list[Interval] = []
    for run in sample_runs:
        start = max(0, min(run.start, total))
        end = max(start, min(run.end, total))
        if intervals and start <= intervals[-1].end:
            prev = intervals[-1]
            intervals[-1] = Interval(prev.start, max(prev.end, end))
        else:
            intervals.append(Interval(start, end))
    return intervals, noise_floor, enter_db, exit_db


def _iter_mono_blocks(source: BlockSource) -> Iterator[np.ndarray]:
    for block in source():
        mono = sig.merge_channels(np.asarray(block))
        if mono.size:
            yield mono


def exact_region_stats(
    source: BlockSource,
    intervals: Sequence[Interval],
) -> list[tuple[float, float]]:
    """Second pass: exact peak/mean-RMS per interval without retaining audio."""

    results: list[tuple[float, float]] = []
    if not intervals:
        return results
    cursor = 0
    position = 0
    sums = [0.0] * len(intervals)
    peaks = [0.0] * len(intervals)
    counts = [0] * len(intervals)
    for block in _iter_mono_blocks(source):
        block_end = position + block.size
        while cursor < len(intervals) and intervals[cursor].end <= position:
            cursor += 1
        idx = cursor
        while idx < len(intervals) and intervals[idx].start < block_end:
            item = intervals[idx]
            lo = max(item.start, position) - position
            hi = min(item.end, block_end) - position
            if hi > lo:
                window = block[lo:hi]
                sums[idx] += float(np.sum(np.square(window, dtype=np.float64)))
                peaks[idx] = max(peaks[idx], sig.peak_amplitude(window))
                counts[idx] += int(window.size)
            idx += 1
        position = block_end
    for i in range(len(intervals)):
        count = counts[i]
        rms = math.sqrt(sums[i] / count) if count else 0.0
        results.append((peaks[i], rms))
    return results


def detect_stream(
    source: BlockSource,
    sample_rate: int,
    total_samples: int,
    config: DetectionConfig,
    path: str = "",
) -> DetectionResult:
    """Full detection for a re-openable block source."""

    config.validate(sample_rate)
    grid = sig.FrameGrid(sample_rate, config.frame_ms, config.hop_ms)
    acc = sig.FeatureAccumulator(grid)
    for block in _iter_mono_blocks(source):
        acc.push(block)
    features = acc.finish()
    if features.total_samples != total_samples:
        total_samples = features.total_samples
    intervals, noise_floor, enter_db, exit_db = detect_from_features(
        features, config
    )
    pad_samples = int(round(sample_rate * config.padding_ms / 1000.0))
    intervals = reg.apply_padding(intervals, pad_samples, total_samples)
    stats = exact_region_stats(source, intervals)

    segments = [
        Segment(
            index=i,
            start_sample=item.start,
            end_sample=item.end,
            peak=stats[i][0],
            rms=stats[i][1],
            sample_rate=sample_rate,
        )
        for i, item in enumerate(intervals)
    ]
    silences = [
        SilenceRegion(
            index=i,
            start_sample=item.start,
            end_sample=item.end,
            sample_rate=sample_rate,
        )
        for i, item in enumerate(reg.complement_intervals(intervals, total_samples))
    ]
    return DetectionResult(
        path=path,
        sample_rate=sample_rate,
        total_samples=total_samples,
        noise_floor_db=noise_floor,
        enter_threshold_db=enter_db,
        exit_threshold_db=exit_db,
        segments=segments,
        silences=silences,
    )


def detect_array(
    samples: np.ndarray,
    sample_rate: int,
    config: DetectionConfig,
    path: str = "",
    block_size: int = 1 << 15,
) -> DetectionResult:
    """Convenience entry point for in-memory arrays (tests, fixtures)."""

    mono = sig.merge_channels(np.asarray(samples))

    def source() -> Iterator[np.ndarray]:
        for offset in range(0, mono.size, block_size):
            yield mono[offset : offset + block_size]

    return detect_stream(source, sample_rate, int(mono.size), config, path=path)
