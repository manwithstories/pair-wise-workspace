"""Aggregate run statistics: bitrate, PSNR, alignment and quality.

Keeping these pure computations out of the orchestration module lets the
pipeline stay focused on stage wiring and checkpoint/restart flow.
"""

from __future__ import annotations

from dataclasses import asdict
from fractions import Fraction

import numpy as np

from .render import frame_psnr


def compute_stats(plan, frames, exporter, audio_stats, segment_start_pts,
                  queue, resumed, events, composer, errors) -> dict:
    """Build the JSON-serializable summary for one pipeline run."""
    # Bitrate is measured only on intact segments; damaged segments lose
    # bytes by truncation, which must not be misread as an encoding bitrate
    # deviation.  Both numerator and denominator exclude the same segments.
    gap_seqs = {g.sequence for g in composer.gaps}
    intact = [s for s in plan.segments if s.sequence not in gap_seqs]
    total_bytes = sum(s.byte_size for s in intact)
    total_s = max(float(sum((s.duration for s in intact), Fraction(0))), 1e-9)
    actual = int(total_bytes * 8 / total_s)
    target = plan.declared_bitrate or 0
    err_pct = abs(actual - target) / target * 100.0 if target else 0.0

    real = [f for f in frames if not f.filled]
    if len(real) >= 2:
        psnr_vals = [frame_psnr(real[i - 1].rgb, real[i].rgb)
                     for i in range(1, len(real))]
        psnr = float(sum(psnr_vals) / len(psnr_vals))
    else:
        psnr = 60.0

    alignment_ms = _alignment_ms(plan, segment_start_pts, gap_seqs)
    return {
        "segments": len(plan.segments),
        "frames": len(exporter.records),
        "audio_peak": round(audio_stats["peak"], 6),
        "audio_rms": round(audio_stats["rms"], 6),
        "drift_ppm": round(audio_stats["drift_ppm"], 3),
        "bitrate_target_bps": target,
        "bitrate_actual_bps": actual,
        "bitrate_error_pct": round(err_pct, 3),
        "quality_psnr_db": round(psnr, 3),
        "manifest_alignment_ms": round(alignment_ms, 3),
        "frame_hashes": len(exporter.hashes),
        "slow_consumer_events": queue.slow_consumer_events,
        "queue_high_watermark": queue.high_watermark,
        "resumed": resumed,
        "events": events,
        "gaps": [asdict(g) for g in composer.gaps],
        "anomalies": composer.anomalies,
        "errors": errors.to_list(),
        "error_counts": errors.counts(),
        "first_error_offset": errors.first_offset(),
    }


def _alignment_ms(plan, segment_start_pts, gap_seqs) -> float:
    """Median boundary error over adjacent, both-intact segment pairs."""
    starts_by_seq = {
        seg.sequence: start
        for seg, start in zip(plan.segments, segment_start_pts)
    }
    durations_by_seq = {seg.sequence: seg.duration for seg in plan.segments}
    ordered = sorted(starts_by_seq)
    boundary_errors_ms: list[float] = []
    for a, b in zip(ordered, ordered[1:]):
        if a in gap_seqs or b in gap_seqs or b != a + 1:
            continue
        observed_step = starts_by_seq[b] - starts_by_seq[a]
        # The wrap boundary pair jumps by ~the modulus; genuine adjacent
        # segments differ by roughly one declared duration.
        if abs(observed_step) > durations_by_seq[a] * 2:
            continue
        boundary_errors_ms.append(
            abs(float(observed_step - durations_by_seq[a])) * 1000.0)
    # Median suppresses a single segment's VFR first-frame jitter while
    # systematic drift would still shift every pair and move the median.
    return float(np.median(boundary_errors_ms)) if boundary_errors_ms else 0.0
