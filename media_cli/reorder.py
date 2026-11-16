"""Bounded GOP display-order reconstruction from decode-order packets."""

from __future__ import annotations

from statistics import median

from .packets import PacketRecord


class ReorderError(RuntimeError):
    """The reorder window is invalid or exhausted."""


def _decode_order(packets: list[PacketRecord]) -> list[PacketRecord]:
    if all(packet.dts is not None for packet in packets):
        return sorted(packets, key=lambda packet: (packet.dts, packet.index))
    return list(packets)


def _sortable_pts(records: list[PacketRecord]) -> dict[int, int]:
    """Expand PTS locally so a 32-bit wrap does not reverse GOP sorting."""

    modulus = 1 << 32
    threshold = modulus // 2
    result: dict[int, int] = {}
    raw_values = [record.pts for record in records if record.pts is not None]
    if raw_values and max(raw_values) - min(raw_values) > threshold:
        pivot = max(raw_values) - threshold
        raw_values = [value + modulus if value < pivot else value for value in raw_values]
        wrapped = iter(raw_values)
    else:
        wrapped = None
    adjustment = 0
    previous: int | None = None
    for record in records:
        if record.pts is None:
            continue
        value = next(wrapped) if wrapped is not None else record.pts + adjustment
        while previous is not None and value < previous - threshold:
            adjustment += modulus
            value += modulus
        result[record.index] = value
        previous = value
    offsets = [
        value - record.dts
        for record in records
        if record.pts is not None and record.dts is not None
        for value in (result[record.index],)
    ]
    dts_offset = int(median(offsets)) if offsets else 0
    for record in records:
        if record.pts is None and record.dts is not None:
            result[record.index] = record.dts + dts_offset
    return result


def _gop_windows(records: list[PacketRecord], window_size: int | None) -> list[list[PacketRecord]]:
    windows: list[list[PacketRecord]] = []
    current: list[PacketRecord] = []
    for record in records:
        if record.keyframe and current:
            windows.append(current)
            current = []
        current.append(record)
        if window_size is not None and len(current) > window_size:
            raise ReorderError(
                f"GOP window exceeded configured limit {window_size}; refusing unbounded buffering"
            )
    if current:
        windows.append(current)
    return windows


def reorder_packets(
    packets: list[PacketRecord],
    *,
    window_size: int | None = None,
) -> list[PacketRecord]:
    """Return display order, using DTS as decode order and PTS inside each GOP."""

    if not packets:
        return []
    half = max(1, len(packets) // 2)
    if window_size is not None and (window_size <= 0 or window_size > half):
        raise ReorderError(f"window_size must be between 1 and half the packet count ({half})")

    decode_order = _decode_order(packets)
    sort_keys = _sortable_pts(decode_order)
    output: list[PacketRecord] = []
    for window in _gop_windows(decode_order, window_size):
        ordered = sorted(
            window,
            key=lambda packet: (
                sort_keys.get(packet.index, packet.dts if packet.dts is not None else packet.index),
                packet.index,
            ),
        )
        output.extend(ordered)

    return output
