"""Unit tests for the unified timebase: unwrap, drift and monotonicity."""

import pytest

from archive_media.errors import RecoverableMediaError
from archive_media.timebase import SharedTimeline, TrackTimeline


def test_unwrap_expands_modulo_counter():
    track = TrackTimeline(
        "video", time_base=1 / 1000.0, wrap_period_ticks=1000, ref_period_s=1.0
    )
    # Raw ticks walk up to the period then jump backwards (wraparound).
    raw = [900, 950, 0, 50, 100]
    unwrapped = [track.unwrap(pts, strict=False) for pts in raw]
    assert unwrapped == [900, 950, 1000, 1050, 1100]
    assert track.wrap_count == 1


def test_unwrap_flags_genuine_backwards_step():
    track = TrackTimeline(
        "video", time_base=1 / 1000.0, wrap_period_ticks=1_000_000, ref_period_s=1.0
    )
    track.unwrap(100, strict=False)
    track.unwrap(200, strict=False)
    with pytest.raises(RecoverableMediaError):
        track.unwrap(150, strict=True)  # backwards, far inside one period


def test_drift_median_baseline():
    # Every interval is 5% longer than nominal -> +50000 ppm drift.
    track = TrackTimeline(
        "video", time_base=1 / 1000.0, wrap_period_ticks=None, ref_period_s=1.0
    )
    nominal = track.expected_interval_ticks  # 1000 ticks
    pts = [int(round(i * nominal * 1.05)) for i in range(20)]
    for p in pts:
        track.unwrap(p, strict=False)
    track.freeze()
    assert track.drift_ppm == pytest.approx(50_000.0, rel=0.02)


def test_shared_timeline_two_pass_monotonic():
    tl = SharedTimeline()
    tl.add_track("video", time_base=1 / 1000.0, wrap_period_ticks=None, ref_period_s=1.0)
    raw = [i * 1000 for i in range(10)]
    for p in raw:
        tl.calibrate("video", p)
    tl.finalize_calibration()
    corrected = [tl.correct("video", p) for p in raw]
    assert corrected[0] == 0.0
    assert all(corrected[i] < corrected[i + 1] for i in range(len(corrected) - 1))
    assert tl.anomalies == []


def test_corrections_summary_shape():
    tl = SharedTimeline()
    tl.add_track("audio", time_base=1 / 48000.0, wrap_period_ticks=None,
                 ref_period_s=1024 / 24000.0)
    for i in range(20):
        tl.calibrate("audio", i * 2048)
    tl.finalize_calibration()
    summary = tl.corrections()
    assert "audio" in summary
    assert set(summary["audio"]) == {
        "drift_ppm", "drift_rate", "wrap_count", "first_anomaly_s"
    }
