"""Offline vs streaming equivalence at the sample, curve and stat level."""

from __future__ import annotations

import numpy as np
import pytest

from src.mixer.automation import AutomationCurve
from src.mixer.engine import MixSession, OfflineEngine, StreamingEngine
from src.mixer.model import AutomationPoint

MAX_ERROR = 1e-5


def test_offline_and_stream_match_project(project):
    offline = OfflineEngine(project).render()
    for block in (256, 1024):
        stream = StreamingEngine(project, block_size=block).render()
        assert stream.audio.shape == offline.audio.shape
        assert stream.timeline_frames == offline.timeline_frames
        error = float(np.max(np.abs(offline.audio - stream.audio)))
        assert error <= MAX_ERROR, f"block {block} drifted: {error:.3e}"


def test_mse_and_clip_counts_agree(project):
    offline = OfflineEngine(project).render()
    stream = StreamingEngine(project, block_size=256).render()
    diff = offline.audio - stream.audio
    assert float(np.mean(np.square(diff))) <= MAX_ERROR**2
    assert offline.limiter["clip_count"] == stream.limiter["clip_count"]
    assert offline.limiter["peak_out_dbfs"] == pytest.approx(
        stream.limiter["peak_out_dbfs"], abs=1e-3
    )


def test_streaming_handles_short_empty_and_tail_chunks(project):
    offline = OfflineEngine(project).render()
    total = offline.timeline_frames
    plan = [1, 0, 3, 0, 257, 0, 1000]
    while sum(plan) < total:
        plan.append(min(256, total - sum(plan)))
    stream = StreamingEngine(project, block_size=256, chunk_sizes=plan).render()
    assert stream.audio.shape[0] == total
    assert float(np.max(np.abs(offline.audio - stream.audio))) <= MAX_ERROR


def test_empty_chunks_do_not_advance_state(project):
    offline = OfflineEngine(project).render()
    total = offline.timeline_frames
    with_empty = [0] * 5 + [total]
    result = StreamingEngine(project, block_size=256, chunk_sizes=with_empty).render()
    assert float(np.max(np.abs(offline.audio - result.audio))) <= MAX_ERROR


def test_short_plan_is_extended_to_cover_the_timeline(project):
    """A short pattern repeats, then a tail closes the remaining frames."""
    offline = OfflineEngine(project).render()
    total = offline.timeline_frames
    engine = StreamingEngine(project, block_size=256, chunk_sizes=[100, 100])
    chunks = engine.plan(total)
    assert sum(chunk.frames for chunk in chunks) == total
    result = engine.render()
    assert result.audio.shape[0] == total
    assert float(np.max(np.abs(offline.audio - result.audio))) <= MAX_ERROR


def test_negative_chunk_size_is_rejected(project):
    from src.mixer.errors import MixerError

    with pytest.raises(MixerError) as excinfo:
        StreamingEngine(project, block_size=256, chunk_sizes=[100, -1]).plan(1000)
    assert excinfo.value.code == "invalid_block_plan"


def test_curve_sampling_is_block_size_independent():
    points = [
        AutomationPoint(0.0, 0.0),
        AutomationPoint(0.01, 12.0),
        AutomationPoint(0.35, -6.0),
        AutomationPoint(0.8, 3.0),
    ]
    curve = AutomationCurve(points, default_db=0.0, max_slew_db_per_s=300.0, rate=48000)
    frames = 40000
    whole = curve.sample_db(frames, 48000, 0)
    for size in (256, 1024, 97):
        chunks = []
        position = 0
        while position < frames:
            count = min(size, frames - position)
            chunks.append(curve.sample_db(count, 48000, position))
            position += count
        stacked = np.concatenate(chunks)
        assert float(np.max(np.abs(whole - stacked))) == 0.0


def test_slew_limit_caps_segment_slope():
    points = [AutomationPoint(0.0, 0.0), AutomationPoint(0.05, 18.0)]
    curve = AutomationCurve(points, max_slew_db_per_s=60.0, rate=48000)
    assert curve.max_segment_slope() <= 60.0 + 1e-9
    values = curve.sample_db(4800, 48000, 0)
    per_sample = np.abs(np.diff(values))
    assert float(per_sample.max()) <= 60.0 / 48000 + 1e-9


def test_block_boundary_continuity_is_small(project):
    with MixSession(project) as session:
        session.timeline_frames()
        for block in (256, 1024):
            jumps = session.check_continuity(block)
            assert set(jumps) == {"drums", "bass", "pad", "fx"}
            assert max(jumps.values()) < 0.01


def test_track_statistics_are_identical_between_paths(project):
    offline = OfflineEngine(project).render()
    stream = StreamingEngine(project, block_size=1024).render()
    for left, right in zip(offline.stats, stream.stats):
        assert left.name == right.name
        assert left.frames == right.frames
        assert left.peak_in_dbfs == pytest.approx(right.peak_in_dbfs, abs=1e-4)
        assert left.rms_dbfs == pytest.approx(right.rms_dbfs, abs=1e-4)
        assert left.clip_count == right.clip_count
        assert left.max_gain_delta_db == pytest.approx(right.max_gain_delta_db, abs=1e-6)


def test_automation_point_counts_survive_dedup(project):
    counts = {track.name: len(track.automation) for track in project.tracks}
    assert counts == {"drums": 3, "bass": 2, "pad": 2, "fx": 2}
    with MixSession(project) as session:
        session.timeline_frames()
        for track in project.tracks:
            curve = session._runtimes[track.name].evaluator.curve
            assert curve.point_count == len(track.automation)
