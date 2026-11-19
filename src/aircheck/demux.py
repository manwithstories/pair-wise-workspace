"""Bounded, cancellable demux stage.

The demuxer walks packets from a container and hands them to a consumer through
a :class:`pipeline.BoundedQueue`.  The queue enforces the memory ceiling: when
the consumer is slow, ``put`` blocks the demux thread instead of buffering, so
resident memory stays proportional to ``queue_size`` rather than to the asset.

Cancellation is checked at every block point, and each packet is annotated with
the segment it belongs to before it is queued.  The last *confirmed* segment
boundary (a segment whose packets have all been accepted downstream) is what
resume uses to avoid emitting duplicates on a restart.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterator

import av

from .probe import ProbeResult
from .timebase import (
    CancelledError,
    CorruptStreamError,
    TimestampTracker,
    to_ticks,
)

#: A packet is "committed" once its segment boundary has been observed by the
#: writer; finish packets (size 0) flush decoder state and carry no boundary.
COMMIT_GRACE_SEGMENTS = 1


@dataclass(frozen=True)
class DemuxedPacket:
    """A single demuxed packet plus the routing metadata the writer needs."""

    stream_index: int
    segment: int
    pts_ticks: int | None
    dts_ticks: int | None
    time_base: tuple[int, int]
    is_keyframe: bool
    size: int
    pos: int | None
    payload: bytes

    def to_dict(self) -> dict[str, object]:
        return {
            "stream_index": self.stream_index,
            "segment": self.segment,
            "pts_ticks": self.pts_ticks,
            "dts_ticks": self.dts_ticks,
            "time_base": list(self.time_base),
            "is_keyframe": self.is_keyframe,
            "size": self.size,
            "pos": self.pos,
        }


@dataclass
class DemuxStats:
    packets: int = 0
    bytes: int = 0
    segments_seen: set[int] = field(default_factory=set)
    per_stream: dict[int, int] = field(default_factory=dict)
    last_timestamp_ticks: int | None = None
    skipped_before: int = 0

    def to_dict(self) -> dict[str, object]:
        return {
            "packets": self.packets,
            "bytes": self.bytes,
            "segments_seen": len(self.segments_seen),
            "per_stream": {str(k): v for k, v in sorted(self.per_stream.items())},
            "last_timestamp_ticks": self.last_timestamp_ticks,
            "skipped_before": self.skipped_before,
        }


@dataclass
class Demuxer:
    """Iterate packets for ``probe``, segment-tagged and cancellable.

    ``segment_ticks`` is the canonical nanosecond length of one timeline
    segment.  A packet's segment is ``pts_ticks // segment_ticks``; packets with
    no pts fall back to dts, and packets with neither inherit the previous
    packet's segment (finish packets behave this way).
    """

    probe: ProbeResult
    segment_ticks: int
    queue_size: int = 4
    resume_from_segment: int = 0
    validate_timestamps: bool = True

    stats: DemuxStats = field(default_factory=DemuxStats)

    def _segment_for(self, pts_ticks: int | None, dts_ticks: int | None,
                     fallback: int) -> int:
        ticks = pts_ticks if pts_ticks is not None else dts_ticks
        if ticks is None or ticks < 0:
            return fallback
        return ticks // self.segment_ticks

    def iter_packets(self, cancel: object | None = None) -> Iterator[DemuxedPacket]:
        """Yield packets in mux order, honouring ``resume_from_segment``.

        ``cancel`` may be a :class:`threading.Event`; it is polled between
        packets so a blocked consumer does not delay cancellation.
        """
        if self.segment_ticks <= 0:
            raise ValueError("segment_ticks must be positive")
        try:
            container = av.open(str(self.probe.path))
        except av.error.FFmpegError as exc:
            raise CorruptStreamError(
                "input could not be reopened for demuxing",
                path=str(self.probe.path),
                error=str(exc),
            ) from exc

        trackers = {
            stream.index: TimestampTracker(stream.index, stream.time_base)
            for stream in self.probe.streams
            if stream.is_media
        }
        resume_ticks = self.resume_from_segment * self.segment_ticks
        last_segment = self.resume_from_segment
        try:
            for packet in container.demux():
                if _is_cancelled(cancel):
                    raise CancelledError(
                        "demux cancelled",
                        path=str(self.probe.path),
                        last_confirmed_segment=last_segment - COMMIT_GRACE_SEGMENTS,
                    )
                if packet.size == 0:
                    # Decoder flush packet: no payload, no new timeline info.
                    continue
                stream_info = next(
                    (s for s in self.probe.streams if s.index == packet.stream.index),
                    None,
                )
                if stream_info is None:
                    continue
                time_base = stream_info.time_base
                pts_ticks = to_ticks(packet.pts, time_base)
                dts_ticks = to_ticks(packet.dts, time_base)
                segment = self._segment_for(pts_ticks, dts_ticks, last_segment)

                if self.validate_timestamps:
                    tracker = trackers.get(packet.stream.index)
                    if tracker is not None:
                        tracker.observe(packet.pts, packet.dts)

                if packet.pts is not None and resume_ticks > 0:
                    # Drop packets strictly before the resume boundary; a
                    # packet exactly on the boundary belongs to the resumed
                    # segment and is emitted.
                    packet_ticks = to_ticks(packet.pts, time_base)
                    if packet_ticks is not None and packet_ticks < resume_ticks:
                        self.stats.skipped_before += 1
                        continue

                self.stats.packets += 1
                self.stats.bytes += packet.size
                self.stats.per_stream[packet.stream.index] = (
                    self.stats.per_stream.get(packet.stream.index, 0) + 1
                )
                self.stats.segments_seen.add(segment)
                if pts_ticks is not None and pts_ticks >= 0:
                    if (self.stats.last_timestamp_ticks is None
                            or pts_ticks > self.stats.last_timestamp_ticks):
                        self.stats.last_timestamp_ticks = pts_ticks
                last_segment = max(last_segment, segment)
                yield DemuxedPacket(
                    stream_index=packet.stream.index,
                    segment=segment,
                    pts_ticks=pts_ticks,
                    dts_ticks=dts_ticks,
                    time_base=(time_base.numerator, time_base.denominator),
                    is_keyframe=bool(packet.is_keyframe),
                    size=packet.size,
                    pos=packet.pos,
                    payload=bytes(packet),
                )
        except av.error.FFmpegError as exc:
            raise CorruptStreamError(
                "demux failed mid-stream",
                path=str(self.probe.path),
                error=str(exc),
                packets_emitted=self.stats.packets,
                last_timestamp_ticks=self.stats.last_timestamp_ticks,
            ) from exc
        finally:
            container.close()

    def segment_boundaries(self) -> dict[int, int]:
        """Return ``{segment: packet_count}`` for this asset's packets."""
        counts: dict[int, int] = {}
        for packet in self.iter_packets():
            counts[packet.segment] = counts.get(packet.segment, 0) + 1
        return counts


def _is_cancelled(cancel: object | None) -> bool:
    if cancel is None:
        return False
    is_set = getattr(cancel, "is_set", None)
    if callable(is_set):
        return bool(is_set())
    return False


__all__ = [
    "COMMIT_GRACE_SEGMENTS",
    "DemuxStats",
    "DemuxedPacket",
    "Demuxer",
]
