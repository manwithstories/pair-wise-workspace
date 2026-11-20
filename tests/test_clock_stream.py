"""Unified media clock (RTCP SR mapping) and bounded-queue backpressure."""

from __future__ import annotations

import threading
import time

import pytest

from archiveflow.clock import (CONFLICT_TOLERANCE_S, ClockDomain, MediaClock)
from archiveflow.errors import CancelledError
from archiveflow.rtp import RtcpSR
from archiveflow.stream import BoundedQueue, Pipeline

RATE = 90_000


def sr(ssrc: int, ntp: float, rtp_ts: int) -> RtcpSR:
    return RtcpSR(ssrc=ssrc, ntp_seconds=ntp, rtp_timestamp=rtp_ts)


class TestMediaClock:
    def test_origin_anchors_zero_and_never_negative(self):
        clk = MediaClock(RATE)
        clk.set_origin(10_000)
        assert clk.map(10_000).clock_ms == 0.0
        # An earlier timestamp (reordered) clamps to zero, never negative.
        assert clk.map(9_000).clock_ms == 0.0

    def test_mapping_matches_90khz_rate(self):
        clk = MediaClock(RATE)
        clk.set_origin(0)
        sample = clk.map(RATE // 2)  # half a second
        assert sample.clock_ms == pytest.approx(500.0)

    def test_missing_sender_report_adds_uncertainty_once(self):
        clk = MediaClock(RATE)
        clk.set_origin(0)
        first = clk.map(0).uncertainty_ms
        assert first > 0
        again = clk.map(1000).uncertainty_ms
        assert again == first  # not re-added on every map

    def test_conflicting_report_keeps_last_trusted_mapping(self):
        clk = MediaClock(RATE)
        clk.add_sender_report(sr(1, 100.0, 0), 0)
        trusted = clk.map(0).clock_ms
        # Same RTP timestamp but NTP shifted beyond the conflict tolerance,
        # so the implied RTP<->NTP offset jumps by more than the threshold.
        clk.add_sender_report(
            sr(1, 100.0 + CONFLICT_TOLERANCE_S + 1.0, 0), 0)
        assert clk.conflicts == 1
        assert clk.map(0).clock_ms == trusted  # mapping unchanged
        assert clk.uncertainty_ms > 0

    def test_consistent_reports_refine_without_conflict(self):
        clk = MediaClock(RATE)
        clk.add_sender_report(sr(1, 100.0, 0), 0)
        clk.add_sender_report(sr(1, 101.0, RATE), RATE)
        assert clk.conflicts == 0
        assert clk.uncertainty_ms == 0

    def test_non_finite_ntp_rejected(self):
        clk = MediaClock(RATE)
        with pytest.raises(Exception):
            clk.add_sender_report(sr(1, float("nan"), 0), 0)

    def test_non_finite_clock_output_rejected(self):
        clk = MediaClock(RATE)
        clk.set_origin(0)
        # A finite RTP timestamp always maps to a finite, non-negative ms.
        sample = clk.map(RATE)
        assert sample.clock_ms >= 0.0
        assert sample.clock_ms == 1000.0

    def test_clock_domain_tracks_per_ssrc_and_max_uncertainty(self):
        domain = ClockDomain()
        domain.clock_for(1).note_missing_reports()
        domain.clock_for(2).note_missing_reports()
        assert domain.max_uncertainty_ms() > 0


class TestBoundedQueue:
    def test_put_get_roundtrip_and_peak_tracking(self):
        q = BoundedQueue(4)
        for i in range(4):
            q.put(i)
        assert q.peak_items == 4
        assert [q.get(timeout=0.1) for _ in range(4)] == [0, 1, 2, 3]

    def test_backpressure_blocks_when_full(self):
        q = BoundedQueue(1)
        q.put("a")
        drained = threading.Event()

        def slow_consumer():
            time.sleep(0.15)
            q.get(timeout=1)
            drained.set()

        t = threading.Thread(target=slow_consumer)
        t.start()
        start = time.monotonic()
        q.put("b")  # must block until the consumer frees a slot
        assert time.monotonic() - start >= 0.1
        assert drained.is_set()
        t.join()

    def test_cancel_unblocks_put_with_cancelled_error(self):
        cancel = threading.Event()
        q = BoundedQueue(1, cancel)
        q.put("full")

        def cancel_soon():
            time.sleep(0.1)
            cancel.set()

        threading.Thread(target=cancel_soon).start()
        with pytest.raises(CancelledError):
            q.put("blocked")

    def test_iteration_stops_on_close(self):
        q = BoundedQueue(4)
        q.put(1)
        q.put(2)
        q.close()
        assert list(q) == [1, 2]


class TestPipeline:
    def test_happy_path_collects_items(self):
        pipe = Pipeline(queue_size=4)
        received = []

        def producer(q):
            for i in range(10):
                q.put(i)

        def consumer(items):
            received.extend(items)

        stats = pipe.run(producer, consumer)
        assert received == list(range(10))
        assert stats.peak_queue_items <= 4
        assert not stats.cancelled

    def test_producer_error_cancels_and_propagates(self):
        pipe = Pipeline(queue_size=2)

        def producer(q):
            q.put(0)
            raise RuntimeError("boom")

        with pytest.raises(RuntimeError, match="boom"):
            pipe.run(producer, lambda items: list(items))
        assert pipe.stats.cancelled
        assert pipe.stats.stage_errors

    def test_slow_consumer_exerts_backpressure_without_loss(self):
        pipe = Pipeline(queue_size=2)

        def producer(q):
            for i in range(8):
                q.put(i)

        def consumer(items):
            out = []
            for item in items:
                time.sleep(0.01)  # slower than the producer
                out.append(item)
            assert out == list(range(8))

        stats = pipe.run(producer, consumer)
        assert stats.peak_queue_items <= 2  # memory stayed bounded
