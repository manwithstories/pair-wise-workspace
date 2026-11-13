from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path

from forensictl.imageio import ImageIO
from forensictl.timebase import TimeBase, load_ntp_samples


def test_spring_nonexistent_time_uses_pre_transition_offset(image_path: Path) -> None:
    with ImageIO.open(image_path) as image:
        timebase = TimeBase.from_image(image)
    normalized = timebase.normalize_local("2026-03-08T02:30:00", apply_clock=False)
    assert normalized.dst_kind == "nonexistent"
    assert normalized.offset_seconds == -5 * 3600
    assert normalized.utc == datetime(2026, 3, 8, 7, 30, tzinfo=timezone.utc)


def test_fall_repeated_time_uses_first_occurrence(image_path: Path) -> None:
    with ImageIO.open(image_path) as image:
        timebase = TimeBase.from_image(image)
    normalized = timebase.normalize_local("2026-11-01T01:30:00", apply_clock=False)
    assert normalized.dst_kind == "ambiguous"
    assert normalized.offset_seconds == -4 * 3600
    assert normalized.utc == datetime(2026, 11, 1, 5, 30, tzinfo=timezone.utc)


def test_offset_timestamp_is_direct(image_path: Path) -> None:
    with ImageIO.open(image_path) as image:
        timebase = TimeBase.from_image(image)
    normalized = timebase.normalize_local("2026-03-08T02:30:00-05:00")
    assert normalized.utc == datetime(2026, 3, 8, 7, 30, tzinfo=timezone.utc)
    assert normalized.correction_seconds == 0.0


def test_two_steps_and_linear_drift(image_path: Path) -> None:
    with ImageIO.open(image_path) as image:
        timebase = TimeBase.from_image(image)
        samples = load_ntp_samples(image)
    assert sum(sample.mode == "step" for sample in samples) == 2
    assert {sample.mode for sample in samples} >= {"drift_start", "drift_end"}
    midpoint = datetime(2026, 3, 8, 6, 20, tzinfo=timezone.utc)
    assert abs(timebase.correction_seconds(midpoint) - (-4.5)) < 1e-9
    before_step = datetime(2026, 3, 8, 7, 59, 59, tzinfo=timezone.utc)
    after_step = datetime(2026, 3, 8, 8, 0, 0, tzinfo=timezone.utc)
    assert abs(timebase.correction_seconds(before_step) - (-6.0)) < 1e-9
    assert abs(timebase.correction_seconds(after_step) - 1.2) < 1e-9
