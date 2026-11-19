"""Pipeline tests: round-trip fidelity, backpressure, cancel and resume.

These are the tests that justify the architecture.  Each one asserts a property
the operator depends on, not an implementation detail:

* a faithful copy is byte-identical per segment, across every format pair;
* a slow consumer cannot make the queue grow past its configured ceiling;
* cancellation lands on a real segment boundary and never duplicates packets on
  resume.
"""

from __future__ import annotations

import threading
from collections import Counter
from pathlib import Path

import pytest

from aircheck.demux import Demuxer
from aircheck.hashing import compare_hashes, compare_timeline
from aircheck.mapping import resolve_mapping
from aircheck.pipeline import (
    Pipeline,
    PipelineConfig,
    compare_roundtrip,
    hash_demuxed,
    hash_demuxed_with_plan,
    part_path,
    read_checkpoint,
)
from aircheck.probe import probe

SEGMENT = 250_000_000


def _demux_all(path: Path, segment_ticks: int = SEGMENT):
    return list(Demuxer(probe=probe(path), segment_ticks=segment_ticks).iter_packets())


class TestRoundTrip:
    @pytest.mark.parametrize("target", ["mp4", "mov", "mkv"])
    def test_mp4_source_roundtrips_to_every_container(self, multi_mp4, workdir, target):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / f"roundtrip_mp4_to_{target}.{target}"
        pipeline = Pipeline(
            source_probe,
            mapping,
            output,
            PipelineConfig(segment_ticks=SEGMENT, target_format=target),
        )
        stage = pipeline.run()
        assert stage.ok, stage.errors
        comparison, drift, _plan = compare_roundtrip(
            _demux_all(multi_mp4.path), _demux_all(stage.output)
        )
        assert comparison.all_match, comparison.mismatched
        assert all(item["within_tolerance"] for item in drift)

    @pytest.mark.parametrize("target", ["mp4", "mov", "mkv"])
    def test_mov_source_roundtrips_to_every_container(self, multi_mov, workdir, target):
        source_probe = probe(multi_mov.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / f"roundtrip_mov_to_{target}.{target}"
        stage = Pipeline(
            source_probe,
            mapping,
            output,
            PipelineConfig(segment_ticks=SEGMENT, target_format=target),
        ).run()
        assert stage.ok, stage.errors
        comparison, _drift, _plan = compare_roundtrip(
            _demux_all(multi_mov.path), _demux_all(stage.output)
        )
        assert comparison.all_match

    def test_partial_mapping_writes_only_selected_streams(self, multi_mp4, workdir):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe, "v:0=a:2")
        output = workdir / "mapped.mkv"
        stage = Pipeline(
            source_probe, mapping, output, PipelineConfig(segment_ticks=SEGMENT)
        ).run()
        assert stage.ok, stage.errors
        output_streams = probe(stage.output).streams
        assert len(output_streams) == 2
        assert sorted(stream.kind for stream in output_streams) == ["audio", "video"]

    def test_output_is_never_overwritten(self, multi_mp4, workdir):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / "exists.mkv"
        output.write_bytes(b"sentinel")
        stage = Pipeline(
            source_probe, mapping, output, PipelineConfig(segment_ticks=SEGMENT)
        ).run()
        assert not stage.ok
        assert stage.errors[0]["category"] == "input"
        assert output.read_bytes() == b"sentinel"


    def test_cross_container_copy_keeps_every_video_packet(self, multi_mkv, workdir):
        # Regression: a rebuilt packet without ``duration`` makes the MP4 muxer
        # silently drop a track's final sample, so an "exact" copy loses one
        # packet.  Every video packet must survive MKV -> MP4.
        source_probe = probe(multi_mkv.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / "count_check.mp4"
        stage = Pipeline(
            source_probe, mapping, output, PipelineConfig(segment_ticks=SEGMENT,
                                                          target_format="mp4")
        ).run()
        assert stage.ok, stage.errors
        source_video = [
            packet for packet in _demux_all(multi_mkv.path) if packet.stream_index == 0
        ]
        output_video = [
            packet for packet in _demux_all(stage.output) if packet.stream_index == 0
        ]
        assert len(output_video) == len(source_video)
        assert [packet.payload for packet in output_video] == [
            packet.payload for packet in source_video
        ]


    def test_unwritable_output_directory_is_reported_with_context(self, multi_mp4, workdir):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        blocker = workdir / "blocker"
        blocker.write_text("not a directory")
        stage = Pipeline(
            source_probe,
            mapping,
            blocker / "sub" / "out.mkv",
            PipelineConfig(segment_ticks=SEGMENT),
        ).run()
        assert not stage.ok
        assert stage.errors[0]["category"] == "input"
        assert "output directory" in stage.errors[0]["message"]


class TestBackpressure:
    def test_peak_queue_depth_never_exceeds_capacity(self, multi_mp4, workdir):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        stage = Pipeline(
            source_probe,
            mapping,
            workdir / "backpressure.mkv",
            PipelineConfig(segment_ticks=SEGMENT, queue_size=2, slow_consumer_ms=3),
        ).run()
        assert stage.ok
        for name, stats in stage.queue_stats.items():
            assert stats.peak_items <= 2, (name, stats.peak_items)
            assert stats.blocked_puts > 0, f"{name} never applied backpressure"

    def test_queue_size_one_still_completes(self, multi_mp4, workdir):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        stage = Pipeline(
            source_probe,
            mapping,
            workdir / "q1.mkv",
            PipelineConfig(segment_ticks=SEGMENT, queue_size=1, slow_consumer_ms=1),
        ).run()
        assert stage.ok, stage.errors
        assert max(stats.peak_items for stats in stage.queue_stats.values()) == 1

    def test_byte_ceiling_is_enforced(self, multi_mp4, workdir):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        # A tiny byte budget forces the queue to block on bytes, not just count.
        stage = Pipeline(
            source_probe,
            mapping,
            workdir / "bytes.mkv",
            PipelineConfig(segment_ticks=SEGMENT, queue_size=64, queue_bytes=8_000),
        ).run()
        assert stage.ok, stage.errors
        for stats in stage.queue_stats.values():
            assert stats.peak_bytes <= 8_000 + 12_000  # one oversized packet allowed


class TestCancellation:
    def test_cancel_records_a_committed_boundary_and_writes_checkpoint(
        self, multi_mp4, workdir
    ):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / "cancelled.mkv"
        cancel = threading.Event()
        pipeline = Pipeline(
            source_probe,
            mapping,
            output,
            PipelineConfig(segment_ticks=SEGMENT, queue_size=2, slow_consumer_ms=3),
            cancel=cancel,
        )
        threading.Timer(0.05, cancel.set).start()
        stage = pipeline.run()
        assert stage.cancelled
        assert not stage.ok
        assert stage.errors[0]["category"] == "cancelled"
        checkpoint = read_checkpoint(output)
        assert checkpoint is not None
        assert checkpoint.complete is False
        assert checkpoint.last_committed_segment >= -1
        # A cancelled run never leaves a part at the uncommitted boundary.
        if checkpoint.last_committed_segment < 0:
            assert not output.exists()

    def test_resume_after_cancel_covers_every_packet_once(self, multi_mp4, workdir):
        source_probe = probe(multi_mp4.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / "resume.mkv"
        cancel = threading.Event()
        first = Pipeline(
            source_probe,
            mapping,
            output,
            PipelineConfig(segment_ticks=SEGMENT, queue_size=2, slow_consumer_ms=2),
            cancel=cancel,
        )
        threading.Timer(0.05, cancel.set).start()
        first_stage = first.run()
        assert first_stage.cancelled

        second = Pipeline(
            source_probe,
            mapping,
            output,
            PipelineConfig(segment_ticks=SEGMENT, queue_size=8, resume=True),
        )
        second_stage = second.run()
        assert second_stage.ok, second_stage.errors

        source_payloads = [packet.payload for packet in _demux_all(multi_mp4.path)]
        assembled: list[bytes] = []
        segment_labels: list[int] = []
        for index in range(8):
            candidate = part_path(output, index)
            if not candidate.exists():
                continue
            part_packets = _demux_all(candidate)
            assembled.extend(packet.payload for packet in part_packets)
            segment_labels.extend(
                sorted({packet.segment for packet in part_packets})
            )
        # Equal length plus an equal multiset means every payload appears
        # exactly as often as in the source: no packet lost, none duplicated.
        assert len(assembled) == len(source_payloads)
        assert Counter(assembled) == Counter(source_payloads)

    def test_resume_without_checkpoint_starts_from_zero(self, multi_mkv, workdir):
        source_probe = probe(multi_mkv.path)
        mapping = resolve_mapping(source_probe)
        output = workdir / "no_checkpoint.mkv"
        stage = Pipeline(
            source_probe,
            mapping,
            output,
            PipelineConfig(segment_ticks=SEGMENT, queue_size=4, resume=True),
        ).run()
        assert stage.ok, stage.errors
        assert stage.resumed_from_segment is None
        assert any("no checkpoint" in note for note in stage.notes)


class TestHashing:
    def test_plan_regroups_output_by_stream_ordinal(self, multi_mp4):
        packets = _demux_all(multi_mp4.path)
        expected, plan = hash_demuxed(packets)
        # Hashing the same packets with the plan must reproduce the digests.
        actual = hash_demuxed_with_plan(packets, plan)
        assert compare_hashes(expected, actual).all_match

    def test_mutated_payload_is_detected(self, multi_mp4):
        packets = _demux_all(multi_mp4.path)
        expected, plan = hash_demuxed(packets)
        tampered = list(packets)
        target = next(packet for packet in tampered if packet.segment == 0)
        mutated_payload = bytes([target.payload[0] ^ 0xFF]) + target.payload[1:]
        tampered[tampered.index(target)] = target.__class__(
            stream_index=target.stream_index,
            segment=target.segment,
            pts_ticks=target.pts_ticks,
            dts_ticks=target.dts_ticks,
            time_base=target.time_base,
            is_keyframe=target.is_keyframe,
            size=target.size,
            pos=target.pos,
            payload=mutated_payload,
            pts_raw=target.pts_raw,
            dts_raw=target.dts_raw,
        )
        actual = hash_demuxed_with_plan(tampered, plan)
        comparison = compare_hashes(expected, actual)
        assert not comparison.all_match
        assert comparison.mismatched[0]["segment"] == 0

    def test_timeline_drift_tolerates_container_rounding(self, multi_mp4, multi_mkv):
        drifts = compare_timeline(_demux_all(multi_mp4.path), _demux_all(multi_mkv.path))
        assert all(item.within_tolerance for item in drifts)
