"""Timing records: config knobs, repair statistics, per-track timelines.

These are deliberately plain data so the JSON report and the MP4 writer can
both consume them without importing the planning machinery.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .matroska_model import Track

#: Candidate MP4 media timescales, in preference order.
VIDEO_TIMESCALE = 90_000
AUDIO_TIMESCALE = 48_000
SUBTITLE_TIMESCALE = 1_000

#: Smallest legal gap between consecutive DTS values, per track kind.
VIDEO_MIN_STEP = 1
AUDIO_MIN_STEP = 1
SUBTITLE_MIN_STEP = 1


class TimestampError(ValueError):
    """Raised when a track's timing cannot be made MP4-legal."""

    def __init__(self, code: str, message: str, track_number: int | None = None) -> None:
        self.code = code
        self.track_number = track_number
        super().__init__(message)

    def as_dict(self) -> dict[str, object]:
        return {"code": self.code, "message": str(self), "track_number": self.track_number}

@dataclass
class TimingConfig:
    """Knobs that decide how aggressively timestamps get repaired."""

    video_timescale: int = VIDEO_TIMESCALE
    audio_timescale: int = AUDIO_TIMESCALE
    subtitle_timescale: int = SUBTITLE_TIMESCALE
    #: Shift every track so its first rewritten PTS is at least zero.
    normalize_origin: bool = True
    #: Move repeated PTS values forward by the track's minimum step.
    repair_duplicates: bool = True
    #: Flatten backwards presentation times into a monotonic order. Off by
    #: default: a backwards PTS is usually B-frame reordering, which is carried
    #: faithfully through the composition time table instead of being destroyed.
    force_monotonic_pts: bool = False
    #: A gap larger than this many nominal frame durations is reported as a jump.
    jump_threshold_frames: float = 10.0

    def timescale_for(self, track: Track) -> int:
        if track.is_video:
            return self.video_timescale
        if track.is_audio:
            return self.audio_timescale
        return self.subtitle_timescale

    def min_step_for(self, track: Track) -> int:
        if track.is_video:
            return VIDEO_MIN_STEP
        if track.is_audio:
            return AUDIO_MIN_STEP
        return SUBTITLE_MIN_STEP


@dataclass
class RepairStats:
    """Per-track counts of every timing anomaly seen and what was done about it."""

    negative: int = 0  # source PTS below zero (origin was shifted)
    duplicates: int = 0  # repeated PTS (nudged by the minimum step)
    regressions: int = 0  # backwards PTS, kept as composition offsets
    missing_duration: int = 0  # samples with no BlockDuration
    jumps: int = 0  # gaps wider than the jump threshold
    clamped: int = 0  # values forced to zero because normalization was off

    def total(self) -> int:
        return (
            self.negative
            + self.duplicates
            + self.regressions
            + self.missing_duration
            + self.jumps
            + self.clamped
        )

    def as_dict(self) -> dict[str, int]:
        return {
            "negative": self.negative,
            "duplicates": self.duplicates,
            "regressions": self.regressions,
            "missing_duration": self.missing_duration,
            "jumps": self.jumps,
            "clamped": self.clamped,
            "total": self.total(),
        }


@dataclass
class TrackTimeline:
    """Everything we know about one track's timing, before and after repair."""

    track_number: int
    track_type: str
    timescale: int
    source_scale_ns: int
    source_ticks: np.ndarray
    source_ns: np.ndarray
    rewritten_dts: np.ndarray
    rewritten_pts: np.ndarray
    durations: np.ndarray
    shift_ticks: int
    shift_ns: int
    stats: RepairStats
    sample_count: int
    nominal_duration_ns: float | None = None
    notes: list[str] = field(default_factory=list)

    @property
    def first_source_ns(self) -> float | None:
        return float(self.source_ns[0]) if self.source_ns.size else None

    @property
    def last_source_ns(self) -> float | None:
        return float(self.source_ns[-1]) if self.source_ns.size else None

    def first_rewritten_seconds(self) -> float | None:
        return float(self.rewritten_dts[0]) / self.timescale if self.rewritten_dts.size else None

    def last_rewritten_seconds(self) -> float | None:
        if not self.rewritten_dts.size:
            return None
        end = self.rewritten_dts[-1] + self.durations[-1]
        return float(end) / self.timescale

    def as_dict(self) -> dict[str, object]:
        return {
            "track_number": self.track_number,
            "track_type": self.track_type,
            "timescale": self.timescale,
            "source_scale_ns": self.source_scale_ns,
            "sample_count": self.sample_count,
            "shift_ticks": self.shift_ticks,
            "shift_ns": self.shift_ns,
            "source_first_ns": self.first_source_ns,
            "source_last_ns": self.last_source_ns,
            "rewritten_first_pts": _r(self.first_rewritten_seconds()),
            "rewritten_last_pts": _r(self.last_rewritten_seconds()),
            "repairs": self.stats.as_dict(),
            "notes": list(self.notes),
        }


@dataclass
class RewritePlan:
    """The complete, auditable timing rewrite for one document."""

    source_scale_ns: int
    timelines: dict[int, TrackTimeline]
    global_shift_ticks: int = 0
    warnings: list[dict[str, object]] = field(default_factory=list)

    def timeline(self, track_number: int) -> TrackTimeline:
        if track_number not in self.timelines:
            raise TimestampError(
                "timeline_missing", f"no timeline for track {track_number}", track_number
            )
        return self.timelines[track_number]

    @property
    def repair_total(self) -> int:
        return sum(t.stats.total() for t in self.timelines.values())

    def summary(self) -> dict[str, object]:
        return {
            "source_scale_ns": self.source_scale_ns,
            "global_shift_ticks": self.global_shift_ticks,
            "track_count": len(self.timelines),
            "repair_total": self.repair_total,
            "tracks": [t.as_dict() for t in self.timelines.values()],
            "warnings": list(self.warnings),
        }


def _r(value: float | None) -> float | None:
    return None if value is None else round(value, 6)


def _r(value: float | None) -> float | None:
    return None if value is None else round(value, 6)


__all__ = [
    "AUDIO_TIMESCALE",
    "RepairStats",
    "RewritePlan",
    "SUBTITLE_TIMESCALE",
    "TimingConfig",
    "TimestampError",
    "TrackTimeline",
    "VIDEO_TIMESCALE",
]
