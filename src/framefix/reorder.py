"""Bounded DTS-to-display-order reordering for B-frame streams."""

from __future__ import annotations

import heapq
from dataclasses import dataclass
from typing import Iterable, Optional

from .models import PacketMeta, ticks_to_ms


@dataclass(slots=True)
class ReorderSummary:
    packets: int = 0
    duplicate_dts: int = 0
    regressed_dts: int = 0
    missing_dts: int = 0
    max_buffer_depth: int = 0
    late_pts: int = 0
    max_dts_jump_ms: float = 0.0


class ReorderBuffer:
    """Reorder a bounded number of packets from demux order to PTS order.

    Packets enter in DTS/decode order.  A min-heap keyed by PTS restores display
    order, while the configurable window caps memory.  A window smaller than the
    actual B-frame depth is detectable through ``late_pts``.
    """

    def __init__(self, window: int = 64):
        if window < 1:
            raise ValueError("reorder window must be at least 1")
        self.window = window
        self.summary = ReorderSummary()
        self._heap: list[tuple[int, int, PacketMeta]] = []
        self._last_input_dts: Optional[int] = None
        self._last_output_pts: Optional[int] = None

    def push(self, packet: PacketMeta) -> list[PacketMeta]:
        summary = self.summary
        summary.packets += 1
        if packet.dts is None:
            summary.missing_dts += 1
        else:
            if self._last_input_dts is not None:
                delta = packet.dts - self._last_input_dts
                if delta == 0:
                    summary.duplicate_dts += 1
                elif delta < 0:
                    summary.regressed_dts += 1
                jump_ms = ticks_to_ms(abs(delta), packet.time_base) or 0.0
                summary.max_dts_jump_ms = max(summary.max_dts_jump_ms, jump_ms)
            self._last_input_dts = packet.dts

        pts = packet.pts if packet.pts is not None else packet.seq
        if self._last_output_pts is not None and pts < self._last_output_pts:
            summary.late_pts += 1
        heapq.heappush(self._heap, (pts, packet.seq, packet))
        summary.max_buffer_depth = max(summary.max_buffer_depth, len(self._heap))

        emitted: list[PacketMeta] = []
        if len(self._heap) >= self.window:
            emitted.append(self._pop())
        return emitted

    def _pop(self) -> PacketMeta:
        _pts, _seq, packet = heapq.heappop(self._heap)
        if packet.pts is not None:
            self._last_output_pts = packet.pts
        return packet

    def flush(self) -> Iterable[PacketMeta]:
        while self._heap:
            yield self._pop()


def audit_packets(packets: Iterable[PacketMeta], window: int = 64) -> ReorderSummary:
    """Consume packet metadata through a bounded buffer and return counts."""
    buffer = ReorderBuffer(window=window)
    for packet in packets:
        list(buffer.push(packet))
    for _packet in buffer.flush():
        pass
    return buffer.summary

