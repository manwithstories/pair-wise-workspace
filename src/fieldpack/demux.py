"""Demux: record-boundary awareness and unified nanosecond timeline.

Each device keeps its own unwrap state.  Sequence-number resets (storage
card swap) open a new segment; timestamp wraparound is unwrapped onto the
integer-nanosecond base; duplicate sequence numbers and non-monotonic
timestamps are quarantined (isolated) rather than silently reordered.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .model import (
    TIMESTAMP_WRAP_NS,
    DemuxEvent,
    FrameReady,
    FrameRecord,
    Quarantined,
    Segment,
    SegmentStart,
)


@dataclass
class _DeviceState:
    segment_index: int = -1
    last_seq: int | None = None
    wrap_count: int = 0
    last_pts_ns: int | None = None
    segment: Segment | None = None


class Demuxer:
    """Stateful per-device record -> timeline-event converter."""

    def __init__(self) -> None:
        self._devices: dict[int, _DeviceState] = {}

    def handle(self, record: FrameRecord) -> list[DemuxEvent]:
        st = self._devices.setdefault(record.device_id, _DeviceState())
        events: list[DemuxEvent] = []

        # Sequence handling: reset (card swap) starts a new segment; an
        # exact repeat of the previous sequence number is a duplicate.
        if st.last_seq is not None and record.seq <= st.last_seq:
            if record.seq == st.last_seq:
                events.append(Quarantined(record, "duplicate sequence number"))
                return events
            st.segment_index += 1
            st.segment = None
            # The device clock keeps running across a card swap: unwrap
            # state carries over so the unified timeline stays monotonic.
            # A clock that genuinely restarted becomes unresolvable below
            # and the record is quarantined, never silently reordered.
        st.last_seq = record.seq

        if st.segment is None:
            st.segment = Segment(
                device_id=record.device_id,
                index=st.segment_index if st.segment_index >= 0 else 0,
                start_offset=record.offset,
            )
            if st.segment_index < 0:
                st.segment_index = 0
            events.append(SegmentStart(st.segment))

        # Unwrap the device clock onto the unified nanosecond base.
        pts = record.raw_ts_ns + st.wrap_count * TIMESTAMP_WRAP_NS
        if st.last_pts_ns is not None:
            if pts + TIMESTAMP_WRAP_NS // 2 < st.last_pts_ns:
                st.wrap_count += 1
                pts += TIMESTAMP_WRAP_NS
            if pts < st.last_pts_ns:
                events.append(Quarantined(record, "non-monotonic timestamp"))
                return events
        st.last_pts_ns = pts

        events.append(FrameReady(record=record, pts_ns=pts, segment=st.segment))
        return events
