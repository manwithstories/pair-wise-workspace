"""Stream mapping resolution and timeline validation.

Two responsibilities live here:

* :func:`resolve_mapping` turns a user map specification (``v:0=a:1`` style or
  ``{"video": [0], "audio": [1]}``) into a validated assignment, reporting the
  offending stream number for every missing, duplicated or wrong-typed entry.
* :func:`validate_timeline` checks the demuxed packet stream for non-monotonic
  timestamps, negative timestamps, cut points that fall outside the container
  and transition overlaps that disagree with the segment grid.

Both return plain data; neither writes anything.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .probe import ProbeResult, StreamInfo
from .timebase import MappingError, as_fraction, format_ticks

#: Tolerance applied when comparing a cut point to a segment boundary.  One
#: frame at 24 fps is ~41.7 ms; we allow half a frame so rounding in container
#: time bases (1/1000 vs 1/12288) does not create false positives.
CUT_TOLERANCE_TICKS = 20_000_000


@dataclass(frozen=True)
class StreamAssignment:
    """One input stream routed to one output slot."""

    input_index: int
    kind: str
    output_index: int

    def to_dict(self) -> dict[str, object]:
        return {
            "input_index": self.input_index,
            "kind": self.kind,
            "output_index": self.output_index,
        }


@dataclass
class Mapping:
    assignments: list[StreamAssignment]
    dropped: list[int] = field(default_factory=list)

    @property
    def by_input(self) -> dict[int, StreamAssignment]:
        return {item.input_index: item for item in self.assignments}

    @property
    def by_output(self) -> dict[int, StreamAssignment]:
        return {item.output_index: item for item in self.assignments}

    def to_dict(self) -> dict[str, object]:
        return {
            "assignments": [item.to_dict() for item in self.assignments],
            "dropped": list(self.dropped),
        }


def _parse_spec(spec: str) -> dict[str, list[int]]:
    """Parse ``"v:0,1=a:2"`` or ``"video=0,1;audio=2"`` forms.

    The left/right of ``=`` are two lists of typed stream numbers; the format
    follows ffmpeg's ``-map`` vocabulary closely enough for operators to reuse
    muscle memory, but always requires explicit kind:number pairs.
    """
    result: dict[str, list[int]] = {"video": [], "audio": []}
    if not spec.strip():
        return result
    # ``=`` separates ordered groups, ``;`` is accepted as an alternative
    # separator for operators used to a different shell vocabulary.
    groups = spec.replace(";", "=").split("=")
    for group in groups:
        tokens = [token.strip() for token in group.split(",")]
        for position, token in enumerate(tokens):
            if not token:
                continue
            if ":" not in token:
                # Bare numbers continue the previous token's kind, so
                # ``v:0,1`` and ``v:0=a:2`` both work.
                previous_kind = None
                for previous in reversed(tokens[:position]):
                    if ":" in previous:
                        previous_kind = previous.partition(":")[0].strip().lower()
                        break
                if previous_kind is None:
                    raise MappingError(
                        "map entries must use kind:number (e.g. v:0 or a:1)",
                        token=token,
                    )
                token = f"{previous_kind}:{token}"
            kind, _, number = token.partition(":")
            kind = kind.strip().lower()
            if kind in ("v", "video"):
                bucket = "video"
            elif kind in ("a", "audio"):
                bucket = "audio"
            else:
                raise MappingError("unknown stream kind in map", token=token)
            try:
                index = int(number)
            except ValueError as exc:
                raise MappingError("stream number must be an integer", token=token) from exc
            result[bucket].append(index)
    return result


def resolve_mapping(
    probe: ProbeResult,
    spec: str | dict[str, list[int]] | None = None,
) -> Mapping:
    """Validate ``spec`` against the probed streams and build an assignment.

    With ``spec=None`` every media stream is copied (identity mapping).  Errors
    carry the offending stream number and kind so an operator can fix the
    command without re-probing the asset.
    """
    if spec is None:
        requested = {
            "video": [s.index for s in probe.video_streams],
            "audio": [s.index for s in probe.audio_streams],
        }
    elif isinstance(spec, str):
        requested = _parse_spec(spec)
    else:
        requested = {
            "video": list(spec.get("video", [])),
            "audio": list(spec.get("audio", [])),
        }

    if not requested["video"] and not requested["audio"]:
        raise MappingError(
            "mapping selects no streams",
            path=str(probe.path),
        )

    by_index: dict[int, StreamInfo] = {s.index: s for s in probe.streams}
    media_indices = {s.index for s in probe.media_streams}
    seen: set[int] = set()
    assignments: list[StreamAssignment] = []
    output_index = 0
    for kind in ("video", "audio"):
        for input_index in requested[kind]:
            if input_index in seen:
                raise MappingError(
                    "stream mapped more than once",
                    stream_index=input_index,
                    kind=kind,
                )
            stream = by_index.get(input_index)
            if stream is None:
                raise MappingError(
                    "mapped stream number does not exist",
                    stream_index=input_index,
                    kind=kind,
                    available_streams=sorted(media_indices),
                )
            if stream.kind != kind:
                raise MappingError(
                    "mapped stream has the wrong type",
                    stream_index=input_index,
                    expected_kind=kind,
                    actual_kind=stream.kind,
                )
            seen.add(input_index)
            assignments.append(
                StreamAssignment(input_index=input_index, kind=kind, output_index=output_index)
            )
            output_index += 1

    dropped = sorted(media_indices - seen)
    return Mapping(assignments=assignments, dropped=dropped)


# --------------------------------------------------------------------------- #
# Timeline validation
# --------------------------------------------------------------------------- #

@dataclass(frozen=True)
class TimelinePacket:
    """The subset of a demuxed packet the validator needs."""

    stream_index: int
    pts_ticks: int | None
    dts_ticks: int | None
    is_keyframe: bool
    segment: int


@dataclass
class TimelineIssue:
    kind: str
    stream_index: int | None
    detail: str
    tick: int | None = None

    def to_dict(self) -> dict[str, object]:
        return {
            "kind": self.kind,
            "stream_index": self.stream_index,
            "detail": self.detail,
            "tick": self.tick,
        }


@dataclass
class TimelineReport:
    segment_ticks: int
    duration_ticks: int
    cut_points: list[int] = field(default_factory=list)
    segments: list[int] = field(default_factory=list)
    keyframes: list[int] = field(default_factory=list)
    issues: list[TimelineIssue] = field(default_factory=list)
    transition_ms: int = 0
    packets: int = 0

    @property
    def ok(self) -> bool:
        return not self.issues

    def to_dict(self) -> dict[str, object]:
        return {
            "segment_ticks": self.segment_ticks,
            "duration_ticks": self.duration_ticks,
            "cut_points": self.cut_points,
            "segments": self.segments,
            "keyframe_count": len(self.keyframes),
            "transition_ms": self.transition_ms,
            "packets": self.packets,
            "issues": [issue.to_dict() for issue in self.issues],
            "ok": self.ok,
        }


def validate_timeline(
    packets: list[TimelinePacket],
    *,
    segment_ticks: int,
    duration_ticks: int,
    transition_ms: int = 0,
    cut_points: list[int] | None = None,
) -> TimelineReport:
    """Check timestamp monotonicity, cut bounds and transition alignment.

    ``cut_points`` are nanosecond positions where the operator declares an edit.
    Each must fall inside ``[0, duration_ticks]`` and, when transitions are in
    use, land on a segment boundary.

    ``transition_ms`` drives the overlap check: a cross-segment transition
    consumes ``transition_ms`` on each side of the boundary, so neighbouring
    segments must be at least that long; a shorter segment means the declared
    overlap cannot be honoured and is reported.
    """
    if segment_ticks <= 0:
        raise ValueError("segment_ticks must be positive")
    report = TimelineReport(
        segment_ticks=segment_ticks,
        duration_ticks=duration_ticks,
        transition_ms=transition_ms,
    )
    if cut_points:
        report.cut_points = list(cut_points)

    previous: dict[int, int] = {}
    negative_reported: set[int] = set()
    started: set[int] = set()
    for packet in packets:
        report.packets += 1
        report.segments.append(packet.segment)
        if packet.pts_ticks is None:
            continue
        if packet.pts_ticks < 0:
            # Negative pts before a stream's first non-negative timestamp is
            # encoder priming (AAC/Opus pre-roll), not a timeline defect; the
            # same rule is applied by timebase.TimestampTracker during probing.
            if packet.stream_index in started:
                if packet.stream_index not in negative_reported:
                    negative_reported.add(packet.stream_index)
                    report.issues.append(
                        TimelineIssue(
                            "negative_timestamp",
                            packet.stream_index,
                            f"negative pts {format_ticks(packet.pts_ticks)}",
                            packet.pts_ticks,
                        )
                    )
            continue
        started.add(packet.stream_index)
        last = previous.get(packet.stream_index)
        if last is not None and packet.pts_ticks < last:
            report.issues.append(
                TimelineIssue(
                    "non_monotonic",
                    packet.stream_index,
                    (
                        f"pts moved backwards from {format_ticks(last)} "
                        f"to {format_ticks(packet.pts_ticks)}"
                    ),
                    packet.pts_ticks,
                )
            )
        previous[packet.stream_index] = packet.pts_ticks
        if packet.is_keyframe:
            report.keyframes.append(packet.pts_ticks)

    for point in report.cut_points:
        if point < 0 or point > duration_ticks:
            report.issues.append(
                TimelineIssue(
                    "cut_out_of_bounds",
                    None,
                    f"cut point {format_ticks(point)} lies outside [0, {format_ticks(duration_ticks)}]",
                    point,
                )
            )
            continue
        if transition_ms and point % segment_ticks != 0:
            report.issues.append(
                TimelineIssue(
                    "cut_off_grid",
                    None,
                    f"cut point {format_ticks(point)} is not on a {format_ticks(segment_ticks)} boundary",
                    point,
                )
            )

    if transition_ms:
        transition_ticks = transition_ms * 1_000_000
        if transition_ticks * 2 > segment_ticks:
            report.issues.append(
                TimelineIssue(
                    "transition_overlap_inconsistent",
                    None,
                    (
                        f"declared {transition_ms} ms transition needs "
                        f"{format_ticks(transition_ticks * 2)} of overlap on a "
                        f"{format_ticks(segment_ticks)} segment"
                    ),
                )
            )
        # A partial tail segment shorter than the transition cannot host the
        # declared overlap.  This is a property of the segment grid, so at most
        # one finding (the earliest offending segment) is reported.
        short_segments = []
        for segment in sorted(set(report.segments)):
            start = segment * segment_ticks
            length = min(segment_ticks, max(0, duration_ticks - start))
            if 0 < length < transition_ticks:
                short_segments.append((segment, start, length))
        if short_segments:
            segment, start, length = short_segments[0]
            report.issues.append(
                TimelineIssue(
                    "transition_overlap_inconsistent",
                    None,
                    (
                        f"{len(short_segments)} segment(s) shorter than the "
                        f"{transition_ms} ms transition; first is segment "
                        f"{segment} at {format_ticks(start)} "
                        f"({format_ticks(length)} long)"
                    ),
                    start,
                )
            )

    # De-duplicate segment list while preserving ascending order.
    report.segments = sorted(set(report.segments))
    report.keyframes.sort()
    return report


def as_fraction_pair(time_base: tuple[int, int]) -> Fraction:
    return as_fraction(time_base)


__all__ = [
    "CUT_TOLERANCE_TICKS",
    "Mapping",
    "StreamAssignment",
    "TimelineIssue",
    "TimelinePacket",
    "TimelineReport",
    "resolve_mapping",
    "validate_timeline",
]
