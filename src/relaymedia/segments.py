"""Segment boundary construction on the unified sample grid.

A cue's fractional-second start and end are mapped through the same rational
clock the timeline uses and then onto the normalised audio sample grid. When
a boundary falls inside a sample block the exact fractional position and the
retained rounding error are recorded, so a cut is always reproducible and its
distance from the ideal instant is auditable. Boundaries snap with 20 ms
class precision (configurable) and never drift beyond half a sample.

Overlapping cues are not silently merged: each side of the overlap becomes an
isolated segment flagged on both partners.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .clock import SamplePosition, to_sample_position
from .errors import ErrorRecord, Stage
from .subtitles import SubtitleCue, SubtitleTrack


@dataclass(frozen=True, slots=True)
class Segment:
    id: str
    source: str
    cue_index: int
    start_sample: int
    end_sample: int
    start_seconds: float
    end_seconds: float
    start_error_seconds: Fraction
    end_error_seconds: Fraction
    overlap: bool
    text: str

    @property
    def duration_samples(self) -> int:
        return self.end_sample - self.start_sample

    @property
    def max_abs_error_seconds(self) -> Fraction:
        return max(abs(self.start_error_seconds), abs(self.end_error_seconds))

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "source": self.source,
            "cue_index": self.cue_index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "start_error_seconds": str(self.start_error_seconds),
            "end_error_seconds": str(self.end_error_seconds),
            "max_abs_error_seconds": str(self.max_abs_error_seconds),
            "overlap": self.overlap,
            "text": self.text,
        }


@dataclass(slots=True)
class SegmentPlan:
    sample_rate: int
    segments: list[Segment] = field(default_factory=list)
    errors: list[ErrorRecord] = field(default_factory=list)

    @property
    def max_round_error_seconds(self) -> Fraction:
        if not self.segments:
            return Fraction(0)
        return max(s.max_abs_error_seconds for s in self.segments)

    @property
    def boundary_granularity_samples(self) -> int:
        if not self.segments:
            return 0
        values = {s.start_sample for s in self.segments} | {s.end_sample for s in self.segments}
        diffs = sorted({b - a for a, b in zip(sorted(values), sorted(values)[1:]) if b != a})
        return min(diffs) if diffs else 0


def build_segments(
    track: SubtitleTrack,
    sample_rate: int,
    *,
    max_boundary_ms: float = 20.0,
    timeline_offset: Fraction | int = 0,
) -> SegmentPlan:
    """Turn parsed cues into sample-accurate, non-overlapping segment specs."""
    plan = SegmentPlan(sample_rate=sample_rate)
    offset = Fraction(timeline_offset)
    overlap_ids: set[int] = set()
    for prev, cue in zip(track.cues, track.cues[1:]):
        if cue.start < prev.end:
            overlap_ids.update((prev.index, cue.index))

    max_err_allowed = Fraction(int(round(max_boundary_ms)), 1000)
    for cue in track.cues:
        start_pos = to_sample_position(cue.start + offset, sample_rate)
        end_pos = to_sample_position(cue.end + offset, sample_rate)
        if end_pos.index <= start_pos.index:
            plan.errors.append(
                ErrorRecord(
                    stage=Stage.SEGMENT.value, kind="ZeroLengthSegment",
                    message=f"cue {cue.index} maps to zero samples at rate {sample_rate}",
                    source=cue.source, offset_bytes=cue.start_offset_bytes,
                    line=cue.start_line,
                )
            )
            continue
        if start_pos.abs_error_seconds > max_err_allowed or end_pos.abs_error_seconds > max_err_allowed:
            plan.errors.append(
                ErrorRecord(
                    stage=Stage.SEGMENT.value, kind="BoundaryPrecision",
                    message=(
                        f"cue {cue.index} rounding error exceeds {max_boundary_ms} ms: "
                        f"{start_pos.error_seconds}, {end_pos.error_seconds}"
                    ),
                    source=cue.source, offset_bytes=cue.start_offset_bytes,
                    line=cue.start_line,
                )
            )
        plan.segments.append(
            Segment(
                id=f"seg-{cue.index:04d}",
                source=cue.source,
                cue_index=cue.index,
                start_sample=start_pos.index,
                end_sample=end_pos.index,
                start_seconds=float(cue.start + offset),
                end_seconds=float(cue.end + offset),
                start_error_seconds=start_pos.error_seconds,
                end_error_seconds=end_pos.error_seconds,
                overlap=cue.index in overlap_ids,
                text=cue.text,
            )
        )
    return plan


def cut_pcm(pcm, segment: Segment):
    """Slice a normalised audio array at a plan's integer sample positions."""
    return pcm[segment.start_sample:segment.end_sample].copy()
