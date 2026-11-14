from __future__ import annotations

from pathlib import Path

from cutplan.convert import load_timeline, save_timeline
from cutplan.model import Timeline
from cutplan.otio_bridge import export_timeline, import_timeline, roundtrip_check, to_otio, from_otio
from cutplan.render import build_frame_plan, render_timeline
from cutplan.resolve import resolve_timeline
from cutplan.validate import validate_timeline


def test_fixture_acceptance_numbers(conflict_timeline: Timeline) -> None:
    report = validate_timeline(conflict_timeline)
    assert len(conflict_timeline.tracks) == 5
    assert conflict_timeline.clip_count == 42
    assert report.conflicts["clusters"] == 3
    assert report.conflicts["pairs"] == 5
    resolved = resolve_timeline(conflict_timeline, "trim_head")
    assert resolved.report["resolved_clips"] == 39
    assert resolved.report["removed_zero"] == 2
    assert resolved.report["ripple_shifted"] == 7
    assert resolved.report["clusters_after"] == 0
    assert resolved.report["duration_delta_frames"] == 0
    assert resolved.report["duration_delta_seconds"] == 0.0
    again = resolve_timeline(resolved.timeline, "trim_head")
    assert resolved.timeline.canonical_dict() == again.timeline.canonical_dict()


def test_otio_roundtrip_preserves_model_and_policy(conflict_timeline: Timeline, tmp_path: Path) -> None:
    resolved = resolve_timeline(conflict_timeline, "trim_head").timeline
    path = tmp_path / "resolved.otio"
    export_timeline(resolved, path)
    imported = import_timeline(path)
    assert imported.canonical_dict() == resolved.canonical_dict()
    assert imported.metadata["resolution_policy"] == "trim_head"
    assert imported.manifest.ext["cross_track"]["action"] == "orphan"
    result = roundtrip_check(path)
    assert result["ok"]
    assert result["equal"]
    assert result["duration_delta_frames"] == 0


def test_otio_in_memory_roundtrip(conflict_timeline: Timeline) -> None:
    resolved = resolve_timeline(conflict_timeline, "trim_head").timeline
    assert from_otio(to_otio(resolved)).canonical_dict() == resolved.canonical_dict()


def test_render_is_deterministic_and_has_expected_gap(conflict_timeline: Timeline, tmp_path: Path) -> None:
    resolved = resolve_timeline(conflict_timeline, "trim_head").timeline
    plan = build_frame_plan(resolved)
    assert len(plan) == 300
    assert sum(frame.gap for frame in plan) == 12
    first = render_timeline(resolved, tmp_path / "one", width=64, height=36)
    second = render_timeline(resolved, tmp_path / "two", width=64, height=36)
    assert first["frame_count"] == 300
    assert first["gap_frames"] == 12
    assert first["first_hash"] == second["first_hash"]
    assert first["digest"] == second["digest"]
    assert first["plan_digest"] == second["plan_digest"]
    assert len(list((tmp_path / "one").glob("frame_*.png"))) == 300


def test_render_ignores_track_and_clip_insertion_order(conflict_timeline: Timeline, tmp_path: Path) -> None:
    resolved = resolve_timeline(conflict_timeline, "trim_head").timeline
    reversed_timeline = resolved.clone()
    reversed_timeline.tracks = list(reversed(reversed_timeline.tracks))
    for track in reversed_timeline.tracks:
        track.clips = list(reversed(track.clips))
    one = render_timeline(resolved, tmp_path / "a", width=48, height=24)
    two = render_timeline(reversed_timeline, tmp_path / "b", width=48, height=24)
    assert one["first_hash"] == two["first_hash"]
    assert one["digest"] == two["digest"]
    assert one["plan_digest"] == two["plan_digest"]


def test_format_files_are_equal_after_conversion(conflict_timeline: Timeline, tmp_path: Path) -> None:
    v1_path = tmp_path / "plan.txt"
    v2_path = tmp_path / "plan.json"
    save_timeline(conflict_timeline, v1_path, "v1")
    save_timeline(conflict_timeline, v2_path, "v2")
    assert load_timeline(v1_path).canonical_dict() == load_timeline(v2_path).canonical_dict()
