"""End-to-end statistics: sync error, recovery rate and latency quantiles.

All statistics are deterministic functions of the processed material, the
timeline and the decode statistics - never of wall-clock timing alone (latency
is measured, but its *quantiles* are reported over recorded values, and the
acceptance run uses enough blocks for the quantiles to be stable).

Reported fields (the names are part of the acceptance contract):

``sync_error_ms_p95``
    p95 of the absolute per-frame A/V sync error estimate, in milliseconds.
``packet_recovery_rate``
    Recovered losses / (recovered + unrecoverable losses).  Concealed frames
    never count as recoveries.
``latency_ms_p95``
    p95 of per-block producer->consumer latency in milliseconds.
``peak_queue_frames``
    Maximum queue depth observed, in *frames* (blocks x block size).
``frame_hash_mismatches``
    Number of output frames whose hash differs from the recorded manifest.

Degenerate inputs (empty tracks, zero frames, timestamp jumps, total loss)
produce explicit statistics and a documented non-zero exit code rather than a
crash or a silent zero.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Iterable, Sequence

import numpy as np

from . import EXIT_INPUT_INVALID, EXIT_MEDIA_UNRECOVERABLE, EXIT_OK
from .decode import DecodeStats

__all__ = [
    "quantile",
    "LatencyRecorder",
    "QcReport",
    "build_report",
    "evaluate_thresholds",
    "derive_exit_code",
]


def quantile(values: Sequence[float], q: float) -> float:
    """Linear-interpolated quantile with deterministic tie handling.

    ``q`` is in ``[0, 1]``.  Empty input returns 0.0.  This matches
    ``numpy.percentile(method="linear")`` without importing numpy here.
    """
    if not values:
        return 0.0
    if not 0.0 <= q <= 1.0:
        raise ValueError("q must be within [0, 1]")
    ordered = sorted(float(v) for v in values)
    if len(ordered) == 1:
        return ordered[0]
    position = q * (len(ordered) - 1)
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    weight = position - lower
    return ordered[lower] * (1.0 - weight) + ordered[upper] * weight


@dataclass
class LatencyRecorder:
    """Collects per-block latency samples (milliseconds)."""

    samples: list[float] = field(default_factory=list)

    def add(self, latency_ms: float) -> None:
        if latency_ms < 0:
            raise ValueError("latency cannot be negative")
        self.samples.append(float(latency_ms))

    @property
    def p50(self) -> float:
        return quantile(self.samples, 0.50)

    @property
    def p95(self) -> float:
        return quantile(self.samples, 0.95)

    @property
    def max(self) -> float:
        return max(self.samples) if self.samples else 0.0

    def to_json(self) -> dict:
        return {
            "samples": len(self.samples),
            "p50_ms": round(self.p50, 3),
            "p95_ms": round(self.p95, 3),
            "max_ms": round(self.max, 3),
        }


@dataclass
class QcReport:
    sync_error_ms_p95: float = 0.0
    sync_error_ms_max: float = 0.0
    packet_recovery_rate: float = 1.0
    latency_ms_p50: float = 0.0
    latency_ms_p95: float = 0.0
    latency_ms_max: float = 0.0
    peak_queue_frames: int = 0
    peak_queue_blocks: int = 0
    queue_capacity_blocks: int = 0
    frame_hash_mismatches: int = 0
    frames_rendered: int = 0
    frames_verified: int = 0
    recovered_frames: int = 0
    concealed_frames: int = 0
    decoded_frames: int = 0
    packets_lost: int = 0
    recovered_packets: int = 0
    unrecoverable_packets: int = 0
    decoder_resets: int = 0
    illegal_units: int = 0
    max_gap_ms: float = 0.0
    duration_seconds: float = 0.0
    tracks: int = 0
    excluded_tracks: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    exit_code: int = EXIT_OK
    ok: bool = True

    def to_json(self) -> dict:
        return {
            "sync_error_ms_p95": round(self.sync_error_ms_p95, 3),
            "sync_error_ms_max": round(self.sync_error_ms_max, 3),
            "packet_recovery_rate": round(self.packet_recovery_rate, 4),
            "latency_ms_p50": round(self.latency_ms_p50, 3),
            "latency_ms_p95": round(self.latency_ms_p95, 3),
            "latency_ms_max": round(self.latency_ms_max, 3),
            "peak_queue_frames": self.peak_queue_frames,
            "peak_queue_blocks": self.peak_queue_blocks,
            "queue_capacity_blocks": self.queue_capacity_blocks,
            "frame_hash_mismatches": self.frame_hash_mismatches,
            "frames_rendered": self.frames_rendered,
            "frames_verified": self.frames_verified,
            "recovered_frames": self.recovered_frames,
            "concealed_frames": self.concealed_frames,
            "decoded_frames": self.decoded_frames,
            "packets_lost": self.packets_lost,
            "recovered_packets": self.recovered_packets,
            "unrecoverable_packets": self.unrecoverable_packets,
            "decoder_resets": self.decoder_resets,
            "illegal_units": self.illegal_units,
            "max_gap_ms": round(self.max_gap_ms, 3),
            "duration_seconds": round(self.duration_seconds, 6),
            "tracks": self.tracks,
            "excluded_tracks": sorted(self.excluded_tracks),
            "notes": list(self.notes),
            "ok": self.ok,
            "exit_code": self.exit_code,
        }


@dataclass
class Thresholds:
    """Acceptance targets from the brief."""

    sync_error_ms_p95: float = 20.0
    packet_recovery_rate: float = 0.90
    latency_ms_p95: float = 500.0
    peak_queue_frames: int = 8
    frame_hash_mismatches: int = 0

    def to_json(self) -> dict:
        return {
            "sync_error_ms_p95": self.sync_error_ms_p95,
            "packet_recovery_rate": self.packet_recovery_rate,
            "latency_ms_p95": self.latency_ms_p95,
            "peak_queue_frames": self.peak_queue_frames,
            "frame_hash_mismatches": self.frame_hash_mismatches,
        }


def evaluate_thresholds(report: QcReport, thresholds: Thresholds) -> dict[str, dict[str, Any]]:
    """Compare a report against thresholds; returns per-criterion verdicts."""
    checks = {
        "sync_error_ms_p95": (
            report.sync_error_ms_p95 <= thresholds.sync_error_ms_p95,
            f"{report.sync_error_ms_p95:.3f} <= {thresholds.sync_error_ms_p95:.3f} ms",
        ),
        "packet_recovery_rate": (
            report.packet_recovery_rate >= thresholds.packet_recovery_rate,
            f"{report.packet_recovery_rate:.4f} >= {thresholds.packet_recovery_rate:.4f}",
        ),
        "latency_ms_p95": (
            report.latency_ms_p95 <= thresholds.latency_ms_p95,
            f"{report.latency_ms_p95:.3f} <= {thresholds.latency_ms_p95:.3f} ms",
        ),
        "peak_queue_frames": (
            report.peak_queue_frames <= thresholds.peak_queue_frames,
            f"{report.peak_queue_frames} <= {thresholds.peak_queue_frames}",
        ),
        "frame_hash_mismatches": (
            report.frame_hash_mismatches <= thresholds.frame_hash_mismatches,
            f"{report.frame_hash_mismatches} <= {thresholds.frame_hash_mismatches}",
        ),
    }
    return {name: {"pass": passed, "detail": detail} for name, (passed, detail) in checks.items()}


def derive_exit_code(
    *,
    input_valid: bool,
    rendered: int,
    decoded: int,
    unrecoverable: int,
    frames_expected: int,
    thresholds_met: bool,
    media_error: bool = False,
) -> tuple[int, list[str]]:
    """Map pipeline outcomes onto the documented exit codes.

    ``2`` input/timeline invalid, ``3`` unrecoverable media loss that left the
    output incomplete, ``4`` reserved for resources (raised elsewhere), and
    ``0`` only when the output is complete and every threshold passes.
    """
    notes: list[str] = []
    if not input_valid:
        return EXIT_INPUT_INVALID, ["input or timeline was rejected"]
    if rendered == 0:
        return EXIT_INPUT_INVALID, ["no output frames were produced"]
    if decoded == 0:
        return EXIT_MEDIA_UNRECOVERABLE, ["no frame could be decoded"]
    if frames_expected and rendered < frames_expected:
        shortfall = frames_expected - rendered
        return EXIT_MEDIA_UNRECOVERABLE, [f"output is short by {shortfall} frames"]
    if media_error:
        return EXIT_MEDIA_UNRECOVERABLE, ["unrecoverable media error"]
    if not thresholds_met:
        return EXIT_MEDIA_UNRECOVERABLE, ["acceptance thresholds were not met"]
    if unrecoverable:
        notes.append(f"{unrecoverable} losses were unrecoverable but output is complete")
    return EXIT_OK, notes


def build_report(
    *,
    decode_stats: Iterable[DecodeStats],
    latency: LatencyRecorder,
    sync_errors_ms: Sequence[float],
    peak_queue_blocks: int,
    queue_capacity_blocks: int,
    block_frames: int,
    frames_rendered: int,
    frames_verified: int,
    frame_hash_mismatches: int,
    recovered_frames: int,
    concealed_frames: int,
    concealed_gap_ms: float,
    duration_seconds: float,
    tracks: int,
    excluded_tracks: Sequence[str],
    notes: Sequence[str] = (),
) -> QcReport:
    """Aggregate per-track decode stats and measured properties into a report."""
    stats = list(decode_stats)
    recovered = sum(s.recovered_packets for s in stats)
    unrecoverable = sum(s.unrecoverable_packets for s in stats)
    attempts = recovered + unrecoverable
    if attempts:
        recovery_rate = recovered / attempts
    elif any(s.packets_lost for s in stats):
        recovery_rate = 0.0
    else:
        recovery_rate = 1.0
    abs_sync = [abs(float(v)) for v in sync_errors_ms]
    report = QcReport(
        sync_error_ms_p95=quantile(abs_sync, 0.95),
        sync_error_ms_max=max(abs_sync) if abs_sync else 0.0,
        packet_recovery_rate=recovery_rate,
        latency_ms_p50=latency.p50,
        latency_ms_p95=latency.p95,
        latency_ms_max=latency.max,
        peak_queue_blocks=peak_queue_blocks,
        peak_queue_frames=peak_queue_blocks * block_frames,
        queue_capacity_blocks=queue_capacity_blocks,
        frame_hash_mismatches=frame_hash_mismatches,
        frames_rendered=frames_rendered,
        frames_verified=frames_verified,
        recovered_frames=recovered_frames,
        concealed_frames=concealed_frames,
        decoded_frames=sum(s.decoded_frames for s in stats),
        packets_lost=sum(s.packets_lost for s in stats),
        recovered_packets=recovered,
        unrecoverable_packets=unrecoverable,
        decoder_resets=sum(s.decoder_resets for s in stats),
        illegal_units=sum(s.illegal_units for s in stats),
        max_gap_ms=max(
            [float(s.max_gap_ms) for s in stats] + [float(concealed_gap_ms)]
        ),
        duration_seconds=duration_seconds,
        tracks=tracks,
        excluded_tracks=list(excluded_tracks),
        notes=list(notes),
    )
    return report


def alignment_from_duplicates(tracker) -> dict:
    """Detect frames that repeat their predecessor (concealment / freeze).

    A run that lost material replaces missing pictures with a copy of the last
    good frame.  Real programme material never repeats a frame bit-for-bit, so
    consecutive pixel-identical frames are an exact, audio-independent signal
    that the pipeline inserted a duplicate.  ``first_duplicate`` and
    ``duplicate_runs`` make the result reviewable frame by frame.
    """
    duplicated: list[int] = []
    runs: list[list[int]] = []
    hashes = tracker.frame_hashes
    for index in range(1, len(hashes)):
        if hashes[index] == hashes[index - 1]:
            duplicated.append(index)
            if runs and runs[-1][-1] == index - 1:
                runs[-1].append(index)
            else:
                runs.append([index])
    return {
        "frames_compared": len(hashes),
        "duplicated_count": len(duplicated),
        "duplicated": duplicated,
        "duplicate_runs": runs,
    }


def measure_placement(tracker, reference_hashes: Sequence[str]) -> dict:
    """Locate each output frame's picture in a clean reference run.

    ``reference_hashes`` come from a *clean* pass over the same timeline
    (correct material, no injected loss).  For every output frame the report
    finds the nearest reference frame with the identical pixel hash and
    records the positional displacement.  Displacement 0 means the picture is
    exactly where it belongs; a non-zero displacement means the output shows
    material from elsewhere (for example an older picture because frames were
    lost and concealed); a frame whose picture does not exist in the reference
    is reported as missing (corrupt or synthesised).
    """
    # A hash may legitimately repeat (static shots, identical slates).  Keep
    # every occurrence and choose the one nearest the output position: a
    # first-occurrence map would report a spurious displacement for identical
    # material, and a last-occurrence map would hide a real one.
    occurrences: dict[str, list[int]] = {}
    for index, value in enumerate(reference_hashes):
        occurrences.setdefault(value, []).append(index)
    displaced: list[int] = []
    missing: list[int] = []
    offsets: dict[int, int] = {}
    for index, value in enumerate(tracker.frame_hashes):
        candidates = occurrences.get(value)
        if not candidates:
            missing.append(index)
            continue
        match = min(candidates, key=lambda pos: (abs(pos - index), pos))
        delta = index - match
        offsets[index] = delta
        if delta != 0:
            displaced.append(index)
    return {
        "frames": len(tracker.frame_hashes),
        "reference_frames": len(reference_hashes),
        "matched": len(tracker.frame_hashes) - len(missing) - len(displaced),
        "displaced_count": len(displaced),
        "displaced": displaced,
        "missing_count": len(missing),
        "missing": missing,
        "max_displacement_frames": max((abs(v) for v in offsets.values()), default=0),
    }


def measure_av_placement(tracker, reference) -> dict:
    """Audio/video displacement measured through the shared marker signal.

    Both the video picture and the audio mix carry a deterministic per-frame
    marker (the fixture's luma step and its amplitude envelope).  Measuring
    both against a clean reference pass gives the picture displacement and the
    audio displacement independently; their difference is the true end-to-end
    A/V error, in frames, without relying on the two being correlated.

    Returns ``video_lag`` and ``audio_lag`` in frames.  Positive audio lag
    means the audio is behind the picture.
    """
    def best_lag(series: Sequence[float], reference_series: Sequence[float], limit: int = 12) -> int:
        n = min(len(series), len(reference_series))
        if n < 4:
            return 0
        a = np.asarray(series[:n], dtype=np.float64)
        b = np.asarray(reference_series[:n], dtype=np.float64)
        a = a - a.mean()
        b = b - b.mean()
        if np.linalg.norm(a) == 0 or np.linalg.norm(b) == 0:
            return 0
        a /= np.linalg.norm(a)
        b /= np.linalg.norm(b)
        best_score = -np.inf
        best = 0
        for lag in range(-limit, limit + 1):
            if lag >= 0:
                left, right = a[: n - lag], b[lag:]
            else:
                left, right = a[-lag:], b[: n + lag]
            if left.size < max(4, n // 2):
                continue
            score = float(np.dot(left, right)) * (n / left.size)
            if score > best_score + 1e-12:
                best_score, best = score, lag
        return best

    video_lag = best_lag(tracker.luma, reference.luma)
    audio_lag = best_lag(tracker.audio_centre, reference.audio_centre)
    return {
        "video_lag_frames": video_lag,
        "audio_lag_frames": audio_lag,
        "av_offset_frames": audio_lag - video_lag,
    }


def sync_errors_from_tracker(tracker, *, window: int = 25, step: int = 5) -> list[float]:
    """Sliding-window sync error estimates from a SyncTracker.

    A single global lag hides drift, so the tracker is evaluated over windows
    of ``window`` frames stepped by ``step``.  Windows with too little signal
    variance return the previous estimate (or 0), never a random value.
    """
    errors: list[float] = []
    total = min(len(tracker.luma), len(tracker.audio_level))
    if total == 0:
        return errors
    # coarse global lag first, then constrain every window to a small
    # neighbourhood of it (short windows cannot support a wide search)
    global_error, global_lag = tracker.sync_error_ms()
    last = 0.0
    for start in range(0, max(1, total - window + 1), step):
        local = tracker._slice(start, start + window)
        value, _ = local.sync_error_ms(max_lag_frames=global_lag + 2, around=global_lag)
        if value is None:
            # no signal variance in this window (silence or a still picture):
            # it is not evidence of a sync error, so carry the previous value
            # instead of inventing one
            errors.append(last)
            continue
        last = float(value)
        errors.append(last)
    if not errors:
        # shorter than one window: evaluate what we have
        value, _ = tracker.sync_error_ms()
        errors.append(float(value) if value is not None else 0.0)
    return errors
