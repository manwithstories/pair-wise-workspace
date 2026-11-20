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
from typing import TYPE_CHECKING, Sequence

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
    ticks_from_seconds,
)
from .errors import InputError
from .ingest import Anchor, AudioTrack, VideoTrack

if TYPE_CHECKING:  # pragma: no cover - typing only
    from .recover import Segment

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
    audio_seconds: float = 0.0
    video_seconds: float = 0.0

    def audio_ticks(self, sample_rate: int) -> int:
        if self.audio_seconds or self.video_seconds:
            return ticks_from_seconds(self.audio_seconds - self.video_seconds)
        return ticks_from_samples(self.audio_sample, sample_rate)

    def video_ticks(self, video: VideoTrack, nominal_step_ticks: int) -> int:
        if self.video_seconds or self.audio_seconds:
            return ticks_from_seconds(self.video_seconds)
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


def detect_fps_changes(
    durations: Sequence[int], nominal_ticks: int, tolerance: float
) -> list[tuple[int, int]]:
    """Find frames whose duration departs from nominal by more than tolerance."""

    if nominal_ticks <= 0:
        raise InputError("nominal frame duration must be positive")
    if not 0 < tolerance < 1:
        raise InputError("fps tolerance must be within (0, 1)")
    low = nominal_ticks * (1.0 - tolerance)
    high = nominal_ticks * (1.0 + tolerance)
    return [
        (index, duration)
        for index, duration in enumerate(durations)
        if not low <= duration <= high
    ]


def _anchors_to_pairs(
    video: VideoTrack, audio: AudioTrack
) -> list[AnchorPair]:
    """Pair anchors by label, keeping only those present on both tracks."""

    audio_by_label: dict[str, Anchor] = {
        anchor.label: anchor for anchor in audio.anchors
    }
    pairs: list[AnchorPair] = []
    for anchor in video.anchors:
        match = audio_by_label.get(anchor.label)
        if match is None:
            continue
        if anchor.video_index is None or match.audio_sample is None:
            continue
        pairs.append(
            AnchorPair(
                label=anchor.label,
                audio_sample=match.audio_sample,
                video_index=anchor.video_index,
                audio_seconds=match.audio_seconds,
                video_seconds=anchor.video_seconds,
            )
        )
    return pairs


def align_track(
    video: VideoTrack,
    audio: AudioTrack,
    *,
    segment: "Segment | None" = None,
    config: SyncConfig | None = None,
) -> TrackSync:
    """Fit and check one audio/video pair against its recovered segment.

    Anchors are measured on the video's unwrapped capture timeline (so a clock
    wrap or restart cannot masquerade as a constant offset) and on the audio
    sample grid.  The alignment error is the raw placement difference between
    the two, measured from media zero: a fixed delay and a growing drift are
    both real sync faults, so neither is absorbed by a curve fit.  The fit is
    still reported so drift in ppm is visible in the manifest.
    """

    config = config or SyncConfig()
    nominal_step = ticks_from_rate(1, video.nominal_rate)
    if segment is not None and segment.partial:
        return TrackSync(
            track_id=video.track_id,
            kind="av",
            aligned=False,
            max_error_ticks=config.max_error_ticks + 1,
            reason=f"unrecoverable-segment:{segment.reason}",
            error_intervals=[
                ErrorInterval(
                    video.track_id,
                    segment.start_ticks,
                    max(segment.start_ticks + 1, segment.end_ticks),
                    config.max_error_ticks + 1,
                    "unrecoverable-segment",
                )
            ],
        )

    pairs = _anchors_to_pairs(video, audio)
    if len(pairs) < 2:
        return TrackSync(
            track_id=video.track_id,
            kind="av",
            aligned=False,
            reason="insufficient-anchors",
            anchors=len(pairs),
        )

    # Media-zero origin: a capture offset must show up as an alignment error,
    # not be absorbed into the display baseline.
    display = build_display_track(
        [packet.pts_ticks for packet in video.packets],
        nominal_step_ticks=nominal_step,
        wrap=config.wrap_ticks,
        origin_ticks=0,
    )
    display_ticks = display.display_ticks
    anchor_video: list[int] = []
    anchor_audio: list[int] = []
    for pair in pairs:
        if not 0 <= pair.video_index < len(display_ticks):
            raise InputError(
                f"{video.track_id}: anchor {pair.label!r} frame "
                f"{pair.video_index} outside 0..{len(display_ticks) - 1}"
            )
        anchor_video.append(display_ticks[pair.video_index])
        anchor_audio.append(
            ticks_from_samples(pair.audio_sample, audio.sample_rate)
        )
    # Alignment error is the raw placement difference between the displayed
    # video tick and the audio sample tick, both measured from media zero.
    # A constant offset and a growing drift are both real synchronization
    # errors, so neither is absorbed by a fit.
    drift = fit_drift(anchor_video, anchor_audio, origin_ticks=anchor_video[0], min_samples=2)
    errors = [video_tick - audio_tick for video_tick, audio_tick in zip(anchor_video, anchor_audio)]
    abs_errors = [abs(value) for value in errors]
    max_error = max(abs_errors)
    mean_error = sum(abs_errors) / len(abs_errors)
    fps_changes = detect_fps_changes(
        [packet.duration_ticks for packet in video.packets],
        nominal_step,
        config.fps_change_tolerance,
    )
    restarts = len(display.restarts)

    intervals: list[ErrorInterval] = []
    bad_drift = abs(drift.ppm) > config.drift_limit_ppm
    if bad_drift:
        intervals.append(
            ErrorInterval(
                video.track_id,
                0,
                max(1, video.duration_ticks),
                max_error,
                f"drift-exceeds-{config.drift_limit_ppm:g}ppm",
            )
        )
    for index, duration in fps_changes[:8]:
        start = display_ticks[index] if index < len(display_ticks) else 0
        intervals.append(
            ErrorInterval(
                video.track_id,
                start,
                start + max(duration, 1),
                abs(duration - nominal_step) + max_error,
                "fps-change",
            )
        )
    for pair, error in zip(pairs, errors):
        if abs(error) <= config.max_error_ticks:
            continue
        centre = display_ticks[pair.video_index]
        intervals.append(
            ErrorInterval(
                video.track_id,
                max(0, centre - nominal_step),
                centre + nominal_step,
                abs(error),
                "anchor-error",
            )
        )
    aligned = (
        max_error <= config.max_error_ticks and not bad_drift and not fps_changes
    )
    reason = ""
    if not aligned:
        parts = []
        if max_error > config.max_error_ticks:
            parts.append("error-over-tolerance")
        if bad_drift:
            parts.append("drift-over-limit")
        if fps_changes:
            parts.append("frame-rate-change")
        reason = "+".join(parts)
    return TrackSync(
        track_id=video.track_id,
        kind="av",
        aligned=aligned,
        max_error_ticks=max_error,
        mean_error_ticks=mean_error,
        drift_ppm=drift.ppm,
        offset_ticks=drift.origin_ticks,
        anchors=len(pairs),
        error_intervals=intervals,
        restarts=restarts,
        fps_changes=len(fps_changes),
        reason=reason,
    )


def align_audio_only(
    audio: AudioTrack,
    reference_start_ticks: int,
    *,
    config: SyncConfig | None = None,
) -> TrackSync:
    """Report placement error for an audio track not paired with video."""

    config = config or SyncConfig()
    error = audio.start_pts_ticks - reference_start_ticks
    aligned = abs(error) <= config.max_error_ticks
    intervals = []
    if not aligned:
        intervals.append(
            ErrorInterval(
                audio.track_id,
                0,
                max(1, audio.duration_ticks),
                abs(error),
                "audio-offset",
            )
        )
    return TrackSync(
        track_id=audio.track_id,
        kind="audio",
        aligned=aligned,
        max_error_ticks=abs(error),
        mean_error_ticks=float(abs(error)),
        anchors=len(audio.anchors),
        error_intervals=intervals,
        reason="" if aligned else "audio-offset-over-tolerance",
    )


def sample_position(track: TrackSync, tick: int, sample_rate: int) -> int:
    """Map a tick on a synchronized track to its audio sample position."""

    return samples_from_ticks(tick + track.offset_ticks, sample_rate)


def implied_frame_rate(index_delta: int, tick_delta: int) -> Fraction:
    """Frame rate implied by an index/tick delta, for VFR diagnostics."""

    if index_delta <= 0 or tick_delta <= 0:
        raise InputError("frame rate needs positive index and tick deltas")
    return Fraction(index_delta * TICKS_PER_SECOND, tick_delta)


__all__ = [
    "DEFAULT_DRIFT_LIMIT_PPM",
    "DEFAULT_MAX_ERROR_MS",
    "AnchorPair",
    "ErrorInterval",
    "SyncConfig",
    "SyncReport",
    "TrackSync",
    "align_audio_only",
    "align_track",
    "detect_fps_changes",
    "implied_frame_rate",
    "parse_fps",
    "sample_position",
]
