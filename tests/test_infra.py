"""Tests for the bounded queue and the fixture/MKV writers."""

from __future__ import annotations

import threading
import time

import pytest

from aircheck.fixtures import (
    bad_index_asset,
    disorder_timestamp_asset,
    wrap_timestamp_asset,
)
from aircheck.mkvwrite import (
    MkvBlock,
    MkvCluster,
    MkvCue,
    MkvTrack,
    build_mkv,
    pcm_silence,
)
from aircheck.probe import probe
from aircheck.queueing import BoundedQueue
from aircheck.timebase import CancelledError


class TestBoundedQueue:
    def test_blocks_producer_when_full(self):
        queue: BoundedQueue[int] = BoundedQueue(2, name="test")
        queue.put(1)
        queue.put(2)
        assert queue.qsize() == 2
        assert queue.stats.peak_items == 2
        # A third put must block; release it by consuming in another thread.
        released = threading.Event()

        def consume():
            time.sleep(0.05)
            queue.get()
            released.set()

        threading.Thread(target=consume, daemon=True).start()
        assert released.wait(0) is False
        queue.put(3)  # blocks until the consumer took one
        assert released.is_set()
        assert queue.stats.blocked_puts == 1

    def test_cancellation_wakes_a_blocked_producer(self):
        cancel = threading.Event()
        queue: BoundedQueue[int] = BoundedQueue(1, name="test", cancel=cancel)
        queue.put(1)
        threading.Timer(0.05, cancel.set).start()
        with pytest.raises(CancelledError):
            queue.put(2)

    def test_cancellation_wakes_a_blocked_consumer(self):
        cancel = threading.Event()
        queue: BoundedQueue[int] = BoundedQueue(1, name="test", cancel=cancel)
        threading.Timer(0.05, cancel.set).start()
        with pytest.raises(CancelledError):
            queue.get()

    def test_byte_ceiling_limits_depth(self):
        queue: BoundedQueue[bytes] = BoundedQueue(
            100, name="test", capacity_bytes=1000, item_size=len
        )
        for _ in range(10):
            queue.put(b"x" * 100)
        assert queue.bytes_held <= 1000
        assert queue.stats.peak_bytes <= 1000

    def test_oversized_item_is_admitted_alone(self):
        queue: BoundedQueue[bytes] = BoundedQueue(
            100, name="test", capacity_bytes=10, item_size=len
        )
        # A single item larger than the byte budget must not deadlock.
        queue.put(b"x" * 50)
        assert queue.get() == b"x" * 50

    def test_close_releases_consumer(self):
        queue: BoundedQueue[int] = BoundedQueue(1, name="test")
        threading.Timer(0.05, queue.close).start()
        with pytest.raises(Exception):
            queue.get()

    def test_invalid_capacity_rejected(self):
        with pytest.raises(ValueError):
            BoundedQueue(0)

    def test_stats_are_reported(self):
        queue: BoundedQueue[int] = BoundedQueue(4, name="test")
        queue.put(1)
        queue.get()
        payload = queue.stats.to_dict()
        assert payload["capacity_items"] == 4
        assert payload["puts"] == 1
        assert payload["gets"] == 1


class TestMkvWriter:
    def test_minimal_file_is_readable(self, workdir):
        tracks = [MkvTrack(number=1, kind="audio")]
        clusters = [
            MkvCluster(
                timecode_ms=index * 20,
                blocks=[MkvBlock(track=1, timecode_ms=index * 20,
                                 keyframe=True, payload=pcm_silence(960))],
            )
            for index in range(10)
        ]
        path = workdir / "minimal.mkv"
        path.write_bytes(build_mkv(tracks, clusters, duration_ms=200))
        result = probe(path)
        assert len(result.streams) == 1
        assert result.streams[0].sample_rate == 48000
        assert sum(result.packet_counts.values()) == 10
        assert result.has_index is False  # no cues written

    def test_cues_make_the_file_seekable(self, workdir):
        tracks = [MkvTrack(number=1, kind="audio")]
        clusters = []
        cues = []
        for index in range(10):
            timecode = index * 100
            clusters.append(
                MkvCluster(timecode_ms=timecode,
                           blocks=[MkvBlock(track=1, timecode_ms=timecode,
                                            keyframe=True, payload=pcm_silence(480))])
            )
            cues.append(MkvCue(track=1, timecode_ms=timecode, cluster_index=index))
        path = workdir / "with_cues.mkv"
        path.write_bytes(build_mkv(tracks, clusters, cues=cues, duration_ms=1000))
        result = probe(path)
        assert result.has_index
        assert result.index_info["cue_count"] == 10

    def test_block_outside_cluster_range_is_rejected(self):
        cluster = MkvCluster(
            timecode_ms=0,
            blocks=[MkvBlock(track=1, timecode_ms=100_000, keyframe=True,
                             payload=b"x")],
        )
        with pytest.raises(ValueError):
            build_mkv([MkvTrack(number=1, kind="audio")], [cluster])


class TestDamagedFixtureGenerators:
    def test_wrap_fixture_has_ascending_cues_but_wrapping_blocks(self, workdir):
        fixture = wrap_timestamp_asset(workdir / "wrap.mkv")
        result = probe(fixture.path)
        assert "wrap" in {issue.kind for issue in result.issues}
        assert result.index_info["problems"] == []

    def test_disorder_fixture_flags_non_monotonic(self, workdir):
        fixture = disorder_timestamp_asset(workdir / "disorder.mkv")
        result = probe(fixture.path)
        assert "non_monotonic" in {issue.kind for issue in result.issues}

    def test_bad_index_fixture_reports_three_cue_problems(self, workdir):
        fixture = bad_index_asset(workdir / "bad.mkv")
        result = probe(fixture.path)
        problems = set(result.index_info["problems"])
        assert problems == {"duplicate_cue", "cue_out_of_order", "cue_past_eof"}

    def test_fixtures_are_deterministic(self, workdir):
        first = wrap_timestamp_asset(workdir / "wrap_a.mkv").path.read_bytes()
        second = wrap_timestamp_asset(workdir / "wrap_b.mkv").path.read_bytes()
        assert first == second
