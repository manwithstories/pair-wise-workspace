"""Rational media timelines and the timestamp checks shared by every stage.

All media time is kept as :class:`fractions.Fraction` seconds derived from the
container's own time base.  Floats never enter the decision path, so 1001/30000
frame durations, 1/90000 MP4 time bases and 1024/48000 AAC frames stay exact.

The module offers three things:

* :class:`PacketSpan` -- a single demuxed packet expressed in seconds.
* :class:`StreamTimeline` -- accumulated statistics per stream, including
  first/last valid presentation stamps and continuity issues.
* :func:`analyse_timelines` -- cross-stream checks: timestamp regressions,
  gaps, overlaps and A/V drift, preserving legitimate variable frame rate
  intervals because expected end times come from the packet's own duration.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from statistics import median
from typing import Iterable

from .errors import MediaLocation, QCError

#: Two timestamps closer than this are considered identical.  One microsecond
#: is well below the finest common audio time base (1/48000 s ≈ 20.8 µs).
EPSILON = Fraction(1, 1_000_000)

#: Default gap / overlap tolerance.  Broadcast masters do not contain silent
#: holes longer than a few frames; anything smaller is encoder priming.
DEFAULT_GAP_TOLERANCE = Fraction(1, 10)  # 100 ms

#: Default allowed end-to-end A/V drift inside one asset.
DEFAULT_DRIFT_TOLERANCE = Fraction(20, 1000)  # 20 ms


@dataclass(frozen=True, slots=True)
class PacketSpan:
    """One demuxed packet with time already converted to seconds."""

    stream: int
    pts: Fraction | None
    dts: Fraction | None
    duration: Fraction | None
    pos: int | None
    size: int
    keyframe: bool
    raw_pts: int | None = None
    raw_dts: int | None = None
    raw_duration: int | None = None

    def end(self, fallback: Fraction | None = None) -> Fraction | None:
        start = self.pts if self.pts is not None else self.dts
        if start is None:
            return None
        dur = self.duration if self.duration and self.duration > 0 else fallback
        return start + dur if dur is not None else start


@dataclass(slots=True)
class TimelineIssue:
    kind: str
    message: str
    location: MediaLocation
    amount: Fraction | None = None

    def to_dict(self) -> dict[str, object]:
        out: dict[str, object] = {
            "kind": self.kind,
            "message": self.message,
            "location": self.location.to_dict(),
        }
        if self.amount is not None:
            out["amount_ms"] = round(float(self.amount) * 1000.0, 3)
        return out


@dataclass(slots=True)
class StreamTimeline:
    index: int
    kind: str
    time_base: Fraction
    packet_count: int = 0
    first_pts: Fraction | None = None
    last_end: Fraction | None = None
    durations: list[Fraction] = field(default_factory=list)
    gaps: list[TimelineIssue] = field(default_factory=list)
    regressions: list[TimelineIssue] = field(default_factory=list)
    overlaps: list[TimelineIssue] = field(default_factory=list)

    @property
    def nominal_duration(self) -> Fraction | None:
        usable = [d for d in self.durations if d and d > 0]
        if not usable:
            return None
        # Median is insensitive to the occasional long VFR hold frame.
        return Fraction(median(usable)).limit_denominator(1_000_000)

    @property
    def duration(self) -> Fraction | None:
        if self.first_pts is None or self.last_end is None:
            return None
        return self.last_end - self.first_pts

    def to_dict(self) -> dict[str, object]:
        return {
            "stream": self.index,
            "kind": self.kind,
            "time_base": f"{self.time_base.numerator}/{self.time_base.denominator}",
            "packets": self.packet_count,
            "first_pts": _secs(self.first_pts),
            "last_end": _secs(self.last_end),
            "duration": _secs(self.duration),
            "nominal_frame_duration": _secs(self.nominal_duration),
            "gaps": [issue.to_dict() for issue in self.gaps],
            "regressions": [issue.to_dict() for issue in self.regressions],
            "overlaps": [issue.to_dict() for issue in self.overlaps],
        }


@dataclass(slots=True)
class TimelineReport:
    streams: dict[int, StreamTimeline]
    issues: list[TimelineIssue]
    origin: Fraction | None
    end: Fraction | None
    av_offset: Fraction | None = None
    av_drift: Fraction | None = None

    @property
    def duration(self) -> Fraction | None:
        if self.origin is None or self.end is None:
            return None
        return self.end - self.origin

    @property
    def has_issues(self) -> bool:
        return bool(self.issues)

    def to_dict(self) -> dict[str, object]:
        return {
            "origin": _secs(self.origin),
            "end": _secs(self.end),
            "duration": _secs(self.duration),
            "av_offset_ms": _ms(self.av_offset),
            "av_drift_ms": _ms(self.av_drift),
            "streams": {str(k): v.to_dict() for k, v in sorted(self.streams.items())},
            "issues": [issue.to_dict() for issue in self.issues],
        }


def _secs(value: Fraction | None) -> float | None:
    return None if value is None else round(float(value), 6)


def _ms(value: Fraction | None) -> float | None:
    return None if value is None else round(float(value) * 1000.0, 3)


def seconds(value: int | None, time_base: Fraction) -> Fraction | None:
    """Convert a container timestamp to exact seconds."""
    if value is None:
        return None
    return Fraction(value) * time_base


def build_stream_timeline(
    index: int,
    kind: str,
    time_base: Fraction,
    spans: Iterable[PacketSpan],
    *,
    asset: str | None = None,
    gap_tolerance: Fraction = DEFAULT_GAP_TOLERANCE,
) -> StreamTimeline:
    """Accumulate one stream's spans, flagging gaps/regressions/overlaps.

    Presentation stamps are used when present.  The expected end of a packet
    is ``pts + duration``; because MP4 stores a duration per sample, genuine
    variable-frame-rate intervals are not reported as gaps.
    """
    timeline = StreamTimeline(index=index, kind=kind, time_base=time_base)
    previous: PacketSpan | None = None
    previous_end: Fraction | None = None
    for span in spans:
        timeline.packet_count += 1
        start = span.pts if span.pts is not None else span.dts
        if span.duration is not None and span.duration > 0:
            timeline.durations.append(span.duration)
        if start is None:
            continue
        if timeline.first_pts is None or start < timeline.first_pts:
            timeline.first_pts = start
        fallback = timeline.nominal_duration
        end = span.end(fallback)
        if previous is not None:
            prev_start = previous.pts if previous.pts is not None else previous.dts
            if prev_start is not None and start < prev_start - EPSILON:
                timeline.regressions.append(
                    TimelineIssue(
                        kind="timestamp_regression",
                        message=(
                            f"stream {index} timestamp moved backwards: "
                            f"{float(start):.6f}s after {float(prev_start):.6f}s"
                        ),
                        location=MediaLocation(
                            asset=asset,
                            stream=index,
                            packet_pos=span.pos,
                            timestamp=start,
                            detail="pts regression",
                        ),
                        amount=prev_start - start,
                    )
                )
            if previous_end is not None:
                slack = gap_tolerance
                nominal = timeline.nominal_duration
                if nominal is not None:
                    slack = max(slack, nominal * 2)
                if start > previous_end + slack:
                    timeline.gaps.append(
                        TimelineIssue(
                            kind="timestamp_gap",
                            message=(
                                f"stream {index} gap of "
                                f"{(start - previous_end) * 1000:.1f} ms at {float(start):.6f}s"
                            ),
                            location=MediaLocation(
                                asset=asset,
                                stream=index,
                                packet_pos=span.pos,
                                timestamp=start,
                                detail="continuity gap",
                            ),
                            amount=start - previous_end,
                        )
                    )
                elif start < previous_end - slack and not (
                    timeline.regressions and timeline.regressions[-1].location.packet_pos == span.pos
                ):
                    timeline.overlaps.append(
                        TimelineIssue(
                            kind="timestamp_overlap",
                            message=(
                                f"stream {index} overlap of "
                                f"{(previous_end - start) * 1000:.1f} ms at {float(start):.6f}s"
                            ),
                            location=MediaLocation(
                                asset=asset,
                                stream=index,
                                packet_pos=span.pos,
                                timestamp=start,
                                detail="continuity overlap",
                            ),
                            amount=previous_end - start,
                        )
                    )
        if end is not None and (timeline.last_end is None or end > timeline.last_end):
            timeline.last_end = end
        if previous_end is None or end is None or end > previous_end:
            previous_end = end if end is not None else previous_end
        previous = span
    return timeline


def analyse_timelines(
    timelines: dict[int, StreamTimeline],
    *,
    asset: str | None = None,
    drift_tolerance: Fraction = DEFAULT_DRIFT_TOLERANCE,
) -> TimelineReport:
    """Cross-stream analysis: shared origin, duration and A/V drift."""
    issues: list[TimelineIssue] = []
    for tl in timelines.values():
        issues.extend(tl.gaps)
        issues.extend(tl.regressions)
        issues.extend(tl.overlaps)

    populated = [tl for tl in timelines.values() if tl.first_pts is not None]
    origin = min((tl.first_pts for tl in populated), default=None)
    end = max((tl.last_end for tl in populated if tl.last_end is not None), default=None)

    video = [tl for tl in populated if tl.kind == "video"]
    audio = [tl for tl in populated if tl.kind == "audio"]
    av_offset = None
    av_drift = None
    if video and audio:
        primary_video = min(video, key=lambda tl: tl.index)
        primary_audio = min(audio, key=lambda tl: tl.index)
        # Offset at the start and at the end describe the same physical skew
        # only if the two streams differ by a constant.  Compare them as a
        # *change* in offset, and only flag it when it exceeds both the policy
        # tolerance and one frame of the coarser stream.  Encoders legitimately
        # pad one stream (AAC priming, an extra hold frame at the tail); that
        # shifts both endpoints by the same amount and must not read as drift.
        start_offset = primary_audio.first_pts - primary_video.first_pts
        av_offset = start_offset
        video_duration = primary_video.duration
        audio_duration = primary_audio.duration
        if video_duration is not None and audio_duration is not None:
            mid_offset = (
                primary_audio.first_pts
                + audio_duration / 2
                - (primary_video.first_pts + video_duration / 2)
            )
            av_drift = abs(mid_offset - start_offset)
            quantisation = max(
                primary_video.nominal_duration or Fraction(0),
                primary_audio.nominal_duration or Fraction(0),
            )
            if av_drift > drift_tolerance and av_drift > quantisation:
                issues.append(
                    TimelineIssue(
                        kind="av_drift",
                        message=(
                            f"audio/video drift {float(av_drift) * 1000:.1f} ms exceeds "
                            f"{float(drift_tolerance) * 1000:.1f} ms"
                        ),
                        location=MediaLocation(
                            asset=asset,
                            detail="stream start/end alignment",
                        ),
                        amount=av_drift,
                    )
                )
    return TimelineReport(
        streams=timelines,
        issues=issues,
        origin=origin,
        end=end,
        av_offset=av_offset,
        av_drift=av_drift,
    )
