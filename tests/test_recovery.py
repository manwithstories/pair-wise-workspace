"""Recovery behaviour: damage detection, resume equivalence, graph failures.

These are the tests that back the acceptance criteria:

* each damage class is *located* (first fault, ordinal, timestamp) within one
  frame;
* a resumed run reuses completed nodes and produces a byte-identical output
  to a full run;
* cyclic graphs fail before any work; overlapping clips are rejected;
* an upstream failure isolates only its dependants.
"""

from __future__ import annotations

import shutil
from dataclasses import replace

import pytest

from mediacore.audio_io import file_sha256
from mediacore.checkpoint import read_checkpoint
from mediacore.errors import GraphError, TimelineError
from mediacore.graph import build_plan
from mediacore.integrity import structural_integrity
from mediacore.pipeline import Pipeline
from mediacore.pipeline_types import PipelineSettings
from mediacore.probe import probe_track
from mediacore.timeline import ClipSpec, Timeline, TrackSpec, TransitionSpec


def _video_scan(fixtures, name):
    return probe_track(fixtures[name], track_id=f"{name}:video", kind="video")


def test_clean_fixture_has_no_damage(fixtures):
    scan = _video_scan(fixtures, "clean")
    assert scan.damage == []
    assert scan.packet_count == scan.declared_frames == 90
    assert scan.drift.max_abs_us <= 1


def test_truncation_is_located_at_the_last_good_packet(fixtures):
    scan = _video_scan(fixtures, "truncated")
    assert [d.code for d in scan.damage] == ["probe.truncated"]
    fault = scan.damage[0]
    assert fault.context["demuxed_frames"] < fault.context["declared_frames"]
    assert fault.context["locator_error_frames"] == 0
    # The reported PTS is the last decodable frame, within one frame (33 ms).
    assert scan.declared_frames - scan.packet_count > 0
    assert not structural_integrity(fixtures["truncated"])["complete"]


def test_corrupt_packet_is_located_by_ordinal(fixtures):
    scan = _video_scan(fixtures, "corrupt")
    codes = {d.code for d in scan.damage}
    assert "probe.corrupt" in codes
    fault = next(d for d in scan.damage if d.code == "probe.corrupt")
    assert fault.context["locator_error_frames"] == 0
    # Packets before the fault decode; the count is unchanged.
    assert scan.packet_count == scan.declared_frames
    assert structural_integrity(fixtures["corrupt"])["complete"]


def test_index_gap_is_detected_in_a_structurally_complete_file(fixtures):
    scan = _video_scan(fixtures, "index_gap")
    codes = {d.code for d in scan.damage}
    assert "probe.index_gap" in codes
    fault = next(d for d in scan.damage if d.code == "probe.index_gap")
    assert fault.context["locator_error_frames"] == 0
    assert fault.context["demuxed_samples"] < fault.context["declared_samples"]
    assert structural_integrity(fixtures["index_gap"])["complete"]


def test_missing_index_reports_zero_packets(fixtures):
    scan = _video_scan(fixtures, "index_missing")
    codes = {d.code for d in scan.damage}
    assert "probe.index_missing" in codes
    assert scan.packet_count == 0


def test_nonzero_origin_is_normalised(fixtures):
    scan = _video_scan(fixtures, "nonzero_origin")
    assert scan.start_us == 2_500_000
    assert scan.damage == []


# ---------------------------------------------------------------------------
# graph failures
# ---------------------------------------------------------------------------


def test_cycle_is_detected_before_work(fixtures):
    timeline = Timeline.load(fixtures["timeline_cycle"])
    with pytest.raises(GraphError) as info:
        build_plan(timeline)
    assert info.value.code == "graph.cycle"
    assert info.value.context["cycle"][0] == info.value.context["cycle"][-1]


def test_overlap_without_transition_is_rejected(fixtures):
    with pytest.raises(TimelineError) as info:
        Timeline.load(fixtures["timeline_overlap"])
    assert info.value.code == "timeline.overlap"
    assert info.value.context["overlap_us"] == 200_000


def test_out_of_range_clip_is_rejected_with_overshoot():
    timeline = Timeline()
    timeline.add_track(
        TrackSpec(id="v", path="x", kind="video", timebase="1/15360")
    )
    timeline.add_clip(ClipSpec(id="c", track="v", start_us=0, end_us=2_000_000))
    with pytest.raises(TimelineError) as info:
        timeline.validate(durations_us={"v": 1_500_000})
    assert info.value.code == "timeline.out_of_range"
    assert info.value.context["overshoot_us"] == 500_000


def test_empty_clip_is_rejected():
    with pytest.raises(TimelineError) as info:
        ClipSpec(id="c", track="v", start_us=1_000, end_us=1_000)
    assert info.value.code == "timeline.empty_clip"


def test_upstream_failure_isolates_only_dependants(fixtures):
    timeline = Timeline.load(fixtures["timeline_clean"])
    plan = build_plan(timeline)
    plan.mark_done("clip:tactical-a")
    skipped = plan.fail("clip:voice-a", "boom")
    assert "transition:voice-xfade" in skipped
    assert plan.state("clip:voice-b") == "pending"
    assert plan.state("clip:tactical-a") == "done"


# ---------------------------------------------------------------------------
# resume equivalence
# ---------------------------------------------------------------------------


def test_resume_reuses_completed_nodes_and_matches_full_run(fixtures, tmp_path):
    timeline = Timeline.load(fixtures["timeline_clean"])
    settings = PipelineSettings(
        workdir=str(tmp_path / "run"), sample_rate=48_000, layout="stereo"
    )
    first = Pipeline(timeline, settings, stop_after=2).run()
    assert first.interrupted
    assert first.rendered_nodes == 2
    checkpoint = read_checkpoint(tmp_path / "run" / "checkpoint.json")
    assert set(checkpoint.records) == set(first.outcomes)

    resumed = Pipeline(timeline, settings).run()
    assert not resumed.interrupted
    assert resumed.reused_nodes == 2
    assert resumed.rendered_nodes == 2

    full_settings = PipelineSettings(
        workdir=str(tmp_path / "full"), sample_rate=48_000, layout="stereo"
    )
    full = Pipeline(timeline, full_settings).run()
    assert resumed.output_sha256 == full.output_sha256
    assert resumed.output_samples == full.output_samples
    assert file_sha256(resumed.output_path) == resumed.output_sha256


def test_changed_source_invalidates_checkpoint(fixtures, tmp_path):
    timeline = Timeline.load(fixtures["timeline_clean"])
    settings = PipelineSettings(
        workdir=str(tmp_path / "run"), sample_rate=48_000, layout="stereo"
    )
    Pipeline(timeline, settings).run()
    checkpoint = read_checkpoint(tmp_path / "run" / "checkpoint.json")
    assert checkpoint is not None and checkpoint.records

    # Rewrite the timeline to point at a different source file: every digest
    # that folds in the source content hash must change.
    other = Timeline.load(fixtures["timeline_clean"])
    other.tracks["voice"] = replace(
        other.tracks["voice"], path=str(fixtures["nonzero_origin"])
    )
    other.validate(durations_us={t: 3_000_000 for t in other.tracks})
    again = Pipeline(other, settings).run()
    assert again.reused_nodes == 0


def test_corrupt_checkpoint_restarts_cleanly(fixtures, tmp_path):
    timeline = Timeline.load(fixtures["timeline_clean"])
    settings = PipelineSettings(
        workdir=str(tmp_path / "run"), sample_rate=48_000, layout="stereo"
    )
    (tmp_path / "run").mkdir()
    (tmp_path / "run" / "checkpoint.json").write_text("{not json", encoding="utf-8")
    result = Pipeline(timeline, settings).run()
    assert result.reused_nodes == 0
    assert result.output_sha256


def test_damaged_input_recovers_and_reports(fixtures, tmp_path):
    timeline = Timeline.load(fixtures["timeline_clean"])
    truncated = track_path(fixtures, "truncated")
    timeline.tracks["voice"] = replace(timeline.tracks["voice"], path=str(truncated))
    timeline.tracks["tactical"] = replace(
        timeline.tracks["tactical"], path=str(truncated)
    )
    timeline.clips["voice-a"] = replace(timeline.clips["voice-a"], end_us=1_200_000)
    timeline.clips["voice-b"] = replace(
        timeline.clips["voice-b"], start_us=1_000_000, end_us=2_000_000
    )
    timeline.clips["tactical-a"] = replace(
        timeline.clips["tactical-a"], end_us=2_000_000
    )
    settings = PipelineSettings(
        workdir=str(tmp_path / "run"), sample_rate=48_000, layout="stereo"
    )
    result = Pipeline(timeline, settings).run()
    assert result.damaged
    assert "truncated" in result.damage.damage_kinds()
    assert result.output_samples > 0
    assert len(result.output_sha256) == 64


# ---------------------------------------------------------------------------
# fixture determinism
# ---------------------------------------------------------------------------


def test_fixture_generation_is_reproducible(fixtures, tmp_path):
    from mediacore.testdata import generate_fixtures

    clone = generate_fixtures(tmp_path / "clone", force=True)
    for name in ("clean", "truncated", "corrupt", "index_gap"):
        assert file_sha256(fixtures[name]) == file_sha256(clone[name])


def track_path(fixtures, name):
    return fixtures[name]
