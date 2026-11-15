"""Tests for anomaly detection, equal-interval repair and idempotency."""

from __future__ import annotations

from fractions import Fraction

import pytest

from cinecheck.timeline import (
    DEFAULT_TOLERANCE_MS,
    detect_anomalies,
    frame_step_ticks,
    repair_timeline,
)

TB = Fraction(1, 12288)
FPS = Fraction(24, 1)
STEP = 512


def test_frame_step_is_exact_integer_ticks():
    assert frame_step_ticks(TB, FPS) == 512
    assert frame_step_ticks(Fraction(1, 90000), FPS) == 3750


def test_clean_timeline_has_no_anomalies():
    pts = [i * STEP for i in range(120)]
    report = detect_anomalies(pts, TB, FPS)
    assert report.total == 0
    assert all(count == 0 for count in report.counts.values())


def test_each_anomaly_kind_is_counted_and_original_kept():
    pts = [0, 512, 512, 400, 1024, 3000, 3500]
    report = detect_anomalies(pts, TB, FPS)
    counts = report.counts
    assert counts["duplicate_pts"] == 1
    assert counts["negative_delta"] == 1
    assert counts["gap"] >= 1
    assert counts["jitter"] >= 1
    assert counts["non_zero_start"] == 0
    # every event retains the offending observed pts
    assert all(event.observed_pts is not None for event in report.events)


def test_non_zero_start_and_tail_residual():
    pts = [100, 612, 1124, 1300]
    report = detect_anomalies(pts, TB, FPS)
    assert report.counts["non_zero_start"] == 1
    assert report.counts["tail_residual"] == 1


def test_repair_builds_equal_interval_and_reports_max_jump():
    pts = [i * STEP for i in range(60)]
    pts[30] += 1024
    result = repair_timeline(pts, TB, FPS)
    assert result.repaired_pts == [i * STEP for i in range(60)]
    assert result.max_jump_ticks == 1024
    assert result.repaired_count >= 1


def test_repair_is_idempotent():
    pts = [i * STEP for i in range(60)]
    pts[10] += 256
    pts[40] += 1024
    first = repair_timeline(pts, TB, FPS)
    second = repair_timeline(first.repaired_pts, TB, FPS)
    assert first.second_pass_repairs == 0
    assert first.idempotent_pass is True
    assert second.repaired_count == 0
    assert second.max_jump_ticks == 0


def test_tolerance_gates_small_deviations():
    pts = [i * STEP for i in range(40)]
    pts[20] += 4  # well inside the default ~12-tick tolerance
    report = detect_anomalies(pts, TB, FPS, tolerance_ms=DEFAULT_TOLERANCE_MS)
    assert report.counts["jitter"] == 0


def test_empty_timeline_is_safe():
    report = detect_anomalies([], TB, FPS)
    assert report.total == 0
    result = repair_timeline([], TB, FPS)
    assert result.repaired_pts == []
    assert result.idempotent_pass is True


@pytest.mark.parametrize("time_base", [Fraction(1, 90000), Fraction(1, 12288)])
def test_both_required_time_bases(time_base):
    step = frame_step_ticks(time_base, FPS)
    pts = [i * step for i in range(24)]
    report = detect_anomalies(pts, time_base, FPS)
    assert report.total == 0
    assert repair_timeline(pts, time_base, FPS).idempotent_pass is True
