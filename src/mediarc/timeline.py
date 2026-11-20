"""Integer timeline primitives.

Everything downstream of :mod:`mediarc.probe` speaks in **integer ticks** on a
single rational timeline rate.  No float seconds ever reach the archive
manifest, because float round-off is exactly what makes a restored edit point
drift by a frame.

The canonical rate is chosen once per archive run (see
:func:`choose_timeline_rate`) as the least common multiple of the video stream
time bases and the audio sample rates, capped at :data:`MAX_TICK_RATE`.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable, Sequence

from .errors import MediaError

#: Upper bound for the canonical timeline rate.  5.1 @ 96 kHz plus 90 kHz video
#: (the composite requirement is 14_112_000) fits comfortably below this; the
#: cap only guards pathological time bases such as 1/10_000_000.
MAX_TICK_RATE = 100_000_000

#: Half-open segment bounds are the only representation that survives a
#: lossless round-trip: [start, end) has no double-counted boundary frame.
KINDS = ("video", "audio")


@dataclass(frozen=True, order=True)
class Rational:
    """A positive rational number ``num/den`` used as a ticks-per-second rate."""

    num: int
    den: int = 1

    def __post_init__(self) -> None:
        if self.den == 0:
            raise ValueError("rational denominator must not be zero")
        if self.den < 0:
            object.__setattr__(self, "num", -self.num)
            object.__setattr__(self, "den", -self.den)
        g = math.gcd(abs(self.num), self.den) or 1
        object.__setattr__(self, "num", self.num // g)
        object.__setattr__(self, "den", self.den // g)

    @classmethod
    def parse(cls, text: str | int | Fraction) -> "Rational":
        if isinstance(text, Rational):
            return text
        if isinstance(text, int):
            return cls(text, 1)
        value = Fraction(text)
        return cls(value.numerator, value.denominator)

    @property
    def value(self) -> Fraction:
        return Fraction(self.num, self.den)

    def __int__(self) -> int:
        return math.floor(self.value)

    def __str__(self) -> str:
        return f"{self.num}/{self.den}"

    def to_dict(self) -> str:
        return str(self)


def _round_half_even(value: Fraction) -> int:
    return round(value)  # Fraction.__round__ is banker's rounding for .5


class TimeBase:
    """A rational tick rate plus exact conversions to and from it."""

    __slots__ = ("rate",)

    def __init__(self, rate: Rational | str | int | Fraction) -> None:
        self.rate = rate if isinstance(rate, Rational) else Rational.parse(rate)

    @property
    def ticks_per_second(self) -> Rational:
        return self.rate

    def exact(self, seconds: Fraction | int) -> Fraction:
        return Fraction(seconds) * self.rate.value

    def to_ticks(self, seconds: Fraction | int) -> int:
        return _round_half_even(self.exact(seconds))

    def to_ticks_checked(self, seconds: Fraction | int) -> tuple[int, bool]:
        """Return ``(ticks, exact)``; ``exact`` is false when rounding lost time."""
        value = self.exact(seconds)
        ticks = _round_half_even(value)
        return ticks, value == ticks

    def to_seconds(self, ticks: int) -> Fraction:
        return Fraction(ticks) / self.rate.value

    def rescale(self, ticks: int, other: "TimeBase") -> int:
        if other.rate == self.rate:
            return ticks
        return _round_half_even(Fraction(ticks) * other.rate.value / self.rate.value)

    def __eq__(self, other: object) -> bool:
        return isinstance(other, TimeBase) and other.rate == self.rate

    def __hash__(self) -> int:
        return hash(self.rate)

    def __repr__(self) -> str:
        return f"TimeBase({self.rate})"


def choose_timeline_rate(
    video_time_bases: Iterable[Rational] = (),
    audio_rates: Iterable[int] = (),
    cap: int = MAX_TICK_RATE,
) -> Rational:
    """Pick the smallest integer tick rate that represents every stream exactly.

    A stream timestamp ``pts`` on time base ``num/den`` converts exactly when
    ``den`` divides the canonical rate; an audio frame index converts exactly
    when its sample rate divides it.  The canonical rate is therefore the least
    common multiple of all denominators and sample rates.

    If that multiple exceeds ``cap`` we keep the audio requirement (sample
    positions stay exact) and drop the video denominators; callers then rely on
    :meth:`TimeBase.to_ticks_checked` and
    :func:`analyze_timestamps` to report the resulting rounding.
    """
    video = [tb.den for tb in video_time_bases if tb.num == 1]
    audio = [int(rate) for rate in audio_rates if rate > 0]
    both = 1
    for value in [*video, *audio]:
        both = math.lcm(both, value)
    if both <= cap:
        return Rational(both, 1)
    audio_only = 1
    for value in audio:
        audio_only = math.lcm(audio_only, value)
    return Rational(min(audio_only, cap), 1)


# ---------------------------------------------------------------------------
# Timestamp analysis
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class TimestampIssue:
    """One diagnosed timing problem, still tied to a packet index."""

    code: str
    stream_index: int
    packet_index: int
    ticks: int
    exact: Fraction
    detail: str
    source_offset: int | None = None

    def error(self, path: str | None, offset: int | None = None) -> MediaError:
        return MediaError(
            self.code,
            self.detail,
            stage="timeline",
            path=path,
            stream_index=self.stream_index,
            offset=self.source_offset if offset is None else offset,
            detail={"packet_index": self.packet_index, "ticks": self.ticks},
        )


def analyze_timestamps(
    stream_index: int,
    entries: Sequence[tuple[int, int | None, int | None, int | None]],
    time_base: Rational,
    timeline: TimeBase,
) -> list[TimestampIssue]:
    """Check one stream's packets for timing defects.

    ``entries`` holds ``(packet_index, pts, dts, source_offset)`` in demux
    order, with raw stream ticks.  Checks, in order:

    * negative presentation or decode timestamps,
    * non-monotonic decode timestamps,
    * two distinct presentation times collapsing onto one timeline tick.
    """
    issues: list[TimestampIssue] = []
    seen: dict[int, Fraction] = {}
    last_dts: Fraction | None = None

    for packet_index, pts, dts, source_offset in entries:
        if pts is not None and pts < 0:
            issues.append(
                TimestampIssue(
                    "negative-timestamp", stream_index, packet_index,
                    pts, Fraction(pts) * time_base.value,
                    f"presentation timestamp {pts} is before origin",
                    source_offset,
                )
            )
            continue
        if dts is not None and dts < 0:
            issues.append(
                TimestampIssue(
                    "non-monotonic-timestamp", stream_index, packet_index,
                    dts, Fraction(dts) * time_base.value,
                    f"decode timestamp {dts} is before origin",
                    source_offset,
                )
            )
            continue
        if dts is not None:
            dts_exact = Fraction(dts) * time_base.value
            if last_dts is not None and dts_exact <= last_dts:
                issues.append(
                    TimestampIssue(
                        "non-monotonic-timestamp", stream_index, packet_index,
                        dts, dts_exact,
                        f"decode timestamp did not advance ({dts_exact} <= {last_dts})",
                        source_offset,
                    )
                )
            last_dts = dts_exact
        if pts is None:
            continue
        exact = Fraction(pts) * time_base.value
        tick, _ = timeline.to_ticks_checked(exact)
        prior = seen.get(tick)
        if prior is not None and prior != exact:
            issues.append(
                TimestampIssue(
                    "rounding-collision", stream_index, packet_index,
                    tick, exact,
                    f"{exact}s and {prior}s both round to tick {tick}",
                    source_offset,
                )
            )
        else:
            seen.setdefault(tick, exact)
    return issues


# ---------------------------------------------------------------------------
# Tracks, edit points, segments, transitions
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class Track:
    """A track as bound for archiving, in timeline ticks."""

    index: int
    kind: str
    codec: str
    time_base: Rational
    start_ticks: int = 0
    duration_ticks: int | None = None
    language: str | None = None
    attributes: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if self.kind not in KINDS:
            raise ValueError(f"track kind must be one of {KINDS}, got {self.kind!r}")

    @property
    def end_ticks(self) -> int | None:
        if self.duration_ticks is None:
            return None
        return self.start_ticks + self.duration_ticks

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "kind": self.kind,
            "codec": self.codec,
            "time_base": str(self.time_base),
            "start_ticks": self.start_ticks,
            "duration_ticks": self.duration_ticks,
            "language": self.language,
            "attributes": dict(self.attributes),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Track":
        return cls(
            index=int(data["index"]),
            kind=str(data["kind"]),
            codec=str(data["codec"]),
            time_base=Rational.parse(data["time_base"]),
            start_ticks=int(data.get("start_ticks", 0)),
            duration_ticks=None if data.get("duration_ticks") is None else int(data["duration_ticks"]),
            language=data.get("language"),
            attributes=dict(data.get("attributes") or {}),
        )


@dataclass(slots=True)
class EditPoint:
    """A named cut position on one track, expressed in timeline ticks."""

    name: str
    track_index: int
    ticks: int


@dataclass(slots=True)
class Segment:
    """A half-open ``[start_ticks, end_ticks)`` region of one track."""

    segment_id: str
    track_index: int
    start_ticks: int
    end_ticks: int
    source_start_ticks: int | None = None
    source_end_ticks: int | None = None
    mapping: dict[str, Any] = field(default_factory=dict)

    @property
    def duration_ticks(self) -> int:
        return self.end_ticks - self.start_ticks

    def contains(self, start: int, end: int) -> bool:
        return self.start_ticks <= start < end <= self.end_ticks

    def to_dict(self) -> dict[str, Any]:
        return {
            "segment_id": self.segment_id,
            "track_index": self.track_index,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "source_start_ticks": self.source_start_ticks,
            "source_end_ticks": self.source_end_ticks,
            "mapping": dict(self.mapping),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Segment":
        return cls(
            segment_id=str(data["segment_id"]),
            track_index=int(data["track_index"]),
            start_ticks=int(data["start_ticks"]),
            end_ticks=int(data["end_ticks"]),
            source_start_ticks=_opt_int(data.get("source_start_ticks")),
            source_end_ticks=_opt_int(data.get("source_end_ticks")),
            mapping=dict(data.get("mapping") or {}),
        )


@dataclass(slots=True)
class Transition:
    """A declared overlap between two segments, possibly across tracks.

    ``start_ticks``/``end_ticks`` bound the overlap itself, not the segments.
    The timeline requires the overlap to sit inside both segments and to have
    the declared duration.
    """

    transition_id: str
    from_segment: str
    to_segment: str
    start_ticks: int
    end_ticks: int
    kind: str = "crossfade"
    duration_ticks: int | None = None

    @property
    def overlap_ticks(self) -> int:
        return self.end_ticks - self.start_ticks

    def to_dict(self) -> dict[str, Any]:
        return {
            "transition_id": self.transition_id,
            "from_segment": self.from_segment,
            "to_segment": self.to_segment,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "kind": self.kind,
            "duration_ticks": self.duration_ticks,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Transition":
        return cls(
            transition_id=str(data["transition_id"]),
            from_segment=str(data["from_segment"]),
            to_segment=str(data["to_segment"]),
            start_ticks=int(data["start_ticks"]),
            end_ticks=int(data["end_ticks"]),
            kind=str(data.get("kind", "crossfade")),
            duration_ticks=_opt_int(data.get("duration_ticks")),
        )


@dataclass(slots=True)
class Timeline:
    """Tracks, segments and transitions sharing one integer tick base."""

    time_base: TimeBase
    duration_ticks: int | None = None
    tracks: list[Track] = field(default_factory=list)
    segments: list[Segment] = field(default_factory=list)
    transitions: list[Transition] = field(default_factory=list)
    edit_points: list[EditPoint] = field(default_factory=list)

    # -- lookups -----------------------------------------------------------
    def track(self, index: int) -> Track | None:
        return next((t for t in self.tracks if t.index == index), None)

    def segment(self, segment_id: str) -> Segment | None:
        return next((s for s in self.segments if s.segment_id == segment_id), None)

    def segments_for_track(self, track_index: int) -> list[Segment]:
        return [s for s in self.segments if s.track_index == track_index]

    # -- validation --------------------------------------------------------
    def validate(self, *, path: str | None = None) -> list[MediaError]:
        """Return every timeline defect; isolation happens at the caller."""
        errors: list[MediaError] = []
        errors.extend(self._validate_segments(path))
        errors.extend(self._validate_edit_points(path))
        errors.extend(self._validate_transitions(path))
        return errors

    def _validate_segments(self, path: str | None) -> list[MediaError]:
        errors: list[MediaError] = []
        by_track: dict[int, list[Segment]] = {}
        for seg in self.segments:
            if seg.end_ticks <= seg.start_ticks:
                errors.append(
                    MediaError(
                        "edit-order", f"segment {seg.segment_id} is empty or reversed",
                        stage="timeline", path=path, stream_index=seg.track_index,
                    )
                )
                continue
            track = self.track(seg.track_index)
            if track is None:
                errors.append(
                    MediaError(
                        "type-mismatch",
                        f"segment {seg.segment_id} references unknown track {seg.track_index}",
                        stage="timeline", path=path, stream_index=seg.track_index,
                    )
                )
                continue
            if track.end_ticks is not None and seg.end_ticks > track.end_ticks:
                errors.append(
                    MediaError(
                        "edit-out-of-range",
                        f"segment {seg.segment_id} ends at {seg.end_ticks} beyond track end {track.end_ticks}",
                        stage="timeline", path=path, stream_index=seg.track_index,
                    )
                )
            by_track.setdefault(seg.track_index, []).append(seg)
        for track_index, segments in by_track.items():
            ordered = sorted(segments, key=lambda s: (s.start_ticks, s.end_ticks))
            for prev, nxt in zip(ordered, ordered[1:]):
                if nxt.start_ticks < prev.end_ticks and not self._bridged(prev, nxt):
                    errors.append(
                        MediaError(
                            "transition-overlap",
                            f"segments {prev.segment_id} and {nxt.segment_id} overlap without a transition",
                            stage="timeline", path=path, stream_index=track_index,
                        )
                    )
        return errors

    def _bridged(self, left: Segment, right: Segment) -> bool:
        for tr in self.transitions:
            pair = {tr.from_segment, tr.to_segment}
            if pair == {left.segment_id, right.segment_id}:
                return True
        return False

    def _validate_edit_points(self, path: str | None) -> list[MediaError]:
        errors: list[MediaError] = []
        for ep in self.edit_points:
            if ep.ticks < 0:
                errors.append(
                    MediaError(
                        "negative-timestamp", f"edit point {ep.name} is negative",
                        stage="timeline", path=path, stream_index=ep.track_index,
                    )
                )
                continue
            track = self.track(ep.track_index)
            if track and track.end_ticks is not None and ep.ticks > track.end_ticks:
                errors.append(
                    MediaError(
                        "edit-out-of-range",
                        f"edit point {ep.name} at {ep.ticks} beyond track end {track.end_ticks}",
                        stage="timeline", path=path, stream_index=ep.track_index,
                    )
                )
        per_track: dict[int, list[EditPoint]] = {}
        for ep in self.edit_points:
            per_track.setdefault(ep.track_index, []).append(ep)
        for track_index, points in per_track.items():
            ordered = sorted(points, key=lambda p: p.ticks)
            for prev, nxt in zip(ordered, ordered[1:]):
                if nxt.ticks == prev.ticks:
                    errors.append(
                        MediaError(
                            "edit-order",
                            f"edit points {prev.name} and {nxt.name} share tick {nxt.ticks}",
                            stage="timeline", path=path, stream_index=track_index,
                        )
                    )
        return errors

    def _validate_transitions(self, path: str | None) -> list[MediaError]:
        errors: list[MediaError] = []
        ordered: list[Transition] = []
        for tr in self.transitions:
            src = self.segment(tr.from_segment)
            dst = self.segment(tr.to_segment)
            if src is None or dst is None:
                errors.append(
                    MediaError(
                        "transition-mismatch",
                        f"transition {tr.transition_id} references a missing segment",
                        stage="timeline", path=path,
                    )
                )
                continue
            if tr.overlap_ticks <= 0:
                errors.append(
                    MediaError(
                        "transition-overlap", f"transition {tr.transition_id} has no overlap",
                        stage="timeline", path=path, stream_index=dst.track_index,
                    )
                )
                continue
            if tr.duration_ticks is not None and tr.duration_ticks != tr.overlap_ticks:
                errors.append(
                    MediaError(
                        "transition-mismatch",
                        f"transition {tr.transition_id} declares {tr.duration_ticks} ticks "
                        f"but overlaps {tr.overlap_ticks}",
                        stage="timeline", path=path, stream_index=dst.track_index,
                    )
                )
            if not src.contains(tr.start_ticks, tr.end_ticks) or not dst.contains(
                tr.start_ticks, tr.end_ticks
            ):
                errors.append(
                    MediaError(
                        "transition-overlap",
                        f"transition {tr.transition_id} overlap leaves its segments",
                        stage="timeline", path=path, stream_index=dst.track_index,
                    )
                )
                continue
            ordered.append(tr)
        # Two transitions on one track may not cover the same tick twice.
        for first, second in zip(ordered, ordered[1:]):
            first_seg = self.segment(first.from_segment)
            second_seg = self.segment(second.from_segment)
            if first_seg is None or second_seg is None:
                continue
            if first_seg.track_index != second_seg.track_index:
                continue
            if second.start_ticks < first.end_ticks and first.start_ticks < second.end_ticks:
                errors.append(
                    MediaError(
                        "transition-overlap",
                        f"transitions {first.transition_id} and {second.transition_id} overlap",
                        stage="timeline", path=path, stream_index=second_seg.track_index,
                    )
                )
        return errors

    # -- serialisation -----------------------------------------------------
    def to_dict(self) -> dict[str, Any]:
        return {
            "time_base": str(self.time_base.rate),
            "duration_ticks": self.duration_ticks,
            "tracks": [t.to_dict() for t in self.tracks],
            "segments": [s.to_dict() for s in self.segments],
            "transitions": [t.to_dict() for t in self.transitions],
            "edit_points": [
                {"name": e.name, "track_index": e.track_index, "ticks": e.ticks}
                for e in self.edit_points
            ],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Timeline":
        return cls(
            time_base=TimeBase(Rational.parse(data["time_base"])),
            duration_ticks=_opt_int(data.get("duration_ticks")),
            tracks=[Track.from_dict(t) for t in data.get("tracks", [])],
            segments=[Segment.from_dict(s) for s in data.get("segments", [])],
            transitions=[Transition.from_dict(t) for t in data.get("transitions", [])],
            edit_points=[
                EditPoint(str(e["name"]), int(e["track_index"]), int(e["ticks"]))
                for e in data.get("edit_points", [])
            ],
        )


def _opt_int(value: Any) -> int | None:
    return None if value is None else int(value)


def contiguous_segments(
    track_index: int,
    starts: Sequence[int],
    end_ticks: int,
    *,
    source_origin: int = 0,
    prefix: str = "seg",
) -> list[Segment]:
    """Build back-to-back segments from cut points, preserving source bounds."""
    bounds = [0, *starts, end_ticks]
    segments: list[Segment] = []
    for i in range(len(bounds) - 1):
        start, end = bounds[i], bounds[i + 1]
        segments.append(
            Segment(
                segment_id=f"{prefix}{i:02d}",
                track_index=track_index,
                start_ticks=start,
                end_ticks=end,
                source_start_ticks=source_origin + start,
                source_end_ticks=source_origin + end,
            )
        )
    return segments
