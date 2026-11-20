"""Audio-to-video synchronization on the shared 90 kHz grid.

The synchronizer converts audio sample positions to ticks, fits each track's
capture clock against the reference program clock and reports an explicit
error interval with a reason whenever drift or a frame-rate change exceeds the
configured tolerance.  Alignment is never silently forced: a rejected track
carries the measured error, not a clamped one.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Sequence

from .clock import (
    DEFAULT_WRAP_TICKS,
    DriftFit,
    TICKS_PER_SECOND,
    build_display_track,
    fit_drift,
    parse_fps,
    samples_from_ticks,
    ticks_from_rate,
    ticks_from_samples,
)
from .errors import InputError
from .ingest import Anchor, AudioTrack, VideoTrack
from .timeline import Segment

#: A successful track must stay within this many milliseconds of the reference.
DEFAULT_MAX_ERROR_MS = 20.0
#: Capture clocks are expected to drift by less than this many ppm.
DEFAULT_DRIFT_LIMIT_PPM = 500.0


@dataclass(frozen=True)
class SyncConfig:
    max_error_ms: float = DEFAULT_MAX_ERROR_MS
    drift_limit_ppm: float = DEFAULT_DRIFT_LIMIT_PPM
    fps_change_tolerance: float = 0.02
    reference_track: str | None = None
    wrap_ticks: int = DEFAULT_WRAP_TICKS

    def __post_init__(self) -> None:
        if self.max_error_ms <= 0:
            raise InputError("max error must be positive")
        if self.drift_limit_ppm <= 0:
            raise InputError("drift limit must be positive")
        if not 0 < self.fps_change_tolerance < 1:
            raise InputError("fps change tolerance must be within (0, 1)")

    @property
    def max_error_ticks(self) -> int:
        return int(round(self.max_error_ms * TICKS_PER_SECOND / 1000.0))


@dataclass(frozen=True)
class ErrorInterval:
    """A bounded region where alignment error exceeds tolerance."""

    track_id: str
    start_ticks: int
    end_ticks: int
    peak_error_ticks: int
    reason: str

    @property
    def peak_error_ms(self) -> float:
        return self.peak_error_ticks * 1000.0 / TICKS_PER_SECOND

    def to_dict(self) -> dict[str, object]:
        return {
            "track_id": self.track_id,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "peak_error_ticks": self.peak_error_ticks,
            "peak_error_ms": self.peak_error_ms,
            "reason": self.reason,
        }


@dataclass(frozen=True)
class AnchorPair:
    """A matched audio sample position and video presentation time."""

    label: str
    audio_sample: int
    video_index: int

    def audio_ticks(self, sample_rate: int) -> int:
        return ticks_from_samples(self.audio_sample, sample_rate)

    def video_ticks(self, video: VideoTrack, nominal_step_ticks: int) -> int:
        if self.video_index >= len(video.packets):
            raise InputError(
                f"anchor {self.label!r} points at video frame {self.video_index}, "
                f"track has {len(video.packets)} packets"
            )
        return self.video_index * nominal_step_ticks


@dataclass
class TrackSync:
    track_id: str
    kind: str
    aligned: bool
    max_error_ticks: int = 0
    mean_error_ticks: float = 0.0
    drift_ppm: float = 0.0
    offset_ticks: int = 0
    anchors: int = 0
    error_intervals: list[ErrorInterval] = field(default_factory=list)
    restarts: int = 0
    fps_changes: int = 0
    reason: str = ""

    @property
    def max_error_ms(self) -> float:
        return self.max_error_ticks * 1000.0 / TICKS_PER_SECOND

    def to_dict(self) -> dict[str, object]:
        return {
            "track_id": self.track_id,
            "kind": self.kind,
            "aligned": self.aligned,
            "max_error_ticks": self.max_error_ticks,
            "max_error_ms": self.max_error_ms,
            "mean_error_ticks": self.mean_error_ticks,
            "drift_ppm": self.drift_ppm,
            "offset_ticks": self.offset_ticks,
            "anchors": self.anchors,
            "restarts": self.restarts,
            "fps_changes": self.fps_changes,
            "reason": self.reason,
            "error_intervals": [item.to_dict() for item in self.error_intervals],
        }


@dataclass
class SyncReport:
    tracks: list[TrackSync] = field(default_factory=list)

    def by_id(self, track_id: str) -> TrackSync | None:
        for item in self.tracks:
            if item.track_id == track_id:
                return item
        return None

    @property
    def max_error_ticks(self) -> int:
        return max((item.max_error_ticks for item in self.tracks if item.aligned), default=0)

    @property
    def max_error_ms(self) -> float:
        return self.max_error_ticks * 1000.0 / TICKS_PER_SECOND

    @property
    def all_aligned(self) -> bool:
        return all(item.aligned for item in self.tracks)

    def summary(self) -> dict[str, object]:
        return {
            "tracks": len(self.tracks),
            "aligned": sum(1 for item in self.tracks if item.aligned),
            "misaligned": sum(1 for item in self.tracks if not item.aligned),
            "max_error_ms": self.max_error_ms,
        }
