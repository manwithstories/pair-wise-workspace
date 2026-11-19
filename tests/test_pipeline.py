"""端到端与单元测试：覆盖故障链路与可恢复流程。"""

from fractions import Fraction

import numpy as np
import pytest

from relay_timeline import audio as audio_mod
from relay_timeline.checkpoint import CheckpointStore
from relay_timeline.errors import (
    CheckpointError,
    MetricsExceededError,
    TimebaseError,
    TimelineError,
)
from relay_timeline.manifest import verify_manifest, write_manifest, build_manifest
from relay_timeline.packet_repair import Packet, repair_packets
from relay_timeline.pipeline import (
    PipelineConfig,
    PipelineInterrupted,
    run_pipeline,
)
from relay_timeline.simulate import FAULT_CASES, inject_faults, packetize
from relay_timeline.timebase import TimeBase
from relay_timeline.timeline import Clip, Timeline


@pytest.fixture
def tb():
    return TimeBase()


# ---- timebase ----

def test_timebase_rounding_half_even(tb):
    assert tb.to_ticks(Fraction(1, 2) / tb.ticks_per_second) in (0,)
    assert tb.to_ticks(1.0) == 90_000
    assert tb.frame_ticks(24) == 3750


def test_timebase_rejects_bad_values():
    with pytest.raises(TimebaseError):
        TimeBase(0)
    with pytest.raises(TimebaseError):
        TimeBase(7).frame_ticks(24)  # 无法整除
    with pytest.raises(TimebaseError):
        TimeBase().require_duration(-1)


# ---- packet_repair ----

def _pkts(n, segment=0):
    return [
        Packet(seq=i, segment_id=segment, packet_index=i,
               payload=f"p{i}".encode(), timestamp_ticks=0)
        for i in range(n)
    ]


def test_repair_dedup_and_reorder():
    pkts = _pkts(6)
    stream = [pkts[1], pkts[0], pkts[2], pkts[2], pkts[3], pkts[4], pkts[5]]
    repaired, report = repair_packets(stream, {0: 6})
    assert [p.packet_index for p in repaired] == list(range(6))
    assert report.packet_duplicate_count == 1
    assert report.packet_reorder_count == 1
    assert report.missing_ranges == []


def test_repair_burst_loss_classification():
    pkts = _pkts(12)
    stream = pkts[:2] + pkts[3:5] + pkts[9:]  # 缺 2, 缺 5..8
    _repaired, report = repair_packets(stream, {0: 12})
    assert (0, 5, 8) in report.missing_ranges
    assert report.burst_loss_count == 1
    assert (0, 2, 2) in report.recoverable_gaps
    assert 0 in report.undecodable_segments


# ---- timeline ----

def test_timeline_rejects_invalid(tb):
    with pytest.raises(TimelineError):
        Timeline(tb=tb, duration=100, clips=[
            Clip(track=0, start=0, duration=-5, source_id="a")]).validate()
    with pytest.raises(TimelineError):
        Timeline(tb=tb, duration=100, clips=[
            Clip(track=0, start=50, duration=60, source_id="a")]).validate()
    with pytest.raises(TimelineError):
        Timeline(tb=tb, duration=100, clips=[
            Clip(track=0, start=0, duration=60, source_id="a"),
            Clip(track=0, start=30, duration=40, source_id="b")]).validate()


def test_timeline_transition_weights(tb):
    tl = Timeline(tb=tb, duration=1000, clips=[
        Clip(track=0, start=0, duration=1000, source_id="base"),
        Clip(track=1, start=100, duration=200, source_id="top",
             transition_in=100),
    ])
    tl.validate()
    layers = tl.composition_at(150)
    assert len(layers) == 2
    assert abs(sum(l.weight for l in layers) - 1.0) < 1e-9
    assert layers[-1].source_id == "top"
    assert abs(layers[-1].weight - 0.5) < 1e-9


# ---- checkpoint ----

def test_checkpoint_commit_and_conflicts(tmp_path):
    store = CheckpointStore(tmp_path, "t1", "digest-a")
    store.commit_chunk(0, 0, 10, "c0", b"payload-0")
    assert store.is_committed(0, "c0")
    assert not store.is_committed(0, "other")

    again = CheckpointStore(tmp_path, "t1", "digest-a")
    assert again.committed_count == 1

    with pytest.raises(CheckpointError):
        CheckpointStore(tmp_path, "t2", "digest-a")  # 任务标识冲突
    with pytest.raises(CheckpointError):
        CheckpointStore(tmp_path, "t1", "digest-b")  # 输入摘要不一致

    forced = CheckpointStore(tmp_path, "t1", "digest-b", force_rebuild=True)
    assert forced.committed_count == 0


def test_checkpoint_detects_truncated_output(tmp_path):
    store = CheckpointStore(tmp_path, "t1", "d")
    store.commit_chunk(0, 0, 10, "c0", b"payload-0")
    victim = store.chunk_path(0)
    victim.write_bytes(b"x")  # 截断/篡改
    with pytest.raises(CheckpointError) as exc:
        CheckpointStore(tmp_path, "t1", "d")
    assert exc.value.conflict_blocks == [0]


def test_checkpoint_corrupt_index(tmp_path):
    (tmp_path / "checkpoint.index.json").write_text("{not json")
    with pytest.raises(CheckpointError):
        CheckpointStore(tmp_path, "t1", "d")


# ---- audio ----

def test_audio_offset_and_stats(tb):
    sig = audio_mod.synth_audio("trk", tb.to_ticks(2.0), tb)
    shifted = audio_mod.apply_offset(sig, tb.to_ticks(0.1), tb)
    assert len(shifted) == len(sig) + 4800
    stats = audio_mod.audio_stats(sig)
    assert stats["peak"] > 0.3
    assert stats["rms"] > 0.1
    assert stats["silence_intervals_ms"]  # 中段静音被检出


# ---- pipeline 端到端 ----

def _small_config(out, fault, **kw):
    return PipelineConfig(
        out_dir=out, task_id="t", seed=7, size=(160, 96), duration_s=2.0,
        chunk_frames=24, bit_rate=200_000, fault=fault, **kw,
    )


def test_pipeline_end_to_end_and_resume(tmp_path):
    cfg = _small_config(tmp_path, FAULT_CASES["mixed"],
                        interrupt_after_chunks=1)
    with pytest.raises(PipelineInterrupted):
        run_pipeline(cfg)
    result = run_pipeline(_small_config(tmp_path, FAULT_CASES["mixed"]))
    stats = result.stats
    assert stats["checkpoint_reused_blocks"] > 0
    assert stats["frame_count"] == 48
    assert stats["bitrate_deviation_pct"] <= 8.0
    assert stats["manifest_alignment_error_ms"] <= 1.0
    assert stats["packet_duplicate_count"] > 0
    assert stats["packet_reorder_count"] > 0
    assert verify_manifest(result.manifest_path)["frame_count"] == 48


def test_pipeline_truncated_output_fails_verify(tmp_path):
    result = run_pipeline(_small_config(tmp_path, FAULT_CASES["normal"]))
    victim = tmp_path / "chunk-t-0000.npy"
    data = victim.read_bytes()
    victim.write_bytes(data[: len(data) // 2])
    with pytest.raises(CheckpointError) as exc:
        verify_manifest(result.manifest_path)
    assert exc.value.conflict_blocks == [0]


def test_manifest_rejects_missing_fields(tmp_path):
    path = tmp_path / "m.json"
    write_manifest(path, build_manifest("t", 90_000, 24, [], [], {}))
    with pytest.raises(Exception):
        verify_manifest(path)


def test_fault_injection_deterministic():
    pkts = packetize(0, b"x" * 20000, 0, 0)
    rng1 = np.random.default_rng(3)
    rng2 = np.random.default_rng(3)
    profile = FAULT_CASES["mixed"]
    a = inject_faults(pkts, profile, rng1)
    b = inject_faults(pkts, profile, rng2)
    assert [p.key for p in a] == [p.key for p in b]
