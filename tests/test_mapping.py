"""Tests for stream mapping and timeline validation."""

from __future__ import annotations

import pytest

from aircheck.mapping import (
    TimelinePacket,
    resolve_mapping,
    validate_timeline,
)
from aircheck.probe import probe
from aircheck.timebase import MappingError

SEGMENT = 250_000_000


@pytest.fixture(scope="module")
def probed(multi_mp4):
    return probe(multi_mp4.path, scan_packets=False)


def test_identity_mapping_copies_every_media_stream(probed):
    mapping = resolve_mapping(probed)
    assert [item.input_index for item in mapping.assignments] == [0, 1, 2, 3]
    assert [item.output_index for item in mapping.assignments] == [0, 1, 2, 3]
    assert mapping.dropped == []


def test_explicit_mapping_reports_dropped_streams(probed):
    mapping = resolve_mapping(probed, "v:0=a:2")
    assert [(item.input_index, item.kind) for item in mapping.assignments] == [
        (0, "video"),
        (2, "audio"),
    ]
    assert mapping.dropped == [1, 3]


def test_bare_numbers_continue_previous_kind(probed):
    mapping = resolve_mapping(probed, "v:0,1")
    assert [item.input_index for item in mapping.assignments] == [0, 1]


def test_missing_stream_reports_the_stream_number(probed):
    with pytest.raises(MappingError) as info:
        resolve_mapping(probed, "v:9")
    assert info.value.context["stream_index"] == 9
    assert info.value.context["available_streams"] == [0, 1, 2, 3]


def test_wrong_type_reports_both_kinds(probed):
    with pytest.raises(MappingError) as info:
        resolve_mapping(probed, "v:2")
    assert info.value.context["expected_kind"] == "video"
    assert info.value.context["actual_kind"] == "audio"
    assert info.value.context["stream_index"] == 2


def test_duplicate_stream_reports_the_stream_number(probed):
    with pytest.raises(MappingError) as info:
        resolve_mapping(probed, "v:0=a:0")
    assert info.value.context["stream_index"] == 0


def test_unknown_kind_is_rejected(probed):
    with pytest.raises(MappingError):
        resolve_mapping(probed, "x:0")


def test_bare_number_without_prior_kind_is_rejected(probed):
    with pytest.raises(MappingError):
        resolve_mapping(probed, "0")


def test_mapping_dict_input_is_accepted(probed):
    mapping = resolve_mapping(probed, {"video": [1], "audio": [3]})
    assert [item.input_index for item in mapping.assignments] == [1, 3]


def test_empty_mapping_is_rejected(probed):
    with pytest.raises(MappingError):
        resolve_mapping(probed, {"video": [], "audio": []})


def _packets(stream: int, timestamps: list[int], segment_ticks: int = SEGMENT,
             keyframes: set[int] | None = None) -> list[TimelinePacket]:
    keyframes = keyframes or set()
    return [
        TimelinePacket(
            stream_index=stream,
            pts_ticks=pts,
            dts_ticks=pts,
            is_keyframe=index in keyframes,
            segment=pts // segment_ticks,
        )
        for index, pts in enumerate(timestamps)
    ]


def test_clean_timeline_has_no_issues():
    packets = _packets(0, [0, 20_000_000, 40_000_000], keyframes={0, 2})
    report = validate_timeline(packets, segment_ticks=SEGMENT, duration_ticks=250_000_000)
    assert report.ok
    assert report.keyframes == [0, 40_000_000]


def test_non_monotonic_is_reported_per_stream():
    packets = _packets(0, [0, 100, 50])
    report = validate_timeline(packets, segment_ticks=SEGMENT, duration_ticks=1000)
    assert [issue.kind for issue in report.issues] == ["non_monotonic"]
    assert report.issues[0].stream_index == 0


def test_negative_after_start_is_reported_but_priming_is_not():
    priming = _packets(0, [-1024, 0, 1024])
    assert validate_timeline(priming, segment_ticks=SEGMENT, duration_ticks=1e9).ok
    late_negative = _packets(0, [0, 1024, -1])
    kinds = [issue.kind for issue in validate_timeline(
        late_negative, segment_ticks=SEGMENT, duration_ticks=1e9).issues]
    assert kinds == ["negative_timestamp"]


def test_cut_points_outside_duration_are_reported():
    report = validate_timeline(
        _packets(0, [0, 10]),
        segment_ticks=SEGMENT,
        duration_ticks=1_000_000_000,
        cut_points=[250_000_000, 2_000_000_000],
    )
    kinds = [issue.kind for issue in report.issues]
    assert kinds == ["cut_out_of_bounds"]


def test_cut_points_off_grid_are_reported_only_with_transitions():
    cut = [100_000_000]
    without = validate_timeline(
        _packets(0, [0]), segment_ticks=SEGMENT, duration_ticks=500_000_000, cut_points=cut
    )
    assert without.ok
    with_transition = validate_timeline(
        _packets(0, [0]), segment_ticks=SEGMENT, duration_ticks=500_000_000,
        cut_points=cut, transition_ms=25,
    )
    assert "cut_off_grid" in {issue.kind for issue in with_transition.issues}


def test_transition_longer_than_segment_is_inconsistent():
    report = validate_timeline(
        _packets(0, [0, SEGMENT]),
        segment_ticks=SEGMENT,
        duration_ticks=500_000_000,
        transition_ms=400,
    )
    assert "transition_overlap_inconsistent" in {issue.kind for issue in report.issues}


def test_transition_findings_are_reported_once():
    packets = _packets(0, [0, SEGMENT, SEGMENT * 2, SEGMENT * 3])
    report = validate_timeline(
        packets, segment_ticks=SEGMENT, duration_ticks=1_000_000_000, transition_ms=400
    )
    overlap = [issue for issue in report.issues
               if issue.kind == "transition_overlap_inconsistent"]
    assert len(overlap) == 2  # one for the impossible width, one summarising shorts


def test_segments_are_deduplicated_and_sorted():
    packets = _packets(0, [0, SEGMENT * 2, SEGMENT])
    report = validate_timeline(packets, segment_ticks=SEGMENT, duration_ticks=1_000_000_000)
    assert report.segments == [0, 1, 2]
