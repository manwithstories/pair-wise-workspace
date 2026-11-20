"""De-jitter tests: duplication, reorder, wrap, conflict, late and loss budget."""

from __future__ import annotations

import pytest

from mediasync.errors import InputError
from mediasync.jitter import (
    JitterConfig,
    TransportPacket,
    dejitter,
    expand_sequences,
)

CONFIG = JitterConfig(wait_budget_ms=45, reorder_window=64)


def packet(seq: int, arrival: int, payload: bytes | None = None) -> TransportPacket:
    return TransportPacket(seq, arrival, payload if payload is not None else bytes([seq % 256]) * 4)


def plan(entries):
    return [packet(seq, arrival) for seq, arrival in entries]


def test_clean_stream_passes_through() -> None:
    result = dejitter(plan([(index, index * 10) for index in range(20)]), CONFIG)
    assert [item.seq for item in result.ordered] == list(range(20))
    assert not result.missing
    assert result.duplicates == 0
    assert result.reordered == 0


def test_out_of_order_arrivals_are_sorted() -> None:
    entries = [(0, 0), (1, 10), (2, 20), (4, 30), (3, 40), (5, 50)]
    result = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in result.ordered] == [0, 1, 2, 3, 4, 5]
    assert result.reordered == 1
    assert result.max_reorder_distance == 1


def test_sequence_wrap_is_expanded() -> None:
    entries = [((65534 + offset) % 65536, offset * 10) for offset in range(6)]
    result = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in result.ordered] == [65534, 65535, 0, 1, 2, 3]
    assert not result.missing


def test_wrap_with_reordering() -> None:
    entries = [(65534, 0), (65535, 10), (1, 20), (0, 30), (2, 40)]
    result = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in result.ordered] == [65534, 65535, 0, 1, 2]
    assert result.reordered == 1


def test_duplicate_identical_payload_is_dropped() -> None:
    entries = [(0, 0), (1, 10), (2, 20), (2, 31), (3, 40)]
    result = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in result.ordered] == [0, 1, 2, 3]
    assert result.duplicates == 1
    assert not result.conflicts


def test_duplicate_conflicting_payload_is_reported() -> None:
    entries = [
        packet(0, 0),
        packet(1, 10),
        packet(2, 20),
        packet(2, 31, payload=b"corrupt"),
        packet(3, 40),
    ]
    result = dejitter(entries, CONFIG)
    assert result.duplicates == 1
    assert len(result.conflicts) == 1
    conflict = result.conflicts[0]
    assert conflict.seq == 2
    assert conflict.first_size != conflict.second_size


def test_lost_packet_is_declared_after_wait_budget() -> None:
    entries = [(0, 0), (1, 10), (2, 20), (4, 40), (5, 50), (6, 60), (7, 70), (8, 80)]
    result = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in result.ordered] == [0, 1, 2, 4, 5, 6, 7, 8]
    assert [item.seq for item in result.missing] == [3]
    assert result.missing[0].deadline_ms >= 40 + CONFIG.wait_budget_ms - 1


def test_late_packet_is_rejected_once_hole_is_closed() -> None:
    entries = [(0, 0), (1, 10), (3, 70), (2, 200)]
    result = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in result.ordered] == [0, 1, 3]
    assert [item.seq for item in result.late] == [2]
    assert [item.seq for item in result.missing] == [2]
    assert result.late[0].lateness_ms > 0


def test_reorder_window_forces_early_loss_declaration() -> None:
    entries = [(0, 0)] + [(index, index * 5) for index in range(100, 110)] + [(1, 60)]
    entries.sort(key=lambda item: item[1])
    result = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in result.ordered][:2] == [0, 1]
    assert len(result.missing) >= 60


def test_zero_budget_closes_gaps_immediately() -> None:
    config = JitterConfig(wait_budget_ms=0, reorder_window=8)
    result = dejitter(plan([(0, 0), (2, 5), (3, 6)]), config)
    assert [item.seq for item in result.ordered] == [0, 2, 3]
    assert [item.seq for item in result.missing] == [1]


def test_expand_sequences_rejects_out_of_range() -> None:
    with pytest.raises(InputError):
        expand_sequences([65536])
    with pytest.raises(InputError):
        expand_sequences([-1])


def test_packet_rejects_negative_fields() -> None:
    with pytest.raises(InputError):
        TransportPacket(-1, 0, b"x")
    with pytest.raises(InputError):
        TransportPacket(0, -1, b"x")
    with pytest.raises(InputError):
        TransportPacket(0, 0, b"x", duration_ticks=-5)


def test_config_rejects_bad_budgets() -> None:
    with pytest.raises(InputError):
        JitterConfig(wait_budget_ms=-1)
    with pytest.raises(InputError):
        JitterConfig(reorder_window=-1)
    with pytest.raises(InputError):
        JitterConfig(seq_wrap=1)


def test_packet_limit_is_enforced() -> None:
    config = JitterConfig(max_packets=2)
    with pytest.raises(InputError):
        dejitter(plan([(0, 0), (1, 1), (2, 2)]), config)


def test_result_is_deterministic() -> None:
    entries = [(0, 0), (2, 20), (1, 30), (4, 40), (3, 50), (6, 200)]
    first = dejitter(plan(entries), CONFIG)
    second = dejitter(plan(entries), CONFIG)
    assert [item.seq for item in first.ordered] == [item.seq for item in second.ordered]
    assert [item.seq for item in first.missing] == [item.seq for item in second.missing]
