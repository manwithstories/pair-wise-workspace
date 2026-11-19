"""Pipeline metrics: sync error, packet recovery and reorder latency."""

from __future__ import annotations

from fractions import Fraction

from .clock import TICKS_PER_MS
from .packets import PacketStream
from .timeline import AudioTimeline, VideoTimeline


def _percentile(values: list[float], q: float) -> float:
    """Nearest-rank percentile (q in [0, 100]); 0.0 for empty input."""
    if not values:
        return 0.0
    ordered = sorted(values)
    rank = max(1, min(len(ordered), int(round(q / 100 * len(ordered)))))
    return ordered[rank - 1]


def ticks_to_ms_float(ticks: int | Fraction) -> float:
    return float(Fraction(ticks) / TICKS_PER_MS)


def packet_recovery_rate(streams: list[PacketStream]) -> float:
    """Recovered / expected packets across all tracks.

    Expected = full sequence span (min..max seq). Recovered packets are those
    released into the timeline, including healed retransmissions.
    """
    total_expected = 0
    total_recovered = 0
    for s in streams:
        if s.expected_seq_max < 0:
            continue
        lo = min(p.seq for p in s.packets) if s.packets else 0
        span = s.expected_seq_max - lo + 1
        total_expected += span
        total_recovered += span - len(s.unrecovered_seqs)
    if total_expected == 0:
        return 1.0
    return total_recovered / total_expected


def latency_ms_p95(streams: list[PacketStream]) -> float:
    """p95 time packets spent inside the bounded reorder buffer."""
    holds: list[float] = []
    for s in streams:
        for p in s.packets:
            holds.append(ticks_to_ms_float(p.hold_ticks))
    return round(_percentile(holds, 95), 3)


def sync_error_ms_p95(
    videos: list[VideoTimeline],
    audios: list[AudioTimeline],
) -> float:
    """p95 end-to-end A/V sync error in milliseconds.

    Combines two error sources:
    1. per-frame residual of the drift fit against the reference clock;
    2. measured audio offset error vs the expected capture offset.
    """
    errors: list[float] = []
    for tl in videos:
        if tl.drift is None or len(tl.frames) < 2:
            continue
        for i, fr in enumerate(tl.frames):
            mapped = tl.drift.map(fr.capture_ts, i)
            errors.append(abs(mapped - fr.ref_ts) / TICKS_PER_MS)
    for a in audios:
        errors.append(abs(a.measured_offset_ticks - a.expected_offset_ticks)
                      / TICKS_PER_MS)
    return round(_percentile(errors, 95), 3)


def summarize(
    streams: list[PacketStream],
    videos: list[VideoTimeline],
    audios: list[AudioTimeline],
    seek_failures: int,
) -> dict:
    return {
        "sync_error_ms_p95": sync_error_ms_p95(videos, audios),
        "packet_recovery_rate": round(packet_recovery_rate(streams), 4),
        "latency_ms_p95": latency_ms_p95(streams),
        "index_seek_failures": seek_failures,
        "tracks": {
            "video": [
                {
                    "track": v.track,
                    "codec": v.codec,
                    "frames": len(v.frames),
                    "keyframes": sum(1 for f in v.frames if f.is_keyframe),
                    "uncertain_intervals": sum(1 for f in v.frames if f.uncertain),
                    "drift_segments": len(v.drift.segments) if v.drift else 1,
                    "duration_ms": round(ticks_to_ms_float(v.duration_ticks), 3),
                }
                for v in videos
            ],
            "audio": [
                {
                    "track": a.track,
                    "samplerate": a.samplerate,
                    "frames": a.frames,
                    "measured_offset_ms": round(
                        ticks_to_ms_float(a.measured_offset_ticks), 3),
                    "expected_offset_ms": round(
                        ticks_to_ms_float(a.expected_offset_ticks), 3),
                    "overrun_ms": round(ticks_to_ms_float(a.overrun_ticks), 3),
                }
                for a in audios
            ],
        },
    }
