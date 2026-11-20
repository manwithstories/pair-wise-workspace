"""Recovery tests: retransmission, boundary repair, gap marking and propagation."""

from __future__ import annotations

import pytest

from mediasync.errors import PlanError
from mediasync.jitter import MissingSequence, TransportPacket
from mediasync.recover import (
    RecoveryConfig,
    RetransmitOracle,
    recover_interval,
    recover_timeline,
)
from mediasync.timeline import Interval, Timeline


def packets(seqs, step: int = 3000):
    return [
        TransportPacket(
            seq=seq,
            arrival_ms=seq * 10,
            data=bytes([seq % 256]) * 4,
            pts_ticks=seq * step,
            duration_ticks=step,
        )
        for seq in seqs
    ]


def config(**overrides) -> RecoveryConfig:
    base = dict(seed=17, max_retransmit_attempts=4, retransmit_success_rate=1.0)
    base.update(overrides)
    return RecoveryConfig(**base)


def timeline_with(*intervals: Interval) -> Timeline:
    timeline = Timeline()
    for item in intervals:
        timeline.add(item)
    timeline.validate()
    return timeline


def test_successful_retransmission_completes_the_segment() -> None:
    interval = Interval("v@seg0", "cam", 0, 30000)
    ordered = packets([0, 1, 2, 3, 5, 6, 7, 8, 9])
    missing = [MissingSequence(4, 50, 5)]
    result = recover_interval(
        interval, ordered=ordered, missing=missing, config=config()
    )
    assert result.segment.complete
    assert result.segment.recovered_seqs == (4,)
    assert not result.segment.loss_seqs
    assert not result.gaps
    assert result.retransmissions[0].succeeded


def test_failed_retransmission_marks_gap() -> None:
    interval = Interval("v@seg0", "cam", 0, 30000)
    ordered = packets([0, 1, 2, 3, 5, 6, 7, 8, 9])
    missing = [MissingSequence(4, 50, 5)]
    result = recover_interval(
        interval,
        ordered=ordered,
        missing=missing,
        config=config(max_retransmit_attempts=0),
    )
    assert not result.segment.complete
    assert result.segment.loss_seqs == (4,)
    assert len(result.gaps) == 1
    gap = result.gaps[0]
    assert gap.start_ticks == 12000
    assert gap.end_ticks == 15000
    assert gap.reason == "packet-loss"


def test_retransmission_is_order_independent() -> None:
    config_ = config()
    oracle = RetransmitOracle(config_)
    outcomes = {
        (seq, attempt): oracle.attempt("cam", seq, attempt)
        for seq in range(50)
        for attempt in (1, 2)
    }
    shuffled = RetransmitOracle(config_)
    for seq in reversed(range(50)):
        for attempt in (2, 1):
            assert shuffled.attempt("cam", seq, attempt) == outcomes[(seq, attempt)]


def test_boundary_repair_clamps_to_last_usable_packet() -> None:
    interval = Interval("v@seg0", "cam", 0, 30000)
    ordered = packets(range(10))
    result = recover_interval(
        interval,
        ordered=ordered,
        missing=[],
        truncated_seqs=range(8, 10),
        config=config(),
    )
    assert result.repairs
    assert result.segment.end_ticks == 24000
    assert not result.segment.complete
    assert result.segment.reason == "truncated-index-boundary"
    assert result.gaps[0].reason == "truncated-index-boundary"


def test_boundary_repair_respects_budget() -> None:
    interval = Interval("v@seg0", "cam", 0, 30000)
    ordered = packets(range(10))
    result = recover_interval(
        interval,
        ordered=ordered,
        missing=[],
        truncated_seqs=range(1, 10),
        config=config(boundary_budget_ticks=100),
    )
    # A clamp would drop more than the budget allows, so the whole span is
    # marked unrecoverable instead of being silently shortened.
    assert not result.segment.complete
    assert result.repairs and result.repairs[0].reason == "unrecoverable-tail"


def test_holes_are_attributed_to_one_interval_only() -> None:
    timeline = timeline_with(
        Interval("a", "cam", 0, 15000),
        Interval("b", "cam", 15000, 30000, depends_on=("a",)),
    )
    ordered = packets([0, 1, 2, 3, 5, 6, 7, 9])
    missing = [MissingSequence(4, 50, 5), MissingSequence(8, 90, 9)]
    cfg = config(max_retransmit_attempts=0)
    first = recover_interval(timeline.intervals["a"], ordered=ordered, missing=missing, config=cfg)
    second = recover_interval(timeline.intervals["b"], ordered=ordered, missing=missing, config=cfg)
    assert first.segment.loss_seqs == (4,)
    assert second.segment.loss_seqs == (8,)
    report = recover_timeline(timeline, {"a": first, "b": second})
    assert len(report.gaps) == 2


def test_incomplete_prerequisite_blocks_dependent() -> None:
    timeline = timeline_with(
        Interval("a", "cam", 0, 15000),
        Interval("b", "cam", 15000, 30000, depends_on=("a",)),
    )
    ordered = packets([0, 1, 2, 3, 5, 6, 7, 8, 9])
    cfg = config(max_retransmit_attempts=0)
    first = recover_interval(
        timeline.intervals["a"], ordered=ordered, missing=[MissingSequence(4, 50, 5)], config=cfg
    )
    second = recover_interval(timeline.intervals["b"], ordered=ordered, missing=[], config=cfg)
    report = recover_timeline(timeline, {"a": first, "b": second})
    dependent = next(item for item in report.segments if item.interval_id == "b")
    assert not dependent.complete
    assert dependent.reason == "prerequisite-incomplete"
    assert report.propagated == [{"interval_id": "b", "blocked_by": ["a"]}]


def test_recovery_requires_every_interval() -> None:
    timeline = timeline_with(Interval("a", "cam", 0, 1000))
    with pytest.raises(PlanError):
        recover_timeline(timeline, {})


def test_recovery_rejects_invalid_bounds() -> None:
    interval = Interval.__new__(Interval)  # bypass __post_init__ for the check
    object.__setattr__(interval, "interval_id", "x")
    object.__setattr__(interval, "track_id", "cam")
    object.__setattr__(interval, "start_ticks", 10)
    object.__setattr__(interval, "end_ticks", 10)
    object.__setattr__(interval, "kind", "video")
    object.__setattr__(interval, "depends_on", ())
    with pytest.raises(PlanError):
        recover_interval(interval, ordered=[], missing=[], config=config())


def test_recovery_config_validation() -> None:
    from mediasync.errors import InputError

    with pytest.raises(InputError):
        RecoveryConfig(max_retransmit_attempts=-1)
    with pytest.raises(InputError):
        RecoveryConfig(retransmit_success_rate=1.5)
    with pytest.raises(InputError):
        RecoveryConfig(boundary_budget_ticks=-1)
    with pytest.raises(InputError):
        RetransmitOracle(config()).attempt("cam", 1, 0)
