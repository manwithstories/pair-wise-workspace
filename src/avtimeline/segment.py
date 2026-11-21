"""Segment boundary planning shared by every representation.

One set of boundaries is computed for all renditions, so a client can switch
bitrate at exactly the same media time.  A boundary is expressed in all three
units the pipeline cares about:

* rational time on the shared clock,
* video frame index at the timeline rate,
* audio sample index at the target sample rate.

The planner snaps the requested cut to a video frame, checks the keyframe
constraint when a source keyframe grid is supplied, keeps cuts clear of
transition spans, and preserves any residue shorter than one frame or one audio
block as a short tail segment with its actual duration.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .errors import MediaLocation
from .model import Rate, Span, TimeBase

DEFAULT_SEGMENT_DURATION = Fraction(2, 5)
DEFAULT_AUDIO_RATE = 48_000
AUDIO_BLOCK = 1024


@dataclass(frozen=True)
class BoundaryConflict:
    """A requested boundary that cannot satisfy its constraints."""

    time: Fraction
    reason: str
    detail: str
    location: MediaLocation = field(default_factory=MediaLocation)

    def __str__(self) -> str:
        return f"{self.reason} at {self.time}: {self.detail}"


@dataclass(frozen=True)
class Segment:
    """One planned segment, shared by every rendition."""

    index: int
    span: Span
    video_start_frame: int
    audio_start_sample: int
    video_frames: int
    audio_samples: int
    is_short: bool = False
    kind: str = "regular"

    @property
    def start(self) -> Fraction:
        return self.span.start

    @property
    def end(self) -> Fraction:
        return self.span.end

    @property
    def duration(self) -> Fraction:
        return self.span.duration

    @property
    def has_video(self) -> bool:
        return self.video_frames > 0

    @property
    def has_audio(self) -> bool:
        return self.audio_samples > 0

    def to_dict(self) -> dict:
        return {
            "index": self.index,
            "start": str(self.start),
            "end": str(self.end),
            "duration": str(self.duration),
            "duration_seconds": float(self.duration),
            "video_start_frame": self.video_start_frame,
            "audio_start_sample": self.audio_start_sample,
            "video_frames": self.video_frames,
            "audio_samples": self.audio_samples,
            "has_video": self.has_video,
            "has_audio": self.has_audio,
            "is_short": self.is_short,
            "kind": self.kind,
        }


@dataclass
class SegmentPlan:
    segments: list[Segment]
    conflicts: list[BoundaryConflict] = field(default_factory=list)
    guards: list[dict] = field(default_factory=list)
    video_rate: Rate = Rate(25)
    audio_rate: int = DEFAULT_AUDIO_RATE

    @property
    def regular_segments(self) -> list[Segment]:
        return [s for s in self.segments if not s.is_short]

    @property
    def short_segments(self) -> list[Segment]:
        return [s for s in self.segments if s.is_short]

    @property
    def boundaries(self) -> list[Fraction]:
        return [s.start for s in self.segments] + [self.segments[-1].end] if self.segments else []

    def max_boundary_error(self) -> Fraction:
        """Largest deviation between the frame and audio representation of a cut."""
        error = Fraction(0)
        for segment in self.segments:
            for moment, ticks, ticks_per_second in (
                (segment.start, segment.video_start_frame, self.video_rate.value),
                (segment.start, segment.audio_start_sample, Fraction(self.audio_rate)),
            ):
                represented = Fraction(ticks) / ticks_per_second
                error = max(error, abs(represented - moment))
        return error

    def to_dict(self) -> dict:
        return {
            "video_rate": str(self.video_rate),
            "audio_rate": self.audio_rate,
            "segments": [s.to_dict() for s in self.segments],
            "conflicts": [
                {
                    "time": str(c.time),
                    "reason": c.reason,
                    "detail": c.detail,
                }
                for c in self.conflicts
            ],
            "guards": self.guards,
            "max_boundary_error": str(self.max_boundary_error()),
        }


def _nearest_frame(time: Fraction, step: Fraction) -> int:
    units = time / step
    lower = units.numerator // units.denominator
    remainder = units - lower
    if remainder > Fraction(1, 2):
        return lower + 1
    if remainder == Fraction(1, 2):
        # Round half to even for deterministic, symmetric snapping.
        return lower if lower % 2 == 0 else lower + 1
    return lower


def _sample_at(time: Fraction, sample_rate: int) -> int:
    units = time * sample_rate
    lower = units.numerator // units.denominator
    remainder = units - lower
    if remainder > Fraction(1, 2):
        return lower + 1
    if remainder == Fraction(1, 2):
        return lower if lower % 2 == 0 else lower + 1
    return lower


def _shift_out_of_transitions(
    time: Fraction, transitions: list[Span]
) -> tuple[Fraction, dict | None]:
    """Move a cut that lands inside a transition to the transition's end."""
    for span in transitions:
        if span.start < time < span.end:
            return span.end, {
                "requested": str(time),
                "shifted_to": str(span.end),
                "transition": [str(span.start), str(span.end)],
            }
    return time, None


def _keyframe_conflict(
    cut: Fraction,
    step: Fraction,
    keyframes: list[Fraction] | None,
    tolerance: Fraction,
) -> BoundaryConflict | None:
    if not keyframes:
        return None
    candidate = min(keyframes, key=lambda k: abs(k - cut))
    if abs(candidate - cut) > tolerance:
        return BoundaryConflict(
            time=cut,
            reason="keyframe_distance",
            detail=(
                f"nearest keyframe is {candidate} ({abs(candidate - cut)}s away), "
                f"beyond the {tolerance}s tolerance"
            ),
        )
    # A boundary must land on a frame boundary: snapping to a keyframe that is
    # not a frame multiple would move the cut by more than half a frame.
    if (candidate / step).denominator != 1:
        return BoundaryConflict(
            time=cut,
            reason="keyframe_not_on_frame",
            detail=f"keyframe at {candidate} is not on a {step}s frame boundary",
        )
    return None


def plan_segments(
    window: Span,
    video_rate: Rate,
    audio_rate: int = DEFAULT_AUDIO_RATE,
    target_duration: Fraction = DEFAULT_SEGMENT_DURATION,
    keyframe_grid: list[Fraction] | None = None,
    keyframe_tolerance: Fraction | None = None,
    transitions: list[Span] | None = None,
    video_duration: Fraction | None = None,
    audio_duration: Fraction | None = None,
) -> SegmentPlan:
    """Plan the shared boundary set for *window*.

    ``video_duration`` and ``audio_duration`` bound the available media; the
    plan never creates a segment beyond them, and any residue shorter than one
    video frame or one audio block becomes a marked short tail.
    """
    step = video_rate.frame_duration
    transitions = transitions or []
    tolerance = keyframe_tolerance if keyframe_tolerance is not None else step
    conflicts: list[BoundaryConflict] = []
    guards: list[dict] = []
    if window.duration <= 0:
        return SegmentPlan([], conflicts, guards, video_rate, audio_rate)
    video_end = window.end if video_duration is None else min(window.end, video_duration)
    audio_end = window.end if audio_duration is None else min(window.end, audio_duration)
    media_end = max(video_end, audio_end)
    if video_end < window.start or audio_end < window.start:
        return SegmentPlan([], conflicts, guards, video_rate, audio_rate)
    start_frame = max(0, _nearest_frame(window.start, step))
    end_frame = max(start_frame, int((video_end - window.start) / step) + start_frame)
    # Target cuts, relative to the window, snapped to frame boundaries.
    cuts: list[int] = []
    if target_duration > 0:
        target = window.start + target_duration
        while target < video_end:
            frame, guard = _shift_out_of_transitions(target, transitions)
            if guard is not None:
                guards.append(guard)
                frame_time = Fraction(round(frame / step)) * step
            else:
                frame_time = Fraction(_nearest_frame(frame, step)) * step
            index = int((frame_time - window.start) / step) + start_frame
            if start_frame < index < end_frame and index not in cuts:
                cuts.append(index)
            target += target_duration
    cuts = sorted(set(cuts))
    frame_cuts = [start_frame] + cuts + [end_frame]
    for frame in frame_cuts:
        cut_time = window.start + Fraction(frame - start_frame) * step
        conflict = _keyframe_conflict(cut_time, step, keyframe_grid, tolerance)
        if conflict is not None:
            conflicts.append(conflict)
    segments: list[Segment] = []
    for index in range(len(frame_cuts) - 1):
        start = window.start + Fraction(frame_cuts[index] - start_frame) * step
        end = window.start + Fraction(frame_cuts[index + 1] - start_frame) * step
        audio_start = _sample_at(start, audio_rate)
        audio_end = _sample_at(end, audio_rate)
        segments.append(
            Segment(
                index=index,
                span=Span(start, end),
                video_start_frame=frame_cuts[index],
                audio_start_sample=audio_start,
                video_frames=frame_cuts[index + 1] - frame_cuts[index],
                audio_samples=audio_end - audio_start,
                is_short=False,
                kind="regular",
            )
        )
    # Residue beyond the last full video frame: audio-only, or a sub-block tail.
    video_represented = window.start + Fraction(end_frame - start_frame) * step
    tail_end = media_end
    if tail_end > video_represented:
        tail_start = video_represented
        audio_start = _sample_at(tail_start, audio_rate)
        audio_end = _sample_at(tail_end, audio_rate)
        if audio_end > audio_start:
            segments.append(
                Segment(
                    index=len(segments),
                    span=Span(tail_start, tail_end),
                    video_start_frame=end_frame,
                    audio_start_sample=audio_start,
                    video_frames=0,
                    audio_samples=audio_end - audio_start,
                    is_short=True,
                    kind="short_tail",
                )
            )
    # A planned segment whose audio is shorter than one block (and which has no
    # video of its own) is the "short block" case: keep it, mark its actual
    # duration.
    for position, segment in enumerate(segments):
        if (
            segment.audio_samples > 0
            and segment.audio_samples < AUDIO_BLOCK
            and not segment.is_short
        ):
            segments[position] = Segment(
                index=segment.index,
                span=segment.span,
                video_start_frame=segment.video_start_frame,
                audio_start_sample=segment.audio_start_sample,
                video_frames=segment.video_frames,
                audio_samples=segment.audio_samples,
                is_short=True,
                kind="short_tail" if segment.video_frames == 0 else "short_block",
            )
    return SegmentPlan(segments, conflicts, guards, video_rate, audio_rate)


def describe_plan(plan: SegmentPlan) -> dict:
    return {
        "segment_count": len(plan.segments),
        "regular_segments": len(plan.regular_segments),
        "short_segments": len(plan.short_segments),
        "conflicts": [str(c) for c in plan.conflicts],
        "max_boundary_error": str(plan.max_boundary_error()),
        "segments": [s.to_dict() for s in plan.segments],
    }


def sample_index_for(time: Fraction, audio_rate: int) -> int:
    return _sample_at(time, audio_rate)


def frame_index_for(time: Fraction, rate: Rate) -> int:
    return _nearest_frame(time, rate.frame_duration)


def frame_time(index: int, rate: Rate) -> Fraction:
    return Fraction(index) * rate.frame_duration


def audio_time(index: int, audio_rate: int) -> Fraction:
    return Fraction(index, audio_rate)


def time_base_for(rate: int) -> TimeBase:
    return TimeBase(rate)
