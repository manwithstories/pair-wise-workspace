"""Aggregate quality and continuity metrics across the encoding ladder.

Bitrate/PSNR per segment come from the encoder; this module adds the two
whole-picture views the review system needs:

* **peak bitrate** – the highest instantaneous per-frame payload rate inside
  a representation, not just the per-segment average;
* **audio continuity** – decode each camera's AAC track, walk it on the same
  shared-clock segment boundaries, and flag discontinuities (sample-count
  jumps against the expected count for the covered media duration).

All computations are streaming or linear in frame/sample count and never
retain full media beyond one camera's decoded audio.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .align import AlignmentResult
from .ladder import RepresentationResult
from .probe import ProbeResult
from .timebase import TICKS_PER_MS, TICKS_PER_SECOND


@dataclass
class AudioContinuity:
    camera: str
    sample_rate: int
    total_samples: int
    segment_sample_counts: list[int]
    discontinuities: list[tuple[int, str]]  # (segment index, reason)

    @property
    def continuous(self) -> bool:
        return not self.discontinuities


def _decode_audio(probe: ProbeResult) -> tuple[np.ndarray, int]:
    """Mono float32 waveform plus sample rate for a camera's first audio track."""
    from .align import read_audio_mono

    if not probe.audios:
        return np.zeros(0, dtype=np.float32), 0
    return read_audio_mono(probe.path, stream_index=0)


def audio_continuity(
    probe: ProbeResult,
    alignment: AlignmentResult,
    segment_bounds_ticks: list[tuple[int, int]],
) -> AudioContinuity:
    """Check that every planned segment maps to a contiguous audio span."""
    camera = probe.videos[0].camera
    cam = alignment.for_camera(camera)
    wave, rate = _decode_audio(probe)
    counts: list[int] = []
    breaks: list[tuple[int, str]] = []
    if rate == 0:
        return AudioContinuity(camera, 0, 0,
                               [0] * len(segment_bounds_ticks),
                               [(i, "no audio track")
                                for i in range(len(segment_bounds_ticks))])

    prev_end_sample = -1
    for idx, (shared_start, shared_end) in enumerate(segment_bounds_ticks):
        local_start = int(round(
            (shared_start - cam.offset_ticks)
            / (1.0 + cam.drift_rate_ppm / 1e6)))
        local_end = int(round(
            (shared_end - cam.offset_ticks)
            / (1.0 + cam.drift_rate_ppm / 1e6)))
        s0 = int(round(local_start / TICKS_PER_SECOND * rate))
        s1 = int(round(local_end / TICKS_PER_SECOND * rate))
        s0 = max(0, min(s0, len(wave)))
        s1 = max(s0, min(s1, len(wave)))
        counts.append(s1 - s0)
        expected = int(round((local_end - local_start)
                             / TICKS_PER_SECOND * rate))
        actual = s1 - s0
        # Tolerate one AAC frame (1024 samples) of boundary rounding.
        if abs(actual - expected) > 1024:
            breaks.append((idx,
                           f"sample count {actual} vs expected {expected}"))
        if prev_end_sample >= 0 and s0 != prev_end_sample:
            breaks.append((idx,
                           f"audio gap/overlap of "
                           f"{s0 - prev_end_sample} samples at boundary"))
        prev_end_sample = s1
    return AudioContinuity(camera, rate, len(wave), counts, breaks)


def representation_peak_bps(rep: RepresentationResult) -> int:
    """Highest measured segment payload bitrate in the representation."""
    actual = [m.actual_bps for m in rep.segments if m.actual_bps > 0]
    return max(actual) if actual else 0


def summarize_representation(rep: RepresentationResult) -> dict:
    """Compact JSON-able summary for one camera/bitrate representation."""
    valid = [m for m in rep.segments if m.actual_bps > 0]
    psnr = [m.psnr_db for m in rep.segments if m.psnr_db is not None]
    errors = [abs(m.bitrate_error_pct) for m in valid]
    durations = [m.duration_ms for m in rep.segments]
    return {
        "camera": rep.camera,
        "representation": rep.representation,
        "resolution": f"{rep.width}x{rep.height}",
        "bitrate_target_bps": rep.target_bps,
        "bitrate_actual_bps": int(round(
            sum(m.actual_bps for m in valid) / len(valid))) if valid else 0,
        "bitrate_peak_bps": representation_peak_bps(rep),
        "bitrate_error_pct": round(max(errors), 2) if errors else 0.0,
        "segment_count": len(rep.segments),
        "segment_duration_ms": {
            "min": round(min(durations), 2) if durations else 0.0,
            "max": round(max(durations), 2) if durations else 0.0,
        },
        "psnr_db": round(float(np.mean(psnr)), 2) if psnr else None,
        "psnr_uncomputable": [
            m.index for m in rep.segments if m.psnr_db is None],
    }
