"""Per-segment packet hashing.

The integrity claim this tool makes is "the compressed payload survived the
remux byte for byte".  It is verified by hashing every packet's payload in
segment order on the way in, and again on the way out, then comparing.

Hashing is *per stream within a segment*.  Packet interleaving is a container
scheduling decision - MP4 and Matroska legitimately order packets differently
inside the same segment - so comparing the global interleaved byte sequence
would flag a faithful copy as damaged.  Order *within* a stream, however, is
semantic (decode order) and must be preserved exactly.

Each :class:`SegmentHash` therefore holds one digest per stream:

* ``payload``  - SHA-256 over that stream's packet payloads in demux order;
* ``identity`` - SHA-256 over ``(stream_index, keyframe, size, payload)`` per
  packet, which also detects a lost keyframe flag or a size/payload mismatch.

Timestamps are deliberately excluded.  Containers renormalise encoder pre-roll
and choose their own time bases, so raw timestamps are expected to differ after
a cross-container copy; timeline drift is reported separately by
:func:`compare_timeline` with an explicit tolerance rather than being folded
into the byte-integrity verdict.

Segments are derived from the input's packet counts per stream.  A timestamp
boundary can move by a frame when a muxer renormalises pre-roll, so the copy is
regrouped by packet ordinal (see :class:`SegmentPlan`) to keep the comparison
stable while still bounding memory to O(segments x streams).
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from typing import Iterable

from .demux import DemuxedPacket

#: Timestamps may shift by up to this much across a container change without
#: being considered a defect: Matroska rounds to milliseconds and MP4/MOV to
#: 1/12288 or 1/90000, and encoder pre-roll is renormalised per container.
TIMELINE_TOLERANCE_TICKS = 25_000_000


@dataclass(frozen=True)
class StreamDigest:
    packets: int
    payload_sha256: str
    identity_sha256: str

    def to_dict(self) -> dict[str, object]:
        return {
            "packets": self.packets,
            "payload_sha256": self.payload_sha256,
            "identity_sha256": self.identity_sha256,
        }


@dataclass(frozen=True)
class SegmentHash:
    segment: int
    streams: dict[int, StreamDigest]

    @property
    def packets(self) -> int:
        return sum(digest.packets for digest in self.streams.values())

    def to_dict(self) -> dict[str, object]:
        return {
            "segment": self.segment,
            "packets": self.packets,
            "streams": {str(k): v.to_dict() for k, v in sorted(self.streams.items())},
        }


@dataclass(frozen=True)
class SegmentPlan:
    """Segment boundaries expressed as per-stream packet counts.

    Produced while hashing the input and replayed while hashing the output.
    A packet is assigned to the earliest segment that still expects packets for
    its stream, which reproduces the input grouping exactly for a faithful copy
    even when the muxer has shifted a packet across a timestamp boundary.
    """

    counts: list[tuple[int, dict[int, int]]]

    def to_dict(self) -> dict[str, object]:
        return {
            "counts": [
                {"segment": segment, "streams": {str(k): v for k, v in sorted(streams.items())}}
                for segment, streams in self.counts
            ],
        }


class _SegmentAccumulator:
    """Per-segment, per-stream running digests."""

    __slots__ = ("payload", "identity", "packets")

    def __init__(self) -> None:
        self.payload: dict[int, "hashlib._Hash"] = {}
        self.identity: dict[int, "hashlib._Hash"] = {}
        self.packets: dict[int, int] = {}

    def update(self, packet: DemuxedPacket) -> None:
        stream = packet.stream_index
        payload = self.payload.get(stream)
        if payload is None:
            payload = hashlib.sha256()
            self.payload[stream] = payload
            self.identity[stream] = hashlib.sha256()
        payload.update(packet.payload)
        identity = hashlib.sha256()
        identity.update(stream.to_bytes(4, "big", signed=True))
        identity.update(b"\x01" if packet.is_keyframe else b"\x00")
        identity.update(packet.size.to_bytes(8, "big"))
        identity.update(hashlib.sha256(packet.payload).digest())
        self.identity[stream].update(identity.digest())
        self.packets[stream] = self.packets.get(stream, 0) + 1

    def freeze(self, segment: int) -> SegmentHash:
        return SegmentHash(
            segment=segment,
            streams={
                stream: StreamDigest(
                    packets=self.packets[stream],
                    payload_sha256=self.payload[stream].hexdigest(),
                    identity_sha256=self.identity[stream].hexdigest(),
                )
                for stream in sorted(self.packets)
            },
        )


class SegmentHasher:
    """Accumulate packet hashes one packet at a time, segment by segment.

    Without a plan, ``packet.segment`` decides the grouping (used on the input
    side).  With a plan, packets are placed by per-stream ordinal, which makes
    the grouping copy-stable.
    """

    def __init__(self, plan: SegmentPlan | None = None) -> None:
        self._plan_order = [segment for segment, _ in plan.counts] if plan else []
        self._plan_counts = {segment: dict(streams) for segment, streams in plan.counts} if plan else {}
        self._plan_used: dict[int, dict[int, int]] = {}
        self._plan_position = 0
        self._current: int | None = None
        self._accumulator = _SegmentAccumulator()
        self._hashes: list[SegmentHash] = []

    def assign_segment(self, packet: DemuxedPacket) -> int:
        if not self._plan_order:
            return packet.segment
        while self._plan_position < len(self._plan_order):
            segment = self._plan_order[self._plan_position]
            expected = self._plan_counts[segment].get(packet.stream_index, 0)
            used = self._plan_used.get(segment, {}).get(packet.stream_index, 0)
            if used < expected:
                return segment
            self._plan_position += 1
        # Overflow beyond the plan lands in the final segment so that the
        # mismatch is reported rather than silently dropped.
        return self._plan_order[-1]

    def update(self, packet: DemuxedPacket) -> None:
        segment = self.assign_segment(packet)
        if self._current is None:
            self._current = segment
        elif segment != self._current:
            if segment < self._current:
                raise ValueError(
                    f"segments must arrive in ascending order "
                    f"(got {segment} after {self._current})"
                )
            self._close_current()
            self._current = segment
        if self._plan_order:
            used = self._plan_used.setdefault(segment, {})
            used[packet.stream_index] = used.get(packet.stream_index, 0) + 1
        self._accumulator.update(packet)

    def _close_current(self) -> None:
        assert self._current is not None
        self._hashes.append(self._accumulator.freeze(self._current))
        self._accumulator = _SegmentAccumulator()

    def finish(self) -> list[SegmentHash]:
        if self._current is not None:
            self._close_current()
            self._current = None
        return list(self._hashes)

    def plan(self) -> SegmentPlan:
        counts = [
            (segment_hash.segment, {stream: digest.packets for stream, digest in segment_hash.streams.items()})
            for segment_hash in self._hashes
        ]
        return SegmentPlan(counts=counts)


def hash_packets(
    packets: Iterable[DemuxedPacket], *, plan: SegmentPlan | None = None
) -> list[SegmentHash]:
    hasher = SegmentHasher(plan=plan)
    for packet in packets:
        hasher.update(packet)
    return hasher.finish()


def hash_packets_with_plan(
    packets: Iterable[DemuxedPacket],
) -> tuple[list[SegmentHash], SegmentPlan]:
    """Hash an input asset and return the grouping plan for its copy."""
    hasher = SegmentHasher()
    for packet in packets:
        hasher.update(packet)
    hashes = hasher.finish()
    return hashes, hasher.plan()


@dataclass
class HashComparison:
    """Result of comparing input and output segment hashes."""

    matched: list[int] = field(default_factory=list)
    mismatched: list[dict[str, object]] = field(default_factory=list)
    missing_in_output: list[int] = field(default_factory=list)
    extra_in_output: list[int] = field(default_factory=list)

    @property
    def total(self) -> int:
        return len(self.matched) + len(self.mismatched)

    @property
    def match_ratio(self) -> float:
        if self.total == 0:
            return 1.0 if not self.missing_in_output and not self.extra_in_output else 0.0
        return len(self.matched) / self.total

    @property
    def all_match(self) -> bool:
        return not self.mismatched and not self.missing_in_output and not self.extra_in_output

    def to_dict(self) -> dict[str, object]:
        return {
            "matched": list(self.matched),
            "mismatched": list(self.mismatched),
            "missing_in_output": list(self.missing_in_output),
            "extra_in_output": list(self.extra_in_output),
            "total": self.total,
            "match_ratio": self.match_ratio,
            "all_match": self.all_match,
        }


def compare_hashes(
    expected: list[SegmentHash], actual: list[SegmentHash]
) -> HashComparison:
    result = HashComparison()
    expected_by_segment = {item.segment: item for item in expected}
    actual_by_segment = {item.segment: item for item in actual}
    for segment in sorted(expected_by_segment):
        want = expected_by_segment[segment]
        got = actual_by_segment.get(segment)
        if got is None:
            result.missing_in_output.append(segment)
            continue
        stream_report = _compare_streams(want.streams, got.streams)
        if stream_report is None:
            result.matched.append(segment)
        else:
            result.mismatched.append(
                {
                    "segment": segment,
                    "expected_packets": want.packets,
                    "actual_packets": got.packets,
                    "streams": stream_report,
                }
            )
    for segment in sorted(set(actual_by_segment) - set(expected_by_segment)):
        result.extra_in_output.append(segment)
    return result


def _compare_streams(
    want: dict[int, StreamDigest], got: dict[int, StreamDigest]
) -> list[dict[str, object]] | None:
    """Return per-stream differences, or ``None`` when all streams match."""
    differences: list[dict[str, object]] = []
    for stream in sorted(set(want) | set(got)):
        expected = want.get(stream)
        actual = got.get(stream)
        if expected is None or actual is None:
            differences.append(
                {
                    "stream_index": stream,
                    "status": "missing" if expected is None else "unexpected",
                    "expected_packets": expected.packets if expected else 0,
                    "actual_packets": actual.packets if actual else 0,
                }
            )
            continue
        if (
            expected.payload_sha256 != actual.payload_sha256
            or expected.identity_sha256 != actual.identity_sha256
            or expected.packets != actual.packets
        ):
            differences.append(
                {
                    "stream_index": stream,
                    "status": "mismatch",
                    "expected_packets": expected.packets,
                    "actual_packets": actual.packets,
                    "payload_equal": expected.payload_sha256 == actual.payload_sha256,
                    "identity_equal": expected.identity_sha256 == actual.identity_sha256,
                }
            )
    return differences or None


@dataclass
class TimelineDrift:
    """Per-stream comparison of first and last presentation timestamps."""

    stream_index: int
    start_delta_ticks: int
    end_delta_ticks: int
    within_tolerance: bool

    def to_dict(self) -> dict[str, object]:
        return {
            "stream_index": self.stream_index,
            "start_delta_ticks": self.start_delta_ticks,
            "end_delta_ticks": self.end_delta_ticks,
            "within_tolerance": self.within_tolerance,
        }


def compare_timeline(
    expected: Iterable[DemuxedPacket],
    actual: Iterable[DemuxedPacket],
    *,
    tolerance_ticks: int = TIMELINE_TOLERANCE_TICKS,
) -> list[TimelineDrift]:
    """Compare start/end timestamps per stream with an explicit tolerance."""

    def bounds(packets: Iterable[DemuxedPacket]) -> dict[int, tuple[int, int]]:
        limits: dict[int, tuple[int, int]] = {}
        for packet in packets:
            if packet.pts_ticks is None or packet.pts_ticks < 0:
                continue
            low, high = limits.get(packet.stream_index, (packet.pts_ticks, packet.pts_ticks))
            limits[packet.stream_index] = (min(low, packet.pts_ticks), max(high, packet.pts_ticks))
        return limits

    want = bounds(expected)
    got = bounds(actual)
    drifts: list[TimelineDrift] = []
    for stream_index in sorted(want):
        want_start, want_end = want[stream_index]
        got_start, got_end = got.get(stream_index, (want_start, want_end))
        start_delta = got_start - want_start
        end_delta = got_end - want_end
        drifts.append(
            TimelineDrift(
                stream_index=stream_index,
                start_delta_ticks=start_delta,
                end_delta_ticks=end_delta,
                within_tolerance=abs(start_delta) <= tolerance_ticks
                and abs(end_delta) <= tolerance_ticks,
            )
        )
    return drifts


def digest_packet_payloads(packets: Iterable[DemuxedPacket]) -> str:
    """One digest over all payload bytes, used for whole-asset checks."""
    digest = hashlib.sha256()
    for packet in packets:
        digest.update(packet.payload)
    return digest.hexdigest()


__all__ = [
    "HashComparison",
    "SegmentHash",
    "SegmentHasher",
    "SegmentPlan",
    "StreamDigest",
    "TIMELINE_TOLERANCE_TICKS",
    "TimelineDrift",
    "compare_hashes",
    "compare_timeline",
    "digest_packet_payloads",
    "hash_packets",
    "hash_packets_with_plan",
]
