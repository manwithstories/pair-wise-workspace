"""Map stream anomalies to reviewable source byte offsets and time ranges.

Every diagnosis the pipeline raises is normalised into a :class:`DamageRecord`
carrying four things an operator can audit: the track, a source byte interval
inside ``stream.rlyd``, a rational time interval on the track time base, and a
stable category.  Cross-track continuity at edit points and transition
boundaries is checked here as well.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .container import ParsedPacket, iter_packets
from .timeline import TimeBase, TimeRange

#: Stable category vocabulary (also emitted in the JSON manifest).
CAT_TRUNCATED = "truncated_packet"
CAT_DECODE = "decode_failure"
CAT_DUPLICATE = "duplicate_timestamp"
CAT_REVERSED = "reversed_timestamp"
CAT_WRAP = "wraparound"
CAT_CONTINUITY = "cross_track_gap"


@dataclass(slots=True)
class DamageRecord:
    category: str
    track: str
    byte_range: tuple[int, int]
    time_range: TimeRange
    detail: str
    expected: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        return {
            "category": self.category,
            "track": self.track,
            "byte_offset_start": self.byte_range[0],
            "byte_offset_end": self.byte_range[1],
            "time_range": self.time_range.to_dict(),
            "detail": self.detail,
            "expected": self.expected,
        }


class DamageLocator:
    """Consume parsed packets and collect fully-qualified damage records."""

    def __init__(self, header: dict, modulus: int | None = None) -> None:
        self.header = header
        self.modulus = modulus or header.get("wrap_modulus", 1 << 32)
        self._bases = {
            t["id"]: TimeBase(*t["time_base"]) for t in header.get("tracks", [])
        }
        self._last: dict[str, ParsedPacket] = {}
        self._seen_pts: dict[str, set[int]] = {}
        self._packet_by_pts: dict[str, dict[int, ParsedPacket]] = {}
        self.records: list[DamageRecord] = []

    def base_for(self, track: str) -> TimeBase:
        return self._bases.get(track, TimeBase(1, 90000))

    def observe(self, parsed: ParsedPacket) -> list[DamageRecord]:
        rec = parsed.record
        emitted: list[DamageRecord] = []
        if rec.pts_wrapped:
            emitted.append(self._record(
                CAT_WRAP, parsed, "32-bit PTS wraparound observed"))
        if rec.kind == "corrupt":
            emitted.append(self._record(
                CAT_DECODE, parsed, "payload fails decode checksum expectation"))
        if rec.kind == "duplicate" or rec.pts in self._seen_pts.get(rec.track, set()):
            # Attribute the duplicate to the original packet sharing the PTS,
            # so its byte window matches the generator's ground-truth frame.
            original = self._packet_by_pts.get(rec.track, {}).get(rec.pts)
            target = original or parsed
            emitted.append(self._record(
                CAT_DUPLICATE, target, "repeated PTS on the same track"))
        previous = self._last.get(rec.track)
        if previous is not None and rec.kind == "media" \
                and rec.pts < previous.record.pts \
                and not rec.pts_wrapped:
            emitted.append(self._record(
                CAT_REVERSED, parsed,
                f"PTS {rec.pts} precedes prior {previous.record.pts}"))
        self._seen_pts.setdefault(rec.track, set()).add(rec.pts)
        self._packet_by_pts.setdefault(rec.track, {})[rec.pts] = parsed
        self._last[rec.track] = parsed
        self.records.extend(emitted)
        return emitted

    def note_truncation(self, offset: int, track: str,
                        time_range: TimeRange, detail: str) -> DamageRecord:
        record = DamageRecord(
            CAT_TRUNCATED, track, (offset, offset), time_range, detail)
        self.records.append(record)
        return record

    def _record(self, category: str, parsed: ParsedPacket, detail: str) -> DamageRecord:
        rec = parsed.record
        base = self.base_for(rec.track)
        start = base.ticks_to_seconds(rec.pts)
        end = start + base.ticks_to_seconds(max(1, _nominal_tick(base)))
        # Body start skips the 4-byte length prefix, matching the generator's
        # ground-truth windows and pointing at the first reviewable content byte.
        return DamageRecord(
            category, rec.track,
            (parsed.packet_range[0] + 4, parsed.packet_range[1]),
            TimeRange(start, end, rec.track), detail)

    def check_continuity(self, transitions: list[dict],
                         track_spans: dict[str, TimeRange]) -> list[DamageRecord]:
        """Verify both sides of a transition cover the boundary timestamp."""
        out: list[DamageRecord] = []
        for tr in transitions:
            at = Fraction(str(tr["at_s"]))
            for side_key in ("in_track", "out_track"):
                track = tr[side_key]
                span = track_spans.get(track)
                if span is None or not (span.start <= at <= span.end):
                    rng = TimeRange(at, at + Fraction(1, 1000), track)
                    rec = DamageRecord(
                        CAT_CONTINUITY, track, (0, 0), rng,
                        f"{tr['id']}: {track} missing at boundary {float(at):.3f}s",
                        {"transition": tr["id"], "side": side_key})
                    out.append(rec)
        self.records.extend(out)
        return out


def _nominal_tick(base: TimeBase) -> int:
    """Representative one-frame tick used only to size a time interval."""
    if base.den >= 90000:
        return base.den // 30
    if base.den >= 48000:
        return base.den // 10
    return base.den


def locate_truncated_stream(path: str, locator: DamageLocator) -> int:
    """Open a possibly truncated container; return count of truncation errors."""
    count = 0
    try:
        for _header, parsed in iter_packets(path):
            locator.observe(parsed)
    except EOFError as exc:
        offset = _stream_size(path)
        locator.note_truncation(
            offset, "video", TimeRange(Fraction(0), Fraction(1, 10)), str(exc))
        count += 1
    return count


def _stream_size(path: str) -> int:
    import os
    return os.path.getsize(path)
