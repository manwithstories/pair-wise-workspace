from fractions import Fraction

from tlcore.model import Segment, Timeline, Track
from tlcore.schedule import build_schedule


def test_frame_count_and_activity_at_24_fps():
    timeline = Timeline(
        fps=Fraction(24),
        tracks=[
            Track("v1", "video", segments=[Segment("a", 0, 48_000)]),
            Track("a1", "audio", segments=[Segment("m", 24_000, 24_000)]),
        ],
        declared_duration=48_000,
    )
    result = build_schedule(timeline)
    assert result.schedule_frames == 24
    assert result.report["frames"][0]["active_count"] == 1
    assert result.report["frames"][12]["active_count"] == 2
    assert result.report["frames"][23]["active_count"] == 2
    assert len(result.digest) == 64
    assert result.report["max_audio_concurrent"] == 1


def test_fractional_rate_and_visual_occlusion():
    timeline = Timeline(
        fps=Fraction(30_000, 1_001),
        tracks=[
            Track("v1", "video", z_order=0, segments=[Segment("low", 0, 1600)]),
            Track("v2", "video", z_order=1, segments=[Segment("high", 0, 800)]),
        ],
        declared_duration=1600,
    )
    report = build_schedule(timeline).report
    assert report["schedule_frames"] == 1
    active = report["frames"][0]["active"]
    assert next(item for item in active if item["id"] == "low")["occluded_by"] == ["high"]


def test_schedule_is_deterministic():
    timeline = Timeline(
        tracks=[Track("v1", "video", segments=[Segment("a", 7, 200)])],
        declared_duration=333,
    )
    assert build_schedule(timeline).digest == build_schedule(timeline).digest
