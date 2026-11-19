"""Unified integer-microsecond timeline and timestamp conflict detection.

Manifest segments carry nominal start times expressed in each representation's
own timescale.  This module maps them onto one integer-microsecond axis and
rejects ambiguous input explicitly:

* timestamp rollback  – a segment starting before its predecessor
* equal-time duplicate – two segments claiming the same start instant
* rounding conflict   – an overlap smaller than one microsecond of tolerance
* wraparound          – a restart at zero explained by counter wraparound

Nothing is silently overwritten: every collision becomes a structured error.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .errors import ErrorCollector, ErrorStage, skippable
from .manifest import Representation, SegmentRef

# Segments longer than this multiple of the representation target duration
# are flagged as overlong (still kept; the alignment stage reports the drift).
OVERLONG_FACTOR = 3
ROUNDING_TOLERANCE_US = 1


@dataclass
class TimelineSegment:
    index: int
    path: object
    start_us: int
    end_us: int
    duration_us: int
    byte_offset: int | None = None
    byte_length: int | None = None
    valid: bool = True
    note: str = ""
    position: int = 0
    disorder: bool = False

    @property
    def key(self) -> str:
        return f"{self.index}:{self.path}"


@dataclass
class Gap:
    start_us: int
    end_us: int
    reason: str

    @property
    def duration_us(self) -> int:
        return self.end_us - self.start_us


@dataclass
class RepresentationTimeline:
    rep_id: str
    kind: str
    codec: str
    bandwidth: int
    segments: list[TimelineSegment] = field(default_factory=list)
    gaps: list[Gap] = field(default_factory=list)
    errors: ErrorCollector = field(default_factory=ErrorCollector)

    @property
    def duration_us(self) -> int:
        return self.segments[-1].end_us if self.segments else 0


@dataclass
class TimelineResult:
    timelines: dict[str, RepresentationTimeline]
    errors: ErrorCollector

    def has_fatal(self) -> bool:
        return self.errors.has_fatal() or any(t.errors.has_fatal() for t in self.timelines.values())


def _build_one(rep: Representation, errors: ErrorCollector) -> RepresentationTimeline:
    asset = rep.rep_id
    tl = RepresentationTimeline(rep.rep_id, rep.kind, rep.codec, rep.bandwidth)
    high_water = 0
    wrap_offset = 0
    for position, ref in enumerate(rep.segments):
        raw_start = (ref.start_us or 0) + wrap_offset
        duration = max(ref.duration_us or 0, 0)
        note = ""
        # Wraparound is structural and takes precedence: shift the restarted
        # origin before any duplicate/rollback comparison runs.
        if raw_start < high_water and position in rep.wrap_points:
            wrap_offset += high_water
            start = raw_start + high_water
            note = "timestamp wraparound compensated"
            tl.errors.add(skippable(
                ErrorStage.TIMELINE, asset,
                f"timestamp wraparound at segment {ref.index}, offset +{high_water}us",
                segment_index=ref.index, path=str(ref.path),
                detail={"wrap_offset_us": high_water}))
        else:
            start = raw_start
        duplicate = any(s.start_us == start and s.duration_us == duration and s.index != ref.index
                        for s in tl.segments)
        if duration == 0:
            note = "zero-duration segment"
            tl.errors.add(skippable(ErrorStage.TIMELINE, asset, "zero-duration segment",
                                    segment_index=ref.index, path=str(ref.path)))
        if duplicate:
            tl.errors.add(skippable(
                ErrorStage.TIMELINE, asset, f"equal-time duplicate at {start}us",
                segment_index=ref.index, byte_offset=ref.byte_offset, path=str(ref.path)))
            note = "duplicate"
        elif start < high_water:
            conflict = high_water - start
            if conflict <= ROUNDING_TOLERANCE_US:
                tl.errors.add(skippable(
                    ErrorStage.TIMELINE, asset,
                    f"rounding conflict of {conflict}us at segment {ref.index}",
                    segment_index=ref.index, byte_offset=ref.byte_offset, path=str(ref.path),
                    detail={"conflict_us": conflict}))
                start = high_water
            elif getattr(ref, "disorder", False):
                # Out-of-order manifest entry: keep its true start and let the
                # recovery stage re-sequence by time instead of parking it.
                note = "out-of-order"
            else:
                tl.errors.add(skippable(
                    ErrorStage.TIMELINE, asset,
                    f"timestamp rollback by {conflict}us at segment {ref.index}",
                    segment_index=ref.index, byte_offset=ref.byte_offset, path=str(ref.path),
                    detail={"rollback_us": conflict}))
                # Keep the item but park it after the high-water mark so
                # recovery can re-sequence it without overwriting history.
                start = high_water
                note = "rollback parked"
        if rep.target_duration_us and duration > rep.target_duration_us * OVERLONG_FACTOR:
            tl.errors.add(skippable(
                ErrorStage.TIMELINE, asset,
                f"overlong segment: {duration}us > target {rep.target_duration_us}us",
                segment_index=ref.index, path=str(ref.path),
                detail={"duration_us": duration, "target_us": rep.target_duration_us}))
            note = (note + "; overlong").strip("; ")
        seg = TimelineSegment(ref.index, ref.path, start, start + duration, duration,
                              ref.byte_offset, ref.byte_length, valid=duration > 0, note=note,
                              position=getattr(ref, "position", 0),
                              disorder=getattr(ref, "disorder", False))
        tl.segments.append(seg)
        if high_water > 0 and start > high_water and not seg.disorder:
            tl.gaps.append(Gap(high_water, start, "missing segment"))
        high_water = max(high_water, seg.end_us)
    errors.extend(tl.errors.records)
    return tl


def build_timelines(representations: list[Representation],
                    errors: ErrorCollector | None = None) -> TimelineResult:
    """Map every representation onto its own integer-microsecond timeline."""
    errors = errors or ErrorCollector()
    timelines: dict[str, RepresentationTimeline] = {}
    for rep in representations:
        timelines[rep.rep_id] = _build_one(rep, errors)
    return TimelineResult(timelines, errors)


def timeline_span(result: TimelineResult) -> tuple[int, int]:
    """Return the global (min start, max end) across all timelines."""
    starts = [s.start_us for tl in result.timelines.values() for s in tl.segments]
    ends = [s.end_us for tl in result.timelines.values() for s in tl.segments]
    return (min(starts) if starts else 0), (max(ends) if ends else 0)
