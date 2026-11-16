"""Timestamp repair for wrapped, missing, duplicate and offset PTS values."""

from __future__ import annotations

from dataclasses import dataclass, replace
from fractions import Fraction
from statistics import median
from typing import Iterable

from .packets import PacketRecord


class TimestampRepairError(RuntimeError):
    """No timestamp source is available."""


@dataclass(frozen=True)
class RepairAction:
    frame_index: int
    decode_index: int
    action: str
    reason: str
    original_pts: int | None
    repaired_pts: int | None

    def to_dict(self) -> dict[str, object]:
        return {
            "frame_index": self.frame_index,
            "decode_index": self.decode_index,
            "action": self.action,
            "reason": self.reason,
            "original_pts": self.original_pts,
            "repaired_pts": self.repaired_pts,
        }


@dataclass
class TimestampRepairResult:
    packets: list[PacketRecord]
    actions: list[RepairAction]
    offset: int
    repaired_timestamps: int
    max_jump_ms_before: float
    max_jump_ms_after: float
    has_pts: bool
    has_dts: bool
    empty_stream: bool
    single_frame: bool
    has_b_frames: bool


def median_interval(values: Iterable[int | None]) -> int | None:
    clean = [value for value in values if value is not None]
    deltas = [b - a for a, b in zip(clean, clean[1:]) if b > a]
    return int(median(deltas)) if deltas else None


def _step_for(packets: list[PacketRecord], fallback: int | None) -> int:
    dts_step = median_interval(sorted(packet.dts for packet in packets if packet.dts is not None))
    pts_step = median_interval(packet.pts for packet in packets)
    for value in (dts_step, pts_step, fallback):
        if value is not None and value > 0:
            return value
    return 1


def _max_jump_ms(values: list[int | None], time_base: Fraction) -> float:
    clean = [value for value in values if value is not None]
    deltas = [b - a for a, b in zip(clean, clean[1:])]
    if not deltas:
        return 0.0
    positive = [delta for delta in deltas if delta > 0]
    if not positive:
        return 0.0
    expected = median(positive)
    ticks = max(abs(delta - expected) for delta in deltas)
    return round(float(time_base * ticks * 1000), 3)


def _unwrap(
    packets: list[PacketRecord], modulus: int, threshold: int
) -> tuple[list[int | None], dict[int, int], dict[int, int]]:
    values: list[int | None] = []
    originals: dict[int, int] = {}
    added: dict[int, int] = {}
    adjustment = 0
    previous: int | None = None
    for index, packet in enumerate(packets):
        if packet.pts is None:
            values.append(None)
            continue
        value = packet.pts + adjustment
        while previous is not None and value < previous - threshold:
            adjustment += modulus
            value += modulus
            added[index] = added.get(index, 0) + modulus
        if index in added:
            originals[index] = packet.pts
        values.append(value)
        previous = value
    return values, originals, added


def repair_timestamps(
    packets: list[PacketRecord],
    time_base: Fraction,
    *,
    default_interval: int | None = None,
    wrap_bits: int = 32,
) -> TimestampRepairResult:
    modulus = 1 << wrap_bits
    has_pts = any(packet.pts is not None for packet in packets)
    has_dts = any(packet.dts is not None for packet in packets)
    if packets and not has_pts and not has_dts:
        raise TimestampRepairError("packets contain neither PTS nor DTS")

    raw_values = [packet.pts for packet in packets]
    before_ms = _max_jump_ms(raw_values, time_base)
    expanded, originals, added = _unwrap(packets, modulus, modulus // 2)
    step = _step_for(packets, default_interval)
    actions: list[RepairAction] = []

    if not has_pts and has_dts:
        expanded = [packet.dts for packet in packets]

    for index, packet in enumerate(packets):
        if packet.pts is not None:
            continue
        if expanded[index] is None and packet.dts is not None:
            expanded[index] = packet.dts
        previous = next((expanded[i] for i in range(index - 1, -1, -1) if expanded[i] is not None), None)
        following = next(
            (expanded[i] for i in range(index + 1, len(expanded)) if expanded[i] is not None), None
        )
        if previous is not None:
            candidate = previous + step
        elif following is not None:
            candidate = following - step
        else:
            candidate = index * step
        if following is not None and candidate >= following and following - step > (previous or candidate):
            candidate = following - step
        expanded[index] = candidate
        reason = (
            f"missing PTS inferred from adjacent DTS and median interval {step} ticks"
            if packet.dts is not None
            else f"missing PTS inferred from median interval {step} ticks"
        )
        actions.append(
            RepairAction(
                frame_index=index,
                decode_index=packet.index,
                action="infer_missing_pts",
                reason=reason,
                original_pts=None,
                repaired_pts=candidate,
            )
        )

    previous: int | None = None
    for index, value in enumerate(expanded):
        if value is None:
            continue
        if previous is not None and value <= previous:
            repaired = previous + step
            actions.append(
                RepairAction(
                    frame_index=index,
                    decode_index=packets[index].index,
                    action="deduplicate_pts",
                    reason=f"non-increasing PTS advanced by median interval {step} ticks",
                    original_pts=value,
                    repaired_pts=repaired,
                )
            )
            expanded[index] = repaired
            value = repaired
        previous = value

    for index in added:
        if index >= len(packets) or expanded[index] is None:
            continue
        actions.insert(
            sum(1 for action in actions if action.frame_index <= index),
            RepairAction(
                frame_index=index,
                decode_index=packets[index].index,
                action="wrap_expand",
                reason=f"negative jump exceeded threshold; added 2^{wrap_bits}",
                original_pts=originals[index],
                repaired_pts=expanded[index],
            ),
        )

    offset = next((value for value in expanded if value is not None), 0)
    if offset:
        actions.append(
            RepairAction(
                frame_index=0,
                decode_index=packets[0].index if packets else 0,
                action="zero_origin",
                reason=f"first timestamp shifted to zero from {offset}",
                original_pts=offset,
                repaired_pts=0,
            )
        )

    repaired_packets = [
        replace(packet, pts=None if value is None else value - offset)
        for packet, value in zip(packets, expanded)
    ]
    local_repairs = sum(1 for action in actions if action.action != "zero_origin")
    repaired_timestamps = local_repairs + (1 if offset else 0)
    return TimestampRepairResult(
        packets=repaired_packets,
        actions=actions,
        offset=offset,
        repaired_timestamps=repaired_timestamps,
        max_jump_ms_before=before_ms,
        max_jump_ms_after=_max_jump_ms([packet.pts for packet in repaired_packets], time_base),
        has_pts=has_pts,
        has_dts=has_dts,
        empty_stream=not packets,
        single_frame=len(packets) == 1,
        has_b_frames=any(packet.pict_type == "B" for packet in packets),
    )
